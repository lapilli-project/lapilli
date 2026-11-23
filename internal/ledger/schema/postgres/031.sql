-- A backtest's repository by its id (docs/design/backtests.md), as
-- SQLite's 031.sql: what the column is for is said there.
ALTER TABLE backtests ADD COLUMN repository_id TEXT COLLATE "C" NOT NULL DEFAULT '';
