// Command meshycord bridges a MeshCore LoRa mesh to Discord.
//
// One process owns the radio link, the Discord Gateway, the routing core, the
// database and the web console. No separate daemon, no log tailing, no IPC.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"meshycord/internal/bridge"
	"meshycord/internal/config"
	"meshycord/internal/meshcore"
	"meshycord/internal/sdnotify"
	"meshycord/internal/server"
	"meshycord/internal/store"
)

// Version is set at build time with -ldflags "-X main.Version=v1.2.3".
var Version = "dev"

func main() {
	var (
		listen    = flag.String("listen", "0.0.0.0:9150", "address for the web console")
		dbPath    = flag.String("db", "/var/lib/meshycord/db.sqlite", "path to the database")
		logLevel  = flag.String("log-level", "info", "debug, info, warn or error")
		showVer   = flag.Bool("version", false, "print the version and exit")
		listPorts = flag.Bool("list-ports", false, "list serial devices that look like a MeshCore node, and exit")
		setToken  = flag.String("set-token", "", "store a Discord bot token and exit")
		setGuild  = flag.String("set-guild", "", "store a Discord server (guild) id and exit")
		setPass   = flag.String("set-password", "", "set the web console password and exit")
		setDevice = flag.String("set-serial", "", "store the serial device path and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("meshycord", Version)
		return
	}

	if *listPorts {
		ports, _ := meshcore.FindSerialPorts()
		if len(ports) == 0 {
			fmt.Println("No serial devices found. Plug the node in and try again.")
			return
		}
		for _, p := range ports {
			fmt.Println(p)
		}
		return
	}

	log := newLogger(*logLevel)
	slog.SetDefault(log)

	db, err := store.Open(*dbPath)
	if err != nil {
		log.Error("could not open the database", "path", *dbPath, "err", err)
		os.Exit(1)
	}
	defer db.Close()

	cfg, err := config.New(db)
	if err != nil {
		log.Error("could not read settings", "err", err)
		os.Exit(1)
	}

	// One-shot setup commands. These exist so a headless install can be
	// configured entirely from the shell, without ever opening the console —
	// which matters when the box has no screen and the console has no password
	// yet.
	//
	// Which flags were GIVEN, not which are non-empty: `-set-serial ""` means
	// "go back to auto-detecting the port", and treating that as absent made it
	// fall through and start a whole second bridge, which then fought the real
	// service for the listening port.
	given := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if handled := applySetupFlags(cfg, given, *setToken, *setGuild, *setPass, *setDevice); handled {
		return
	}

	if !cfg.Configured() {
		log.Warn("setup is not finished",
			"missing", strings.Join(cfg.MissingSettings(), ", "),
			"console", "http://"+hostPart(*listen)+"/settings")
	}
	if !cfg.HasPassword() {
		log.Warn("the web console has NO PASSWORD; anyone who can reach this machine can read " +
			"your message history and bot token. Set one on the Settings page, or with " +
			"--set-password.")
	}

	br := bridge.New(cfg, db, log)

	console, err := server.New(server.Options{
		Listen:  *listen,
		DBPath:  *dbPath,
		Version: Version,
	}, cfg, db, br, log)
	if err != nil {
		log.Error("could not start the web console", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("meshycord starting", "version", Version, "db", *dbPath, "console", *listen)
	db.LogEvent("info", "system", "meshycord "+Version+" started")

	done := make(chan struct{})
	go func() {
		defer close(done)
		br.Run(ctx)
	}()

	// The console is a management surface, not the job.
	//
	// If it cannot bind — the port is taken, most often by a second copy
	// started by hand — that must NOT take the bridge down with it. It used
	// to: the failure cancelled the whole context, systemd restarted the
	// process, and it wedged in a restart loop that dropped the radio link and
	// the Discord session every few seconds while the actual conflict went
	// unnoticed. Relaying messages does not depend on anyone looking at a web
	// page, so keep bridging and keep complaining.
	go func() {
		for {
			err := console.ListenAndServe()
			if ctx.Err() != nil {
				return
			}
			log.Error("the web console is not running; the bridge itself is unaffected",
				"listen", *listen, "err", err, "hint", listenHint(*listen, err))
			db.LogEvent("error", "system", "web console could not start: "+err.Error())
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
		}
	}()

	// Tell systemd we are up, then heartbeat.
	//
	// This is the whole replacement for the ESP32's watchdog scaffolding: no
	// feeding calls threaded through the application, just a goroutine with
	// nothing else to do. If the process wedges, systemd restarts it.
	_ = sdnotify.Ready()
	_ = sdnotify.Status("starting up")
	go runWatchdog(ctx, br, log)

	<-ctx.Done()
	log.Info("shutting down")
	_ = sdnotify.Stopping()
	db.LogEvent("info", "system", "meshycord stopping")

	// Give the bridge a moment to finish what it is doing. A transmission
	// mid-flight is airtime already spent; cutting it off wastes it.
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		log.Warn("bridge did not stop cleanly within 10s; exiting anyway")
	}
}

// runWatchdog heartbeats systemd and keeps the `systemctl status` line
// current, so the health of both links is visible without opening a browser.
func runWatchdog(ctx context.Context, br *bridge.Bridge, log *slog.Logger) {
	interval := sdnotify.WatchdogInterval()
	if interval <= 0 {
		interval = 30 * time.Second // no watchdog configured; still update the status line
	} else {
		log.Info("systemd watchdog active", "ping_every", interval)
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = sdnotify.Watchdog()
			st := br.Status("")
			radio, discord := "radio down", "discord down"
			if st.Mesh.Connected {
				radio = "radio up"
			}
			if st.DiscordUp {
				discord = "discord up"
			}
			_ = sdnotify.Status(fmt.Sprintf("%s, %s, %d links, %d messages today",
				radio, discord, st.Links, st.Stats.Last24h))
		}
	}
}

// applySetupFlags handles the one-shot configuration commands. Reports whether
// the process should exit rather than carry on to run.
//
// `given` says which flags actually appeared on the command line, so that
// setting something back to empty is a real instruction rather than a no-op.
func applySetupFlags(cfg *config.Store, given map[string]bool, token, guild, password, device string) bool {
	handled := false
	if given["set-serial"] {
		if err := cfg.SetSerialDevice(device); err != nil {
			fmt.Fprintln(os.Stderr, "could not store the device:", err)
			os.Exit(1)
		}
		if err := cfg.SetTransport(config.TransportSerial); err != nil {
			fmt.Fprintln(os.Stderr, "could not select the serial transport:", err)
			os.Exit(1)
		}
		if device == "" {
			fmt.Println("Serial device cleared — the port will be auto-detected.")
		} else {
			fmt.Println("Serial device stored, and the serial transport selected.")
		}
		handled = true
	}
	if given["set-token"] {
		if err := cfg.SetBotToken(token); err != nil {
			fmt.Fprintln(os.Stderr, "could not store the token:", err)
			os.Exit(1)
		}
		if token == "" {
			fmt.Println("Bot token cleared.")
		} else {
			fmt.Println("Bot token stored.")
		}
		handled = true
	}
	if given["set-guild"] {
		if err := cfg.SetGuildID(guild); err != nil {
			fmt.Fprintln(os.Stderr, "could not store the server id:", err)
			os.Exit(1)
		}
		if guild == "" {
			fmt.Println("Server (guild) id cleared.")
		} else {
			fmt.Println("Server (guild) id stored.")
		}
		handled = true
	}
	if given["set-password"] {
		if err := cfg.SetPassword(password); err != nil {
			fmt.Fprintln(os.Stderr, "could not set the password:", err)
			os.Exit(1)
		}
		fmt.Println("Console password set. Every existing session is signed out.")
		handled = true
	}
	return handled
}

// newLogger writes to stderr, which systemd routes to the journal. Nothing is
// written to its own log file: on a Pi, an application log on the SD card is
// avoidable wear, and journald already rotates and compresses.
func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch strings.ToLower(level) {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: l,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			// systemd stamps every line already, so a second timestamp is
			// noise that makes journalctl output harder to read.
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
}

// listenHint turns a bind failure into something actionable.
//
// The default console port is 915 — chosen for the 915 MHz LoRa band — and
// anything below 1024 is privileged on Linux. Under systemd that is handled
// (the unit grants CAP_NET_BIND_SERVICE), but somebody running the binary by
// hand as themselves just sees "permission denied", which says nothing about
// why a port they can obviously reach will not open.
func listenHint(listen string, err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
		if port := portOf(listen); port > 0 && port < 1024 {
			return fmt.Sprintf("port %d is privileged: run under systemd, or as root, or grant the "+
				"binary the capability once with `sudo setcap cap_net_bind_service=+ep $(command -v meshycord)`, "+
				"or pick a port above 1024 with -listen", port)
		}
	}
	if errors.Is(err, syscall.EADDRINUSE) {
		return "something else already holds that address; `sudo ss -lptn \"sport = :" +
			strconv.Itoa(portOf(listen)) + "\"` will say what"
	}
	return ""
}

func portOf(listen string) int {
	_, p, err := net.SplitHostPort(listen)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return 0
	}
	return n
}

func hostPart(listen string) string {
	if strings.HasPrefix(listen, "0.0.0.0") {
		return "<this-machine>" + strings.TrimPrefix(listen, "0.0.0.0")
	}
	return listen
}
