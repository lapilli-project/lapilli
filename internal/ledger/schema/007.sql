-- A team's shifts, as --shifts takes them: hours that take turns, in place
-- of the team's one week of hours. Empty, the team keeps one week.
ALTER TABLE teams ADD COLUMN shifts TEXT NOT NULL DEFAULT '';
