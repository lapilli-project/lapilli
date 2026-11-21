-- The alerts an incident's tool linked to it, and whether a catch was by
-- one of them (docs/design/links.md), as SQLite's 028.sql: what each
-- column is for is said there.
ALTER TABLE incidents ADD COLUMN alerts TEXT COLLATE "C" NOT NULL DEFAULT '';
ALTER TABLE catches ADD COLUMN linked BIGINT NOT NULL DEFAULT 0;
