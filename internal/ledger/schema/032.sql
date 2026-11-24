-- Whether a catch was loose (verdict.Catch.Loose), and which labels it
-- lacked, after 031: the names of the labels of the match in effect that
-- every firing of the rule that caught the incident lacked and the incident
-- has, joined by commas, and '' when the catch wasn't loose: every catch's
-- before, which shows no mark until its incident is judged again.
ALTER TABLE catches ADD COLUMN loose TEXT NOT NULL DEFAULT '';
