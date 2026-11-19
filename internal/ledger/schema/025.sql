-- Episodes beside firings (docs/design/judging.md, item 2), after a
-- lead of a signal's or a rule entry's own's 024.
--
-- episode on firings: the id of the first firing of the episode it is of
-- (its rule, the values of the match labels in effect, overlapping or
-- within --refire of the latest end so far, parted at midnight), its own
-- id for a first; 0 for a firing a quiet window excused, which takes no
-- part in one, and for one not walked yet. episode_white, on a first
-- firing: 1 when any firing of its episode is white.
--
-- episodes and episodes_white on days, weeks, marked_days and
-- marked_weeks: the first firings counted there, by the key each row has,
-- and those of them whose episode is white.
--
-- Every firing kept before is walked again, a week a write, from the
-- newest week back (Ledger.CountEpisodes): meta episodes_from is the
-- Monday from which on the weeks are counted, the week after the newest
-- firing's until the walk has been through one. No stone changes.
ALTER TABLE firings ADD COLUMN episode INTEGER NOT NULL DEFAULT 0;
ALTER TABLE firings ADD COLUMN episode_white INTEGER NOT NULL DEFAULT 0;
ALTER TABLE days ADD COLUMN episodes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE days ADD COLUMN episodes_white INTEGER NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN episodes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE weeks ADD COLUMN episodes_white INTEGER NOT NULL DEFAULT 0;
ALTER TABLE marked_days ADD COLUMN episodes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE marked_days ADD COLUMN episodes_white INTEGER NOT NULL DEFAULT 0;
ALTER TABLE marked_weeks ADD COLUMN episodes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE marked_weeks ADD COLUMN episodes_white INTEGER NOT NULL DEFAULT 0;
INSERT INTO meta (key, value) SELECT 'episodes_from', CAST(max(day) - (max(day) + 3) % 7 + 7 AS TEXT) FROM firings HAVING count(*) > 0;
