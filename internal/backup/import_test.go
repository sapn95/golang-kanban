package backup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"kanban/internal/model"
	"kanban/internal/store"
	"kanban/internal/store/memory"
)

// small is the least a snapshot can be and still import: one board, one column.
func small() *Snapshot {
	return &Snapshot{
		Format:  Format,
		TakenAt: stamp,
		Boards: []Board{{
			ID: "b-1", Slug: "work", Name: "Work",
			Columns: []Column{{ID: "c-1", Name: "To Do"}},
			Labels:  []Label{{ID: "l-1", Name: "bug", Color: "#e11d48"}},
			Cards: []Card{{
				ID: "k-1", ColumnID: "c-1", Title: "First",
				Labels:   []string{"l-1"},
				Subtasks: []Subtask{{ID: "s-1", Title: "one"}},
				Comments: []Comment{{ID: "m-1", Author: "sapn95", Body: "hello", CreatedAt: stamp}},
			}},
		}},
	}
}

func TestCheckCounts(t *testing.T) {
	rep, err := Check(small())
	must(t, "check", err)
	want := Report{Boards: 1, Columns: 1, Labels: 1, Cards: 1, Comments: 1}
	if rep != want {
		t.Errorf("report = %+v, want %+v", rep, want)
	}
	if got := rep.String(); got != "1 board, 1 column, 1 label, 1 card, 1 comment" {
		t.Errorf("String = %q", got)
	}
	rep.Boards, rep.Replaced, rep.Cards = 2, 1, 0
	if got := rep.String(); got != "2 boards, 1 column, 1 label, 0 cards, 1 comment (1 existing board overwritten)" {
		t.Errorf("String = %q", got)
	}
	rep.Replaced = 2
	if got := rep.String(); !strings.HasSuffix(got, "(2 existing boards overwritten)") {
		t.Errorf("String = %q", got)
	}
}

func TestCheckRefusesWhatCannotBeWritten(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
		bend func(*Snapshot)
	}{
		{"no snapshot at all", "no snapshot", nil},
		{"format 0", "format 0", func(s *Snapshot) { s.Format = 0 }},
		{"format from the future", "format 99", func(s *Snapshot) { s.Format = 99 }},
		{"board with no id", "a board has no id", func(s *Snapshot) { s.Boards[0].ID = "" }},
		{"board with no slug", "has no slug", func(s *Snapshot) { s.Boards[0].Slug = "" }},
		{"board with no columns", "has no columns", func(s *Snapshot) { s.Boards[0].Columns = nil }},
		{"two boards, one slug", "the slug work", func(s *Snapshot) {
			second := s.Boards[0]
			second.ID, second.Columns = "b-2", []Column{{ID: "c-2", Name: "A"}}
			second.Labels, second.Cards = nil, nil
			s.Boards = append(s.Boards, second)
		}},
		{"an id used twice", "used by two things (board and column)", func(s *Snapshot) { s.Boards[0].Columns[0].ID = "b-1" }},
		{"a card and a comment sharing an id", "used by two things (card and comment)", func(s *Snapshot) {
			s.Boards[0].Cards[0].Comments[0].ID = "k-1"
		}},
		{"a subtask with no id", "a subtask has no id", func(s *Snapshot) { s.Boards[0].Cards[0].Subtasks[0].ID = "" }},
		{"a label with no name", "has no name", func(s *Snapshot) { s.Boards[0].Labels[0].Name = "" }},
		{"a card in a column that is not there", "names column c-9", func(s *Snapshot) { s.Boards[0].Cards[0].ColumnID = "c-9" }},
		{"a card with a label the board lacks", "names label l-9", func(s *Snapshot) { s.Boards[0].Cards[0].Labels = []string{"l-9"} }},
		{"a card with a label from another board", "names label l-2", func(s *Snapshot) {
			second := Board{ID: "b-2", Slug: "other", Name: "Other",
				Columns: []Column{{ID: "c-2", Name: "A"}},
				Labels:  []Label{{ID: "l-2", Name: "ops"}}}
			s.Boards = append(s.Boards, second)
			s.Boards[0].Cards[0].Labels = []string{"l-2"}
		}},
		{"a due date that is not a date", `due_date "tomorrow"`, func(s *Snapshot) { s.Boards[0].Cards[0].DueDate = "tomorrow" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var snap *Snapshot
			if tc.bend != nil {
				snap = small()
				tc.bend(snap)
			}
			_, err := Check(snap)
			if err == nil {
				t.Fatal("accepted a snapshot that cannot be written")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
			// And Import refuses the same file without writing anything.
			s := memory.New()
			if _, err := Import(context.Background(), s, snap, Options{Replace: true}); err == nil {
				t.Fatal("Import accepted it")
			}
			boards, err := s.ListBoards(context.Background())
			must(t, "list boards", err)
			if len(boards) != 0 {
				t.Errorf("Import wrote %d boards before finding the problem", len(boards))
			}
		})
	}
}

func TestImportRefusesABoardThatIsAlreadyThere(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, want string
		bend       func(*Snapshot)
	}{
		{"same id", "id b-1", func(*Snapshot) {}},
		// A restore has to work when the board was recreated by hand under the
		// same name while the snapshot was being fetched, which is a new id.
		{"same slug", "slug work", func(s *Snapshot) { s.Boards[0].ID = "b-other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := memory.New()
			_, err := Import(ctx, s, small(), Options{})
			must(t, "first import", err)

			snap := small()
			tc.bend(snap)
			_, err = Import(ctx, s, snap, Options{})
			if !errors.Is(err, ErrExists) {
				t.Fatalf("error = %v, want ErrExists", err)
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "-replace") {
				t.Errorf("error = %v, want it to name %s and say how to overwrite", err, tc.want)
			}
			boards, err := s.ListBoards(ctx)
			must(t, "list boards", err)
			if len(boards) != 1 {
				t.Errorf("boards = %d, want the one that was already there", len(boards))
			}
		})
	}
}

// TestReplaceOverwrites is the restore case: the board is there, the snapshot
// wins, and nothing of the old board is left behind.
func TestReplaceOverwrites(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	_, err := Import(ctx, s, small(), Options{})
	must(t, "first import", err)

	// Something that is only in the store, so a stale card would be visible.
	board, err := s.GetBoard(ctx, "work")
	must(t, "get board", err)
	must(t, "extra card", s.CreateCard(ctx, &model.Card{
		ID: "k-stale", BoardID: board.ID, ColumnID: "c-1", Title: "written after the snapshot"}))

	snap := small()
	snap.Boards[0].Name = "Work, restored"
	rep, err := Import(ctx, s, snap, Options{Replace: true})
	must(t, "replace", err)
	if rep.Replaced != 1 {
		t.Errorf("Replaced = %d, want 1", rep.Replaced)
	}
	board, err = s.GetBoard(ctx, "work")
	must(t, "get board", err)
	if board.Name != "Work, restored" {
		t.Errorf("Name = %q, want the snapshot's", board.Name)
	}
	cards, err := s.ListCards(ctx, board.ID)
	must(t, "list cards", err)
	if len(cards) != 1 || cards[0].ID != "k-1" {
		t.Errorf("cards = %+v, want only the snapshot's", cards)
	}
	if _, err := s.GetCard(ctx, "k-stale"); !errors.Is(err, store.ErrNotFound) {
		t.Error("a card written after the snapshot survived the replace")
	}
}

// TestReplaceTakesBothClaimants is the awkward case Options.Replace names: the
// id belongs to one board and the slug to another, so a restore has to delete
// two.
func TestReplaceTakesBothClaimants(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	first := small()
	_, err := Import(ctx, s, first, Options{})
	must(t, "import the board with the id", err)

	renamed := small()
	renamed.Boards[0].ID = "b-2"
	renamed.Boards[0].Slug = "other"
	renamed.Boards[0].Columns[0].ID = "c-2"
	renamed.Boards[0].Labels = nil
	renamed.Boards[0].Cards = nil
	_, err = Import(ctx, s, renamed, Options{})
	must(t, "import the board with the slug", err)
	// Now b-1 holds slug work and b-2 holds slug other; a snapshot that claims
	// b-2 with slug work overlaps both.
	both := small()
	both.Boards[0].ID = "b-2"
	both.Boards[0].Columns[0].ID = "c-3"
	both.Boards[0].Labels[0].ID = "l-3"
	both.Boards[0].Cards[0].ColumnID = "c-3"
	both.Boards[0].Cards[0].Labels = []string{"l-3"}

	rep, err := Import(ctx, s, both, Options{Replace: true})
	must(t, "replace", err)
	if rep.Replaced != 2 {
		t.Errorf("Replaced = %d, want both boards", rep.Replaced)
	}
	boards, err := s.ListBoards(ctx)
	must(t, "list boards", err)
	if len(boards) != 1 || boards[0].ID != "b-2" || boards[0].Slug != "work" {
		t.Errorf("boards = %+v, want only the imported one", boards)
	}
}

// TestImportKeepsTheIDs is what makes a restore usable: a link someone saved
// still resolves.
func TestImportKeepsTheIDs(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	_, err := Import(ctx, s, small(), Options{})
	must(t, "import", err)
	card, err := s.GetCard(ctx, "k-1")
	must(t, "get card", err)
	if card.BoardID != "b-1" || card.ColumnID != "c-1" || card.Position != 1 {
		t.Errorf("card = %+v", card)
	}
	if len(card.Subtasks) != 1 || card.Subtasks[0].ID != "s-1" || card.Subtasks[0].Position != 1 {
		t.Errorf("subtasks = %+v", card.Subtasks)
	}
	if _, err := s.GetComment(ctx, "m-1"); err != nil {
		t.Errorf("comment id was not kept: %v", err)
	}
}

// TestImportWritesTheWholeCard covers the fields that only exist on the way in:
// a due date parsed back into a time, a WIP limit, a layout and an archive.
func TestImportWritesTheWholeCard(t *testing.T) {
	ctx := context.Background()
	archived := stamp.Add(-2 * time.Hour)
	snap := small()
	snap.Boards[0].Layout = model.LayoutRows
	snap.Boards[0].Columns[0].WIPLimit = 3
	snap.Boards[0].Cards[0].DueDate = "2026-12-24"
	snap.Boards[0].Cards[0].Assignee = "sapn95@users.noreply.github.com"
	snap.Boards[0].Cards[0].ArchivedAt = &archived
	snap.Boards[0].Cards[0].Subtasks = append(snap.Boards[0].Cards[0].Subtasks,
		Subtask{ID: "s-2", Title: "two", Done: true})

	s := memory.New()
	_, err := Import(ctx, s, snap, Options{})
	must(t, "import", err)

	board, err := s.GetBoard(ctx, "work")
	must(t, "get board", err)
	if board.Layout != model.LayoutRows || board.Columns[0].WIPLimit != 3 {
		t.Errorf("board = %+v", board)
	}
	card, err := s.GetCard(ctx, "k-1")
	must(t, "get card", err)
	if got := card.DueDate.Format(dateOnly); got != "2026-12-24" {
		t.Errorf("DueDate = %q", got)
	}
	if card.Assignee != "sapn95@users.noreply.github.com" {
		t.Errorf("Assignee = %q", card.Assignee)
	}
	if !card.Archived() || !card.ArchivedAt.Equal(archived) {
		t.Errorf("ArchivedAt = %v, want %v", card.ArchivedAt, archived)
	}
	if len(card.Subtasks) != 2 || !card.Subtasks[1].Done || card.Subtasks[1].Position != 2 {
		t.Errorf("subtasks = %+v", card.Subtasks)
	}
}

// TestImportSaysWhichWriteFailed checks the error wrapping, which is what an
// operator has to read at the point a restore stops half way.
func TestImportSaysWhichWriteFailed(t *testing.T) {
	for _, tc := range []struct{ method, want string }{
		{"GetBoardByID", "the database went away"},
		{"GetBoard", "the database went away"},
		{"CreateBoard", "import board work: create board"},
		{"CreateLabel", "create label bug"},
		{"CreateCard", "create card k-1"},
		{"SetCardArchived", "archive"},
		{"CreateComment", "create comment m-1"},
	} {
		t.Run(tc.method, func(t *testing.T) {
			snap := small()
			at := stamp
			snap.Boards[0].Cards[0].ArchivedAt = &at
			_, err := Import(context.Background(), broken{Store: memory.New(), fail: tc.method}, snap, Options{})
			if !errors.Is(err, errBroken) {
				t.Fatalf("error = %v, want the store's", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to say %q", err, tc.want)
			}
		})
	}
	// And the delete half, which only Replace reaches.
	s := memory.New()
	_, err := Import(context.Background(), s, small(), Options{})
	must(t, "import", err)
	_, err = Import(context.Background(), broken{Store: s, fail: "DeleteBoard"}, small(), Options{Replace: true})
	if !errors.Is(err, errBroken) || !strings.Contains(err.Error(), "delete board b-1") {
		t.Errorf("error = %v, want a failed delete naming the board", err)
	}
}

func TestPlural(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{{0, "0 boards"}, {1, "1 board"}, {2, "2 boards"}} {
		if got := plural(tc.n, "board"); got != tc.want {
			t.Errorf("plural(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
