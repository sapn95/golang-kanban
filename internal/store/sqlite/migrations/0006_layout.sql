-- How the board draws its columns: side by side, or stacked as rows with the
-- cards flowing across each one. A default rather than a nullable column:
-- every board has a layout, and an existing board keeps the one it had.
ALTER TABLE boards ADD COLUMN layout TEXT NOT NULL DEFAULT 'columns';
