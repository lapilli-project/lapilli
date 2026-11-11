-- What comes in (docs/design/inputs.md). Numbered 015: 014 is another
-- change's, made at the same time.
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
-- tracker names them.
ALTER TABLE comments ADD COLUMN origin TEXT NOT NULL DEFAULT '';
ALTER TABLE comments ADD COLUMN ref TEXT NOT NULL DEFAULT '';
ALTER TABLE comments ADD COLUMN author TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX comments_ref ON comments (on_kind, on_key, ref) WHERE ref != '';
