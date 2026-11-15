-- Declared configuration (docs/design/serveops.md), as SQLite's 019.sql:
-- what each column is for is said there.
ALTER TABLE teams ADD COLUMN declared BIGINT NOT NULL DEFAULT 0;
ALTER TABLE windows ADD COLUMN declared BIGINT NOT NULL DEFAULT 0;
ALTER TABLE reports ADD COLUMN declared BIGINT NOT NULL DEFAULT 0;
ALTER TABLE grants ADD COLUMN declared BIGINT NOT NULL DEFAULT 0;

-- A team's name in lower case, ASCII letters alone under COLLATE "C", as
-- SQLite's lower(); NULL for those whose names differ only in letter case
-- from another's already.
ALTER TABLE teams ADD COLUMN name_key TEXT COLLATE "C";
UPDATE teams SET name_key = lower(name)
	WHERE lower(name) IN (SELECT lower(name) FROM teams GROUP BY lower(name) HAVING count(*) = 1);
CREATE UNIQUE INDEX teams_name_key ON teams (name_key);
