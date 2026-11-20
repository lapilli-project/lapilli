-- What checking each rule's expectations found (docs/design/expectations.md,
-- steps 2 and 4), after the judging design's 026. Expectations are
-- declared, in --expect's file, --config's expectations: or a rule's
-- annotation lapilli.dev/budget, not made in the UI: the ledger keeps what
-- checking them found, not what they are. The leader writes it after each
-- round.
--
-- A row a rule, an expectation and a week: rule is the name the rule is
-- judged under; expect, the first 32 hex digits of the SHA-256 of what it
-- is held to, as text (its selector, what and held: "rule: Restarts\nbudget
-- week\n10"); week, the week's Monday, as days, of a budget, and of a
-- catch the week its incident started in (0 while the incident isn't
-- written down), and of a scenario the week what it found was found in.
-- what is "budget week", "budget asleep", "budget refires", "catches
-- INC-43" or "scenario tests/restarts.yaml"; held, what it is held to
-- ("10", "within 5m", "caught", "passes"); selector, what holds the rule
-- ("rule: Restarts", "group: shop", "labels: severity=page"); source, where
-- it is declared (file, config or annotation).
--
-- result is met, broken, not known, or open: of the week under way, a
-- budget not over yet. why is, of not known and open, why (no_hours,
-- not_whole, unreplayed, no_incident, not_judged_again, ...), '' else. n
-- is a budget's count, NULL for a catch; value, what decided it as a
-- person reads it ("26", "+2m", "✗", "fails"). at is a catch's
-- incident's start, Unix seconds, 0 else; version, of a catch judged again
-- on a rule's new version by a replay of it (docs/design/expectations.md,
-- "A catch lost"), what the rule was then (its key's hash), '' when judged
-- by the ledger's own firings; of a scenario, what it was run on (a hash
-- of promtool's path, the rule's group as written and the test file), so
-- that it is run again when one of them changes. detail is, of a
-- scenario, what promtool said of it failing, or why it couldn't be run,
-- 2,000 bytes at most; '' else. current is 1 for the expectations held
-- now, 0 for one no longer declared, or of a rule no longer in effect,
-- kept so that a ConfigMap put back tells no week again; checked, when it
-- was last written.
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
	detail TEXT NOT NULL DEFAULT '',
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

-- Drafts of what a rule is held to (docs/design/expectations.md, step 3),
-- for a person to accept or drop: an agent's, through MCP's
-- propose_expectation, or the history's, the most the rule fired in a
-- week rounded up and the incidents it caught, kept from its tab. Accepting
-- one is a change like any other: the git path's pull request to the
-- expectations file, through the change recipes, or, where serve can't
-- write where they are declared, the YAML for a person to put there.
--
-- rule is the name the rule is judged under; origin, agent or history;
-- made_by, who proposed it, as proposals keep a person's name
-- (ledger.FoldName): the person, or the person the agent acts for, with
-- agent 1; made, when (Unix seconds). text is the expectations, as Lapilli
-- wrote them with the YAML encoder (64 KiB at most); tests, of an agent's,
-- the scenarios' test files by path, as JSON (256 KiB at most in all);
-- reason, why, in the agent's or the person's words (2,000 bytes); found,
-- what checking it found, item by item, as JSON (64 KiB). target is where
-- accepting it goes: file (--expect's, by the git path when it can), config
-- or none; doc_hash, the SHA-256 of the expectations file it was made
-- against, which the git path finds the file by; document, once accepted
-- by the git path, the file with it taken in, as Lapilli wrote it (1 MiB).
--
-- state is open, accepted or dropped; decided_by and decided, who
-- accepted or dropped it and when (0 while open). pr is, of one accepted by
-- the git path, its pull request's: ready (for a recipe), opened, merged,
-- closed or failed, '' for none; link, its pull request's; pr_reason, why
-- it failed or was closed. Drafts are bounded: 10 open a rule, 1,000 kept
-- in all, the oldest decided let go first.
--
-- proposal is, of the expectations a proposal adding a rule was held to
-- (step 5, origin proposal), that proposal: kept accepted, by its first
-- approver, once the rule is added, where a draft accepted goes; 0 for
-- every other draft. A proposal counts toward the hour's limit, not the
-- expectations it carries.
CREATE TABLE expectation_drafts (
	id INTEGER PRIMARY KEY,
	rule TEXT NOT NULL,
	origin TEXT NOT NULL,
	made_by TEXT NOT NULL,
	agent INTEGER NOT NULL DEFAULT 0,
	made INTEGER NOT NULL,
	text TEXT NOT NULL,
	tests TEXT NOT NULL DEFAULT '{}',
	reason TEXT NOT NULL DEFAULT '',
	found TEXT NOT NULL DEFAULT '[]',
	target TEXT NOT NULL,
	doc_hash TEXT NOT NULL DEFAULT '',
	document TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL,
	decided_by TEXT NOT NULL DEFAULT '',
	decided INTEGER NOT NULL DEFAULT 0,
	pr TEXT NOT NULL DEFAULT '',
	link TEXT NOT NULL DEFAULT '',
	pr_reason TEXT NOT NULL DEFAULT '',
	proposal INTEGER NOT NULL DEFAULT 0
);
-- A rule's drafts, its tab's; who made how many in the last hour, as the
-- proposals they count with; those of a state, the list and the recipes'.
CREATE INDEX expectation_drafts_rule ON expectation_drafts (rule, id);
CREATE INDEX expectation_drafts_by ON expectation_drafts (made_by, made);
CREATE INDEX expectation_drafts_state ON expectation_drafts (state, id);
CREATE INDEX expectation_drafts_pr ON expectation_drafts (pr, id);
-- The draft a proposal adding a rule was kept as.
CREATE INDEX expectation_drafts_proposal ON expectation_drafts (proposal);

-- What a proposal adding a rule (kind add, step 5) holds the rule to: the
-- expectations as Lapilli wrote them and their scenarios' test files, as
-- JSON ({"text": ..., "tests": {...}}), bounded as a draft's are, and,
-- once the rule is added and they are kept as a draft, that draft's id
-- ("kept"), so that they are kept once however many drafts are let go;
-- '' for every other kind. Read with the proposal alone, never with a
-- list of them.
ALTER TABLE proposals ADD COLUMN expect TEXT NOT NULL DEFAULT '';
