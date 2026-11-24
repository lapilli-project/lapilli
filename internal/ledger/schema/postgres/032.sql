-- Whether a catch was loose, as SQLite's 032.sql: what the column is for
-- is said there.
ALTER TABLE catches ADD COLUMN loose BIGINT NOT NULL DEFAULT 0;
