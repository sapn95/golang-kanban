package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Target is where snapshots are kept. It is deliberately three methods:
// putting one, seeing what is there, and removing one. Retention needs the
// second and the third, and so does the check on start that keeps a restart
// loop from writing a snapshot per restart.
//
// Names are opaque to the caller apart from the shape Name gives them. A target
// stores them flat; the S3 target puts the prefix in front and takes it off
// again, so nothing above this interface knows about it.
type Target interface {
	Put(ctx context.Context, name string, body []byte) error
	// List returns the snapshot names the target holds, oldest first.
	List(ctx context.Context) ([]string, error)
	Delete(ctx context.Context, name string) error
	// String describes the target well enough for a log line and carries no
	// credentials.
	String() string
}

// A snapshot's name is its timestamp, which makes the lexical order of a
// listing the chronological one. Retention and the freshness check on start
// both lean on that, so neither has to read the files to know their age.
const (
	namePrefix = "kanban-"
	nameSuffix = ".json"
	nameLayout = "20060102T150405Z"
)

// Name is the file name of a snapshot taken at a given moment.
func Name(at time.Time) string {
	return namePrefix + at.UTC().Format(nameLayout) + nameSuffix
}

// TakenAt reads the timestamp back out of a name, and reports false for a name
// this package did not write. Listings are filtered through it, so a target
// that also holds somebody else's files is not a problem and retention never
// deletes one of them.
func TakenAt(name string) (time.Time, bool) {
	stamp, ok := strings.CutPrefix(name, namePrefix)
	if !ok {
		return time.Time{}, false
	}
	stamp, ok = strings.CutSuffix(stamp, nameSuffix)
	if !ok {
		return time.Time{}, false
	}
	at, err := time.Parse(nameLayout, stamp)
	if err != nil {
		return time.Time{}, false
	}
	return at.UTC(), true
}

// sortNames puts a listing in the order List promises: oldest first.
func sortNames(names []string) {
	sort.Strings(names)
}

// Dir keeps snapshots in a directory. It is the target for a deployment with a
// volume and no object storage, and it is what the tests use.
type Dir struct{ Path string }

var _ Target = Dir{}

// String names the target for the log and the doctor report.
func (d Dir) String() string { return "dir " + d.Path }

// Put writes the snapshot beside its neighbours through a temporary file and a
// rename, so a reader either sees the previous snapshot or the whole new one.
// 0o600, because the document is every card on every board.
func (d Dir) Put(_ context.Context, name string, body []byte) error {
	if err := os.MkdirAll(d.Path, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", d.Path, err)
	}
	final := d.file(name)
	tmp, err := os.CreateTemp(d.Path, filepath.Base(name)+".part-*")
	if err != nil {
		return fmt.Errorf("create temporary file in %s: %w", d.Path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op once the rename worked
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmp.Name(), err)
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	// Flushed before the rename: a rename that outruns the data is how a
	// directory ends up holding an empty snapshot after a power cut.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return fmt.Errorf("rename to %s: %w", final, err)
	}
	return nil
}

// List returns the snapshots already in the directory, newest last, which is
// the order retention walks them in.
func (d Dir) List(context.Context) ([]string, error) {
	entries, err := os.ReadDir(d.Path)
	if os.IsNotExist(err) {
		// Nothing has been written yet, which is not a failure to report on
		// every tick until the first snapshot lands.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", d.Path, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, ok := TakenAt(e.Name()); ok {
			names = append(names, e.Name())
		}
	}
	sortNames(names)
	return names, nil
}

// Delete removes one snapshot, for the retention sweep after a successful write.
func (d Dir) Delete(_ context.Context, name string) error {
	if err := os.Remove(d.file(name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	return nil
}

// file resolves a name inside the directory. Base is applied because a target
// name is data: it comes back from List, and on the S3 target from a response.
func (d Dir) file(name string) string {
	return filepath.Join(d.Path, filepath.Base(name))
}
