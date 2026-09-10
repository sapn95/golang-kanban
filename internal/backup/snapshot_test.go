package backup

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	// seed() sets a board's SLA, and the service checks the zone name with
	// time.LoadLocation before it writes one. A host with no
	// /usr/share/zoneinfo would fail the seed rather than the assertion, so
	// the test binary carries the zone database the same way cmd/kanban does.
	_ "time/tzdata"

	"kanban/internal/model"
	"kanban/internal/service"
	"kanban/internal/store"
	"kanban/internal/store/memory"
	"kanban/internal/store/sqlite"
)

var update = flag.Bool("update", false, "rewrite testdata/snapshot.json from what Export produces now")

const goldenFile = "testdata/snapshot.json"

// backends are the store implementations a snapshot has to cross. Postgres is
// left out because it needs a server; what these tests check is the format, and
// the contract suite is what proves the three backends agree.
var backends = map[string]func(*testing.T) store.Store{
	"memory": func(*testing.T) store.Store { return memory.New() },
	"sqlite": func(t *testing.T) store.Store {
		s, err := sqlite.Open(filepath.Join(t.TempDir(), "kanban.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		if err := s.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		return s
	},
}

// stamp is the fixed TakenAt a comparison uses, since Export reads the clock.
var stamp = time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)

func must(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// seed writes a board of every shape the format carries: a column with a WIP
// limit, labels, a card with a due date, an assignee, subtasks and comments, an
// archived card, and a second board with the other layout and nothing on it.
//
// It goes through the service rather than the store so the data is the shape the
// running application produces, with the clock and the IDs fixed so two runs
// against two backends give the same file.
func seed(t *testing.T, s store.Store) {
	t.Helper()
	ctx := context.Background()
	tick := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	n := 0
	svc := service.New(s,
		service.WithClock(func() time.Time { tick = tick.Add(time.Minute); return tick }),
		service.WithIDs(func() model.ID { n++; return model.ID(fmt.Sprintf("id-%02d", n)) }),
	)

	work, err := svc.CreateBoard(ctx, "Work", "work", []string{"To Do", "Doing", "Done"})
	must(t, "create board", err)
	must(t, "wip limit", svc.UpdateColumn(ctx, work.Columns[1].ID, "Doing", 2, false))
	// A desk with a promise, and the column its clock does not run in.
	must(t, "stops clock", svc.UpdateColumn(ctx, work.Columns[2].ID, "Done", 0, true))
	must(t, "sla", svc.SetBoardSLA(ctx, work.ID, model.SLA{
		ResponseHours: 4, Days: model.MonToFri, Start: 8 * 60, End: 17 * 60, Zone: "Europe/Zurich",
	}))
	bug, err := svc.CreateLabel(ctx, work.ID, "bug", "#e11d48")
	must(t, "create label", err)
	_, err = svc.CreateLabel(ctx, work.ID, "ops", "")
	must(t, "create label", err)

	first, err := svc.CreateCard(ctx, work.ID, work.Columns[0].ID, service.CardInput{
		Title:       "Write the ADR",
		Description: "Say why S3 is a target and not a store.\nLink 0003.",
		DueDate:     "2026-03-31",
		Assignee:    "sapn95@users.noreply.github.com",
		Subtasks: []model.Subtask{
			{Title: "draft", Done: true},
			{Title: "review"},
		},
	})
	must(t, "create card", err)
	_, err = svc.ToggleCardLabel(ctx, first.ID, bug.ID)
	must(t, "label card", err)

	second, err := svc.CreateCard(ctx, work.ID, work.Columns[1].ID, service.CardInput{Title: "Fix the filer"})
	must(t, "create card", err)
	_, err = svc.AddComment(ctx, second.ID, "sapn95", "The qtree came back.")
	must(t, "comment", err)
	_, err = svc.AddComment(ctx, second.ID, "", "Anonymous, from the header-less deployment.")
	must(t, "comment", err)

	// Two cards in one column, so the order of the array is the order of the
	// board and an import that lost it would show up.
	_, err = svc.CreateCard(ctx, work.ID, work.Columns[0].ID, service.CardInput{Title: "Second in the column"})
	must(t, "create card", err)

	done, err := svc.CreateCard(ctx, work.ID, work.Columns[2].ID, service.CardInput{Title: "Ship it"})
	must(t, "create card", err)
	must(t, "archive", svc.ArchiveCard(ctx, done.ID))

	notes, err := svc.CreateBoard(ctx, "Notes", "notes", []string{"Inbox"})
	must(t, "create board", err)
	must(t, "layout", svc.SetBoardLayout(ctx, notes.ID, model.LayoutRows))
}

// export is Export with the clock-dependent fields pinned, which is what makes
// two exports comparable byte for byte.
func export(t *testing.T, s store.Store) []byte {
	t.Helper()
	snap, err := Export(context.Background(), s)
	must(t, "export", err)
	if snap.Format != Format {
		t.Fatalf("Format = %d, want %d", snap.Format, Format)
	}
	if time.Since(snap.TakenAt) > time.Hour || snap.TakenAt.Location() != time.UTC {
		t.Errorf("TakenAt = %v, want a recent UTC instant", snap.TakenAt)
	}
	snap.TakenAt = stamp
	snap.Build = "test"
	body, err := Bytes(snap)
	must(t, "encode", err)
	return body
}

// TestExportGolden pins the document itself. The round-trip test below proves
// export and import agree with each other; only a file on disk catches the two
// of them agreeing on something new.
func TestExportGolden(t *testing.T) {
	s := memory.New()
	seed(t, s)
	got := export(t, s)

	if *update {
		must(t, "write golden", os.MkdirAll("testdata", 0o755))
		must(t, "write golden", os.WriteFile(goldenFile, got, 0o644))
		t.Logf("wrote %s", goldenFile)
		return
	}
	want, err := os.ReadFile(goldenFile)
	must(t, "read golden", err)
	if !bytes.Equal(got, want) {
		t.Errorf("export does not match %s; run go test ./internal/backup -update and read the diff\n--- got ---\n%s", goldenFile, got)
	}
}

// TestGoldenImports is the other direction: the file on disk still loads. A
// change that made Export and Import agree on a format the old files are not in
// would pass the round trip and lose every snapshot anyone has kept.
func TestGoldenImports(t *testing.T) {
	body, err := os.ReadFile(goldenFile)
	must(t, "read golden", err)
	snap, err := Read(bytes.NewReader(body))
	must(t, "read snapshot", err)
	for name, open := range backends {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			rep, err := Import(context.Background(), s, snap, Options{})
			must(t, "import", err)
			if rep.Boards != 2 || rep.Cards != 4 || rep.Comments != 2 || rep.Labels != 2 || rep.Columns != 4 {
				t.Errorf("report = %+v", rep)
			}
			if got := export(t, s); !bytes.Equal(got, body) {
				t.Errorf("re-export of the golden file differs:\n%s", got)
			}
		})
	}
}

// TestRoundTrip is the property the format was built for: a snapshot taken from
// one backend, imported into another and exported again is the same document.
// Every backend pair, because a difference between two of them is exactly what
// a shared format is supposed to rule out.
func TestRoundTrip(t *testing.T) {
	for fromName, openFrom := range backends {
		for toName, openTo := range backends {
			t.Run(fromName+" to "+toName, func(t *testing.T) {
				from := openFrom(t)
				seed(t, from)
				want := export(t, from)

				snap, err := Read(bytes.NewReader(want))
				must(t, "read", err)
				to := openTo(t)
				if _, err := Import(context.Background(), to, snap, Options{}); err != nil {
					t.Fatalf("import: %v", err)
				}
				if got := export(t, to); !bytes.Equal(got, want) {
					t.Errorf("round trip changed the document:\n--- before ---\n%s\n--- after ---\n%s", want, got)
				}
			})
		}
	}
}

// TestArchivedCardComesBack is the one thing a round trip cannot show, because
// the export of a restored archive matches: an archived card is archived again
// on import, and it lands at the end of its column rather than in the row it
// left ([0012]).
func TestArchivedCardComesBack(t *testing.T) {
	src := memory.New()
	seed(t, src)
	snap, err := Export(context.Background(), src)
	must(t, "export", err)

	dst := memory.New()
	_, err = Import(context.Background(), dst, snap, Options{})
	must(t, "import", err)

	board, err := dst.GetBoard(context.Background(), "work")
	must(t, "get board", err)
	archived, err := dst.ListArchivedCards(context.Background(), board.ID)
	must(t, "list archived", err)
	if len(archived) != 1 || archived[0].Title != "Ship it" {
		t.Fatalf("archive holds %+v, want the one card that was archived", archived)
	}
	live, err := dst.ListCards(context.Background(), board.ID)
	must(t, "list cards", err)
	for _, c := range live {
		if c.Title == "Ship it" {
			t.Error("an archived card came back onto the board")
		}
	}
	if len(live) != 3 {
		t.Errorf("board holds %d live cards, want 3", len(live))
	}
}

func TestEmptyStoreExports(t *testing.T) {
	body := export(t, memory.New())
	snap, err := Read(bytes.NewReader(body))
	must(t, "read", err)
	if len(snap.Boards) != 0 {
		t.Fatalf("boards = %v", snap.Boards)
	}
	// An empty snapshot still imports, and importing it changes nothing.
	rep, err := Import(context.Background(), memory.New(), snap, Options{})
	must(t, "import", err)
	if rep != (Report{}) {
		t.Errorf("report = %+v, want nothing done", rep)
	}
	if !strings.Contains(string(body), `"boards": []`) {
		t.Errorf("an empty store must still write a boards array:\n%s", body)
	}
}

func TestReadRefusesWhatItCannotImport(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"not json", "{", "decode snapshot"},
		{"not a snapshot", `{"hello": "world"}`, "not a kanban snapshot"},
		{"from the future", `{"format": 99, "boards": []}`, "this build reads up to 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Read(strings.NewReader(tc.body))
			if err == nil {
				t.Fatalf("accepted %s", tc.body)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
			if tc.name != "not json" && !errors.Is(err, ErrFormat) {
				t.Errorf("error = %v, want ErrFormat", err)
			}
		})
	}
}

// TestWriteDoesNotEscape keeps a description readable in the file. The default
// encoder settings turn every angle bracket and ampersand into a \u escape,
// which shows up in every snapshot anyone opens.
func TestWriteDoesNotEscape(t *testing.T) {
	snap := &Snapshot{Format: Format, TakenAt: stamp, Boards: []Board{{
		ID: "b", Slug: "s", Name: "n", Columns: []Column{{ID: "c", Name: "A"}},
		Cards: []Card{{ID: "k", ColumnID: "c", Title: "a < b && c > d"}},
	}}}
	body, err := Bytes(snap)
	must(t, "encode", err)
	if !strings.Contains(string(body), "a < b && c > d") {
		t.Errorf("description was escaped:\n%s", body)
	}
	if !strings.HasSuffix(string(body), "\n") {
		t.Error("want a trailing newline, so the file ends like a text file")
	}
}

// failWriter is a writer that refuses, so Write's error is returned rather than
// swallowed by a bytes.Buffer that cannot fail.
type failWriter struct{}

var errWriter = errors.New("disk full")

func (failWriter) Write([]byte) (int, error) { return 0, errWriter }

func TestWriteReportsAWriteFailure(t *testing.T) {
	if err := Write(failWriter{}, &Snapshot{Format: Format}); !errors.Is(err, errWriter) {
		t.Errorf("Write = %v, want the writer's error", err)
	}
}

// broken wraps a store and fails one named method. It is how the error paths of
// Export and Import are reached without a second backend that misbehaves.
type broken struct {
	store.Store
	fail string
}

var errBroken = errors.New("the database went away")

func (b broken) check(name string) error {
	if b.fail == name {
		return errBroken
	}
	return nil
}

func (b broken) ListBoards(ctx context.Context) ([]model.Board, error) {
	if err := b.check("ListBoards"); err != nil {
		return nil, err
	}
	return b.Store.ListBoards(ctx)
}

func (b broken) ListCards(ctx context.Context, boardID model.ID) ([]model.Card, error) {
	if err := b.check("ListCards"); err != nil {
		return nil, err
	}
	return b.Store.ListCards(ctx, boardID)
}

func (b broken) ListArchivedCards(ctx context.Context, boardID model.ID) ([]model.Card, error) {
	if err := b.check("ListArchivedCards"); err != nil {
		return nil, err
	}
	return b.Store.ListArchivedCards(ctx, boardID)
}

func (b broken) CountComments(ctx context.Context, boardID model.ID) (map[model.ID]int, error) {
	if err := b.check("CountComments"); err != nil {
		return nil, err
	}
	return b.Store.CountComments(ctx, boardID)
}

func (b broken) ListComments(ctx context.Context, cardID model.ID) ([]model.Comment, error) {
	if err := b.check("ListComments"); err != nil {
		return nil, err
	}
	return b.Store.ListComments(ctx, cardID)
}

func (b broken) GetBoard(ctx context.Context, slug string) (*model.Board, error) {
	if err := b.check("GetBoard"); err != nil {
		return nil, err
	}
	return b.Store.GetBoard(ctx, slug)
}

func (b broken) GetBoardByID(ctx context.Context, id model.ID) (*model.Board, error) {
	if err := b.check("GetBoardByID"); err != nil {
		return nil, err
	}
	return b.Store.GetBoardByID(ctx, id)
}

func (b broken) CreateBoard(ctx context.Context, board *model.Board) error {
	if err := b.check("CreateBoard"); err != nil {
		return err
	}
	return b.Store.CreateBoard(ctx, board)
}

func (b broken) CreateLabel(ctx context.Context, l *model.Label) error {
	if err := b.check("CreateLabel"); err != nil {
		return err
	}
	return b.Store.CreateLabel(ctx, l)
}

func (b broken) CreateCard(ctx context.Context, c *model.Card) error {
	if err := b.check("CreateCard"); err != nil {
		return err
	}
	return b.Store.CreateCard(ctx, c)
}

func (b broken) CreateComment(ctx context.Context, c *model.Comment) error {
	if err := b.check("CreateComment"); err != nil {
		return err
	}
	return b.Store.CreateComment(ctx, c)
}

func (b broken) SetCardArchived(ctx context.Context, id model.ID, at time.Time) error {
	if err := b.check("SetCardArchived"); err != nil {
		return err
	}
	return b.Store.SetCardArchived(ctx, id, at)
}

func (b broken) DeleteBoard(ctx context.Context, id model.ID) error {
	if err := b.check("DeleteBoard"); err != nil {
		return err
	}
	return b.Store.DeleteBoard(ctx, id)
}

func TestExportSaysWhichReadFailed(t *testing.T) {
	for _, tc := range []struct{ method, want string }{
		{"ListBoards", "list boards"},
		// Boards come back ordered by name, so the first one read is Notes.
		{"ListCards", "list cards of notes"},
		{"ListArchivedCards", "list archived cards of notes"},
		{"CountComments", "count comments of notes"},
		{"ListComments", "list comments of card"},
	} {
		t.Run(tc.method, func(t *testing.T) {
			s := memory.New()
			seed(t, s)
			_, err := Export(context.Background(), broken{Store: s, fail: tc.method})
			if !errors.Is(err, errBroken) {
				t.Fatalf("error = %v, want the store's", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to say %q", err, tc.want)
			}
		})
	}
}
