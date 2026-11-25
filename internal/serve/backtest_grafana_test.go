package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
)

// prGrafana is a Grafana alerting provisioning file of these rules, in a
// group of the pull request's own.
func prGrafana(rs ...string) string {
	return "apiVersion: 1\ngroups:\n  - orgId: 1\n    name: pr\n    folder: f\n    interval: 1m\n    rules:\n" + strings.Join(rs, "")
}

// gRule is a Grafana rule of a provisioning file: its uid, its title, its
// condition and its data.
func gRule(uid, title, cond string, data ...string) string {
	return fmt.Sprintf("      - uid: %s\n        title: %s\n        condition: %s\n        data:\n%s", uid, title, cond, strings.Join(data, ""))
}

// gQuery is a query of a Grafana rule's data, its model in YAML's flow
// style, the data source's uid quoted as it is.
func gQuery(ref, uid, model string) string {
	return fmt.Sprintf("          - refId: %s\n            datasourceUid: %q\n            model: %s\n", ref, uid, model)
}

// gRange is gQuery with a relativeTimeRange, in seconds.
func gRange(ref, uid string, from, to int, model string) string {
	return fmt.Sprintf("          - refId: %s\n            datasourceUid: %q\n            relativeTimeRange: {from: %d, to: %d}\n            model: %s\n", ref, uid, from, to, model)
}

// inEffectGrafana are Grafana rules serve runs: one replayed on prom-a, a
// query beside its condition on notes-a, which isn't asked; one replayed
// on loki-a, a Loki data source asked of --loki; one paused on paused-a;
// one on Grafana's own data source's uid, and one on a uid Grafana
// wouldn't make, each read as PromQL, their types not said.
var inEffectGrafana = "apiVersion: 1\ngroups:\n  - orgId: 1\n    name: g\n    folder: f\n    interval: 1m\n    rules:\n" +
	gRule("e1", "PromInEffect", "A", gQuery("A", "prom-a", `{expr: 'errors > 5', datasource: {type: prometheus}}`),
		gQuery("N", "notes-a", `{expr: 'notes', datasource: {type: prometheus}}`)) +
	gRule("e2", "LokiInEffect", "A", gRange("A", "loki-a", 300, 0, `{expr: 'sum by (service) (count_over_time({app="checkout"} |= "error" [5m])) > 5', queryType: instant, datasource: {type: loki}}`)) +
	"      - uid: e3\n        title: PausedInEffect\n        isPaused: true\n        condition: A\n        data:\n" + gQuery("A", "paused-a", `{expr: 'errors > 5'}`) +
	gRule("e4", "BuiltInEffect", "A", gQuery("A", "grafana", `{expr: 'errors > 5'}`)) +
	gRule("e5", "OddInEffect", "A", gQuery("A", "in/effect", `{expr: 'errors > 5'}`))

// lokiRules is a Loki ruler's rule file.
const lokiRules = `groups:
  - name: logs
    rules:
      - alert: CheckoutLogErrors
        expr: sum by (service) (count_over_time({app="checkout"} |= "error" [5m])) > 5
        for: 2m
`

// btGrafana is serve with the port on, running inEffectGrafana's rules
// beside ruleFile's, with --loki; with loki, a Loki ruler's rules too.
// Its Lokis are logs, given for every rule in LogQL.
func btGrafana(t *testing.T, loki bool, more func(*Config)) (*btSet, *history, *lokiLogs) {
	t.Helper()
	h, logs := &history{}, &lokiLogs{}
	b := btServer(t, replay.WithLogs(h, func(rules.Rule) (replay.History, error) { return logs, nil }), "", func(c *Config) {
		os.WriteFile(filepath.Join(c.RulesPath, "grafana.yaml"), []byte(inEffectGrafana), 0o644)
		c.Loki = "http://loki.example:3100"
		if loki {
			dir := filepath.Join(filepath.Dir(c.RulesPath), "loki")
			os.MkdirAll(dir, 0o755)
			os.WriteFile(filepath.Join(dir, "logs.yaml"), []byte(lokiRules), 0o644)
			c.LokiRules = dir
		}
		if more != nil {
			more(c)
		}
	})
	return b, h, logs
}

// btAdding is a body whose pull request adds these files beside
// ruleFile's: each a path and its content, and, with loki, a Loki ruler's.
func btAdding(files ...map[string]any) []byte {
	shop := map[string]any{"path": "shop.yaml", "content": ruleFile}
	out, _ := json.Marshal(map[string]any{"before": []map[string]any{shop}, "after": append([]map[string]any{shop}, files...), "commit": "0123abcd"})
	return out
}

func added(path, content string, loki bool) map[string]any {
	f := map[string]any{"path": path, "content": content}
	if loki {
		f["loki"] = true
	}
	return f
}

// The rules in effect read, as the port holds a pull request's to them:
// the data sources replayed, by uid and as asked, never one only beside a
// condition, nor a paused rule's; a Loki ruler's rule.
func TestBacktestReadsInEffect(t *testing.T) {
	parallel(t)
	b, _, _ := btGrafana(t, true, nil)
	e, err := b.s.readsInEffect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !e.promql["prom-a"] || !e.logql["loki-a"] || e.promql["loki-a"] || e.logql["prom-a"] || e.promql["notes-a"] || e.promql["paused-a"] || !e.loki {
		t.Errorf("%+v", e)
	}
	b, _, _ = btGrafana(t, false, nil)
	if e, _ := b.s.readsInEffect(context.Background()); e.loki {
		t.Errorf("no Loki ruler's rule in effect: %+v", e)
	}
	// With no Loki to ask, the rule on loki-a isn't replayed: it reads
	// nothing.
	b, _, _ = btGrafana(t, false, func(c *Config) { c.Loki = "" })
	if e, _ := b.s.readsInEffect(context.Background()); e.logql["loki-a"] || !e.promql["prom-a"] {
		t.Errorf("no Loki: %+v", e)
	}
}

// A pull request's Grafana rule is taken only on the data sources a rule in
// effect asks, by uid exactly and as it asks them: refused, one message
// for each, which names nothing of serve's, for a data source no rule in
// effect asks (one serve's Grafana said it has among them), a uid written
// otherwise (its case, a space, a prefix), a rule asking one taken and
// one not, a query beside its condition on one not, an expression's
// own copy of __expr__ in another case, Grafana's own data sources, a
// paused rule's, and a data source asked as LogQL where the rules in effect
// ask it as PromQL, and the reverse. Nothing is queued, and nothing asked.
func TestBacktestGrafanaRefused(t *testing.T) {
	parallel(t)
	b, h, logs := btGrafana(t, true, nil)
	h.Lock()
	h.exprs = nil
	h.Unlock()
	logs.Lock()
	logs.asked = nil
	logs.Unlock()
	// Serve's Grafana said these data sources are, and what type: one no
	// rule in effect asks among them.
	b.s.dataSources.Lock()
	b.s.dataSources.kinds = map[string]string{"prom-z": "prometheus", "prom-a": "prometheus", "loki-a": "loki"}
	b.s.dataSources.Unlock()
	prom := `{expr: 'errors > 1', datasource: {type: prometheus}}`
	for name, rule := range map[string]string{
		"a data source no rule in effect asks":   gRule("p1", "Raised", "A", gQuery("A", "prom-b", prom)),
		"one serve's Grafana says it has":        gRule("p1", "Raised", "A", gQuery("A", "prom-z", prom)),
		"its case":                               gRule("p1", "Raised", "A", gQuery("A", "PROM-A", prom)),
		"a space":                                gRule("p1", "Raised", "A", gQuery("A", "prom-a ", prom)),
		"a prefix":                               gRule("p1", "Raised", "A", gQuery("A", "prom-", prom)),
		"longer":                                 gRule("p1", "Raised", "A", gQuery("A", "prom-ab", prom)),
		"one taken and one not":                  gRule("p1", "Raised", "C", gQuery("A", "prom-a", prom), gQuery("B", "prom-b", prom), gQuery("C", "__expr__", `{type: math, expression: '$A + $B'}`)),
		"beside its condition":                   gRule("p1", "Raised", "A", gQuery("A", "prom-a", prom), gQuery("N", "notes-a", prom)),
		"__expr__ in another case":               gRule("p1", "Raised", "B", gQuery("A", "prom-a", prom), gQuery("B", "__EXPR__", `{type: math, expression: '$A'}`)),
		"Grafana's own, though in effect":        gRule("p1", "Raised", "A", gQuery("A", "grafana", prom)),
		"a uid Grafana makes none of, in effect": gRule("p1", "Raised", "A", gQuery("A", "in/effect", prom)),
		"-- Mixed --":                            gRule("p1", "Raised", "A", gQuery("A", "-- Mixed --", prom)),
		"-- Dashboard --":                        gRule("p1", "Raised", "A", gQuery("A", "-- Dashboard --", prom)),
		"a paused rule's":                        gRule("p1", "Raised", "A", gQuery("A", "paused-a", prom)),
	} {
		code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", b.github(nil), btAdding(added("grafana.yaml", prGrafana(rule), false)))
		why, _ := m["error"].(string)
		if code != 400 || why != "Raised reads a data source no rule in effect reads: here a Grafana rule reads only the data sources the rules lapilli serve runs read already" {
			t.Errorf("%s: %d %q", name, code, why)
		}
	}
	// Asked as the other type, of a data source in effect whose type
	// serve's Grafana didn't say: the rule's own word is the rule's.
	b.s.dataSources.Lock()
	b.s.dataSources.kinds = nil
	b.s.dataSources.Unlock()
	for name, rule := range map[string]string{
		"prom-a as LogQL":  gRule("p1", "Raised", "A", gRange("A", "prom-a", 300, 0, `{expr: 'sum(count_over_time({app="x"}[5m])) > 1', queryType: instant, datasource: {type: loki}}`)),
		"loki-a as PromQL": gRule("p1", "Raised", "A", gQuery("A", "loki-a", `{expr: 'errors > 1', datasource: {type: prometheus}}`)),
	} {
		code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", b.plain, btAdding(added("grafana.yaml", prGrafana(rule), false)))
		if why, _ := m["error"].(string); code != 400 || !strings.Contains(why, "no rule in effect reads") {
			t.Errorf("%s: %d %q", name, code, why)
		}
	}
	if n, _ := b.s.l.BacktestsWaiting(context.Background()); len(n) != 0 {
		t.Errorf("queued: %v", n)
	}
	h.Lock()
	for expr := range h.exprs {
		if strings.Contains(expr, "errors > 1") {
			t.Errorf("asked %q", expr)
		}
	}
	h.Unlock()
	logs.Lock()
	if len(logs.asked) != 0 {
		t.Errorf("Loki asked %v", logs.asked)
	}
	logs.Unlock()
}

// A pull request's Grafana rule on a data source in effect is taken and
// replayed as the rules in effect are: its PromQL of serve's Prometheus,
// through the lane; its LogQL of serve's Loki. An expression's model naming
// another data source is the expression's alone: nothing reads it.
func TestBacktestGrafanaTaken(t *testing.T) {
	parallel(t)
	b, h, logs := btGrafana(t, false, nil)
	h.Lock()
	h.exprs = nil
	h.Unlock()
	logs.Lock()
	logs.asked = nil
	logs.Unlock()
	rule := gRule("p1", "ErrorsRaised", "C",
		gRange("A", "prom-a", 600, 0, `{expr: 'errors', datasource: {type: prometheus}}`),
		gQuery("B", "__expr__", `{type: reduce, expression: A, reducer: max, datasource: {uid: secret-ds, type: prometheus}, expr: 'secret_metric'}`),
		gQuery("C", "__expr__", `{type: threshold, expression: B, conditions: [{evaluator: {type: gt, params: [1]}}]}`)) +
		gRule("p2", "LogErrors", "A", gRange("A", "loki-a", 300, 0, `{expr: 'sum by (service) (count_over_time({app="checkout"} |= "error" [5m])) > 5', queryType: instant, datasource: {type: loki}}`))
	md, rep := b.done(t, b.github(nil), btAdding(added("grafana.yaml", prGrafana(rule), false)))
	if !strings.Contains(md, "`ErrorsRaised`") || !strings.Contains(md, "`LogErrors`") {
		t.Errorf("%s", md)
	}
	if sum, _ := rep["summary"].(map[string]any); sum == nil || sum["added"] != float64(2) || sum["unreplayed"] != float64(0) {
		t.Errorf("%v", rep["summary"])
	}
	h.Lock()
	var asked []string
	for expr := range h.exprs {
		asked = append(asked, expr)
	}
	h.Unlock()
	if !slices.ContainsFunc(asked, func(e string) bool { return strings.Contains(e, "errors") }) || slices.ContainsFunc(asked, func(e string) bool { return strings.Contains(e, "secret") }) {
		t.Errorf("asked %q", asked)
	}
	logs.Lock()
	if len(logs.asked) == 0 || slices.ContainsFunc(logs.asked, func(e string) bool { return strings.Contains(e, "secret") }) {
		t.Errorf("Loki asked %q", logs.asked)
	}
	logs.Unlock()
}

// A Loki ruler's rule, in a file the job says is one, is taken only where
// serve has --loki and its rules in effect hold one, and asked of it; a
// Grafana rule in a file said to be a Loki ruler's is refused.
func TestBacktestLokiRules(t *testing.T) {
	parallel(t)
	rule := strings.Replace(lokiRules, "> 5", "> 2", 1)
	for name, c := range map[string]struct {
		loki    bool
		more    func(*Config)
		code    int
		says    string
		content string
	}{
		"no Loki ruler's rule in effect": {false, nil, 400, "is a Loki ruler's rule: here one is taken only where", rule},
		"no --loki":                      {true, func(c *Config) { c.Loki = "" }, 400, "is a Loki ruler's rule: here one is taken only where", rule},
		"a Grafana rule said a Loki's":   {true, nil, 400, "isn't a Loki ruler's rule", prGrafana(gRule("p1", "Raised", "A", gRange("A", "loki-a", 300, 0, `{expr: 'sum(count_over_time({app="x"}[5m])) > 1', queryType: instant, datasource: {type: loki}}`)))},
		"taken":                          {true, nil, 202, "", rule},
	} {
		b, _, logs := btGrafana(t, c.loki, c.more)
		code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", b.plain, btAdding(added("loki.yaml", c.content, true)))
		if why, _ := m["error"].(string); code != c.code || !strings.Contains(why, c.says) {
			t.Errorf("%s: %d %q", name, code, why)
			continue
		}
		if code != 202 {
			continue
		}
		if got := laneOnce(t, b.s); got.State != ledger.BacktestDone {
			t.Fatalf("%s: %+v", name, got)
		}
		logs.Lock()
		if !slices.Contains(logs.asked, `sum by (service) (count_over_time({app="checkout"} |= "error" [5m])) > 2`) {
			t.Errorf("%s: Loki asked %q", name, logs.asked)
		}
		logs.Unlock()
	}
}

// A pull request's rules in LogQL and Grafana rules are held to bounds of
// their own, beside a rule's: a Grafana rule's queries and expressions,
// its relativeTimeRange, each query it asks as a rule's query is held, the
// relativeTimeRange added to what it reads back; a LogQL query a metric
// query, each stream named by a label's value, its range and offset within
// eight days, no subquery and no @, past a comment as well.
func TestBacktestGrafanaLokiBounds(t *testing.T) {
	parallel(t)
	b, _, _ := btGrafana(t, true, nil)
	prom := func(expr string) string { return fmt.Sprintf(`{expr: '%s', datasource: {type: prometheus}}`, expr) }
	var many, wide []string
	for i := range 9 {
		many = append(many, gQuery(fmt.Sprintf("Q%d", i), "prom-a", prom("errors")))
	}
	for i := range 17 {
		wide = append(wide, gQuery(fmt.Sprintf("E%d", i), "__expr__", `{type: math, expression: '$A'}`))
	}
	wide = append(wide, gQuery("A", "prom-a", prom("errors > 1")))
	grafana := map[string]struct{ rule, says string }{
		"9 queries":                   {gRule("p1", "Raised", "Q0", many...), "9 queries of data sources"},
		"17 expressions":              {gRule("p1", "Raised", "A", wide...), "8 and 16 at most"},
		"relativeTimeRange 9d":        {gRule("p1", "Raised", "B", gRange("A", "prom-a", 9*86400, 0, prom("errors")), gQuery("B", "__expr__", `{type: reduce, expression: A, reducer: max}`)), "before each evaluation (its relativeTimeRange)"},
		"a selector naming no metric": {gRule("p1", "Raised", "A", gQuery("A", "prom-a", prom(`{job="api"} > 1`))), "Raised's query A reads a selector that names no metric"},
		"7d read, ending 2d before":   {gRule("p1", "Raised", "A", gRange("A", "prom-a", 2*86400, 2*86400, prom("rate(errors[7d]) > 1"))), "Raised's query A reads past what is taken: here"},
		"for 49h":                     {strings.Replace(gRule("p1", "Raised", "A", gQuery("A", "prom-a", prom("errors > 1"))), "        condition:", "        for: 49h\n        condition:", 1), "48h"},
		"LogQL from 8d, a 5m range":   {gRule("p1", "Raised", "B", gRange("A", "loki-a", 8*86400, 0, `{expr: 'sum(count_over_time({app="x"}[5m]))', datasource: {type: loki}}`), gQuery("B", "__expr__", `{type: reduce, expression: A, reducer: max}`)), "Raised's query B reads past what is taken: here what a rule in LogQL reads"},
		// Asked again over the day before when it returns nothing: 7.5
		// days, and the day, past 8.
		"7.5d, and the day before":       {gRule("p1", "Raised", "A", gQuery("A", "prom-a", prom("rate(errors[7d] offset 12h) > 1"))), "Raised's query A, looked through a day further back when it returns nothing, reads past what is taken"},
		"LogQL 7.5d, and the day before": {gRule("p1", "Raised", "A", gRange("A", "loki-a", 300, 0, `{expr: 'sum(count_over_time({app="x"}[7d] offset 12h)) > 1', queryType: instant, datasource: {type: loki}}`)), "Raised's query A, looked through a day further back when it returns nothing, reads past what is taken"},
	}
	for name, c := range grafana {
		code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", b.github(nil), btAdding(added("grafana.yaml", prGrafana(c.rule), false)))
		if why, _ := m["error"].(string); code != 400 || !strings.Contains(why, c.says) {
			t.Errorf("%s: %d %q, want %q", name, code, why, c.says)
		}
	}
	loki := func(expr string) string {
		return fmt.Sprintf("groups:\n  - name: logs\n    rules:\n      - alert: Raised\n        expr: %q\n", expr)
	}
	for name, c := range map[string]struct{ expr, says string }{
		"log lines":                 {`{app="checkout"} |= "error"`, "log lines"},
		"a pattern alone":           {`sum(count_over_time({app=~".+"}[5m])) > 1`, "names no stream"},
		"a negation alone":          {`sum(count_over_time({app!="x"}[5m])) > 1`, "names no stream"},
		"an empty value":            {`sum(count_over_time({app=""}[5m])) > 1`, "names no stream"},
		"9 days":                    {`sum(count_over_time({app="x"}[9d])) > 1`, "past 8d"},
		"offset 7d after 2d":        {`sum(count_over_time({app="x"}[2d] offset 7d)) > 1`, "past 8d"},
		"a subquery":                {`max_over_time(sum(count_over_time({app="x"}[5m]))[1d:1m]) > 1`, "subquery"},
		"an offset after no range":  {`sum(count_over_time({app="x"}[5m])) offset 30d > 1`, "after no range"},
		"@":                         {`sum(count_over_time({app="x"}[5m] @ 0)) > 1`, "no @"},
		"9 days past a comment":     {"sum(count_over_time({app=\"x\"}[5m])) # \"\n + sum(count_over_time({app=\"x\"}[9d])) > 1", "past 8d"},
		"a wide one past a comment": {"sum(count_over_time({app=\"x\"}[5m])) # `\n + sum(count_over_time({app=~\".*\"}[5m])) > 1", "names no stream"},
	} {
		code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", b.plain, btAdding(added("loki.yaml", loki(c.expr), true)))
		if why, _ := m["error"].(string); code != 400 || !strings.Contains(why, c.says) {
			t.Errorf("%s: %d %q, want %q", name, code, why, c.says)
		}
	}
	if n, _ := b.s.l.BacktestsWaiting(context.Background()); len(n) != 0 {
		t.Errorf("queued: %v", n)
	}
}

// The lane asks a Loki through a client of its own, at its share of that
// Loki's pace, which the runs keep for it only while a backtest asking that
// Loki runs; with serve's tenant, which the request can't name.
func TestBacktestLokiLane(t *testing.T) {
	parallel(t)
	var mu sync.Mutex
	var tenants []string
	var held []bool
	var pace *prom.Pace
	answer := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}
	note := func() {
		if pace != nil {
			held = append(held, pace.Holding())
		}
	}
	lokiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tenants = append(tenants, r.Header.Get("X-Scope-OrgID"))
		note()
		mu.Unlock()
		answer(w, r)
	}))
	defer lokiSrv.Close()
	promSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		note()
		mu.Unlock()
		answer(w, r)
	}))
	defer promSrv.Close()
	runs := source.Loki{URL: lokiSrv.URL, Tenant: "tenant-a", MaxRate: 100}.History("test")
	logs := &source.Logs{Loki: runs}
	h := replay.WithLogs(prom.New(promSrv.URL, "test", prom.WithLimiter(prom.NewLimiter(100))), logs.For)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "logs.yaml"), []byte(lokiRules), 0o644)
	b := btServer(t, h, "", func(c *Config) { c.Loki, c.LokiTenant, c.LokiRules = lokiSrv.URL, "tenant-a", dir })
	mu.Lock()
	pace, tenants = runs.Limiter().Pace(), nil
	mu.Unlock()
	// The request names no tenant: a field it doesn't know is refused.
	body := btAdding()
	if code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", b.plain, append(body[:len(body)-1], []byte(`,"tenant":"tenant-b"}`)...)); code != 400 || !strings.Contains(m["error"].(string), "unknown field") {
		t.Errorf("a tenant: %d %v", code, m)
	}
	b.done(t, b.plain, btAdding(added("loki.yaml", strings.Replace(lokiRules, "> 5", "> 2", 1), true)))
	mu.Lock()
	if len(tenants) == 0 || slices.ContainsFunc(tenants, func(x string) bool { return x != "tenant-a" }) {
		t.Errorf("Loki asked as %q", tenants)
	}
	if !slices.Contains(held, true) {
		t.Errorf("Loki's share not kept while the backtest asking it ran: %v", held)
	}
	held = nil
	mu.Unlock()
	b.s.btPort.lanesMu.Lock()
	lane := b.s.btPort.lanes[runs]
	b.s.btPort.lanesMu.Unlock()
	if lane == nil || lane == runs || lane.Limiter() == runs.Limiter() || lane.Limiter().Pace().Rate() != 25 || pace.Reserved() != 0.25 || pace.Holding() {
		t.Fatalf("the lane's Loki: %v; the runs' reserving %g, holding %t", lane, pace.Reserved(), pace.Holding())
	}
	// A backtest of Prometheus's rules alone keeps none of Loki's share.
	b.done(t, b.plain, btBody(ruleFile, changedRules, nil))
	mu.Lock()
	defer mu.Unlock()
	if len(held) == 0 || slices.Contains(held, true) {
		t.Errorf("Loki's share kept for a backtest that asks no Loki: %v", held)
	}
}

// What the rules in effect read is read again before the lane runs a
// backtest: one whose data source no rule in effect reads by then fails,
// saying so, having asked nothing.
func TestBacktestGrafanaInEffectChanged(t *testing.T) {
	parallel(t)
	b, h, _ := btGrafana(t, false, nil)
	rule := gRule("p1", "ErrorsRaised", "A", gQuery("A", "prom-a", `{expr: 'errors > 1', datasource: {type: prometheus}}`))
	if code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", b.plain, btAdding(added("grafana.yaml", prGrafana(rule), false))); code != 202 {
		t.Fatalf("%d %v", code, m)
	}
	os.WriteFile(filepath.Join(b.s.cfg.RulesPath, "grafana.yaml"), []byte(strings.Replace(inEffectGrafana, `"prom-a"`, `"prom-c"`, 1)), 0o644)
	raw := ""
	if err := b.s.look(context.Background(), &raw); err != nil {
		t.Fatal(err)
	}
	if e, _ := b.s.readsInEffect(context.Background()); e.promql["prom-a"] {
		t.Fatalf("prom-a still in effect: %+v", e)
	}
	h.Lock()
	h.exprs = nil
	h.Unlock()
	if got := laneOnce(t, b.s); got.State != ledger.BacktestFailed || !strings.Contains(got.Error, "no rule in effect reads") {
		t.Errorf("%+v", got)
	}
	h.Lock()
	defer h.Unlock()
	if len(h.exprs) != 0 {
		t.Errorf("asked %v", h.exprs)
	}
}

// Loki's lexer skips a // comment and a /* */ one as well as #, and reads
// a ' as a character: a pull request's LogQL with one is refused as a
// whole, a Loki ruler's rule and a Grafana rule's on a Loki data source
// alike, so that a # or a quote inside one can't hide a wide selector, a
// long range or a log query from the checks; and a Grafana rule's log
// query behind a comment, which the replay doesn't take, asks Loki
// nothing.
func TestBacktestLogQLComments(t *testing.T) {
	parallel(t)
	b, _, _ := btGrafana(t, true, nil)
	loki := func(expr string) string {
		return fmt.Sprintf("groups:\n  - name: logs\n    rules:\n      - alert: Raised\n        expr: %q\n", expr)
	}
	grafana := func(expr string) []byte {
		rule := gRule("p1", "Wide", "A", gRange("A", "loki-a", 300, 0, `{expr: '`+expr+`', queryType: instant, datasource: {type: loki}}`))
		return btAdding(added("grafana.yaml", prGrafana(rule), false))
	}
	for _, expr := range []string{
		`/* # */ sum(count_over_time({namespace=~".+"}[30d])) > 1`,
		`/* " */ sum(count_over_time({namespace=~".+"}[30d])) > 1 /* " */`,
		`/**/{app="x"} |= "x"`,
		"// #\nsum(count_over_time({namespace=~\".+\"}[30d])) > 1",
	} {
		code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", b.plain, btAdding(added("loki.yaml", loki(expr), true)))
		if why, _ := m["error"].(string); code != 400 || !strings.Contains(why, "Raised has a // or /* comment, or a ' outside a string") {
			t.Errorf("a Loki ruler's %q: %d %q", expr, code, why)
		}
		if strings.Contains(expr, "\n") || strings.HasPrefix(expr, "/**/") {
			continue
		}
		code, m, _ = btAsk(t, b.h, "POST", "/v1/backtests", b.github(nil), grafana(expr))
		if why, _ := m["error"].(string); code != 400 || !strings.Contains(why, "Wide's query A has a // or /* comment, or a ' outside a string") {
			t.Errorf("a Grafana rule's %q: %d %q", expr, code, why)
		}
	}
	// A log query in brackets, or after a byte order mark, which Loki's
	// grammar and lexer read as one.
	for _, expr := range []string{`({app="x"} |= "x")`, "\uFEFF{app=\"x\"} |= \"x\""} {
		code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", b.plain, btAdding(added("loki.yaml", loki(expr), true)))
		if why, _ := m["error"].(string); code != 400 || !strings.Contains(why, "Raised asks Loki for log lines") {
			t.Errorf("a Loki ruler's %q: %d %q", expr, code, why)
		}
	}
	if n, _ := b.s.l.BacktestsWaiting(context.Background()); len(n) != 0 {
		t.Errorf("queued: %v", n)
	}
	b, _, logs := btGrafana(t, false, nil)
	for _, expr := range []string{`/**/{app="x"} |= "x"`, `({app="x"} |= "x")`} {
		if code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", b.github(nil), grafana(expr)); code == 202 {
			if got := laneOnce(t, b.s); got.State != ledger.BacktestDone {
				t.Errorf("a Grafana rule's log query %q: %s", expr, got.State)
			}
		} else if code != 400 {
			t.Errorf("a Grafana rule's log query %q: %d %v", expr, code, m)
		}
	}
	logs.Lock()
	defer logs.Unlock()
	if slices.ContainsFunc(logs.asked, func(s string) bool { return strings.Contains(s, `|= "x"`) }) {
		t.Errorf("a Grafana rule's log query: Loki asked %q", logs.asked)
	}
}

// A Grafana rule's query refused for what it reads is said by its refId,
// nothing of it quoted: the PromQL the replay asks has the macros put in,
// $__rate_interval with the scrape interval serve read of its data
// source, which a pull request could otherwise read back from the
// refusal. A Loki ruler's rule, which has no macros, is quoted as before.
func TestBacktestGrafanaRefusalQuotesNothing(t *testing.T) {
	parallel(t)
	b, _, _ := btGrafana(t, true, nil)
	b.s.dataSources.Lock()
	b.s.dataSources.ds = rules.DataSources{"prom-a": 37 * time.Second}
	b.s.dataSources.kinds = map[string]string{"prom-a": "prometheus", "loki-a": "loki"}
	b.s.dataSources.Unlock()
	for name, c := range map[string]struct{ expr, says string }{
		"reach":     {`rate(errors[$__rate_interval] offset 9d) > 1`, "Probe's query A reads past what is taken: here"},
		"a pattern": {`rate({__name__=~"err.+", job="$__rate_interval"}[5m]) > 1`, "Probe's query A reads a selector that names no metric"},
	} {
		rule := gRule("p1", "Probe", "A", gQuery("A", "prom-a", `{expr: '`+c.expr+`', datasource: {type: prometheus}}`))
		code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", b.github(nil), btAdding(added("grafana.yaml", prGrafana(rule), false)))
		why, _ := m["error"].(string)
		if code != 400 || !strings.Contains(why, c.says) || strings.Contains(why, "2m28s") || strings.Contains(why, "148") || strings.Contains(why, "37") {
			t.Errorf("%s: %d %q", name, code, why)
		}
	}
	loki := "groups:\n  - name: logs\n    rules:\n      - alert: Raised\n        expr: 'sum(count_over_time({app=\"x\"}[9d])) > 1'\n"
	if code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", b.plain, btAdding(added("loki.yaml", loki, true))); code != 400 || !strings.Contains(m["error"].(string), "Raised reads a range and its offset reaching 9d back, past 8d") {
		t.Errorf("a Loki ruler's rule: %d %v", code, m)
	}
}

// A Grafana rule's query that returns nothing is looked through over the
// day before, in a subquery at its group's interval (replay.ProbeStep):
// held to backtestPoints as any subquery is, an interval under 8 seconds
// is refused, 86,400 points a series at 1s; 8s, 10s, a minute and five,
// as Grafana's groups run, are taken.
func TestBacktestGrafanaProbePoints(t *testing.T) {
	parallel(t)
	b, _, _ := btGrafana(t, true, nil)
	prom := func(expr string) string { return fmt.Sprintf(`{expr: '%s', datasource: {type: prometheus}}`, expr) }
	// Each taken a newer commit of the pull request, which takes the place
	// of the one before.
	n := 0
	for interval, refused := range map[string]bool{"1s": true, "7s": true, "8s": false, "10s": false, "1m": false, "5m": false} {
		rule := strings.Replace(prGrafana(gRule("p1", "Raised", "A", gQuery("A", "prom-a", prom("errors > 1")))), "interval: 1m", "interval: "+interval, 1)
		n++
		body := strings.Replace(string(btAdding(added("grafana.yaml", rule, false))), `"commit":"0123abcd"`, `"commit":"`+commitN(n)+`"`, 1)
		code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", b.github(nil), []byte(body))
		why, _ := m["error"].(string)
		switch {
		case refused && (code != 400 || !strings.Contains(why, "an interval of 8s or more")):
			t.Errorf("interval %s: %d %q, want refused", interval, code, why)
		case !refused && code != 202:
			t.Errorf("interval %s: %d %q, want taken", interval, code, why)
		}
	}
}

// A rule in effect whose condition reads Prometheus with a Loki query beside
// it asks that Loki too, as a Loki data source is asked; so a pull request's
// rule is taken on it, and not as PromQL.
func TestBacktestReadsAsideLoki(t *testing.T) {
	parallel(t)
	beside := "apiVersion: 1\ngroups:\n  - orgId: 1\n    name: g2\n    folder: f\n    interval: 1m\n    rules:\n" +
		gRule("b1", "Beside", "A", gQuery("A", "prom-b", `{expr: 'errors > 5', instant: true, datasource: {type: prometheus}}`),
			gRange("L", "loki-b", 300, 0, `{expr: 'sum(count_over_time({app="checkout"}[5m]))', queryType: instant, datasource: {type: loki}}`))
	b, _, _ := btGrafana(t, false, func(c *Config) {
		os.WriteFile(filepath.Join(c.RulesPath, "beside.yaml"), []byte(beside), 0o644)
	})
	e, err := b.s.readsInEffect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !e.promql["prom-b"] || !e.logql["loki-b"] || e.promql["loki-b"] || e.logql["prom-b"] {
		t.Fatalf("%+v", e)
	}
	rs, err := rules.Parse([]byte(strings.Replace(beside, "uid: b1", "uid: b2", 1)))
	if err != nil || len(rs) != 1 {
		t.Fatal(err, rs)
	}
	if why := taken(rs[0], e); why != "" {
		t.Errorf("taken: %s", why)
	}
	// Not where it is asked as PromQL, nor where no rule in effect asks it.
	e.logql["loki-b"] = false
	if why := taken(rs[0], e); why == "" {
		t.Error("a Loki no rule in effect asks was taken")
	}
	e.logql["loki-b"], e.promql["loki-b"] = true, false
	if why := taken(rs[0], e); why != "" {
		t.Errorf("taken: %s", why)
	}
}

// A pull request's Grafana rule whose condition reads a data source in effect,
// with a Loki query beside it on another in effect, is replayed whole: the
// Loki query is held to what LogQL is held to, and asked of the Loki.
func TestBacktestAsideTaken(t *testing.T) {
	parallel(t)
	b, _, logs := btGrafana(t, false, nil)
	logs.Lock()
	logs.asked = nil
	logs.Unlock()
	rule := gRule("p1", "Beside", "A", gRange("A", "prom-a", 600, 0, `{expr: 'errors', instant: true, datasource: {type: prometheus}}`),
		gRange("L", "loki-a", 300, 0, `{expr: 'sum(count_over_time({app="checkout"}[5m]))', queryType: instant, datasource: {type: loki}}`))
	_, rep := b.done(t, b.github(nil), btAdding(added("grafana.yaml", prGrafana(rule), false)))
	if sum, _ := rep["summary"].(map[string]any); sum == nil || sum["added"] != float64(1) || sum["unreplayed"] != float64(0) {
		t.Errorf("%v", rep["summary"])
	}
	logs.Lock()
	if !slices.ContainsFunc(logs.asked, func(e string) bool { return strings.Contains(e, `{app="checkout"}[5m]`) }) {
		t.Errorf("Loki asked %q", logs.asked)
	}
	logs.Unlock()
}

// The Grafana rules serve runs are taken by vet on what the replay of each
// asks, as readsInEffect says it: not refused for the uid of a query beside
// the condition, nor for a uid of Grafana's own or an odd one that a rule in
// effect already asks.
func TestLeftAloneVetServesGrafana(t *testing.T) {
	parallel(t)
	b, _, _ := btGrafana(t, false, nil)
	ctx := context.Background()
	eff, err := b.s.readsInEffect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	v, ok, err := b.s.l.Latest(ctx)
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	rs, err := b.s.parse(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	a := &leftAlone{}
	for _, r := range rs {
		if strings.HasSuffix(r.Name, "InEffect") && r.Name != "LokiInEffect" && r.Name != "PausedInEffect" {
			a.alone = append(a.alone, aloneRule{rule: r})
		}
	}
	if len(a.alone) != 3 {
		t.Fatalf("%d rules", len(a.alone))
	}
	a.vet(&prepared{eff: eff})
	for _, r := range a.alone {
		if r.refused {
			t.Errorf("%s, a rule in effect, refused", r.rule.Name)
		}
	}
}
