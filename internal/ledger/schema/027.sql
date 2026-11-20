-- What checking each rule's expectations found (docs/design/expectations.md,
-- step 2), after the judging design's 026. Expectations are declared, in
-- --expect's file, --config's expectations: or a rule's annotation
-- lapilli.dev/budget, not made in the UI: the ledger keeps what checking
-- them found, not what they are. The leader writes it after each round.
--
-- A row a rule, an expectation and a week: rule is the name the rule is
-- judged under; expect, the first 32 hex digits of the SHA-256 of what it
-- is held to, as text (its selector, what and held: "rule: Restarts\nbudget
-- week\n10"); week, the week's Monday, as days, of a budget, and of a
-- catch the week its incident started in (0 while the incident isn't
-- written down). what is "budget week", "budget asleep", "budget refires"
-- or "catches INC-43"; held, what it is held to ("10", "within 5m",
-- "caught"); selector, what holds the rule ("rule: Restarts", "group:
-- shop", "labels: severity=page"); source, where it is declared (file,
-- config or annotation).
--
-- result is met, broken, not known, or open: of the week under way, a
-- budget not over yet. why is, of not known and open, why (no_hours,
-- not_whole, unreplayed, no_incident, not_judged_again, ...), '' else. n
-- is a budget's count, NULL for a catch; value, what decided it as a
-- person reads it ("26", "+2m", "✗"). at is a catch's incident's start,
-- Unix seconds, 0 else; version, of a catch judged again on a rule's new
-- version by a replay of it (docs/design/expectations.md, "A catch
-- lost"), what the rule was then (its key's hash), '' when judged by the
-- ledger's own firings. current is 1 for the expectations held now, 0 for
-- one no longer declared, or of a rule no longer in effect, kept so that a
-- ConfigMap put back tells no week again; checked, when it was last
-- written.
--
-- told is the week (its Monday, as days) a notice of it broken was last
-- put in the outbox, 0 for none: once a rule, an expectation and a week;
-- a catch met again is told again when it is next broken, in a later
-- week. task is the task made of it, with --expect-tasks or a person's
-- Make it a task, 0 for none; tasked, the week it was made in.
CREATE TABLE expectation_checks (
	rule TEXT NOT NULL,
	expect TEXT NOT NULL,
	week INTEGER NOT NULL,
	what TEXT NOT NULL,
	held TEXT NOT NULL,
	selector TEXT NOT NULL,
	source TEXT NOT NULL,
	result TEXT NOT NULL,
	why TEXT NOT NULL DEFAULT '',
	n INTEGER,
	value TEXT NOT NULL DEFAULT '',
	at INTEGER NOT NULL DEFAULT 0,
	version TEXT NOT NULL DEFAULT '',
	current INTEGER NOT NULL DEFAULT 1,
	checked INTEGER NOT NULL,
	told INTEGER NOT NULL DEFAULT 0,
	task INTEGER NOT NULL DEFAULT 0,
	tasked INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (rule, expect, week)
) WITHOUT ROWID;
-- What is broken, of the weeks after one: the overview, the digest, the
-- rules list's filter.
CREATE INDEX expectation_checks_result ON expectation_checks (result, week);
