-- What people did when a rule fired (docs/design/responses.md), as SQLite's
-- 034.sql: what each table and column is for is said there. The keys are
-- bounded well below what a btree holds (a source's name, a page's id of 256
-- bytes at most, a Kubernetes name of 253), so they are plain primary keys.
CREATE TABLE responses (
	source TEXT COLLATE "C" NOT NULL,
	id TEXT COLLATE "C" NOT NULL,
	sent_at BIGINT NOT NULL,
	acknowledged_at BIGINT,
	resolved_at BIGINT,
	resolved_by TEXT COLLATE "C" NOT NULL DEFAULT '',
	alerts TEXT COLLATE "C" NOT NULL,
	PRIMARY KEY (source, id)
);
CREATE INDEX responses_sent ON responses (sent_at);

CREATE TABLE rollouts (
	kind TEXT COLLATE "C" NOT NULL,
	at BIGINT NOT NULL,
	namespace TEXT COLLATE "C" NOT NULL,
	workload_kind TEXT COLLATE "C" NOT NULL,
	workload TEXT COLLATE "C" NOT NULL,
	revision TEXT COLLATE "C" NOT NULL DEFAULT '',
	PRIMARY KEY (kind, at, namespace, workload_kind, workload)
);
CREATE INDEX rollouts_at ON rollouts (at);
CREATE TABLE rollout_spans (
	namespace TEXT COLLATE "C" NOT NULL,
	workload_kind TEXT COLLATE "C" NOT NULL,
	workload TEXT COLLATE "C" NOT NULL,
	from_at BIGINT NOT NULL,
	to_at BIGINT NOT NULL,
	PRIMARY KEY (namespace, workload_kind, workload, from_at)
);
CREATE INDEX rollout_spans_to ON rollout_spans (to_at);
CREATE TABLE rollout_owners (
	namespace TEXT COLLATE "C" NOT NULL,
	pod TEXT COLLATE "C" NOT NULL,
	workload_kind TEXT COLLATE "C" NOT NULL,
	workload TEXT COLLATE "C" NOT NULL,
	seen_at BIGINT NOT NULL,
	PRIMARY KEY (namespace, pod)
);
CREATE INDEX rollout_owners_seen ON rollout_owners (seen_at);

ALTER TABLE firings ADD COLUMN response BIGINT NOT NULL DEFAULT 0;
ALTER TABLE firings ADD COLUMN routine BIGINT NOT NULL DEFAULT 0;
ALTER TABLE firings ADD COLUMN useful BIGINT NOT NULL DEFAULT -1;

ALTER TABLE days ADD COLUMN resp_changed BIGINT NOT NULL DEFAULT 0;
ALTER TABLE days ADD COLUMN resp_routine BIGINT NOT NULL DEFAULT 0;
ALTER TABLE days ADD COLUMN resp_acknowledged BIGINT NOT NULL DEFAULT 0;
ALTER TABLE days ADD COLUMN resp_unanswered BIGINT NOT NULL DEFAULT 0;
ALTER TABLE days ADD COLUMN useful BIGINT NOT NULL DEFAULT 0;
ALTER TABLE days ADD COLUMN not_useful BIGINT NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN resp_changed BIGINT NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN resp_routine BIGINT NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN resp_acknowledged BIGINT NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN resp_unanswered BIGINT NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN useful BIGINT NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN not_useful BIGINT NOT NULL DEFAULT 0;
