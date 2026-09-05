package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// Migration is one schema step. Up runs inside a transaction; the version is
// recorded in the same transaction, so a failed step leaves no trace.
type Migration struct {
	Version int
	Name    string
	Up      func(ctx context.Context, tx *sql.Tx) error
}

// SQL wraps one or more SQL statements as a migration step.
func SQL(statements string) func(ctx context.Context, tx *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, statements)
		return err
	}
}

const migrationsTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version    INTEGER PRIMARY KEY,
	name       TEXT NOT NULL,
	applied_at TEXT NOT NULL
)`

// RunMigrations applies every migration whose version is not yet recorded,
// in ascending order, and returns the versions it applied.
func RunMigrations(ctx context.Context, db *sql.DB, migrations []Migration) ([]int, error) {
	sorted := append([]Migration(nil), migrations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Version < sorted[j].Version })
	for i := 1; i < len(sorted); i++ {
		if sorted[i].Version == sorted[i-1].Version {
			return nil, fmt.Errorf("migrate: duplicate version %d", sorted[i].Version)
		}
	}
	if _, err := db.ExecContext(ctx, migrationsTable); err != nil {
		return nil, fmt.Errorf("migrate: create schema_migrations: %w", err)
	}
	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return nil, err
	}
	var done []int
	for _, m := range sorted {
		if applied[m.Version] {
			continue
		}
		if err := applyOne(ctx, db, m); err != nil {
			return done, err
		}
		done = append(done, m.Version)
	}
	return done, nil
}

func appliedVersions(ctx context.Context, db *sql.DB) (map[int]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("migrate: read schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

func applyOne(ctx context.Context, db *sql.DB, m Migration) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate %d: %w", m.Version, err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = m.Up(ctx, tx); err != nil {
		return fmt.Errorf("migrate %d %s: %w", m.Version, m.Name, err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, applied_at) VALUES ($1, $2, $3)`,
		m.Version, m.Name, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("migrate %d: record: %w", m.Version, err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("migrate %d: commit: %w", m.Version, err)
	}
	return nil
}
