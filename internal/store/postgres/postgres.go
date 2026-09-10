// Package postgres is the PostgreSQL store.Store, using lib/pq and
// database/sql. The schema lives in migrations/ and is embedded.
package postgres

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/lib/pq"

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
		{Version: 1, Name: "init", Up: store.SQL(sqlFile("0001_init.sql"))},
		{Version: 2, Name: "import_v1", Up: importV1},
		{Version: 3, Name: "assignee", Up: store.SQL(sqlFile("0003_assignee.sql"))},
		{Version: 4, Name: "archive", Up: store.SQL(sqlFile("0004_archive.sql"))},
		{Version: 5, Name: "comments", Up: store.SQL(sqlFile("0005_comments.sql"))},
		{Version: 6, Name: "layout", Up: store.SQL(sqlFile("0006_layout.sql"))},
		{Version: 7, Name: "sla", Up: store.SQL(sqlFile("0007_sla.sql"))},
	}
}

// Store is a PostgreSQL-backed store.
type Store struct {
	db *sql.DB
}

var _ store.Store = (*Store)(nil)

// Open connects to dsn. It does not verify the connection; call Ping.
func Open(dsn string) (*Store, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(1)
	return &Store{db: db}, nil
}

// DB exposes the connection for tests and tooling.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) Migrate(ctx context.Context) error {
	_, err := store.RunMigrations(ctx, s.db, migrations())
	return err
}

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
func (s *Store) Close() error                   { return s.db.Close() }

const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
)

// mapErr translates driver errors into the store sentinels.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	var pgErr *pq.Error
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgUniqueViolation:
			return store.ErrConflict
		case pgForeignKeyViolation:
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

func ids(in []model.ID) []string {
	out := make([]string, len(in))
	for i, id := range in {
		out[i] = string(id)
	}
	return out
}

// --- boards -----------------------------------------------------------------

const boardColumns = `id, slug, name, layout, sla_response_hours, sla_days, sla_start, sla_end, sla_zone, created_at, updated_at`

func scanBoard(row interface{ Scan(...any) error }) (*model.Board, error) {
	var b model.Board
	var days int
	if err := row.Scan(&b.ID, &b.Slug, &b.Name, &b.Layout,
		&b.SLA.ResponseHours, &days, &b.SLA.Start, &b.SLA.End, &b.SLA.Zone,
		&b.CreatedAt, &b.UpdatedAt); err != nil {
		return nil, mapErr(err)
	}
	b.SLA.Days = model.DaySet(days)
	b.CreatedAt, b.UpdatedAt = b.CreatedAt.UTC(), b.UpdatedAt.UTC()
	return &b, nil
}

// fillBoards loads columns and labels for the given boards.
func (s *Store) fillBoards(ctx context.Context, boards []*model.Board) error {
	if len(boards) == 0 {
		return nil
	}
	byID := map[model.ID]*model.Board{}
	var list []string
	for _, b := range boards {
		byID[b.ID] = b
		list = append(list, string(b.ID))
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, board_id, name, position, wip_limit, stops_clock FROM columns
		WHERE board_id = ANY($1) ORDER BY board_id, position, id`, pq.Array(list))
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
		WHERE board_id = ANY($1) ORDER BY board_id, name`, pq.Array(list))
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
	return s.getBoard(ctx, `slug = $1`, slug)
}

func (s *Store) GetBoardByID(ctx context.Context, id model.ID) (*model.Board, error) {
	return s.getBoard(ctx, `id = $1`, id)
}

func (s *Store) CreateBoard(ctx context.Context, b *model.Board) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		sla := b.SLA.Clean()
		if _, err := tx.ExecContext(ctx, `INSERT INTO boards
			(id, slug, name, layout, sla_response_hours, sla_days, sla_start, sla_end, sla_zone, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			b.ID, b.Slug, b.Name, model.LayoutOrDefault(b.Layout),
			sla.ResponseHours, int(sla.Days), sla.Start, sla.End, sla.Zone,
			b.CreatedAt.UTC(), b.UpdatedAt.UTC()); err != nil {
			return err
		}
		for i := range b.Columns {
			c := &b.Columns[i]
			c.BoardID = b.ID
			c.Position = i + 1
			if _, err := tx.ExecContext(ctx, `INSERT INTO columns (id, board_id, name, position, wip_limit, stops_clock) VALUES ($1, $2, $3, $4, $5, $6)`,
				c.ID, c.BoardID, c.Name, c.Position, c.WIPLimit, c.StopsClock); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) UpdateBoard(ctx context.Context, b *model.Board) error {
	sla := b.SLA.Clean()
	return affected(s.db.ExecContext(ctx, `UPDATE boards SET name = $1, slug = $2, layout = $3,
		sla_response_hours = $4, sla_days = $5, sla_start = $6, sla_end = $7, sla_zone = $8, updated_at = $9 WHERE id = $10`,
		b.Name, b.Slug, model.LayoutOrDefault(b.Layout),
		sla.ResponseHours, int(sla.Days), sla.Start, sla.End, sla.Zone, b.UpdatedAt.UTC(), b.ID))
}

func (s *Store) DeleteBoard(ctx context.Context, id model.ID) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM boards WHERE id = $1`, id))
}

// --- columns ----------------------------------------------------------------

func (s *Store) CreateColumn(ctx context.Context, c *model.Column) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position), 0) + 1 FROM columns WHERE board_id = $1`, c.BoardID).Scan(&c.Position); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO columns (id, board_id, name, position, wip_limit, stops_clock) VALUES ($1, $2, $3, $4, $5, $6)`,
			c.ID, c.BoardID, c.Name, c.Position, c.WIPLimit, c.StopsClock)
		return err
	})
}

func (s *Store) UpdateColumn(ctx context.Context, c *model.Column) error {
	return affected(s.db.ExecContext(ctx, `UPDATE columns SET name = $1, wip_limit = $2, stops_clock = $3, updated_at = now() WHERE id = $4`,
		c.Name, c.WIPLimit, c.StopsClock, c.ID))
}

func (s *Store) DeleteColumn(ctx context.Context, id, moveCardsTo model.ID) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var boardID model.ID
		if err := tx.QueryRowContext(ctx, `SELECT board_id FROM columns WHERE id = $1`, id).Scan(&boardID); err != nil {
			return err
		}
		if moveCardsTo != "" {
			var targetBoard model.ID
			err := tx.QueryRowContext(ctx, `SELECT board_id FROM columns WHERE id = $1`, moveCardsTo).Scan(&targetBoard)
			if moveCardsTo == id || errors.Is(err, sql.ErrNoRows) || (err == nil && targetBoard != boardID) {
				return store.ErrInvalid
			}
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE cards SET column_id = $1,
				position = (SELECT COALESCE(MAX(position), 0) FROM cards WHERE column_id = $1) + moved.rn
				FROM (SELECT id, ROW_NUMBER() OVER (ORDER BY position, id) AS rn FROM cards WHERE column_id = $2) moved
				WHERE cards.id = moved.id`, moveCardsTo, id); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM columns WHERE id = $1`, id)
		return err
	})
}

func (s *Store) ReorderColumns(ctx context.Context, boardID model.ID, order []model.ID) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM boards WHERE id = $1)`, boardID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return store.ErrNotFound
		}
		rows, err := tx.QueryContext(ctx, `SELECT id FROM columns WHERE board_id = $1`, boardID)
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
			if _, err := tx.ExecContext(ctx, `UPDATE columns SET position = $1 WHERE id = $2`, i+1, id); err != nil {
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
	var due, archived sql.NullTime
	if err := row.Scan(&c.ID, &c.BoardID, &c.ColumnID, &c.Title, &c.Description, &c.Position, &due, &c.Assignee, &archived, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, mapErr(err)
	}
	if due.Valid {
		c.DueDate = time.Date(due.Time.Year(), due.Time.Month(), due.Time.Day(), 0, 0, 0, 0, time.UTC)
	}
	if archived.Valid {
		c.ArchivedAt = archived.Time.UTC()
	}
	c.CreatedAt, c.UpdatedAt = c.CreatedAt.UTC(), c.UpdatedAt.UTC()
	return &c, nil
}

func dueArg(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format("2006-01-02")
}

// fillCards loads subtasks and labels for the given cards.
func (s *Store) fillCards(ctx context.Context, cards []*model.Card) error {
	if len(cards) == 0 {
		return nil
	}
	byID := map[model.ID]*model.Card{}
	var list []string
	for _, c := range cards {
		byID[c.ID] = c
		list = append(list, string(c.ID))
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, card_id, title, done, position FROM subtasks
		WHERE card_id = ANY($1) ORDER BY card_id, position, id`, pq.Array(list))
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
		WHERE card_id = ANY($1) ORDER BY card_id, label_id`, pq.Array(list))
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
		WHERE c.board_id = $1 AND c.archived_at IS NULL
		ORDER BY col.position, c.position, c.id`, boardID)
}

func (s *Store) ListArchivedCards(ctx context.Context, boardID model.ID) ([]model.Card, error) {
	return s.listCardsWhere(ctx, `SELECT `+cardColumns+` FROM cards c
		WHERE c.board_id = $1 AND c.archived_at IS NOT NULL
		ORDER BY c.archived_at DESC, c.id`, boardID)
}

func (s *Store) SetCardArchived(ctx context.Context, id model.ID, at time.Time) error {
	var arg any
	if !at.IsZero() {
		arg = at.UTC()
	}
	return affected(s.db.ExecContext(ctx, `UPDATE cards SET archived_at = $1 WHERE id = $2`, arg, id))
}

func (s *Store) GetCard(ctx context.Context, id model.ID) (*model.Card, error) {
	c, err := scanCard(s.db.QueryRowContext(ctx, `SELECT `+cardColumns+` FROM cards c WHERE c.id = $1`, id))
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
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT id) FROM labels WHERE board_id = $1 AND id = ANY($2)`,
			c.BoardID, pq.Array(ids(c.Labels))).Scan(&n); err != nil {
			return err
		}
		if n != len(uniqueIDs(c.Labels)) {
			return store.ErrNotFound
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM subtasks WHERE card_id = $1`, c.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM card_labels WHERE card_id = $1`, c.ID); err != nil {
		return err
	}
	for i := range c.Subtasks {
		st := &c.Subtasks[i]
		st.Position = i + 1
		if _, err := tx.ExecContext(ctx, `INSERT INTO subtasks (id, card_id, title, done, position) VALUES ($1, $2, $3, $4, $5)`,
			st.ID, c.ID, st.Title, st.Done, st.Position); err != nil {
			return err
		}
	}
	c.Labels = uniqueIDs(c.Labels)
	for _, l := range c.Labels {
		if _, err := tx.ExecContext(ctx, `INSERT INTO card_labels (card_id, label_id) VALUES ($1, $2)`, c.ID, l); err != nil {
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
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM columns WHERE id = $1 AND board_id = $2)`, c.ColumnID, c.BoardID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return store.ErrNotFound
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position), 0) + 1 FROM cards WHERE column_id = $1`, c.ColumnID).Scan(&c.Position); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO cards (id, board_id, column_id, title, description, position, due_date, assignee, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			c.ID, c.BoardID, c.ColumnID, c.Title, c.Description, c.Position, dueArg(c.DueDate), c.Assignee, c.CreatedAt.UTC(), c.UpdatedAt.UTC()); err != nil {
			return err
		}
		return writeCardChildren(ctx, tx, c)
	})
}

func (s *Store) UpdateCard(ctx context.Context, c *model.Card) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `UPDATE cards SET title = $1, description = $2, due_date = $3, assignee = $4, updated_at = $5
			WHERE id = $6 RETURNING board_id`, c.Title, c.Description, dueArg(c.DueDate), c.Assignee, c.UpdatedAt.UTC(), c.ID).Scan(&c.BoardID); err != nil {
			return err
		}
		return writeCardChildren(ctx, tx, c)
	})
}

func (s *Store) DeleteCard(ctx context.Context, id model.ID) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM cards WHERE id = $1`, id))
}

func (s *Store) ReorderCards(ctx context.Context, boardID, columnID model.ID, order []model.ID) error {
	seen := map[model.ID]bool{}
	for _, id := range order {
		if seen[id] {
			return store.ErrInvalid
		}
		seen[id] = true
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var ok bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM columns WHERE id = $1 AND board_id = $2)`, columnID, boardID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return store.ErrNotFound
		}
		listed := pq.Array(ids(order))
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM cards WHERE board_id = $1 AND id = ANY($2)`, boardID, listed).Scan(&n); err != nil {
			return err
		}
		if n != len(order) {
			return store.ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `UPDATE cards SET position = $1 + rest.rn
			FROM (SELECT id, ROW_NUMBER() OVER (ORDER BY position, id) AS rn FROM cards WHERE column_id = $2 AND NOT (id = ANY($3))) rest
			WHERE cards.id = rest.id`, len(order), columnID, listed); err != nil {
			return err
		}
		for i, id := range order {
			if _, err := tx.ExecContext(ctx, `UPDATE cards SET column_id = $1, position = $2 WHERE id = $3`, columnID, i+1, id); err != nil {
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
	if err := row.Scan(&c.ID, &c.CardID, &c.Author, &c.Body, &c.CreatedAt); err != nil {
		return nil, mapErr(err)
	}
	c.CreatedAt = c.CreatedAt.UTC()
	return &c, nil
}

func (s *Store) ListComments(ctx context.Context, cardID model.ID) ([]model.Comment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+commentColumns+` FROM comments
		WHERE card_id = $1 ORDER BY created_at, id`, cardID)
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
	return scanComment(s.db.QueryRowContext(ctx, `SELECT `+commentColumns+` FROM comments WHERE id = $1`, id))
}

func (s *Store) CreateComment(ctx context.Context, c *model.Comment) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO comments (id, card_id, author, body, created_at) VALUES ($1, $2, $3, $4, $5)`,
		c.ID, c.CardID, c.Author, c.Body, c.CreatedAt.UTC())
	return mapErr(err)
}

func (s *Store) DeleteComment(ctx context.Context, id model.ID) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM comments WHERE id = $1`, id))
}

func (s *Store) CountComments(ctx context.Context, boardID model.ID) (map[model.ID]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT cm.card_id, COUNT(*) FROM comments cm
		JOIN cards c ON c.id = cm.card_id WHERE c.board_id = $1 GROUP BY cm.card_id`, boardID)
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO labels (id, board_id, name, color) VALUES ($1, $2, $3, $4)`, l.ID, l.BoardID, l.Name, l.Color)
	return mapErr(err)
}

func (s *Store) UpdateLabel(ctx context.Context, l *model.Label) error {
	return affected(s.db.ExecContext(ctx, `UPDATE labels SET name = $1, color = $2 WHERE id = $3`, l.Name, l.Color, l.ID))
}

func (s *Store) DeleteLabel(ctx context.Context, id model.ID) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM labels WHERE id = $1`, id))
}

// String describes the backend for logs.
func (s *Store) String() string { return fmt.Sprintf("postgres(%p)", s.db) }
