// Command crowdvm-server is the CrowdVM coordinator.
//
//	crowdvm-server serve --listen :8420 --data-dir DIR --public-url http://192.168.1.10:8420
//	crowdvm-server user create --username alice --password ... [--admin]
//	crowdvm-server user list | user grant-credits alice 25 | user passwd alice NEWPASS
//	crowdvm-server join-token create [--ttl 24h] [--uses 1] [--owner alice]
//	crowdvm-server join-token list | join-token revoke <id>
//	crowdvm-server invite create [--uses 1] [--ttl 168h]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "user":
		err = userCmd(os.Args[2:])
	case "join-token":
		err = joinTokenCmd(os.Args[2:])
	case "invite":
		err = inviteCmd(os.Args[2:])
	case "downloads":
		err = downloadsCmd(os.Args[2:])
	case "version", "--version":
		fmt.Println("crowdvm-server", server.Version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `crowdvm-server: the CrowdVM coordinator

Commands:
  serve                         run the server (see serve -h)
  user create|list|grant-credits|passwd|admin
  join-token create|list|revoke node join tokens
  invite create                 signup invite code
  downloads import <dist dir>   add binaries (scripts/dist.sh output) for /install and /downloads
  version

Every command takes --data-dir (default ~/.local/share/crowdvm/server).
`)
}

func defaultDataDir() string {
	if v := os.Getenv("CROWDVM_SERVER_DATA"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "crowdvm", "server")
}

// open opens the server state for admin commands (no background loops).
func open(dataDir string) (*server.Server, error) {
	return server.New(server.Config{DataDir: dataDir, SignupCredits: server.DefaultSignupCredits, Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))})
}

// ---------------------------------------------------------------------------
// serve
// ---------------------------------------------------------------------------

func serve(args []string) error {
	fl := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fl.String("listen", fmt.Sprintf(":%d", proto.DefaultServerPort), "listen address")
	dataDir := fl.String("data-dir", defaultDataDir(), "data directory (database, blobs)")
	publicURL := fl.String("public-url", "", "URL users, nodes and guests reach the server at (default http://<lan-ip>:<port>)")
	publicDomain := fl.String("public-domain", "", "wildcard domain for public URLs <id>-<port>.<domain> (default <public-url ip>.nip.io; *.localhost always works)")
	tlsCert := fl.String("tls-cert", "", "TLS certificate file (optional)")
	tlsKey := fl.String("tls-key", "", "TLS key file (optional)")
	openSignup := fl.Bool("open-signup", false, "anyone may sign up (otherwise the first user, then invite codes)")
	openNodes := fl.Bool("open-nodes", false, "any user may create node join tokens (otherwise admins only)")
	enforce := fl.Bool("enforce-credits", false, "refuse starts and stop sandboxes when a wallet has no credits (402 billing_required)")
	signupCredits := fl.Float64("signup-credits", server.DefaultSignupCredits, "credits granted to new accounts (1 credit = 1 hour of a default machine)")
	earn := fl.Float64("node-earn-rate", 1, "share of spent credits that node owners earn")
	perMin := fl.Int("starts-per-minute", server.DefaultLimits.PerMinute, "starts (create/fork/resume) per wallet per minute, 0 = unlimited")
	perHour := fl.Int("starts-per-hour", server.DefaultLimits.PerHour, "starts per wallet per hour, 0 = unlimited")
	perDay := fl.Int("starts-per-day", server.DefaultLimits.PerDay, "starts per wallet per day, 0 = unlimited")
	maxActive := fl.Int("max-active", server.DefaultLimits.Active, "sandboxes at once per wallet, 0 = unlimited")
	ipLimit := fl.Int("ip-rate-limit", server.DefaultLimits.IP, "API requests per 60 s per IP, 0 = unlimited")
	snapSec := fl.Int("snapshot-interval", 0, "periodic live snapshot interval pushed to nodes, seconds (0 = 60, -1 = off)")
	image := fl.String("default-image", "", "base image id for new sandboxes (default: newest known)")
	startWait := fl.Duration("start-wait", 60*time.Second, "how long guest calls wait while a sandbox starts")
	smtpAddr := fl.String("smtp-addr", "", "SMTP server host:port for login codes and invites (optional)")
	smtpFrom := fl.String("smtp-from", "", "SMTP From address")
	smtpUser := fl.String("smtp-user", "", "SMTP username")
	smtpPass := fl.String("smtp-password", os.Getenv("CROWDVM_SMTP_PASSWORD"), "SMTP password (or CROWDVM_SMTP_PASSWORD)")
	trustProxy := fl.Bool("trust-proxy", false, "trust X-Forwarded-For for client IPs")
	logLevel := fl.String("log-level", "info", "debug, info, warn or error")
	noWeb := fl.Bool("no-web", false, "do not serve the dashboard")
	downloadsDir := fl.String("downloads-dir", "", "binaries served at /downloads/{os}/{arch}/{name} (default <data-dir>/downloads)")
	savedIncl := fl.Int("saved-snapshots-included", 10, "saved snapshots included per account or organization (an admin wallet policy overrides it)")
	savedDaily := fl.Float64("saved-snapshot-daily-dollars", 0.056, "daily price of each saved snapshot above the included count, charged in credits")
	outputCap := fl.Int64("output-cap-per-hour", server.DefaultOutputCapPerHour, "stored command output per wallet and hour, and again for other processes, bytes (-1 = no cap)")
	minGuest := fl.String("min-guest-version", server.MinGuestVersion, "oldest guest agent version calls into a sandbox accept (older: 409 agent_outdated; dev builds always pass)")
	noChannel := fl.Bool("no-guest-channel", false, "boot sandboxes the legacy way: the node gets their whole config (secrets included) and proxies their API (default: guests fetch their config from this server over a pinned channel)")
	nodeEgress := fl.Bool("node-egress", false, "let sandboxes reach the internet through their node (default: through the guest channel and out of this server)")
	egressAllow := fl.String("egress-allow", "", "comma-separated CIDRs or ip:port on this server's networks that sandboxes may reach through server egress (e.g. an internal git server); LAN, loopback and tailnet addresses are refused otherwise")
	hooksLocal := fl.Bool("webhook-allow-local", false, "let webhook and OTel export URLs point at loopback, link-local and cloud metadata addresses (LAN and Tailscale addresses are always allowed)")
	aiPrivate := fl.Bool("ai-custom-api-allow-private", os.Getenv("CROWDVM_AI_CUSTOM_API_ALLOW_PRIVATE") == "1", "let custom AI API base URLs use http://, ports, LAN hosts and IPs (or CROWDVM_AI_CUSTOM_API_ALLOW_PRIVATE=1)")
	_ = fl.Parse(args)

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("--log-level: %w", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	if *publicURL == "" {
		*publicURL = guessPublicURL(*listen, *tlsCert != "")
	}
	if *aiPrivate {
		_ = os.Setenv("CROWDVM_AI_CUSTOM_API_ALLOW_PRIVATE", "1") // read by the AI credentials feature
	}
	var webFS fs.FS
	if !*noWeb {
		webFS = web.FS
	}
	cfg := server.Config{
		DataDir: *dataDir, PublicURL: strings.TrimRight(*publicURL, "/"), PublicDomain: *publicDomain, OpenSignup: *openSignup, OpenNodes: *openNodes,
		EnforceCredits: *enforce, SignupCredits: *signupCredits, NodeEarnRate: *earn,
		StartsPerMinute: *perMin, StartsPerHour: *perHour, StartsPerDay: *perDay, MaxActive: *maxActive, IPRateLimit: *ipLimit,
		SnapshotInterval: time.Duration(*snapSec) * time.Second, DefaultImage: *image, StartWait: *startWait,
		SMTP:  server.SMTPConfig{Addr: *smtpAddr, From: *smtpFrom, Username: *smtpUser, Password: *smtpPass},
		WebFS: webFS, Logger: log, TrustProxy: *trustProxy, DownloadsDir: *downloadsDir, MinGuestVersion: *minGuest,
		SavedSnapshotsIncluded: *savedIncl, SavedSnapshotExtraDollarsPerDay: *savedDaily, OutputCapPerHour: *outputCap,
		WebhookAllowLocal: *hooksLocal,
		NoGuestChannel:    *noChannel,
		NodeEgress:        *nodeEgress,
		EgressAllow:       splitList(*egressAllow),
	}
	allow, err := server.ParseEgressAllow(cfg.EgressAllow)
	if err != nil {
		return err
	}
	cfg.EgressAllow = allow
	srv, err := server.New(cfg)
	if err != nil {
		return err
	}
	savePublicURL(*dataDir, cfg.PublicURL)
	srv.Start()
	hs := &http.Server{Addr: *listen, Handler: srv.Handler(), ReadHeaderTimeout: 30 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() {
		log.Info("crowdvm-server listening", "addr", *listen, "publicUrl", cfg.PublicURL, "dataDir", *dataDir, "version", server.Version)
		if *tlsCert != "" {
			errc <- hs.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			errc <- hs.ListenAndServe()
		}
	}()
	select {
	case err := <-errc:
		_ = srv.Close()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = hs.Shutdown(sctx)
	return srv.Close()
}

// guessPublicURL uses the first LAN or Tailscale IPv4 address.
// savePublicURL remembers the URL `serve` runs with, so `join-token create`
// prints it too (bugs-r8-opus 7). Best effort.
func savePublicURL(dataDir, u string) {
	_ = os.WriteFile(filepath.Join(dataDir, "public-url"), []byte(u+"\n"), 0o644)
}

// joinServerURL is the server URL for a join command: the --public-url flag,
// else the one `serve` last ran with, else a guess.
func joinServerURL(flagURL, dataDir string) string {
	if flagURL != "" {
		return flagURL
	}
	if b, err := os.ReadFile(filepath.Join(dataDir, "public-url")); err == nil {
		if u := strings.TrimSpace(string(b)); u != "" {
			return u
		}
	}
	return guessPublicURL(fmt.Sprintf(":%d", proto.DefaultServerPort), false)
}

func guessPublicURL(listen string, tls bool) string {
	_, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		port = strconv.Itoa(proto.DefaultServerPort)
	}
	host, _, _ := net.SplitHostPort(listen)
	scheme := "http"
	if tls {
		scheme = "https"
	}
	if host != "" && host != "0.0.0.0" && host != "::" {
		return scheme + "://" + net.JoinHostPort(host, port)
	}
	best := ""
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.To4() == nil || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() {
			continue
		}
		ip := ipn.IP.To4()
		if ip.IsPrivate() {
			return scheme + "://" + net.JoinHostPort(ip.String(), port)
		}
		if ip[0] == 100 && ip[1]&0xC0 == 64 && best == "" { // Tailscale CGNAT
			best = ip.String()
		}
	}
	if best == "" {
		best = "localhost"
	}
	return scheme + "://" + net.JoinHostPort(best, port)
}

// ---------------------------------------------------------------------------
// admin subcommands
// ---------------------------------------------------------------------------

func userCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: user create|list|grant-credits|passwd|admin")
	}
	fl := flag.NewFlagSet("user "+args[0], flag.ExitOnError)
	dataDir := fl.String("data-dir", defaultDataDir(), "data directory")
	switch args[0] {
	case "create":
		username := fl.String("username", "", "username (required)")
		password := fl.String("password", "", "password (at least 8 characters)")
		email := fl.String("email", "", "email address")
		admin := fl.Bool("admin", false, "make the user an administrator")
		credits := fl.Float64("credits", -1, "initial credits (default: the server's signup credits)")
		_ = fl.Parse(args[1:])
		s, err := open(*dataDir)
		if err != nil {
			return err
		}
		defer s.Close()
		p := server.NewUserParams{Username: *username, Password: *password, Email: *email, Admin: *admin}
		if *credits >= 0 {
			p.Credits = credits
		}
		u, err := s.CreateUser(p)
		if err != nil {
			return err
		}
		fmt.Printf("created user %s (%s) admin=%v\n", u.Username, u.ID, u.IsAdmin)
	case "list":
		_ = fl.Parse(args[1:])
		s, err := open(*dataDir)
		if err != nil {
			return err
		}
		defer s.Close()
		users, err := s.Users()
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tUSERNAME\tEMAIL\tADMIN\tKIND\tCREDITS\tDISABLED")
		for _, u := range users {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%v\t%s\t%.3f\t%v\n", u.ID, u.Username, u.Email, u.IsAdmin, u.Kind, s.Balance(u.ID), u.Disabled)
		}
		tw.Flush()
	case "grant-credits":
		reason := fl.String("reason", "admin grant", "ledger note")
		if err := parseGrantFlags(fl, args[1:]); err != nil {
			return err
		}
		if fl.NArg() != 2 {
			return errors.New("usage: user grant-credits <user> <amount> [--reason text]")
		}
		amount, err := strconv.ParseFloat(fl.Arg(1), 64)
		if err != nil {
			return fmt.Errorf("amount: %w", err)
		}
		s, err := open(*dataDir)
		if err != nil {
			return err
		}
		defer s.Close()
		u, err := s.UserByLogin(fl.Arg(0))
		if err != nil {
			return fmt.Errorf("no user %q", fl.Arg(0))
		}
		if err := s.GrantCredits(u.ID, amount, *reason, ""); err != nil {
			return err
		}
		fmt.Printf("%s now has %.3f credits\n", u.Username, s.Balance(u.ID))
	case "passwd":
		_ = fl.Parse(args[1:])
		if fl.NArg() != 2 {
			return errors.New("usage: user passwd <user> <new password>")
		}
		s, err := open(*dataDir)
		if err != nil {
			return err
		}
		defer s.Close()
		u, err := s.UserByLogin(fl.Arg(0))
		if err != nil {
			return fmt.Errorf("no user %q", fl.Arg(0))
		}
		if err := s.SetPassword(u.ID, fl.Arg(1), ""); err != nil { // bugs-r11-sonnet 6
			return err
		}
		fmt.Println("password changed")
	case "admin":
		off := fl.Bool("off", false, "remove admin rights instead")
		_ = fl.Parse(args[1:])
		if fl.NArg() != 1 {
			return errors.New("usage: user admin <user> [--off]")
		}
		s, err := open(*dataDir)
		if err != nil {
			return err
		}
		defer s.Close()
		u, err := s.UserByLogin(fl.Arg(0))
		if err != nil {
			return fmt.Errorf("no user %q", fl.Arg(0))
		}
		if _, err := s.DB.Exec(`UPDATE users SET is_admin=? WHERE id=?`, !*off, u.ID); err != nil {
			return err
		}
		fmt.Printf("%s admin=%v\n", u.Username, !*off)
	default:
		return fmt.Errorf("unknown user command %q", args[0])
	}
	return nil
}

func joinTokenCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: join-token create|list|revoke")
	}
	fl := flag.NewFlagSet("join-token "+args[0], flag.ExitOnError)
	dataDir := fl.String("data-dir", defaultDataDir(), "data directory")
	switch args[0] {
	case "create":
		name := fl.String("name", "", "note shown in lists")
		ttl := fl.Duration("ttl", 24*time.Hour, "validity (0 = never expires)")
		uses := fl.Int("uses", 1, "how many nodes may join with it (0 = unlimited)")
		owner := fl.String("owner", "", "user who earns credits for these nodes (default: first admin)")
		label := fl.String("label", "", "node labels, comma separated k=v")
		publicURL := fl.String("public-url", "", "server URL to print in the join command")
		_ = fl.Parse(args[1:])
		s, err := open(*dataDir)
		if err != nil {
			return err
		}
		defer s.Close()
		ownerID := ""
		if *owner != "" {
			u, err := s.UserByLogin(*owner)
			if err != nil {
				return fmt.Errorf("no user %q", *owner)
			}
			ownerID = u.ID
		} else if users, err := s.Users(); err == nil {
			for _, u := range users {
				if u.IsAdmin {
					ownerID = u.ID
					break
				}
			}
		}
		labels := map[string]string{}
		for _, kv := range strings.Split(*label, ",") {
			if k, v, ok := strings.Cut(strings.TrimSpace(kv), "="); ok {
				labels[k] = v
			}
		}
		id, tok, err := s.CreateJoinToken(server.JoinTokenParams{Name: *name, CreatedBy: ownerID, OwnerID: ownerID, TTL: *ttl, MaxUses: *uses, Labels: labels})
		if err != nil {
			return err
		}
		u := joinServerURL(*publicURL, *dataDir)
		fmt.Printf("join token %s (id %s)\n\non the node run:\n  crowdvm-node join --server %s --token %s\n", tok, id, u, tok)
	case "list":
		_ = fl.Parse(args[1:])
		s, err := open(*dataDir)
		if err != nil {
			return err
		}
		defer s.Close()
		list, err := s.JoinTokens()
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tPREFIX\tNAME\tSTATUS\tUSES\tEXPIRES")
		for _, t := range list {
			fmt.Fprintf(tw, "%v\t%v\t%v\t%v\t%v/%v\t%v\n", t["id"], t["prefix"], t["name"], t["status"], t["uses"], t["maxUses"], t["expiresAt"])
		}
		tw.Flush()
	case "revoke":
		_ = fl.Parse(args[1:])
		if fl.NArg() != 1 {
			return errors.New("usage: join-token revoke <id>")
		}
		s, err := open(*dataDir)
		if err != nil {
			return err
		}
		defer s.Close()
		if err := s.RevokeJoinToken(fl.Arg(0)); err != nil {
			return err
		}
		fmt.Println("revoked")
	default:
		return fmt.Errorf("unknown join-token command %q", args[0])
	}
	return nil
}

func inviteCmd(args []string) error {
	if len(args) == 0 || args[0] != "create" {
		return errors.New("usage: invite create [--uses N] [--ttl 168h] [--note text]")
	}
	fl := flag.NewFlagSet("invite create", flag.ExitOnError)
	dataDir := fl.String("data-dir", defaultDataDir(), "data directory")
	uses := fl.Int("uses", 1, "how many signups may use it")
	ttl := fl.Duration("ttl", 7*24*time.Hour, "validity")
	note := fl.String("note", "", "note")
	_ = fl.Parse(args[1:])
	s, err := open(*dataDir)
	if err != nil {
		return err
	}
	defer s.Close()
	_, code, err := s.CreateInviteCode("", *note, *uses, *ttl)
	if err != nil {
		return err
	}
	fmt.Println(code)
	return nil
}

// ---------------------------------------------------------------------------
// downloads
// ---------------------------------------------------------------------------

func downloadsCmd(args []string) error {
	if len(args) < 1 || args[0] != "import" {
		return errors.New("usage: crowdvm-server downloads import [--data-dir DIR] [--downloads-dir DIR] <dist dir>")
	}
	fl := flag.NewFlagSet("downloads import", flag.ExitOnError)
	dataDir := fl.String("data-dir", defaultDataDir(), "data directory")
	dst := fl.String("downloads-dir", "", "target directory (default <data-dir>/downloads)")
	_ = fl.Parse(args[1:])
	if fl.NArg() != 1 {
		return errors.New("usage: crowdvm-server downloads import [--data-dir DIR] [--downloads-dir DIR] <dist dir>")
	}
	if *dst == "" {
		*dst = filepath.Join(*dataDir, "downloads")
	}
	files, err := server.ImportDownloads(fl.Arg(0), *dst)
	for _, f := range files {
		fmt.Println(filepath.Join(*dst, f))
	}
	return err
}

// parseGrantFlags also accepts --reason after the user and amount.
func parseGrantFlags(fl *flag.FlagSet, args []string) error {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") && fl.Lookup(strings.SplitN(strings.TrimLeft(arg, "-"), "=", 2)[0]) != nil {
			flags = append(flags, arg)
			if !strings.Contains(arg, "=") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		} else {
			positional = append(positional, arg)
		}
	}
	return fl.Parse(append(flags, positional...))
}

// splitList splits a comma-separated flag value, dropping empty items.
func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
