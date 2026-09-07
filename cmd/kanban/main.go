// Command kanban runs the board. With no arguments it serves HTTP, so the
// Docker CMD and existing deployments keep working.
//
//	kanban [serve]    run the HTTP server
//	kanban migrate    apply pending schema migrations and exit
//	kanban version    print the version
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"kanban/internal/config"
	"kanban/internal/service"
	"kanban/internal/store"
	"kanban/internal/store/memory"
	"kanban/internal/store/postgres"
	"kanban/internal/store/sqlite"
	"kanban/internal/web"
)

// Set with -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = "unknown"
)

// notifyListening is called with the bound address once serve is listening.
// Tests use it to find the port.
var notifyListening = func(addr net.Addr) {}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func usage(w io.Writer) {
	_, _ = io.WriteString(w, `usage: kanban [serve|migrate|version|help]
  serve     run the HTTP server (default)
  migrate   apply pending schema migrations and exit
  version   print the version
Configuration is read from the environment; see docs/architecture.md.
`)
}

func run(ctx context.Context, args []string, getenv config.Lookup, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "version", "-v", "--version":
		_, _ = fmt.Fprintf(stdout, "kanban %s (%s, %s)\n", version, commit, runtime.Version())
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	case "serve", "migrate":
	default:
		_, _ = fmt.Fprintf(stderr, "kanban: unknown command %q\n", cmd)
		usage(stderr)
		return 2
	}

	cfg, err := config.FromEnv(getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "kanban: configuration: %v\n", err)
		return 2
	}
	log := cfg.Logger(stderr)

	st, err := openStore(cfg)
	if err != nil {
		log.Error("open store", "storage", cfg.Storage, "err", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	if cmd == "migrate" || cfg.AutoMigrate {
		if err := st.Migrate(ctx); err != nil {
			log.Error("migrate", "storage", cfg.Storage, "err", err)
			return 1
		}
		log.Info("schema up to date", "storage", cfg.Storage)
	}
	if cmd == "migrate" {
		return 0
	}
	return serve(ctx, cfg, st, log)
}

func openStore(cfg config.Config) (store.Store, error) {
	switch cfg.Storage {
	case config.StorageMemory:
		return memory.New(), nil
	case config.StoragePostgres:
		return postgres.Open(cfg.DSN())
	case config.StorageSQLite:
		return sqlite.Open(cfg.SQLitePath)
	}
	return nil, fmt.Errorf("unknown storage %q", cfg.Storage)
}

func serve(ctx context.Context, cfg config.Config, st store.Store, log *slog.Logger) int {
	svc := service.New(st)
	boards, err := svc.EnsureDefaultBoard(ctx)
	if err != nil {
		log.Error("prepare default board", "err", err)
		return 1
	}
	log.Info("boards ready", "count", len(boards))

	srv := &http.Server{
		Handler:           web.New(svc, st.Ping, log),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	ln, err := net.Listen("tcp", cfg.Addr())
	if err != nil {
		log.Error("listen", "addr", cfg.Addr(), "err", err)
		return 1
	}
	log.Info("server started", "addr", ln.Addr().String(), "version", version, "storage", cfg.Storage)
	notifyListening(ln.Addr())

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "err", err)
			return 1
		}
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Error("shutdown", "err", err)
			return 1
		}
	}
	return 0
}
