package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"kanban/internal/model"
	"kanban/internal/store"
	"kanban/internal/store/memory"
)

var fixed = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

func newSvc(t *testing.T) *Kanban {
	t.Helper()
	n := 0
	return New(memory.New(),
		WithClock(func() time.Time { return fixed }),
		WithIDs(func() model.ID { n++; return model.ID(fmt.Sprintf("id%03d", n)) }))
}

func isValidation(err error, field string) bool {
	var ve *ValidationError
	return errors.As(err, &ve) && ve.Field == field
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Kanban Board":           "kanban-board",
		"  Hello,  World!":       "hello-world",
		"--x--":                  "x",
		"ÄÖÜ":                    "",
		"a":                      "a",
		strings.Repeat("a", 100): strings.Repeat("a", MaxSlug),
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEnsureDefaultBoard(t *testing.T) {
	k := newSvc(t)
	ctx := context.Background()
	boards, err := k.EnsureDefaultBoard(ctx)
	if err != nil || len(boards) != 1 || boards[0].Slug != DefaultBoardSlug || len(boards[0].Columns) != 3 {
		t.Fatalf("boards = %+v, %v", boards, err)
	}
	if boards[0].Columns[1].Name != "In Progress" || !boards[0].CreatedAt.Equal(fixed) {
		t.Fatalf("board = %+v", boards[0])
	}
	again, err := k.EnsureDefaultBoard(ctx)
	if err != nil || len(again) != 1 || again[0].ID != boards[0].ID {
		t.Fatalf("second call = %+v, %v", again, err)
	}
	list, _ := k.Boards(ctx)
	if len(list) != 1 {
		t.Fatalf("Boards = %+v", list)
	}
	if _, err := k.Board(ctx, DefaultBoardSlug); err != nil {
		t.Fatal(err)
	}
	if _, err := k.BoardByID(ctx, boards[0].ID); err != nil {
		t.Fatal(err)
	}
}

func TestCreateBoardValidation(t *testing.T) {
	k := newSvc(t)
	ctx := context.Background()
	if _, err := k.CreateBoard(ctx, "  ", "", nil); !isValidation(err, "name") {
		t.Errorf("empty name: %v", err)
	}
	if _, err := k.CreateBoard(ctx, strings.Repeat("x", MaxName+1), "", nil); !isValidation(err, "name") {
		t.Errorf("long name: %v", err)
	}
	if _, err := k.CreateBoard(ctx, "Ok", "Not A Slug", nil); !isValidation(err, "slug") {
		t.Errorf("bad slug: %v", err)
	}
	if _, err := k.CreateBoard(ctx, "ÄÖÜ", "", nil); !isValidation(err, "slug") {
		t.Errorf("unslugifiable name: %v", err)
	}
	if _, err := k.CreateBoard(ctx, "Ok", "", []string{"A", " "}); !isValidation(err, "column") {
		t.Errorf("empty column: %v", err)
	}
	b, err := k.CreateBoard(ctx, "My Team", "", []string{"Backlog", "Live"})
	if err != nil || b.Slug != "my-team" || len(b.Columns) != 2 || b.Columns[1].Name != "Live" {
		t.Fatalf("board = %+v, %v", b, err)
	}
	if _, err := k.CreateBoard(ctx, "My Team", "", nil); !errors.Is(err, store.ErrConflict) {
		t.Errorf("duplicate slug: %v", err)
	}

	if _, err := k.RenameBoard(ctx, b.ID, "Renamed", ""); err != nil {
		t.Fatal(err)
	}
	got, _ := k.Board(ctx, "my-team")
	if got.Name != "Renamed" {
		t.Errorf("rename: %+v", got)
	}
	if _, err := k.RenameBoard(ctx, b.ID, "Renamed", "bad slug"); !isValidation(err, "slug") {
		t.Errorf("rename bad slug: %v", err)
	}
	if _, err := k.RenameBoard(ctx, b.ID, "", ""); !isValidation(err, "name") {
		t.Errorf("rename empty: %v", err)
	}
	if _, err := k.RenameBoard(ctx, "nope", "x", ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("rename unknown: %v", err)
	}
	if err := k.DeleteBoard(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := k.DeleteBoard(ctx, b.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete twice: %v", err)
	}
}

func TestColumns(t *testing.T) {
	k := newSvc(t)
	ctx := context.Background()
	b, _ := k.CreateBoard(ctx, "B", "", nil)
	if _, err := k.AddColumn(ctx, b.ID, " ", 0); !isValidation(err, "name") {
		t.Errorf("empty: %v", err)
	}
	if _, err := k.AddColumn(ctx, b.ID, "X", -1); !isValidation(err, "wip_limit") {
		t.Errorf("negative: %v", err)
	}
	c, err := k.AddColumn(ctx, b.ID, " Review ", 2)
	if err != nil || c.Name != "Review" || c.Position != 4 {
		t.Fatalf("column = %+v, %v", c, err)
	}
	if err := k.UpdateColumn(ctx, c.ID, "", 0); !isValidation(err, "name") {
		t.Errorf("update empty: %v", err)
	}
	if err := k.UpdateColumn(ctx, c.ID, "QA", -2); !isValidation(err, "wip_limit") {
		t.Errorf("update negative: %v", err)
	}
	if err := k.UpdateColumn(ctx, c.ID, "QA", 3); err != nil {
		t.Fatal(err)
	}
	order := []model.ID{c.ID, b.Columns[0].ID, b.Columns[1].ID, b.Columns[2].ID}
	if err := k.ReorderColumns(ctx, b.ID, order); err != nil {
		t.Fatal(err)
	}
	got, _ := k.Board(ctx, b.Slug)
	if got.Columns[0].Name != "QA" || got.Columns[0].WIPLimit != 3 {
		t.Fatalf("columns = %+v", got.Columns)
	}
	if err := k.RemoveColumn(ctx, b.ID, c.ID, b.Columns[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := k.RemoveColumn(ctx, b.ID, c.ID, ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("remove twice: %v", err)
	}
}

func TestTheLastColumnCannotBeDeleted(t *testing.T) {
	k := newSvc(t)
	ctx := context.Background()
	b, err := k.CreateBoard(ctx, "One", "", []string{"Only"})
	if err != nil {
		t.Fatal(err)
	}
	// A board with no columns holds no cards and offers nowhere to put one,
	// so the delete would leave something only the database could repair.
	if err := k.RemoveColumn(ctx, b.ID, b.Columns[0].ID, ""); !isValidation(err, "column") {
		t.Errorf("deleting the last column = %v, want a validation error", err)
	}
	got, _ := k.Board(ctx, b.Slug)
	if len(got.Columns) != 1 {
		t.Errorf("board has %d columns, want the one it started with", len(got.Columns))
	}

	// A column from another board is a miss, not a delete.
	other, err := k.CreateBoard(ctx, "Two", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.RemoveColumn(ctx, b.ID, other.Columns[0].ID, ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleting another board's column = %v, want ErrNotFound", err)
	}
	// And so is a destination that belongs to another board, which would
	// otherwise move cards across boards.
	if err := k.RemoveColumn(ctx, other.ID, other.Columns[0].ID, b.Columns[0].ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("moving cards to another board's column = %v, want ErrNotFound", err)
	}
}

func TestCards(t *testing.T) {
	k := newSvc(t)
	ctx := context.Background()
	b, _ := k.CreateBoard(ctx, "B", "", nil)
	todo, doing := b.Columns[0].ID, b.Columns[1].ID
	lbl, err := k.CreateLabel(ctx, b.ID, " bug ", " #f00 ")
	if err != nil || lbl.Name != "bug" || lbl.Color != "#f00" {
		t.Fatalf("label = %+v, %v", lbl, err)
	}

	bad := []struct {
		name string
		in   CardInput
		fld  string
	}{
		{"empty title", CardInput{Title: " "}, "title"},
		{"long title", CardInput{Title: strings.Repeat("t", MaxTitle+1)}, "title"},
		{"long description", CardInput{Title: "t", Description: strings.Repeat("d", MaxDescription+1)}, "description"},
		{"bad date", CardInput{Title: "t", DueDate: "24.12.2026"}, "due_date"},
		{"long subtask", CardInput{Title: "t", Subtasks: []model.Subtask{{Title: strings.Repeat("s", MaxTitle+1)}}}, "subtask"},
	}
	for _, tc := range bad {
		if _, err := k.CreateCard(ctx, b.ID, todo, tc.in); !isValidation(err, tc.fld) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	if _, err := k.CreateCard(ctx, "nope", todo, CardInput{Title: "t"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown board: %v", err)
	}
	if _, err := k.CreateCard(ctx, b.ID, "nope", CardInput{Title: "t"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown column: %v", err)
	}

	c, err := k.CreateCard(ctx, b.ID, todo, CardInput{
		Title: " Write tests ", Description: "line1\r\nline2", DueDate: "2026-12-24", Labels: []model.ID{lbl.ID},
		Subtasks: []model.Subtask{{Title: " one "}, {Title: ""}, {Title: "two", Done: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Title != "Write tests" || c.Description != "line1\nline2" || c.DueDate.Format("2006-01-02") != "2026-12-24" ||
		c.Position != 1 || !c.CreatedAt.Equal(fixed) || len(c.Labels) != 1 {
		t.Fatalf("card = %+v", c)
	}
	if len(c.Subtasks) != 2 || c.Subtasks[0].ID == "" || c.Subtasks[0].Title != "one" || c.Subtasks[1].Position != 2 || !c.Subtasks[1].Done {
		t.Fatalf("subtasks = %+v", c.Subtasks)
	}

	keep := c.Subtasks[1].ID
	u, err := k.UpdateCard(ctx, c.ID, CardInput{Title: "Renamed", Subtasks: []model.Subtask{{ID: keep, Title: "two"}, {Title: "three"}}})
	if err != nil {
		t.Fatal(err)
	}
	if u.Title != "Renamed" || !u.DueDate.IsZero() || len(u.Labels) != 0 || len(u.Subtasks) != 2 || u.Subtasks[0].ID != keep || u.Subtasks[1].ID == "" {
		t.Fatalf("updated = %+v", u)
	}
	if _, err := k.UpdateCard(ctx, c.ID, CardInput{Title: ""}); !isValidation(err, "title") {
		t.Errorf("update empty title: %v", err)
	}
	if _, err := k.UpdateCard(ctx, "nope", CardInput{Title: "x"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("update unknown: %v", err)
	}
	if got, err := k.Card(ctx, c.ID); err != nil || got.Title != "Renamed" {
		t.Fatalf("Card = %+v, %v", got, err)
	}

	c2, _ := k.CreateCard(ctx, b.ID, todo, CardInput{Title: "second"})
	if err := k.ReorderCards(ctx, b.ID, doing, []model.ID{c2.ID}); err != nil {
		t.Fatal(err)
	}
	cards, _ := k.Cards(ctx, b.ID)
	if len(cards) != 2 || cards[1].ID != c2.ID || cards[1].ColumnID != doing {
		t.Fatalf("cards = %+v", cards)
	}
	if err := k.ReorderCards(ctx, "nope", doing, nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("reorder unknown board: %v", err)
	}
	if err := k.ReorderCards(ctx, b.ID, "nope", nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("reorder unknown column: %v", err)
	}
	if err := k.DeleteCard(ctx, c2.ID); err != nil {
		t.Fatal(err)
	}
	if err := k.DeleteCard(ctx, c2.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete twice: %v", err)
	}

	if _, err := k.CreateLabel(ctx, b.ID, "", ""); !isValidation(err, "name") {
		t.Errorf("empty label: %v", err)
	}
	if err := k.UpdateLabel(ctx, lbl.ID, "", ""); !isValidation(err, "name") {
		t.Errorf("empty label update: %v", err)
	}
	if err := k.UpdateLabel(ctx, lbl.ID, "defect", ""); err != nil {
		t.Fatal(err)
	}
	if err := k.DeleteLabel(ctx, lbl.ID); err != nil {
		t.Fatal(err)
	}
}

func TestWIPLimit(t *testing.T) {
	k := newSvc(t)
	ctx := context.Background()
	b, _ := k.CreateBoard(ctx, "B", "", nil)
	todo, doing := b.Columns[0].ID, b.Columns[1].ID
	if err := k.UpdateColumn(ctx, doing, "Doing", 1); err != nil {
		t.Fatal(err)
	}
	c1, _ := k.CreateCard(ctx, b.ID, todo, CardInput{Title: "1"})
	c2, _ := k.CreateCard(ctx, b.ID, todo, CardInput{Title: "2"})
	d1, err := k.CreateCard(ctx, b.ID, doing, CardInput{Title: "d1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.CreateCard(ctx, b.ID, doing, CardInput{Title: "d2"}); !errors.Is(err, ErrWIPLimit) {
		t.Errorf("create over limit: %v", err)
	}
	if err := k.ReorderCards(ctx, b.ID, doing, []model.ID{c1.ID, d1.ID}); !errors.Is(err, ErrWIPLimit) {
		t.Errorf("move over limit: %v", err)
	}
	// Reordering within the full column is fine.
	if err := k.ReorderCards(ctx, b.ID, doing, []model.ID{d1.ID}); err != nil {
		t.Errorf("reorder at limit: %v", err)
	}
	// Moving the only card out and another in is fine when done as two calls.
	if err := k.ReorderCards(ctx, b.ID, todo, []model.ID{d1.ID, c1.ID, c2.ID}); err != nil {
		t.Fatal(err)
	}
	if err := k.ReorderCards(ctx, b.ID, doing, []model.ID{c2.ID}); err != nil {
		t.Errorf("move into emptied column: %v", err)
	}
	// A card already in the column does not count twice.
	if err := k.ReorderCards(ctx, b.ID, doing, []model.ID{c2.ID}); err != nil {
		t.Errorf("same card again: %v", err)
	}
}

// failing wraps a store and fails ListCards, to cover error propagation.
type failing struct {
	store.Store
}

var errBoom = errors.New("boom")

func (failing) ListCards(context.Context, model.ID) ([]model.Card, error) { return nil, errBoom }

func TestStoreErrors(t *testing.T) {
	mem := memory.New()
	k := New(failing{mem})
	ctx := context.Background()
	b, err := k.CreateBoard(ctx, "B", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.UpdateColumn(ctx, b.Columns[0].ID, "A", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := k.CreateCard(ctx, b.ID, b.Columns[0].ID, CardInput{Title: "t"}); !errors.Is(err, errBoom) {
		t.Errorf("CreateCard: %v", err)
	}
	if err := k.ReorderCards(ctx, b.ID, b.Columns[0].ID, nil); !errors.Is(err, errBoom) {
		t.Errorf("ReorderCards: %v", err)
	}
	if _, err := k.Cards(ctx, b.ID); !errors.Is(err, errBoom) {
		t.Errorf("Cards: %v", err)
	}
	ve := &ValidationError{Field: "f", Message: "m"}
	if ve.Error() != "f: m" {
		t.Errorf("Error() = %q", ve.Error())
	}
}

func TestBulkRejects(t *testing.T) {
	k := newSvc(t)
	ctx := context.Background()
	b, _ := k.CreateBoard(ctx, "B", "", nil)
	c, err := k.CreateCard(ctx, b.ID, b.Columns[0].ID, CardInput{Title: "one"})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		action BulkAction
		ids    []model.ID
		field  string
	}{
		{"nothing selected", BulkDelete, nil, "ids"},
		{"unknown action", BulkAction("burn"), []model.ID{c.ID}, "action"},
		{"more than the cap", BulkDelete, make([]model.ID, MaxBulk+1), "ids"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := k.Bulk(ctx, b.ID, tt.action, tt.ids, ""); !isValidation(err, tt.field) {
				t.Errorf("err = %v, want a validation error on %q", err, tt.field)
			}
		})
	}
}

func TestBulkActions(t *testing.T) {
	ctx := context.Background()

	setup := func(t *testing.T) (*Kanban, *model.Board, []model.ID) {
		t.Helper()
		k := newSvc(t)
		b, _ := k.CreateBoard(ctx, "B", "", nil)
		var ids []model.ID
		for _, title := range []string{"one", "two", "three"} {
			c, err := k.CreateCard(ctx, b.ID, b.Columns[0].ID, CardInput{Title: title})
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, c.ID)
		}
		return k, b, ids
	}

	t.Run("assign", func(t *testing.T) {
		k, b, ids := setup(t)
		res, err := k.Bulk(ctx, b.ID, BulkAssign, ids[:2], " someone@example.com ")
		if err != nil || len(res.Changed) != 2 || len(res.Failed) != 0 {
			t.Fatalf("res = %+v, err = %v", res, err)
		}
		for _, id := range ids[:2] {
			c, err := k.Card(ctx, id)
			if err != nil || c.Assignee != "someone@example.com" {
				t.Errorf("card %s assignee = %q (err %v), want it trimmed and set", id, c.Assignee, err)
			}
		}
		if c, _ := k.Card(ctx, ids[2]); c.Assignee != "" {
			t.Errorf("an unselected card was assigned: %q", c.Assignee)
		}
	})

	t.Run("unassign", func(t *testing.T) {
		k, b, ids := setup(t)
		if _, err := k.Bulk(ctx, b.ID, BulkAssign, ids, "someone@example.com"); err != nil {
			t.Fatal(err)
		}
		if _, err := k.Bulk(ctx, b.ID, BulkAssign, ids, ""); err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			if c, _ := k.Card(ctx, id); c.Assignee != "" {
				t.Errorf("card %s still assigned to %q", id, c.Assignee)
			}
		}
	})

	t.Run("delete", func(t *testing.T) {
		k, b, ids := setup(t)
		res, err := k.Bulk(ctx, b.ID, BulkDelete, ids[:2], "")
		if err != nil || len(res.Changed) != 2 {
			t.Fatalf("res = %+v, err = %v", res, err)
		}
		cards, _ := k.Cards(ctx, b.ID)
		if len(cards) != 1 {
			t.Errorf("%d cards left, want 1", len(cards))
		}
	})

	t.Run("move", func(t *testing.T) {
		k, b, ids := setup(t)
		doing := b.Columns[1].ID
		if _, err := k.Bulk(ctx, b.ID, BulkMove, ids, string(doing)); err != nil {
			t.Fatal(err)
		}
		cards, _ := k.Cards(ctx, b.ID)
		for _, c := range cards {
			if c.ColumnID != doing {
				t.Errorf("card %s is in %s, want %s", c.ID, c.ColumnID, doing)
			}
		}
	})

	t.Run("a duplicate id is applied once", func(t *testing.T) {
		k, b, ids := setup(t)
		res, err := k.Bulk(ctx, b.ID, BulkDelete, []model.ID{ids[0], ids[0]}, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Changed) != 1 || len(res.Failed) != 0 {
			t.Errorf("res = %+v, want one change and no failure", res)
		}
	})

	t.Run("a card on another board is refused, not applied", func(t *testing.T) {
		k, b, ids := setup(t)
		other, _ := k.CreateBoard(ctx, "Other", "", nil)
		victim, err := k.CreateCard(ctx, other.ID, other.Columns[0].ID, CardInput{Title: "theirs"})
		if err != nil {
			t.Fatal(err)
		}
		res, err := k.Bulk(ctx, b.ID, BulkDelete, []model.ID{ids[0], victim.ID}, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Changed) != 1 {
			t.Errorf("changed = %v, want only the card on this board", res.Changed)
		}
		if _, ok := res.Failed[victim.ID]; !ok {
			t.Error("the other board's card was not reported as failed")
		}
		if _, err := k.Card(ctx, victim.ID); err != nil {
			t.Error("the other board's card was deleted")
		}
	})

	t.Run("one bad id does not stop the rest", func(t *testing.T) {
		k, b, ids := setup(t)
		res, err := k.Bulk(ctx, b.ID, BulkDelete, []model.ID{ids[0], "nope", ids[1]}, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Changed) != 2 || len(res.Failed) != 1 {
			t.Errorf("res = %+v, want two changes and one failure", res)
		}
	})
}

func TestSubtaskCountIsCapped(t *testing.T) {
	k := newSvc(t)
	ctx := context.Background()
	b, _ := k.CreateBoard(ctx, "B", "", nil)

	// Measured before the cap existed: 200 000 subtasks rendered a board page
	// of 39 MB against a 128Mi pod limit, and the card stayed in the database
	// so every later render failed the same way.
	many := make([]model.Subtask, MaxSubtasks+1)
	for i := range many {
		many[i] = model.Subtask{Title: "x"}
	}
	if _, err := k.CreateCard(ctx, b.ID, b.Columns[0].ID,
		CardInput{Title: "poison", Subtasks: many}); !isValidation(err, "subtasks") {
		t.Fatalf("err = %v, want a validation error on subtasks", err)
	}

	// The cap itself is allowed, so it is a limit and not an off-by-one.
	ok := many[:MaxSubtasks]
	if _, err := k.CreateCard(ctx, b.ID, b.Columns[0].ID,
		CardInput{Title: "fine", Subtasks: ok}); err != nil {
		t.Errorf("exactly MaxSubtasks was refused: %v", err)
	}
}

func TestBulkDoesNotEchoTheAction(t *testing.T) {
	k := newSvc(t)
	ctx := context.Background()
	b, _ := k.CreateBoard(ctx, "B", "", nil)
	c, _ := k.CreateCard(ctx, b.ID, b.Columns[0].ID, CardInput{Title: "one"})

	_, err := k.Bulk(ctx, b.ID, BulkAction("<script>alert(1)</script>"), []model.ID{c.ID}, "")
	if err == nil {
		t.Fatal("an unknown action was accepted")
	}
	if strings.Contains(err.Error(), "<script>") {
		t.Errorf("the caller's input is reflected back: %v", err)
	}
}

func TestArchiveAndRestore(t *testing.T) {
	k := newSvc(t)
	ctx := context.Background()
	b, _ := k.CreateBoard(ctx, "B", "", nil)
	todo := b.Columns[0].ID
	a, _ := k.CreateCard(ctx, b.ID, todo, CardInput{Title: "one"})
	keep, _ := k.CreateCard(ctx, b.ID, todo, CardInput{Title: "two"})

	if err := k.ArchiveCard(ctx, a.ID); err != nil {
		t.Fatalf("ArchiveCard: %v", err)
	}
	cards, _ := k.Cards(ctx, b.ID)
	if len(cards) != 1 || cards[0].ID != keep.ID {
		t.Fatalf("board holds %d cards, want only the un-archived one", len(cards))
	}
	arch, err := k.ArchivedCards(ctx, b.ID)
	if err != nil || len(arch) != 1 || arch[0].ID != a.ID {
		t.Fatalf("archive = %+v, err %v", arch, err)
	}

	if err := k.RestoreCard(ctx, a.ID); err != nil {
		t.Fatalf("RestoreCard: %v", err)
	}
	if cards, _ = k.Cards(ctx, b.ID); len(cards) != 2 {
		t.Errorf("board holds %d cards after a restore, want 2", len(cards))
	}
}

func TestRestoreIsNotRefusedByAWIPLimit(t *testing.T) {
	k := newSvc(t)
	ctx := context.Background()
	b, _ := k.CreateBoard(ctx, "B", "", nil)
	todo := b.Columns[0].ID
	a, _ := k.CreateCard(ctx, b.ID, todo, CardInput{Title: "archived"})
	if err := k.ArchiveCard(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	// Fill the column to its limit while the card is away.
	if _, err := k.CreateCard(ctx, b.ID, todo, CardInput{Title: "took the slot"}); err != nil {
		t.Fatal(err)
	}
	if err := k.UpdateColumn(ctx, todo, b.Columns[0].Name, 1); err != nil {
		t.Fatalf("setting the WIP limit: %v", err)
	}

	// Refusing here would strand the card in the archive with no way back
	// except moving something else first.
	if err := k.RestoreCard(ctx, a.ID); err != nil {
		t.Errorf("restore was refused by a WIP limit: %v", err)
	}
	cards, _ := k.Cards(ctx, b.ID)
	if len(cards) != 2 {
		t.Errorf("board holds %d cards, want the restored one back", len(cards))
	}
}

func TestComments(t *testing.T) {
	newCard := func(t *testing.T) (*Kanban, context.Context, model.ID) {
		t.Helper()
		k := newSvc(t)
		ctx := context.Background()
		b, err := k.CreateBoard(ctx, "B", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		c, err := k.CreateCard(ctx, b.ID, b.Columns[0].ID, CardInput{Title: "card"})
		if err != nil {
			t.Fatal(err)
		}
		return k, ctx, c.ID
	}

	t.Run("a comment keeps its author, body and instant", func(t *testing.T) {
		k, ctx, card := newCard(t)
		c, err := k.AddComment(ctx, card, " her@example.com ", "  something\r\nover two lines  ")
		if err != nil {
			t.Fatalf("AddComment: %v", err)
		}
		if c.Author != "her@example.com" {
			t.Errorf("author = %q, want it trimmed", c.Author)
		}
		if c.Body != "something\nover two lines" {
			t.Errorf("body = %q, want it trimmed with normalised line endings", c.Body)
		}
		if !c.CreatedAt.Equal(fixed) {
			t.Errorf("CreatedAt = %v, want the service clock %v", c.CreatedAt, fixed)
		}
	})

	t.Run("an empty or oversized body is refused", func(t *testing.T) {
		k, ctx, card := newCard(t)
		if _, err := k.AddComment(ctx, card, "her@example.com", "   \n  "); !isValidation(err, "body") {
			t.Errorf("a whitespace-only comment = %v, want a validation error on body", err)
		}
		if _, err := k.AddComment(ctx, card, "her@example.com", strings.Repeat("x", MaxComment+1)); !isValidation(err, "body") {
			t.Errorf("an oversized comment = %v, want a validation error on body", err)
		}
	})

	t.Run("only the author may remove one", func(t *testing.T) {
		k, ctx, card := newCard(t)
		c, err := k.AddComment(ctx, card, "Her@Example.com", "hers")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := k.DeleteComment(ctx, c.ID, "him@example.com"); !errors.Is(err, ErrNotAuthor) {
			t.Errorf("someone else removing it = %v, want ErrNotAuthor", err)
		}
		// Addresses are not case-sensitive, and an identity provider is free to
		// hand back a different casing than the one that was stored.
		gone, err := k.DeleteComment(ctx, c.ID, "her@example.COM")
		if err != nil {
			t.Fatalf("the author could not remove their own comment: %v", err)
		}
		if gone.CardID != card {
			t.Errorf("DeleteComment returned card %s, want %s so the caller can redraw it", gone.CardID, card)
		}
		list, err := k.Comments(ctx, card)
		if err != nil || len(list) != 0 {
			t.Errorf("%d comments left (err %v), want none", len(list), err)
		}
	})

	t.Run("with no authentication at all there is one user", func(t *testing.T) {
		k, ctx, card := newCard(t)
		c, err := k.AddComment(ctx, card, "", "nobody signed in")
		if err != nil {
			t.Fatal(err)
		}
		// Not a hole: an empty author can only happen where the deployment has
		// no identity layer, and there the board has exactly one user.
		if _, err := k.DeleteComment(ctx, c.ID, ""); err != nil {
			t.Errorf("removing an anonymous comment without authentication = %v, want it allowed", err)
		}
	})

	t.Run("counts are per card and skip the ones with none", func(t *testing.T) {
		k := newSvc(t)
		ctx := context.Background()
		b, _ := k.CreateBoard(ctx, "B", "", nil)
		loud, _ := k.CreateCard(ctx, b.ID, b.Columns[0].ID, CardInput{Title: "loud"})
		quiet, _ := k.CreateCard(ctx, b.ID, b.Columns[0].ID, CardInput{Title: "quiet"})
		for _, body := range []string{"one", "two"} {
			if _, err := k.AddComment(ctx, loud.ID, "her@example.com", body); err != nil {
				t.Fatal(err)
			}
		}
		counts, err := k.CommentCounts(ctx, b.ID)
		if err != nil {
			t.Fatal(err)
		}
		if counts[loud.ID] != 2 {
			t.Errorf("count for the discussed card = %d, want 2", counts[loud.ID])
		}
		if _, ok := counts[quiet.ID]; ok {
			t.Error("a card with no comments is present in the map; the badge would render a zero")
		}
	})

	t.Run("an unknown card and an unknown comment are misses", func(t *testing.T) {
		k, ctx, _ := newCard(t)
		if _, err := k.AddComment(ctx, "nope", "her@example.com", "hello"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("commenting on an unknown card = %v, want ErrNotFound", err)
		}
		if _, err := k.DeleteComment(ctx, "nope", "her@example.com"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("removing an unknown comment = %v, want ErrNotFound", err)
		}
	})
}

func TestLabelColourIsCheckedBeforeItReachesATemplate(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		want  string
		valid bool
	}{
		{"empty is the default", "", "", true},
		{"six digits", "#3B82F6", "#3b82f6", true},
		{"three digits", "#F00", "#f00", true},
		{"surrounding space", "  #f00  ", "#f00", true},
		{"a css keyword", "red", "", false},
		{"no hash", "3b82f6", "", false},
		{"four digits", "#abcd", "", false},
		{"a url", "javascript:alert(1)", "", false},
		{"an expression", "#f00; background: url(x)", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newSvc(t)
			ctx := context.Background()
			b, _ := k.CreateBoard(ctx, "B", "", nil)
			l, err := k.CreateLabel(ctx, b.ID, "label", tt.in)
			if tt.valid {
				if err != nil {
					t.Fatalf("CreateLabel(%q) = %v, want it accepted", tt.in, err)
				}
				// Anything html/template cannot prove is a colour renders as
				// ZgotmplZ in a style attribute, which reads as a broken label
				// rather than as a rejection. So it is rejected here instead.
				if l.Color != tt.want {
					t.Errorf("colour = %q, want %q", l.Color, tt.want)
				}
				return
			}
			if !isValidation(err, "color") {
				t.Errorf("CreateLabel(%q) = %v, want a validation error on color", tt.in, err)
			}
		})
	}
}

func TestSetCardAssignee(t *testing.T) {
	k := newSvc(t)
	ctx := context.Background()
	b, _ := k.CreateBoard(ctx, "B", "", nil)
	c, err := k.CreateCard(ctx, b.ID, b.Columns[0].ID, CardInput{Title: "T"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := k.SetCardAssignee(ctx, c.ID, "  her@example.com  "); err != nil {
		t.Fatalf("SetCardAssignee: %v", err)
	}
	got, _ := k.Card(ctx, c.ID)
	if got.Assignee != "her@example.com" {
		t.Errorf("assignee = %q, want it trimmed to her@example.com", got.Assignee)
	}
	// Everything else on the card is untouched: a quick edit that reset the
	// title to empty would be worse than no quick edit at all.
	if got.Title != "T" {
		t.Errorf("title = %q, want T", got.Title)
	}

	if _, err := k.SetCardAssignee(ctx, c.ID, ""); err != nil {
		t.Fatalf("unassigning: %v", err)
	}
	if got, _ = k.Card(ctx, c.ID); got.Assignee != "" {
		t.Errorf("assignee = %q after unassigning, want empty", got.Assignee)
	}

	if _, err := k.SetCardAssignee(ctx, c.ID, strings.Repeat("a", MaxAssignee+1)); !isValidation(err, "assignee") {
		t.Errorf("an over-long assignee = %v, want a validation error", err)
	}
	if _, err := k.SetCardAssignee(ctx, "nope", "her@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("assigning an unknown card = %v, want ErrNotFound", err)
	}
}

func TestSetCardAssigneeToTheSamePersonChangesNothing(t *testing.T) {
	k := newSvc(t)
	ctx := context.Background()
	b, _ := k.CreateBoard(ctx, "B", "", nil)
	c, _ := k.CreateCard(ctx, b.ID, b.Columns[0].ID, CardInput{Title: "T", Assignee: "her@example.com"})

	// A clock that moves, so a needless write would show up as a new UpdatedAt.
	later := fixed.Add(time.Hour)
	k.now = func() time.Time { return later }
	if _, err := k.SetCardAssignee(ctx, c.ID, "her@example.com"); err != nil {
		t.Fatal(err)
	}
	got, _ := k.Card(ctx, c.ID)
	if !got.UpdatedAt.Equal(c.UpdatedAt) {
		t.Errorf("UpdatedAt moved to %v for a click that changed nothing", got.UpdatedAt)
	}
}

func TestToggleCardLabel(t *testing.T) {
	k := newSvc(t)
	ctx := context.Background()
	b, _ := k.CreateBoard(ctx, "B", "", nil)
	bug, _ := k.CreateLabel(ctx, b.ID, "bug", "#f00")
	c, _ := k.CreateCard(ctx, b.ID, b.Columns[0].ID, CardInput{Title: "T"})

	if _, err := k.ToggleCardLabel(ctx, c.ID, bug.ID); err != nil {
		t.Fatalf("adding: %v", err)
	}
	if got, _ := k.Card(ctx, c.ID); len(got.Labels) != 1 || got.Labels[0] != bug.ID {
		t.Errorf("labels = %v, want just the bug label", got.Labels)
	}
	if _, err := k.ToggleCardLabel(ctx, c.ID, bug.ID); err != nil {
		t.Fatalf("removing: %v", err)
	}
	if got, _ := k.Card(ctx, c.ID); len(got.Labels) != 0 {
		t.Errorf("labels = %v after the second toggle, want none", got.Labels)
	}

	// A label belongs to a board. Without this check a crafted id would hang
	// another board's label on this card, and the board would then render a
	// card carrying a label it does not have.
	other, _ := k.CreateBoard(ctx, "Other", "", nil)
	theirs, _ := k.CreateLabel(ctx, other.ID, "theirs", "#00f")
	if _, err := k.ToggleCardLabel(ctx, c.ID, theirs.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another board's label = %v, want ErrNotFound", err)
	}
	if got, _ := k.Card(ctx, c.ID); len(got.Labels) != 0 {
		t.Errorf("labels = %v, want the refused toggle to have changed nothing", got.Labels)
	}
	if _, err := k.ToggleCardLabel(ctx, "nope", theirs.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("toggling on an unknown card = %v, want ErrNotFound", err)
	}
}

func TestPeople(t *testing.T) {
	cards := []model.Card{
		{Assignee: "zoe@example.com"},
		{Assignee: ""},
		{Assignee: "  anna@example.com  "},
		{Assignee: "ZOE@example.com"},
		{Assignee: "mike@example.com"},
	}
	tests := []struct {
		name  string
		extra []string
		want  []string
	}{
		{"nobody signed in", nil,
			[]string{"anna@example.com", "mike@example.com", "zoe@example.com"}},
		{"the viewer comes first even out of order", []string{"zoe@example.com"},
			[]string{"zoe@example.com", "anna@example.com", "mike@example.com"}},
		{"a viewer nobody has assigned is still offered", []string{"new@example.com"},
			[]string{"new@example.com", "anna@example.com", "mike@example.com", "zoe@example.com"}},
		{"an anonymous viewer adds nothing", []string{""},
			[]string{"anna@example.com", "mike@example.com", "zoe@example.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := People(cards, tt.extra...)
			if len(got) != len(tt.want) {
				t.Fatalf("People = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("People = %v, want %v", got, tt.want)
				}
			}
		})
	}
}
