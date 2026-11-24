-- A merge queue's backtest told apart (docs/design/backtests.md), as
-- SQLite's 032.sql: what the column is for is said there.
ALTER TABLE backtests ADD COLUMN queue_branch TEXT COLLATE "C" NOT NULL DEFAULT '';
