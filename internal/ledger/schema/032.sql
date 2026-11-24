-- A merge queue's backtest told apart, and a run's place among its
-- workflow's (docs/design/backtests.md), after 031. queue_branch is, of a
-- backtest a merge queue's run asked for (GitHub's merge_group), the
-- branch the queue merges into, as the ref its ID token names says it
-- (refs/heads/gh-readonly-queue/BRANCH/pr-N-SHA, the pull request's
-- number N); '' for a pull request's, any other run's, and a backtest
-- asked before this. workflow and run_number are, of a GitHub Actions
-- job's, the workflow its run is of (its file, as the token's
-- workflow_ref names it before its @ref) and the run's number
-- (run_number: 1 for a workflow's first run, one more for each new run,
-- the same when a run is run again); '' and 0 for any other CI's, and a
-- backtest asked before this. A backtest of another commit takes the
-- place of the one waiting for the same entry, repository, pull request
-- and branch, a group the queue built again as a pull request's newer
-- commit; of a workflow, only of an earlier run of it, so that a run that
-- asks late, or an earlier run run again, takes no newer one's place.
ALTER TABLE backtests ADD COLUMN queue_branch TEXT NOT NULL DEFAULT '';
ALTER TABLE backtests ADD COLUMN workflow TEXT NOT NULL DEFAULT '';
ALTER TABLE backtests ADD COLUMN run_number INTEGER NOT NULL DEFAULT 0;
