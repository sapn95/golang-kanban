// Package config reads the process configuration from environment variables.
// It is the only package that looks at the environment.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Config is the effective configuration of one kanban process.
type Config struct {
	ListenAddr string // LISTEN_ADDR; wins over SERVER_PORT when set
	ServerPort string // SERVER_PORT, default 17808

	Storage     string // STORAGE: postgres | sqlite | memory
	DatabaseURL string // DATABASE_URL; wins over the DB_* variables when set
	DBUser      string
	DBPass      string
	DBHost      string
	DBPort      string
	DBName      string
	DBSSLMode   string // DB_SSLMODE, default disable
	SQLitePath  string // SQLITE_PATH, default /data/kanban.db

	// AUTH_MODE: none | proxy | access. See internal/identity for why proxy
	// and access are not interchangeable.
	AuthMode         string
	AuthHeader       string // AUTH_HEADER, the header read in proxy mode
	AccessTeamDomain string // ACCESS_TEAM_DOMAIN, e.g. team.cloudflareaccess.com
	AccessAudience   string // ACCESS_AUD, the Access application's AUD tag

	// AVATARS: who has a picture, as address=github-login pairs. Empty means
	// the board draws initials and makes no outbound request.
	Avatars map[string]string

	AutoMigrate bool   // AUTO_MIGRATE, default true
	LogLevel    string // LOG_LEVEL: debug | info | warn | error
	LogFormat   string // LOG_FORMAT: text | json
}

// Storage backends accepted by STORAGE.
const (
	StoragePostgres = "postgres"
	StorageSQLite   = "sqlite"
	StorageMemory   = "memory"
)

// Authentication modes accepted by AUTH_MODE.
const (
	AuthNone   = "none"
	AuthProxy  = "proxy"
	AuthAccess = "access"
)

// The image declares VOLUME ["/data"] for this file, so mounting something
// there is the whole of the setup SQLite needs.
const defaultSQLitePath = "/data/kanban.db"

// Lookup returns the value of an environment variable; the empty string means
// unset. os.Getenv satisfies it.
type Lookup func(key string) string

// FromEnv builds a Config from the environment. Unset variables take their
// defaults; invalid values are reported with the variable name.
func FromEnv(get Lookup) (Config, error) {
	if get == nil {
		get = os.Getenv
	}
	env := func(key, def string) string {
		if v := strings.TrimSpace(get(key)); v != "" {
			return v
		}
		return def
	}
	c := Config{
		ListenAddr:  env("LISTEN_ADDR", ""),
		ServerPort:  env("SERVER_PORT", "17808"),
		Storage:     strings.ToLower(env("STORAGE", StoragePostgres)),
		DatabaseURL: env("DATABASE_URL", ""),
		DBUser:      env("DB_USER", "user"),
		DBPass:      env("DB_PASS", "password"),
		DBHost:      env("DB_HOST", "postgres"),
		DBPort:      env("DB_PORT", "5432"),
		DBName:      env("DB_NAME", "kanban"),
		DBSSLMode:   env("DB_SSLMODE", "disable"),
		SQLitePath:  env("SQLITE_PATH", defaultSQLitePath),

		AuthMode:         strings.ToLower(env("AUTH_MODE", AuthNone)),
		AuthHeader:       env("AUTH_HEADER", "X-Forwarded-Email"),
		AccessTeamDomain: strings.TrimSuffix(strings.TrimPrefix(env("ACCESS_TEAM_DOMAIN", ""), "https://"), "/"),
		AccessAudience:   env("ACCESS_AUD", ""),
		LogLevel:         strings.ToLower(env("LOG_LEVEL", "info")),
		LogFormat:        strings.ToLower(env("LOG_FORMAT", "text")),
	}
	auto, err := strconv.ParseBool(env("AUTO_MIGRATE", "true"))
	if err != nil {
		return c, fmt.Errorf("AUTO_MIGRATE: %w", err)
	}
	c.AutoMigrate = auto
	avatars, err := parseAvatars(env("AVATARS", ""))
	if err != nil {
		return c, err
	}
	c.Avatars = avatars
	return c, c.Validate()
}

// parseAvatars reads "somebody@example.com=their-login, other@example.com=theirs"
// into a map. A GitHub noreply address carries the login after the plus sign, so
// for those the pair is a copy of two things that are already on screen.
//
// A wrong pair is an error rather than a pair that is skipped: a picture that
// never appears is the kind of thing nobody investigates, and the process would
// otherwise be the only place that knows why.
func parseAvatars(spec string) (map[string]string, error) {
	if spec == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(spec, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		addr, login, ok := strings.Cut(pair, "=")
		addr, login = strings.ToLower(strings.TrimSpace(addr)), strings.TrimSpace(login)
		if !ok || addr == "" || login == "" {
			return nil, fmt.Errorf("AVATARS: %q is not an address=login pair", pair)
		}
		if !isGitHubLogin(login) {
			return nil, fmt.Errorf("AVATARS: %q is not a GitHub login", login)
		}
		out[addr] = login
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// isGitHubLogin reports whether s is shaped like a GitHub account name: up to 39
// letters, digits or single hyphens. It is checked here because the login ends
// up in a URL this process fetches.
func isGitHubLogin(s string) bool {
	if s == "" || len(s) > 39 || strings.HasPrefix(s, "-") || strings.HasSuffix(s, "-") || strings.Contains(s, "--") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

// Validate reports the first invalid field.
func (c Config) Validate() error {
	switch c.Storage {
	case StoragePostgres, StorageSQLite, StorageMemory:
	default:
		return fmt.Errorf("STORAGE: unknown backend %q (want postgres, sqlite or memory)", c.Storage)
	}
	if _, err := c.SlogLevel(); err != nil {
		return err
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		return fmt.Errorf("LOG_FORMAT: unknown format %q (want text or json)", c.LogFormat)
	}
	switch c.AuthMode {
	case AuthNone, AuthProxy:
	case AuthAccess:
		// Both are load-bearing. Without the team domain there is nowhere
		// to fetch signing keys; without the audience, a token minted for
		// any other application on the same team would be accepted here.
		if c.AccessTeamDomain == "" {
			return fmt.Errorf("ACCESS_TEAM_DOMAIN: required when AUTH_MODE is %q", AuthAccess)
		}
		if c.AccessAudience == "" {
			return fmt.Errorf("ACCESS_AUD: required when AUTH_MODE is %q", AuthAccess)
		}
	default:
		return fmt.Errorf("AUTH_MODE: unknown mode %q (want none, proxy or access)", c.AuthMode)
	}
	if c.AuthMode == AuthProxy && c.AuthHeader == "" {
		return fmt.Errorf("AUTH_HEADER: required when AUTH_MODE is %q", AuthProxy)
	}
	if c.ListenAddr == "" {
		if _, err := strconv.Atoi(c.ServerPort); err != nil {
			return fmt.Errorf("SERVER_PORT: %q is not a port", c.ServerPort)
		}
	} else if _, _, err := net.SplitHostPort(c.ListenAddr); err != nil {
		return fmt.Errorf("LISTEN_ADDR: %w", err)
	}
	return nil
}

// Addr is the address the HTTP server listens on.
func (c Config) Addr() string {
	if c.ListenAddr != "" {
		return c.ListenAddr
	}
	return ":" + c.ServerPort
}

// DSN is the Postgres connection string.
func (c Config) DSN() string {
	if c.DatabaseURL != "" {
		return c.DatabaseURL
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(c.DBUser, c.DBPass),
		Host:     net.JoinHostPort(c.DBHost, c.DBPort),
		Path:     "/" + c.DBName,
		RawQuery: "sslmode=" + url.QueryEscape(c.DBSSLMode),
	}
	return u.String()
}

// SlogLevel maps LogLevel to slog.
func (c Config) SlogLevel() (slog.Level, error) {
	switch c.LogLevel {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return slog.LevelInfo, fmt.Errorf("LOG_LEVEL: unknown level %q", c.LogLevel)
}

// Logger builds the process logger for this configuration.
func (c Config) Logger(w interface{ Write([]byte) (int, error) }) *slog.Logger {
	level, _ := c.SlogLevel()
	opts := &slog.HandlerOptions{Level: level}
	if c.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// AccessIssuer is the iss claim Cloudflare puts on assertions for this team.
func (c Config) AccessIssuer() string { return "https://" + c.AccessTeamDomain }

// AccessCertsURL is where the team's signing keys are published.
func (c Config) AccessCertsURL() string {
	return c.AccessIssuer() + "/cdn-cgi/access/certs"
}
