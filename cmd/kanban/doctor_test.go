package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kanban/internal/backup"
)

// doctorRun runs the command the way a container would and returns the report.
func doctorRun(t *testing.T, e map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), append([]string{"doctor"}, args...), env(e), &out, &errOut)
	return code, out.String(), errOut.String()
}

// wants asserts the report has a line for a check with a given status, so a test
// says "storage warned" rather than matching a whole line of prose.
func wants(t *testing.T, report, status, name string) {
	t.Helper()
	for _, line := range strings.Split(report, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == status && fields[1] == name {
			return
		}
	}
	t.Errorf("no %q line for %s in:\n%s", status, name, report)
}

func TestDoctorMemory(t *testing.T) {
	code, out, _ := doctorRun(t, map[string]string{"STORAGE": "memory", "DB_PASS": "hunter2"})
	// A memory store and no identity provider are choices, not failures: the
	// exit code is what a health check reads and it must not cry wolf.
	if code != 0 {
		t.Errorf("exit %d, want 0:\n%s", code, out)
	}
	wants(t, out, "ok", "configuration")
	wants(t, out, "warn", "storage")
	wants(t, out, "ok", "schema")
	wants(t, out, "warn", "identity")
	wants(t, out, "--", "avatars")
	wants(t, out, "--", "backup")
	if !strings.Contains(out, "nothing failed, 2 worth a look") {
		t.Errorf("summary missing:\n%s", out)
	}
	if strings.Contains(out, "hunter2") {
		t.Errorf("the report printed a password:\n%s", out)
	}
	if !strings.Contains(out, "DB_PASS") || !strings.Contains(out, "[set]") {
		t.Errorf("DB_PASS is not reported as set:\n%s", out)
	}
	// Every variable, whether it is set or not: the usual reason a deployment
	// misbehaves is a name that never arrived.
	for _, name := range []string{"SERVER_PORT", "AUTH_MODE", "BACKUP_S3_ENDPOINT", "AWS_SESSION_TOKEN"} {
		if !strings.Contains(out, name) {
			t.Errorf("%s is not in the report:\n%s", name, out)
		}
	}
}

func TestDoctorBadConfiguration(t *testing.T) {
	code, out, _ := doctorRun(t, map[string]string{"STORAGE": "oracle"})
	if code != 1 {
		t.Errorf("exit %d, want 1:\n%s", code, out)
	}
	wants(t, out, "fail", "configuration")
	if !strings.Contains(out, "unknown backend") {
		t.Errorf("the reason is missing:\n%s", out)
	}
	// The rest is skipped rather than reported as broken, so the one line that
	// matters is not buried under five consequences of it.
	for _, name := range []string{"storage", "schema", "identity", "avatars", "backup"} {
		wants(t, out, "--", name)
	}
	if strings.Count(out, "not checked: settle the configuration first") != 5 {
		t.Errorf("want five skipped checks:\n%s", out)
	}
}

// The schema is read, not written: a diagnostic that migrates the database is
// not a diagnostic. Two runs in a row say the same thing.
func TestDoctorReadsTheSchemaAndWritesNothing(t *testing.T) {
	e := map[string]string{"STORAGE": "sqlite", "SQLITE_PATH": filepath.Join(t.TempDir(), "kanban.db")}
	for i := range 2 {
		code, out, _ := doctorRun(t, e)
		if code != 1 {
			t.Fatalf("run %d: exit %d, want 1:\n%s", i, code, out)
		}
		wants(t, out, "fail", "schema")
		if !strings.Contains(out, "serve would create it on start; doctor changes nothing") {
			t.Errorf("run %d: no hint about AUTO_MIGRATE:\n%s", i, out)
		}
		wants(t, out, "ok", "storage")
	}
	// With AUTO_MIGRATE off the hint points at the command instead.
	e["AUTO_MIGRATE"] = "false"
	_, out, _ := doctorRun(t, e)
	if !strings.Contains(out, "run `kanban migrate`") {
		t.Errorf("no hint about the migrate command:\n%s", out)
	}

	var ignored bytes.Buffer
	if code := run(context.Background(), []string{"migrate"}, env(e), &ignored, &ignored); code != 0 {
		t.Fatalf("migrate: %d %s", code, ignored.String())
	}
	code, out, _ := doctorRun(t, e)
	if code != 0 {
		t.Errorf("after migrate: exit %d, want 0:\n%s", code, out)
	}
	wants(t, out, "ok", "schema")
	if !strings.Contains(out, "no board in it yet") {
		t.Errorf("after migrate: %s", out)
	}
}

func TestDoctorBackupDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "snapshots")
	e := map[string]string{
		"STORAGE": "memory", "BACKUP_DIR": dir, "BACKUP_INTERVAL": "6h", "BACKUP_KEEP": "3",
	}
	// A target with nothing in it yet is the state of every new deployment.
	code, out, _ := doctorRun(t, e)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wants(t, out, "ok", "backup")
	if !strings.Contains(out, "every 6h0m0s, keeping 3") || !strings.Contains(out, "0 snapshots there") {
		t.Errorf("schedule not described:\n%s", out)
	}
	// The probe leaves nothing behind, and the directory it had to create for it
	// is the one the first snapshot would have created.
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("after the write probe: %v, %d entries", err, len(entries))
	}

	name := backup.Name(time.Now().Add(-90 * time.Minute))
	if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out, _ = doctorRun(t, e)
	if !strings.Contains(out, "1 snapshot there, newest "+name) || !strings.Contains(out, "1h30m0s ago") {
		t.Errorf("the newest snapshot is not reported:\n%s", out)
	}

	// A schedule that is off is worth a line: the target is configured, so
	// somebody expects snapshots to be taken.
	e["BACKUP_INTERVAL"] = "0"
	code, out, _ = doctorRun(t, e)
	if code != 0 {
		t.Errorf("exit %d, want 0:\n%s", code, out)
	}
	wants(t, out, "warn", "backup")
	if !strings.Contains(out, "BACKUP_INTERVAL is 0") {
		t.Errorf("the reason is missing:\n%s", out)
	}
}

func TestDoctorBackupDirNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a directory whatever its mode says")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	code, out, _ := doctorRun(t, map[string]string{
		"STORAGE": "memory", "BACKUP_DIR": filepath.Join(parent, "snapshots"),
	})
	if code != 1 {
		t.Errorf("exit %d, want 1:\n%s", code, out)
	}
	wants(t, out, "fail", "backup")
}

func TestDoctorBackupBucket(t *testing.T) {
	name := backup.Name(time.Now())
	var listed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			// The whole point of listing rather than writing: a diagnostic does
			// not put an object in somebody's bucket.
			t.Errorf("doctor sent a %s to the bucket", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		listed = true
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult>` +
			`<Contents><Key>` + name + `</Key></Contents></ListBucketResult>`))
	}))
	defer srv.Close()

	code, out, _ := doctorRun(t, map[string]string{
		"STORAGE": "memory", "BACKUP_S3_BUCKET": "boards", "BACKUP_S3_REGION": "eu-central-2",
		"BACKUP_S3_ENDPOINT": srv.URL, "AWS_ACCESS_KEY_ID": "AKID", "AWS_SECRET_ACCESS_KEY": "secret",
	})
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !listed {
		t.Error("the bucket was never listed")
	}
	wants(t, out, "ok", "backup")
	if !strings.Contains(out, "a write was not attempted") {
		t.Errorf("the report does not say the write was skipped:\n%s", out)
	}
	if strings.Contains(out, "secret") {
		t.Errorf("the report printed the secret key:\n%s", out)
	}
}

func TestDoctorBackupBucketRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	code, out, _ := doctorRun(t, map[string]string{
		"STORAGE": "memory", "BACKUP_S3_BUCKET": "boards", "BACKUP_S3_REGION": "eu-central-2",
		"BACKUP_S3_ENDPOINT": srv.URL, "AWS_ACCESS_KEY_ID": "AKID", "AWS_SECRET_ACCESS_KEY": "secret",
	})
	if code != 1 {
		t.Errorf("exit %d, want 1:\n%s", code, out)
	}
	wants(t, out, "fail", "backup")
}

// rewriteTo sends a request for the Access team domain to a local server, since
// the URL is built from the domain and is always https.
type rewriteTo struct{ host string }

func (r rewriteTo) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme, clone.URL.Host = "http", r.host
	return http.DefaultTransport.RoundTrip(clone)
}

func TestDoctorAccessKeys(t *testing.T) {
	var path string
	keys := `{"keys":[{"kid":"a"},{"kid":"b"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(keys))
	}))
	defer srv.Close()
	old := doctorHTTP
	doctorHTTP = &http.Client{Transport: rewriteTo{host: srv.Listener.Addr().String()}}
	t.Cleanup(func() { doctorHTTP = old })

	e := map[string]string{
		"STORAGE": "memory", "AUTH_MODE": "access",
		"ACCESS_TEAM_DOMAIN": "team.cloudflareaccess.com", "ACCESS_AUD": "aud-tag",
	}
	code, out, _ := doctorRun(t, e)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wants(t, out, "ok", "identity")
	if path != "/cdn-cgi/access/certs" {
		t.Errorf("fetched %q", path)
	}
	if !strings.Contains(out, "2 signing keys") || !strings.Contains(out, "aud-tag") {
		t.Errorf("keys or audience missing:\n%s", out)
	}

	// A team domain with a typo answers, and answers with nothing usable. From
	// the sign-in page that looks the same as blocked egress.
	keys = `{"keys":[]}`
	code, out, _ = doctorRun(t, e)
	if code != 1 {
		t.Errorf("exit %d, want 1:\n%s", code, out)
	}
	wants(t, out, "fail", "identity")
	if !strings.Contains(out, "ACCESS_TEAM_DOMAIN") {
		t.Errorf("no pointer at the variable to fix:\n%s", out)
	}
}

func TestDoctorAccessUnreachable(t *testing.T) {
	old := doctorHTTP
	// A port nothing listens on, which is what blocked egress looks like.
	doctorHTTP = &http.Client{Transport: rewriteTo{host: "127.0.0.1:1"}}
	t.Cleanup(func() { doctorHTTP = old })
	code, out, _ := doctorRun(t, map[string]string{
		"STORAGE": "memory", "AUTH_MODE": "access",
		"ACCESS_TEAM_DOMAIN": "team.cloudflareaccess.com", "ACCESS_AUD": "aud-tag",
	})
	if code != 1 {
		t.Errorf("exit %d, want 1:\n%s", code, out)
	}
	wants(t, out, "fail", "identity")
}

func TestDoctorProxyAndAvatars(t *testing.T) {
	code, out, _ := doctorRun(t, map[string]string{
		"STORAGE": "memory", "AUTH_MODE": "proxy", "AUTH_HEADER": "X-Auth-Request-Email",
		"AVATARS": "me@example.com=octocat,other@example.com=someone",
	})
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wants(t, out, "ok", "identity")
	wants(t, out, "ok", "avatars")
	if !strings.Contains(out, "X-Auth-Request-Email") {
		t.Errorf("the header is not named:\n%s", out)
	}
	if !strings.Contains(out, "2 pairs") {
		t.Errorf("the pair count is missing:\n%s", out)
	}
	// The addresses are personal data and a report gets pasted into an issue.
	if strings.Contains(out, "me@example.com") {
		t.Errorf("the report printed an address:\n%s", out)
	}
}

func TestDoctorFlags(t *testing.T) {
	e := map[string]string{"STORAGE": "memory"}
	if code, _, errOut := doctorRun(t, e, "everything"); code != 2 || !strings.Contains(errOut, "unexpected argument") {
		t.Errorf("extra argument: %d %q", code, errOut)
	}
	if code, _, errOut := doctorRun(t, e, "-timeout", "0"); code != 2 || !strings.Contains(errOut, "must be positive") {
		t.Errorf("zero timeout: %d %q", code, errOut)
	}
	if code, _, errOut := doctorRun(t, e, "-nonsense"); code != 2 {
		t.Errorf("unknown flag: %d %q", code, errOut)
	}
	if code, out, _ := doctorRun(t, e, "-timeout", "3s"); code != 0 {
		t.Errorf("timeout: %d %s", code, out)
	}
}
