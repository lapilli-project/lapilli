-- Declared configuration (docs/design/serveops.md), after 0.43's 018.
--
-- A team, a quiet window, a report or a role that --config declares is
-- kept with declared = 1: the UI and the API show it and refuse to change
-- or delete it; the file alone does. One the file no longer names is the
-- UI's again (declared = 0), or deleted with --config-prune.
ALTER TABLE teams ADD COLUMN declared INTEGER NOT NULL DEFAULT 0;
ALTER TABLE windows ADD COLUMN declared INTEGER NOT NULL DEFAULT 0;
ALTER TABLE reports ADD COLUMN declared INTEGER NOT NULL DEFAULT 0;
ALTER TABLE grants ADD COLUMN declared INTEGER NOT NULL DEFAULT 0;

-- given_back = 1 is one a file declared that a leader without --config
-- gave back to the UI (declared = 0): until the UI changes it, the next
-- file that doesn't declare it again leaves it as one it no longer
-- declares, a role taken away, so that a rollback past --config and
-- forward again keeps no role the file took out.
ALTER TABLE teams ADD COLUMN given_back INTEGER NOT NULL DEFAULT 0;
ALTER TABLE windows ADD COLUMN given_back INTEGER NOT NULL DEFAULT 0;
ALTER TABLE reports ADD COLUMN given_back INTEGER NOT NULL DEFAULT 0;
ALTER TABLE grants ADD COLUMN given_back INTEGER NOT NULL DEFAULT 0;

-- A team's name is unique in any letter case: name_key is its name in
-- lower case (ASCII letters alone, as SQLite's lower() and Postgres's
-- under COLLATE "C" both fold them), kept unique. Lapilli checks every new
-- and renamed team's name in full Unicode case folding before it writes,
-- and fills name_key as it does. A ledger may hold teams already whose
-- names differ only in letter case (Payments, payments): this migration
-- doesn't fail on them, nor choose one. Their name_key stays NULL, which a
-- unique index takes any number of; the Teams page lists them to be
-- renamed, and each is given its key once no other team's name is like
-- its own.
ALTER TABLE teams ADD COLUMN name_key TEXT;
UPDATE teams SET name_key = lower(name)
	WHERE lower(name) IN (SELECT lower(name) FROM teams GROUP BY lower(name) HAVING count(*) = 1);
CREATE UNIQUE INDEX teams_name_fold ON teams (name_key);
