# 0002 — Board → Column → Card: columns are data

Status: proposed
Date: 2026-09-05

## Context

The schema is one table:

```sql
CREATE TABLE cards (
    id SERIAL PRIMARY KEY,
    title TEXT NOT NULL,
    description TEXT,
    subtasks TEXT,                                   -- "1|done thing\n0|open thing"
    status VARCHAR(20) NOT NULL DEFAULT 'todo',      -- todo | inprogress | done, checked in Go
    card_order INTEGER NOT NULL DEFAULT 0
);
```

The three columns are string constants in `main.go` and three hard-coded
`<div>`s in `index.html`. A card whose `status` is anything else is read from
the database and silently never rendered. Subtasks are a home-grown line
format; a `|` in a subtask title truncates it, and a line without `|` makes
`index $parts 1` fail and the whole board return 500. There is one board.

The roadmap needs: column add/remove/rename/reorder with WIP limits (the
maintainer's own todo), multiple boards, labels, due dates, markdown
descriptions, a checklist for subtasks, and later per-board membership.

## Decision

Three entities, all owned by a board, plus labels and subtasks:

```text
Board 1──* Column 1──* Card *──* Label      (labels are per board)
                         │
                         └──* Subtask
```

### Go model (`internal/model`)

```go
type ID string

type Board struct {
    ID        ID
    Slug      string     // URL segment, unique; [a-z0-9-]{1,64}
    Name      string
    Columns   []Column   // ordered by Position
    Labels    []Label
    CreatedAt time.Time
    UpdatedAt time.Time
}

type Column struct {
    ID       ID
    BoardID  ID
    Name     string
    Position int
    WIPLimit int        // 0 = no limit
}

type Card struct {
    ID          ID
    BoardID     ID
    ColumnID    ID
    Title       string
    Description string    // markdown; rendered server-side in Phase 2
    Position    int
    DueDate     time.Time // zero = none; date only, stored without time
    Labels      []ID
    Subtasks    []Subtask // ordered by Position
    CreatedAt   time.Time
    UpdatedAt   time.Time
}

type Label struct {
    ID      ID
    BoardID ID
    Name    string
    Color   string // CSS colour or empty for the default
}

type Subtask struct {
    ID       ID
    Title    string
    Done     bool
    Position int
}
```

`Card.BoardID` is denormalised on purpose: it lets `ReorderCards` refuse a
move across boards with one composite foreign key, and it is the partition key
for the Mongo and S3 backends.

### SQL schema (Postgres; `internal/store/postgres/migrations/0001_init.sql`)

```sql
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
    UNIQUE (id, board_id)                          -- target of the composite FK on cards
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
```

The SQLite file (Phase 1) is the same DDL with `TIMESTAMPTZ` → `TEXT`
(RFC 3339 UTC), `DATE` → `TEXT` (`YYYY-MM-DD`), `BOOLEAN` → `INTEGER`, and
`PRAGMA foreign_keys = ON` set per connection. Positions are not unique
constraints; `ReorderCards` renumbers a column from 1 inside its transaction,
and gaps left in other columns are harmless.

The document backends store a board as one document/object of the same shape
as the JSON snapshot, which is the Go model serialised with `cards` nested
under the board.

### Migration of existing data

Migration `0001_init` is SQL: if a table `cards` exists without a `column_id`
column, it is renamed to `cards_v1`, then the tables above are created.
Migration `0002_import_v1` is a Go function: if `cards_v1` exists, it creates
one board (`slug` `board`, name `Kanban Board`) with columns `To Do`,
`In Progress`, `Done`, copies every row with a fresh ID into the matching
column keeping `card_order`, parses the `flag|title` subtask lines into
`subtasks` rows (a `|` in the title is now kept), maps an unknown `status` to
the first column instead of dropping the card, and drops `cards_v1` in the
same transaction. Postgres users get the usual advice in the release notes:
`pg_dump` before upgrading.

`/` redirects to `/b/board`, so bookmarks keep working.

## Consequences

- Column CRUD, WIP limits, and multiple boards are schema features from day
  one; Phase 2 is UI work only.
- Existing Postgres data survives with the same visible content; IDs and the
  table shape change once, under `AUTO_MIGRATE` or `kanban migrate`.
- Labels, due dates, and subtasks are proper rows, so the JSON API in Phase 4
  and the snapshot format in Phase 5 do not have to parse a line format.
- Users and board memberships (Phase 3) attach to `boards` with two tables
  (`users`, `board_members`) and touch nothing above.
- Archiving cards, card comments, attachments, and per-column colours are
  deliberately not in this schema. Each is one additive migration when it is
  asked for.
