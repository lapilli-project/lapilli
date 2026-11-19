-- A lead of a signal's or a rule entry's own, and labels an entry
-- requires (docs/design/judging.md, item 1), after pull request
-- backtests' 023.
--
-- lead_ns on impact_signals and impact_rules: how long before a window's
-- start a firing still catches it, for every rule judged against the
-- signal, or for the entry's rules against its signals' windows, in
-- nanoseconds, from 0 to a day; NULL to follow (an entry its signal's, a
-- signal --lead). require_labels on impact_rules: the labels a firing of
-- its rules and a window of its signals must both carry, a JSON list;
-- NULL for none. Every signal and entry kept before follows, and requires
-- nothing, as each judged.
ALTER TABLE impact_signals ADD COLUMN lead_ns INTEGER;
ALTER TABLE impact_rules ADD COLUMN lead_ns INTEGER;
ALTER TABLE impact_rules ADD COLUMN require_labels TEXT;
