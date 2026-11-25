package preflight

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
)

// queryStandIn is a Prometheus, and a Loki, that answer as each refuses an
// expression, counting what each is asked.
type queryStandIn struct {
	mu    sync.Mutex
	asked map[string]int
}

func (q *queryStandIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	expr := r.PostFormValue("query")
	if r.Method == http.MethodGet {
		expr = r.URL.Query().Get("query")
	}
	q.mu.Lock()
	q.asked[r.URL.Path+" "+expr]++
	q.mu.Unlock()
	fail := func(code int, kind, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		fmt.Fprintf(w, `{"status":"error","errorType":%q,"error":%q}`, kind, msg)
	}
	switch {
	case strings.HasPrefix(expr, "sum(rate(broken"):
		fail(http.StatusBadRequest, "bad_data", `1:20: parse error: unclosed left parenthesis`)
	case strings.HasPrefix(expr, "mystery("):
		fail(http.StatusBadRequest, "bad_data", `1:1: parse error: unknown function with name "mystery"`)
	case strings.HasPrefix(expr, "everything"):
		fail(http.StatusUnprocessableEntity, "execution", "query processing would load too many samples into memory in query execution")
	case strings.HasPrefix(expr, "{app=~"):
		fail(http.StatusBadRequest, "", `parse error at line 1, col 7: syntax error: unexpected IDENTIFIER`)
	}
	if w.Header().Get("Content-Type") != "" {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
}

func queryRules() []rules.Rule {
	mk := func(name, expr string) rules.Rule { return rules.Rule{Name: name, Expr: expr} }
	logs := mk("LogErrors", `{app=~"shop"`)
	logs.Logs = "loki"
	return []rules.Rule{
		mk("Up", "up == 0"),
		mk("UpAgain", "up == 0"), // the same expression: asked once
		mk("Broken", "sum(rate(broken[5m]) > 1"),
		mk("Unknown", "mystery(up) > 1"),
		mk("Wide", "everything > 1"),
		logs,
	}
}

// With --queries, each replayable rule's expression is asked once, an
// instant query at now, of where it is replayed, and each rule whose query
// fails is said with why, by where it was asked; the rest of a preflight is
// as it was, and it is off by default.
func TestQueries(t *testing.T) {
	q := &queryStandIn{asked: map[string]int{}}
	hist := httptest.NewServer(q)
	defer hist.Close()
	loki := httptest.NewServer(q)
	defer loki.Close()
	c := config()
	c.History = source.Prometheus{URL: hist.URL}
	c.Serve.Loki, c.Loki = loki.URL, source.Loki{URL: loki.URL}
	rd := &read{all: queryRules()}

	// Off by default: nothing is asked.
	p := guarded(t, c)
	p.queries(context.Background(), rd)
	if len(p.r.Items) != 0 || len(q.asked) != 0 {
		t.Fatalf("--queries off: %+v, asked %v", p.r.Items, q.asked)
	}

	c.Queries = true
	p = guarded(t, c)
	p.queries(context.Background(), rd)
	h, l := itemOf(p, "queries.history"), itemOf(p, "queries.loki")
	if h == nil || h.State != Problem || !strings.Contains(h.What, "3 rule queries of 4 expressions asked of [--prometheus] failed") {
		t.Fatalf("history: %+v", h)
	}
	got := map[string]string{}
	for _, f := range h.Failed {
		got[f.Rule] = f.Reason
	}
	want := map[string]string{"Broken": reasonParse, "Unknown": reasonFunction, "Wide": reasonSamples}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("failed %v, want %v", got, want)
	}
	if l == nil || l.State != Problem || len(l.Failed) != 1 || l.Failed[0].Rule != "LogErrors" || l.Failed[0].Reason != reasonParse {
		t.Errorf("loki: %+v", l)
	}
	// Each expression once: the two rules of one are one request.
	if n := q.asked["/api/v1/query_range up == 0"]; n != 1 {
		t.Errorf("up == 0 asked %d times", n)
	}
	if a := p.al.asked(); a.Refused != 0 || a.By["prometheus"] != 4 || a.By["loki"] != 1 {
		t.Errorf("asked %+v", a)
	}

	// The report says them, in its text and its JSON.
	var text, js bytes.Buffer
	if err := p.r.Text(&text); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"queries.history", "parse error: Broken", "unknown function: Unknown", "too many samples: Wide", "queries.loki", "parse error: LogErrors"} {
		if !strings.Contains(text.String(), w) {
			t.Errorf("no %q in the text:\n%s", w, text.String())
		}
	}
	if err := p.r.JSON(&js); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Items []struct {
			ID     string `json:"id"`
			Failed []struct{ Rule, Reason, Why string }
		}
	}
	if err := json.Unmarshal(js.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range doc.Items {
		if it.ID == "queries.history" && len(it.Failed) == 3 && it.Failed[0].Rule == "Broken" {
			found = true
		}
	}
	if !found {
		t.Errorf("the JSON has no failed rules:\n%s", js.String())
	}

	// A query that is all answered: ok. With no Loki, its rules say so.
	c.Serve.Loki = ""
	p = guarded(t, c)
	p.queries(context.Background(), &read{all: queryRules()[:2]})
	if it := itemOf(p, "queries.history"); it == nil || it.State != OK || !strings.Contains(it.What, "1 expression asked") {
		t.Errorf("all answered: %+v", it)
	}
	p = guarded(t, c)
	p.queries(context.Background(), &read{all: queryRules()[5:]})
	if it := itemOf(p, "queries.loki"); it == nil || it.State != Unknown || !strings.Contains(it.What, "no --loki") {
		t.Errorf("no Loki: %+v", it)
	}

	// --offline asks nothing.
	before := len(q.asked)
	c.Offline = true
	p = guarded(t, c)
	p.queries(context.Background(), rd)
	if it := itemOf(p, "queries.history"); it == nil || it.State != Unknown || len(q.asked) != before {
		t.Errorf("offline: %+v", it)
	}
}

// A query that is asked once, the allowlist holds to exactly what was
// meant: an expression it wasn't told of is refused before it is sent.
func TestQueriesAllowlist(t *testing.T) {
	q := &queryStandIn{asked: map[string]int{}}
	srv := httptest.NewServer(q)
	defer srv.Close()
	c := config()
	c.History, c.Queries = source.Prometheus{URL: srv.URL}, true
	p := guarded(t, c)
	h, err := p.historyClient(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.QueryRange(context.Background(), "up == 0", p.now, p.now, c.Serve.Step); !errors.Is(err, ErrOffList) {
		t.Errorf("an expression not allowed: %v", err)
	}
	if len(q.asked) != 0 {
		t.Errorf("sent: %v", q.asked)
	}
}

// What a failed query says is a kind: what a server says of an expression
// it reads, a bound of the client's, a time, a token not let in.
func TestQueryReason(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{&prom.APIError{Status: 400, Msg: `bad_data: 1:3: parse error: unexpected "}"`}, reasonParse},
		{&prom.APIError{Status: 400, Msg: `parse error: unknown function with name "x"`}, reasonFunction},
		{&prom.APIError{Status: 422, Msg: "query processing would load too many samples into memory"}, reasonSamples},
		{io.ErrUnexpectedEOF, reasonOther},
		{fmt.Errorf("Post: unexpected EOF"), reasonOther},
		{&prom.APIError{Status: 400, Msg: `1:12: parse error: unexpected identifier "timeout"`}, reasonParse},
		{&prom.APIError{Status: 400, Msg: `parse error: unknown function with name "timeout"`}, reasonFunction},
		{&prom.APIError{Status: 503, Msg: "query timed out in expression evaluation"}, reasonTimeout},
		{context.DeadlineExceeded, reasonTimeout},
		{&prom.APIError{Status: 403, Msg: "forbidden"}, reasonLetIn},
		{errors.New("connection reset"), reasonOther},
	} {
		if got := queryReason(c.err); got != c.want {
			t.Errorf("%v: %q, want %q", c.err, got, c.want)
		}
	}
}

const mixedDoc = `apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    interval: 1m
    rules:
      - uid: mix
        title: Mixed
        condition: B
        data:
          - {refId: A, datasourceUid: prom, model: {expr: x, instant: true}}
          - {refId: L, datasourceUid: LOKIUID, model: {expr: 'sum(count_over_time({app="shop"} |= "error" [5m]))', instant: true}}
          - {refId: E, datasourceUid: __expr__, model: {type: math, expression: $L * 2}}
          - refId: B
            datasourceUid: __expr__
            model: {type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [0]}}]}
`

// A Grafana rule's Loki query beside a Prometheus condition is asked of its
// Loki, as the replay routes it: through Grafana's data source proxy, or
// --loki without --grafana; never of the history, which would refuse it as
// PromQL. A uid Grafana doesn't make is not asked, and its path not allowed.
func TestQueriesLokiBesideCondition(t *testing.T) {
	for _, c := range []struct {
		uid     string
		grafana bool
		path    string // where the LogQL is asked, "" for nowhere
	}{
		{"loki", true, "/api/datasources/proxy/uid/loki/loki/api/v1/query_range"},
		{"loki", false, "/loki/api/v1/query_range"},
		{"..", true, ""},
	} {
		q := &queryStandIn{asked: map[string]int{}}
		srv := httptest.NewServer(q)
		doc := strings.Replace(mixedDoc, "LOKIUID", c.uid, 1)
		rs, err := rules.Parse([]byte(doc))
		if err != nil || len(rs) != 1 {
			t.Fatalf("%v %+v", err, rs)
		}
		rules.Replaying{Kinds: map[string]string{c.uid: rules.KindLoki, "prom": "prometheus"}}.Apply(rs)
		cfg := config()
		cfg.Queries, cfg.History = true, source.Prometheus{URL: srv.URL}
		if c.grafana {
			cfg.Serve.Grafana = srv.URL
		} else {
			cfg.Serve.Loki, cfg.Loki = srv.URL, source.Loki{URL: srv.URL}
		}
		p := guarded(t, cfg)
		p.queries(context.Background(), &read{all: rs})
		srv.Close()
		for k := range q.asked {
			if strings.Contains(k, "count_over_time") != (c.path != "" && strings.HasPrefix(k, c.path)) {
				t.Errorf("%+v: asked %q", c, k)
			}
		}
		if it := itemOf(p, "queries.history"); it == nil || it.State != OK {
			t.Errorf("%+v: history %+v", c, it)
		}
		if a := p.al.asked(); a.Refused != 0 {
			t.Errorf("%+v: %+v", c, a)
		}
		if c.path != "" && len(q.asked) != 2 || c.path == "" && len(q.asked) != 1 {
			t.Errorf("%+v: asked %v", c, q.asked)
		}
	}
}

// The client's own bound on an answer (--max-samples) is "too many
// samples", by its type and not its words; and it holds Grafana's Loki
// data sources too.
func TestQueriesMaxSamples(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		at := r.FormValue("start")
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[`+
			`{"metric":{"a":"1"},"values":[[`+at+`,"1"]]},{"metric":{"a":"2"},"values":[[`+at+`,"1"]]}]}}`)
	}))
	defer srv.Close()
	c := config()
	c.Queries, c.History = true, source.Prometheus{URL: srv.URL, MaxSamples: 1}
	c.Serve.Grafana, c.MaxSamples = srv.URL, 1
	for _, kind := range []string{"history", "grafana"} {
		var r rules.Rule
		if kind == "history" {
			r = rules.Rule{Name: "R", Expr: "up"}
		} else {
			r = rules.Rule{Name: "R", Expr: "x", Logs: "loki", Graph: &rules.Graph{Queries: map[string]string{"A": `{a="b"}`}, Cond: "A", Asks: map[string]rules.Ask{"A": {Logs: true, Expr: `{a="b"}`}}}}
		}
		p := guarded(t, c)
		p.queries(context.Background(), &read{all: []rules.Rule{r}})
		it := itemOf(p, "queries."+kind)
		if it == nil || len(it.Failed) != 1 || it.Failed[0].Reason != reasonSamples {
			t.Errorf("%s: %+v", kind, it)
		}
	}
}
