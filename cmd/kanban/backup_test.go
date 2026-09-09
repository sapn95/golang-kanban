package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"kanban/internal/backup"
	"kanban/internal/config"
	"kanban/internal/store/memory"
)

// snapshotFile writes a small snapshot and returns its path. It is built here
// rather than exported from a seeded store, so what the round trip below starts
// from is a document and not whatever the current code happens to produce.
func snapshotFile(t *testing.T) string {
	t.Helper()
	at := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	archived := at.Add(-time.Hour)
	snap := &backup.Snapshot{
		Format:  backup.Format,
		TakenAt: at,
		Boards: []backup.Board{{
			ID: "b-1", Slug: "homelab", Name: "Homelab", Layout: "columns",
			CreatedAt: at, UpdatedAt: at,
			Columns: []backup.Column{{ID: "c-1", Name: "To Do"}, {ID: "c-2", Name: "Done", WIPLimit: 2}},
			Labels:  []backup.Label{{ID: "l-1", Name: "pi", Color: "#e11d48"}},
			Cards: []backup.Card{
				{
					ID: "k-1", ColumnID: "c-1", Title: "Reflash the SD card",
					Description: "It survived < 2 years.",
					DueDate:     "2026-03-31", Assignee: "sapn95@users.noreply.github.com",
					CreatedAt: at, UpdatedAt: at,
					Labels:   []string{"l-1"},
					Subtasks: []backup.Subtask{{ID: "s-1", Title: "back up first", Done: true}},
					Comments: []backup.Comment{{ID: "m-1", Author: "sapn95", Body: "done", CreatedAt: at}},
				},
				{ID: "k-2", ColumnID: "c-2", Title: "Ship 2.1.0", CreatedAt: at, UpdatedAt: at, ArchivedAt: &archived},
			},
		}},
	}
	body, err := backup.Bytes(snap)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snapshot.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// sqliteEnv is a store that survives between two calls to run, which is what an
// export of what an import wrote needs.
func sqliteEnv(t *testing.T) func(string) string {
	t.Helper()
	return env(map[string]string{"STORAGE": "sqlite", "SQLITE_PATH": filepath.Join(t.TempDir(), "kanban.db")})
}

// TestExportImportRoundTripThroughTheCLI is the pair of commands doing what they
// are for: a snapshot goes into one database, comes out again, and goes into a
// second one as the same document.
func TestExportImportRoundTripThroughTheCLI(t *testing.T) {
	ctx := context.Background()
	first, second := sqliteEnv(t), sqliteEnv(t)
	in := snapshotFile(t)
	out := filepath.Join(t.TempDir(), "exported.json")

	var stdout, stderr bytes.Buffer
	if code := run(ctx, []string{"import", in}, first, &stdout, &stderr); code != 0 {
		t.Fatalf("import: %d %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "imported 1 board, 2 columns, 1 label, 2 cards, 1 comment") {
		t.Errorf("stdout = %q", stdout.String())
	}

	stdout.Reset()
	if code := run(ctx, []string{"export", "-o", out}, first, &stdout, &stderr); code != 0 {
		t.Fatalf("export: %d %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "wrote "+out) || !strings.Contains(stdout.String(), "2 cards") {
		t.Errorf("stdout = %q", stdout.String())
	}
	// The file is every card on every board, including the comments.
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}

	stdout.Reset()
	if code := run(ctx, []string{"import", out}, second, &stdout, &stderr); code != 0 {
		t.Fatalf("import into the second database: %d %s", code, stderr.String())
	}
	// And the export of the second database is the export of the first. TakenAt
	// is the one field that is allowed to differ, because it is the clock.
	var again bytes.Buffer
	if code := run(ctx, []string{"export"}, second, &again, &stderr); code != 0 {
		t.Fatalf("export from the second database: %d %s", code, stderr.String())
	}
	want := read(t, out)
	got, err := backup.Read(bytes.NewReader(again.Bytes()))
	if err != nil {
		t.Fatalf("read the second export: %v", err)
	}
	got.TakenAt = want.TakenAt
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the round trip changed the document:\n--- first ---\n%+v\n--- second ---\n%+v", want, got)
	}
	if want.Build != version {
		t.Errorf("Build = %q, want the version of the binary that wrote it", want.Build)
	}
}

func read(t *testing.T, path string) *backup.Snapshot {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	snap, err := backup.Read(f)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// TestExportToStdout is the pipeable form, and an empty store is the case that
// has to produce a document rather than nothing at all.
func TestExportToStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"export"}, env(map[string]string{"STORAGE": "memory"}), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("export: %d %s", code, stderr.String())
	}
	snap, err := backup.Read(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		t.Fatalf("what export wrote does not read back: %v\n%s", err, stdout.String())
	}
	if snap.Format != backup.Format || len(snap.Boards) != 0 {
		t.Errorf("snapshot = %+v", snap)
	}
}

// TestImportFromStdin is the other half of the pipe, so a snapshot can be moved
// between two deployments without touching a disk on the way.
func TestImportFromStdin(t *testing.T) {
	body, err := os.ReadFile(snapshotFile(t))
	if err != nil {
		t.Fatal(err)
	}
	old := stdin
	defer func() { stdin = old }()
	stdin = bytes.NewReader(body)

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"import"}, sqliteEnv(t), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("import: %d %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "imported 1 board") {
		t.Errorf("stdout = %q", stdout.String())
	}
	// A dash is the same thing said out loud, which is what people type.
	stdin = bytes.NewReader(body)
	stdout.Reset()
	if code := run(context.Background(), []string{"import", "-"}, sqliteEnv(t), &stdout, &stderr); code != 0 {
		t.Fatalf("import -: %d %s", code, stderr.String())
	}
}

// TestImportDryRun is what an operator runs on a file they were handed before
// letting it near a database.
func TestImportDryRun(t *testing.T) {
	ctx := context.Background()
	e := sqliteEnv(t)
	var stdout, stderr bytes.Buffer
	if code := run(ctx, []string{"import", "-dry-run", snapshotFile(t)}, e, &stdout, &stderr); code != 0 {
		t.Fatalf("dry run: %d %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "is a format 1 snapshot taken 2026-03-04T12:00:00Z") ||
		!strings.Contains(stdout.String(), "2 cards") {
		t.Errorf("stdout = %q", stdout.String())
	}

	// Nothing was written, which the export of the same database shows.
	stdout.Reset()
	if code := run(ctx, []string{"export"}, e, &stdout, &stderr); code != 0 {
		t.Fatalf("export: %d %s", code, stderr.String())
	}
	snap, err := backup.Read(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Boards) != 0 {
		t.Errorf("the dry run wrote %d boards", len(snap.Boards))
	}
}

// TestImportRefusesToOverwriteWithoutBeingAsked: the second import of the same
// file exits 2, which is the code that says the command was wrong rather than
// that something broke.
func TestImportRefusesToOverwriteWithoutBeingAsked(t *testing.T) {
	ctx := context.Background()
	e := sqliteEnv(t)
	in := snapshotFile(t)
	var stdout, stderr bytes.Buffer
	if code := run(ctx, []string{"import", in}, e, &stdout, &stderr); code != 0 {
		t.Fatalf("first import: %d %s", code, stderr.String())
	}
	if code := run(ctx, []string{"import", in}, e, &stdout, &stderr); code != 2 {
		t.Fatalf("second import: %d %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "-replace") {
		t.Errorf("stderr = %q, want it to say how to overwrite", stderr.String())
	}
	stdout.Reset()
	if code := run(ctx, []string{"import", "-replace", in}, e, &stdout, &stderr); code != 0 {
		t.Fatalf("replace: %d %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "1 existing board overwritten") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestExportImportRefuseWhatTheyCannotDo(t *testing.T) {
	ctx := context.Background()
	notJSON := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(notJSON, []byte("dear diary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := filepath.Join(t.TempDir(), "future.json")
	if err := os.WriteFile(future, []byte(`{"format": 99, "boards": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A snapshot that reads but cannot be written: a card in a column the board
	// does not have.
	broken := filepath.Join(t.TempDir(), "broken.json")
	body, err := backup.Bytes(&backup.Snapshot{Format: backup.Format, Boards: []backup.Board{{
		ID: "b-1", Slug: "s", Name: "n",
		Columns: []backup.Column{{ID: "c-1", Name: "A"}},
		Cards:   []backup.Card{{ID: "k-1", ColumnID: "c-9", Title: "nowhere"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(broken, body, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		args []string
		code int
		want string
	}{
		{"export with an unknown flag", []string{"export", "-bogus"}, 2, "flag provided but not defined"},
		{"export with an argument", []string{"export", "everything"}, 2, "unexpected argument"},
		{"export into a directory that is not there", []string{"export", "-o", filepath.Join(t.TempDir(), "nope", "x.json")}, 1, "kanban export"},
		{"import with an unknown flag", []string{"import", "-bogus"}, 2, "flag provided but not defined"},
		{"import of two files", []string{"import", "a.json", "b.json"}, 2, "one file at a time"},
		{"import of a file that is not there", []string{"import", filepath.Join(t.TempDir(), "gone.json")}, 1, "no such file"},
		{"import of something that is not a snapshot", []string{"import", notJSON}, 1, "decode snapshot"},
		{"import of a newer format", []string{"import", future}, 1, "this build reads up to 1"},
		{"import of a snapshot that cannot be written", []string{"import", broken}, 1, "names column c-9"},
		{"the same on a dry run", []string{"import", "-dry-run", broken}, 1, "names column c-9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(ctx, tc.args, sqliteEnv(t), &stdout, &stderr)
			if code != tc.code {
				t.Errorf("exit code %d, want %d (stderr %q)", code, tc.code, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want it to mention %q", stderr.String(), tc.want)
			}
		})
	}
}

func TestBackupTarget(t *testing.T) {
	t.Run("no target", func(t *testing.T) {
		target, err := backupTarget(config.Config{})
		if err != nil || target != nil {
			t.Errorf("target = %v, %v", target, err)
		}
	})
	t.Run("a directory", func(t *testing.T) {
		target, err := backupTarget(config.Config{BackupDir: "/data/snapshots"})
		if err != nil {
			t.Fatal(err)
		}
		if dir, ok := target.(backup.Dir); !ok || dir.Path != "/data/snapshots" {
			t.Errorf("target = %#v", target)
		}
	})
	t.Run("a bucket", func(t *testing.T) {
		target, err := backupTarget(config.Config{
			BackupS3Bucket: "kanban-backups", BackupS3Prefix: "pi/", BackupS3Region: "eu-central-2",
			BackupS3Endpoint: "https://minio.example.com", AWSAccessKeyID: "AKID",
			AWSSecretAccessKey: "secret", AWSSessionToken: "token",
		})
		if err != nil {
			t.Fatal(err)
		}
		s3, ok := target.(*backup.S3)
		if !ok {
			t.Fatalf("target = %#v", target)
		}
		if s3.Bucket != "kanban-backups" || s3.Prefix != "pi/" || s3.Region != "eu-central-2" ||
			s3.Endpoint != "https://minio.example.com" || s3.AccessKeyID != "AKID" ||
			s3.SecretAccessKey != "secret" || s3.SessionToken != "token" {
			t.Errorf("target = %#v", s3)
		}
		// Whatever the log says about it, it does not say the secret.
		if strings.Contains(s3.String(), "secret") {
			t.Errorf("String = %q", s3.String())
		}
	})
}

func TestStartBackups(t *testing.T) {
	logs := func() (*slog.Logger, *syncBuf) {
		b := &syncBuf{}
		return slog.New(slog.NewTextHandler(b, nil)), b
	}

	t.Run("no target, no schedule", func(t *testing.T) {
		log, b := logs()
		stop, err := startBackups(context.Background(), config.Config{}, memory.New(), log)
		if err != nil {
			t.Fatal(err)
		}
		stop()
		if b.String() != "" {
			t.Errorf("log = %q, want nothing said about a schedule nobody asked for", b.String())
		}
	})

	// A target with the interval at zero is a schedule turned off, and an
	// operator who set BACKUP_DIR should be told it is not running.
	t.Run("a target with no interval says so", func(t *testing.T) {
		log, b := logs()
		stop, err := startBackups(context.Background(),
			config.Config{BackupDir: t.TempDir()}, memory.New(), log)
		if err != nil {
			t.Fatal(err)
		}
		stop()
		if !strings.Contains(b.String(), "backup schedule off") {
			t.Errorf("log = %q", b.String())
		}
	})

	t.Run("a snapshot on start, and stop waits for it", func(t *testing.T) {
		dir := t.TempDir()
		log, b := logs()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stop, err := startBackups(ctx, config.Config{
			BackupDir: dir, BackupInterval: time.Hour, BackupKeep: 2,
		}, memory.New(), log)
		if err != nil {
			t.Fatal(err)
		}
		defer stop()

		deadline := time.Now().Add(5 * time.Second)
		for {
			names, err := (backup.Dir{Path: dir}).List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(names) == 1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("no snapshot was written: %s", b.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
		stop()
		if !strings.Contains(b.String(), "backup schedule stopped") {
			t.Errorf("stop did not wait for the schedule: %q", b.String())
		}
	})
}
