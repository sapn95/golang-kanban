-- Boards, columns, cards, labels, subtasks. See docs/adr/0002-board-column-card-model.md.
-- The SQLite dialect of the Postgres schema: TIMESTAMPTZ becomes TEXT holding
-- RFC 3339 UTC, DATE becomes TEXT holding YYYY-MM-DD, BOOLEAN becomes INTEGER.
-- The v1 `cards` table is renamed to cards_v1 by renameV1 before this file
-- runs; SQLite has no DO block and no conditional DDL, so that test lives in Go.
-- Every ON DELETE CASCADE below is inert unless the connection has switched
-- foreign keys on, which Open does through the DSN.

CREATE TABLE boards (
    id         TEXT PRIMARY KEY,
    slug       TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f000Z', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f000Z', 'now'))
);

CREATE TABLE columns (
    id         TEXT PRIMARY KEY,
    board_id   TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    position   INTEGER NOT NULL,
    wip_limit  INTEGER NOT NULL DEFAULT 0 CHECK (wip_limit >= 0),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f000Z', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f000Z', 'now')),
    UNIQUE (id, board_id)
);
CREATE INDEX columns_board_position ON columns (board_id, position);

CREATE TABLE cards (
    id          TEXT PRIMARY KEY,
    board_id    TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
    column_id   TEXT NOT NULL,
    title       TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    position    INTEGER NOT NULL,
    due_date    TEXT,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f000Z', 'now')),
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f000Z', 'now')),
    FOREIGN KEY (column_id, board_id) REFERENCES columns (id, board_id) ON DELETE CASCADE
);
CREATE INDEX cards_column_position ON cards (column_id, position);

CREATE TABLE labels (
    id       TEXT PRIMARY KEY,
    board_id TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
    name     TEXT NOT NULL,
    color    TEXT NOT NULL DEFAULT '',
    UNIQUE (board_id, name)
);

CREATE TABLE card_labels (
    card_id  TEXT NOT NULL REFERENCES cards(id)  ON DELETE CASCADE,
    label_id TEXT NOT NULL REFERENCES labels(id) ON DELETE CASCADE,
    PRIMARY KEY (card_id, label_id)
);

CREATE TABLE subtasks (
    id       TEXT PRIMARY KEY,
    card_id  TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
    title    TEXT NOT NULL,
    done     INTEGER NOT NULL DEFAULT 0 CHECK (done IN (0, 1)),
    position INTEGER NOT NULL
);
CREATE INDEX subtasks_card_position ON subtasks (card_id, position);
