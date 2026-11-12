-- What an operations team asks of its alerts (docs/design/noc.md), after
-- 0.28's 016.
--
-- When a service or a namespace was deployed, as --deploys reads it, which
-- owns them: a deploy from start to end, with the labels that say what was
-- deployed (service, namespace), the revision and the history it was read
-- from. A firing that starts in one, or within --deploy-after of its end,
-- and has the value of a label it names, came during it: deploy, a mark
-- beside its stones that changes neither. The days and weeks count those
-- firings (deploy) and those of them that caught something (deploy_white),
-- so precision can be read with them and without.
CREATE TABLE deploys (
	id INTEGER PRIMARY KEY,
	start_at INTEGER NOT NULL,
	end_at INTEGER NOT NULL,
	labels TEXT NOT NULL DEFAULT '{}',
	revision TEXT NOT NULL DEFAULT '',
	source TEXT NOT NULL DEFAULT ''
);
CREATE INDEX deploys_start ON deploys (start_at);

ALTER TABLE firings ADD COLUMN deploy INTEGER NOT NULL DEFAULT 0;
ALTER TABLE days ADD COLUMN deploy INTEGER NOT NULL DEFAULT 0;
ALTER TABLE days ADD COLUMN deploy_white INTEGER NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN deploy INTEGER NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN deploy_white INTEGER NOT NULL DEFAULT 0;

-- A rule's re-fired firings, found without reading the rest: which of its
-- alerts re-fired most, over a week or twelve.
CREATE INDEX firings_refired ON firings (rule_id, start_at) WHERE refired = 1;
