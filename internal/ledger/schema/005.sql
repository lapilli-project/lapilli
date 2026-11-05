-- A rule's days in turn: measuring a change to a rule reads the weeks
-- either side of it, for that rule alone.
CREATE INDEX days_rule ON days (rule_id, day);
