-- Reports people schedule themselves (docs/design/reports.md). schedule is
-- the preset or cron text as given; span, a cron expression's span in
-- seconds, 0 for the time since the one before; zone, ledger or team;
-- sections and dests, JSON lists. When each was last sent, and for which
-- span, is kept in meta, as TellOnce keeps it.
CREATE TABLE reports (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	team_id INTEGER NOT NULL DEFAULT 0,
	schedule TEXT NOT NULL,
	span INTEGER NOT NULL DEFAULT 0,
	zone TEXT NOT NULL DEFAULT 'ledger',
	sections TEXT NOT NULL DEFAULT '[]',
	dests TEXT NOT NULL DEFAULT '[]',
	enabled INTEGER NOT NULL DEFAULT 1,
	created INTEGER NOT NULL,
	created_by TEXT NOT NULL DEFAULT '',
	updated INTEGER NOT NULL,
	updated_by TEXT NOT NULL DEFAULT ''
);
