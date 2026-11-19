-- Episodes beside firings (docs/design/judging.md, item 2), as SQLite's
-- 025.sql: what each column is for is said there.
ALTER TABLE firings ADD COLUMN episode BIGINT NOT NULL DEFAULT 0;
ALTER TABLE firings ADD COLUMN episode_white BIGINT NOT NULL DEFAULT 0;
ALTER TABLE days ADD COLUMN episodes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE days ADD COLUMN episodes_white BIGINT NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN episodes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN episodes_white BIGINT NOT NULL DEFAULT 0;
ALTER TABLE marked_days ADD COLUMN episodes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE marked_days ADD COLUMN episodes_white BIGINT NOT NULL DEFAULT 0;
ALTER TABLE marked_weeks ADD COLUMN episodes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE marked_weeks ADD COLUMN episodes_white BIGINT NOT NULL DEFAULT 0;
INSERT INTO meta (key, value) SELECT 'episodes_from', CAST(max(day) - (max(day) + 3) % 7 + 7 AS TEXT) FROM firings HAVING count(*) > 0;
