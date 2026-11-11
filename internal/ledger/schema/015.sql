-- What comes in (docs/design/inputs.md), after 0.25's 014: the files
-- are applied in their order.
--
-- The turns of on-call schedules read from their feeds (--on-call), kept
-- past what a feed holds: a read replaces the schedule's turns that end
-- after the earliest start the feed holds, and keeps those before it. who
-- is the person a turn names, as the file names them, or as the feed does
-- for someone not in it.
CREATE TABLE on_call_turns (
	schedule TEXT NOT NULL,
	start_at INTEGER NOT NULL,
	end_at INTEGER NOT NULL,
	who TEXT NOT NULL,
	PRIMARY KEY (schedule, start_at)
);

-- Comments kept in step with a tracker: origin is '' for people's, tracker
-- for one copied in from the task's issue by the tasks token; ref is the
-- tracker's id for it, kept once a task; author who wrote it, as the
-- tracker names them. A comment's id is never another's again, one
-- deleted's included (AUTOINCREMENT): a tracker job finds a comment it
-- posted by the id, and a new comment with an old one's would be taken
-- for posted. SQLite adds that only to a table it makes, so the table is
-- made again, its rows and ids as they were.
CREATE TABLE comments_015 (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	on_kind TEXT NOT NULL,
	on_key TEXT NOT NULL,
	body TEXT NOT NULL,
	created INTEGER NOT NULL,
	created_by TEXT NOT NULL DEFAULT '',
	updated INTEGER NOT NULL,
	origin TEXT NOT NULL DEFAULT '',
	ref TEXT NOT NULL DEFAULT '',
	author TEXT NOT NULL DEFAULT ''
);
INSERT INTO comments_015 (id, on_kind, on_key, body, created, created_by, updated)
	SELECT id, on_kind, on_key, body, created, created_by, updated FROM comments;
DROP TABLE comments;
ALTER TABLE comments_015 RENAME TO comments;
CREATE INDEX comments_on ON comments (on_kind, on_key, created);
CREATE UNIQUE INDEX comments_ref ON comments (on_kind, on_key, ref) WHERE ref != '';
