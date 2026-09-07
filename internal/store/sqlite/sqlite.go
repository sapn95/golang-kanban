// Package sqlite is the SQLite store.Store, using modernc.org/sqlite and
// database/sql. The driver is pure Go, so the binary stays CGO-free. The
// schema lives in migrations/ and is embedded.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	driver "modernc.org/sqlite"

	"kanban/internal/model"
	"kanban/internal/store"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// sqlFile reads an embedded migration. A missing file is a build mistake, not
// a runtime condition, so it panics rather than returning an error nobody
// could act on.
func sqlFile(name string) string {
	b, err := migrationFiles.ReadFile("migrations/" + name)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func migrations() []store.Migration {
	return []store.Migration{
		{Version: 1, Name: "init", Up: initSchema(sqlFile("0001_init.sql"))},
		{Version: 2, Name: "import_v1", Up: importV1},
		{Version: 3, Name: "assignee", Up: store.SQL(sqlFile("0003_assignee.sql"))},
		{Version: 4, Name: "archive", Up: store.SQL(sqlFile("0004_archive.sql"))},
		{Version: 5, Name: "comments", Up: store.SQL(sqlFile("0005_comments.sql"))},
		{Version: 6, Name: "layout", Up: store.SQL(sqlFile("0006_layout.sql"))},
	}
}

// initSchema renames a v1 cards table out of the way and then creates the
// schema. Postgres does the rename in a DO block inside the same file; SQLite
// has no procedural language and no conditional DDL, so the condition is a Go
// statement and the file itself is plain CREATE TABLEs.
func initSchema(schema string) func(ctx context.Context, tx *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		if err := renameV1(ctx, tx); err != nil {
			return err
		}
		return store.SQL(schema)(ctx, tx)
	}
}

// Store is a SQLite-backed store.
type Store struct {
	db   *sql.DB
	path string
}

var _ store.Store = (*Store)(nil)

// Pragmas every connection needs. Foreign keys are off by default in SQLite
// and the schema leans on ON DELETE CASCADE throughout, so without this the
// cascades silently do nothing. WAL lets a reader run while the writer holds
// the file, and busy_timeout waits instead of failing when another process has
// it. _txlock=immediate takes the write lock at BEGIN: a transaction that
// starts as a reader and turns into a writer gets SQLITE_BUSY_SNAPSHOT, which
// busy_timeout does not retry.
const pragmas = "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_txlock=immediate"

// dsn appends the pragmas to a file name, keeping any the caller already set.
func dsn(path string) string {
	if strings.Contains(path, "?") {
		return path + "&" + pragmas
	}
	return path + "?" + pragmas
}

// Open opens the database file at path, creating it if it does not exist. It
// does not verify the file; call Ping. A path of ":memory:" gives a private
// database that lives as long as the Store.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, err
	}
	// SQLite serialises writers. With more than one connection the queue is
	// the busy timeout: writers spin, and one that waits longer than the
	// timeout fails with "database is locked". One connection puts the queue in
	// database/sql instead, where waiting is free and bounded by the context.
	// For a board three people share that costs nothing, and it keeps
	// ":memory:" usable, since a second connection would open a second, empty
	// database.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return &Store{db: db, path: path}, nil
}

// DB exposes the connection for tests and tooling.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) Migrate(ctx context.Context) error {
	_, err := store.RunMigrations(ctx, s.db, migrations())
	return err
}

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
func (s *Store) Close() error                   { return s.db.Close() }

// SQLite extended result codes; see https://sqlite.org/rescode.html. The driver
// switches extended codes on for every connection, so these arrive unmasked.
const (
	sqliteConstraintForeignKey = 787  // SQLITE_CONSTRAINT_FOREIGNKEY
	sqliteConstraintPrimaryKey = 1555 // SQLITE_CONSTRAINT_PRIMARYKEY
	sqliteConstraintUnique     = 2067 // SQLITE_CONSTRAINT_UNIQUE
)

// mapErr translates driver errors into the store sentinels.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	var sqliteErr *driver.Error
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() {
		case sqliteConstraintPrimaryKey, sqliteConstraintUnique:
			return store.ErrConflict
		case sqliteConstraintForeignKey:
			return store.ErrNotFound
		}
	}
	return err
}

func (s *Store) tx(ctx context.Context, fn func(tx *sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return mapErr(err)
	}
	return tx.Commit()
}

func affected(res sql.Result, err error) error {
	if err != nil {
		return mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// inClause renders "(?, ?, ...)" for n arguments. SQLite has no ANY(array), so
// every IN list is built for the number of arguments it is given.
func inClause(n int) string {
	if n == 0 {
		return "(NULL)"
	}
	return "(" + strings.Repeat("?, ", n-1) + "?)"
}

func idArgs(in []model.ID) []any {
	out := make([]any, len(in))
	for i, id := range in {
		out[i] = string(id)
	}
	return out
}

// Storage formats for the two Postgres types SQLite does not have. The
// timestamp fraction is fixed at nine digits so the text sorts like the
// instant it stands for, and a round trip returns the instant it was given.
const (
	timeLayout = "2006-01-02T15:04:05.000000000Z07:00"
	dateLayout = "2006-01-02"
)

func timeArg(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("sqlite: unreadable timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}

func dueArg(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(dateLayout)
}

func parseDate(s string) (time.Time, error) {
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("sqlite: unreadable date %q: %w", s, err)
	}
	return t, nil
}

// --- boards -----------------------------------------------------------------

const boardColumns = `id, slug, name, layout, created_at, updated_at`

func scanBoard(row interface{ Scan(...any) error }) (*model.Board, error) {
	var b model.Board
	var created, updated string
	if err := row.Scan(&b.ID, &b.Slug, &b.Name, &b.Layout, &created, &updated); err != nil {
		return nil, mapErr(err)
	}
	var err error
	if b.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if b.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, err
	}
	return &b, nil
}

// fillBoards loads columns and labels for the given boards.
func (s *Store) fillBoards(ctx context.Context, boards []*model.Board) error {
	if len(boards) == 0 {
		return nil
	}
	byID := map[model.ID]*model.Board{}
	var list []model.ID
	for _, b := range boards {
		byID[b.ID] = b
		list = append(list, b.ID)
	}
	in := inClause(len(list))
	args := idArgs(list)
	rows, err := s.db.QueryContext(ctx, `SELECT id, board_id, name, position, wip_limit FROM columns
		WHERE board_id IN `+in+` ORDER BY board_id, position, id`, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var c model.Column
		if err := rows.Scan(&c.ID, &c.BoardID, &c.Name, &c.Position, &c.WIPLimit); err != nil {
			_ = rows.Close()
			return err
		}
		byID[c.BoardID].Columns = append(byID[c.BoardID].Columns, c)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT id, board_id, name, color FROM labels
		WHERE board_id IN `+in+` ORDER BY board_id, name`, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var l model.Label
		if err := rows.Scan(&l.ID, &l.BoardID, &l.Name, &l.Color); err != nil {
			return err
		}
		byID[l.BoardID].Labels = append(byID[l.BoardID].Labels, l)
	}
	return rows.Err()
}

func (s *Store) ListBoards(ctx context.Context) ([]model.Board, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+boardColumns+` FROM boards ORDER BY name, slug`)
	if err != nil {
		return nil, err
	}
	var boards []*model.Board
	for rows.Next() {
		b, err := scanBoard(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		boards = append(boards, b)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.fillBoards(ctx, boards); err != nil {
		return nil, err
	}
	out := make([]model.Board, len(boards))
	for i, b := range boards {
		out[i] = *b
	}
	return out, nil
}

func (s *Store) getBoard(ctx context.Context, where string, arg any) (*model.Board, error) {
	b, err := scanBoard(s.db.QueryRowContext(ctx, `SELECT `+boardColumns+` FROM boards WHERE `+where, arg))
	if err != nil {
		return nil, err
	}
	if err := s.fillBoards(ctx, []*model.Board{b}); err != nil {
		return nil, err
	}
	return b, nil
}

func (s *Store) GetBoard(ctx context.Context, slug string) (*model.Board, error) {
	return s.getBoard(ctx, `slug = ?`, slug)
}

func (s *Store) GetBoardByID(ctx context.Context, id model.ID) (*model.Board, error) {
	return s.getBoard(ctx, `id = ?`, id)
}

func (s *Store) CreateBoard(ctx context.Context, b *model.Board) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO boards (id, slug, name, layout, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			b.ID, b.Slug, b.Name, model.LayoutOrDefault(b.Layout), timeArg(b.CreatedAt), timeArg(b.UpdatedAt)); err != nil {
			return err
		}
		for i := range b.Columns {
			c := &b.Columns[i]
			c.BoardID = b.ID
			c.Position = i + 1
			if _, err := tx.ExecContext(ctx, `INSERT INTO columns (id, board_id, name, position, wip_limit) VALUES (?, ?, ?, ?, ?)`,
				c.ID, c.BoardID, c.Name, c.Position, c.WIPLimit); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) UpdateBoard(ctx context.Context, b *model.Board) error {
	return affected(s.db.ExecContext(ctx, `UPDATE boards SET name = ?, slug = ?, layout = ?, updated_at = ? WHERE id = ?`,
		b.Name, b.Slug, model.LayoutOrDefault(b.Layout), timeArg(b.UpdatedAt), b.ID))
}

func (s *Store) DeleteBoard(ctx context.Context, id model.ID) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM boards WHERE id = ?`, id))
}

// --- columns ----------------------------------------------------------------

func (s *Store) CreateColumn(ctx context.Context, c *model.Column) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position), 0) + 1 FROM columns WHERE board_id = ?`, c.BoardID).Scan(&c.Position); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO columns (id, board_id, name, position, wip_limit) VALUES (?, ?, ?, ?, ?)`,
			c.ID, c.BoardID, c.Name, c.Position, c.WIPLimit)
		return err
	})
}

func (s *Store) UpdateColumn(ctx context.Context, c *model.Column) error {
	return affected(s.db.ExecContext(ctx, `UPDATE columns SET name = ?, wip_limit = ?, updated_at = ? WHERE id = ?`,
		c.Name, c.WIPLimit, timeArg(time.Now()), c.ID))
}

// columnCardIDs returns the cards of one column in their current order.
func columnCardIDs(ctx context.Context, tx *sql.Tx, columnID model.ID) ([]model.ID, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM cards WHERE column_id = ? ORDER BY position, id`, columnID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []model.ID
	for rows.Next() {
		var id model.ID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) DeleteColumn(ctx context.Context, id, moveCardsTo model.ID) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var boardID model.ID
		if err := tx.QueryRowContext(ctx, `SELECT board_id FROM columns WHERE id = ?`, id).Scan(&boardID); err != nil {
			return err
		}
		if moveCardsTo != "" {
			var targetBoard model.ID
			err := tx.QueryRowContext(ctx, `SELECT board_id FROM columns WHERE id = ?`, moveCardsTo).Scan(&targetBoard)
			if moveCardsTo == id || errors.Is(err, sql.ErrNoRows) || (err == nil && targetBoard != boardID) {
				return store.ErrInvalid
			}
			if err != nil {
				return err
			}
			// Postgres appends the cards with one UPDATE ... FROM over a
			// ROW_NUMBER(). SQLite has neither, and a correlated subquery is no
			// substitute: it would read rows this same statement has already
			// moved. So the new positions are numbered here.
			moved, err := columnCardIDs(ctx, tx, id)
			if err != nil {
				return err
			}
			var last int
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position), 0) FROM cards WHERE column_id = ?`, moveCardsTo).Scan(&last); err != nil {
				return err
			}
			for i, cardID := range moved {
				if _, err := tx.ExecContext(ctx, `UPDATE cards SET column_id = ?, position = ? WHERE id = ?`,
					moveCardsTo, last+i+1, cardID); err != nil {
					return err
				}
			}
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM columns WHERE id = ?`, id)
		return err
	})
}

func (s *Store) ReorderColumns(ctx context.Context, boardID model.ID, order []model.ID) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM boards WHERE id = ?)`, boardID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return store.ErrNotFound
		}
		rows, err := tx.QueryContext(ctx, `SELECT id FROM columns WHERE board_id = ?`, boardID)
		if err != nil {
			return err
		}
		current := map[model.ID]bool{}
		for rows.Next() {
			var id model.ID
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			current[id] = true
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(order) != len(current) {
			return store.ErrInvalid
		}
		seen := map[model.ID]bool{}
		for _, id := range order {
			if seen[id] || !current[id] {
				return store.ErrInvalid
			}
			seen[id] = true
		}
		for i, id := range order {
			if _, err := tx.ExecContext(ctx, `UPDATE columns SET position = ? WHERE id = ?`, i+1, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// --- cards ------------------------------------------------------------------

const cardColumns = `c.id, c.board_id, c.column_id, c.title, c.description, c.position, c.due_date, c.assignee, c.archived_at, c.created_at, c.updated_at`

func scanCard(row interface{ Scan(...any) error }) (*model.Card, error) {
	var c model.Card
	var due, archived sql.NullString
	var created, updated string
	if err := row.Scan(&c.ID, &c.BoardID, &c.ColumnID, &c.Title, &c.Description, &c.Position, &due, &c.Assignee, &archived, &created, &updated); err != nil {
		return nil, mapErr(err)
	}
	var err error
	if due.Valid {
		if c.DueDate, err = parseDate(due.String); err != nil {
			return nil, err
		}
	}
	if archived.Valid {
		if c.ArchivedAt, err = parseTime(archived.String); err != nil {
			return nil, err
		}
	}
	if c.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if c.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, err
	}
	return &c, nil
}

// fillCards loads subtasks and labels for the given cards.
func (s *Store) fillCards(ctx context.Context, cards []*model.Card) error {
	if len(cards) == 0 {
		return nil
	}
	byID := map[model.ID]*model.Card{}
	var list []model.ID
	for _, c := range cards {
		byID[c.ID] = c
		list = append(list, c.ID)
	}
	in := inClause(len(list))
	args := idArgs(list)
	rows, err := s.db.QueryContext(ctx, `SELECT id, card_id, title, done, position FROM subtasks
		WHERE card_id IN `+in+` ORDER BY card_id, position, id`, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var st model.Subtask
		var cardID model.ID
		if err := rows.Scan(&st.ID, &cardID, &st.Title, &st.Done, &st.Position); err != nil {
			_ = rows.Close()
			return err
		}
		byID[cardID].Subtasks = append(byID[cardID].Subtasks, st)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT card_id, label_id FROM card_labels
		WHERE card_id IN `+in+` ORDER BY card_id, label_id`, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cardID, labelID model.ID
		if err := rows.Scan(&cardID, &labelID); err != nil {
			return err
		}
		byID[cardID].Labels = append(byID[cardID].Labels, labelID)
	}
	return rows.Err()
}

// listCardsWhere runs one of the two card listings; they differ only in the
// predicate and the order, so the assembly of labels and subtasks is shared.
func (s *Store) listCardsWhere(ctx context.Context, query string, boardID model.ID) ([]model.Card, error) {
	rows, err := s.db.QueryContext(ctx, query, boardID)
	if err != nil {
		return nil, err
	}
	var cards []*model.Card
	for rows.Next() {
		c, err := scanCard(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		cards = append(cards, c)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.fillCards(ctx, cards); err != nil {
		return nil, err
	}
	out := make([]model.Card, len(cards))
	for i, c := range cards {
		out[i] = *c
	}
	return out, nil
}

func (s *Store) ListCards(ctx context.Context, boardID model.ID) ([]model.Card, error) {
	return s.listCardsWhere(ctx, `SELECT `+cardColumns+` FROM cards c JOIN columns col ON col.id = c.column_id
		WHERE c.board_id = ? AND c.archived_at IS NULL
		ORDER BY col.position, c.position, c.id`, boardID)
}

func (s *Store) ListArchivedCards(ctx context.Context, boardID model.ID) ([]model.Card, error) {
	return s.listCardsWhere(ctx, `SELECT `+cardColumns+` FROM cards c
		WHERE c.board_id = ? AND c.archived_at IS NOT NULL
		ORDER BY c.archived_at DESC, c.id`, boardID)
}

func (s *Store) SetCardArchived(ctx context.Context, id model.ID, at time.Time) error {
	var arg any
	if !at.IsZero() {
		arg = timeArg(at)
	}
	return affected(s.db.ExecContext(ctx, `UPDATE cards SET archived_at = ? WHERE id = ?`, arg, id))
}

func (s *Store) GetCard(ctx context.Context, id model.ID) (*model.Card, error) {
	c, err := scanCard(s.db.QueryRowContext(ctx, `SELECT `+cardColumns+` FROM cards c WHERE c.id = ?`, id))
	if err != nil {
		return nil, err
	}
	if err := s.fillCards(ctx, []*model.Card{c}); err != nil {
		return nil, err
	}
	return c, nil
}

// writeCardChildren replaces the subtasks and labels of a card inside tx.
func writeCardChildren(ctx context.Context, tx *sql.Tx, c *model.Card) error {
	if len(c.Labels) > 0 {
		args := append([]any{string(c.BoardID)}, idArgs(c.Labels)...)
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT id) FROM labels WHERE board_id = ? AND id IN `+inClause(len(c.Labels)),
			args...).Scan(&n); err != nil {
			return err
		}
		if n != len(uniqueIDs(c.Labels)) {
			return store.ErrNotFound
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM subtasks WHERE card_id = ?`, c.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM card_labels WHERE card_id = ?`, c.ID); err != nil {
		return err
	}
	for i := range c.Subtasks {
		st := &c.Subtasks[i]
		st.Position = i + 1
		if _, err := tx.ExecContext(ctx, `INSERT INTO subtasks (id, card_id, title, done, position) VALUES (?, ?, ?, ?, ?)`,
			st.ID, c.ID, st.Title, st.Done, st.Position); err != nil {
			return err
		}
	}
	c.Labels = uniqueIDs(c.Labels)
	for _, l := range c.Labels {
		if _, err := tx.ExecContext(ctx, `INSERT INTO card_labels (card_id, label_id) VALUES (?, ?)`, c.ID, l); err != nil {
			return err
		}
	}
	return nil
}

func uniqueIDs(in []model.ID) []model.ID {
	seen := map[model.ID]bool{}
	var out []model.ID
	for _, id := range in {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (s *Store) CreateCard(ctx context.Context, c *model.Card) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var ok bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM columns WHERE id = ? AND board_id = ?)`, c.ColumnID, c.BoardID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return store.ErrNotFound
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position), 0) + 1 FROM cards WHERE column_id = ?`, c.ColumnID).Scan(&c.Position); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO cards (id, board_id, column_id, title, description, position, due_date, assignee, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			c.ID, c.BoardID, c.ColumnID, c.Title, c.Description, c.Position, dueArg(c.DueDate), c.Assignee, timeArg(c.CreatedAt), timeArg(c.UpdatedAt)); err != nil {
			return err
		}
		return writeCardChildren(ctx, tx, c)
	})
}

func (s *Store) UpdateCard(ctx context.Context, c *model.Card) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `UPDATE cards SET title = ?, description = ?, due_date = ?, assignee = ?, updated_at = ?
			WHERE id = ? RETURNING board_id`, c.Title, c.Description, dueArg(c.DueDate), c.Assignee, timeArg(c.UpdatedAt), c.ID).Scan(&c.BoardID); err != nil {
			return err
		}
		return writeCardChildren(ctx, tx, c)
	})
}

func (s *Store) DeleteCard(ctx context.Context, id model.ID) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM cards WHERE id = ?`, id))
}

func (s *Store) ReorderCards(ctx context.Context, boardID, columnID model.ID, order []model.ID) error {
	listed := map[model.ID]bool{}
	for _, id := range order {
		if listed[id] {
			return store.ErrInvalid
		}
		listed[id] = true
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var ok bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM columns WHERE id = ? AND board_id = ?)`, columnID, boardID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return store.ErrNotFound
		}
		if len(order) > 0 {
			args := append([]any{string(boardID)}, idArgs(order)...)
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM cards WHERE board_id = ? AND id IN `+inClause(len(order)),
				args...).Scan(&n); err != nil {
				return err
			}
			if n != len(order) {
				return store.ErrNotFound
			}
		}
		// The cards of the column that the caller did not list keep their
		// relative order after the listed ones. Postgres numbers them with one
		// UPDATE ... FROM over a ROW_NUMBER(); see DeleteColumn for why that
		// does not survive the translation.
		rest, err := columnCardIDs(ctx, tx, columnID)
		if err != nil {
			return err
		}
		position := len(order)
		for _, id := range rest {
			if listed[id] {
				continue
			}
			position++
			if _, err := tx.ExecContext(ctx, `UPDATE cards SET position = ? WHERE id = ?`, position, id); err != nil {
				return err
			}
		}
		for i, id := range order {
			if _, err := tx.ExecContext(ctx, `UPDATE cards SET column_id = ?, position = ? WHERE id = ?`, columnID, i+1, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// --- comments ---------------------------------------------------------------

const commentColumns = `id, card_id, author, body, created_at`

func scanComment(row interface{ Scan(...any) error }) (*model.Comment, error) {
	var c model.Comment
	var created string
	if err := row.Scan(&c.ID, &c.CardID, &c.Author, &c.Body, &created); err != nil {
		return nil, mapErr(err)
	}
	var err error
	if c.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) ListComments(ctx context.Context, cardID model.ID) ([]model.Comment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+commentColumns+` FROM comments
		WHERE card_id = ? ORDER BY created_at, id`, cardID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.Comment
	for rows.Next() {
		c, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (s *Store) GetComment(ctx context.Context, id model.ID) (*model.Comment, error) {
	return scanComment(s.db.QueryRowContext(ctx, `SELECT `+commentColumns+` FROM comments WHERE id = ?`, id))
}

func (s *Store) CreateComment(ctx context.Context, c *model.Comment) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO comments (id, card_id, author, body, created_at) VALUES (?, ?, ?, ?, ?)`,
		c.ID, c.CardID, c.Author, c.Body, timeArg(c.CreatedAt))
	return mapErr(err)
}

func (s *Store) DeleteComment(ctx context.Context, id model.ID) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM comments WHERE id = ?`, id))
}

func (s *Store) CountComments(ctx context.Context, boardID model.ID) (map[model.ID]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT cm.card_id, COUNT(*) FROM comments cm
		JOIN cards c ON c.id = cm.card_id WHERE c.board_id = ? GROUP BY cm.card_id`, boardID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[model.ID]int{}
	for rows.Next() {
		var id model.ID
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// --- labels -----------------------------------------------------------------

func (s *Store) CreateLabel(ctx context.Context, l *model.Label) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO labels (id, board_id, name, color) VALUES (?, ?, ?, ?)`, l.ID, l.BoardID, l.Name, l.Color)
	return mapErr(err)
}

func (s *Store) UpdateLabel(ctx context.Context, l *model.Label) error {
	return affected(s.db.ExecContext(ctx, `UPDATE labels SET name = ?, color = ? WHERE id = ?`, l.Name, l.Color, l.ID))
}

func (s *Store) DeleteLabel(ctx context.Context, id model.ID) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM labels WHERE id = ?`, id))
}

// String describes the backend for logs.
func (s *Store) String() string { return fmt.Sprintf("sqlite(%s)", s.path) }
