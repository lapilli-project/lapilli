-- Pull request backtests (docs/design/backtests.md), as SQLite's 023.sql:
-- what each table and column is for is said there.
CREATE TABLE backtests (
	id TEXT COLLATE "C" PRIMARY KEY,
	n BIGINT NOT NULL,
	trust TEXT COLLATE "C" NOT NULL,
	repository TEXT COLLATE "C" NOT NULL DEFAULT '',
	pull_request BIGINT NOT NULL DEFAULT 0,
	commit_sha TEXT COLLATE "C" NOT NULL DEFAULT '',
	state TEXT COLLATE "C" NOT NULL,
	asked_at BIGINT NOT NULL,
	started_at BIGINT NOT NULL DEFAULT 0,
	ended_at BIGINT NOT NULL DEFAULT 0,
	rules BIGINT NOT NULL DEFAULT 0,
	requests BIGINT NOT NULL DEFAULT 0,
	asked BIGINT NOT NULL DEFAULT 0,
	key_hash TEXT COLLATE "C" NOT NULL,
	cancel BIGINT NOT NULL DEFAULT 0,
	attempts BIGINT NOT NULL DEFAULT 0,
	body BYTEA,
	result BYTEA,
	error TEXT COLLATE "C" NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX backtests_n ON backtests (n);
CREATE INDEX backtests_state ON backtests (state, n);

CREATE TABLE backtest_tokens (
	trust TEXT COLLATE "C" NOT NULL,
	token TEXT COLLATE "C" NOT NULL,
	body_hash TEXT COLLATE "C" NOT NULL,
	backtest_id TEXT COLLATE "C" NOT NULL,
	until_at BIGINT NOT NULL,
	PRIMARY KEY (trust, token)
);
CREATE INDEX backtest_tokens_until ON backtest_tokens (until_at);
