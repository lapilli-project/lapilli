-- A team's own hours, as --work-hours, --sleep-hours and --tz take them:
-- its rules' firings are counted asleep or off hours by them. Each empty is
-- the server's.
ALTER TABLE teams ADD COLUMN work_hours TEXT NOT NULL DEFAULT '';
ALTER TABLE teams ADD COLUMN sleep_hours TEXT NOT NULL DEFAULT '';
ALTER TABLE teams ADD COLUMN tz TEXT NOT NULL DEFAULT '';
