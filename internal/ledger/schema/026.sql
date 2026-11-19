-- A person's verdict on one firing, and rules judged by nothing
-- (docs/design/judging.md, item 3), after episodes' 025.
--
-- verdicts: a row a firing a person said something of, kept by its alert
-- and start (series_id, start_at), not by the firing's id, which a run
-- gives again; end_at is the firing's end when it was said, NULL while it
-- was on, which bounds the firing the verdict finds again when a run or a
-- replay moves it. unrelated is a JSON list of the incidents it was said
-- to be unrelated to, each {key, start, end} as it was then, '[]' for
-- none; action is '', yes or no. Each part keeps who said it, when, and a
-- note of its own (500 characters at most, one line). moved is '', or why
-- the verdict is to be looked at again: its firing moved, another verdict
-- holds the firing it found, an incident's span changed, or it catches
-- one it wasn't said to be unrelated to.
CREATE TABLE verdicts (
	id INTEGER PRIMARY KEY,
	series_id INTEGER NOT NULL REFERENCES series (id),
	start_at INTEGER NOT NULL,
	end_at INTEGER,
	unrelated TEXT NOT NULL DEFAULT '[]',
	unrelated_by TEXT NOT NULL DEFAULT '',
	unrelated_at INTEGER NOT NULL DEFAULT 0,
	unrelated_note TEXT NOT NULL DEFAULT '',
	action TEXT NOT NULL DEFAULT '',
	action_by TEXT NOT NULL DEFAULT '',
	action_at INTEGER NOT NULL DEFAULT 0,
	action_note TEXT NOT NULL DEFAULT '',
	moved TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX verdicts_firing ON verdicts (series_id, start_at);
CREATE INDEX verdicts_start ON verdicts (start_at);

-- person on firings: bits judging writes from the verdicts and the
-- incidents, for the tallies: 1, unrelated (white as Lapilli judged it,
-- black by a person's word); 2, a person said it needed action; 4, that
-- it didn't; 8, white only by incidents "This was real" wrote (black as
-- Lapilli judged it, white by people's word); 16, of those, in a quiet
-- window that matches it, which would have excused it. As Lapilli judged
-- it, a firing is white when it caught an incident "This was real"
-- didn't write, whatever a person said it is unrelated to.
-- unrelated on firings: the keys of the incidents it catches as Lapilli
-- judges it that its verdicts say it is unrelated to, a JSON list, '' for
-- none: what the catches and the reports read.
ALTER TABLE firings ADD COLUMN person INTEGER NOT NULL DEFAULT 0;
ALTER TABLE firings ADD COLUMN unrelated TEXT NOT NULL DEFAULT '';

-- The tallies of the bits, by day and week: unrelated (bit 1, excused or
-- not) and unrelated_excused (of them, those a quiet window then
-- excused); real_white (bit 8) and real_excused (bit 16); action_yes and
-- action_no, of the firings not excused, and of them action_yes_black
-- (needed action, and caught nothing) and action_no_white (needed none,
-- and caught something).
ALTER TABLE days ADD COLUMN unrelated INTEGER NOT NULL DEFAULT 0;
ALTER TABLE days ADD COLUMN unrelated_excused INTEGER NOT NULL DEFAULT 0;
ALTER TABLE days ADD COLUMN real_white INTEGER NOT NULL DEFAULT 0;
ALTER TABLE days ADD COLUMN real_excused INTEGER NOT NULL DEFAULT 0;
ALTER TABLE days ADD COLUMN action_yes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE days ADD COLUMN action_no INTEGER NOT NULL DEFAULT 0;
ALTER TABLE days ADD COLUMN action_yes_black INTEGER NOT NULL DEFAULT 0;
ALTER TABLE days ADD COLUMN action_no_white INTEGER NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN unrelated INTEGER NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN unrelated_excused INTEGER NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN real_white INTEGER NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN real_excused INTEGER NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN action_yes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN action_no INTEGER NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN action_yes_black INTEGER NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN action_no_white INTEGER NOT NULL DEFAULT 0;

-- real_of on incidents: the alert and start "This was real" wrote it from,
-- as 'SERIES START' (the series' id and the start's Unix time), '' for any
-- other; one written so before this schema is '', and counted as none.
ALTER TABLE incidents ADD COLUMN real_of TEXT NOT NULL DEFAULT '';

-- judged on impact_rules: reserved, and unused: nothing writes or reads
-- it, and it is NULL in every row. Whether an entry's rules are judged by
-- nothing (judged: false) is read from where the entries are given, as
-- the rest of an entry is.
ALTER TABLE impact_rules ADD COLUMN judged INTEGER;

-- stones on goals: 'lapilli' for a goal checked against the numbers as
-- Lapilli judged them, people's verdicts and the incidents "This was
-- real" wrote left out; '' for the stones as they are.
ALTER TABLE goals ADD COLUMN stones TEXT NOT NULL DEFAULT '';
