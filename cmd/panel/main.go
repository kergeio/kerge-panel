// Command panel runs the Kerge monitoring panel.
//
// Usage:
//
//	kerge-panel                    run the panel
//	kerge-panel healthcheck        exit 0 if the local panel is healthy
//	kerge-panel reset-admin [--yes] delete the admin account
//	kerge-panel add-host <name>    create a host and print its install token
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	_ "time/tzdata" // IANA time zones must be available without host files

	"github.com/kergeio/kerge-panel/internal/auth"
	"github.com/kergeio/kerge-panel/internal/hosts"
	"github.com/kergeio/kerge-panel/internal/i18n"
	"github.com/kergeio/kerge-panel/internal/ingest"
	"github.com/kergeio/kerge-panel/internal/metrics"
	"github.com/kergeio/kerge-panel/internal/rollup"
	"github.com/kergeio/kerge-panel/internal/settings"
	"github.com/kergeio/kerge-panel/internal/setup"
	"github.com/kergeio/kerge-panel/internal/status"
	"github.com/kergeio/kerge-panel/internal/store"
	"github.com/kergeio/kerge-panel/internal/web"
	webfiles "github.com/kergeio/kerge-panel/web"
)

const shutdownTimeout = 10 * time.Second

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	var err error
	switch args := os.Args[1:]; {
	case len(args) == 0:
		err = serve(logger)
	case args[0] == "healthcheck" && len(args) == 1:
		err = healthcheck(os.Getenv)
	case args[0] == "reset-admin":
		err = resetAdmin(args[1:], os.Stdin, os.Stdout)
	case args[0] == "add-host" && len(args) == 2:
		err = addHost(args[1], os.Stdout)
	default:
		fmt.Fprintln(os.Stderr, "usage: kerge-panel [healthcheck | reset-admin [--yes] | add-host <name>]")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "kerge-panel:", err)
		os.Exit(1)
	}
}

func openStore(ctx context.Context, cfg config) (*store.DB, error) {
	if err := checkDataDir(cfg.dataDir); err != nil {
		return nil, err
	}
	return store.Open(ctx, filepath.Join(cfg.dataDir, "kerge.db"))
}

// checkDataDir fails with instructions when the panel cannot create files in
// its data directory, which in the container usually means ./data on the
// host is not owned by the panel's user.
func checkDataDir(dir string) error {
	f, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return fmt.Errorf(`data directory %s is missing or not writable: %w

In the Docker deployment the panel runs as uid 65532 and needs to own the
data directory. On the host, run (with your install directory if it is not
the default /opt/kerge):

    sudo mkdir -p /opt/kerge/data && sudo chown -R 65532:65532 /opt/kerge/data

then start the panel again.`, dir, err)
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		return fmt.Errorf("data directory %s: %w", dir, err)
	}
	return os.Remove(name)
}

func serve(logger *slog.Logger) error {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	csrf, err := auth.LoadCSRF(cfg.dataDir)
	if err != nil {
		return err
	}
	setupSvc, code, err := setup.Start(ctx, db)
	if err != nil {
		return err
	}
	if code != "" {
		printSetupCode(os.Stdout, code)
	}

	locales, err := fs.Sub(webfiles.Files, "locales")
	if err != nil {
		return err
	}
	catalog, err := i18n.Load(locales, logger)
	if err != nil {
		return err
	}

	hostSvc := hosts.NewService(db)
	metricsSvc := metrics.NewService(db)
	settingsSvc := settings.NewService(db)
	liveStatus := status.New(status.Options{})
	rollupSvc, err := rollup.New(rollup.Options{DB: db, Logger: logger})
	if err != nil {
		return err
	}
	// The panel's time zone decides what "today" is, for reminders and
	// for the day traffic is counted on.
	timeZone := func() *time.Location {
		name, _, err := db.GetSetting(context.Background(), store.SettingTimeZone)
		if err != nil {
			logger.Error("read time zone setting", "err", err)
		}
		loc, err := time.LoadLocation(name)
		if err != nil {
			return time.UTC
		}
		return loc
	}

	ingestSvc, err := ingest.New(ingest.Options{
		DB: db, Hosts: hostSvc, Status: liveStatus, Logger: logger, TimeZone: timeZone,
	})
	if err != nil {
		return err
	}
	defer ingestSvc.Close()

	handler, err := web.New(web.Options{
		Files:         webfiles.Files,
		Catalog:       catalog,
		Auth:          auth.NewService(db),
		CSRF:          csrf,
		Setup:         setupSvc,
		Hosts:         hostSvc,
		Metrics:       metricsSvc,
		Settings:      settingsSvc,
		Status:        liveStatus,
		Agents:        ingestSvc,
		AgentEndpoint: cfg.agentEndpoint,
		ClientIP:      web.NewClientIPResolver(cfg.trustedProxies, logger),
		Logger:        logger,
		Language: func() string {
			lang, _, err := db.GetSetting(context.Background(), store.SettingLanguage)
			if err != nil {
				logger.Error("read language setting", "err", err)
			}
			return lang // unknown or empty values fall back to i18n.Default
		},
		TimeZone: timeZone,
		DateFormat: func() string {
			format, _, err := db.GetSetting(context.Background(), store.SettingDateFormat)
			if err != nil {
				logger.Error("read date format setting", "err", err)
			}
			return format // unknown or empty values fall back to the default
		},
		Health:  func(ctx context.Context) error { return db.Read().PingContext(ctx) },
		AgentWS: ingestSvc.Handler(),
	})
	if err != nil {
		return err
	}

	// The evaluator recomputes every state every few seconds, closes the
	// connection of a host it has written off and pushes the result to the
	// browsers watching.
	go liveStatus.Run(ctx, func(ctx context.Context) []int64 {
		records, err := hostSvc.List(ctx)
		if err != nil {
			logger.Error("listing the hosts for the evaluator", "err", err)
			return nil
		}
		ids := make([]int64, len(records))
		for i, rec := range records {
			ids[i] = rec.ID
		}
		return ids
	})

	// Summarizing and pruning run on the same writer as the samples
	// themselves.
	go rollupSvc.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() {
		logger.Info("panel listening", "addr", cfg.listen)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http shutdown: %w", err)
	}
	return nil
}

// printSetupCode writes the setup code to standard output in a fixed format
// that the installer reads from the container log.
func printSetupCode(w io.Writer, code string) {
	fmt.Fprintln(w, "Kerge is not set up yet. Open the panel in a browser and enter this code:")
	fmt.Fprintln(w, "Setup code: "+code)
}
