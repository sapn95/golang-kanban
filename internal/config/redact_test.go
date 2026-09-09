package config

import (
	"reflect"
	"strings"
	"testing"
)

// probe is what a field is set to when the test wants to see whether FromEnv
// reads the variable its tag names. One per kind, and each one has to differ
// from that field's default.
func probe(f reflect.StructField) string {
	switch f.Type.Kind() {
	case reflect.Bool:
		return "false" // AUTO_MIGRATE defaults to true
	case reflect.Int, reflect.Int64:
		if f.Type.Name() == "Duration" {
			return "90m"
		}
		return "3"
	case reflect.Map:
		return "probe@example.com=probe"
	default:
		return "probe-x"
	}
}

// TestEveryEnvTagIsRead is what makes the tags safe to build a report on. A tag
// that names a variable FromEnv does not read would put a line in `kanban
// doctor` saying a setting is unset while the process is using it, which is
// worse than not reporting it at all.
//
// The validation error is ignored: a probe value is not a valid port or storage
// backend, and the question here is only whether the field arrived.
func TestEveryEnvTagIsRead(t *testing.T) {
	def, _ := FromEnv(func(string) string { return "" })
	defv := reflect.ValueOf(def)
	rt := defv.Type()
	tagged := 0
	for i := range rt.NumField() {
		field := rt.Field(i)
		name, ok := field.Tag.Lookup("env")
		if !ok {
			continue
		}
		tagged++
		t.Run(name, func(t *testing.T) {
			value := probe(field)
			got, _ := FromEnv(func(key string) string {
				if key == name {
					return value
				}
				return ""
			})
			was, now := defv.Field(i).Interface(), reflect.ValueOf(got).Field(i).Interface()
			if reflect.DeepEqual(was, now) {
				t.Errorf("%s=%q left %s at %v; the tag names a variable FromEnv does not read",
					name, value, field.Name, was)
			}
		})
	}
	if tagged < 20 {
		t.Errorf("only %d tagged fields; the tags have gone missing", tagged)
	}
}

// TestSecretsAreTagged catches the credential that gets added without a tag,
// which is the mistake that puts a password in a doctor report.
func TestSecretsAreTagged(t *testing.T) {
	rt := reflect.TypeOf(Config{})
	for i := range rt.NumField() {
		field := rt.Field(i)
		looksSecret := strings.Contains(field.Name, "Pass") ||
			strings.Contains(field.Name, "Secret") ||
			strings.Contains(field.Name, "Token") ||
			strings.Contains(field.Name, "URL")
		if looksSecret && field.Tag.Get("secret") == "" {
			t.Errorf("%s carries a credential and has no `secret` tag", field.Name)
		}
	}
}

func TestRedacted(t *testing.T) {
	c := Config{
		DatabaseURL:        "postgres://kanban:hunter2@db:5432/kanban?sslmode=require",
		DBUser:             "kanban",
		DBPass:             "hunter2",
		AWSAccessKeyID:     "AKID",
		AWSSecretAccessKey: "secret",
	}
	got := c.Redacted()
	if strings.Contains(got.DatabaseURL, "hunter2") {
		t.Errorf("DATABASE_URL = %q", got.DatabaseURL)
	}
	// The rest of the URL survives, because which host and database it points
	// at is the whole reason the line is printed.
	if !strings.Contains(got.DatabaseURL, "db:5432/kanban") {
		t.Errorf("DATABASE_URL = %q, want the address kept", got.DatabaseURL)
	}
	if got.DBPass != maskSet || got.AWSSecretAccessKey != maskSet {
		t.Errorf("DBPass = %q, AWSSecretAccessKey = %q", got.DBPass, got.AWSSecretAccessKey)
	}
	// An access key ID is not a secret, and it is how you tell which key the
	// process is using when a bucket refuses it.
	if got.AWSAccessKeyID != "AKID" || got.DBUser != "kanban" {
		t.Errorf("masked a value that is not a secret: %+v", got)
	}
	if c.DBPass != "hunter2" {
		t.Error("Redacted changed the receiver")
	}
}

func TestRedactedUnsetSecretStaysEmpty(t *testing.T) {
	got := Config{}.Redacted()
	if got.DBPass != "" || got.DatabaseURL != "" {
		t.Errorf("masked an unset secret: DBPass = %q, DatabaseURL = %q", got.DBPass, got.DatabaseURL)
	}
}

func TestRedactedDSN(t *testing.T) {
	c, err := FromEnv(func(key string) string {
		switch key {
		case "DB_PASS":
			return "hunter2"
		case "DB_HOST":
			return "db"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	got := c.RedactedDSN()
	if strings.Contains(got, "hunter2") {
		t.Errorf("RedactedDSN = %q", got)
	}
	if !strings.Contains(got, "user:xxxxx@db:5432/kanban") {
		t.Errorf("RedactedDSN = %q, want the user, host and database kept", got)
	}
}

// A DSN that is not a URL is masked whole rather than printed on the chance that
// it parses.
func TestRedactedDSNUnparseable(t *testing.T) {
	c := Config{DatabaseURL: "postgres://user:pass@ho st:5432/db"}
	if got := c.RedactedDSN(); got != maskSet {
		t.Errorf("RedactedDSN = %q, want %q", got, maskSet)
	}
}

func TestSettings(t *testing.T) {
	c, err := FromEnv(func(key string) string {
		switch key {
		case "AVATARS":
			return "a@example.com=octocat,b@example.com=other"
		case "BACKUP_INTERVAL":
			return "90m"
		case "AWS_SECRET_ACCESS_KEY":
			return "secret"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"SERVER_PORT":           "17808",
		"LISTEN_ADDR":           "",
		"STORAGE":               "postgres",
		"AUTO_MIGRATE":          "true",
		"BACKUP_INTERVAL":       "1h30m0s",
		"BACKUP_KEEP":           "7",
		"AVATARS":               "2 pairs",
		"AWS_SECRET_ACCESS_KEY": maskSet,
	}
	got := map[string]Setting{}
	for _, s := range c.Settings() {
		if _, dup := got[s.Name]; dup {
			t.Errorf("%s is reported twice", s.Name)
		}
		got[s.Name] = s
	}
	for name, value := range want {
		if got[name].Value != value {
			t.Errorf("%s = %q, want %q", name, got[name].Value, value)
		}
	}
	if !got["AWS_SECRET_ACCESS_KEY"].Secret {
		t.Error("AWS_SECRET_ACCESS_KEY is not marked secret")
	}
	if got["AVATARS"].Secret {
		t.Error("AVATARS is marked secret, so doctor would not show the count")
	}
	// The addresses themselves stay out of the report; the count is the answer.
	for _, s := range c.Settings() {
		if strings.Contains(s.Value, "a@example.com") {
			t.Errorf("%s = %q leaks an address", s.Name, s.Value)
		}
	}
	if len(c.Settings()) != len(mustTaggedFields()) {
		t.Errorf("Settings reports %d of %d tagged fields", len(c.Settings()), len(mustTaggedFields()))
	}
}

func mustTaggedFields() []string {
	rt := reflect.TypeOf(Config{})
	var names []string
	for i := range rt.NumField() {
		if name, ok := rt.Field(i).Tag.Lookup("env"); ok {
			names = append(names, name)
		}
	}
	return names
}
