-- The replay beside what really fired (docs/design/fidelity.md): for each
-- rule and day, how many firings the replay had, how many really fired,
-- how many were alike, and how far apart the alike ones started, all told
-- (off_ms) and how many (alike). A day is written once its firings are
-- settled, and only then. alert is the name what really fired goes by;
-- rules that share it are counted together, and shared says so: what
-- really fired doesn't tell them apart.
CREATE TABLE checked (
	rule TEXT NOT NULL,
	day INTEGER NOT NULL,
	alert TEXT NOT NULL,
	shared INTEGER NOT NULL DEFAULT 0,
	replayed INTEGER NOT NULL,
	real INTEGER NOT NULL,
	alike INTEGER NOT NULL,
	off_ms INTEGER NOT NULL,
	PRIMARY KEY (rule, day)
) WITHOUT ROWID;
CREATE INDEX checked_day ON checked (day);

-- The last firings found only one way, for each rule: way is replayed or
-- real.
CREATE TABLE check_misses (
	id INTEGER PRIMARY KEY,
	rule TEXT NOT NULL,
	way TEXT NOT NULL,
	start_at INTEGER NOT NULL,
	labels TEXT NOT NULL
);
CREATE INDEX check_misses_rule ON check_misses (rule, start_at);

-- Alerts that really fired that no rule given is named, by day.
CREATE TABLE check_elsewhere (
	name TEXT NOT NULL,
	day INTEGER NOT NULL,
	firings INTEGER NOT NULL,
	PRIMARY KEY (name, day)
) WITHOUT ROWID;
