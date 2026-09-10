package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"kanban/internal/backup"
	"kanban/internal/config"
	"kanban/internal/store"
)

// doctorHTTP fetches the Access key set. A variable so a test can answer for
// the team domain without a certificate.
var doctorHTTP = &http.Client{}

// status is how one check turned out. The strings are what gets printed, and
// the four are deliberately not two: a homelab board with AUTH_MODE=none and no
// backup target is a working deployment, and a tool that calls that broken gets
// ignored the third time it says so.
type status string

const (
	statusOK   status = "ok"
	statusWarn status = "warn"
	statusFail status = "fail"
	// statusOff is a check for something that is not configured, which is a
	// choice rather than a problem.
	statusOff status = "--"
)

type check struct {
	name   string
	status status
	detail string
}

// doctor collects the checks. Every one of them runs: the first failure is
// rarely the whole story, and a container that will not start is usually two
// things at once.
type doctor struct {
	cfg     config.Config
	cfgErr  error
	timeout time.Duration
	now     func() time.Time
	checks  []check
}

// doctorCmd prints the effective configuration and checks the things that
// otherwise only fail once the process is running: the store, the schema, the
// identity provider and the backup target.
//
//	kanban doctor [-timeout 10s]
//
// It is the command to run inside the container when the board does not come up,
// which is why it takes no configuration of its own and reads the same
// environment serve does.
func doctorCmd(ctx context.Context, args []string, getenv config.Lookup, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	timeout := fs.Duration("timeout", 10*time.Second, "how long any one check may take")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "kanban doctor: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *timeout <= 0 {
		_, _ = fmt.Fprintf(stderr, "kanban doctor: -timeout must be positive, got %s\n", *timeout)
		return 2
	}

	// The error is carried rather than returned: a configuration that does not
	// validate is the most useful thing this command has to say, and it can only
	// say it by printing the report.
	cfg, err := config.FromEnv(getenv)
	d := &doctor{cfg: cfg, cfgErr: err, timeout: *timeout}
	d.run(ctx)
	return d.report(stdout)
}

func (d *doctor) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

func (d *doctor) add(s status, name, detail string) {
	d.checks = append(d.checks, check{name: name, status: s, detail: detail})
}

func (d *doctor) run(ctx context.Context) {
	if d.cfgErr != nil {
		d.add(statusFail, "configuration", d.cfgErr.Error())
		// Every check below reads the configuration. Reporting a store that
		// cannot open because the DSN was never built would bury the one line
		// that matters.
		for _, name := range []string{"storage", "schema", "identity", "avatars", "backup"} {
			d.add(statusOff, name, "not checked: settle the configuration first")
		}
		return
	}
	d.add(statusOK, "configuration", plural(len(d.cfg.Settings()), "variable")+", all valid")

	st := d.storage(ctx)
	if st != nil {
		defer func() { _ = st.Close() }()
		d.schema(ctx, st)
	} else {
		d.add(statusOff, "schema", "not checked: the store did not open")
	}
	d.identity(ctx)
	d.avatars()
	d.backup(ctx)
}

// storage opens the store and asks it something, and returns it open so the
// schema check can use the same connection. Nil means the store is unusable and
// there is no point in the rest.
func (d *doctor) storage(ctx context.Context) store.Store {
	start := d.clock()
	st, err := openStore(d.cfg)
	if err != nil {
		d.add(statusFail, "storage", fmt.Sprintf("%s: %v", d.describeStore(), err))
		return nil
	}
	pingCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	if err := st.Ping(pingCtx); err != nil {
		_ = st.Close()
		d.add(statusFail, "storage", fmt.Sprintf("%s: %v", d.describeStore(), err))
		return nil
	}
	took := d.clock().Sub(start).Round(time.Millisecond)
	if d.cfg.Storage == config.StorageMemory {
		d.add(statusWarn, "storage", "memory: nothing is written anywhere, and a restart loses every board")
		return st
	}
	d.add(statusOK, "storage", fmt.Sprintf("%s answered in %s", d.describeStore(), took))
	return st
}

// describeStore names the backend and where it is, without the password.
func (d *doctor) describeStore() string {
	switch d.cfg.Storage {
	case config.StoragePostgres:
		// The DSN names the scheme itself, so "postgres postgres://" would only
		// be noise.
		return d.cfg.RedactedDSN()
	case config.StorageSQLite:
		return "sqlite " + d.cfg.SQLitePath
	}
	return d.cfg.Storage
}

// schema reads a table instead of counting pending migrations, because a
// migration count would have to be a method on every backend and this asks the
// question the operator has: can it serve a board.
func (d *doctor) schema(ctx context.Context, st store.Store) {
	listCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	boards, err := st.ListBoards(listCtx)
	if err != nil {
		hint := "run `kanban migrate`"
		if d.cfg.AutoMigrate {
			// AUTO_MIGRATE is on, so serve would have created the schema.
			// Doctor deliberately does not: a read-only diagnostic that writes
			// to the database is not one.
			hint = "serve would create it on start; doctor changes nothing"
		}
		d.add(statusFail, "schema", fmt.Sprintf("%v; %s", err, hint))
		return
	}
	detail := "readable, no board in it yet; serve creates the first one"
	if len(boards) > 0 {
		slugs := make([]string, 0, len(boards))
		for _, b := range boards {
			slugs = append(slugs, b.Slug)
		}
		detail = fmt.Sprintf("readable, %s: %s", plural(len(boards), "board"), strings.Join(slugs, ", "))
	}
	d.add(statusOK, "schema", detail)
}

func (d *doctor) identity(ctx context.Context) {
	// A mode says who a caller is; it does not say that a caller has to be
	// anybody. The line says both, because "we run access mode" was read here
	// as "the board is behind a gate" while a request that arrived without an
	// assertion was still being served every write the board has.
	anon := "a request with no identity is served anonymously and can change every board"
	if d.cfg.AuthRequired {
		anon = "AUTH_REQUIRED: a request with no identity is refused"
	}
	switch d.cfg.AuthMode {
	case config.AuthProxy:
		d.add(statusOK, "identity", fmt.Sprintf("proxy mode, reading %s; the port must not be reachable except through the proxy that sets it. %s",
			d.cfg.AuthHeader, anon))
	case config.AuthAccess:
		start := d.clock()
		keys, err := accessKeys(ctx, doctorHTTP, d.cfg.AccessCertsURL(), d.timeout)
		if err != nil {
			d.add(statusFail, "identity", fmt.Sprintf("access mode: %s: %v", d.cfg.AccessCertsURL(), err))
			return
		}
		if keys == 0 {
			d.add(statusFail, "identity", fmt.Sprintf("access mode: %s published no signing keys; check ACCESS_TEAM_DOMAIN",
				d.cfg.AccessCertsURL()))
			return
		}
		d.add(statusOK, "identity", fmt.Sprintf("access mode, %s from %s in %s, audience %s. %s",
			plural(keys, "signing key"), d.cfg.AccessTeamDomain, d.clock().Sub(start).Round(time.Millisecond), d.cfg.AccessAudience, anon))
	default:
		d.add(statusWarn, "identity", "AUTH_MODE=none: every request is anonymous, so whoever reaches the port can read and change every board")
	}
}

// accessKeys fetches the team's signing keys and reports how many there are. A
// team domain with a typo and blocked egress look identical from the sign-in
// page, where nobody gets in and the reason is in a log line, so doctor makes
// the request the verifier makes.
func accessKeys(ctx context.Context, client *http.Client, url string, timeout time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%s", res.Status)
	}
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	// Bounded: this is an unauthenticated response from the internet, and the
	// key set of a team is a few kilobytes.
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&doc); err != nil {
		return 0, fmt.Errorf("read the key set: %w", err)
	}
	return len(doc.Keys), nil
}

func (d *doctor) avatars() {
	if len(d.cfg.Avatars) == 0 {
		d.add(statusOff, "avatars", "AVATARS is unset: the board draws initials and makes no outbound request")
		return
	}
	d.add(statusOK, "avatars", plural(len(d.cfg.Avatars), "pair")+
		"; the pictures are fetched by the server, so GitHub never sees who is looking")
}

func (d *doctor) backup(ctx context.Context) {
	target, err := backupTarget(d.cfg)
	if err != nil {
		d.add(statusFail, "backup", err.Error())
		return
	}
	if target == nil {
		d.add(statusOff, "backup", "no target: set BACKUP_DIR or BACKUP_S3_BUCKET, or take one yourself with `kanban export`")
		return
	}
	// A directory that cannot be written to is a failure the schedule would
	// otherwise report at the first tick, in a log line nobody is reading yet.
	if d.cfg.BackupDir != "" {
		if err := probeDir(d.cfg.BackupDir); err != nil {
			d.add(statusFail, "backup", fmt.Sprintf("%s: %v", target, err))
			return
		}
	}
	listCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	names, err := target.List(listCtx)
	if err != nil {
		d.add(statusFail, "backup", fmt.Sprintf("%s: %v", target, err))
		return
	}

	detail := fmt.Sprintf("%s, %s", target, d.describeSchedule())
	detail += ", " + plural(len(names), "snapshot") + " there"
	if len(names) > 0 {
		newest := names[len(names)-1]
		detail += ", newest " + newest
		if at, ok := backup.TakenAt(newest); ok {
			if age := d.clock().Sub(at); age >= time.Minute {
				detail += fmt.Sprintf(" (%s ago)", age.Round(time.Minute))
			} else {
				detail += " (just now)"
			}
		}
	}
	if d.cfg.BackupS3Bucket != "" {
		// Said out loud, because a listing proves the credentials and the
		// bucket name and says nothing about whether a PUT is allowed.
		detail += "; the bucket was listed, a write was not attempted"
	}
	if !d.cfg.BackupScheduled() {
		d.add(statusWarn, "backup", detail)
		return
	}
	d.add(statusOK, "backup", detail)
}

func (d *doctor) describeSchedule() string {
	if !d.cfg.BackupScheduled() {
		return "BACKUP_INTERVAL is 0, so serve takes no snapshots"
	}
	if d.cfg.BackupKeep == 0 {
		return fmt.Sprintf("every %s, keeping every snapshot", d.cfg.BackupInterval)
	}
	return fmt.Sprintf("every %s, keeping %d", d.cfg.BackupInterval, d.cfg.BackupKeep)
}

// plural renders a count with its noun. "1 pairs" in a report is the kind of
// thing that makes an operator wonder what else was not looked at.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// dash stands in for a value there is none of. It is easier to find with the eye
// than a blank, and an unset variable is usually what a report is being read for.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// probeDir proves the schedule could write. The file is removed again, and the
// directory is created the same way the first snapshot would create it.
func probeDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	f, err := os.CreateTemp(path, ".doctor-*")
	if err != nil {
		return fmt.Errorf("write to %s: %w", path, err)
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	return nil
}

// report prints the whole thing and returns the exit code: 1 only when
// something failed. A warning is a deployment somebody chose.
func (d *doctor) report(w io.Writer) int {
	var b strings.Builder
	fmt.Fprintf(&b, "kanban %s (%s, %s)\n\n", version, commit, runtime.Version())

	settings := d.cfg.Settings()
	width := 0
	for _, s := range settings {
		width = max(width, len(s.Name))
	}
	b.WriteString("settings\n")
	for _, s := range settings {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, s.Name, dash(s.Value))
	}

	b.WriteString("\nderived\n")
	fmt.Fprintf(&b, "  %-*s  %s\n", width, "listen address", dash(d.cfg.Addr()))
	fmt.Fprintf(&b, "  %-*s  %s\n", width, "store", dash(d.describeStore()))

	b.WriteString("\nchecks\n")
	failed, warned := 0, 0
	for _, c := range d.checks {
		switch c.status {
		case statusFail:
			failed++
		case statusWarn:
			warned++
		}
		fmt.Fprintf(&b, "  %-4s  %-13s  %s\n", c.status, c.name, c.detail)
	}
	switch {
	case failed > 0:
		fmt.Fprintf(&b, "\n%d failed, %d worth a look\n", failed, warned)
	case warned > 0:
		fmt.Fprintf(&b, "\nnothing failed, %d worth a look\n", warned)
	default:
		b.WriteString("\nall good\n")
	}

	// One write, so a report is never half printed.
	_, _ = io.WriteString(w, b.String())
	if failed > 0 {
		return 1
	}
	return 0
}
