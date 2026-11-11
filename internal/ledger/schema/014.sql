-- serve run by the people who use it (docs/design/serveops.md).

-- Roles given in the UI, on top of the flags: who, as the sign-in proxy
-- names them (user:NAME or group:NAME), is an editor of every team
-- (team_id 0) or of one. Only an editor's role is ever kept here: admin and
-- approver are the flags' alone.
CREATE TABLE grants (
	id INTEGER PRIMARY KEY,
	who TEXT NOT NULL,
	role TEXT NOT NULL DEFAULT 'editor' CHECK (role = 'editor'),
	team_id INTEGER NOT NULL DEFAULT 0,
	created INTEGER NOT NULL,
	created_by TEXT NOT NULL DEFAULT '',
	UNIQUE (who, team_id)
);

-- A quiet window can be a team's, and then excuses only the firings of
-- rules the team owns; 0 for everyone's, a team removed's negated. It can
-- repeat every month (repeat_on: a day, last, or a weekday of the month,
-- such as first sat) or by a cron expression (repeat_on: its five fields).
ALTER TABLE windows ADD COLUMN team_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE windows ADD COLUMN repeat_on TEXT NOT NULL DEFAULT '';
