package ledger

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// grafanaVersion is a version holding a replayed rule and Orders slow, a
// Grafana rule Lapilli can't replay, under the names given.
func grafanaVersion(t *testing.T, l *Ledger, since time.Time, ns ...string) {
	t.Helper()
	docs := []rules.Doc{{Source: "rules.yaml", Data: []byte("groups:\n  - name: g\n    rules:\n      - alert: Replayed\n        expr: up == 0\n")}}
	if _, err := l.AddVersion(context.Background(), Version{Since: since, Digest: since.String(), Docs: docs, Names: append([]string{"Replayed"}, ns...)}); err != nil {
		t.Fatal(err)
	}
}

// A Grafana rule Lapilli can't replay and what Grafana's own history says
// it fired, copied in under GrafanaSource, are one rule, named as the rule
// is judged (docs/design/grafanajudged.md): through runs, which keep its
// firings, and versions; by name, whatever labels its alerts carry, an
// alarm_id among them. Its record covers what is copied in. No longer
// judged so, it is named apart, as a source's rule is.
func TestTiedGrafanaHistory(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if err := l.SetFromHistory(ctx, []string{"Orders slow", "Orders slow"}); err != nil {
		t.Fatal(err)
	}
	if got, err := l.FromHistory(ctx); err != nil || !slices.Equal(got, []string{"Orders slow"}) {
		t.Fatalf("kept %v %v", got, err)
	}
	grafanaVersion(t, l, t0, "Orders slow")
	run := func(from, to time.Time) {
		t.Helper()
		f := Found{From: from, To: to, Rules: []Rule{{Name: "Orders slow", Unsupported: "its condition reads a mysql data source: only PromQL and LogQL are replayed"},
			{Name: "Replayed"}}, Run: Run{Kind: "test", Started: to, Finished: to, From: from, To: to}}
		if err := l.Record(ctx, f, opts); err != nil {
			t.Fatal(err)
		}
	}
	run(t0, t0.Add(3*time.Hour))
	if ties, err := l.Ties(ctx); err != nil || len(ties) != 0 {
		t.Errorf("nothing copied in, yet tied: %+v %v", ties, err)
	}
	labels := map[string]string{"service": "orders", AlarmID: "its own label"}
	putFirings(t, l, GrafanaSource, t0, t0.Add(3*time.Hour), []firing.Firing{{Rule: "Orders slow", Labels: labels, Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)}})
	want := []string{"Orders slow <grafana>", "Replayed <>"}
	if got := names(t, l); !slices.Equal(got, want) {
		t.Errorf("names\n got %q\nwant %q", got, want)
	}
	run(t0.Add(3*time.Hour), t0.Add(6*time.Hour))
	grafanaVersion(t, l, t0.Add(4*time.Hour), "Orders slow")
	putFirings(t, l, GrafanaSource, t0.Add(3*time.Hour), t0.Add(6*time.Hour), []firing.Firing{{Rule: "Orders slow", Labels: labels, Start: t0.Add(4 * time.Hour)}})
	run(t0.Add(6*time.Hour), t0.Add(9*time.Hour))
	if got := names(t, l); !slices.Equal(got, want) {
		t.Errorf("after runs and a version\n got %q\nwant %q", got, want)
	}
	fs, err := l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(9 * time.Hour), Rule: "Orders slow"})
	if err != nil || len(fs) != 2 {
		t.Errorf("firings %+v %v", fs, err)
	}
	open, err := l.OpenFirings(ctx, GrafanaSource, t0.Add(9*time.Hour))
	if err != nil || len(open) != 1 || open[0].Rule != "Orders slow" || !open[0].Start.Equal(t0.Add(4*time.Hour)) || open[0].Labels["service"] != "orders" {
		t.Errorf("open: %+v %v", open, err)
	}
	ties, err := l.Ties(ctx)
	if x := ties["Orders slow"]; err != nil || x.Source != GrafanaSource || x.Since != t0.Unix() || x.To != t0.Add(6*time.Hour).Unix() || x.Why != "" {
		t.Errorf("ties: %+v %v", ties, err)
	}
	rep, err := l.Report(ctx, t0, t0.Add(9*time.Hour), opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(rep.Rules, func(r verdict.RuleResult) bool { return r.Rule == "Orders slow" }); i < 0 || !rep.Rules[i].ByHistory() || rep.Rules[i].Firings != 2 {
		t.Errorf("report: %+v", rep.Rules)
	}
	// No longer judged from the history: a source's rule, named apart.
	if err := l.SetFromHistory(ctx, nil); err != nil {
		t.Fatal(err)
	}
	run(t0.Add(9*time.Hour), t0.Add(12*time.Hour))
	if got := names(t, l); !slices.Equal(got, []string{"Orders slow <>", "Orders slow [grafana] <grafana>", "Replayed <>"}) {
		t.Errorf("no longer judged from the history: %q", got)
	}
}

// A Grafana rule whose alerts carry two severities, templated or changed
// once, is one recorded rule, tied to it.
func TestTiedGrafanaHistorySeverities(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if err := l.SetFromHistory(ctx, []string{"Orders slow"}); err != nil {
		t.Fatal(err)
	}
	grafanaVersion(t, l, t0, "Orders slow")
	f := Found{From: t0, To: t0.Add(3 * time.Hour), Rules: []Rule{{Name: "Orders slow", Unsupported: "its condition reads a mysql data source: only PromQL and LogQL are replayed"},
		{Name: "Replayed"}}, Run: Run{Kind: "test", Started: t0, Finished: t0, From: t0, To: t0.Add(3 * time.Hour)}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	putFirings(t, l, GrafanaSource, t0, t0.Add(3*time.Hour), []firing.Firing{
		{Rule: "Orders slow", Labels: map[string]string{"severity": "critical"}, Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)},
		{Rule: "Orders slow", Labels: map[string]string{"severity": "warning", "pod": "b"}, Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)}})
	if got := names(t, l); !slices.Equal(got, []string{"Orders slow <grafana>", "Replayed <>"}) {
		t.Errorf("names %q", got)
	}
	ties, err := l.Ties(ctx)
	if err != nil || ties["Orders slow"].Source != GrafanaSource {
		t.Errorf("ties: %+v %v", ties, err)
	}
	rep, err := l.Report(ctx, t0, t0.Add(3*time.Hour), opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(rep.Rules, func(r verdict.RuleResult) bool { return r.Rule == "Orders slow" }); i < 0 || rep.Rules[i].Firings != 2 || !rep.Rules[i].ByHistory() {
		t.Errorf("report: %+v", rep.Rules)
	}
}

// A rule judged from Grafana's history has the severity its labels say, as
// every other rule has its own: kept by SetJudgedSeverity, given by
// RuleTable and the report, though the recorded rule it is copied in as
// keeps none. One no longer judged so has none of it; a severity too long
// is cut.
func TestJudgedSeverity(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if err := l.SetFromHistory(ctx, []string{"Orders slow"}); err != nil {
		t.Fatal(err)
	}
	if err := l.SetJudgedSeverity(ctx, map[string]string{"Orders slow": "page", "Gone": ""}); err != nil {
		t.Fatal(err)
	}
	grafanaVersion(t, l, t0, "Orders slow")
	f := Found{From: t0, To: t0.Add(3 * time.Hour), Rules: []Rule{{Name: "Orders slow", Severity: "page", Unsupported: "its condition reads a mysql data source: only PromQL and LogQL are replayed"},
		{Name: "Replayed", Severity: "ticket"}}, Run: Run{Kind: "test", Started: t0, Finished: t0, From: t0, To: t0.Add(3 * time.Hour)}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	putFirings(t, l, GrafanaSource, t0, t0.Add(3*time.Hour), []firing.Firing{
		{Rule: "Orders slow", Labels: map[string]string{"service": "orders"}, Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)}})
	severities := func() map[string]string {
		t.Helper()
		table, err := l.RuleTable(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, r := range table {
			out[r.Name+" <"+r.Source+">"] = r.Severity
		}
		return out
	}
	if got := severities(); got["Orders slow <grafana>"] != "page" || got["Replayed <>"] != "ticket" {
		t.Errorf("severities: %v", got)
	}
	rep, err := l.Report(ctx, t0, t0.Add(3*time.Hour), opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(rep.Rules, func(r verdict.RuleResult) bool { return r.Rule == "Orders slow" }); i < 0 || rep.Rules[i].Severity != "page" {
		t.Errorf("report: %+v", rep.Rules)
	}
	// Cut to MaxSeverityLen, at a character.
	long := strings.Repeat("é", MaxSeverityLen)
	if err := l.SetJudgedSeverity(ctx, map[string]string{"Orders slow": long}); err != nil {
		t.Fatal(err)
	}
	if got := severities()["Orders slow <grafana>"]; len(got) > MaxSeverityLen || !utf8.ValidString(got) || !strings.HasPrefix(long, got) || got == "" {
		t.Errorf("cut to %d bytes: %q", len(got), got)
	}
	// No longer judged so: named apart, with none.
	if err := l.SetFromHistory(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := l.SetJudgedSeverity(ctx, nil); err != nil {
		t.Fatal(err)
	}
	f.From, f.To = t0.Add(3*time.Hour), t0.Add(6*time.Hour)
	f.Run.From, f.Run.To, f.Run.Started, f.Run.Finished = f.From, f.To, f.To, f.To
	if err := l.Record(ctx, f, opts); err != nil {
		t.Fatal(err)
	}
	if got := severities(); got["Orders slow [grafana] <grafana>"] != "" {
		t.Errorf("no longer judged: %v", got)
	}
}
