-- Comments are append-only, so there is no updated_at: a row never changes
-- after it is written. See internal/model.Comment for why. created_at is TEXT
-- holding RFC 3339 UTC, like every other timestamp in this schema.
CREATE TABLE comments (
    id         TEXT PRIMARY KEY,
    card_id    TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
    -- No foreign key: the board has no user table, and someone who logs in
    -- once and never again should not leave a row behind.
    author     TEXT NOT NULL DEFAULT '',
    body       TEXT NOT NULL,
    created_at TEXT NOT NULL
);
-- Both reads are per card, oldest first; the id breaks a tie between two
-- comments written in the same instant so the order is stable across calls.
CREATE INDEX comments_card_created ON comments (card_id, created_at, id);
