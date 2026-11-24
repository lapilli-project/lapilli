-- Whether a catch was loose (verdict.Catch.Loose), after 031: loose is 1
-- when every firing of the rule that caught the incident lacked a label of
-- the match in effect (--match) that the incident has, 0 else: every
-- catch's before, which shows no mark until its incident is judged again.
ALTER TABLE catches ADD COLUMN loose INTEGER NOT NULL DEFAULT 0;
