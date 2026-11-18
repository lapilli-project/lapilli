-- Pull request backtests (docs/design/backtests.md), after impact
-- signals' 022.
--
-- backtests are the backtests CI jobs asked for on --backtest-listen, a row
-- each, kept a day after it ends and 200 at most, the oldest ended going
-- first. id is random (hex), so that ids don't count backtests; n orders
-- the queue, the oldest first. trust is the --backtest-trust entry that
-- took it; repository the repository as the token's claim names it, '' for
-- one that doesn't show as a name does (the entry's name is shown then);
-- pull_request and commit_sha as the token or the job said them. state is
-- waiting, running, done, failed, superseded or cancelled; asked_at,
-- started_at and ended_at in Unix seconds, 0 for not yet. rules are the
-- rules that differ, on either side; requests those it was counted to ask
-- of the history before it was queued, asked those it asked. key_hash is
-- the SHA-256 of the key it is read by, in hex. cancel is 1 once its job
-- cancelled it while it ran; attempts how often it was started, a leader
-- that stopped while it ran (a restart, a crash) putting it back to wait:
-- past twice, it fails. body is the request as it was sent, gzipped,
-- until it ends; result what it came to (its Markdown and JSON), gzipped,
-- once done; error why it failed, in words of Lapilli's own.
CREATE TABLE backtests (
	id TEXT PRIMARY KEY,
	n INTEGER NOT NULL,
	trust TEXT NOT NULL,
	repository TEXT NOT NULL DEFAULT '',
	pull_request INTEGER NOT NULL DEFAULT 0,
	commit_sha TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL,
	asked_at INTEGER NOT NULL,
	started_at INTEGER NOT NULL DEFAULT 0,
	ended_at INTEGER NOT NULL DEFAULT 0,
	rules INTEGER NOT NULL DEFAULT 0,
	requests INTEGER NOT NULL DEFAULT 0,
	asked INTEGER NOT NULL DEFAULT 0,
	key_hash TEXT NOT NULL,
	cancel INTEGER NOT NULL DEFAULT 0,
	attempts INTEGER NOT NULL DEFAULT 0,
	body BLOB,
	result BLOB,
	error TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;
CREATE UNIQUE INDEX backtests_n ON backtests (n);
CREATE INDEX backtests_state ON backtests (state, n);

-- backtest_tokens are the ID tokens that started a backtest, each by its
-- jti, or the token's hash when it has none (SHA-256, hex), under its
-- entry: its key is the check that a token starts one backtest, with the
-- body it first came with, among every replica at once. body_hash is that
-- body's SHA-256, backtest_id the backtest it started; until_at when the
-- row may go, past the hour a token is taken for, from its iat.
CREATE TABLE backtest_tokens (
	trust TEXT NOT NULL,
	token TEXT NOT NULL,
	body_hash TEXT NOT NULL,
	backtest_id TEXT NOT NULL,
	until_at INTEGER NOT NULL,
	PRIMARY KEY (trust, token)
) WITHOUT ROWID;
CREATE INDEX backtest_tokens_until ON backtest_tokens (until_at);
