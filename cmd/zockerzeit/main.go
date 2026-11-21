// Command zockerzeit tracks and limits play time by blocking devices on a
// UniFi WLAN.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // the container image ships without zoneinfo

	"github.com/slauger/zockerzeit/internal/config"
	"github.com/slauger/zockerzeit/internal/demo"
	"github.com/slauger/zockerzeit/internal/engine"
	"github.com/slauger/zockerzeit/internal/fritzbox"
	"github.com/slauger/zockerzeit/internal/nintendo"
	"github.com/slauger/zockerzeit/internal/notify"
	"github.com/slauger/zockerzeit/internal/secret"
	"github.com/slauger/zockerzeit/internal/store"
	"github.com/slauger/zockerzeit/internal/unifi"
	"github.com/slauger/zockerzeit/internal/web"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config.yaml", "path to the configuration file")
	demoMode := flag.Bool("demo", false, "run with a simulated UniFi controller and sample data")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	var cfg config.Config
	var err error
	if *demoMode {
		cfg = config.Default()
		cfg.Database = "zockerzeit-demo.db"
		cfg.DryRun = false
		if v := os.Getenv("ZOCKERZEIT_LISTEN"); v != "" {
			cfg.Listen = v
		}
	} else if cfg, err = config.Load(*configPath); err != nil {
		return err
	}
	loc := cfg.Location()

	st, err := store.Open(cfg.Database)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = st.Close() }()

	var ctrl engine.Controller
	if *demoMode {
		log.Warn("demo mode: using a simulated UniFi controller")
		if err := demo.Seed(st, time.Now().In(loc)); err != nil {
			return fmt.Errorf("seed demo data: %w", err)
		}
		if err := demo.SeedNintendo(st, time.Now().In(loc)); err != nil {
			return fmt.Errorf("seed demo data: %w", err)
		}
		ctrl = demo.NewController()
	} else {
		ctrl, err = unifi.New(unifi.Config{
			URL: cfg.UniFi.URL, Site: cfg.UniFi.Site, UniFiOS: cfg.UniFi.UniFiOS, APIKey: cfg.UniFi.APIKey,
			Username: cfg.UniFi.Username, Password: cfg.UniFi.Password, InsecureSkipVerify: cfg.UniFi.InsecureSkipVerify,
		})
		if err != nil {
			return err
		}
	}

	var internet engine.InternetBlocker
	if cfg.Fritzbox.URL != "" && !*demoMode {
		fb, err := fritzbox.New(fritzbox.Config{URL: cfg.Fritzbox.URL, Username: cfg.Fritzbox.Username, Password: cfg.Fritzbox.Password})
		if err != nil {
			return err
		}
		internet = fb
		log.Info("internet blocking via FRITZ!Box enabled", "url", cfg.Fritzbox.URL)
	}

	var notifier engine.Notifier
	if cfg.Notify.NtfyURL != "" {
		notifier = notify.NewNtfy(cfg.Notify.NtfyURL, cfg.Notify.NtfyToken)
	}

	eng := engine.New(st, ctrl, notifier, engine.Options{
		Interval: cfg.PollInterval, DryRun: cfg.DryRun, MinBytesPerMinute: cfg.MinBytesPerMinute,
		Location: loc, WarnMinutes: cfg.WarnMinutes, RetentionDays: cfg.RetentionDays, Internet: internet,
	}, log)

	if cfg.Auth.AdminPassword == "" {
		log.Warn("no admin password configured: the parent UI is open to everyone on the network")
	}
	webOpts := web.Options{AdminPassword: cfg.Auth.AdminPassword, SessionSecret: cfg.Auth.SessionSecret, Version: version}
	var syncer *nintendo.Syncer
	switch {
	case *demoMode:
	case cfg.Auth.SessionSecret == "":
		log.Warn("Nintendo integration disabled: set ZOCKERZEIT_SESSION_SECRET to store tokens encrypted")
	default:
		box, err := secret.NewBox([]byte(cfg.Auth.SessionSecret), "nintendo")
		if err != nil {
			return err
		}
		syncer = nintendo.NewSyncer(st, nintendo.New(cfg.Timezone), box, 10*time.Minute, log)
		syncer.Location = loc
		webOpts.Nintendo = syncer
	}
	handler, err := web.New(st, eng, webOpts, log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go eng.Run(ctx)
	if syncer != nil {
		go syncer.Run(ctx)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Listen, "version", version, "dryRun", cfg.DryRun)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
