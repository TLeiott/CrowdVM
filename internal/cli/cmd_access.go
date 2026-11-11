package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/client"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

// sshKeyPath is ~/.ssh/crowdvm_ed25519. A key from older versions in the
// config directory keeps being used; an explicit CROWDVM_CONFIG_DIR keeps
// the key there too (isolated setups and tests).
func (a *App) sshKeyPath() string {
	legacy := filepath.Join(a.configDir(), "id_ed25519")
	if fileExists(legacy) || a.ConfigDir != "" || a.Getenv("CROWDVM_CONFIG_DIR") != "" {
		return legacy
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".ssh", "crowdvm_ed25519")
	}
	return legacy
}

// sshKey returns the CLI's private key path, generating the pair on first use.
func (a *App) sshKey() (keyPath, pub string, err error) {
	keyPath = a.sshKeyPath()
	dir := filepath.Dir(keyPath)
	if b, err := os.ReadFile(keyPath + ".pub"); err == nil && fileExists(keyPath) {
		return keyPath, strings.TrimSpace(string(b)), nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	block, err := ssh.MarshalPrivateKey(sk, "crowdvm")
	if err != nil {
		return "", "", err
	}
	sp, err := ssh.NewPublicKey(pk)
	if err != nil {
		return "", "", err
	}
	pub = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))) + " crowdvm"
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(keyPath+".pub", []byte(pub+"\n"), 0o644); err != nil {
		return "", "", err
	}
	return keyPath, pub, nil
}

// sshOptions authorizes the CLI key on the sandbox and returns the ssh -o
// options (ProxyCommand through `crowdvm tunnel`) and the login user.
func (a *App) sshOptions(ctx context.Context, c *client.Client, id string) ([]string, string, error) {
	keyPath, pub, err := a.sshKey()
	if err != nil {
		return nil, "", err
	}
	res, err := c.AddSSHKey(ctx, id, pub)
	if err != nil {
		return nil, "", err
	}
	kh := filepath.Join(a.configDir(), "known_hosts")
	strict := "accept-new"
	if res.HostKey != "" {
		if err := setKnownHost(kh, id, res.HostKey); err == nil {
			strict = "yes"
		}
	}
	proxy := []string{quoteExe(a.Exe)}
	if a.apiURL != "" {
		proxy = append(proxy, "--api-url", a.apiURL)
	}
	if a.profileF != "" {
		proxy = append(proxy, "--profile", a.profileF)
	}
	proxy = append(proxy, "tunnel", "%h", "22")
	opts := []string{
		"-i", keyPath,
		"-o", "IdentitiesOnly=yes",
		"-o", "ProxyCommand=" + strings.Join(proxy, " "),
		"-o", "UserKnownHostsFile=" + kh,
		"-o", "StrictHostKeyChecking=" + strict,
		"-o", "ServerAliveInterval=30",
		"-o", "LogLevel=ERROR",
	}
	return opts, firstNonEmpty(res.SSHUser, "user"), nil
}

func quoteExe(p string) string {
	if strings.ContainsAny(p, " \t'\"") {
		return `"` + p + `"`
	}
	return p
}

// setKnownHost replaces the known_hosts entry for host.
func setKnownHost(file, host, key string) error {
	key = strings.TrimSpace(key)
	if f := strings.Fields(key); len(f) >= 3 && !strings.HasPrefix(f[0], "ssh-") && !strings.HasPrefix(f[0], "ecdsa-") {
		key = strings.Join(f[1:], " ") // "host keytype key" form
	}
	var keep []string
	if b, err := os.ReadFile(file); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if l == "" || strings.HasPrefix(l, host+" ") {
				continue
			}
			keep = append(keep, l)
		}
	}
	keep = append(keep, host+" "+key)
	return os.WriteFile(file, []byte(strings.Join(keep, "\n")+"\n"), 0o600)
}

// runTool runs an external program attached to the CLI's stdio and returns
// its exit code.
func (a *App) runTool(name string, args []string) error {
	bin, err := a.LookPath(name)
	if err != nil {
		return fmt.Errorf("%s not found in PATH; install OpenSSH (or use `crowdvm exec`/`crowdvm scp --api`)", name)
	}
	c := exec.Command(bin, args...)
	c.Stdin, c.Stdout, c.Stderr = a.In, a.Out, a.Err
	err = a.Run(c)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return exitError{ee.ExitCode()}
	}
	return err
}

func (a *App) sshCmd() *cobra.Command {
	var tty bool
	cmd := &cobra.Command{
		Use:   "ssh [-t] ID [--] [COMMAND...]",
		Short: "Open an SSH session in a sandbox, or run one command",
		Long: `Open an SSH session (system ssh through a crowdvm tunnel; the key lives in
the crowdvm config directory and is authorized automatically). For one command,
'crowdvm exec' is faster. Pass -t for full-screen tools.`,
		Example: `  crowdvm ssh bx_f7k2q9hd
  crowdvm ssh bx_f7k2q9hd "cd my-repo && npm test"
  crowdvm ssh bx_f7k2q9hd -- bash -s < ./setup.sh
  crowdvm ssh -t bx_f7k2q9hd -- tmux attach`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			first, rest, err := reparse(cmd, args)
			if err != nil {
				return err
			}
			c, id, err := a.clientAndID(first)
			if err != nil {
				return err
			}
			opts, user, err := a.sshOptions(cmd.Context(), c, id)
			if err != nil {
				return err
			}
			if tty {
				opts = append(opts, "-t")
			}
			// "--" before the destination keeps ssh from parsing the command's flags.
			opts = append(opts, "--", user+"@"+id)
			if len(rest) > 0 {
				opts = append(opts, commandLine(rest))
			}
			return a.runTool("ssh", opts)
		},
	}
	cmd.Flags().SetInterspersed(false)
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "give the remote command a terminal")
	return cmd
}

func (a *App) sshConfigCmd() *cobra.Command {
	var print bool
	cmd := &cobra.Command{
		Use:   "ssh-config ID",
		Short: "Write an OpenSSH config entry for a sandbox (for ssh, scp, VS Code, rsync)",
		Long: `Authorize this computer's key on the sandbox and write
~/.ssh/crowdvm/<id>.conf (Host <id>, ProxyCommand crowdvm tunnel, pinned host
key). Add 'Include crowdvm/*.conf' at the top of ~/.ssh/config once; then
'ssh <id>', 'scp file <id>:' and VS Code Remote-SSH work. --print only prints it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, id, err := a.clientAndID(args[0])
			if err != nil {
				return err
			}
			opts, user, err := a.sshOptions(cmd.Context(), c, id)
			if err != nil {
				return err
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Host %s\n  HostName %s\n  User %s\n", id, id, user)
			for i := 0; i+1 < len(opts); i += 2 {
				switch opts[i] {
				case "-i":
					fmt.Fprintf(&b, "  IdentityFile %s\n", opts[i+1])
				case "-o":
					k, v, _ := strings.Cut(opts[i+1], "=")
					fmt.Fprintf(&b, "  %s %s\n", k, v)
				}
			}
			conf := b.String()
			if print {
				a.rawOutput(cmd)
				fmt.Fprint(a.Out, conf)
				return nil
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			dir := filepath.Join(home, ".ssh", "crowdvm")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			path := filepath.Join(dir, id+".conf")
			if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
				return err
			}
			included := false
			if bb, err := os.ReadFile(filepath.Join(home, ".ssh", "config")); err == nil {
				included = strings.Contains(string(bb), "crowdvm/*.conf")
			}
			return a.result(map[string]any{"id": id, "path": path, "included": included}, func() {
				a.printf("wrote %s\n", path)
				if !included {
					a.printf("add this line at the top of ~/.ssh/config:\n  Include crowdvm/*.conf\n")
				}
				a.printf("then: ssh %s\n", id)
			})
		},
	}
	cmd.Flags().BoolVar(&print, "print", false, "print the entry instead of writing it")
	return cmd
}

func (a *App) scpCmd() *cobra.Command {
	var recursive, useAPI bool
	cmd := &cobra.Command{
		Use:   "scp [-r] SRC... DST",
		Short: "Copy files to or from a sandbox (ID:/path is the remote side)",
		Long: `Copy files with the system scp through a crowdvm tunnel. Without scp (for
example on Windows without OpenSSH), or with --api, files go through the
API instead. Relative remote paths start in /home/user.`,
		Example: `  crowdvm scp bx_f7k2q9hd:/home/user/output.zip ./output.zip
  crowdvm scp ./local-file.txt bx_f7k2q9hd:
  crowdvm scp -r ./my-project bx_f7k2q9hd:/home/user/`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var srcs []location
			for _, s := range args[:len(args)-1] {
				srcs = append(srcs, parseLocation(s))
			}
			dst := parseLocation(args[len(args)-1])
			id := dst.id
			for _, s := range srcs {
				if s.remote == dst.remote {
					return usagef("copy between this computer and one sandbox: exactly one side must be ID:/path")
				}
				if s.remote {
					if id != "" && s.id != id {
						return usagef("all sources must come from the same sandbox")
					}
					id = s.id
				}
			}
			c, id, err := a.clientAndID(id)
			if err != nil {
				return err
			}
			resolve := func(l location) location {
				if l.remote {
					l.id = id
				}
				return l
			}
			for i := range srcs {
				srcs[i] = resolve(srcs[i])
			}
			dst = resolve(dst)
			if _, err := a.LookPath("scp"); err != nil || useAPI {
				return a.apiCopy(cmd.Context(), c, srcs, dst, recursive)
			}
			opts, user, err := a.sshOptions(cmd.Context(), c, id)
			if err != nil {
				return err
			}
			if recursive {
				opts = append(opts, "-r")
			}
			for _, l := range append(srcs, dst) {
				if l.remote {
					opts = append(opts, firstNonEmpty(l.user, user)+"@"+id+":"+l.path)
				} else {
					opts = append(opts, l.path)
				}
			}
			return a.runTool("scp", opts)
		},
	}
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "copy directories recursively")
	cmd.Flags().BoolVar(&useAPI, "api", false, "copy through the API instead of scp")
	return cmd
}

func (a *App) tunnelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tunnel ID PORT",
		Short: "Bridge stdin/stdout to a TCP port in the sandbox (ssh ProxyCommand)",
		Long: `Connect stdin/stdout to 127.0.0.1:PORT inside the sandbox. This is the
ProxyCommand that 'crowdvm ssh' uses; you can use it in ~/.ssh/config:

  Host bx_*
    User user
    IdentityFile ~/.ssh/crowdvm_ed25519
    ProxyCommand crowdvm tunnel %h 22

('crowdvm ssh-config ID' writes such an entry for you.) The tunnel ends when
the sandbox side closes; end of input on stdin is not passed on yet.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, id, err := a.clientAndID(args[0])
			if err != nil {
				return err
			}
			port, err := strconv.Atoi(args[1])
			if err != nil {
				return usagef("bad port %q", args[1])
			}
			conn, err := c.Tunnel(cmd.Context(), id, port)
			if err != nil {
				return err
			}
			return client.Pipe(stdio{a.In, a.Out}, conn)
		},
	}
}

type stdio struct {
	io.Reader
	io.Writer
}

func (a *App) forwardCmd() *cobra.Command {
	var remote, local int
	var bind string
	var reverse bool
	cmd := &cobra.Command{
		Use:     "forward ID",
		Aliases: []string{"port-forward"},
		Short:   "Forward a sandbox TCP port to this computer (or --reverse)",
		Example: `  crowdvm forward bx_f7k2q9hd --remote 5432
  crowdvm forward bx_f7k2q9hd --remote 3000 --local 8080 --bind 0.0.0.0
  crowdvm forward bx_f7k2q9hd --reverse --local 7777   # your :7777 at 127.0.0.1:7777 in the sandbox`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, id, err := a.clientAndID(args[0])
			if err != nil {
				return err
			}
			if reverse {
				if local == 0 {
					return usagef("--reverse needs --local PORT")
				}
				if remote == 0 {
					remote = local
				}
				return a.reverseForward(cmd.Context(), c, id, bind, remote, local)
			}
			if remote == 0 {
				return usagef("--remote PORT is required")
			}
			if local == 0 {
				local = remote
			}
			return a.forward(cmd.Context(), c, id, net.JoinHostPort(bind, strconv.Itoa(local)), remote)
		},
	}
	cmd.Flags().IntVar(&remote, "remote", 0, "port in the sandbox")
	cmd.Flags().IntVar(&local, "local", 0, "port on this computer (default: same as --remote)")
	cmd.Flags().StringVar(&bind, "bind", "127.0.0.1", "bind address (in the sandbox with --reverse)")
	cmd.Flags().BoolVar(&reverse, "reverse", false, "make a port on this computer reachable inside the sandbox")
	return cmd
}

// reverseForward runs `ssh -R` and reconnects after drops (network loss,
// stop/resume). It gives up after 5 quick failures in a row.
func (a *App) reverseForward(ctx context.Context, c *client.Client, id, bind string, remote, local int) error {
	spec := fmt.Sprintf("%s:%d:127.0.0.1:%d", bind, remote, local)
	a.info("127.0.0.1:%d on this computer → %s:%d in %s (Ctrl+C to stop)\n", local, bind, remote, id)
	quick := 0
	for {
		start := time.Now()
		err := func() error {
			opts, user, err := a.sshOptions(ctx, c, id)
			if err != nil {
				return err
			}
			opts = append(opts, "-N", "-o", "ExitOnForwardFailure=yes", "-o", "ServerAliveCountMax=3", "-R", spec, user+"@"+id)
			return a.runTool("ssh", opts)
		}()
		if err == nil || ctx.Err() != nil {
			return nil
		}
		var ee exitError
		if !errors.As(err, &ee) && !client.IsStatus(err, 409) && !client.IsStatus(err, 502) && !client.IsStatus(err, 503) {
			return err // not a dropped connection (e.g. ssh missing, unauthorized)
		}
		if time.Since(start) < 10*time.Second {
			if quick++; quick >= 5 {
				return err
			}
		} else {
			quick = 0
		}
		a.info("reverse forward dropped (%v); reconnecting...\n", err)
		if err := sleepCtx(ctx, 2*time.Second); err != nil {
			return nil
		}
	}
}

// forward listens on addr and tunnels every connection to port in the sandbox.
func (a *App) forward(ctx context.Context, c *client.Client, id, addr string, port int) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer ln.Close()
	go func() { <-ctx.Done(); ln.Close() }()
	ev := map[string]any{"event": "forwarding", "id": id, "local": ln.Addr().String(), "remote": port}
	if a.jsonOut {
		a.emit(ev)
	} else {
		a.printf("%s → %s:%d (Ctrl+C to stop)\n", ln.Addr(), id, port)
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() {
			defer conn.Close()
			tctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			remote, err := c.Tunnel(tctx, id, port)
			cancel()
			if err != nil {
				fmt.Fprintf(a.Err, "tunnel: %v\n", err)
				return
			}
			_ = client.Pipe(conn, remote)
		}()
	}
}

// hostArgs splits [ID] PORT: the id may be left out (current, or self
// inside a sandbox).
func hostArgs(args []string) (id, port string) {
	if len(args) == 2 {
		return args[0], args[1]
	}
	return "", args[0]
}

func parsePort(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, usagef("bad port %q", s)
	}
	return n, nil
}

func (a *App) hostCmd() *cobra.Command {
	var title string
	var public, private, localhost bool
	cmd := &cobra.Command{
		Use:   "host [ID] PORT",
		Short: "Expose a sandbox port on a stable URL (also: host list|url|hide)",
		Long: `Expose a service in the sandbox on a URL served by the CrowdVM server
(http://<id>-<port>.<domain> or http://<server>/p/<id>/<port>/). Private URLs
carry ?_token=; --public drops the token. Start the service first, e.g.
crowdvm exec ID --detach "npm run dev". Running it again returns the same URL.

Without ID the current sandbox is used (inside a sandbox: the sandbox itself).`,
		Example: `  crowdvm host bx_f7k2q9hd 3000
  crowdvm host bx_f7k2q9hd 3000 --public --title "Login preview"
  crowdvm host list bx_f7k2q9hd
  BACKEND_URL=$(crowdvm host url bx_f7k2q9hd 3001)
  crowdvm host hide bx_f7k2q9hd 3000`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.rawOutput(cmd) // Boat: prints the URL, also when captured ($(host 3000)); --json for the object
			idArg, portArg := hostArgs(args)
			c, id, err := a.clientAndID(idArg)
			if err != nil {
				return err
			}
			port, err := parsePort(portArg)
			if err != nil {
				return err
			}
			if public && private {
				return usagef("--public and --private cannot be combined")
			}
			res, err := c.HostPort(cmd.Context(), id, &client.HostPortRequest{Port: port, Public: public, Localhost: localhost, Title: title})
			if err != nil {
				return err
			}
			if res.DirectEndpoint != "" {
				// like Boat, `host --public` opens the TCP port on the sandbox's IP: let it through the guest firewall
				a.openFirewall(cmd.Context(), c, id, []client.DirectPort{{Port: port, Proto: "tcp"}})
			}
			return a.result(res, func() { a.printf("%s\n", res.URL) })
		},
	}
	cmd.Flags().StringVar(&title, "title", "", "display title")
	cmd.Flags().BoolVar(&public, "public", false, "no token needed to open the URL")
	cmd.Flags().BoolVar(&private, "private", false, "require the URL token (default)")
	cmd.Flags().BoolVar(&localhost, "localhost", false, "the service listens on localhost only")

	var all bool
	list := &cobra.Command{
		Use: "list [ID]", Aliases: []string{"ls"}, Short: "List the hosted ports (tokens are not printed; see host url)", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, id, err := a.clientAndID(arg0(args))
			if err != nil {
				return err
			}
			a.rawOutput(cmd) // Boat: the table, also when captured (agents, commands API)
			ports, err := c.ListHostedPorts(cmd.Context(), id, all)
			if err != nil {
				return err
			}
			// host list never prints the access token (Boat); host url does.
			for i := range ports {
				ports[i].URL, ports[i].HostURL, ports[i].PathURL = noToken(ports[i].URL), noToken(ports[i].HostURL), noToken(ports[i].PathURL)
			}
			if a.jsonOut {
				a.emit(map[string]any{"sandboxId": id, "ports": ports})
				return nil
			}
			if len(ports) == 0 {
				a.info("no hosted ports on %s\n", id)
				return nil
			}
			var rows [][]string
			for _, p := range ports {
				u, _, _ := strings.Cut(p.URL, "?")
				var marks []string
				if p.IsProtected {
					marks = append(marks, "(gated)")
				}
				if p.Localhost {
					marks = append(marks, "(localhost)")
				}
				if p.Hidden {
					marks = append(marks, "(hidden)")
				}
				rows = append(rows, []string{strconv.Itoa(p.Port), u, strings.Join(marks, " "), p.Title})
			}
			table(a.Out, "PORT\tURL\t\tTITLE", rows)
			return nil
		},
	}
	list.Flags().BoolVar(&all, "all", false, "include hidden ports")
	var urlPublic, urlLocalhost bool
	urlCmd := &cobra.Command{
		Use: "url [ID] PORT", Short: "Print the full URL of a port (hosting it first if needed)", Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.rawOutput(cmd) // BACKEND_URL=$(host url 3001) gets the bare URL
			idArg, portArg := hostArgs(args)
			c, id, err := a.clientAndID(idArg)
			if err != nil {
				return err
			}
			port, err := parsePort(portArg)
			if err != nil {
				return err
			}
			var u string
			if !urlPublic && !urlLocalhost {
				if hp, err := c.GetHostedPort(cmd.Context(), id, port); err == nil {
					u = hp.URL
				} else if !client.IsCode(err, "port_not_hosted") && !client.IsStatus(err, 404) {
					return err
				}
			}
			if u == "" {
				res, err := c.HostPort(cmd.Context(), id, &client.HostPortRequest{Port: port, Public: urlPublic, Localhost: urlLocalhost})
				if err != nil {
					return err
				}
				u = res.URL
			}
			return a.result(map[string]any{"sandboxId": id, "port": port, "url": u}, func() { a.printf("%s\n", u) })
		},
	}
	urlCmd.Flags().BoolVar(&urlPublic, "public", false, "host the port without a token first")
	urlCmd.Flags().BoolVar(&urlLocalhost, "localhost", false, "the service listens on localhost only")
	hide := &cobra.Command{
		Use: "hide [ID] PORT", Short: "Take a port's URL down (the token is kept)", Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.rawOutput(cmd)
			idArg, portArg := hostArgs(args)
			c, id, err := a.clientAndID(idArg)
			if err != nil {
				return err
			}
			port, err := parsePort(portArg)
			if err != nil {
				return err
			}
			r, err := c.HideHostedPort(cmd.Context(), id, port)
			if err != nil {
				return err
			}
			return a.result(r, func() { a.printf("%s: port %d hidden\n", id, port) })
		},
	}
	cmd.AddCommand(list, urlCmd, hide)
	return cmd
}

func (a *App) browserCmd() *cobra.Command {
	var profile string
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "browser ID",
		Short: "Open a stream of only the sandbox's Chrome (no desktop)",
		Long: `Print and open a URL that streams only Chrome from the running sandbox
(tabs and address bar, no desktop). Running it again keeps the live stream.
The URL carries a token: keep it secret.

--profile picks the Chrome user data dir: an absolute path on the sandbox,
outside /home/user and system directories; it is created for you and kept in
snapshots and forks. (Here --profile is the Chrome profile; choose a config
profile with CROWDVM_PROFILE.)`,
		Example: `  crowdvm browser bx_f7k2q9hd
  crowdvm browser bx_f7k2q9hd --profile /data/chrome-profile`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, id, err := a.clientAndID(args[0])
			if err != nil {
				return err
			}
			if profile != "" && !strings.HasPrefix(profile, "/") {
				return usagef("--profile must be an absolute path on the sandbox")
			}
			res, err := c.Browser(cmd.Context(), id, profile)
			if err != nil {
				return err
			}
			u := firstNonEmpty(res.BrowserURL, res.ViewerURL)
			if u == "" {
				return &client.Error{Code: "browser_unavailable", Message: "the server returned no browser URL; retry"}
			}
			if !noOpen && !a.jsonOut {
				_ = a.OpenURL(u)
			}
			return a.result(map[string]any{"id": id, "browserUrl": u, "profile": res.Profile, "expiresAt": res.ExpiresAt}, func() {
				a.printf("%s\n", u)
			})
		},
	}
	cmd.Flags().StringVar(&profile, "profile", "", "Chrome user data dir on the sandbox (absolute path)")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "print the URL only")
	return cmd
}

func (a *App) desktopCmd() *cobra.Command {
	var noOpen, public bool
	cmd := &cobra.Command{
		Use:   "desktop ID",
		Short: "Open the sandbox desktop (noVNC) in your browser",
		Long:  "Print and open the desktop URL. It carries a token: treat it as a secret.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, id, err := a.clientAndID(args[0])
			if err != nil {
				return err
			}
			var res *client.DesktopResult
			for i := 0; ; i++ {
				res, err = c.Desktop(cmd.Context(), id, &client.DesktopRequest{PublicAccess: public}, true)
				if err != nil {
					return err
				}
				if !res.Provisioning || res.DesktopURL != "" || i > 120 {
					break
				}
				a.info("%s\n", firstNonEmpty(res.Message, "preparing the desktop..."))
				if err := sleepCtx(cmd.Context(), 2*a.Poll); err != nil {
					return err
				}
			}
			if res.DesktopURL == "" {
				return &client.Error{Code: "desktop_unavailable", Message: firstNonEmpty(res.Message, "no desktop URL yet; retry")}
			}
			if !noOpen && !a.jsonOut {
				_ = a.OpenURL(res.DesktopURL)
			}
			return a.result(map[string]any{"id": id, "desktopUrl": res.DesktopURL, "viewerUrl": res.DesktopURL, "mode": res.Mode}, func() {
				a.printf("%s\n", res.DesktopURL)
			})
		},
	}
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "print the URL only")
	cmd.Flags().BoolVar(&public, "public", false, "allow access without a signed-in session")
	return cmd
}

// openFirewall allows the ports through the guest firewall (ufw); best effort.
func (a *App) openFirewall(ctx context.Context, c *client.Client, id string, ports []client.DirectPort) {
	var rules []string
	for _, p := range ports {
		rules = append(rules, fmt.Sprintf("sudo -n ufw allow %d/%s >/dev/null", p.Port, firstNonEmpty(p.Proto, "tcp")))
	}
	if len(rules) == 0 {
		return
	}
	cmd := "command -v ufw >/dev/null || exit 0; " + strings.Join(rules, " && ")
	if res, err := c.Exec(ctx, id, client.CommandRequest{Command: cmd, TimeoutSeconds: 60}); err != nil || client.ExitCode(res.ExitCode) != 0 {
		a.info("note: could not open the guest firewall; run in the sandbox: sudo ufw allow <port>/<proto>\n")
	}
}

func (a *App) directPortsCmd() *cobra.Command {
	var clear, noFirewall bool
	cmd := &cobra.Command{
		Use:   "direct-ports ID [PORT[/udp]...]",
		Short: "Show or set the raw TCP/UDP ports published on the node's direct host",
		Long: `Publish guest ports raw (TCP or UDP) on the LAN/Tailscale address of a node
started with --direct-host, like Boat's "raw TCP or UDP ports" on the sandbox IP.
Without ports it shows the published set (also the ports of host --public).
Ports given replace the earlier list; --clear removes them. The guest firewall
is opened for each port unless --no-firewall.`,
		Example: `  crowdvm direct-ports bx_f7k2q9hd
  crowdvm direct-ports bx_f7k2q9hd 27015/udp 25565
  crowdvm direct-ports bx_f7k2q9hd --clear`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, id, err := a.clientAndID(args[0])
			if err != nil {
				return err
			}
			var res *client.DirectPorts
			if len(args) == 1 && !clear {
				res, err = c.DirectPorts(cmd.Context(), id)
			} else {
				var ports []client.DirectPort
				for _, s := range args[1:] {
					num, proto, _ := strings.Cut(s, "/")
					n, err := parsePort(num)
					if err != nil {
						return err
					}
					if proto != "" && proto != "tcp" && proto != "udp" {
						return usagef("%s: protocol must be tcp or udp", s)
					}
					ports = append(ports, client.DirectPort{Port: n, Proto: firstNonEmpty(proto, "tcp")})
				}
				if res, err = c.SetDirectPorts(cmd.Context(), id, ports); err == nil && !noFirewall {
					a.openFirewall(cmd.Context(), c, id, ports)
				}
			}
			if err != nil {
				return err
			}
			return a.result(res, func() {
				if len(res.Ports) == 0 {
					a.printf("no direct ports on %s\n", id)
				}
				for _, p := range res.Ports {
					a.printf("%d/%s\t%s\t%s\n", p.Port, firstNonEmpty(p.Proto, "tcp"), p.Endpoint, p.Source)
				}
			})
		},
	}
	cmd.Flags().BoolVar(&clear, "clear", false, "remove the ports asked for")
	cmd.Flags().BoolVar(&noFirewall, "no-firewall", false, "do not open the guest firewall for the ports")
	return cmd
}

// noToken drops the _token query parameter from a hosted-port URL.
func noToken(u string) string {
	base, q, ok := strings.Cut(u, "?")
	if !ok {
		return u
	}
	var keep []string
	for _, p := range strings.Split(q, "&") {
		if !strings.HasPrefix(p, "_token=") && p != "" {
			keep = append(keep, p)
		}
	}
	if len(keep) == 0 {
		return base
	}
	return base + "?" + strings.Join(keep, "&")
}
