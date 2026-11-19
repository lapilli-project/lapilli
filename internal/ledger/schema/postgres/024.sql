-- A lead of a signal's or a rule entry's own, and labels an entry
-- requires (docs/design/judging.md, item 1), as SQLite's 024.sql: what
-- each column is for is said there.
ALTER TABLE impact_signals ADD COLUMN lead_ns BIGINT;
ALTER TABLE impact_rules ADD COLUMN lead_ns BIGINT;
ALTER TABLE impact_rules ADD COLUMN require_labels TEXT COLLATE "C";
