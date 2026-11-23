-- Rules left out of a source's copies (docs/design/grafanajudged.md),
-- after 029: the periods a rule judged from Grafana's own alert state
-- history was left out of what lapilli serve copied in under its source,
-- as the history couldn't say what it fired then (its name another rule's
-- too, changes of it passed over, a firing no write holds), while the
-- other rules' days were written. A source's copies cover a period for
-- every rule of it (copied.SOURCE in meta); a row here takes a period out
-- of that for one rule, which has no record there, rather than a quiet
-- one, also once it is judged again.
--
-- source is the source's name; rule, the name the source sends the rule's
-- firings by (rules.sent), kept whether or not a firing of it was ever
-- copied in. from_at and to_at are Unix seconds: the period from the first
-- to before the second. A rule's periods that meet or overlap are kept as
-- one, 64 a rule at most, the two earliest run together past that. Kept
-- for --keep, by to_at.
CREATE TABLE left_out (
	id INTEGER PRIMARY KEY,
	source TEXT NOT NULL,
	rule TEXT NOT NULL,
	from_at INTEGER NOT NULL,
	to_at INTEGER NOT NULL
);
-- A rule's periods, in time; --keep.
CREATE UNIQUE INDEX left_out_rule ON left_out (source, rule, from_at);
CREATE INDEX left_out_to ON left_out (to_at);
