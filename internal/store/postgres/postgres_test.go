package postgres

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"kanban/internal/model"
	"kanban/internal/store"
	"kanban/internal/store/storetest"
)

// testDSN is read from KANBAN_TEST_POSTGRES_URL; tests are skipped without it.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("KANBAN_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("KANBAN_TEST_POSTGRES_URL not set")
	}
	return dsn
}

// reset drops every table the schema owns. It refuses to touch a database whose
// name does not end in _test, so a KANBAN_TEST_POSTGRES_URL that points at a
// development or staging database fails loudly instead of destroying it. The
// table names are generic enough (cards, columns, labels, boards) to collide
// with an unrelated schema, and CASCADE would take dependent objects with them.
func reset(t *testing.T, db *sql.DB) {
	t.Helper()
	var name string
	if err := db.QueryRow(`SELECT current_database()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(name, "_test") {
		t.Fatalf("refusing to drop tables in database %q: KANBAN_TEST_POSTGRES_URL must name a disposable database whose name ends in _test", name)
	}
	_, err := db.Exec(`DROP TABLE IF EXISTS card_labels, subtasks, cards, labels, columns, boards, schema_migrations, cards_v1 CASCADE`)
	if err != nil {
		t.Fatal(err)
	}
}

// fresh returns an empty database without the schema applied.
func fresh(t *testing.T) *Store {
	t.Helper()
	s, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("postgres not reachable: %v", err)
	}
	reset(t, s.DB())
	return s
}

func TestContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		s := fresh(t)
		if err := s.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		return s
	})
}

const v1Schema = `CREATE TABLE cards (
    id SERIAL PRIMARY KEY,
    title TEXT NOT NULL,
    description TEXT,
    subtasks TEXT,
    status VARCHAR(20) NOT NULL DEFAULT 'todo',
    card_order INTEGER NOT NULL DEFAULT 0
)`

func TestImportV1(t *testing.T) {
	s := fresh(t)
	ctx := context.Background()
	db := s.DB()
	if _, err := db.Exec(v1Schema); err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		title, desc, subtasks, status string
		order                         int
	}{
		{"second todo", "", "", "todo", 2},
		{"first todo", "desc", "1|done|pipe\n0|open\n", "todo", 1},
		{"doing", "", "", "inprogress", 1},
		{"finished", "", "", "done", 7},
		{"weird", "", "", "archived", 1},
	}
	for _, r := range rows {
		var desc any = r.desc
		if r.desc == "" {
			desc = nil
		}
		if _, err := db.Exec(`INSERT INTO cards (title, description, subtasks, status, card_order) VALUES ($1, $2, $3, $4, $5)`,
			r.title, desc, r.subtasks, r.status, r.order); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := s.GetBoard(ctx, DefaultBoardSlug)
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != DefaultBoardName || len(b.Columns) != 3 || b.Columns[0].Name != "To Do" || b.Columns[2].Name != "Done" {
		t.Fatalf("board = %+v", b)
	}
	cards, err := s.ListCards(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range cards {
		got = append(got, string(c.ColumnID)[:0]+b.Column(c.ColumnID).Name+":"+c.Title)
	}
	want := []string{"To Do:first todo", "To Do:second todo", "To Do:weird", "In Progress:doing", "Done:finished"}
	if len(got) != len(want) {
		t.Fatalf("cards = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cards = %v, want %v", got, want)
		}
	}
	first := cards[0]
	if first.Description != "desc" || first.Position != 1 || len(first.Subtasks) != 2 ||
		first.Subtasks[0].Title != "done|pipe" || !first.Subtasks[0].Done || first.Subtasks[1].Title != "open" || first.Subtasks[1].Done {
		t.Fatalf("first = %+v", first)
	}
	if cards[1].Position != 2 || cards[2].Position != 3 || cards[4].Position != 1 {
		t.Fatalf("positions not renumbered: %+v", cards)
	}
	var gone bool
	if err := db.QueryRow(`SELECT to_regclass('cards_v1') IS NULL`).Scan(&gone); err != nil || !gone {
		t.Fatalf("cards_v1 still there: %v", err)
	}
	// Idempotent.
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	again, _ := s.ListCards(ctx, b.ID)
	if len(again) != len(cards) {
		t.Fatalf("second migrate changed data: %d cards", len(again))
	}
}

func TestMigrateFreshIsEmpty(t *testing.T) {
	s := fresh(t)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	boards, err := s.ListBoards(context.Background())
	if err != nil || len(boards) != 0 {
		t.Fatalf("boards = %v, %v", boards, err)
	}
}

func TestMigrationRunner(t *testing.T) {
	s := fresh(t)
	ctx := context.Background()
	db := s.DB()

	applied, err := store.RunMigrations(ctx, db, []store.Migration{
		{Version: 2, Name: "two", Up: store.SQL(`CREATE TABLE t2 (x INT)`)},
		{Version: 1, Name: "one", Up: store.SQL(`CREATE TABLE t1 (x INT)`)},
	})
	if err != nil || len(applied) != 2 || applied[0] != 1 || applied[1] != 2 {
		t.Fatalf("applied = %v, %v", applied, err)
	}
	applied, err = store.RunMigrations(ctx, db, []store.Migration{
		{Version: 1, Name: "one", Up: store.SQL(`SELECT 1/0`)},
		{Version: 3, Name: "boom", Up: store.SQL(`CREATE TABLE t3 (x INT); SELECT 1/0`)},
	})
	if err == nil || len(applied) != 0 {
		t.Fatalf("want failure, got applied=%v err=%v", applied, err)
	}
	var exists bool
	if err := db.QueryRow(`SELECT to_regclass('t3') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("failed migration was not rolled back: %v %v", exists, err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("schema_migrations rows = %d, %v", n, err)
	}
	_, err = store.RunMigrations(ctx, db, []store.Migration{{Version: 1}, {Version: 1}})
	if err == nil {
		t.Fatal("duplicate versions must fail")
	}
	failing := errors.New("nope")
	_, err = store.RunMigrations(ctx, db, []store.Migration{{Version: 4, Name: "fn", Up: func(context.Context, *sql.Tx) error { return failing }}})
	if !errors.Is(err, failing) {
		t.Fatalf("err = %v", err)
	}
	if _, err := db.Exec(`DROP TABLE t1, t2`); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateOnClosedDB(t *testing.T) {
	s := fresh(t)
	s.Close()
	if err := s.Migrate(context.Background()); err == nil {
		t.Fatal("want error on closed db")
	}
	if _, err := s.ListBoards(context.Background()); err == nil {
		t.Fatal("want error on closed db")
	}
	if err := s.CreateBoard(context.Background(), &model.Board{ID: "x", Slug: "x"}); err == nil {
		t.Fatal("want error on closed db")
	}
}

func TestOpenBadDSN(t *testing.T) {
	testDSN(t)
	s, err := Open("postgres://nobody:none@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Ping(context.Background()); err == nil {
		t.Fatal("want ping error")
	}
	if s.String() == "" {
		t.Fatal("String empty")
	}
}

func TestMapErr(t *testing.T) {
	if mapErr(nil) != nil {
		t.Fatal("nil")
	}
	if !errors.Is(mapErr(sql.ErrNoRows), store.ErrNotFound) {
		t.Fatal("ErrNoRows")
	}
	other := errors.New("other")
	if mapErr(other) != other {
		t.Fatal("other")
	}
}

// The live database already has cards, so version 3 runs ALTER TABLE against
// a populated table. A fresh-database test would never exercise that, and it
// is the only path the deployed board actually takes.
func TestAssigneeMigrationOnAPopulatedTable(t *testing.T) {
	dsn := testDSN(t)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	reset(t, db)

	// Everything up to the version before assignee, which is the state a
	// board that has been running has.
	before := []store.Migration{}
	for _, m := range migrations() {
		if m.Version < 3 {
			before = append(before, m)
		}
	}
	if _, err := store.RunMigrations(context.Background(), db, before); err != nil {
		t.Fatalf("migrating to v2: %v", err)
	}

	st := &Store{db: db}
	ctx := context.Background()
	// Seeded with raw SQL on purpose: at version 2 the store's own CreateCard
	// would write a column that does not exist yet, which is the whole point.
	if _, err := db.ExecContext(ctx, `INSERT INTO boards (id, slug, name) VALUES ('b-mig', 'mig', 'Mig')`); err != nil {
		t.Fatalf("seeding a board: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO columns (id, board_id, name, position) VALUES ('c-mig', 'b-mig', 'A', 1)`); err != nil {
		t.Fatalf("seeding a column: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO cards (id, board_id, column_id, title, position) VALUES ('k-mig', 'b-mig', 'c-mig', 'existing', 1)`); err != nil {
		t.Fatalf("seeding a card: %v", err)
	}

	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrating to v3 over existing rows: %v", err)
	}

	got, err := st.GetCard(ctx, "k-mig")
	if err != nil {
		t.Fatalf("the card did not survive the migration: %v", err)
	}
	if got.Title != "existing" {
		t.Errorf("Title = %q, want existing", got.Title)
	}
	if got.Assignee != "" {
		t.Errorf("Assignee = %q, want empty for a row that predates the column", got.Assignee)
	}
	// And the column is writable afterwards, not just readable.
	got.Assignee = "someone@example.com"
	got.UpdatedAt = time.Now().UTC()
	if err := st.UpdateCard(ctx, got); err != nil {
		t.Fatalf("UpdateCard after the migration: %v", err)
	}
	if got, err = st.GetCard(ctx, "k-mig"); err != nil || got.Assignee != "someone@example.com" {
		t.Fatalf("Assignee = %q (err %v) after assigning post-migration", got.Assignee, err)
	}
}
