-- The service graph a catch by dependency is judged by, as SQLite's
-- 036.sql: what each table and column is for is said there. Names are 256
-- bytes at most, so the key is a plain primary key.
CREATE TABLE edges (
	hour BIGINT NOT NULL,
	from_svc TEXT COLLATE "C" NOT NULL,
	to_svc TEXT COLLATE "C" NOT NULL,
	PRIMARY KEY (hour, from_svc, to_svc)
);
CREATE TABLE edge_spans (
	from_at BIGINT NOT NULL PRIMARY KEY,
	to_at BIGINT NOT NULL
);
CREATE INDEX edge_spans_to ON edge_spans (to_at);
CREATE TABLE edge_retries (
	from_at BIGINT NOT NULL PRIMARY KEY,
	to_at BIGINT NOT NULL
);
ALTER TABLE catches ADD COLUMN dependency TEXT COLLATE "C" NOT NULL DEFAULT '';
