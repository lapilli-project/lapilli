package ledger

import (
	"context"
	"encoding/json"
	"slices"
	"time"
	"unicode/utf8"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/verdict"
)

// Grafana's rules Lapilli can't replay, judged from what Grafana's own
// alert state history says they fired (docs/design/grafanajudged.md):
// lapilli serve copies their firings in under a source of its own,
// GrafanaSource, which no job may write to, and each such rule and the
// recorded rule of its name are one rule, as a cloud's alarm and the
// history copied in for it are (ties.go). rules.source and rules.sent say
// it, the names of the rules judged so are kept in meta, and the periods a
// rule was left out of the copies in left_out (leftout.go).

// GrafanaSource is the source Grafana's history of the rules Lapilli
// can't replay is copied in under.
const GrafanaSource = verdict.GrafanaSource

// GrafanaCopier is who lapilli serve copies Grafana's history in as: a
// source of the name a job wrote to before, as one could before 0.68,
// isn't serve's to write over.
const GrafanaCopier = "grafana-history"

// judgedKey is the meta key the names of the rules judged from Grafana's
// history are kept under, as JSON.
const judgedKey = "grafana.judged"

// SetFromHistory keeps the names, as the rules in effect are judged under,
// of those judged from Grafana's own alert state history: each is the
// recorded rule of its name under GrafanaSource, once one is copied in.
// Kept as they were, nothing is written.
func (l *Ledger) SetFromHistory(ctx context.Context, names []string) error {
	names = slices.Compact(slices.Sorted(slices.Values(names)))
	if names == nil {
		names = []string{}
	}
	b, err := json.Marshal(names)
	if err != nil {
		return err
	}
	if was, err := l.meta(ctx, l.r, judgedKey); err != nil || was == string(b) || was == "" && len(names) == 0 {
		return err
	}
	return l.write(ctx, func(t *txn) error { return setMeta(t.ctx, t, judgedKey, string(b)) })
}

// FromHistory are the names kept by SetFromHistory.
func (l *Ledger) FromHistory(ctx context.Context) ([]string, error) {
	return l.fromHistory(ctx, l.r)
}

func (l *Ledger) fromHistory(ctx context.Context, q querier) ([]string, error) {
	v, err := l.meta(ctx, q, judgedKey)
	if err != nil || v == "" {
		return nil, err
	}
	var names []string
	return names, json.Unmarshal([]byte(v), &names)
}

// severityKey is the meta key the severities of the rules judged from
// Grafana's history are kept under, as JSON, by the name each is judged
// under.
const severityKey = "grafana.severity"

// MaxSeverityLen is the most bytes of a severity kept of a rule judged
// from Grafana's history: a label's value, as an alert's are kept
// (verdict.KeptLabels).
const MaxSeverityLen = 256

// SetJudgedSeverity keeps, by the name each is judged under, the severity
// of the rules judged from Grafana's own alert state history, as its
// labels say it: as a run reads every other rule's (Severities). The
// recorded rule each is copied in as keeps none in rules.severity, which
// is how a source tells its rules apart, and Grafana's history is of a
// rule whatever severities its alerts carry (PutFirings); RuleTable gives
// it from here. A severity past MaxSeverityLen is cut. Kept as they were,
// nothing is written.
func (l *Ledger) SetJudgedSeverity(ctx context.Context, sev map[string]string) error {
	keep := map[string]string{}
	for name, s := range sev {
		if s = cutText(s, MaxSeverityLen); s != "" {
			keep[name] = s
		}
	}
	b, err := json.Marshal(keep)
	if err != nil {
		return err
	}
	if was, err := l.meta(ctx, l.r, severityKey); err != nil || was == string(b) || was == "" && len(keep) == 0 {
		return err
	}
	return l.write(ctx, func(t *txn) error { return setMeta(t.ctx, t, severityKey, string(b)) })
}

// judgedSeverity are the severities SetJudgedSeverity kept, by name.
func (l *Ledger) judgedSeverity(ctx context.Context, q querier) (map[string]string, error) {
	v, err := l.meta(ctx, q, severityKey)
	if err != nil || v == "" {
		return nil, err
	}
	var out map[string]string
	return out, json.Unmarshal([]byte(v), &out)
}

// cutText is s cut to at most n bytes, at the start of a character.
func cutText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// grafanaTies are the ties of those of names judged from Grafana's
// history: each to the recorded rule of its name, which serve sends its
// firings under.
func (l *Ledger) grafanaTies(ctx context.Context, q querier, names []string) ([]Tie, error) {
	judged, err := l.fromHistory(ctx, q)
	if err != nil || len(judged) == 0 {
		return nil, err
	}
	in := make(map[string]bool, len(names))
	for _, n := range names {
		in[n] = true
	}
	var out []Tie
	for _, n := range judged {
		if in[n] {
			out = append(out, Tie{Source: GrafanaSource, Rule: n, Name: n})
		}
	}
	return out, nil
}

// OpenFirings are a source's firings still on, as it last said, that
// started before before: each under the name the source sent it by, with
// its labels.
func (l *Ledger) OpenFirings(ctx context.Context, source string, before time.Time) ([]firing.Firing, error) {
	rows, err := l.r.QueryContext(ctx, `SELECT r.sent, s.labels, f.start_at FROM firings f
		JOIN series s ON s.id = f.series_id JOIN rules r ON r.id = f.rule_id
		WHERE f.end_at IS NULL AND f.start_at < ? AND r.source = ?`, before.Unix(), source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []firing.Firing
	for rows.Next() {
		var f firing.Firing
		var labels string
		var start int64
		if err := rows.Scan(&f.Rule, &labels, &start); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(labels), &f.Labels); err != nil {
			return nil, err
		}
		f.Start = unix(start)
		out = append(out, f)
	}
	return out, rows.Err()
}

// RecordedCount is how many firings copied in under a source start in
// [from, to).
func (l *Ledger) RecordedCount(ctx context.Context, source string, from, to time.Time) (int, error) {
	var n int
	err := l.r.QueryRowContext(ctx, `SELECT count(*) FROM firings f JOIN rules r ON r.id = f.rule_id
		WHERE f.start_at >= ? AND f.start_at < ? AND r.source = ?`, from.Unix(), to.Unix(), source).Scan(&n)
	return n, err
}

// Recordable says why a firing can't be copied in now, as PutFirings
// refuses a write holding it: nil when it can be.
func Recordable(f firing.Firing) error {
	return recordable(1, f, time.Now().Add(skew), time.Time{}, time.Time{})
}
