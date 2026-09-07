package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kanban/internal/model"
	"kanban/internal/store"
	"kanban/internal/store/storetest"
)

// fresh returns an empty database file without the schema applied. Every
// subtest gets its own file in its own temporary directory, which t.Cleanup
// removes, so nothing has to be dropped between runs.
func fresh(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "kanban.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("sqlite not reachable: %v", err)
	}
	return s
}

func migrated(t *testing.T) *Store {
	t.Helper()
	s := fresh(t)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store { return migrated(t) })
}

// TestPragmas pins the settings the schema depends on. Without foreign keys
// every ON DELETE CASCADE is silently inert, and without the busy timeout a
// second process writing the file fails instead of waiting for it.
func TestPragmas(t *testing.T) {
	s := migrated(t)
	for _, tc := range []struct{ pragma, want string }{
		{"foreign_keys", "1"},
		{"busy_timeout", "5000"},
		{"journal_mode", "wal"},
	} {
		var got string
		if err := s.DB().QueryRow(`PRAGMA ` + tc.pragma).Scan(&got); err != nil {
			t.Fatalf("PRAGMA %s: %v", tc.pragma, err)
		}
		if strings.ToLower(got) != tc.want {
			t.Errorf("PRAGMA %s = %q, want %q", tc.pragma, got, tc.want)
		}
	}
}

func TestDSN(t *testing.T) {
	if got := dsn("/data/kanban.db"); !strings.HasPrefix(got, "/data/kanban.db?_pragma=") {
		t.Errorf("dsn = %q", got)
	}
	if got := dsn("file:kanban.db?mode=ro"); !strings.Contains(got, "?mode=ro&_pragma=") {
		t.Errorf("dsn with existing query = %q", got)
	}
}

// TestTimeRoundTrip checks the storage format directly: the contract suite
// compares instants, and TEXT that loses precision or the zone would still
// look plausible in the database.
func TestTimeRoundTrip(t *testing.T) {
	for _, want := range []time.Time{
		time.Date(2025, 1, 2, 3, 4, 5, 123456000, time.UTC),
		time.Date(2025, 12, 31, 23, 59, 59, 999999999, time.UTC),
		time.Date(2026, 6, 1, 12, 0, 0, 0, time.FixedZone("CEST", 2*60*60)),
	} {
		text := timeArg(want)
		if !strings.HasSuffix(text, "Z") {
			t.Errorf("timeArg(%v) = %q, want a UTC instant", want, text)
		}
		got, err := parseTime(text)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equal(want) {
			t.Errorf("round trip of %v through %q = %v", want, text, got)
		}
	}
	if _, err := parseTime("yesterday"); err == nil {
		t.Error("want an error for unparseable text")
	}
	if _, err := parseDate("2026-13-40"); err == nil {
		t.Error("want an error for an impossible date")
	}
	if dueArg(time.Time{}) != nil {
		t.Error("a zero due date must be stored as NULL")
	}
	if got := dueArg(time.Date(2026, 12, 24, 22, 0, 0, 0, time.UTC)); got != "2026-12-24" {
		t.Errorf("dueArg = %v", got)
	}
}

// TestTimestampsSurviveTheFile reopens the database, because a format that
// only round-trips through the driver's in-memory cache would pass the
// contract suite and still lose data on restart.
func TestTimestampsSurviveTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kanban.db")
	created := time.Date(2025, 1, 2, 3, 4, 5, 123456000, time.UTC)
	due := time.Date(2026, 12, 24, 0, 0, 0, 0, time.UTC)
	ctx := context.Background()

	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	b := &model.Board{ID: "b1", Slug: "s", Name: "n", CreatedAt: created, UpdatedAt: created,
		Columns: []model.Column{{ID: "c1", Name: "A"}}}
	if err := first.CreateBoard(ctx, b); err != nil {
		t.Fatal(err)
	}
	card := &model.Card{ID: "k1", BoardID: b.ID, ColumnID: "c1", Title: "t", DueDate: due, CreatedAt: created, UpdatedAt: created}
	if err := first.CreateCard(ctx, card); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	got, err := second.GetBoard(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(created) || !got.UpdatedAt.Equal(created) {
		t.Errorf("board timestamps after reopen = %v %v, want %v", got.CreatedAt, got.UpdatedAt, created)
	}
	gotCard, err := second.GetCard(ctx, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if !gotCard.CreatedAt.Equal(created) || !gotCard.DueDate.Equal(due) {
		t.Errorf("card timestamps after reopen = %v %v", gotCard.CreatedAt, gotCard.DueDate)
	}
}

// TestConcurrentWrites is the reason the pool is capped at one connection: on
// a pool of five, this fails with "database is locked", because SQLite hands a
// second writer SQLITE_BUSY instead of queueing it.
func TestConcurrentWrites(t *testing.T) {
	s := migrated(t)
	ctx := context.Background()
	b := &model.Board{ID: "b1", Slug: "s", Name: "n", Columns: []model.Column{{ID: "c1", Name: "A"}}}
	if err := s.CreateBoard(ctx, b); err != nil {
		t.Fatal(err)
	}

	const writers, each = 8, 10
	errs := make(chan error, writers*each)
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				id := model.ID(string(rune('a'+w)) + "-" + string(rune('a'+i)))
				errs <- s.CreateCard(ctx, &model.Card{ID: id, BoardID: b.ID, ColumnID: "c1", Title: string(id)})
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent CreateCard: %v", err)
		}
	}
	cards, err := s.ListCards(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != writers*each {
		t.Fatalf("cards = %d, want %d", len(cards), writers*each)
	}
	// Every CreateCard reads MAX(position) and inserts; if two of them could
	// interleave, positions would repeat.
	seen := map[int]bool{}
	for _, c := range cards {
		if seen[c.Position] {
			t.Fatalf("duplicate position %d", c.Position)
		}
		seen[c.Position] = true
	}
}

const v1Schema = `CREATE TABLE cards (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
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
		if _, err := db.Exec(`INSERT INTO cards (title, description, subtasks, status, card_order) VALUES (?, ?, ?, ?, ?)`,
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
		got = append(got, b.Column(c.ColumnID).Name+":"+c.Title)
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
	var left int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'cards_v1'`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("cards_v1 still there: %d, %v", left, err)
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

// TestMigrateKeepsV2Cards is the other half of the rename: a database already
// on the current schema must not have its cards table moved aside.
func TestMigrateKeepsV2Cards(t *testing.T) {
	s := migrated(t)
	ctx := context.Background()
	b := &model.Board{ID: "b1", Slug: "s", Name: "n", Columns: []model.Column{{ID: "c1", Name: "A"}}}
	if err := s.CreateBoard(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateCard(ctx, &model.Card{ID: "k1", BoardID: b.ID, ColumnID: "c1", Title: "t"}); err != nil {
		t.Fatal(err)
	}
	// Force migration 1 to run again over a populated schema.
	if _, err := s.DB().Exec(`DELETE FROM schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err == nil {
		t.Fatal("re-creating the schema must fail, not rename cards away")
	}
	if _, err := s.GetCard(ctx, "k1"); err != nil {
		t.Fatalf("card lost: %v", err)
	}
	var renamed int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'cards_v1'`).Scan(&renamed); err != nil || renamed != 0 {
		t.Fatalf("current cards table was renamed away: %d, %v", renamed, err)
	}
}

func TestMigrateFreshIsEmpty(t *testing.T) {
	s := migrated(t)
	boards, err := s.ListBoards(context.Background())
	if err != nil || len(boards) != 0 {
		t.Fatalf("boards = %v, %v", boards, err)
	}
}

// TestForeignKeysAreEnforced proves the pragma reached the connection rather
// than relying on DeleteBoard happening to remove the rows itself.
func TestForeignKeysAreEnforced(t *testing.T) {
	s := migrated(t)
	_, err := s.DB().Exec(`INSERT INTO columns (id, board_id, name, position) VALUES ('c1', 'nosuchboard', 'A', 1)`)
	if !errors.Is(mapErr(err), store.ErrNotFound) {
		t.Fatalf("orphan column accepted or wrongly mapped: %v", err)
	}
}

func TestMigrateOnClosedDB(t *testing.T) {
	s := fresh(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Migrate(ctx); err == nil {
		t.Fatal("want error on closed db")
	}
	if _, err := s.ListBoards(ctx); err == nil {
		t.Fatal("want error on closed db")
	}
	if err := s.CreateBoard(ctx, &model.Board{ID: "x", Slug: "x"}); err == nil {
		t.Fatal("want error on closed db")
	}
}

func TestOpenBadPath(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "no", "such", "dir", "kanban.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
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

func TestInClause(t *testing.T) {
	for n, want := range map[int]string{0: "(NULL)", 1: "(?)", 3: "(?, ?, ?)"} {
		if got := inClause(n); got != want {
			t.Errorf("inClause(%d) = %q, want %q", n, got, want)
		}
	}
}

// The live database already has cards, so version 3 runs ALTER TABLE against
// a populated table. A fresh-database test would never exercise that, and it
// is the only path the deployed board actually takes.
func TestAssigneeMigrationOnAPopulatedTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kanban.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	db := st.DB()
	t.Cleanup(func() { _ = st.Close() })

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
