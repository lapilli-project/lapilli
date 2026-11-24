-- A merge queue's backtest told apart, and a run's place among its
-- workflow's (docs/design/backtests.md), as SQLite's 032.sql: what the
-- columns are for is said there.
ALTER TABLE backtests ADD COLUMN queue_branch TEXT COLLATE "C" NOT NULL DEFAULT '';
ALTER TABLE backtests ADD COLUMN workflow TEXT COLLATE "C" NOT NULL DEFAULT '';
ALTER TABLE backtests ADD COLUMN run_number BIGINT NOT NULL DEFAULT 0;
