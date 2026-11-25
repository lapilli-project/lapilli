package replay

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
)

// A Grafana rule whose condition reads Prometheus with a Loki query beside
// it: the Loki query is asked too, through Grafana's data source proxy, so
// the rule's no data is known for each of its queries, and the rule is
// replayed whole (docs/design/loki.md).

// asideRules is mixed (verify_test.go), its Loki query read as one: Grafana
// said its data source is Loki.
func asideRules(t *testing.T, doc string) rules.Rule {
	t.Helper()
	rs, err := rules.Parse([]byte(doc))
	if err != nil || len(rs) != 1 {
		t.Fatalf("%v %+v", err, rs)
	}
	rules.Replaying{Kinds: map[string]string{"loki": rules.KindLoki, "prom": "prometheus"}}.Apply(rs)
	if rs[0].Unsupported != "" {
		t.Fatal(rs[0].Unsupported)
	}
	return rs[0]
}

// logsAt is a Loki history answering for the Grafana data source loki
// only, and noting for which rule it was asked.
func logsAt(t *testing.T, prom History, loki History) History {
	t.Helper()
	return WithLogs(prom, func(r rules.Rule) (History, error) {
		if r.Logs != "loki" || r.Graph == nil {
			t.Errorf("asked for logs %q of a Grafana rule: %v", r.Logs, r.Graph != nil)
		}
		return loki, nil
	})
}

func TestAsideLokiRead(t *testing.T) {
	r := asideRules(t, mixed)
	g := r.Graph
	if g.Aside["L"] != "loki" || g.Queries["L"] == "" || g.Data["L"].UID != "loki" || len(g.Other) != 0 || r.Logs != "" {
		t.Errorf("Aside %v, Queries %v, Data %v, Other %v, Logs %q", g.Aside, g.Queries, g.Data, g.Other, r.Logs)
	}
	if got := g.AsideUIDs(); !slices.Equal(got, []string{"loki"}) {
		t.Errorf("AsideUIDs %v", got)
	}
	if slices.Contains(r.Unseen(), rules.UnseenOther) {
		t.Errorf("a Loki query beside the condition is asked: %v", r.Unseen())
	}
	// A log query returns lines: left as a query on another data source.
	logq := asideRules(t, strings.Replace(mixed, `sum(count_over_time({app="shop"} |= "error" [5m]))`, `{app="shop"} |= "error"`, 1))
	if len(logq.Graph.Aside) != 0 || logq.Graph.Other["L"] != "loki" || !slices.Contains(logq.Unseen(), rules.UnseenOther) {
		t.Errorf("a log query: Aside %v Other %v", logq.Graph.Aside, logq.Graph.Other)
	}
	// No way to the Loki: left as another data source's query.
	rs, _ := rules.Parse([]byte(strings.Replace(mixed, `datasourceUid: loki, model: {`, `datasourceUid: loki, model: {datasource: {type: loki}, `, 1)))
	if len(rs[0].Graph.Aside) != 1 {
		t.Fatalf("the model says Loki: %v", rs[0].Graph.Aside)
	}
	rules.Replaying{NoGrafanaLoki: true}.Apply(rs)
	if g := rs[0].Graph; len(g.Aside) != 0 || g.Other["L"] != "loki" || g.Queries["L"] != "" || g.Data["L"].UID != "" || !slices.Contains(rs[0].Unseen(), rules.UnseenOther) {
		t.Errorf("without --grafana or --loki: %+v", g)
	}
}

// TestAsideLokiNoData: the rule is NoData while its Loki query, which
// returned something before, returns nothing, as when its Prometheus query
// does; and it is asked of Loki, through the rule's own data source uid,
// never of Prometheus.
func TestAsideLokiNoData(t *testing.T) {
	r := asideRules(t, mixed)
	promH := history{"x": {warm(at(nil, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1), 1)}}
	// Loki: a line each minute but minutes 3 and 4.
	lokiH := &points{name: "loki", v: func(t time.Time) (float64, bool) {
		m := int(t.Sub(t0) / time.Minute)
		return 3, m != 3 && m != 4
	}}
	_, fs := Run(context.Background(), logsAt(t, promH, lokiH), one([]rules.Rule{r}), t0, t0.Add(8*time.Minute), time.Minute, 1)
	nodata := 0
	for _, f := range fs {
		if f.Labels["grafana_alertstate"] == "nodata" {
			nodata++
			if f.Labels["datasource_uid"] != "loki" || f.Labels["ref_id"] != "L" || f.Start != t0.Add(3*time.Minute) || f.End != t0.Add(5*time.Minute) {
				t.Errorf("NoData alert %v %s-%s", f.Labels, f.Start.Format("15:04"), f.End.Format("15:04"))
			}
		}
	}
	if nodata != 1 {
		t.Errorf("%d NoData alerts, want one: %s", nodata, spans(fs))
	}
	if len(lokiH.asked) == 0 {
		t.Error("Loki wasn't asked")
	}
	for _, a := range lokiH.asked {
		if !strings.HasPrefix(a, `sum(count_over_time({app="shop"} |= "error" [5m])) `) {
			t.Errorf("asked Loki %q", a)
		}
	}
	// The Prometheus is never asked what Loki is.
	for expr := range promH {
		if strings.Contains(expr, "count_over_time") {
			t.Error(expr)
		}
	}
	// With no Loki query's data, NoData is never known: nothing before.
	silent := &points{name: "loki", v: func(time.Time) (float64, bool) { return 0, false }}
	ran, fs := Run(context.Background(), logsAt(t, promH, silent), one([]rules.Rule{r}), t0, t0.Add(8*time.Minute), time.Minute, 1)
	if ran[0].Unsupported != "" || spans(fs) != "0-on[]" {
		t.Errorf("a Loki with no history: %q %s", ran[0].Unsupported, spans(fs))
	}
	// Without a Loki to ask, the rule isn't replayed, as one of LogQL isn't.
	ran, _ = Run(context.Background(), promH, one([]rules.Rule{r}), t0, t0.Add(8*time.Minute), time.Minute, 1)
	if !strings.Contains(ran[0].Unsupported, "no Loki to ask") {
		t.Errorf("no Loki: %q", ran[0].Unsupported)
	}
}

// failingLoki fails, or refuses, as a Loki does.
type failingLoki struct{ err error }

func (f failingLoki) QueryRange(context.Context, string, time.Time, time.Time, time.Duration) ([]prom.Series, error) {
	return nil, f.err
}

// TestAsideLokiFails: a Loki query beside the condition that fails, or is
// refused, doesn't fail the rule: whether it returned anything isn't known,
// so it makes nothing NoData, and the rule says so (Failed, UnseenFailed).
// A Prometheus query refused as written is no data to Grafana; for a Loki
// query Grafana's query errs, which isn't no data.
func TestAsideLokiFails(t *testing.T) {
	r := asideRules(t, mixed)
	promH := history{"x": {warm(at(nil, 1, 1, 1, 1, 1), 1)}}
	for name, err := range map[string]error{"failing": errors.New("503 Service Unavailable"), "refused": &prom.APIError{Status: 400, Msg: "parse error"}} {
		ran, fs := Run(context.Background(), logsAt(t, promH, failingLoki{err}), one([]rules.Rule{r}), t0, t0.Add(4*time.Minute), time.Minute, 1)
		if ran[0].Unsupported != "" || spans(fs) != "0-on[]" || !slices.Equal(ran[0].Failed, []string{"L"}) || !slices.Contains(ran[0].Unseen(), rules.UnseenFailed) {
			t.Errorf("%s: %q %s %v %v", name, ran[0].Unsupported, spans(fs), ran[0].Failed, ran[0].Unseen())
		}
	}
}

// TestAsideLokiRange: a range query beside the condition returned something
// when a point is in its window: the day before is told by the query over
// the day, as for a rule on Loki, and a window with no point is no data.
func TestAsideLokiRange(t *testing.T) {
	doc := strings.Replace(mixed, `datasourceUid: loki, model: {expr: 'sum(count_over_time({app="shop"} |= "error" [5m]))', instant: true}`,
		`datasourceUid: loki, relativeTimeRange: {from: 120, to: 0}, model: {expr: 'sum(count_over_time({app="shop"}[1m]))', queryType: range, intervalMs: 60000}`, 1)
	r := asideRules(t, doc)
	if g := r.Graph; len(g.Aside) != 2 || g.Data["L"].Present == "L" || !g.Asks[g.Data["L"].Present].Logs {
		t.Fatalf("read %+v", g)
	}
	promH := history{"x": {warm(at(nil, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1), 1)}}
	// Points at minutes 0 to 2 and 9 on: a window of 2 minutes has none
	// from minute 5 (3 and 4 are empty) to 8.
	lokiH := &points{name: "loki", v: func(t time.Time) (float64, bool) {
		m := int(t.Sub(t0) / time.Minute)
		return 1, m <= 2 || m >= 9
	}}
	_, fs := Run(context.Background(), logsAt(t, promH, lokiH), one([]rules.Rule{r}), t0, t0.Add(11*time.Minute), time.Minute, 1)
	var got []string
	for _, f := range fs {
		if f.Labels["grafana_alertstate"] == "nodata" {
			got = append(got, fmt.Sprintf("%s-%s", f.Start.Sub(t0), f.End.Sub(t0)))
		}
	}
	if !slices.Equal(got, []string{"5m0s-9m0s"}) {
		t.Errorf("NoData %v: %s", got, spans(fs))
	}
}

// TestPlanAside: the requests Plan counts for a rule with a Loki query
// beside its condition are those Run sends, by history.
func TestPlanAside(t *testing.T) {
	for _, doc := range []string{mixed, strings.Replace(mixed, `datasourceUid: loki, model: {expr: 'sum(count_over_time({app="shop"} |= "error" [5m]))', instant: true}`,
		`datasourceUid: loki, relativeTimeRange: {from: 3600, to: 0}, model: {expr: 'sum(count_over_time({app="shop"}[1m]))', queryType: range, intervalMs: 1000}`, 1)} {
		r := asideRules(t, doc)
		mu := &sync.Mutex{}
		by := map[string]int{}
		promH := &tallied{mu: mu, name: "", by: by, asks: map[request]int{}}
		loki := &tallied{mu: mu, name: "grafana/loki", by: by, asks: map[request]int{}, loki: true}
		h := WithLogs(promH, func(rules.Rule) (History, error) { return loki, nil })
		to := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
		from := to.Add(-3 * 24 * time.Hour)
		vs := one([]rules.Rule{r})
		plan := Plan(h, vs, from, to, time.Minute)
		ran, _ := Run(context.Background(), h, vs, from, to, time.Minute, 1)
		if ran[0].Unsupported != "" {
			t.Fatal(ran[0].Unsupported)
		}
		if by["grafana/loki"] == 0 || by[""] == 0 {
			t.Fatalf("asked %v: both histories should be", by)
		}
		for name, n := range by {
			if plan.By[name] != n {
				t.Errorf("Plan counts %d to %q, Run sent %d (%v)", plan.By[name], name, n, plan.By)
			}
		}
		if plan.Requests != by["grafana/loki"]+by[""] {
			t.Errorf("Plan counts %d requests, Run sent %v", plan.Requests, by)
		}
		// Without a Loki to ask, nothing is asked of either.
		if p := Plan(promH, vs, from, to, time.Minute); p.Requests != 0 {
			t.Errorf("no Loki: %+v", p)
		}
	}
}
