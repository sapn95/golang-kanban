-- Not a foreign key to a users table, because there is no users table: the
-- identity comes from whatever authenticated the request, and a card should
-- not stop rendering because someone left.
ALTER TABLE cards ADD COLUMN assignee TEXT NOT NULL DEFAULT '';
CREATE INDEX cards_assignee ON cards (assignee) WHERE assignee <> '';
