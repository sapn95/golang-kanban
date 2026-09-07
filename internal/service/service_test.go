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
	if err := k.RemoveColumn(ctx, c.ID, b.Columns[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := k.RemoveColumn(ctx, c.ID, ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("remove twice: %v", err)
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
