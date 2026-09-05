-- Boards, columns, cards, labels, subtasks. See docs/adr/0002-board-column-card-model.md.
-- A v1 `cards` table (no column_id) is renamed to cards_v1; migration 0002 imports it.
DO $$
BEGIN
    IF to_regclass('cards') IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema() AND table_name = 'cards' AND column_name = 'column_id'
    ) THEN
        ALTER TABLE cards RENAME TO cards_v1;
    END IF;
END $$;

CREATE TABLE boards (
    id         TEXT PRIMARY KEY,
    slug       TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE columns (
    id         TEXT PRIMARY KEY,
    board_id   TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    position   INTEGER NOT NULL,
    wip_limit  INTEGER NOT NULL DEFAULT 0 CHECK (wip_limit >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
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
    due_date    DATE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
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
    done     BOOLEAN NOT NULL DEFAULT false,
    position INTEGER NOT NULL
);
CREATE INDEX subtasks_card_position ON subtasks (card_id, position);
