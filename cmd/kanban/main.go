// Command kanban runs the board. With no arguments it serves HTTP, so the
// Docker CMD and existing deployments keep working.
//
//	kanban [serve]    run the HTTP server
//	kanban migrate    apply pending schema migrations and exit
//	kanban export     write a snapshot of every board as JSON
//	kanban import     read a snapshot back in
//	kanban doctor     print the configuration and check what it points at
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
	// The zone database, in the binary. A board's office hours are local hours,
	// and time.LoadLocation on a host without /usr/share/zoneinfo falls back to
	// UTC, which is a response-time badge that is an hour or two out and says
	// nothing about why. Half a megabyte to make the zone name mean what it
	// says wherever the binary runs.
	_ "time/tzdata"

	"kanban/internal/api"
	"kanban/internal/config"
	"kanban/internal/identity"
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
	_, _ = io.WriteString(w, `usage: kanban [serve|migrate|export|import|doctor|version|help]
  serve                     run the HTTP server (default)
  migrate                   apply pending schema migrations and exit
  export [-o file]          write a snapshot of every board to stdout or a file
  import [-replace] [file]  read a snapshot back in, from stdin or a file
                            -dry-run checks the file and writes nothing
  doctor [-timeout 10s]     print the configuration and check the store, the
                            identity provider and the backup target
  version                   print the version
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
	case "doctor":
		// Ahead of the configuration and the store on purpose: a configuration
		// that does not validate is the thing doctor is there to print, and
		// AUTO_MIGRATE must not turn a diagnostic into a write.
		return doctorCmd(ctx, args[1:], getenv, stdout, stderr)
	case "serve", "migrate", "export", "import":
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
	switch cmd {
	case "migrate":
		return 0
	case "export":
		return exportCmd(ctx, st, args[1:], stdout, stderr)
	case "import":
		return importCmd(ctx, st, args[1:], stdout, stderr)
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

	waitForBackups, err := startBackups(ctx, cfg, st, log)
	if err != nil {
		log.Error("backup target", "err", err)
		return 1
	}
	defer waitForBackups()

	// One handler tree: the pages, with the JSON API mounted under /api/v1/ and
	// wrapped in the same middleware, so both read identity from the same place.
	// This is the only package that imports both.
	srv := &http.Server{
		Handler: identityMiddleware(cfg, log)(web.New(svc, st.Ping, log,
			web.WithBuild(version, commit), web.WithAvatars(cfg.Avatars),
			web.WithAPI(api.New(svc, log)))),
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

// identityMiddleware builds the request-identity layer from the configuration.
// In none mode it is a pass-through, so the board works with no identity
// provider at all and the rest of the app does not have to know the
// difference: every handler reads identity.FromContext and gets the zero user.
func identityMiddleware(cfg config.Config, log *slog.Logger) func(http.Handler) http.Handler {
	ic := identity.Config{Mode: identity.Mode(cfg.AuthMode), Header: cfg.AuthHeader}
	if cfg.AuthMode == config.AuthAccess {
		ic.Verifier = &identity.AccessVerifier{
			CertsURL: cfg.AccessCertsURL(),
			Audience: cfg.AccessAudience,
			Issuer:   cfg.AccessIssuer(),
		}
		// Logged rather than fatal: an assertion that fails to verify is
		// usually a key rotation or an expired session, and turning either
		// into an outage would be worse than serving the page anonymously.
		ic.OnError = func(err error) { log.Warn("access assertion rejected", "err", err) }
	}
	if cfg.AuthMode != config.AuthNone {
		log.Info("request identity enabled", "mode", cfg.AuthMode)
	}
	return identity.Middleware(ic)
}
