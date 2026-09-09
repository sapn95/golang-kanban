package backup

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kanban/internal/store/memory"
)

// logs is a log a test can read while the schedule is still writing to it.
type logs struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *logs) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// clock is a hand-wound clock, so a test can put four snapshots a day apart
// into a target without waiting.
type clock struct {
	mu   sync.Mutex
	at   time.Time
	step time.Duration
}

func newClock() *clock { return &clock{at: stamp, step: 24 * time.Hour} }

// now returns the current instant and moves on, which gives every snapshot in a
// test its own name.
func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	at := c.at
	c.at = c.at.Add(c.step)
	return at
}

// target is a Dir with a lever for each call, so the failure paths are reachable
// without a bucket that misbehaves. The levers are atomic because the schedule
// runs in its own goroutine and a test turns them over while it is running.
type target struct {
	Dir
	failPut, failList, failDelete atomic.Bool
	// wrote receives every name Put stored, so a test can wait for the schedule
	// instead of sleeping.
	wrote chan string
}

var errTarget = errors.New("the bucket said no")

func newTarget(t *testing.T) *target {
	t.Helper()
	return &target{Dir: Dir{Path: t.TempDir()}, wrote: make(chan string, 16)}
}

func (tg *target) Put(ctx context.Context, name string, body []byte) error {
	if tg.failPut.Load() {
		return errTarget
	}
	if err := tg.Dir.Put(ctx, name, body); err != nil {
		return err
	}
	select {
	case tg.wrote <- name:
	default:
	}
	return nil
}

func (tg *target) List(ctx context.Context) ([]string, error) {
	if tg.failList.Load() {
		return nil, errTarget
	}
	return tg.Dir.List(ctx)
}

func (tg *target) Delete(ctx context.Context, name string) error {
	if tg.failDelete.Load() {
		return errTarget
	}
	return tg.Dir.Delete(ctx, name)
}

// waitFor waits for the next snapshot the schedule writes. The timeout is what
// turns a schedule that has stopped taking backups into a failed test rather
// than one that hangs.
func (tg *target) waitFor(t *testing.T) string {
	t.Helper()
	select {
	case name := <-tg.wrote:
		return name
	case <-time.After(5 * time.Second):
		t.Fatal("no snapshot was written")
		return ""
	}
}

func testSchedule(t *testing.T, tg Target) (*Schedule, *logs) {
	t.Helper()
	l := &logs{}
	s := memory.New()
	seed(t, s)
	return &Schedule{Store: s, Target: tg, Every: time.Hour, Build: "test", Log: l.logger(), Now: newClock().now}, l
}

func TestOnceWritesASnapshotThatImports(t *testing.T) {
	ctx := context.Background()
	tg := newTarget(t)
	sch, _ := testSchedule(t, tg)

	name, err := sch.Once(ctx)
	must(t, "once", err)
	if at, ok := TakenAt(name); !ok || !at.Equal(stamp) {
		t.Errorf("name = %q, want the clock's instant", name)
	}
	names, err := tg.List(ctx)
	must(t, "list", err)
	if len(names) != 1 || names[0] != name {
		t.Errorf("target holds %v, want %s", names, name)
	}

	// What landed is a snapshot of the store, with the build stamped on it.
	body, err := os.ReadFile(tg.file(name))
	must(t, "read back", err)
	snap, err := Read(bytes.NewReader(body))
	must(t, "read snapshot", err)
	if snap.Build != "test" {
		t.Errorf("Build = %q, want the one the schedule was given", snap.Build)
	}
	rep, err := Import(ctx, memory.New(), snap, Options{})
	must(t, "import", err)
	if rep.Boards != 2 {
		t.Errorf("report = %+v, want the seeded boards", rep)
	}
}

// TestKeepDeletesTheOldest is the retention rule: the newest Keep survive and
// the rest go, and a target with fewer than Keep is left alone.
func TestKeepDeletesTheOldest(t *testing.T) {
	ctx := context.Background()
	tg := newTarget(t)
	sch, _ := testSchedule(t, tg)
	sch.Keep = 2

	var written []string
	for range 4 {
		name, err := sch.Once(ctx)
		must(t, "once", err)
		written = append(written, name)
	}
	names, err := tg.List(ctx)
	must(t, "list", err)
	if len(names) != 2 || names[0] != written[2] || names[1] != written[3] {
		t.Errorf("target holds %v, want the last two of %v", names, written)
	}
}

func TestKeepZeroKeepsEverything(t *testing.T) {
	ctx := context.Background()
	tg := newTarget(t)
	sch, _ := testSchedule(t, tg)
	for range 3 {
		_, err := sch.Once(ctx)
		must(t, "once", err)
	}
	names, err := tg.List(ctx)
	must(t, "list", err)
	if len(names) != 3 {
		t.Errorf("target holds %v, want all three", names)
	}
}

// TestPruneFailureStillReportsTheSnapshot: the write is the point of the call,
// so a target that will not let go of the old files is an error beside a name
// rather than a lost backup.
func TestPruneFailureStillReportsTheSnapshot(t *testing.T) {
	ctx := context.Background()
	tg := newTarget(t)
	sch, _ := testSchedule(t, tg)
	sch.Keep = 1

	first, err := sch.Once(ctx)
	must(t, "first", err)

	tg.failDelete.Store(true)
	name, err := sch.Once(ctx)
	if name == "" {
		t.Error("Once did not say what it wrote")
	}
	if !errors.Is(err, errTarget) || !strings.Contains(err.Error(), "keep last 1") {
		t.Errorf("error = %v, want the failed retention", err)
	}
	names, err := tg.Dir.List(ctx)
	must(t, "list", err)
	if len(names) != 2 || names[0] != first {
		t.Errorf("target holds %v, want both snapshots", names)
	}

	// The same for a target that cannot be listed at all.
	tg.failDelete.Store(false)
	tg.failList.Store(true)
	if _, err := sch.Once(ctx); !errors.Is(err, errTarget) {
		t.Errorf("error = %v, want the failed listing", err)
	}
}

// TestPruneFailureIsNotLoggedAsALostBackup: the log is the only place an
// operator hears about either half, and "backup failed" would send them looking
// for a snapshot that is sitting in the target.
func TestPruneFailureIsNotLoggedAsALostBackup(t *testing.T) {
	ctx := context.Background()
	tg := newTarget(t)
	sch, l := testSchedule(t, tg)
	sch.Keep = 1
	_, err := sch.Once(ctx)
	must(t, "first", err)

	tg.failDelete.Store(true)
	sch.runOnce(ctx)
	if strings.Contains(l.String(), "backup failed") {
		t.Errorf("a written snapshot was logged as a failed backup:\n%s", l.String())
	}
	if !strings.Contains(l.String(), "retention failed") {
		t.Errorf("the failed retention was not logged:\n%s", l.String())
	}
	names, err := tg.Dir.List(ctx)
	must(t, "list", err)
	if len(names) != 2 {
		t.Errorf("target holds %v, want the snapshot the log named", names)
	}
	if !strings.Contains(l.String(), names[1]) {
		t.Errorf("the log does not name the snapshot it wrote:\n%s", l.String())
	}
}

func TestOnceReportsWhatFailed(t *testing.T) {
	ctx := context.Background()
	t.Run("the store", func(t *testing.T) {
		tg := newTarget(t)
		sch, _ := testSchedule(t, tg)
		sch.Store = broken{Store: sch.Store, fail: "ListBoards"}
		if _, err := sch.Once(ctx); !errors.Is(err, errBroken) || !strings.Contains(err.Error(), "export") {
			t.Errorf("error = %v, want the failed export", err)
		}
	})
	t.Run("the target", func(t *testing.T) {
		tg := newTarget(t)
		tg.failPut.Store(true)
		sch, _ := testSchedule(t, tg)
		if _, err := sch.Once(ctx); !errors.Is(err, errTarget) {
			t.Errorf("error = %v, want the target's", err)
		}
	})
}

func TestRunNeedsToBeConfigured(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		bend       func(*Schedule)
	}{
		{"no store", "no store", func(s *Schedule) { s.Store = nil }},
		{"no target", "no target", func(s *Schedule) { s.Target = nil }},
		{"no interval", "no interval", func(s *Schedule) { s.Every = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sch, _ := testSchedule(t, newTarget(t))
			tc.bend(sch)
			err := sch.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Run = %v, want it to say %q", err, tc.want)
			}
		})
	}
}

// TestRunTakesOneOnStart: a deployment that is restarted more often than the
// interval would otherwise never take a snapshot at all.
func TestRunTakesOneOnStart(t *testing.T) {
	tg := newTarget(t)
	sch, l := testSchedule(t, tg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sch.Run(ctx) }()

	name := tg.waitFor(t)
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run = %v, want nil after the context was cancelled", err)
	}
	if !strings.Contains(l.String(), "backup written") || !strings.Contains(l.String(), name) {
		t.Errorf("log does not say what was written:\n%s", l.String())
	}
	if !strings.Contains(l.String(), "backup schedule stopped") {
		t.Errorf("log does not say the schedule stopped:\n%s", l.String())
	}
}

// TestRunSkipsTheSnapshotOnStart is the other half: a container in a restart
// loop must not write a snapshot per restart.
func TestRunSkipsTheSnapshotOnStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tg := newTarget(t)
	sch, _ := testSchedule(t, tg)
	// A snapshot from a minute ago, with the interval an hour.
	fresh := Name(stamp.Add(-time.Minute))
	must(t, "put", tg.Dir.Put(ctx, fresh, []byte("{}")))

	done := make(chan error, 1)
	go func() { done <- sch.Run(ctx) }()
	select {
	case name := <-tg.wrote:
		t.Errorf("wrote %s although %s is fresher than the interval", name, fresh)
	case <-time.After(150 * time.Millisecond):
	}
	cancel()
	must(t, "run", <-done)
}

// TestDue is the freshness rule on its own, including the two cases that answer
// yes without a timestamp to compare: an empty target and a name this package
// did not write.
func TestDue(t *testing.T) {
	ctx := context.Background()
	tg := newTarget(t)
	sch, _ := testSchedule(t, tg)
	sch.Now = func() time.Time { return stamp }

	due, err := sch.due(ctx)
	must(t, "due", err)
	if !due {
		t.Error("an empty target is due")
	}

	must(t, "put", tg.Dir.Put(ctx, Name(stamp.Add(-2*time.Hour)), []byte("{}")))
	due, err = sch.due(ctx)
	must(t, "due", err)
	if !due {
		t.Error("a snapshot older than the interval is due")
	}

	must(t, "put", tg.Dir.Put(ctx, Name(stamp.Add(-time.Minute)), []byte("{}")))
	due, err = sch.due(ctx)
	must(t, "due", err)
	if due {
		t.Error("a snapshot from within the interval is not due")
	}

	tg.failList.Store(true)
	if _, err := sch.due(ctx); !errors.Is(err, errTarget) {
		t.Errorf("error = %v, want the target's", err)
	}
}

// TestRunStartsWhenItCannotList: a target that cannot be listed on start is a
// warning and the schedule runs on, because the alternative is no backups.
func TestRunStartsWhenItCannotList(t *testing.T) {
	tg := newTarget(t)
	tg.failList.Store(true)
	sch, l := testSchedule(t, tg)
	sch.Every = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sch.Run(ctx) }()

	tg.waitFor(t) // from the ticker, not from the start
	cancel()
	must(t, "run", <-done)
	if !strings.Contains(l.String(), "cannot tell when the last snapshot was taken") {
		t.Errorf("log does not carry the warning:\n%s", l.String())
	}
}

// TestRunCarriesOnAfterAFailure: a bucket that is unreachable for an hour is
// not a reason to stop trying, and never a reason to stop serving the board.
func TestRunCarriesOnAfterAFailure(t *testing.T) {
	tg := newTarget(t)
	tg.failPut.Store(true)
	sch, l := testSchedule(t, tg)
	sch.Every = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sch.Run(ctx) }()

	// Wait for the failure to be logged, then let the target work again.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(l.String(), "backup failed") {
		if time.Now().After(deadline) {
			t.Fatalf("no failure was logged:\n%s", l.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	tg.failPut.Store(false)
	tg.waitFor(t)
	cancel()
	must(t, "run", <-done)
}

// TestRunSaysNothingAboutAShutdown: cancelling the context mid-snapshot is a
// shutdown, and an operator reading the log after a deployment should not find
// a failed backup in it.
func TestRunSaysNothingAboutAShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tg := newTarget(t)
	tg.failPut.Store(true)
	sch, l := testSchedule(t, tg)
	sch.runOnce(ctx)
	if strings.Contains(l.String(), "backup failed") {
		t.Errorf("a cancelled snapshot was logged as a failure:\n%s", l.String())
	}
}

func TestScheduleFallsBackToTheClockAndTheDefaultLogger(t *testing.T) {
	sch := &Schedule{}
	if sch.now().IsZero() {
		t.Error("now() must fall back to the clock")
	}
	if sch.log() == nil {
		t.Error("log() must fall back to the default logger")
	}
}
