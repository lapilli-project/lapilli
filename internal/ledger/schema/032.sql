-- A merge queue's backtest told apart (docs/design/backtests.md), after
-- 031. queue_branch is, of a backtest a merge queue's run asked for
-- (GitHub's merge_group), the branch the queue merges into, as the ref
-- its ID token names says it (refs/heads/gh-readonly-queue/BRANCH/pr-N-SHA,
-- the pull request's number N); '' for a pull request's, any other run's,
-- and a backtest asked before this. A group the queue built again takes
-- the place of the one waiting for the same entry, repository, pull
-- request and branch, as a pull request's newer commit does of its own:
-- never a pull request's, nor a pull request's newer commit a group's.
ALTER TABLE backtests ADD COLUMN queue_branch TEXT NOT NULL DEFAULT '';
