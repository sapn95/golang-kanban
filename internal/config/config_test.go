package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func lookup(m map[string]string) Lookup {
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	c, err := FromEnv(lookup(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr() != ":17808" {
		t.Errorf("Addr = %q", c.Addr())
	}
	if c.Storage != StoragePostgres || !c.AutoMigrate || c.LogLevel != "info" || c.LogFormat != "text" {
		t.Errorf("unexpected defaults: %+v", c)
	}
	if c.SQLitePath != "/data/kanban.db" {
		t.Errorf("SQLitePath = %q", c.SQLitePath)
	}
	want := "postgres://user:password@postgres:5432/kanban?sslmode=disable"
	if got := c.DSN(); got != want {
		t.Errorf("DSN = %q, want %q", got, want)
	}
}

func TestOverrides(t *testing.T) {
	c, err := FromEnv(lookup(map[string]string{
		"LISTEN_ADDR":  "127.0.0.1:9000",
		"STORAGE":      "Memory",
		"DB_USER":      "k@n",
		"DB_PASS":      "p:w/d",
		"DB_HOST":      "db.local",
		"DB_PORT":      "6543",
		"DB_NAME":      "board",
		"DB_SSLMODE":   "require",
		"AUTO_MIGRATE": "false",
		"LOG_LEVEL":    "DEBUG",
		"LOG_FORMAT":   "json",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr() != "127.0.0.1:9000" {
		t.Errorf("Addr = %q", c.Addr())
	}
	if c.Storage != StorageMemory || c.AutoMigrate {
		t.Errorf("unexpected: %+v", c)
	}
	want := "postgres://k%40n:p%3Aw%2Fd@db.local:6543/board?sslmode=require"
	if got := c.DSN(); got != want {
		t.Errorf("DSN = %q, want %q", got, want)
	}
	if lvl, _ := c.SlogLevel(); lvl != slog.LevelDebug {
		t.Errorf("level = %v", lvl)
	}
	var buf bytes.Buffer
	c.Logger(&buf).Info("hello")
	if !strings.HasPrefix(buf.String(), "{") {
		t.Errorf("json logger expected, got %q", buf.String())
	}
	buf.Reset()
	c.LogFormat = "text"
	c.Logger(&buf).Info("hello")
	if !strings.Contains(buf.String(), "msg=hello") {
		t.Errorf("text logger expected, got %q", buf.String())
	}
}

func TestSQLite(t *testing.T) {
	c, err := FromEnv(lookup(map[string]string{"STORAGE": "SQLite"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Storage != StorageSQLite {
		t.Errorf("Storage = %q", c.Storage)
	}
	if c.SQLitePath != "/data/kanban.db" {
		t.Errorf("SQLitePath = %q", c.SQLitePath)
	}
	c, err = FromEnv(lookup(map[string]string{"STORAGE": "sqlite", "SQLITE_PATH": "/srv/board.db"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.SQLitePath != "/srv/board.db" {
		t.Errorf("SQLitePath = %q", c.SQLitePath)
	}
}

func TestDatabaseURLWins(t *testing.T) {
	c, err := FromEnv(lookup(map[string]string{"DATABASE_URL": "postgres://x@y/z", "DB_HOST": "ignored"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.DSN() != "postgres://x@y/z" {
		t.Errorf("DSN = %q", c.DSN())
	}
}

func TestInvalid(t *testing.T) {
	cases := map[string]map[string]string{
		"STORAGE":      {"STORAGE": "oracle"},
		"AUTO_MIGRATE": {"AUTO_MIGRATE": "maybe"},
		"LOG_LEVEL":    {"LOG_LEVEL": "loud"},
		"LOG_FORMAT":   {"LOG_FORMAT": "xml"},
		"SERVER_PORT":  {"SERVER_PORT": "http"},
		"LISTEN_ADDR":  {"LISTEN_ADDR": "nocolon"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := FromEnv(lookup(env))
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("want error mentioning %s, got %v", name, err)
			}
		})
	}
	for _, lvl := range []string{"warn", "warning", "error"} {
		if _, err := FromEnv(lookup(map[string]string{"LOG_LEVEL": lvl})); err != nil {
			t.Errorf("%s: %v", lvl, err)
		}
	}
}
