-- Times are Unix seconds. A day is a date in the ledger's time zone, as days
-- since 1970-01-01; a week is the day its Monday falls on.

CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID;

-- The documents rules were read from, gzipped, by their SHA-256: a file
-- that didn't change between versions is kept once.
CREATE TABLE docs (hash TEXT PRIMARY KEY, data BLOB NOT NULL) WITHOUT ROWID;

-- A version of the rules is in effect from since until the next one's;
-- since 0 is from before the ledger began. docs lists its documents' sources
-- and hashes; names, the name each rule they hold is judged under.
CREATE TABLE versions (
	id INTEGER PRIMARY KEY,
	since INTEGER NOT NULL,
	digest TEXT NOT NULL,
	docs TEXT NOT NULL,
	names TEXT NOT NULL
);
CREATE INDEX versions_since ON versions (since);

CREATE TABLE rules (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	grp TEXT NOT NULL DEFAULT '',
	severity TEXT NOT NULL DEFAULT '',
	unsupported TEXT NOT NULL DEFAULT ''
);

-- One alert: a rule and a label set.
CREATE TABLE series (
	id INTEGER PRIMARY KEY,
	rule_id INTEGER NOT NULL REFERENCES rules (id),
	labels TEXT NOT NULL,
	UNIQUE (rule_id, labels)
);

-- end_at is NULL while the alert is still firing. white and refired are its
-- stones, given against every incident the ledger has.
CREATE TABLE firings (
	id INTEGER PRIMARY KEY,
	series_id INTEGER NOT NULL REFERENCES series (id),
	rule_id INTEGER NOT NULL,
	start_at INTEGER NOT NULL,
	end_at INTEGER,
	day INTEGER NOT NULL,
	white INTEGER NOT NULL DEFAULT 0,
	refired INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX firings_start ON firings (start_at);
CREATE INDEX firings_rule ON firings (rule_id, start_at);
CREATE INDEX firings_series ON firings (series_id, start_at);

-- origin is file, ui or impact. An impact window's state is '' until it is
-- confirmed or dismissed; counts says whether it is judged against, which
-- it isn't once dismissed or confirmed, or while an incident written down
-- covers it.
CREATE TABLE incidents (
	id INTEGER PRIMARY KEY,
	key TEXT NOT NULL UNIQUE,
	origin TEXT NOT NULL,
	state TEXT NOT NULL DEFAULT '',
	counts INTEGER NOT NULL DEFAULT 1,
	title TEXT NOT NULL DEFAULT '',
	start_at INTEGER NOT NULL,
	end_at INTEGER,
	labels TEXT NOT NULL DEFAULT '{}',
	updated INTEGER NOT NULL,
	updated_by TEXT NOT NULL DEFAULT ''
);
CREATE INDEX incidents_start ON incidents (start_at);

-- Each rule's first firing that caught an incident, as seconds from its
-- start; negative when the alert came first.
CREATE TABLE catches (
	incident_id INTEGER NOT NULL,
	rule_id INTEGER NOT NULL,
	delay INTEGER NOT NULL,
	PRIMARY KEY (incident_id, rule_id)
) WITHOUT ROWID;
CREATE INDEX catches_rule ON catches (rule_id);

-- Each rule's stones by the day and the week its firings started.
CREATE TABLE days (
	day INTEGER NOT NULL,
	rule_id INTEGER NOT NULL,
	firings INTEGER NOT NULL,
	white INTEGER NOT NULL,
	refired INTEGER NOT NULL,
	PRIMARY KEY (day, rule_id)
) WITHOUT ROWID;
CREATE TABLE weeks (
	week INTEGER NOT NULL,
	rule_id INTEGER NOT NULL,
	firings INTEGER NOT NULL,
	white INTEGER NOT NULL,
	refired INTEGER NOT NULL,
	PRIMARY KEY (week, rule_id)
) WITHOUT ROWID;

CREATE TABLE runs (
	id INTEGER PRIMARY KEY,
	kind TEXT NOT NULL,
	started INTEGER NOT NULL,
	finished INTEGER NOT NULL,
	period_from INTEGER NOT NULL DEFAULT 0,
	period_to INTEGER NOT NULL DEFAULT 0,
	firings INTEGER NOT NULL DEFAULT 0,
	failed TEXT NOT NULL DEFAULT '{}',
	error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX runs_started ON runs (started);

CREATE TABLE settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL,
	updated INTEGER NOT NULL,
	updated_by TEXT NOT NULL
) WITHOUT ROWID;

-- Every change made through the UI or API: who, when, before and after.
CREATE TABLE changes (
	id INTEGER PRIMARY KEY,
	at INTEGER NOT NULL,
	by TEXT NOT NULL,
	what TEXT NOT NULL,
	before TEXT NOT NULL DEFAULT '',
	after TEXT NOT NULL DEFAULT ''
);
CREATE INDEX changes_at ON changes (at);

-- What needed Prometheus to work out: suggestions, change reviews. body is
-- gzipped JSON.
CREATE TABLE results (
	id INTEGER PRIMARY KEY,
	kind TEXT NOT NULL,
	at INTEGER NOT NULL,
	period_from INTEGER NOT NULL,
	period_to INTEGER NOT NULL,
	body BLOB NOT NULL
);
CREATE INDEX results_kind ON results (kind, at);
