-- Impact signals, each named, read, started and judged on its own
-- (docs/design/signals.md), after marked windows' 021.
--
-- impact_signals are the signals the runs read: name, unique, '' for the
-- unnamed signal (--impact PROMQL); bit, its bit in series.signals, 0 to
-- 7, each named signal's its own (impact_signals_bit), unused of the
-- unnamed signal, which ties no alert; expr, for_ns,
-- match_labels (a JSON list, NULL for the match in effect, [] for none)
-- and keeps_record as they were started with; set_by and set_at, who set
-- it in the UI and when (none for the command line's or the file's). Then
-- what keeps the record by it, as meta kept it for the one signal before
-- (docs/design/serve.md, "The impact criterion"): since, criterion,
-- again, data, last_read and floor_at, each as its meta key's value was;
-- seen, what it has returned and read, by its match labels (JSON), and
-- seen_to, how far its selectors were read for it; missed_at, from when a run couldn't read it, 0 for none.
CREATE TABLE impact_signals (
	name TEXT PRIMARY KEY,
	bit INTEGER NOT NULL DEFAULT 0,
	expr TEXT NOT NULL DEFAULT '',
	for_ns INTEGER NOT NULL DEFAULT 0,
	match_labels TEXT,
	keeps_record INTEGER NOT NULL DEFAULT 1,
	set_by TEXT NOT NULL DEFAULT '',
	set_at INTEGER NOT NULL DEFAULT 0,
	since TEXT NOT NULL DEFAULT '',
	criterion TEXT NOT NULL DEFAULT '',
	again TEXT NOT NULL DEFAULT '',
	data TEXT NOT NULL DEFAULT '',
	last_read TEXT NOT NULL DEFAULT '',
	floor_at TEXT NOT NULL DEFAULT '',
	seen TEXT NOT NULL DEFAULT '',
	seen_to INTEGER NOT NULL DEFAULT 0,
	missed_at INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;

-- No two named signals share a bit: a series' bits are each one's alone.
CREATE UNIQUE INDEX impact_signals_bit ON impact_signals (bit) WHERE name != '';

-- The one signal before names: what meta kept of it, moved to its row.
INSERT INTO impact_signals (name, since, criterion, again, data, last_read, floor_at)
SELECT '',
	coalesce((SELECT value FROM meta WHERE key = 'impact_since'), ''),
	coalesce((SELECT value FROM meta WHERE key = 'impact_criterion'), ''),
	coalesce((SELECT value FROM meta WHERE key = 'impact_again'), ''),
	coalesce((SELECT value FROM meta WHERE key = 'impact_data'), ''),
	coalesce((SELECT value FROM meta WHERE key = 'impact_read'), ''),
	coalesce((SELECT value FROM meta WHERE key = 'impact_floor'), '')
WHERE EXISTS (SELECT 1 FROM meta WHERE key IN ('impact_since', 'impact_criterion', 'impact_again', 'impact_data', 'impact_read', 'impact_floor'));
DELETE FROM meta WHERE key IN ('impact_since', 'impact_criterion', 'impact_again', 'impact_data', 'impact_read', 'impact_floor');

-- The rule entries in effect, in order (pos from 1): pattern, a Go regular
-- expression of the rules' names, '' for none; labels, the values a rule's
-- labels must have (JSON); signals, the signals its rules are judged
-- against (a JSON list); match_labels, the labels they are matched on
-- against those signals' windows, NULL for each signal's own; ui, whether
-- the UI set them.
CREATE TABLE impact_rules (
	pos INTEGER PRIMARY KEY,
	pattern TEXT NOT NULL DEFAULT '',
	labels TEXT NOT NULL DEFAULT '{}',
	signals TEXT NOT NULL,
	match_labels TEXT,
	ui INTEGER NOT NULL DEFAULT 0
);

-- An impact window's signal, '' for the unnamed signal's and for every
-- incident written down but a confirmation's, which carries its window's;
-- and the labels it is matched on, NULL for the match in effect.
ALTER TABLE incidents ADD COLUMN signal TEXT NOT NULL DEFAULT '';
ALTER TABLE incidents ADD COLUMN match_labels TEXT;

-- The signals each alert is tied to, a bit each (impact_signals.bit), and
-- 256 when none it was tied to by its labels keeps the record, so every
-- one that does was taken too; 0 for none, every alert's while no signal
-- has a name; -1 while it waits to be worked out, in the write that made
-- it.
ALTER TABLE series ADD COLUMN signals INTEGER NOT NULL DEFAULT 0;
CREATE INDEX series_untied ON series (id) WHERE signals < 0;

-- The tallies, keyed by the signals the firings' alerts are tied to as
-- well, so that a rule's firings are judged apart by the signals they wait
-- for: each row copied in with 0, the one set every alert has before
-- signals are named.
CREATE TABLE days_022 (
	day INTEGER NOT NULL,
	rule_id INTEGER NOT NULL,
	firings INTEGER NOT NULL,
	white INTEGER NOT NULL,
	refired INTEGER NOT NULL,
	excused INTEGER NOT NULL DEFAULT 0,
	deploy INTEGER NOT NULL DEFAULT 0,
	deploy_white INTEGER NOT NULL DEFAULT 0,
	signals INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (day, rule_id, signals)
) WITHOUT ROWID;
INSERT INTO days_022 (day, rule_id, firings, white, refired, excused, deploy, deploy_white, signals)
	SELECT day, rule_id, firings, white, refired, excused, deploy, deploy_white, 0 FROM days;
DROP TABLE days;
ALTER TABLE days_022 RENAME TO days;
CREATE INDEX days_rule ON days (rule_id, day);

CREATE TABLE weeks_022 (
	week INTEGER NOT NULL,
	rule_id INTEGER NOT NULL,
	firings INTEGER NOT NULL,
	white INTEGER NOT NULL,
	refired INTEGER NOT NULL,
	excused INTEGER NOT NULL DEFAULT 0,
	deploy INTEGER NOT NULL DEFAULT 0,
	deploy_white INTEGER NOT NULL DEFAULT 0,
	signals INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (week, rule_id, signals)
) WITHOUT ROWID;
INSERT INTO weeks_022 (week, rule_id, firings, white, refired, excused, deploy, deploy_white, signals)
	SELECT week, rule_id, firings, white, refired, excused, deploy, deploy_white, 0 FROM weeks;
DROP TABLE weeks;
ALTER TABLE weeks_022 RENAME TO weeks;

CREATE TABLE marked_days_022 (
	day INTEGER NOT NULL,
	rule_id INTEGER NOT NULL,
	kind_id INTEGER NOT NULL,
	firings INTEGER NOT NULL,
	white INTEGER NOT NULL,
	refired INTEGER NOT NULL,
	signals INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (day, rule_id, kind_id, signals)
);
INSERT INTO marked_days_022 (day, rule_id, kind_id, firings, white, refired, signals)
	SELECT day, rule_id, kind_id, firings, white, refired, 0 FROM marked_days;
DROP TABLE marked_days;
ALTER TABLE marked_days_022 RENAME TO marked_days;

CREATE TABLE marked_weeks_022 (
	week INTEGER NOT NULL,
	rule_id INTEGER NOT NULL,
	kind_id INTEGER NOT NULL,
	firings INTEGER NOT NULL,
	white INTEGER NOT NULL,
	refired INTEGER NOT NULL,
	signals INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (week, rule_id, kind_id, signals)
);
INSERT INTO marked_weeks_022 (week, rule_id, kind_id, firings, white, refired, signals)
	SELECT week, rule_id, kind_id, firings, white, refired, 0 FROM marked_weeks;
DROP TABLE marked_weeks;
ALTER TABLE marked_weeks_022 RENAME TO marked_weeks;
