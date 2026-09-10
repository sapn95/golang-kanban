-- The board's response-time promise: how many office hours a card may sit
-- untouched, and which hours of which days count as office hours.
--
-- Five columns on the board rather than a table of its own, because a board has
-- exactly one promise and a row that is always there is a set of columns. The
-- days are a bitmask over time.Weekday, so the whole week is one integer; 62 is
-- Monday to Friday. The hours are minutes since midnight rather than a
-- timestamp: a clock reading with no date is not an instant, and storing one
-- would put a year 1 in the database. Zero hours is the promise switched off,
-- which is what every board that existed before this migration keeps.
ALTER TABLE boards ADD COLUMN sla_response_hours INTEGER NOT NULL DEFAULT 0 CHECK (sla_response_hours >= 0);
ALTER TABLE boards ADD COLUMN sla_days INTEGER NOT NULL DEFAULT 62 CHECK (sla_days BETWEEN 0 AND 127);
ALTER TABLE boards ADD COLUMN sla_start INTEGER NOT NULL DEFAULT 480 CHECK (sla_start BETWEEN 0 AND 1440);
ALTER TABLE boards ADD COLUMN sla_end INTEGER NOT NULL DEFAULT 1020 CHECK (sla_end BETWEEN 0 AND 1440);
-- An IANA zone name, empty for UTC. The office hours of a desk are local hours,
-- so a clock that ran in UTC would open an hour late for half the year.
ALTER TABLE boards ADD COLUMN sla_zone TEXT NOT NULL DEFAULT '';

-- Where the clock stops. A card in Done, or in one of the columns that means
-- somebody else has the ball, is not a card nobody has attended to.
ALTER TABLE columns ADD COLUMN stops_clock INTEGER NOT NULL DEFAULT 0 CHECK (stops_clock IN (0, 1));
