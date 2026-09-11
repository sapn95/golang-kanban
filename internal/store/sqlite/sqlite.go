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

// migrations is the schema's history, in the order it happened. Each one runs
// once, in version order, and one already recorded as applied is skipped.
func migrations() []store.Migration {
	return []store.Migration{
		{Version: 1, Name: "init", Up: initSchema(sqlFile("0001_init.sql"))},
		{Version: 2, Name: "import_v1", Up: importV1},
		{Version: 3, Name: "assignee", Up: store.SQL(sqlFile("0003_assignee.sql"))},
		{Version: 4, Name: "archive", Up: store.SQL(sqlFile("0004_archive.sql"))},
		{Version: 5, Name: "comments", Up: store.SQL(sqlFile("0005_comments.sql"))},
		{Version: 6, Name: "layout", Up: store.SQL(sqlFile("0006_layout.sql"))},
		{Version: 7, Name: "sla", Up: store.SQL(sqlFile("0007_sla.sql"))},
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

// Migrate applies the migrations that have not run yet. Safe on every start,
// which is what lets AUTO_MIGRATE default to true.
func (s *Store) Migrate(ctx context.Context) error {
	_, err := store.RunMigrations(ctx, s.db, migrations())
	return err
}

// Ping asks the database whether it is there, for /readyz and for the doctor.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Close hands the connection pool back.
func (s *Store) Close() error { return s.db.Close() }

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

// tx runs fn inside a transaction, committing when it returns nil and rolling
// back on anything else, panic included. Every write below that touches more
// than one row goes through it, so a half-written board cannot be left behind.
func (s *Store) tx(ctx context.Context, fn func(tx *sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		// A panic never reaches the assignment below, so err is still nil when
		// this runs and the rollback would be skipped: the transaction and its
		// connection would be left open for whoever recovers. Caught here and
		// re-raised, so the panic still reaches the recover in the middleware
		// and still becomes a 500.
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return mapErr(err)
	}
	return tx.Commit()
}

// affected turns "no rows changed" into ErrNotFound. A write that matched
// nothing and a write that succeeded look the same to the driver, and the
// difference is the whole of what a caller wants to know.
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

// idArgs turns a list of ids into the arguments an IN clause needs, so an id is
// never spliced into the SQL as text.
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

// timeArg writes a time as RFC 3339 in UTC. SQLite has no time type, so the
// format is the contract and it has to be one that sorts as text.
func timeArg(t time.Time) string { return t.UTC().Format(timeLayout) }

// parseTime reads a time back, accepting the formats older rows were written
// in as well as the one written now.
func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("sqlite: unreadable timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}

// dueArg turns an empty due date into NULL rather than into an empty string, so
// "no date" is one value in the column and not two.
func dueArg(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(dateLayout)
}

// parseDate reads a due date, which is a day and not an instant.
func parseDate(s string) (time.Time, error) {
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("sqlite: unreadable date %q: %w", s, err)
	}
	return t, nil
}

// --- boards -----------------------------------------------------------------

const boardColumns = `id, slug, name, layout, sla_response_hours, sla_days, sla_start, sla_end, sla_zone, created_at, updated_at`

// scanBoard reads one board row. Its columns and labels come from their own
// queries, so a board is three reads rather than a join returning the board
// once per column.
func scanBoard(row interface{ Scan(...any) error }) (*model.Board, error) {
	var b model.Board
	var created, updated string
	var days int
	if err := row.Scan(&b.ID, &b.Slug, &b.Name, &b.Layout,
		&b.SLA.ResponseHours, &days, &b.SLA.Start, &b.SLA.End, &b.SLA.Zone,
		&created, &updated); err != nil {
		return nil, mapErr(err)
	}
	b.SLA.Days = model.DaySet(days)
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
	rows, err := s.db.QueryContext(ctx, `SELECT id, board_id, name, position, wip_limit, stops_clock FROM columns
		WHERE board_id IN `+in+` ORDER BY board_id, position, id`, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var c model.Column
		if err := rows.Scan(&c.ID, &c.BoardID, &c.Name, &c.Position, &c.WIPLimit, &c.StopsClock); err != nil {
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

// ListBoards returns every board with its columns and labels, in name order.
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

// getBoard reads a board with its columns and labels. Shared by the two public
// lookups, which differ only in the WHERE clause and the argument they pass.
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

// GetBoard returns one board by its slug, which is what a URL carries.
func (s *Store) GetBoard(ctx context.Context, slug string) (*model.Board, error) {
	return s.getBoard(ctx, `slug = ?`, slug)
}

// GetBoardByID returns one board by id, which is what a card carries.
func (s *Store) GetBoardByID(ctx context.Context, id model.ID) (*model.Board, error) {
	return s.getBoard(ctx, `id = ?`, id)
}

// CreateBoard writes a board and its columns in one transaction, numbering the
// columns 1..n. A duplicate slug comes back from the unique index as a
// conflict rather than as a driver error nobody upstream can read.
func (s *Store) CreateBoard(ctx context.Context, b *model.Board) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		sla := b.SLA.Clean()
		if _, err := tx.ExecContext(ctx, `INSERT INTO boards
			(id, slug, name, layout, sla_response_hours, sla_days, sla_start, sla_end, sla_zone, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			b.ID, b.Slug, b.Name, model.LayoutOrDefault(b.Layout),
			sla.ResponseHours, int(sla.Days), sla.Start, sla.End, sla.Zone,
			timeArg(b.CreatedAt), timeArg(b.UpdatedAt)); err != nil {
			return err
		}
		for i := range b.Columns {
			c := &b.Columns[i]
			c.BoardID = b.ID
			c.Position = i + 1
			if _, err := tx.ExecContext(ctx, `INSERT INTO columns (id, board_id, name, position, wip_limit, stops_clock) VALUES (?, ?, ?, ?, ?, ?)`,
				c.ID, c.BoardID, c.Name, c.Position, c.WIPLimit, c.StopsClock); err != nil {
				return err
			}
		}
		return nil
	})
}

// UpdateBoard changes a board's own fields. Columns and labels have their own
// calls, so a rename cannot drop one.
func (s *Store) UpdateBoard(ctx context.Context, b *model.Board) error {
	sla := b.SLA.Clean()
	return affected(s.db.ExecContext(ctx, `UPDATE boards SET name = ?, slug = ?, layout = ?,
		sla_response_hours = ?, sla_days = ?, sla_start = ?, sla_end = ?, sla_zone = ?, updated_at = ? WHERE id = ?`,
		b.Name, b.Slug, model.LayoutOrDefault(b.Layout),
		sla.ResponseHours, int(sla.Days), sla.Start, sla.End, sla.Zone, timeArg(b.UpdatedAt), b.ID))
}

// DeleteBoard removes a board. The foreign keys cascade, so its columns,
// cards, labels and comments go with it in the same statement.
func (s *Store) DeleteBoard(ctx context.Context, id model.ID) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM boards WHERE id = ?`, id))
}

// --- columns ----------------------------------------------------------------

// CreateColumn appends a column at the end of its board, taking the position
// from MAX(position) + 1 in the same transaction. SQLite serialises writers, so
// here that really is one at a time.
func (s *Store) CreateColumn(ctx context.Context, c *model.Column) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position), 0) + 1 FROM columns WHERE board_id = ?`, c.BoardID).Scan(&c.Position); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO columns (id, board_id, name, position, wip_limit, stops_clock) VALUES (?, ?, ?, ?, ?, ?)`,
			c.ID, c.BoardID, c.Name, c.Position, c.WIPLimit, c.StopsClock)
		return err
	})
}

// UpdateColumn changes a column's name, its limit and whether it stops the
// response clock.
func (s *Store) UpdateColumn(ctx context.Context, c *model.Column) error {
	return affected(s.db.ExecContext(ctx, `UPDATE columns SET name = ?, wip_limit = ?, stops_clock = ?, updated_at = ? WHERE id = ?`,
		c.Name, c.WIPLimit, c.StopsClock, timeArg(time.Now()), c.ID))
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

// DeleteColumn removes a column, moving its cards to another column of the same
// board or deleting them with it. One transaction, so the cards are never
// homeless in between.
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

// ReorderColumns rewrites the positions of a board's columns. The order has to
// name every column exactly once, checked before the first write.
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

// scanCard reads one card row, leaving labels and subtasks to their own queries.
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

// ListCards returns a board's live cards in column order then card order, with
// their labels and subtasks read in bulk rather than per card.
func (s *Store) ListCards(ctx context.Context, boardID model.ID) ([]model.Card, error) {
	return s.listCardsWhere(ctx, `SELECT `+cardColumns+` FROM cards c JOIN columns col ON col.id = c.column_id
		WHERE c.board_id = ? AND c.archived_at IS NULL
		ORDER BY col.position, c.position, c.id`, boardID)
}

// ListArchivedCards returns the cards taken off a board, newest first, because
// an archive is read from the top.
func (s *Store) ListArchivedCards(ctx context.Context, boardID model.ID) ([]model.Card, error) {
	return s.listCardsWhere(ctx, `SELECT `+cardColumns+` FROM cards c
		WHERE c.board_id = ? AND c.archived_at IS NOT NULL
		ORDER BY c.archived_at DESC, c.id`, boardID)
}

// PatchCard writes the columns the patch names and stamps the card, in one
// transaction, without reading it first. A quick edit on the card face changes
// one thing and must not carry back a stale copy of the rest.
func (s *Store) PatchCard(ctx context.Context, id model.ID, p store.CardPatch, at time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		sets, args := []string{"updated_at = ?"}, []any{timeArg(at)}
		if p.Assignee != nil {
			sets = append(sets, "assignee = ?")
			args = append(args, *p.Assignee)
		}
		if p.DueDate != nil {
			sets = append(sets, "due_date = ?")
			args = append(args, dueArg(*p.DueDate))
		}
		args = append(args, id)
		if err := affected(tx.ExecContext(ctx,
			`UPDATE cards SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)); err != nil {
			return err
		}
		if p.Labels == nil {
			return nil
		}
		// Replaced wholesale, like UpdateCard does: a card carries few enough
		// labels that naming the difference would be more to get wrong.
		if _, err := tx.ExecContext(ctx, `DELETE FROM card_labels WHERE card_id = ?`, id); err != nil {
			return err
		}
		for _, l := range uniqueIDs(*p.Labels) {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO card_labels (card_id, label_id) VALUES (?, ?)`, id, l); err != nil {
				return err
			}
		}
		return nil
	})
}

// PatchBoard does the same for a board.
func (s *Store) PatchBoard(ctx context.Context, id model.ID, p store.BoardPatch, at time.Time) error {
	sets, args := []string{"updated_at = ?"}, []any{timeArg(at)}
	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}
	if p.Name != nil {
		add("name", *p.Name)
	}
	if p.Slug != nil {
		add("slug", *p.Slug)
	}
	if p.Layout != nil {
		add("layout", *p.Layout)
	}
	if p.SLA != nil {
		sla := *p.SLA
		add("sla_response_hours", sla.ResponseHours)
		add("sla_days", int(sla.Days))
		add("sla_start", sla.Start)
		add("sla_end", sla.End)
		add("sla_zone", sla.Zone)
	}
	args = append(args, id)
	return affected(s.db.ExecContext(ctx,
		`UPDATE boards SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...))
}

// SetSubtaskDone flips one subtask's done column and stamps its card, in one
// transaction and without reading the card first, so a tick cannot carry back a
// stale copy of everything else on it.
func (s *Store) SetSubtaskDone(ctx context.Context, cardID, subtaskID model.ID, done bool, at time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := affected(tx.ExecContext(ctx,
			`UPDATE subtasks SET done = ? WHERE id = ? AND card_id = ?`, done, subtaskID, cardID)); err != nil {
			return err
		}
		return affected(tx.ExecContext(ctx,
			`UPDATE cards SET updated_at = ? WHERE id = ?`, timeArg(at), cardID))
	})
}

// TouchCard writes UpdatedAt and nothing else, so a comment cannot hand back an
// edit that landed while it was being written.
func (s *Store) TouchCard(ctx context.Context, id model.ID, at time.Time) error {
	return affected(s.db.ExecContext(ctx, `UPDATE cards SET updated_at = ? WHERE id = ?`, timeArg(at), id))
}

// SetCardArchived sets or clears a card's archived_at. Its column and position
// are left alone, so restoring it puts it back where it was.
func (s *Store) SetCardArchived(ctx context.Context, id model.ID, at time.Time) error {
	var arg any
	if !at.IsZero() {
		arg = timeArg(at)
	}
	return affected(s.db.ExecContext(ctx, `UPDATE cards SET archived_at = ? WHERE id = ?`, arg, id))
}

// GetCard returns one card with its labels and subtasks.
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

// uniqueIDs returns the ids once each, sorted. Sorted so a card carrying the
// same labels in a different order writes the same rows, which keeps an
// update from looking like a change when nothing changed.
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

// CreateCard appends a card to its column, with its labels and subtasks, in one
// transaction.
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

// UpdateCard replaces a card's content and its labels and subtasks, and never
// its column or position: moving a card is a different call.
func (s *Store) UpdateCard(ctx context.Context, c *model.Card) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `UPDATE cards SET title = ?, description = ?, due_date = ?, assignee = ?, updated_at = ?
			WHERE id = ? RETURNING board_id`, c.Title, c.Description, dueArg(c.DueDate), c.Assignee, timeArg(c.UpdatedAt), c.ID).Scan(&c.BoardID); err != nil {
			return err
		}
		return writeCardChildren(ctx, tx, c)
	})
}

// DeleteCard removes a card. Its labels, subtasks and comments cascade.
func (s *Store) DeleteCard(ctx context.Context, id model.ID) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM cards WHERE id = ?`, id))
}

// ReorderCards rewrites one column's card positions, and moves in any card the
// order names that was in another column of the same board.
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

// scanComment reads one comment row.
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

// ListComments returns a card's comments oldest first, which is how a thread
// reads.
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

// GetComment returns one comment, for the author check before a delete.
func (s *Store) GetComment(ctx context.Context, id model.ID) (*model.Comment, error) {
	return scanComment(s.db.QueryRowContext(ctx, `SELECT `+commentColumns+` FROM comments WHERE id = ?`, id))
}

// CreateComment writes a comment against a card that exists.
func (s *Store) CreateComment(ctx context.Context, c *model.Comment) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO comments (id, card_id, author, body, created_at) VALUES (?, ?, ?, ?, ?)`,
		c.ID, c.CardID, c.Author, c.Body, timeArg(c.CreatedAt))
	return mapErr(err)
}

// DeleteComment removes one comment. Who may is decided above this.
func (s *Store) DeleteComment(ctx context.Context, id model.ID) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM comments WHERE id = ?`, id))
}

// CountComments returns each card's comment count for a whole board in one
// query, so drawing the badges is not a read per card.
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

// CreateLabel adds a label to a board. A name already used on that board comes
// back as a conflict: two labels with one name cannot be told apart on a card.
func (s *Store) CreateLabel(ctx context.Context, l *model.Label) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO labels (id, board_id, name, color) VALUES (?, ?, ?, ?)`, l.ID, l.BoardID, l.Name, l.Color)
	return mapErr(err)
}

// UpdateLabel renames a label or changes its colour, on every card at once.
func (s *Store) UpdateLabel(ctx context.Context, l *model.Label) error {
	return affected(s.db.ExecContext(ctx, `UPDATE labels SET name = ?, color = ? WHERE id = ?`, l.Name, l.Color, l.ID))
}

// DeleteLabel removes a label and the rows tying it to cards, so no card is
// left pointing at something that is gone.
func (s *Store) DeleteLabel(ctx context.Context, id model.ID) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM labels WHERE id = ?`, id))
}

// String describes the backend for logs.
func (s *Store) String() string { return fmt.Sprintf("sqlite(%s)", s.path) }
