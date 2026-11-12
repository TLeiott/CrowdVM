// Command crowdvm-guest is the agent inside every CrowdVM VM. It runs as root
// from crowdvm-guest.service:
//
//	crowdvm-guest --config /run/crowdvm/cfg/config.json
//
// For tests it also runs on a host: --config FILE --home DIR --unprivileged.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/crowdvm/crowdvm/internal/guest"
	"github.com/crowdvm/crowdvm/internal/proto"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "preinit" {
		// PID 1 before systemd (image v4): unlock the encrypted disk.
		err := guest.Preinit()
		fmt.Fprintf(os.Stderr, "crowdvm-preinit: %v; powering off\n", err)
		time.Sleep(3 * time.Second) // the console shows the reason
		syscall.Sync()
		_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
		os.Exit(1)
	}
	cfgPath := flag.String("config", "/run/crowdvm/cfg/config.json", "guest config (proto.GuestConfig JSON)")
	home := flag.String("home", "", "sandbox home directory (default /home/user; required with --unprivileged)")
	dataDir := flag.String("data-dir", "", "agent state directory (default /var/lib/crowdvm, or HOME/.local/state/crowdvm-guest when unprivileged)")
	unpriv := flag.Bool("unprivileged", false, "run on a host as the current user (tests): skip root-only boot tasks")
	listen := flag.String("listen", "", "listen address (overrides config listenAddr; default :7070)")
	version := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *version {
		fmt.Println(guest.Version)
		return
	}
	logger := log.New(os.Stderr, "crowdvm-guest: ", log.LstdFlags)

	var cfg proto.GuestConfig
	b, err := os.ReadFile(*cfgPath)
	if err != nil {
		logger.Fatalf("read config: %v", err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		logger.Fatalf("parse config: %v", err)
	}
	if *listen != "" {
		cfg.ListenAddr = *listen
	}
	cli := filepath.Join(filepath.Dir(*cfgPath), "crowdvm") // optional, on the config drive
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if cfg.Boot == proto.BootWarm { // node warm pool VM (SIZE-012)
		if ad, ok := guest.LoadAdopted(*dataDir); ok {
			cfg = ad
		} else if cfg, err = guest.RunWarm(ctx, cfg, *dataDir, logger); err != nil {
			logger.Fatal(err)
		}
		if *listen != "" {
			cfg.ListenAddr = *listen
		}
	} else if err := guest.ClearAdopted(*dataDir); err != nil { // bugs-r13-opus 1
		logger.Fatalf("remove the warm pool config: %v", err)
	}
	var ch *guest.Channel
	if cfg.Channel != nil {
		// Secure layer: the config drive is a boot stub; the config comes
		// from the server over the pinned guest channel.
		state := *dataDir
		if state == "" && *unpriv {
			state = filepath.Join(*home, ".local/state/crowdvm-guest")
		} else if state == "" {
			// RAM only: a session on the disk would let the node read it
			// and fetch the config. A reboot gets a new ticket instead.
			state = "/run/crowdvm"
			_ = os.Remove("/var/lib/crowdvm/channel.json") // written by earlier versions
		}
		if cfg, ch, err = guest.ConnectChannel(ctx, cfg, state, logger); err != nil {
			logger.Fatal(err)
		}
		if *listen != "" {
			cfg.ListenAddr = *listen
		}
	}
	a, err := guest.New(guest.Options{Config: cfg, Home: *home, DataDir: *dataDir, Unprivileged: *unpriv, Logger: logger, CLIBinary: cli, Channel: ch})
	if err != nil {
		logger.Fatal(err)
	}
	if err := a.Run(ctx); err != nil {
		logger.Fatal(err)
	}
}
