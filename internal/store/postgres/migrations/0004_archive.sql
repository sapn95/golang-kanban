-- Nullable rather than a boolean plus a timestamp: one column cannot disagree
-- with itself about whether the card is archived.
ALTER TABLE cards ADD COLUMN archived_at TIMESTAMPTZ;
-- The board reads only the un-archived cards, and that is the hot path.
CREATE INDEX cards_board_active ON cards (board_id) WHERE archived_at IS NULL;
