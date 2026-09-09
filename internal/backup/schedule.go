package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"kanban/internal/store"
)

// Schedule writes a snapshot to a Target on a timer and keeps the last few.
//
// It runs in the serving process rather than as a cron job in the deployment,
// because the deployments this project targets are one container: a sidecar
// with a copy of the credentials and a second connection to the database, to
// call code that is already in the binary, is the more complicated of the two.
// An operator who would rather drive it from outside runs `kanban export` on
// their own schedule and leaves BACKUP_INTERVAL unset.
type Schedule struct {
	Store  store.Store
	Target Target
	// Every is the gap between snapshots. Zero disables the schedule.
	Every time.Duration
	// Keep is how many snapshots the target holds; older ones are deleted after
	// a successful write. Zero or less keeps every snapshot.
	Keep int
	// Build goes into the snapshot, so a file found in a bucket says which
	// version wrote it.
	Build string
	Log   *slog.Logger
	// Now is optional; tests replace it.
	Now func() time.Time
}

func (s *Schedule) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Schedule) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Run takes snapshots until ctx is cancelled, then returns nil. A snapshot that
// fails is logged and the schedule carries on: a bucket that is unreachable for
// an hour is not a reason to stop trying, and it is certainly not a reason to
// stop serving the board.
//
// It is called in a goroutine by serve, so misconfiguration is an error return
// and not a panic.
func (s *Schedule) Run(ctx context.Context) error {
	switch {
	case s.Store == nil:
		return errors.New("backup: schedule has no store")
	case s.Target == nil:
		return errors.New("backup: schedule has no target")
	case s.Every <= 0:
		return errors.New("backup: schedule has no interval")
	}
	s.log().Info("backup schedule started", "target", s.Target.String(), "every", s.Every, "keep", s.Keep)

	// One on start, unless the target already holds a snapshot from within the
	// interval. Without the check a container that restarts every few minutes
	// would fill the bucket; without the snapshot on start, a deployment that
	// restarts more often than the interval would never take one.
	if due, err := s.due(ctx); err != nil {
		s.log().Warn("backup: cannot tell when the last snapshot was taken", "target", s.Target.String(), "err", err)
	} else if due {
		s.runOnce(ctx)
	}

	ticker := time.NewTicker(s.Every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.log().Info("backup schedule stopped")
			return nil
		case <-ticker.C:
			s.runOnce(ctx)
		}
	}
}

// runOnce is Once with the logging, which is all the loop wants.
func (s *Schedule) runOnce(ctx context.Context) {
	start := s.now()
	name, err := s.Once(ctx)
	if err != nil {
		if ctx.Err() != nil {
			// Shutdown, not a failure. The next start takes one.
			return
		}
		s.log().Error("backup failed", "target", s.Target.String(), "err", err)
		return
	}
	s.log().Info("backup written", "target", s.Target.String(), "name", name, "took", s.now().Sub(start))
}

// Once takes one snapshot, writes it and applies the retention, and returns the
// name it wrote.
//
// Retention runs after the write, never before: deleting the oldest to make
// room and then failing to write the new one is how a target ends up with fewer
// backups than it is configured to keep.
func (s *Schedule) Once(ctx context.Context) (string, error) {
	snap, err := Export(ctx, s.Store)
	if err != nil {
		return "", fmt.Errorf("export: %w", err)
	}
	snap.Build = s.Build
	body, err := Bytes(snap)
	if err != nil {
		return "", fmt.Errorf("encode: %w", err)
	}
	name := Name(s.now())
	if err := s.Target.Put(ctx, name, body); err != nil {
		return "", err
	}
	if err := s.prune(ctx); err != nil {
		// The snapshot is safe, which is the point of the call. A target that
		// will not let go of the old ones is worth a line in the log and not a
		// failed backup.
		return name, fmt.Errorf("keep last %d: %w", s.Keep, err)
	}
	return name, nil
}

// prune deletes everything but the newest Keep snapshots.
func (s *Schedule) prune(ctx context.Context) error {
	if s.Keep <= 0 {
		return nil
	}
	names, err := s.Target.List(ctx)
	if err != nil {
		return err
	}
	if len(names) <= s.Keep {
		return nil
	}
	// List is oldest first, so the ones to go are at the front.
	for _, name := range names[:len(names)-s.Keep] {
		if err := s.Target.Delete(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

// due reports whether the newest snapshot in the target is older than the
// interval, which is also true when there is no snapshot at all.
func (s *Schedule) due(ctx context.Context) (bool, error) {
	names, err := s.Target.List(ctx)
	if err != nil {
		return false, err
	}
	if len(names) == 0 {
		return true, nil
	}
	at, ok := TakenAt(names[len(names)-1])
	if !ok {
		return true, nil
	}
	return s.now().Sub(at) >= s.Every, nil
}
