-- What people did when a rule fired (docs/design/responses.md), after 033.
--
-- responses are the pages a paging tool sent, as --responses and PUT
-- /api/v1/responses/SOURCE say them: id is the tool's id of the page, unique
-- with source (the file's is 'file'); alerts the alerts it was sent for, as
-- incidents.alerts keeps an incident's (JSON, sorted); acknowledged_at is
-- when a person acknowledged it, NULL for none; resolved_by is 'alert' or
-- 'person', '' while it is open. No person is named.
CREATE TABLE responses (
	source TEXT NOT NULL,
	id TEXT NOT NULL,
	sent_at INTEGER NOT NULL,
	acknowledged_at INTEGER,
	resolved_at INTEGER,
	resolved_by TEXT NOT NULL DEFAULT '',
	alerts TEXT NOT NULL,
	PRIMARY KEY (source, id)
);
CREATE INDEX responses_sent ON responses (sent_at);

-- The sources that have pages, to count them (at most 50, as firings')
-- without counting every page.
-- The rules each page's alerts name, and when it was sent: a firing's pages
-- are read by its rule and its time, not every page of the span. One row for
-- each rule a page names, kept with the page (made and dropped as it is).
CREATE TABLE response_sources (
	source TEXT NOT NULL PRIMARY KEY
);
CREATE TABLE response_rules (
	rule TEXT NOT NULL,
	sent_at INTEGER NOT NULL,
	source TEXT NOT NULL,
	id TEXT NOT NULL,
	PRIMARY KEY (rule, sent_at, id, source)
);
CREATE INDEX response_rules_page ON response_rules (source, id);

-- The changes made to the alerts' targets, read from kube-state-metrics'
-- history by each run for the hours it judges: a rollout, rollback or scale
-- of a workload, at when it showed. rollout_spans are the spans each
-- workload's changes are known over (from where its series begin), and
-- rollout_owners the workload each pod named by a firing belongs to, so that
-- judging again asks the history nothing. One more than the design lists:
-- without the owners a firing that names a pod alone couldn't be tied to its
-- workload again.
CREATE TABLE rollouts (
	kind TEXT NOT NULL,
	at INTEGER NOT NULL,
	namespace TEXT NOT NULL,
	workload_kind TEXT NOT NULL,
	workload TEXT NOT NULL,
	revision TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (kind, at, namespace, workload_kind, workload)
);
CREATE INDEX rollouts_at ON rollouts (at);
CREATE TABLE rollout_spans (
	namespace TEXT NOT NULL,
	workload_kind TEXT NOT NULL,
	workload TEXT NOT NULL,
	from_at INTEGER NOT NULL,
	to_at INTEGER NOT NULL,
	PRIMARY KEY (namespace, workload_kind, workload, from_at)
);
CREATE INDEX rollout_spans_to ON rollout_spans (to_at);
-- The spans a run could not read the changes of (the history failed), kept
-- to be read once more by the next run, and dropped when that run has read
-- them or tried, not again; until then the firings in them have changes not
-- known. A span that overlaps or touches another is one.
CREATE TABLE rollout_retries (
	from_at INTEGER NOT NULL PRIMARY KEY,
	to_at INTEGER NOT NULL
);
CREATE TABLE rollout_owners (
	namespace TEXT NOT NULL,
	pod TEXT NOT NULL,
	workload_kind TEXT NOT NULL,
	workload TEXT NOT NULL,
	seen_at INTEGER NOT NULL,
	PRIMARY KEY (namespace, pod)
);
CREATE INDEX rollout_owners_seen ON rollout_owners (seen_at);

-- A firing's response (0 none, 1 changed, 2 changed routine, 3
-- acknowledged, 4 unanswered), routine (1 when a routine change is kept with
-- an acknowledged or unanswered response) and useful (-1 unknown, 0, 1):
-- worked out as stones are, and again whenever stones are. A firing worked
-- out before has none, until judgedBy j3 judges every firing again.
ALTER TABLE firings ADD COLUMN response INTEGER NOT NULL DEFAULT 0;
ALTER TABLE firings ADD COLUMN routine INTEGER NOT NULL DEFAULT 0;
ALTER TABLE firings ADD COLUMN useful INTEGER NOT NULL DEFAULT -1;

-- The days' and weeks' counts, of the firings not excused: those with each
-- response, and those useful and not useful (the rest are unknown, which a
-- week's record turns not useful when the firing is black and judged).
ALTER TABLE days ADD COLUMN resp_changed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE days ADD COLUMN resp_routine INTEGER NOT NULL DEFAULT 0;
ALTER TABLE days ADD COLUMN resp_acknowledged INTEGER NOT NULL DEFAULT 0;
ALTER TABLE days ADD COLUMN resp_unanswered INTEGER NOT NULL DEFAULT 0;
ALTER TABLE days ADD COLUMN useful INTEGER NOT NULL DEFAULT 0;
ALTER TABLE days ADD COLUMN not_useful INTEGER NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN resp_changed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN resp_routine INTEGER NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN resp_acknowledged INTEGER NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN resp_unanswered INTEGER NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN useful INTEGER NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN not_useful INTEGER NOT NULL DEFAULT 0;
