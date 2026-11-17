-- Impact signals (docs/design/signals.md), as SQLite's 022.sql: what each
-- table and column is for is said there.
CREATE TABLE impact_signals (
	name TEXT COLLATE "C" PRIMARY KEY,
	bit BIGINT NOT NULL DEFAULT 0,
	expr TEXT COLLATE "C" NOT NULL DEFAULT '',
	for_ns BIGINT NOT NULL DEFAULT 0,
	match_labels TEXT COLLATE "C",
	keeps_record BIGINT NOT NULL DEFAULT 1,
	set_by TEXT COLLATE "C" NOT NULL DEFAULT '',
	set_at BIGINT NOT NULL DEFAULT 0,
	since TEXT COLLATE "C" NOT NULL DEFAULT '',
	criterion TEXT COLLATE "C" NOT NULL DEFAULT '',
	again TEXT COLLATE "C" NOT NULL DEFAULT '',
	data TEXT COLLATE "C" NOT NULL DEFAULT '',
	last_read TEXT COLLATE "C" NOT NULL DEFAULT '',
	floor_at TEXT COLLATE "C" NOT NULL DEFAULT '',
	seen TEXT COLLATE "C" NOT NULL DEFAULT '',
	seen_to BIGINT NOT NULL DEFAULT 0,
	missed_at BIGINT NOT NULL DEFAULT 0
);

-- No two named signals share a bit: a series' bits are each one's alone.
CREATE UNIQUE INDEX impact_signals_bit ON impact_signals (bit) WHERE name != '';

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

CREATE TABLE impact_rules (
	pos BIGINT PRIMARY KEY,
	pattern TEXT COLLATE "C" NOT NULL DEFAULT '',
	labels TEXT COLLATE "C" NOT NULL DEFAULT '{}',
	signals TEXT COLLATE "C" NOT NULL,
	match_labels TEXT COLLATE "C",
	ui BIGINT NOT NULL DEFAULT 0
);

ALTER TABLE incidents ADD COLUMN signal TEXT COLLATE "C" NOT NULL DEFAULT '';
ALTER TABLE incidents ADD COLUMN match_labels TEXT COLLATE "C";

ALTER TABLE series ADD COLUMN signals BIGINT NOT NULL DEFAULT 0;
CREATE INDEX series_untied ON series (id) WHERE signals < 0;

-- The tallies' keys, with signals: every row 0, the one set every alert
-- has before signals are named.
ALTER TABLE days ADD COLUMN signals BIGINT NOT NULL DEFAULT 0;
ALTER TABLE days DROP CONSTRAINT days_pkey;
ALTER TABLE days ADD PRIMARY KEY (day, rule_id, signals);
ALTER TABLE weeks ADD COLUMN signals BIGINT NOT NULL DEFAULT 0;
ALTER TABLE weeks DROP CONSTRAINT weeks_pkey;
ALTER TABLE weeks ADD PRIMARY KEY (week, rule_id, signals);
ALTER TABLE marked_days ADD COLUMN signals BIGINT NOT NULL DEFAULT 0;
ALTER TABLE marked_days DROP CONSTRAINT marked_days_pkey;
ALTER TABLE marked_days ADD PRIMARY KEY (day, rule_id, kind_id, signals);
ALTER TABLE marked_weeks ADD COLUMN signals BIGINT NOT NULL DEFAULT 0;
ALTER TABLE marked_weeks DROP CONSTRAINT marked_weeks_pkey;
ALTER TABLE marked_weeks ADD PRIMARY KEY (week, rule_id, kind_id, signals);
