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

	"github.com/NicoMancinelli/notiphy/internal/activity"
	"github.com/NicoMancinelli/notiphy/internal/api"
	"github.com/NicoMancinelli/notiphy/internal/callback"
	"github.com/NicoMancinelli/notiphy/internal/config"
	"github.com/NicoMancinelli/notiphy/internal/router"
	"github.com/NicoMancinelli/notiphy/internal/store"
	"github.com/NicoMancinelli/notiphy/internal/transport"
)

// Settings keys for the persisted VAPID keypair.
const (
	settingVAPIDPublic  = "vapid_public_key"
	settingVAPIDPrivate = "vapid_private_key"
)

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var (
		cfgPath = fs.String("config", "", "path to a YAML config file")
		listen  = fs.String("listen", "", "bind address (overrides config)")
		dbPath  = fs.String("db", "", "SQLite database path (overrides config)")
		baseURL = fs.String("base-url", "", "externally reachable base URL (overrides config)")
		debug   = fs.Bool("debug", false, "enable debug logging")
	)
	fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *dbPath != "" {
		cfg.DB = *dbPath
	}
	if *baseURL != "" {
		cfg.BaseURL = *baseURL
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	st, err := store.Open(cfg.DB)
	if err != nil {
		return err
	}
	defer st.Close()

	// --- transports ---
	reg := transport.NewRegistry()
	reg.Register(transport.NewNtfy(cfg.NtfyDefaultServer))

	vapidPublic, err := ensureVAPID(st, &cfg, log)
	if err != nil {
		return err
	}
	if vapidPublic != "" {
		reg.Register(transport.NewWebPush(vapidPublic, cfg.VAPIDPrivateKey, cfg.VAPIDSubject))
	}

	if cfg.APNsEnabled() {
		ap, err := transport.NewAPNs(cfg.APNsKeyFile, cfg.APNsKeyID, cfg.APNsTeamID, cfg.APNsTopic, cfg.APNsProduct)
		if err != nil {
			return fmt.Errorf("APNs is configured but failed to initialise: %w", err)
		}
		reg.Register(ap)
		log.Info("APNs transport enabled — native Live Activities and one-tap buttons are available",
			"topic", cfg.APNsTopic, "production", cfg.APNsProduct)
	}

	// --- wiring ---
	hub := activity.NewHub()
	rt := router.New(st, reg, hub, cfg.BaseURL, cfg.ActivityProgressStep, log)
	cb := callback.New(st, cfg.CallbackUA, log)

	srv, err := api.New(cfg, st, reg, rt, hub, cb, vapidPublic, log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go cb.Run(ctx, 10*time.Second)
	go sweep(ctx, st, log)

	httpSrv := &http.Server{
		Addr:    cfg.Listen,
		Handler: srv.Handler(),
		// No WriteTimeout: SSE connections on /live/:id/stream are long-lived
		// and a write deadline would sever them mid-activity.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	printBanner(cfg, reg, log)

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// ensureVAPID loads the persisted Web Push keypair, generating it on first boot.
// The keys live in the database rather than the config file so a fresh install
// has working Web Push with no setup step — and so they survive config edits.
func ensureVAPID(st *store.Store, cfg *config.Config, log *slog.Logger) (string, error) {
	// An explicitly configured pair always wins.
	if cfg.VAPIDPublicKey != "" && cfg.VAPIDPrivateKey != "" {
		return cfg.VAPIDPublicKey, nil
	}

	pub, err := st.Setting(settingVAPIDPublic)
	if err != nil {
		return "", err
	}
	priv, err := st.Setting(settingVAPIDPrivate)
	if err != nil {
		return "", err
	}

	if pub != "" && priv != "" {
		cfg.VAPIDPrivateKey = priv
		cfg.VAPIDPublicKey = pub
		return pub, nil
	}

	priv, pub, err = transport.GenerateVAPIDKeys()
	if err != nil {
		return "", err
	}
	if err := st.SetSetting(settingVAPIDPrivate, priv); err != nil {
		return "", err
	}
	if err := st.SetSetting(settingVAPIDPublic, pub); err != nil {
		return "", err
	}

	cfg.VAPIDPrivateKey = priv
	cfg.VAPIDPublicKey = pub
	log.Info("generated a new Web Push (VAPID) keypair and stored it in the database")
	return pub, nil
}

// sweep expires stale responses and activities and trims the idempotency table.
func sweep(ctx context.Context, st *store.Store, log *slog.Logger) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()

	var sinceLastPurge time.Duration
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := st.ExpireDueResponses(); err != nil {
				log.Warn("expire responses failed", "err", err)
			} else if n > 0 {
				log.Info("expired pending responses", "count", n)
			}

			if n, err := st.ExpireDueActivities(); err != nil {
				log.Warn("expire activities failed", "err", err)
			} else if n > 0 {
				log.Info("ended expired activities", "count", n)
			}

			// Idempotency records only need to outlive plausible retries.
			sinceLastPurge += 30 * time.Second
			if sinceLastPurge >= time.Hour {
				sinceLastPurge = 0
				if n, err := st.PurgeIdempotency(48 * time.Hour); err != nil {
					log.Warn("purge idempotency failed", "err", err)
				} else if n > 0 {
					log.Info("purged old idempotency records", "count", n)
				}
			}
		}
	}
}

func printBanner(cfg config.Config, reg *transport.Registry, log *slog.Logger) {
	log.Info("notiphy listening",
		"addr", cfg.Listen,
		"baseURL", cfg.BaseURL,
		"db", cfg.DB,
		"transports", reg.Names(),
	)

	if cfg.AdminToken == "" {
		log.Warn("no admin_token is set: the dashboard and device registration are OPEN. " +
			"Anyone who can reach this server can register a device and receive your notifications. " +
			"Safe on a private network (Tailscale) only — set NOTIPHY_ADMIN_TOKEN before exposing it publicly")
	}

	if !reg.SupportsLiveActivity() {
		// Say this plainly at boot rather than letting the user discover it
		// when a Live Activity silently arrives as a plain notification.
		log.Info("Live Activities will render as a live web page at /live/:id — " +
			"native ActivityKit cards require the iOS app and a paid Apple Developer membership")
	}
	fmt.Fprintf(os.Stderr, "\n  Dashboard:  %s/\n  Add device: %s/subscribe\n\n", cfg.BaseURL, cfg.BaseURL)
}
