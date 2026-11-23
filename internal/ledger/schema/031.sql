-- A backtest's repository by its id (docs/design/backtests.md), after 030.
-- repository_id is the repository's id as the ID token that asked says
-- it, the claim its --backtest-trust entry pins (GitHub's and Forgejo's
-- repository_id, GitLab's project_id, Bitbucket's repositoryUuid): '' for
-- a plain token's, for a CI Lapilli doesn't know, and for a backtest asked
-- before this. A newer commit's backtest takes the place of the one
-- waiting for the same entry, repository id and pull request, whatever
-- the repository is named by then; a row with no id is matched by its
-- repository's name, as every row was before.
ALTER TABLE backtests ADD COLUMN repository_id TEXT NOT NULL DEFAULT '';
