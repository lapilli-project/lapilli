-- What an operations team asks of its alerts (docs/design/noc.md), after
-- 0.28's 016.
--
-- When a service or a namespace was deployed, as --deploys reads it, which
-- owns them: a deploy from start to end, with the labels that say what was
-- deployed (service, namespace), the revision and the history it was read
-- from. A firing that starts in one, from --deploy-before before its start
-- to --deploy-after after its end, and agrees with it on the labels both
-- have, one at least, came during it: deploy, a mark
-- beside its stones that changes neither. The days and weeks count those
-- firings (deploy) and those of them that caught something (deploy_white),
-- so precision can be read with them and without.
--
-- A history's lines all deploy what its entry's labels name: each label set
-- is kept once, in deploy_labels, and a deploy points at it.
CREATE TABLE deploy_labels (
	id INTEGER PRIMARY KEY,
	labels TEXT NOT NULL UNIQUE
);
CREATE TABLE deploys (
	id INTEGER PRIMARY KEY,
	start_at INTEGER NOT NULL,
	end_at INTEGER NOT NULL,
	labels_id INTEGER NOT NULL REFERENCES deploy_labels (id),
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
