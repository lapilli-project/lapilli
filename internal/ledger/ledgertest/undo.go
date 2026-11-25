package ledgertest

import "slices"

// Undo022SQLite takes back what schema 022 made, in SQLite, for the tests
// of the migrations before it, and of a ledger upgraded from 021: the
// unnamed signal's row back in meta, the tallies keyed by rule and day or
// week alone, summed over the signals.
var Undo022SQLite = slices.Concat(undo022meta, []string{
	"DROP TABLE impact_signals",
	"DROP TABLE impact_rules",
	"ALTER TABLE incidents DROP COLUMN match_labels",
	"ALTER TABLE incidents DROP COLUMN signal",
	"DROP INDEX series_untied",
	"ALTER TABLE series DROP COLUMN signals",
	`CREATE TABLE days_021 (day INTEGER NOT NULL, rule_id INTEGER NOT NULL, firings INTEGER NOT NULL, white INTEGER NOT NULL, refired INTEGER NOT NULL,
		excused INTEGER NOT NULL DEFAULT 0, deploy INTEGER NOT NULL DEFAULT 0, deploy_white INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (day, rule_id)) WITHOUT ROWID`,
	`INSERT INTO days_021 SELECT day, rule_id, sum(firings), sum(white), sum(refired), sum(excused), sum(deploy), sum(deploy_white) FROM days GROUP BY day, rule_id`,
	"DROP TABLE days",
	"ALTER TABLE days_021 RENAME TO days",
	"CREATE INDEX days_rule ON days (rule_id, day)",
	`CREATE TABLE weeks_021 (week INTEGER NOT NULL, rule_id INTEGER NOT NULL, firings INTEGER NOT NULL, white INTEGER NOT NULL, refired INTEGER NOT NULL,
		excused INTEGER NOT NULL DEFAULT 0, deploy INTEGER NOT NULL DEFAULT 0, deploy_white INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (week, rule_id)) WITHOUT ROWID`,
	`INSERT INTO weeks_021 SELECT week, rule_id, sum(firings), sum(white), sum(refired), sum(excused), sum(deploy), sum(deploy_white) FROM weeks GROUP BY week, rule_id`,
	"DROP TABLE weeks",
	"ALTER TABLE weeks_021 RENAME TO weeks",
	`CREATE TABLE marked_days_021 (day INTEGER NOT NULL, rule_id INTEGER NOT NULL, kind_id INTEGER NOT NULL, firings INTEGER NOT NULL, white INTEGER NOT NULL,
		refired INTEGER NOT NULL, PRIMARY KEY (day, rule_id, kind_id))`,
	`INSERT INTO marked_days_021 SELECT day, rule_id, kind_id, sum(firings), sum(white), sum(refired) FROM marked_days GROUP BY day, rule_id, kind_id`,
	"DROP TABLE marked_days",
	"ALTER TABLE marked_days_021 RENAME TO marked_days",
	`CREATE TABLE marked_weeks_021 (week INTEGER NOT NULL, rule_id INTEGER NOT NULL, kind_id INTEGER NOT NULL, firings INTEGER NOT NULL, white INTEGER NOT NULL,
		refired INTEGER NOT NULL, PRIMARY KEY (week, rule_id, kind_id))`,
	`INSERT INTO marked_weeks_021 SELECT week, rule_id, kind_id, sum(firings), sum(white), sum(refired) FROM marked_weeks GROUP BY week, rule_id, kind_id`,
	"DROP TABLE marked_weeks",
	"ALTER TABLE marked_weeks_021 RENAME TO marked_weeks",
})

// undo022meta puts what the unnamed signal's row keeps back in meta, under
// the keys 0.49 to 0.53 kept it by.
var undo022meta = []string{
	"INSERT INTO meta (key, value) SELECT 'impact_since', since FROM impact_signals WHERE name = '' AND since != ''",
	"INSERT INTO meta (key, value) SELECT 'impact_criterion', criterion FROM impact_signals WHERE name = '' AND criterion != ''",
	"INSERT INTO meta (key, value) SELECT 'impact_again', again FROM impact_signals WHERE name = '' AND again != ''",
	"INSERT INTO meta (key, value) SELECT 'impact_data', data FROM impact_signals WHERE name = '' AND data != ''",
	"INSERT INTO meta (key, value) SELECT 'impact_read', last_read FROM impact_signals WHERE name = '' AND last_read != ''",
	"INSERT INTO meta (key, value) SELECT 'impact_floor', floor_at FROM impact_signals WHERE name = '' AND floor_at != ''",
}

// Undo022Postgres is Undo022SQLite in Postgres, which changes a key in
// place.
var Undo022Postgres = slices.Concat(undo022meta, []string{
	"DROP TABLE impact_signals",
	"DROP TABLE impact_rules",
	"ALTER TABLE incidents DROP COLUMN match_labels",
	"ALTER TABLE incidents DROP COLUMN signal",
	"ALTER TABLE series DROP COLUMN signals",
	"ALTER TABLE days DROP CONSTRAINT days_pkey",
	"ALTER TABLE days DROP COLUMN signals",
	"ALTER TABLE days ADD PRIMARY KEY (day, rule_id)",
	"ALTER TABLE weeks DROP CONSTRAINT weeks_pkey",
	"ALTER TABLE weeks DROP COLUMN signals",
	"ALTER TABLE weeks ADD PRIMARY KEY (week, rule_id)",
	"ALTER TABLE marked_days DROP CONSTRAINT marked_days_pkey",
	"ALTER TABLE marked_days DROP COLUMN signals",
	"ALTER TABLE marked_days ADD PRIMARY KEY (day, rule_id, kind_id)",
	"ALTER TABLE marked_weeks DROP CONSTRAINT marked_weeks_pkey",
	"ALTER TABLE marked_weeks DROP COLUMN signals",
	"ALTER TABLE marked_weeks ADD PRIMARY KEY (week, rule_id, kind_id)",
})

// Undo035 takes back what schema 035 made, in SQLite and in Postgres
// alike, for the tests of the migrations before it.
var Undo035 = []string{
	"DROP TABLE firing_routed",
	"DROP TABLE routed",
}

// Undo034 takes back what schema 034 made, in SQLite and in Postgres
// alike, for the tests of the migrations before it.
var Undo034 = []string{
	"DROP TABLE response_rules",
	"DROP TABLE response_sources",
	"DROP TABLE responses",
	"DROP TABLE rollout_retries",
	"DROP TABLE rollouts",
	"DROP TABLE rollout_spans",
	"DROP TABLE rollout_owners",
	"ALTER TABLE firings DROP COLUMN response",
	"ALTER TABLE firings DROP COLUMN routine",
	"ALTER TABLE firings DROP COLUMN useful",
	"ALTER TABLE days DROP COLUMN resp_changed",
	"ALTER TABLE days DROP COLUMN resp_routine",
	"ALTER TABLE days DROP COLUMN resp_acknowledged",
	"ALTER TABLE days DROP COLUMN resp_unanswered",
	"ALTER TABLE days DROP COLUMN useful",
	"ALTER TABLE days DROP COLUMN not_useful",
	"ALTER TABLE weeks DROP COLUMN resp_changed",
	"ALTER TABLE weeks DROP COLUMN resp_routine",
	"ALTER TABLE weeks DROP COLUMN resp_acknowledged",
	"ALTER TABLE weeks DROP COLUMN resp_unanswered",
	"ALTER TABLE weeks DROP COLUMN useful",
	"ALTER TABLE weeks DROP COLUMN not_useful",
}

// Undo033 takes back what schema 033 made, in SQLite and in Postgres
// alike, for the tests of the migrations before it.
var Undo033 = []string{
	"ALTER TABLE backtests DROP COLUMN queue_branch",
	"ALTER TABLE backtests DROP COLUMN workflow",
	"ALTER TABLE backtests DROP COLUMN run_number",
}

// Undo032 takes back what schema 032 made, in SQLite and in Postgres
// alike, for the tests of the migrations before it.
var Undo032 = []string{
	"ALTER TABLE catches DROP COLUMN loose",
}

// Undo031 takes back what schema 031 made, in SQLite and in Postgres
// alike, for the tests of the migrations before it.
var Undo031 = []string{
	"ALTER TABLE backtests DROP COLUMN repository_id",
}

// Undo030 takes back what schema 030 made, in SQLite and in Postgres
// alike, for the tests of the migrations before it.
var Undo030 = []string{
	"DROP TABLE left_out",
}

// Undo029 takes back what schema 029 made, in SQLite and in Postgres
// alike, for the tests of the migrations before it.
var Undo029 = []string{
	"DROP TABLE mutes",
}

// Undo028 takes back what schema 028 made, in SQLite and in Postgres
// alike, for the tests of the migrations before it.
var Undo028 = []string{
	"ALTER TABLE catches DROP COLUMN linked",
	"ALTER TABLE incidents DROP COLUMN alerts",
}

// Undo027 takes back what schema 027 made, in SQLite and in Postgres
// alike, for the tests of the migrations before it.
var Undo027 = []string{
	"DROP TABLE expectation_checks",
	"DROP TABLE expectation_drafts",
	"DROP INDEX proposals_unkept",
	"ALTER TABLE proposals DROP COLUMN expect_kept",
	"ALTER TABLE proposals DROP COLUMN expect",
}

// Undo026 takes back what schema 026 made, in SQLite and in Postgres
// alike, for the tests of the migrations before it.
var Undo026 = []string{
	"ALTER TABLE goals DROP COLUMN stones",
	"ALTER TABLE impact_rules DROP COLUMN judged",
	"ALTER TABLE weeks DROP COLUMN action_no_white",
	"ALTER TABLE weeks DROP COLUMN action_yes_black",
	"ALTER TABLE days DROP COLUMN action_no_white",
	"ALTER TABLE days DROP COLUMN action_yes_black",
	"ALTER TABLE incidents DROP COLUMN real_of",
	"ALTER TABLE weeks DROP COLUMN action_no",
	"ALTER TABLE weeks DROP COLUMN action_yes",
	"ALTER TABLE weeks DROP COLUMN real_excused",
	"ALTER TABLE weeks DROP COLUMN real_white",
	"ALTER TABLE weeks DROP COLUMN unrelated_excused",
	"ALTER TABLE weeks DROP COLUMN unrelated",
	"ALTER TABLE days DROP COLUMN action_no",
	"ALTER TABLE days DROP COLUMN action_yes",
	"ALTER TABLE days DROP COLUMN real_excused",
	"ALTER TABLE days DROP COLUMN real_white",
	"ALTER TABLE days DROP COLUMN unrelated_excused",
	"ALTER TABLE days DROP COLUMN unrelated",
	"ALTER TABLE firings DROP COLUMN unrelated",
	"ALTER TABLE firings DROP COLUMN person",
	"DROP TABLE verdicts",
}

// Undo025 takes back what schema 025 made, in SQLite and in Postgres
// alike, for the tests of the migrations before it.
var Undo025 = []string{
	"DELETE FROM meta WHERE key = 'episodes_from'",
	"ALTER TABLE marked_weeks DROP COLUMN episodes_white",
	"ALTER TABLE marked_weeks DROP COLUMN episodes",
	"ALTER TABLE marked_days DROP COLUMN episodes_white",
	"ALTER TABLE marked_days DROP COLUMN episodes",
	"ALTER TABLE weeks DROP COLUMN episodes_white",
	"ALTER TABLE weeks DROP COLUMN episodes",
	"ALTER TABLE days DROP COLUMN episodes_white",
	"ALTER TABLE days DROP COLUMN episodes",
	"ALTER TABLE firings DROP COLUMN episode_white",
	"ALTER TABLE firings DROP COLUMN episode",
}

// Undo024 takes back what schema 024 made, in SQLite and in Postgres
// alike, for the tests of the migrations before it.
var Undo024 = []string{
	"ALTER TABLE impact_rules DROP COLUMN require_labels",
	"ALTER TABLE impact_rules DROP COLUMN lead_ns",
	"ALTER TABLE impact_signals DROP COLUMN lead_ns",
}

// Undo023 takes back what schema 023 made, in SQLite and in Postgres
// alike, for the tests of the migrations before it.
var Undo023 = []string{
	"DROP TABLE backtest_tokens",
	"DROP TABLE backtests",
}
