-- The service graph a catch by dependency is judged by
-- (docs/design/dependencies.md, "In the ledger"), after 035: who called
-- whom, read from the history by each run for the hours it judges (and the
-- hour or lead before them), so that judging again asks the history nothing.
--
-- edges holds an edge for each hour it held: hour is the hour's start, in
-- seconds, UTC (a multiple of 3600); from_svc called to_svc. Names are as
-- long as 256 bytes at most (graph.MaxName), and an hour has 20,000 edges
-- at most (graph.MaxEdgesPerHour): a read past it keeps none. Kept as long
-- as the firings are.
CREATE TABLE edges (
	hour INTEGER NOT NULL,
	from_svc TEXT NOT NULL,
	to_svc TEXT NOT NULL,
	PRIMARY KEY (hour, from_svc, to_svc)
);

-- The spans the graph was read over, apart and whole (one that overlaps or
-- touches another is one), as rollout_spans are: an hour outside them is not
-- known, which a span that failed to be read is until it is read again.
CREATE TABLE edge_spans (
	from_at INTEGER NOT NULL PRIMARY KEY,
	to_at INTEGER NOT NULL
);
CREATE INDEX edge_spans_to ON edge_spans (to_at);

-- The spans a run could not read the graph of (the history failed), kept to
-- be read once more by the next run, and dropped when that run has read
-- them or tried, not again.
CREATE TABLE edge_retries (
	from_at INTEGER NOT NULL PRIMARY KEY,
	to_at INTEGER NOT NULL
);

-- The way a rule caught an incident by the service graph alone, as text
-- (a JSON list of the services in call order), '' for any other catch:
-- every catch's before, none of which was judged by a graph.
ALTER TABLE catches ADD COLUMN dependency TEXT NOT NULL DEFAULT '';
