-- Nullable rather than a boolean plus a timestamp: one column cannot disagree
-- with itself about whether the card is archived. TEXT holding RFC 3339 UTC,
-- like every other timestamp in this schema.
ALTER TABLE cards ADD COLUMN archived_at TEXT;
CREATE INDEX cards_board_active ON cards (board_id) WHERE archived_at IS NULL;
