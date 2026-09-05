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

func newID(prefix string) model.ID {
	seq++
	return model.ID(fmt.Sprintf("%s-%03d", prefix, seq))
}

func now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

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

func mustCard(t *testing.T, s store.Store, b *model.Board, col model.ID, title string) *model.Card {
	t.Helper()
	c := &model.Card{ID: newID("k"), BoardID: b.ID, ColumnID: col, Title: title, CreatedAt: now(), UpdatedAt: now()}
	if err := s.CreateCard(context.Background(), c); err != nil {
		t.Fatalf("CreateCard(%s): %v", title, err)
	}
	return c
}

func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: got %v, want %v", what, err, want)
	}
}

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

func testPing(t *testing.T, s store.Store) {
	if err := s.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

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
