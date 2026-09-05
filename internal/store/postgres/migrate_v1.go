package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"kanban/internal/model"
)

// Default board created for imported v1 data and by the service on an empty
// database.
const (
	DefaultBoardSlug = "board"
	DefaultBoardName = "Kanban Board"
)

// v1Columns maps the old status constants to column names, in board order.
var v1Columns = []struct{ status, name string }{
	{"todo", "To Do"},
	{"inprogress", "In Progress"},
	{"done", "Done"},
}

// importV1 moves rows from cards_v1 (the renamed single-table schema) into a
// default board and drops the old table. It is a no-op when there is none.
func importV1(ctx context.Context, tx *sql.Tx) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('cards_v1') IS NOT NULL`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	now := time.Now().UTC()
	boardID := model.NewID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO boards (id, slug, name, created_at, updated_at) VALUES ($1, $2, $3, $4, $4)`,
		boardID, DefaultBoardSlug, DefaultBoardName, now); err != nil {
		return fmt.Errorf("create default board: %w", err)
	}
	columnByStatus := map[string]model.ID{}
	for i, c := range v1Columns {
		id := model.NewID()
		columnByStatus[c.status] = id
		if _, err := tx.ExecContext(ctx, `INSERT INTO columns (id, board_id, name, position, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $5)`,
			id, boardID, c.name, i+1, now); err != nil {
			return fmt.Errorf("create column %s: %w", c.name, err)
		}
	}

	type v1card struct {
		title, description, subtasks, status string
	}
	rows, err := tx.QueryContext(ctx, `SELECT title, COALESCE(description, ''), COALESCE(subtasks, ''), status
		FROM cards_v1
		ORDER BY CASE status WHEN 'todo' THEN 0 WHEN 'inprogress' THEN 1 WHEN 'done' THEN 2 ELSE 3 END, card_order, id`)
	if err != nil {
		return fmt.Errorf("read cards_v1: %w", err)
	}
	var old []v1card
	for rows.Next() {
		var c v1card
		if err := rows.Scan(&c.title, &c.description, &c.subtasks, &c.status); err != nil {
			_ = rows.Close()
			return err
		}
		old = append(old, c)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	position := map[model.ID]int{}
	for _, c := range old {
		col, ok := columnByStatus[c.status]
		if !ok {
			col = columnByStatus["todo"]
		}
		position[col]++
		id := model.NewID()
		if _, err := tx.ExecContext(ctx, `INSERT INTO cards (id, board_id, column_id, title, description, position, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`, id, boardID, col, c.title, c.description, position[col], now); err != nil {
			return fmt.Errorf("import card %q: %w", c.title, err)
		}
		for _, st := range model.ParseSubtasks(c.subtasks) {
			if _, err := tx.ExecContext(ctx, `INSERT INTO subtasks (id, card_id, title, done, position) VALUES ($1, $2, $3, $4, $5)`,
				model.NewID(), id, st.Title, st.Done, st.Position); err != nil {
				return fmt.Errorf("import subtask of %q: %w", c.title, err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE cards_v1`); err != nil {
		return fmt.Errorf("drop cards_v1: %w", err)
	}
	return nil
}
