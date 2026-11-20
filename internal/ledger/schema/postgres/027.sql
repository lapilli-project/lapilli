-- What checking each rule's expectations found (docs/design/expectations.md,
-- step 2), as SQLite's 027.sql: what each column is for is said there. A
-- rule's name can be longer than a btree holds: SQLite's PRIMARY KEY
-- (rule, expect, week) is a unique index on (md5(rule), expect, week), as
-- checked's is (017.sql).
CREATE TABLE expectation_checks (
	rule TEXT COLLATE "C" NOT NULL,
	expect TEXT COLLATE "C" NOT NULL,
	week BIGINT NOT NULL,
	what TEXT COLLATE "C" NOT NULL,
	held TEXT COLLATE "C" NOT NULL,
	selector TEXT COLLATE "C" NOT NULL,
	source TEXT COLLATE "C" NOT NULL,
	result TEXT COLLATE "C" NOT NULL,
	why TEXT COLLATE "C" NOT NULL DEFAULT '',
	n BIGINT,
	value TEXT COLLATE "C" NOT NULL DEFAULT '',
	at BIGINT NOT NULL DEFAULT 0,
	version TEXT COLLATE "C" NOT NULL DEFAULT '',
	current BIGINT NOT NULL DEFAULT 1,
	checked BIGINT NOT NULL,
	told BIGINT NOT NULL DEFAULT 0,
	task BIGINT NOT NULL DEFAULT 0,
	tasked BIGINT NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX expectation_checks_key ON expectation_checks (md5(rule), expect, week);
CREATE INDEX expectation_checks_result ON expectation_checks (result, week);
