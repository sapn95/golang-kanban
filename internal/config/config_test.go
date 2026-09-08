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

func TestAuthMode(t *testing.T) {
	base := map[string]string{"STORAGE": "memory"}
	with := func(kv map[string]string) Lookup {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range kv {
			m[k] = v
		}
		return func(key string) string { return m[key] }
	}

	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
		check   func(t *testing.T, c Config)
	}{
		{
			name: "defaults to none",
			env:  nil,
			check: func(t *testing.T, c Config) {
				if c.AuthMode != AuthNone {
					t.Errorf("AuthMode = %q, want %q", c.AuthMode, AuthNone)
				}
			},
		},
		{
			name: "proxy takes the default header",
			env:  map[string]string{"AUTH_MODE": "proxy"},
			check: func(t *testing.T, c Config) {
				if c.AuthHeader != "X-Forwarded-Email" {
					t.Errorf("AuthHeader = %q", c.AuthHeader)
				}
			},
		},
		{
			name:    "access without a team domain is refused",
			env:     map[string]string{"AUTH_MODE": "access", "ACCESS_AUD": "x"},
			wantErr: "ACCESS_TEAM_DOMAIN",
		},
		{
			// Without an audience any token from the same team would open
			// this app, so an empty one must not be silently allowed.
			name:    "access without an audience is refused",
			env:     map[string]string{"AUTH_MODE": "access", "ACCESS_TEAM_DOMAIN": "t.cloudflareaccess.com"},
			wantErr: "ACCESS_AUD",
		},
		{
			name:    "an unknown mode is refused",
			env:     map[string]string{"AUTH_MODE": "trust-me"},
			wantErr: "AUTH_MODE",
		},
		{
			name: "access derives the issuer and certs URL",
			env: map[string]string{
				"AUTH_MODE": "access", "ACCESS_TEAM_DOMAIN": "t.cloudflareaccess.com", "ACCESS_AUD": "abc",
			},
			check: func(t *testing.T, c Config) {
				if got, want := c.AccessIssuer(), "https://t.cloudflareaccess.com"; got != want {
					t.Errorf("AccessIssuer() = %q, want %q", got, want)
				}
				if got, want := c.AccessCertsURL(), "https://t.cloudflareaccess.com/cdn-cgi/access/certs"; got != want {
					t.Errorf("AccessCertsURL() = %q, want %q", got, want)
				}
			},
		},
		{
			// Pasting the team URL out of the dashboard is the obvious
			// mistake; it would otherwise produce https://https://...
			name: "a pasted URL is accepted as a team domain",
			env: map[string]string{
				"AUTH_MODE": "access", "ACCESS_TEAM_DOMAIN": "https://t.cloudflareaccess.com/", "ACCESS_AUD": "abc",
			},
			check: func(t *testing.T, c Config) {
				if got, want := c.AccessIssuer(), "https://t.cloudflareaccess.com"; got != want {
					t.Errorf("AccessIssuer() = %q, want %q", got, want)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := FromEnv(with(tt.env))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("accepted %v", tt.env)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if tt.check != nil {
				tt.check(t, c)
			}
		})
	}
}

func TestAvatars(t *testing.T) {
	t.Run("unset means no pictures", func(t *testing.T) {
		c, err := FromEnv(lookup(nil))
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Avatars) != 0 {
			t.Errorf("Avatars = %v, want none", c.Avatars)
		}
	})

	t.Run("pairs are read and the address is case-insensitive", func(t *testing.T) {
		c, err := FromEnv(lookup(map[string]string{
			"AVATARS": "Me@Example.com=sapn95, 116176330+NicolasHaas@users.noreply.github.com=NicolasHaas",
		}))
		if err != nil {
			t.Fatal(err)
		}
		if got := c.Avatars["me@example.com"]; got != "sapn95" {
			t.Errorf("Avatars[me@example.com] = %q", got)
		}
		if got := c.Avatars["116176330+nicolashaas@users.noreply.github.com"]; got != "NicolasHaas" {
			t.Errorf("the noreply address was not read: %v", c.Avatars)
		}
	})

	for _, bad := range []struct{ name, spec, want string }{
		{"no login", "me@example.com", "address=login pair"},
		{"empty login", "me@example.com=", "address=login pair"},
		{"no address", "=sapn95", "address=login pair"},
		// The login ends up in the URL this process fetches, so anything that
		// is not a GitHub account name is refused here rather than sent.
		{"a path", "me@example.com=../../etc/passwd", "not a GitHub login"},
		{"a host", "me@example.com=evil.example.com/x", "not a GitHub login"},
		{"an underscore", "me@example.com=some_body", "not a GitHub login"},
	} {
		t.Run("refused: "+bad.name, func(t *testing.T) {
			_, err := FromEnv(lookup(map[string]string{"AVATARS": bad.spec}))
			if err == nil {
				t.Fatalf("accepted %q", bad.spec)
			}
			if !strings.Contains(err.Error(), bad.want) || !strings.Contains(err.Error(), "AVATARS") {
				t.Errorf("error = %v, want it to name AVATARS and %q", err, bad.want)
			}
		})
	}
}
