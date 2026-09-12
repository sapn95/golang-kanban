// Package storetest is the contract every store.Store backend must satisfy.
// A backend's own test file calls Run with a constructor that returns an
// empty, migrated store.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"kanban/internal/model"
	"kanban/internal/store"
)

// New returns a fresh, empty store for one subtest.
type New func(t *testing.T) store.Store

// Run executes the contract suite against the backend.
func Run(t *testing.T, newStore New) {
	t.Helper()
	tests := map[string]func(*testing.T, store.Store){
		"Ping":          testPing,
		"Boards":        testBoards,
		"Columns":       testColumns,
		"Cards":         testCards,
		"Assignee":      testAssignee,
		"Archive":       testArchive,
		"Touch":         testTouch,
		"SubtaskDone":   testSubtaskDone,
		"Patch":         testPatch,
		"PatchMatrix":   testPatchMatrix,
		"PatchBoards":   testPatchBoardMatrix,
		"Comments":      testComments,
		"Layout":        testLayout,
		"SLA":           testSLA,
		"ReorderCards":  testReorderCards,
		"Labels":        testLabels,
		"Timestamps":    testTimestamps,
		"MigrateTwice":  testMigrateTwice,
		"ListOrdering":  testListOrdering,
		"DeleteCascade": testDeleteCascade,
	}
	for name, fn := range tests {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			fn(t, s)
		})
	}
}

var seq int

// newID makes an id for a fixture. Prefixed and counted rather than random, so
// a failing assertion names something a person can find in the output.
func newID(prefix string) model.ID {
	seq++
	return model.ID(fmt.Sprintf("%s-%03d", prefix, seq))
}

// now is truncated to microseconds, which is the resolution the coarsest of the
// three backends keeps. Comparing at nanoseconds would fail on Postgres alone.
func now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// mustBoard creates a board with the named columns, failing the test rather
// than returning an error every caller would have to check.
func mustBoard(t *testing.T, s store.Store, slug string, columns ...string) *model.Board {
	t.Helper()
	b := &model.Board{ID: newID("b"), Slug: slug, Name: "Board " + slug, CreatedAt: now(), UpdatedAt: now()}
	for _, name := range columns {
		b.Columns = append(b.Columns, model.Column{ID: newID("c"), Name: name})
	}
	if err := s.CreateBoard(context.Background(), b); err != nil {
		t.Fatalf("CreateBoard(%s): %v", slug, err)
	}
	return b
}

// mustCard creates a card in a column, the same way.
func mustCard(t *testing.T, s store.Store, b *model.Board, col model.ID, title string) *model.Card {
	t.Helper()
	c := &model.Card{ID: newID("k"), BoardID: b.ID, ColumnID: col, Title: title, CreatedAt: now(), UpdatedAt: now()}
	if err := s.CreateCard(context.Background(), c); err != nil {
		t.Fatalf("CreateCard(%s): %v", title, err)
	}
	return c
}

// wantErr asserts which error came back, by identity and not by message: the
// three backends word theirs differently and the contract is the sentinel.
func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: got %v, want %v", what, err, want)
	}
}

// columnCards returns one column's card titles in order, which is what most
// assertions about ordering actually compare.
func columnCards(t *testing.T, s store.Store, b *model.Board, col model.ID) []string {
	t.Helper()
	cards, err := s.ListCards(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	last := 0
	for _, c := range cards {
		if c.ColumnID != col {
			continue
		}
		if c.Position <= last {
			t.Fatalf("positions not ascending in %s: %+v", col, cards)
		}
		last = c.Position
		titles = append(titles, c.Title)
	}
	return titles
}

// equalStrings compares two lists of titles.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// testPing checks the backend answers at all.
func testPing(t *testing.T, s store.Store) {
	if err := s.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// testMigrateTwice checks a second migration is a no-op, which is what lets
// AUTO_MIGRATE run on every start.
func testMigrateTwice(t *testing.T, s store.Store) {
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustBoard(t, s, "twice", "A")
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetBoard(context.Background(), "twice"); err != nil {
		t.Fatalf("data lost after second migrate: %v", err)
	}
}

// testBoards covers creating, reading, renaming and deleting a board, and the
// conflict on a duplicate slug.
func testBoards(t *testing.T, s store.Store) {
	ctx := context.Background()
	b := mustBoard(t, s, "alpha", "To Do", "Doing", "Done")

	got, err := s.GetBoard(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != b.ID || got.Name != b.Name || len(got.Columns) != 3 {
		t.Fatalf("GetBoard = %+v", got)
	}
	for i, c := range got.Columns {
		if c.Position != i+1 || c.BoardID != b.ID || c.Name != b.Columns[i].Name || c.ID != b.Columns[i].ID {
			t.Fatalf("column %d = %+v", i, c)
		}
	}
	byID, err := s.GetBoardByID(ctx, b.ID)
	if err != nil || byID.Slug != "alpha" {
		t.Fatalf("GetBoardByID = %+v, %v", byID, err)
	}

	wantErr(t, "GetBoard unknown", func() error { _, err := s.GetBoard(ctx, "nope"); return err }(), store.ErrNotFound)
	wantErr(t, "GetBoardByID unknown", func() error { _, err := s.GetBoardByID(ctx, "nope"); return err }(), store.ErrNotFound)

	dup := &model.Board{ID: newID("b"), Slug: "alpha", Name: "dup"}
	wantErr(t, "CreateBoard duplicate slug", s.CreateBoard(ctx, dup), store.ErrConflict)
	dupID := &model.Board{ID: b.ID, Slug: "other", Name: "dup"}
	wantErr(t, "CreateBoard duplicate id", s.CreateBoard(ctx, dupID), store.ErrConflict)

	mustBoard(t, s, "beta")
	list, err := s.ListBoards(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Slug != "alpha" || list[1].Slug != "beta" || len(list[0].Columns) != 3 {
		t.Fatalf("ListBoards = %+v", list)
	}

	b.Name = "Renamed"
	b.Slug = "gamma"
	b.UpdatedAt = now()
	if err := s.UpdateBoard(ctx, b); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetBoard(ctx, "gamma")
	if err != nil || got.Name != "Renamed" || !got.UpdatedAt.Equal(b.UpdatedAt) {
		t.Fatalf("after UpdateBoard: %+v, %v", got, err)
	}
	b.Slug = "beta"
	wantErr(t, "UpdateBoard slug conflict", s.UpdateBoard(ctx, b), store.ErrConflict)
	wantErr(t, "UpdateBoard unknown", s.UpdateBoard(ctx, &model.Board{ID: "nope", Slug: "x", Name: "x"}), store.ErrNotFound)

	if err := s.DeleteBoard(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "GetBoard after delete", func() error { _, err := s.GetBoard(ctx, "gamma"); return err }(), store.ErrNotFound)
	wantErr(t, "DeleteBoard unknown", s.DeleteBoard(ctx, b.ID), store.ErrNotFound)
}

// testColumns covers appending, renaming, reordering, and where a deleted
// column's cards go.
func testColumns(t *testing.T, s store.Store) {
	ctx := context.Background()
	b := mustBoard(t, s, "cols", "A", "B", "C")
	other := mustBoard(t, s, "other", "X")

	d := &model.Column{ID: newID("c"), BoardID: b.ID, Name: "D", WIPLimit: 2}
	if err := s.CreateColumn(ctx, d); err != nil {
		t.Fatal(err)
	}
	if d.Position != 4 {
		t.Fatalf("CreateColumn position = %d", d.Position)
	}
	wantErr(t, "CreateColumn unknown board", s.CreateColumn(ctx, &model.Column{ID: newID("c"), BoardID: "nope", Name: "E"}), store.ErrNotFound)

	d.Name = "Dee"
	d.WIPLimit = 5
	if err := s.UpdateColumn(ctx, d); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetBoard(ctx, "cols")
	if c := got.Column(d.ID); c == nil || c.Name != "Dee" || c.WIPLimit != 5 || c.Position != 4 {
		t.Fatalf("after UpdateColumn: %+v", c)
	}
	wantErr(t, "UpdateColumn unknown", s.UpdateColumn(ctx, &model.Column{ID: "nope", Name: "x"}), store.ErrNotFound)

	a, bb, c := b.Columns[0].ID, b.Columns[1].ID, b.Columns[2].ID
	if err := s.ReorderColumns(ctx, b.ID, []model.ID{d.ID, c, bb, a}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetBoard(ctx, "cols")
	if got.Columns[0].ID != d.ID || got.Columns[3].ID != a || got.Columns[0].Position != 1 || got.Columns[3].Position != 4 {
		t.Fatalf("after ReorderColumns: %+v", got.Columns)
	}
	wantErr(t, "ReorderColumns missing one", s.ReorderColumns(ctx, b.ID, []model.ID{a, bb, c}), store.ErrInvalid)
	wantErr(t, "ReorderColumns foreign", s.ReorderColumns(ctx, b.ID, []model.ID{a, bb, c, other.Columns[0].ID}), store.ErrInvalid)
	wantErr(t, "ReorderColumns duplicate", s.ReorderColumns(ctx, b.ID, []model.ID{a, a, bb, c}), store.ErrInvalid)
	wantErr(t, "ReorderColumns unknown board", s.ReorderColumns(ctx, "nope", []model.ID{a}), store.ErrNotFound)

	mustCard(t, s, b, a, "a1")
	mustCard(t, s, b, a, "a2")
	mustCard(t, s, b, bb, "b1")
	wantErr(t, "DeleteColumn move to other board", s.DeleteColumn(ctx, a, other.Columns[0].ID), store.ErrInvalid)
	wantErr(t, "DeleteColumn move to self", s.DeleteColumn(ctx, a, a), store.ErrInvalid)
	if err := s.DeleteColumn(ctx, a, bb); err != nil {
		t.Fatal(err)
	}
	if got := columnCards(t, s, b, bb); !equalStrings(got, []string{"b1", "a1", "a2"}) {
		t.Fatalf("after DeleteColumn move: %v", got)
	}
	if err := s.DeleteColumn(ctx, bb, ""); err != nil {
		t.Fatal(err)
	}
	cards, _ := s.ListCards(ctx, b.ID)
	if len(cards) != 0 {
		t.Fatalf("cards survived DeleteColumn: %+v", cards)
	}
	got, _ = s.GetBoard(ctx, "cols")
	if len(got.Columns) != 2 {
		t.Fatalf("columns after delete: %+v", got.Columns)
	}
	wantErr(t, "DeleteColumn unknown", s.DeleteColumn(ctx, a, ""), store.ErrNotFound)
}

// testCards covers a card's whole life, including that an update never moves it
// between columns.
func testCards(t *testing.T, s store.Store) {
	ctx := context.Background()
	b := mustBoard(t, s, "cards", "A", "B")
	other := mustBoard(t, s, "other", "X")
	a, bb := b.Columns[0].ID, b.Columns[1].ID

	l1 := &model.Label{ID: newID("l"), BoardID: b.ID, Name: "bug", Color: "#f00"}
	l2 := &model.Label{ID: newID("l"), BoardID: b.ID, Name: "idea"}
	for _, l := range []*model.Label{l1, l2} {
		if err := s.CreateLabel(ctx, l); err != nil {
			t.Fatal(err)
		}
	}

	due := time.Date(2026, 12, 24, 0, 0, 0, 0, time.UTC)
	c1 := &model.Card{
		ID: newID("k"), BoardID: b.ID, ColumnID: a, Title: "one", Description: "# md\n\ntext", DueDate: due,
		Labels:    []model.ID{l2.ID, l1.ID},
		Subtasks:  []model.Subtask{{ID: newID("s"), Title: "first"}, {ID: newID("s"), Title: "second", Done: true}},
		CreatedAt: now(), UpdatedAt: now(),
	}
	if err := s.CreateCard(ctx, c1); err != nil {
		t.Fatal(err)
	}
	if c1.Position != 1 {
		t.Fatalf("position = %d", c1.Position)
	}
	c2 := mustCard(t, s, b, a, "two")
	if c2.Position != 2 {
		t.Fatalf("position = %d", c2.Position)
	}
	c3 := mustCard(t, s, b, bb, "three")
	if c3.Position != 1 {
		t.Fatalf("position = %d", c3.Position)
	}

	wantErr(t, "CreateCard unknown column", s.CreateCard(ctx, &model.Card{ID: newID("k"), BoardID: b.ID, ColumnID: "nope", Title: "x"}), store.ErrNotFound)
	wantErr(t, "CreateCard column of other board", s.CreateCard(ctx, &model.Card{ID: newID("k"), BoardID: b.ID, ColumnID: other.Columns[0].ID, Title: "x"}), store.ErrNotFound)
	wantErr(t, "CreateCard unknown label", s.CreateCard(ctx, &model.Card{ID: newID("k"), BoardID: b.ID, ColumnID: a, Title: "x", Labels: []model.ID{"nope"}}), store.ErrNotFound)
	wantErr(t, "CreateCard duplicate id", s.CreateCard(ctx, &model.Card{ID: c1.ID, BoardID: b.ID, ColumnID: a, Title: "x"}), store.ErrConflict)

	got, err := s.GetCard(ctx, c1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "one" || got.Description != c1.Description || !got.DueDate.Equal(due) || got.BoardID != b.ID || got.ColumnID != a {
		t.Fatalf("GetCard = %+v", got)
	}
	if len(got.Labels) != 2 || got.Labels[0] != l1.ID || got.Labels[1] != l2.ID {
		t.Fatalf("labels not sorted: %v", got.Labels)
	}
	if len(got.Subtasks) != 2 || got.Subtasks[0].Title != "first" || got.Subtasks[0].Position != 1 ||
		got.Subtasks[1].Title != "second" || !got.Subtasks[1].Done || got.Subtasks[1].Position != 2 || got.Subtasks[1].ID != c1.Subtasks[1].ID {
		t.Fatalf("subtasks = %+v", got.Subtasks)
	}
	wantErr(t, "GetCard unknown", func() error { _, err := s.GetCard(ctx, "nope"); return err }(), store.ErrNotFound)

	upd := *got
	upd.Title = "uno"
	upd.Description = ""
	upd.DueDate = time.Time{}
	upd.Labels = []model.ID{l2.ID}
	upd.Subtasks = []model.Subtask{{ID: newID("s"), Title: "only"}}
	upd.ColumnID = bb // must be ignored
	upd.Position = 99 // must be ignored
	upd.UpdatedAt = now()
	if err := s.UpdateCard(ctx, &upd); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetCard(ctx, c1.ID)
	if got.Title != "uno" || got.Description != "" || !got.DueDate.IsZero() || got.ColumnID != a || got.Position != 1 || !got.UpdatedAt.Equal(upd.UpdatedAt) {
		t.Fatalf("after UpdateCard = %+v", got)
	}
	if len(got.Labels) != 1 || got.Labels[0] != l2.ID || len(got.Subtasks) != 1 || got.Subtasks[0].Title != "only" || got.Subtasks[0].Position != 1 {
		t.Fatalf("after UpdateCard labels/subtasks = %+v %+v", got.Labels, got.Subtasks)
	}
	wantErr(t, "UpdateCard unknown", s.UpdateCard(ctx, &model.Card{ID: "nope", Title: "x"}), store.ErrNotFound)
	upd.Labels = []model.ID{"nope"}
	wantErr(t, "UpdateCard unknown label", s.UpdateCard(ctx, &upd), store.ErrNotFound)

	if err := s.DeleteCard(ctx, c2.ID); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "DeleteCard unknown", s.DeleteCard(ctx, c2.ID), store.ErrNotFound)
	cards, _ := s.ListCards(ctx, b.ID)
	if len(cards) != 2 {
		t.Fatalf("ListCards = %+v", cards)
	}
	cards, _ = s.ListCards(ctx, "nope")
	if len(cards) != 0 {
		t.Fatalf("ListCards unknown board = %+v", cards)
	}
}

// testReorderCards covers ordering inside a column and moving a card in from
// another one.
func testReorderCards(t *testing.T, s store.Store) {
	ctx := context.Background()
	b := mustBoard(t, s, "reorder", "A", "B")
	other := mustBoard(t, s, "other", "X")
	a, bb := b.Columns[0].ID, b.Columns[1].ID
	a1 := mustCard(t, s, b, a, "a1")
	a2 := mustCard(t, s, b, a, "a2")
	a3 := mustCard(t, s, b, a, "a3")
	b1 := mustCard(t, s, b, bb, "b1")
	x1 := mustCard(t, s, other, other.Columns[0].ID, "x1")

	if err := s.ReorderCards(ctx, b.ID, a, []model.ID{a3.ID, a1.ID, a2.ID}); err != nil {
		t.Fatal(err)
	}
	if got := columnCards(t, s, b, a); !equalStrings(got, []string{"a3", "a1", "a2"}) {
		t.Fatalf("same-column reorder: %v", got)
	}

	// Move a1 into B before b1, with one request.
	if err := s.ReorderCards(ctx, b.ID, bb, []model.ID{a1.ID, b1.ID}); err != nil {
		t.Fatal(err)
	}
	if got := columnCards(t, s, b, bb); !equalStrings(got, []string{"a1", "b1"}) {
		t.Fatalf("cross-column move: %v", got)
	}
	if got := columnCards(t, s, b, a); !equalStrings(got, []string{"a3", "a2"}) {
		t.Fatalf("origin after move: %v", got)
	}
	if c, _ := s.GetCard(ctx, a1.ID); c.ColumnID != bb {
		t.Fatalf("moved card column = %s", c.ColumnID)
	}

	// Unlisted cards of the column go after the listed ones, in their order.
	if err := s.ReorderCards(ctx, b.ID, a, []model.ID{a2.ID}); err != nil {
		t.Fatal(err)
	}
	if got := columnCards(t, s, b, a); !equalStrings(got, []string{"a2", "a3"}) {
		t.Fatalf("partial reorder: %v", got)
	}
	if err := s.ReorderCards(ctx, b.ID, a, nil); err != nil {
		t.Fatal(err)
	}
	if got := columnCards(t, s, b, a); !equalStrings(got, []string{"a2", "a3"}) {
		t.Fatalf("empty reorder: %v", got)
	}

	wantErr(t, "unknown column", s.ReorderCards(ctx, b.ID, "nope", []model.ID{a2.ID}), store.ErrNotFound)
	wantErr(t, "column of other board", s.ReorderCards(ctx, b.ID, other.Columns[0].ID, []model.ID{a2.ID}), store.ErrNotFound)
	wantErr(t, "card of other board", s.ReorderCards(ctx, b.ID, a, []model.ID{x1.ID}), store.ErrNotFound)
	wantErr(t, "unknown card", s.ReorderCards(ctx, b.ID, a, []model.ID{"nope"}), store.ErrNotFound)
	wantErr(t, "duplicate", s.ReorderCards(ctx, b.ID, a, []model.ID{a2.ID, a2.ID}), store.ErrInvalid)
	if got := columnCards(t, s, b, a); !equalStrings(got, []string{"a2", "a3"}) {
		t.Fatalf("failed reorders must not change anything: %v", got)
	}
}

// testLabels covers creating, renaming and deleting a label, and that deleting
// one takes it off every card carrying it.
func testLabels(t *testing.T, s store.Store) {
	ctx := context.Background()
	b := mustBoard(t, s, "labels", "A")
	l := &model.Label{ID: newID("l"), BoardID: b.ID, Name: "zeta", Color: "#00f"}
	if err := s.CreateLabel(ctx, l); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateLabel(ctx, &model.Label{ID: newID("l"), BoardID: b.ID, Name: "alpha"}); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "duplicate name", s.CreateLabel(ctx, &model.Label{ID: newID("l"), BoardID: b.ID, Name: "zeta"}), store.ErrConflict)
	wantErr(t, "unknown board", s.CreateLabel(ctx, &model.Label{ID: newID("l"), BoardID: "nope", Name: "q"}), store.ErrNotFound)

	got, _ := s.GetBoard(ctx, "labels")
	if len(got.Labels) != 2 || got.Labels[0].Name != "alpha" || got.Labels[1].Name != "zeta" || got.Labels[1].Color != "#00f" {
		t.Fatalf("labels = %+v", got.Labels)
	}

	c := &model.Card{ID: newID("k"), BoardID: b.ID, ColumnID: b.Columns[0].ID, Title: "t", Labels: []model.ID{l.ID}}
	if err := s.CreateCard(ctx, c); err != nil {
		t.Fatal(err)
	}
	l.Name = "eta"
	l.Color = ""
	if err := s.UpdateLabel(ctx, l); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetBoard(ctx, "labels")
	if lb := got.Label(l.ID); lb == nil || lb.Name != "eta" || lb.Color != "" {
		t.Fatalf("after UpdateLabel = %+v", lb)
	}
	l.Name = "alpha"
	wantErr(t, "UpdateLabel conflict", s.UpdateLabel(ctx, l), store.ErrConflict)
	wantErr(t, "UpdateLabel unknown", s.UpdateLabel(ctx, &model.Label{ID: "nope", Name: "x"}), store.ErrNotFound)

	if err := s.DeleteLabel(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	card, _ := s.GetCard(ctx, c.ID)
	if len(card.Labels) != 0 {
		t.Fatalf("label still on card: %v", card.Labels)
	}
	wantErr(t, "DeleteLabel unknown", s.DeleteLabel(ctx, l.ID), store.ErrNotFound)
}

// testTimestamps covers what each write stamps. TouchCard has its own test,
// because what it must NOT stamp is the point of it.
func testTimestamps(t *testing.T, s store.Store) {
	ctx := context.Background()
	created := time.Date(2025, 1, 2, 3, 4, 5, 123456000, time.UTC)
	b := &model.Board{ID: newID("b"), Slug: "ts", Name: "ts", CreatedAt: created, UpdatedAt: created}
	b.Columns = []model.Column{{ID: newID("c"), Name: "A"}}
	if err := s.CreateBoard(ctx, b); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetBoard(ctx, "ts")
	if !got.CreatedAt.Equal(created) || !got.UpdatedAt.Equal(created) {
		t.Fatalf("board timestamps = %v %v", got.CreatedAt, got.UpdatedAt)
	}
	c := &model.Card{ID: newID("k"), BoardID: b.ID, ColumnID: b.Columns[0].ID, Title: "t", CreatedAt: created, UpdatedAt: created}
	if err := s.CreateCard(ctx, c); err != nil {
		t.Fatal(err)
	}
	card, _ := s.GetCard(ctx, c.ID)
	if !card.CreatedAt.Equal(created) || !card.UpdatedAt.Equal(created) || !card.DueDate.IsZero() {
		t.Fatalf("card timestamps = %+v", card)
	}
	cards, _ := s.ListCards(ctx, b.ID)
	if !cards[0].CreatedAt.Equal(created) {
		t.Fatalf("ListCards timestamps = %+v", cards[0])
	}
}

// testListOrdering covers the order ListCards comes back in, which pages rely
// on and no page sorts again.
func testListOrdering(t *testing.T, s store.Store) {
	ctx := context.Background()
	b := mustBoard(t, s, "order", "A", "B")
	a, bb := b.Columns[0].ID, b.Columns[1].ID
	mustCard(t, s, b, bb, "b1")
	mustCard(t, s, b, a, "a1")
	mustCard(t, s, b, bb, "b2")
	mustCard(t, s, b, a, "a2")
	if err := s.ReorderColumns(ctx, b.ID, []model.ID{bb, a}); err != nil {
		t.Fatal(err)
	}
	cards, err := s.ListCards(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, c := range cards {
		titles = append(titles, c.Title)
	}
	if !equalStrings(titles, []string{"b1", "b2", "a1", "a2"}) {
		t.Fatalf("ListCards order = %v", titles)
	}
}

// testDeleteCascade covers what goes with a deleted board, so no backend leaves
// an orphan the others clean up.
func testDeleteCascade(t *testing.T, s store.Store) {
	ctx := context.Background()
	b := mustBoard(t, s, "cascade", "A")
	l := &model.Label{ID: newID("l"), BoardID: b.ID, Name: "x"}
	if err := s.CreateLabel(ctx, l); err != nil {
		t.Fatal(err)
	}
	c := &model.Card{ID: newID("k"), BoardID: b.ID, ColumnID: b.Columns[0].ID, Title: "t", Labels: []model.ID{l.ID},
		Subtasks: []model.Subtask{{ID: newID("s"), Title: "s"}}}
	if err := s.CreateCard(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBoard(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "card after board delete", func() error { _, err := s.GetCard(ctx, c.ID); return err }(), store.ErrNotFound)
	wantErr(t, "column after board delete", s.UpdateColumn(ctx, &b.Columns[0]), store.ErrNotFound)
	wantErr(t, "label after board delete", s.UpdateLabel(ctx, l), store.ErrNotFound)
	// The slug is free again.
	mustBoard(t, s, "cascade", "A")
}

// testAssignee covers setting and clearing an assignee.
func testAssignee(t *testing.T, s store.Store) {
	ctx := context.Background()
	b := mustBoard(t, s, "assignee", "A")
	col := b.Columns[0].ID

	c := &model.Card{ID: newID("k"), BoardID: b.ID, ColumnID: col, Title: "assigned",
		Assignee: "someone@example.com", CreatedAt: now(), UpdatedAt: now()}
	if err := s.CreateCard(ctx, c); err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	got, err := s.GetCard(ctx, c.ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if got.Assignee != "someone@example.com" {
		t.Errorf("Assignee after create = %q, want someone@example.com", got.Assignee)
	}

	got.Assignee = "other@example.com"
	got.UpdatedAt = now()
	if err := s.UpdateCard(ctx, got); err != nil {
		t.Fatalf("UpdateCard: %v", err)
	}
	if got, err = s.GetCard(ctx, c.ID); err != nil || got.Assignee != "other@example.com" {
		t.Fatalf("Assignee after reassign = %q (err %v), want other@example.com", got.Assignee, err)
	}

	// Unassigning goes through the same path, so an empty string has to mean
	// "nobody" and not "leave it as it was".
	got.Assignee = ""
	got.UpdatedAt = now()
	if err := s.UpdateCard(ctx, got); err != nil {
		t.Fatalf("UpdateCard unassign: %v", err)
	}
	if got, err = s.GetCard(ctx, c.ID); err != nil || got.Assignee != "" {
		t.Fatalf("Assignee after unassign = %q (err %v), want empty", got.Assignee, err)
	}

	// ListCards is a different query from GetCard, and the board renders from
	// that one, so a column missed there would be invisible to GetCard tests.
	got.Assignee = "someone@example.com"
	got.UpdatedAt = now()
	if err := s.UpdateCard(ctx, got); err != nil {
		t.Fatalf("UpdateCard: %v", err)
	}
	cards, err := s.ListCards(ctx, b.ID)
	if err != nil {
		t.Fatalf("ListCards: %v", err)
	}
	found := false
	for _, lc := range cards {
		if lc.ID == c.ID {
			found = true
			if lc.Assignee != "someone@example.com" {
				t.Errorf("Assignee from ListCards = %q, want someone@example.com", lc.Assignee)
			}
		}
	}
	if !found {
		t.Error("the card did not come back from ListCards")
	}
}

// mustComment posts a comment at a given time, for the ordering assertions.
func mustComment(t *testing.T, s store.Store, card model.ID, author, body string, at time.Time) *model.Comment {
	t.Helper()
	c := &model.Comment{ID: newID("m"), CardID: card, Author: author, Body: body, CreatedAt: at}
	if err := s.CreateComment(context.Background(), c); err != nil {
		t.Fatalf("CreateComment(%q): %v", body, err)
	}
	return c
}

// testComments covers posting, listing, counting and deleting comments.
func testComments(t *testing.T, s store.Store) {
	ctx := context.Background()
	b := mustBoard(t, s, "comments", "A")
	card := mustCard(t, s, b, b.Columns[0].ID, "discussed")
	quiet := mustCard(t, s, b, b.Columns[0].ID, "not discussed")

	base := now()
	first := mustComment(t, s, card.ID, "her@example.com", "first", base)
	second := mustComment(t, s, card.ID, "him@example.com", "second", base.Add(time.Minute))
	mustComment(t, s, quiet.ID, "", "on the other card", base)

	got, err := s.ListComments(ctx, card.ID)
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListComments returned %d, want the 2 on this card", len(got))
	}
	// Oldest first, because a thread read out of order is not a thread.
	if got[0].ID != first.ID || got[1].ID != second.ID {
		t.Errorf("order = %s, %s; want %s, %s", got[0].ID, got[1].ID, first.ID, second.ID)
	}
	if got[0].Author != "her@example.com" || got[0].Body != "first" {
		t.Errorf("first comment = %+v, want the author and body it was written with", got[0])
	}
	if !got[0].CreatedAt.Equal(base) {
		t.Errorf("CreatedAt = %v, want %v; a timestamp that does not survive a round trip cannot order a thread", got[0].CreatedAt, base)
	}

	one, err := s.GetComment(ctx, second.ID)
	if err != nil {
		t.Fatalf("GetComment: %v", err)
	}
	if one.Author != "him@example.com" {
		t.Errorf("GetComment author = %q, want him@example.com; the delete check reads it from here", one.Author)
	}

	counts, err := s.CountComments(ctx, b.ID)
	if err != nil {
		t.Fatalf("CountComments: %v", err)
	}
	if counts[card.ID] != 2 || counts[quiet.ID] != 1 {
		t.Errorf("counts = %v, want 2 on %s and 1 on %s", counts, card.ID, quiet.ID)
	}

	if err := s.DeleteComment(ctx, first.ID); err != nil {
		t.Fatalf("DeleteComment: %v", err)
	}
	if got, err = s.ListComments(ctx, card.ID); err != nil || len(got) != 1 {
		t.Errorf("after a delete the card has %d comments (err %v), want 1", len(got), err)
	}

	// Deleting the card takes its comments with it. The SQL backends get this
	// from ON DELETE CASCADE, so the contract is what proves the memory one
	// does the same thing.
	if err := s.DeleteCard(ctx, card.ID); err != nil {
		t.Fatalf("DeleteCard: %v", err)
	}
	if got, err = s.ListComments(ctx, card.ID); err != nil || len(got) != 0 {
		t.Errorf("a deleted card still has %d comments (err %v), want none", len(got), err)
	}
	if counts, err = s.CountComments(ctx, b.ID); err != nil || counts[card.ID] != 0 {
		t.Errorf("counts still hold %d for the deleted card (err %v)", counts[card.ID], err)
	}

	// Misses are misses, not silent successes.
	if _, err := s.GetComment(ctx, model.ID("nope")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetComment on an unknown id = %v, want ErrNotFound", err)
	}
	if err := s.DeleteComment(ctx, model.ID("nope")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteComment on an unknown id = %v, want ErrNotFound", err)
	}
	if err := s.CreateComment(ctx, &model.Comment{ID: newID("m"), CardID: "nope", Body: "orphan", CreatedAt: now()}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("commenting on an unknown card = %v, want ErrNotFound", err)
	}
}

// testArchive covers taking a card off a board and putting it back where it was.
func testArchive(t *testing.T, s store.Store) {
	ctx := context.Background()
	b := mustBoard(t, s, "archive", "A", "B")
	keep := mustCard(t, s, b, b.Columns[0].ID, "stays")
	gone := mustCard(t, s, b, b.Columns[0].ID, "archived")

	// Archiving keeps everything else about the card, which is the whole
	// difference between this and deleting.
	gone.Labels = nil
	when := now()
	if err := s.SetCardArchived(ctx, gone.ID, when); err != nil {
		t.Fatalf("SetCardArchived: %v", err)
	}

	active, err := s.ListCards(ctx, b.ID)
	if err != nil {
		t.Fatalf("ListCards: %v", err)
	}
	if len(active) != 1 || active[0].ID != keep.ID {
		t.Fatalf("board shows %d cards, want only %s", len(active), keep.ID)
	}

	archived, err := s.ListArchivedCards(ctx, b.ID)
	if err != nil {
		t.Fatalf("ListArchivedCards: %v", err)
	}
	if len(archived) != 1 || archived[0].ID != gone.ID {
		t.Fatalf("archive holds %d cards, want only %s", len(archived), gone.ID)
	}
	if !archived[0].Archived() {
		t.Error("a card from the archive does not report itself archived")
	}
	if archived[0].ColumnID != gone.ColumnID {
		t.Errorf("column = %s, want it kept at %s so restoring puts it back", archived[0].ColumnID, gone.ColumnID)
	}
	if archived[0].Title != "archived" {
		t.Errorf("title = %q, want it unchanged", archived[0].Title)
	}

	// GetCard still finds it: an archived card is not gone, and a link to it
	// from somewhere else must not 404.
	got, err := s.GetCard(ctx, gone.ID)
	if err != nil {
		t.Fatalf("GetCard on an archived card: %v", err)
	}
	if !got.Archived() {
		t.Error("GetCard returned it as un-archived")
	}

	// And back again. Restoring is the same call with a zero time.
	if err := s.SetCardArchived(ctx, gone.ID, time.Time{}); err != nil {
		t.Fatalf("restoring: %v", err)
	}
	active, err = s.ListCards(ctx, b.ID)
	if err != nil {
		t.Fatalf("ListCards after restore: %v", err)
	}
	if len(active) != 2 {
		t.Errorf("board shows %d cards after a restore, want 2", len(active))
	}
	if archived, err = s.ListArchivedCards(ctx, b.ID); err != nil || len(archived) != 0 {
		t.Errorf("archive holds %d cards after a restore (err %v), want 0", len(archived), err)
	}

	// A card that does not exist is a miss, not a silent success.
	if err := s.SetCardArchived(ctx, model.ID("nope"), now()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("archiving an unknown card = %v, want ErrNotFound", err)
	}

	// CreateCard puts a card on the board whatever ArchivedAt says, because
	// SetCardArchived is the one way off it. Restoring a backup leans on that:
	// it creates the card and then archives it, and it has to mean the same
	// thing on every backend.
	born := &model.Card{
		ID: newID("k"), BoardID: b.ID, ColumnID: b.Columns[0].ID, Title: "born archived",
		ArchivedAt: now(), CreatedAt: now(), UpdatedAt: now(),
	}
	if err := s.CreateCard(ctx, born); err != nil {
		t.Fatalf("CreateCard with ArchivedAt set: %v", err)
	}
	got, err = s.GetCard(ctx, born.ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if got.Archived() {
		t.Error("CreateCard honoured ArchivedAt; it should leave archiving to SetCardArchived")
	}
}

// testTouch is the one card write that changes nothing but the timestamp. A
// comment restarts the response-time clock with it, and it has to do that
// without carrying a copy of the card through the call, or two people working
// on one card at the same time would each undo the other.
func testTouch(t *testing.T, s store.Store) {
	ctx := context.Background()
	b := mustBoard(t, s, "touch", "A")
	c := mustCard(t, s, b, b.Columns[0].ID, "before")

	// What somebody saved while the comment was on its way.
	c.Title, c.Description, c.UpdatedAt = "renamed", "edited", now()
	if err := s.UpdateCard(ctx, c); err != nil {
		t.Fatalf("UpdateCard: %v", err)
	}

	when := now().Add(time.Minute)
	if err := s.TouchCard(ctx, c.ID, when); err != nil {
		t.Fatalf("TouchCard: %v", err)
	}
	got, err := s.GetCard(ctx, c.ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if !got.UpdatedAt.Equal(when) {
		t.Errorf("UpdatedAt = %s, want %s", got.UpdatedAt, when)
	}
	if got.Title != "renamed" || got.Description != "edited" {
		t.Errorf("the touch also wrote %q / %q, want the edit left alone", got.Title, got.Description)
	}
	if !got.CreatedAt.Equal(c.CreatedAt) {
		t.Errorf("CreatedAt = %s, want it unchanged at %s", got.CreatedAt, c.CreatedAt)
	}

	// A card that is not there is a miss, not a silent success.
	if err := s.TouchCard(ctx, model.ID("nope"), when); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("touching an unknown card = %v, want ErrNotFound", err)
	}
}

// testSLA is the response-time promise through every query that returns a
// board, plus the column flag that goes with it.
//
// The backends do not agree by accident here: the SQL ones have column defaults
// and CHECK constraints, the memory one has neither, and what makes them agree
// is that all three write model.SLA.Clean(). So a board written with no promise
// has to read back as no promise everywhere, and a promise out of range has to
// be brought into range rather than refused.
func testSLA(t *testing.T, s store.Store) {
	ctx := context.Background()
	b := mustBoard(t, s, "sla", "To Do", "Done")

	fresh, err := s.GetBoard(ctx, "sla")
	if err != nil {
		t.Fatalf("GetBoard: %v", err)
	}
	if fresh.SLA != (model.SLA{}) {
		t.Errorf("a board written with no promise came back with %+v", fresh.SLA)
	}
	if fresh.SLA.Enabled() {
		t.Error("a board nobody configured says it makes a promise")
	}

	desk := model.SLA{
		ResponseHours: 4, Days: model.MonToFri,
		Start: 8 * 60, End: 17 * 60, Zone: "Europe/Zurich",
	}
	fresh.SLA = desk
	fresh.UpdatedAt = now()
	if err := s.UpdateBoard(ctx, fresh); err != nil {
		t.Fatalf("UpdateBoard: %v", err)
	}

	// The three queries that return a board are three different statements.
	byID, err := s.GetBoardByID(ctx, b.ID)
	if err != nil {
		t.Fatalf("GetBoardByID: %v", err)
	}
	if byID.SLA != desk {
		t.Errorf("GetBoardByID returned %+v, want %+v", byID.SLA, desk)
	}
	bySlug, err := s.GetBoard(ctx, "sla")
	if err != nil {
		t.Fatalf("GetBoard: %v", err)
	}
	if bySlug.SLA != desk {
		t.Errorf("GetBoard returned %+v, want %+v", bySlug.SLA, desk)
	}
	boards, err := s.ListBoards(ctx)
	if err != nil {
		t.Fatalf("ListBoards: %v", err)
	}
	for _, l := range boards {
		if l.ID == b.ID && l.SLA != desk {
			t.Errorf("ListBoards returned %+v, want %+v", l.SLA, desk)
		}
	}

	// Out of range on the way in, in range on the way out. A backend that wrote
	// this straight through would fail a CHECK constraint instead.
	bySlug.SLA = model.SLA{ResponseHours: -1, Days: 255, Start: -30, End: model.MinutesPerDay + 30}
	bySlug.UpdatedAt = now()
	if err := s.UpdateBoard(ctx, bySlug); err != nil {
		t.Fatalf("UpdateBoard out of range: %v", err)
	}
	cleaned, err := s.GetBoardByID(ctx, b.ID)
	if err != nil {
		t.Fatalf("GetBoardByID: %v", err)
	}
	want := model.SLA{ResponseHours: 0, Days: model.AllDays, Start: 0, End: model.MinutesPerDay}
	if cleaned.SLA != want {
		t.Errorf("an out-of-range promise came back as %+v, want %+v", cleaned.SLA, want)
	}

	// The column flag: written by UpdateColumn, read by every board query.
	done := cleaned.Columns[1]
	done.StopsClock = true
	if err := s.UpdateColumn(ctx, &done); err != nil {
		t.Fatalf("UpdateColumn: %v", err)
	}
	after, err := s.GetBoard(ctx, "sla")
	if err != nil {
		t.Fatalf("GetBoard: %v", err)
	}
	if after.Columns[0].StopsClock {
		t.Error("the clock stopped in a column nobody marked")
	}
	if !after.Columns[1].StopsClock {
		t.Error("stops_clock did not survive UpdateColumn")
	}
	// CreateColumn is the other way in, and a column created with StopsClock keeps it.
	added := &model.Column{ID: newID("c"), BoardID: b.ID, Name: "Waiting", StopsClock: true}
	if err := s.CreateColumn(ctx, added); err != nil {
		t.Fatalf("CreateColumn: %v", err)
	}
	listed, err := s.ListBoards(ctx)
	if err != nil {
		t.Fatalf("ListBoards: %v", err)
	}
	for _, l := range listed {
		if l.ID != b.ID {
			continue
		}
		if len(l.Columns) != 3 {
			t.Fatalf("the board has %d columns, want 3", len(l.Columns))
		}
		if !l.Columns[2].StopsClock {
			t.Error("ListBoards lost stops_clock on the column that was created with it")
		}
	}
}

// testLayout covers storing a board's layout. The response time is testSLA.
func testLayout(t *testing.T, s store.Store) {
	ctx := context.Background()
	b := mustBoard(t, s, "layout", "A")

	// A board written without a layout comes back with the default rather
	// than with an empty string the front-end would have to interpret.
	got, err := s.GetBoard(ctx, "layout")
	if err != nil {
		t.Fatalf("GetBoard: %v", err)
	}
	if got.Layout != model.LayoutColumns {
		t.Errorf("layout = %q on a fresh board, want %q", got.Layout, model.LayoutColumns)
	}

	got.Layout = model.LayoutRows
	got.UpdatedAt = now()
	if err := s.UpdateBoard(ctx, got); err != nil {
		t.Fatalf("UpdateBoard: %v", err)
	}
	again, err := s.GetBoardByID(ctx, b.ID)
	if err != nil {
		t.Fatalf("GetBoardByID: %v", err)
	}
	if again.Layout != model.LayoutRows {
		t.Errorf("layout = %q after an update, want %q", again.Layout, model.LayoutRows)
	}

	// And it survives a listing, which is a different query.
	boards, err := s.ListBoards(ctx)
	if err != nil {
		t.Fatalf("ListBoards: %v", err)
	}
	for _, l := range boards {
		if l.ID == b.ID && l.Layout != model.LayoutRows {
			t.Errorf("ListBoards returned layout %q, want %q", l.Layout, model.LayoutRows)
		}
	}
}

// testSubtaskDone is the other single-field write, and the reason it exists:
// two people ticking two different lines of one checklist must not undo each
// other. Both reads happen before either write, which is the race a read of the
// card plus a write of the whole card loses.
func testSubtaskDone(t *testing.T, s store.Store) {
	ctx := context.Background()
	b := mustBoard(t, s, "subtasks", "To Do")
	c := &model.Card{
		ID: newID("card"), BoardID: b.ID, ColumnID: b.Columns[0].ID, Title: "two lines",
		Subtasks: []model.Subtask{
			{ID: newID("sub"), Title: "first", Position: 1},
			{ID: newID("sub"), Title: "second", Position: 2},
		},
		CreatedAt: now(), UpdatedAt: now(),
	}
	if err := s.CreateCard(ctx, c); err != nil {
		t.Fatal(err)
	}
	first, second := c.Subtasks[0].ID, c.Subtasks[1].ID

	// Both sides read the card before either writes, which is what two people
	// on two phones actually do.
	if _, err := s.GetCard(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCard(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	at := now().Add(time.Minute)
	if err := s.SetSubtaskDone(ctx, c.ID, first, true, at); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSubtaskDone(ctx, c.ID, second, true, at); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetCard(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range got.Subtasks {
		if !st.Done {
			t.Errorf("%q came back unticked; the second write put the first one back", st.Title)
		}
	}
	if got.Title != "two lines" {
		t.Errorf("Title = %q; a tick rewrote the rest of the card", got.Title)
	}
	if !got.UpdatedAt.Equal(at) {
		t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, at)
	}

	// Unticking is the same call with the other value.
	if err := s.SetSubtaskDone(ctx, c.ID, first, false, at); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetCard(ctx, c.ID)
	if got.Subtasks[0].Done {
		t.Error("unticking did nothing")
	}

	// A subtask of another card, or none at all, is not found rather than a
	// write that silently matches nothing.
	wantErr(t, "a subtask that does not exist",
		s.SetSubtaskDone(ctx, c.ID, newID("sub"), true, at), store.ErrNotFound)
	wantErr(t, "a card that does not exist",
		s.SetSubtaskDone(ctx, newID("card"), first, true, at), store.ErrNotFound)
}

// testPatch is the other half of what SetSubtaskDone exists for: a quick edit
// changes one thing, so it must write one thing. The quick edits on the card
// face are the assignee, the due date and a label, and each of them used to
// read the whole card and write it back.
func testPatch(t *testing.T, s store.Store) {
	ctx := context.Background()
	b := mustBoard(t, s, "patch", "To Do")
	lab := &model.Label{ID: newID("label"), BoardID: b.ID, Name: "bug", Color: "#ef4444"}
	if err := s.CreateLabel(ctx, lab); err != nil {
		t.Fatal(err)
	}
	c := &model.Card{
		ID: newID("card"), BoardID: b.ID, ColumnID: b.Columns[0].ID,
		Title: "as written", Description: "the description",
		Subtasks:  []model.Subtask{{ID: newID("sub"), Title: "a line", Position: 1}},
		CreatedAt: now(), UpdatedAt: now(),
	}
	if err := s.CreateCard(ctx, c); err != nil {
		t.Fatal(err)
	}

	// Somebody else is holding the card as it was before any of this.
	stale, err := s.GetCard(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Meanwhile the card is renamed and its line is ticked.
	at := now().Add(time.Minute)
	stale.Title = "renamed by somebody else"
	renamed := *stale
	renamed.Title = "renamed by somebody else"
	renamed.UpdatedAt = at
	if err := s.UpdateCard(ctx, &renamed); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSubtaskDone(ctx, c.ID, c.Subtasks[0].ID, true, at); err != nil {
		t.Fatal(err)
	}

	// Now the quick edits land, each naming its own field.
	who := "somebody@example.com"
	due := time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC)
	labels := []model.ID{lab.ID}
	later := at.Add(time.Minute)
	for _, p := range []store.CardPatch{
		{Assignee: &who}, {DueDate: &due}, {Labels: &labels},
	} {
		if err := s.PatchCard(ctx, c.ID, p, later); err != nil {
			t.Fatalf("%+v: %v", p, err)
		}
	}

	got, err := s.GetCard(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "renamed by somebody else" {
		t.Errorf("Title = %q; a quick edit put the old one back", got.Title)
	}
	if len(got.Subtasks) != 1 || !got.Subtasks[0].Done {
		t.Errorf("subtasks = %+v; a quick edit unticked a line", got.Subtasks)
	}
	if got.Description != "the description" {
		t.Errorf("Description = %q", got.Description)
	}
	if got.Assignee != who || !got.DueDate.Equal(due) || len(got.Labels) != 1 {
		t.Errorf("the quick edits did not land: %+v", got)
	}
	if !got.UpdatedAt.Equal(later) {
		t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, later)
	}

	// An empty value is a value: it is how a card is unassigned and how a due
	// date is taken off.
	none, zero := "", time.Time{}
	if err := s.PatchCard(ctx, c.ID, store.CardPatch{Assignee: &none, DueDate: &zero}, later); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetCard(ctx, c.ID); got.Assignee != "" || !got.DueDate.IsZero() {
		t.Errorf("clearing did nothing: assignee %q due %v", got.Assignee, got.DueDate)
	}

	wantErr(t, "a card that is not there", s.PatchCard(ctx, newID("card"), store.CardPatch{Assignee: &who}, later), store.ErrNotFound)

	// And the same for a board: a layout switch must not put back a rename.
	name := "Renamed board"
	if err := s.PatchBoard(ctx, b.ID, store.BoardPatch{Name: &name}, later); err != nil {
		t.Fatal(err)
	}
	rows := model.LayoutRows
	if err := s.PatchBoard(ctx, b.ID, store.BoardPatch{Layout: &rows}, later); err != nil {
		t.Fatal(err)
	}
	gotBoard, err := s.GetBoardByID(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotBoard.Name != name {
		t.Errorf("Name = %q; the layout switch put the old name back", gotBoard.Name)
	}
	if gotBoard.Layout != model.LayoutRows {
		t.Errorf("Layout = %q", gotBoard.Layout)
	}
	if len(gotBoard.Columns) != 1 {
		t.Errorf("%d columns after two patches, want 1", len(gotBoard.Columns))
	}
	wantErr(t, "a board that is not there",
		s.PatchBoard(ctx, newID("board"), store.BoardPatch{Name: &name}, later), store.ErrNotFound)
}

// testPatchMatrix walks every combination of set and unset fields, because the
// SQL backends build their SET clause by concatenation and the argument order
// is only right if every combination puts the placeholders in the same order as
// the values.
func testPatchMatrix(t *testing.T, s store.Store) {
	ctx := context.Background()
	b := mustBoard(t, s, "matrix", "To Do")
	l1 := &model.Label{ID: newID("label"), BoardID: b.ID, Name: "one", Color: "#ef4444"}
	l2 := &model.Label{ID: newID("label"), BoardID: b.ID, Name: "two", Color: "#22c55e"}
	for _, l := range []*model.Label{l1, l2} {
		if err := s.CreateLabel(ctx, l); err != nil {
			t.Fatal(err)
		}
	}

	who := "person@example.com"
	due := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	both := []model.ID{l1.ID, l2.ID}
	none := []model.ID{}

	for _, tc := range []struct {
		name  string
		patch store.CardPatch
	}{
		{"nothing at all", store.CardPatch{}},
		{"assignee only", store.CardPatch{Assignee: &who}},
		{"due only", store.CardPatch{DueDate: &due}},
		{"labels only", store.CardPatch{Labels: &both}},
		{"assignee and due", store.CardPatch{Assignee: &who, DueDate: &due}},
		{"assignee and labels", store.CardPatch{Assignee: &who, Labels: &both}},
		{"due and labels", store.CardPatch{DueDate: &due, Labels: &both}},
		{"all three", store.CardPatch{Assignee: &who, DueDate: &due, Labels: &both}},
		{"labels emptied", store.CardPatch{Labels: &none}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &model.Card{ID: newID("card"), BoardID: b.ID, ColumnID: b.Columns[0].ID,
				Title: "untouched", Description: "also untouched",
				CreatedAt: now(), UpdatedAt: now()}
			if err := s.CreateCard(ctx, c); err != nil {
				t.Fatal(err)
			}
			at := now().Add(time.Hour)
			if err := s.PatchCard(ctx, c.ID, tc.patch, at); err != nil {
				t.Fatalf("PatchCard: %v", err)
			}
			got, err := s.GetCard(ctx, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Title != "untouched" || got.Description != "also untouched" {
				t.Errorf("the patch touched what it did not name: %+v", got)
			}
			if !got.UpdatedAt.Equal(at) {
				t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, at)
			}
			wantAssignee := ""
			if tc.patch.Assignee != nil {
				wantAssignee = *tc.patch.Assignee
			}
			if got.Assignee != wantAssignee {
				t.Errorf("Assignee = %q, want %q", got.Assignee, wantAssignee)
			}
			wantDue := time.Time{}
			if tc.patch.DueDate != nil {
				wantDue = *tc.patch.DueDate
			}
			if !got.DueDate.Equal(wantDue) {
				t.Errorf("DueDate = %v, want %v", got.DueDate, wantDue)
			}
			wantLabels := 0
			if tc.patch.Labels != nil {
				wantLabels = len(*tc.patch.Labels)
			}
			if len(got.Labels) != wantLabels {
				t.Errorf("%d labels, want %d", len(got.Labels), wantLabels)
			}
		})
	}
}

// testPatchBoardMatrix does the same for a board, whose SLA is five columns.
func testPatchBoardMatrix(t *testing.T, s store.Store) {
	ctx := context.Background()
	name, rows := "Renamed", model.LayoutRows
	sla := model.SLA{ResponseHours: 6, Days: model.DaySet(0b0111110), Start: 9 * 60, End: 17 * 60, Zone: "Europe/Zurich"}

	// The slug is the one field two boards cannot share, so each case that sets
	// it gets its own.
	withSlug := func(p store.BoardPatch) store.BoardPatch {
		s := string(newID("slug"))
		p.Slug = &s
		return p
	}
	for _, tc := range []struct {
		name  string
		patch store.BoardPatch
	}{
		{"nothing at all", store.BoardPatch{}},
		{"name only", store.BoardPatch{Name: &name}},
		{"slug only", withSlug(store.BoardPatch{})},
		{"layout only", store.BoardPatch{Layout: &rows}},
		{"sla only", store.BoardPatch{SLA: &sla}},
		{"name and sla", store.BoardPatch{Name: &name, SLA: &sla}},
		{"layout and sla", store.BoardPatch{Layout: &rows, SLA: &sla}},
		{"everything", withSlug(store.BoardPatch{Name: &name, Layout: &rows, SLA: &sla})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := mustBoard(t, s, string(newID("m")), "To Do")
			// Read back, not as created: the store normalises a layout and an
			// SLA on the way in, so what it holds is what a patch has to leave
			// alone.
			was, err := s.GetBoardByID(ctx, b.ID)
			if err != nil {
				t.Fatal(err)
			}
			at := now().Add(time.Hour)
			if err := s.PatchBoard(ctx, b.ID, tc.patch, at); err != nil {
				t.Fatalf("PatchBoard: %v", err)
			}
			got, err := s.GetBoardByID(ctx, b.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := func(p *string, had string) string {
				if p != nil {
					return *p
				}
				return had
			}
			if got.Name != want(tc.patch.Name, was.Name) {
				t.Errorf("Name = %q", got.Name)
			}
			if got.Slug != want(tc.patch.Slug, was.Slug) {
				t.Errorf("Slug = %q", got.Slug)
			}
			if got.Layout != want(tc.patch.Layout, was.Layout) {
				t.Errorf("Layout = %q", got.Layout)
			}
			if tc.patch.SLA != nil && got.SLA != *tc.patch.SLA {
				t.Errorf("SLA = %+v, want %+v", got.SLA, *tc.patch.SLA)
			}
			if len(got.Columns) != 1 {
				t.Errorf("%d columns after a patch, want 1", len(got.Columns))
			}
			if !got.UpdatedAt.Equal(at) {
				t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, at)
			}
		})
	}
}
