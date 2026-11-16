-- Marked windows (docs/design/marked.md), after goals' 020.
--
-- A marked window is a span of time of a named kind (a sale, a launch, a
-- campaign) that marks the firings and incidents that came during it and
-- changes nothing else: no stone, no excuse, no record. Every count can be
-- read inside each kind and outside it.
--
-- marked_kinds are the kinds, each named once: an id from 1 to 15, the bit
-- 1 << id on firings.marked (deploy is firings.deploy, and week is
-- goals.md's, so neither is one). A kind is kept while a window names it,
-- or an expression (meta 'marked-expr.KIND'); one no longer named is let
-- go once its windows' spans are judged again, and its id can be given to
-- another.
CREATE TABLE marked_kinds (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE
);

-- marked are the windows, from a file (--marked), --config, the UI, or a
-- PromQL expression (--mark): origin is file, config, ui or expr. A window
-- repeats as a quiet window does (every, repeat_on, until_at), and each
-- of its repeats is an occurrence, worked out from it, never kept. labels
-- are matched as a deploy's are: a firing agrees on every label both
-- have, one at least; none marks everything. team_id, as a quiet
-- window's: a team's window marks its team's rules alone. declared and
-- given_back as windows has them (019).
CREATE TABLE marked (
	id INTEGER PRIMARY KEY,
	kind_id INTEGER NOT NULL REFERENCES marked_kinds (id),
	origin TEXT NOT NULL,
	title TEXT NOT NULL,
	start_at INTEGER NOT NULL,
	end_at INTEGER NOT NULL,
	every TEXT NOT NULL DEFAULT '',
	repeat_on TEXT NOT NULL DEFAULT '',
	until_at INTEGER,
	team_id INTEGER NOT NULL DEFAULT 0,
	labels TEXT NOT NULL DEFAULT '{}',
	declared INTEGER NOT NULL DEFAULT 0,
	given_back INTEGER NOT NULL DEFAULT 0,
	updated INTEGER NOT NULL,
	updated_by TEXT NOT NULL DEFAULT ''
);
CREATE INDEX marked_start ON marked (start_at);
CREATE INDEX marked_every ON marked (every, start_at);

-- The kinds a firing is marked with, a bit each: 0, nothing, for every
-- firing kept before.
ALTER TABLE firings ADD COLUMN marked INTEGER NOT NULL DEFAULT 0;

-- By day (and week) and rule and kind, the firings marked with the kind,
-- those of them that caught something, and those that re-fired: counted
-- with days and weeks, in the same tally. Outside a kind is days less
-- these: nothing is kept for it.
CREATE TABLE marked_days (
	day INTEGER NOT NULL,
	rule_id INTEGER NOT NULL,
	kind_id INTEGER NOT NULL,
	firings INTEGER NOT NULL,
	white INTEGER NOT NULL,
	refired INTEGER NOT NULL,
	PRIMARY KEY (day, rule_id, kind_id)
);
CREATE TABLE marked_weeks (
	week INTEGER NOT NULL,
	rule_id INTEGER NOT NULL,
	kind_id INTEGER NOT NULL,
	firings INTEGER NOT NULL,
	white INTEGER NOT NULL,
	refired INTEGER NOT NULL,
	PRIMARY KEY (week, rule_id, kind_id)
);
