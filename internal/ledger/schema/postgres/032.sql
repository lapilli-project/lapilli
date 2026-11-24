-- Which labels a catch lacked, as SQLite's 032.sql: what the column is for
-- is said there.
ALTER TABLE catches ADD COLUMN loose TEXT NOT NULL DEFAULT '';
