-- Goals (docs/design/goals.md), after 019.
--
-- What an operations team holds its alerting to, as --config declares it
-- under goals:, one row a goal, matched to the file by its name, which is
-- unique across everyone's goals and every team's. Kept to the file by the
-- leader (ledger.Reconcile), for every replica to read: a goal the file no
-- longer declares is deleted, with or without --config-prune, and without
-- --config there are none. Their results aren't kept: each week's is
-- worked out from the summary's numbers, as every number Lapilli shows is.
--
-- team_id is the goal's team, 0 for everyone's; every_team = 1 is a goal
-- of team: "*", each team's own, checked for each team (team_id 0). The
-- bound is one of at_least, at_most and no_worse_than, the others ''; a
-- number is kept as the file wrote it, as text, so that both databases
-- keep it alike. slack is the file's by, '' when left out. critical is the
-- severities counted as critical, a JSON list, [] for the default. each is
-- week, or a kind of marked window. declared is 1: no goal is made in the
-- UI.
CREATE TABLE goals (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL,
	team_id INTEGER NOT NULL DEFAULT 0,
	every_team INTEGER NOT NULL DEFAULT 0,
	measure TEXT NOT NULL,
	at_least TEXT NOT NULL DEFAULT '',
	at_most TEXT NOT NULL DEFAULT '',
	no_worse_than TEXT NOT NULL DEFAULT '',
	slack TEXT NOT NULL DEFAULT '',
	critical TEXT NOT NULL DEFAULT '[]',
	each TEXT NOT NULL DEFAULT 'week',
	declared INTEGER NOT NULL DEFAULT 1,
	updated INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX goals_name ON goals (name);

-- The goal a task is about, by its name, '' for none: set as the task is
-- made, and kept as text when the goal is no longer declared.
ALTER TABLE tasks ADD COLUMN goal TEXT NOT NULL DEFAULT '';
CREATE INDEX tasks_goal ON tasks (goal, status) WHERE goal != '';
