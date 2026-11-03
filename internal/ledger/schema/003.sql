-- What people keep in the ledger beside what the runs find: who owns what,
-- when firings are expected, what is to be done, and what was said.

-- A team owns the rules whose name matches rules, or whose labels have the
-- values in labels; and the incidents with those values.
CREATE TABLE teams (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	rules TEXT NOT NULL DEFAULT '',
	labels TEXT NOT NULL DEFAULT '{}',
	note TEXT NOT NULL DEFAULT '',
	updated INTEGER NOT NULL,
	updated_by TEXT NOT NULL DEFAULT ''
);

-- What people keep about a rule, by its name: the team that owns it, if
-- not by the teams' matchers; what is to become of it; its runbook.
CREATE TABLE notes (
	rule TEXT PRIMARY KEY,
	team_id INTEGER REFERENCES teams (id) ON DELETE SET NULL,
	status TEXT NOT NULL DEFAULT '',
	runbook TEXT NOT NULL DEFAULT '',
	note TEXT NOT NULL DEFAULT '',
	updated INTEGER NOT NULL,
	updated_by TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;

-- A quiet window: a deploy, maintenance, a load test. A firing that starts
-- in one and matches it, and catches nothing, is excused: neither white
-- nor black.
CREATE TABLE windows (
	id INTEGER PRIMARY KEY,
	title TEXT NOT NULL,
	start_at INTEGER NOT NULL,
	end_at INTEGER NOT NULL,
	rules TEXT NOT NULL DEFAULT '',
	labels TEXT NOT NULL DEFAULT '{}',
	updated INTEGER NOT NULL,
	updated_by TEXT NOT NULL DEFAULT ''
);
CREATE INDEX windows_start ON windows (start_at);

ALTER TABLE firings ADD COLUMN excused INTEGER NOT NULL DEFAULT 0;
ALTER TABLE days ADD COLUMN excused INTEGER NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN excused INTEGER NOT NULL DEFAULT 0;

-- A follow-up: something someone is to do about a rule or an incident.
CREATE TABLE tasks (
	id INTEGER PRIMARY KEY,
	title TEXT NOT NULL,
	kind TEXT NOT NULL DEFAULT '',
	rule TEXT NOT NULL DEFAULT '',
	incident TEXT NOT NULL DEFAULT '',
	team_id INTEGER REFERENCES teams (id) ON DELETE SET NULL,
	assignee TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'open',
	due INTEGER,
	body TEXT NOT NULL DEFAULT '',
	created INTEGER NOT NULL,
	created_by TEXT NOT NULL DEFAULT '',
	updated INTEGER NOT NULL,
	updated_by TEXT NOT NULL DEFAULT ''
);
CREATE INDEX tasks_open ON tasks (status, updated);
CREATE INDEX tasks_rule ON tasks (rule);
CREATE INDEX tasks_incident ON tasks (incident);

-- Comments on a rule, an incident or a task.
CREATE TABLE comments (
	id INTEGER PRIMARY KEY,
	on_kind TEXT NOT NULL,
	on_key TEXT NOT NULL,
	body TEXT NOT NULL,
	created INTEGER NOT NULL,
	created_by TEXT NOT NULL DEFAULT '',
	updated INTEGER NOT NULL
);
CREATE INDEX comments_on ON comments (on_kind, on_key, created);
