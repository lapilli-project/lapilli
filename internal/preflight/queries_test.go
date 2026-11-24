package preflight

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		{errors.New("the answers to the range hold more than 50000000 samples in all (--max-samples): read it"), reasonSamples},
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
