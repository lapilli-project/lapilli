-- A task's id is never another's again, a deleted task's included
-- (AUTOINCREMENT), as 015 made comments' ids. The tracker jobs find a
-- task's issue by its id (lapilli-task-ID): a new task given a deleted
-- one's id took over its issue, its comments both ways and its close.
-- SQLite adds AUTOINCREMENT only to a table it makes, so the table is made
-- again, its rows and ids as they were.
CREATE TABLE tasks_016 (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
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
	updated_by TEXT NOT NULL DEFAULT '',
	link TEXT NOT NULL DEFAULT '',
	overdue_told INTEGER,
	done_at INTEGER
);
INSERT INTO tasks_016 (id, title, kind, rule, incident, team_id, assignee, status, due, body, created, created_by,
		updated, updated_by, link, overdue_told, done_at)
	SELECT id, title, kind, rule, incident, team_id, assignee, status, due, body, created, created_by,
		updated, updated_by, link, overdue_told, done_at FROM tasks;
DROP TABLE tasks;
ALTER TABLE tasks_016 RENAME TO tasks;
CREATE INDEX tasks_open ON tasks (status, updated);
CREATE INDEX tasks_rule ON tasks (rule);
CREATE INDEX tasks_incident ON tasks (incident);

-- The ids given before and deleted since, past the highest kept, are still
-- some issue's: the next id comes after the highest the changes noted of a
-- task (task 7 made, task 7 deleted), or a proposal was made into.
INSERT INTO sqlite_sequence (name, seq)
	SELECT 'tasks', 0 WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = 'tasks');
UPDATE sqlite_sequence SET seq = max(seq,
		coalesce((SELECT max(CAST(substr(what, 6) AS INTEGER)) FROM changes WHERE what GLOB 'task [0-9]* *'), 0),
		coalesce((SELECT max(task_id) FROM proposals), 0))
	WHERE name = 'tasks';

-- A proposal made into a task deleted since is made into none.
UPDATE proposals SET task_id = NULL WHERE task_id IS NOT NULL AND task_id NOT IN (SELECT id FROM tasks);
