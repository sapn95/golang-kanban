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
	"time"
)

// Config is the effective configuration of one kanban process.
//
// The `env` tag names the variable a field is read from. It is what Settings
// walks for `kanban doctor`, so a field without one is a field the doctor does
// not report; a test asserts that every tag names a variable FromEnv actually
// reads. `secret` marks a value that must not be printed: true masks it, url
// keeps the address and takes out the password.
type Config struct {
	ListenAddr string `env:"LISTEN_ADDR"` // wins over SERVER_PORT when set
	ServerPort string `env:"SERVER_PORT"` // default 17808

	Storage     string `env:"STORAGE"`                   // postgres | sqlite | memory
	DatabaseURL string `env:"DATABASE_URL" secret:"url"` // wins over the DB_* variables when set
	DBUser      string `env:"DB_USER"`
	DBPass      string `env:"DB_PASS" secret:"true"`
	DBHost      string `env:"DB_HOST"`
	DBPort      string `env:"DB_PORT"`
	DBName      string `env:"DB_NAME"`
	DBSSLMode   string `env:"DB_SSLMODE"`  // default disable
	SQLitePath  string `env:"SQLITE_PATH"` // default /data/kanban.db

	// AUTH_MODE: none | proxy | access. See internal/identity for why proxy
	// and access are not interchangeable.
	AuthMode         string `env:"AUTH_MODE"`
	AuthHeader       string `env:"AUTH_HEADER"`        // the header read in proxy mode
	AccessTeamDomain string `env:"ACCESS_TEAM_DOMAIN"` // e.g. team.cloudflareaccess.com
	AccessAudience   string `env:"ACCESS_AUD"`         // the Access application's AUD tag

	// AVATARS: who has a picture, as address=github-login pairs. Empty means
	// the board draws initials and makes no outbound request.
	Avatars map[string]string `env:"AVATARS"`

	// BACKUP_*: where the scheduler puts snapshots and how often. Neither a
	// directory nor a bucket means no schedule, and `kanban export` is then the
	// only way a snapshot gets taken.
	BackupInterval   time.Duration `env:"BACKUP_INTERVAL"` // default 24h; 0 disables the schedule
	BackupKeep       int           `env:"BACKUP_KEEP"`     // default 7; 0 keeps every snapshot
	BackupDir        string        `env:"BACKUP_DIR"`      // a directory on a volume
	BackupS3Bucket   string        `env:"BACKUP_S3_BUCKET"`
	BackupS3Prefix   string        `env:"BACKUP_S3_PREFIX"`   // normalised to end in /
	BackupS3Region   string        `env:"BACKUP_S3_REGION"`   // or AWS_REGION
	BackupS3Endpoint string        `env:"BACKUP_S3_ENDPOINT"` // empty for AWS; anything else is addressed path-style

	// The usual AWS names, so a deployment that already injects credentials for
	// something else does not need a second set under our own names.
	AWSAccessKeyID     string `env:"AWS_ACCESS_KEY_ID"`
	AWSSecretAccessKey string `env:"AWS_SECRET_ACCESS_KEY" secret:"true"`
	AWSSessionToken    string `env:"AWS_SESSION_TOKEN" secret:"true"` // for temporary credentials

	AutoMigrate bool   `env:"AUTO_MIGRATE"` // default true
	LogLevel    string `env:"LOG_LEVEL"`    // debug | info | warn | error
	LogFormat   string `env:"LOG_FORMAT"`   // text | json
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

		BackupDir:          env("BACKUP_DIR", ""),
		BackupS3Bucket:     env("BACKUP_S3_BUCKET", ""),
		BackupS3Prefix:     env("BACKUP_S3_PREFIX", ""),
		BackupS3Region:     env("BACKUP_S3_REGION", env("AWS_REGION", "")),
		BackupS3Endpoint:   strings.TrimSuffix(env("BACKUP_S3_ENDPOINT", ""), "/"),
		AWSAccessKeyID:     env("AWS_ACCESS_KEY_ID", ""),
		AWSSecretAccessKey: env("AWS_SECRET_ACCESS_KEY", ""),
		AWSSessionToken:    env("AWS_SESSION_TOKEN", ""),
	}
	auto, err := strconv.ParseBool(env("AUTO_MIGRATE", "true"))
	if err != nil {
		return c, fmt.Errorf("AUTO_MIGRATE: %w", err)
	}
	c.AutoMigrate = auto
	interval, err := time.ParseDuration(env("BACKUP_INTERVAL", "24h"))
	if err != nil {
		return c, fmt.Errorf("BACKUP_INTERVAL: %w (a number needs a unit, as in 24h or 30m)", err)
	}
	c.BackupInterval = interval
	keep, err := strconv.Atoi(env("BACKUP_KEEP", "7"))
	if err != nil {
		return c, fmt.Errorf("BACKUP_KEEP: %w", err)
	}
	c.BackupKeep = keep
	// A prefix names a folder, and every caller of it joins with no separator.
	if c.BackupS3Prefix != "" && !strings.HasSuffix(c.BackupS3Prefix, "/") {
		c.BackupS3Prefix += "/"
	}
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
	return c.validateBackup()
}

// minBackupInterval is the shortest schedule that is not a mistake. Every
// snapshot reads every board, so a minute is already generous.
const minBackupInterval = time.Minute

// BackupScheduled reports whether serve should run the snapshot schedule: a
// target and an interval. A target with BACKUP_INTERVAL=0 is a schedule that
// has been turned off without taking the configuration apart.
func (c Config) BackupScheduled() bool {
	return c.BackupInterval > 0 && (c.BackupDir != "" || c.BackupS3Bucket != "")
}

func (c Config) validateBackup() error {
	if c.BackupDir != "" && c.BackupS3Bucket != "" {
		return fmt.Errorf("BACKUP_DIR and BACKUP_S3_BUCKET: set one, not both; two targets would need two retention policies and there is one BACKUP_KEEP")
	}
	if c.BackupKeep < 0 {
		return fmt.Errorf("BACKUP_KEEP: %d is negative; 0 keeps every snapshot", c.BackupKeep)
	}
	if c.BackupInterval < 0 {
		return fmt.Errorf("BACKUP_INTERVAL: %s is negative; 0 disables the schedule", c.BackupInterval)
	}
	if c.BackupInterval > 0 && c.BackupInterval < minBackupInterval {
		return fmt.Errorf("BACKUP_INTERVAL: %s is shorter than %s", c.BackupInterval, minBackupInterval)
	}
	if c.BackupS3Bucket == "" {
		return nil
	}
	// Checked whether or not the schedule is on: a bucket named with no way to
	// reach it is a mistake either way, and it is cheaper to find here than at
	// midnight.
	if c.BackupS3Region == "" {
		return fmt.Errorf("BACKUP_S3_REGION: required with BACKUP_S3_BUCKET; it is part of the request signature, and S3-compatible servers that ignore it still want one")
	}
	if c.AWSAccessKeyID == "" || c.AWSSecretAccessKey == "" {
		return fmt.Errorf("AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY: required with BACKUP_S3_BUCKET")
	}
	if !isSafeKeyPrefix(c.BackupS3Prefix) {
		return fmt.Errorf("BACKUP_S3_PREFIX: %q; use letters, digits, dots, dashes, underscores and slashes, and do not start with one", c.BackupS3Prefix)
	}
	if c.BackupS3Endpoint != "" {
		u, err := url.Parse(c.BackupS3Endpoint)
		if err != nil {
			return fmt.Errorf("BACKUP_S3_ENDPOINT: %w", err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("BACKUP_S3_ENDPOINT: %q is not an http or https URL", c.BackupS3Endpoint)
		}
		// A gateway may sit under a path, and that path is signed along with the
		// key, so it is held to the same characters for the same reason.
		if !isSafeKeyPrefix(strings.TrimPrefix(strings.TrimSuffix(u.Path, "/"), "/")) {
			return fmt.Errorf("BACKUP_S3_ENDPOINT: path %q; use letters, digits, dots, dashes, underscores and slashes", u.Path)
		}
		if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return fmt.Errorf("BACKUP_S3_ENDPOINT: %q; a scheme, a host and at most a path, no query, fragment or credentials", c.BackupS3Endpoint)
		}
	}
	return nil
}

// isSafeKeyPrefix keeps the prefix to characters that need no escaping in a
// signed request, so the canonical form of a URL and the URL that is sent are
// the same string.
func isSafeKeyPrefix(prefix string) bool {
	if strings.HasPrefix(prefix, "/") || strings.Contains(prefix, "//") || strings.Contains(prefix, "..") {
		return false
	}
	for _, r := range prefix {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == '/':
		default:
			return false
		}
	}
	return true
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
