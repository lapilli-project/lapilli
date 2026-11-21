package ledger

import (
	"context"
	"slices"
	"testing"
	"time"

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
