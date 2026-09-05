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

	Storage     string // STORAGE: postgres | memory
	DatabaseURL string // DATABASE_URL; wins over the DB_* variables when set
	DBUser      string
	DBPass      string
	DBHost      string
	DBPort      string
	DBName      string
	DBSSLMode   string // DB_SSLMODE, default disable

	AutoMigrate bool   // AUTO_MIGRATE, default true
	LogLevel    string // LOG_LEVEL: debug | info | warn | error
	LogFormat   string // LOG_FORMAT: text | json
}

// Storage backends accepted by STORAGE.
const (
	StoragePostgres = "postgres"
	StorageMemory   = "memory"
)

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
		LogLevel:    strings.ToLower(env("LOG_LEVEL", "info")),
		LogFormat:   strings.ToLower(env("LOG_FORMAT", "text")),
	}
	auto, err := strconv.ParseBool(env("AUTO_MIGRATE", "true"))
	if err != nil {
		return c, fmt.Errorf("AUTO_MIGRATE: %w", err)
	}
	c.AutoMigrate = auto
	return c, c.Validate()
}

// Validate reports the first invalid field.
func (c Config) Validate() error {
	switch c.Storage {
	case StoragePostgres, StorageMemory:
	default:
		return fmt.Errorf("STORAGE: unknown backend %q (want postgres or memory)", c.Storage)
	}
	if _, err := c.SlogLevel(); err != nil {
		return err
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		return fmt.Errorf("LOG_FORMAT: unknown format %q (want text or json)", c.LogFormat)
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
