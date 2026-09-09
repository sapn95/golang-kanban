package backup

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNameRoundTrip(t *testing.T) {
	at := time.Date(2026, 3, 4, 5, 6, 7, 890000000, time.UTC)
	name := Name(at)
	if name != "kanban-20260304T050607Z.json" {
		t.Fatalf("Name = %q", name)
	}
	got, ok := TakenAt(name)
	if !ok {
		t.Fatalf("TakenAt(%q) refused its own name", name)
	}
	// The name is to the second, so the sub-second part is what is lost.
	if want := at.Truncate(time.Second); !got.Equal(want) {
		t.Errorf("TakenAt = %v, want %v", got, want)
	}
	// A name is built from an instant in any zone and still reads back as UTC.
	local := time.Date(2026, 3, 4, 7, 6, 7, 0, time.FixedZone("CEST", 2*60*60))
	if Name(local) != name {
		t.Errorf("Name(%v) = %q, want %q", local, Name(local), name)
	}
}

func TestTakenAtRefusesSomebodyElsesFile(t *testing.T) {
	for _, name := range []string{
		"", "kanban.json", "notes.txt", "kanban-.json",
		"kanban-2026-03-04.json", "kanban-20260304T050607Z.json.gz",
		"dump-20260304T050607Z.json", "kanban-20261340T990607Z.json",
	} {
		if _, ok := TakenAt(name); ok {
			t.Errorf("TakenAt(%q) claimed a file this package did not write", name)
		}
	}
}

// TestNamesSortByAge is what retention and the freshness check both rely on:
// lexical order is chronological order, so neither has to read the files.
func TestNamesSortByAge(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var names []string
	for _, d := range []time.Duration{48 * time.Hour, 0, 25 * time.Hour, time.Second} {
		names = append(names, Name(base.Add(d)))
	}
	sortNames(names)
	if !slices.IsSortedFunc(names, func(a, b string) int {
		x, _ := TakenAt(a)
		y, _ := TakenAt(b)
		return x.Compare(y)
	}) {
		t.Errorf("sorted names are not in age order: %v", names)
	}
}

func TestDir(t *testing.T) {
	ctx := context.Background()
	// A path that does not exist yet, because a volume is mounted empty.
	dir := Dir{Path: filepath.Join(t.TempDir(), "snapshots")}
	if !strings.Contains(dir.String(), dir.Path) {
		t.Errorf("String = %q", dir.String())
	}

	names, err := dir.List(ctx)
	must(t, "list an empty target", err)
	if len(names) != 0 {
		t.Errorf("List = %v, want nothing before the first write", names)
	}

	first := Name(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	second := Name(time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC))
	must(t, "put", dir.Put(ctx, second, []byte("second")))
	must(t, "put", dir.Put(ctx, first, []byte("first")))

	names, err = dir.List(ctx)
	must(t, "list", err)
	if len(names) != 2 || names[0] != first || names[1] != second {
		t.Errorf("List = %v, want %v oldest first", names, []string{first, second})
	}

	body, err := os.ReadFile(filepath.Join(dir.Path, first))
	must(t, "read back", err)
	if string(body) != "first" {
		t.Errorf("file holds %q", body)
	}
	// The document is every card on every board, so it is not group-readable.
	info, err := os.Stat(filepath.Join(dir.Path, first))
	must(t, "stat", err)
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}

	// Writing the same name again replaces it and leaves no partial file behind.
	must(t, "put again", dir.Put(ctx, first, []byte("first, again")))
	entries, err := os.ReadDir(dir.Path)
	must(t, "read dir", err)
	if len(entries) != 2 {
		t.Errorf("directory holds %d files, want 2", len(entries))
	}

	must(t, "delete", dir.Delete(ctx, first))
	// Deleting what is not there is not a failure: retention runs after a write
	// and two processes may both be pruning.
	must(t, "delete twice", dir.Delete(ctx, first))
	names, err = dir.List(ctx)
	must(t, "list", err)
	if len(names) != 1 || names[0] != second {
		t.Errorf("List = %v, want only %s", names, second)
	}
}

// TestDirListIgnoresEverythingElse means a target can be a directory that
// already holds other things, and retention will not delete one of them.
func TestDirListIgnoresEverythingElse(t *testing.T) {
	ctx := context.Background()
	dir := Dir{Path: t.TempDir()}
	name := Name(stamp)
	must(t, "put", dir.Put(ctx, name, []byte("{}")))
	must(t, "other file", os.WriteFile(filepath.Join(dir.Path, "notes.txt"), []byte("mine"), 0o600))
	must(t, "other file", os.WriteFile(filepath.Join(dir.Path, "kanban-old.json"), []byte("mine"), 0o600))
	must(t, "subdirectory", os.Mkdir(filepath.Join(dir.Path, "kanban-20260304T050607Z.json.d"), 0o700))

	names, err := dir.List(ctx)
	must(t, "list", err)
	if len(names) != 1 || names[0] != name {
		t.Errorf("List = %v, want only %s", names, name)
	}
}

// TestDirRefusesAPathItCannotUse is the failure an operator meets first: a
// BACKUP_DIR that is a file, or one the process cannot write to.
func TestDirRefusesAPathItCannotUse(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "not-a-directory")
	must(t, "write", os.WriteFile(file, []byte("x"), 0o600))
	dir := Dir{Path: file}
	if err := dir.Put(ctx, Name(stamp), []byte("{}")); err == nil {
		t.Error("Put into a file succeeded")
	}
	if _, err := dir.List(ctx); err == nil {
		t.Error("List of a file succeeded")
	}
}

// TestDirNameIsData: a name comes back from List, and on the S3 target from a
// response, so a name that walks up the tree must stay inside the directory.
func TestDirNameIsData(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dir := Dir{Path: filepath.Join(root, "snapshots")}
	must(t, "put", dir.Put(ctx, "../escaped.json", []byte("{}")))
	if _, err := os.Stat(filepath.Join(root, "escaped.json")); err == nil {
		t.Error("Put wrote outside the target directory")
	}
	if _, err := os.Stat(filepath.Join(dir.Path, "escaped.json")); err != nil {
		t.Errorf("the file did not land inside the directory: %v", err)
	}
}
