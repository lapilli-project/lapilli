package preflight

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/egress"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/serve"
	"lapilli.dev/internal/source"
)

// forbidden are the calls that open a ledger for serve, migrating it, or
// make a serve: a preflight reads the ledger through ledger.Probe alone.
var forbidden = map[string][]string{
	"lapilli.dev/internal/ledger": {"Open", "OpenPostgres", "OpenPostgresAlone"},
	"lapilli.dev/internal/serve":  {"New"},
}

// The preflight's source calls nothing that opens the ledger for serve or
// makes a serve: this package's, and the command's.
func TestNoLedgerOpen(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	cmd, _ := filepath.Glob("../../cmd/lapilli/preflight*.go")
	files = append(files, cmd...)
	if len(cmd) == 0 {
		t.Fatal("no cmd/lapilli/preflight.go")
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		fs := token.NewFileSet()
		file, err := parser.ParseFile(fs, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		names := map[string]string{} // a package's name here to its path
		for _, im := range file.Imports {
			path, _ := strconv.Unquote(im.Path.Value)
			name := path[strings.LastIndex(path, "/")+1:]
			if im.Name != nil {
				name = im.Name.Name
			}
			names[name] = path
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			x, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			for _, fn := range forbidden[names[x.Name]] {
				if sel.Sel.Name == fn {
					t.Errorf("%s: %s.%s: a preflight never opens the ledger for serve, or makes one", fs.Position(sel.Pos()), x.Name, fn)
				}
			}
			return true
		})
	}
	if checked < 5 {
		t.Fatalf("%d files read", checked)
	}
}

// The test above finds such a call, however its package is named.
func TestNoLedgerOpenFinds(t *testing.T) {
	src := `package x
import l "lapilli.dev/internal/ledger"
func f() { l.Open(nil, "", nil) }`
	fs := token.NewFileSet()
	file, err := parser.ParseFile(fs, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "l" && sel.Sel.Name == "Open" {
				found = true
			}
		}
		return true
	})
	if !found {
		t.Error("not found")
	}
}

func req(t *testing.T, method, raw, form string) *http.Request {
	t.Helper()
	var r *http.Request
	var err error
	if form != "" {
		r, err = http.NewRequest(method, raw, strings.NewReader(form))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r, err = http.NewRequest(method, raw, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The allowlist lets through the reads a preflight makes, by method, path
// and exact query, and refuses everything else before it is sent, counted.
func TestAllowlist(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	c := Config{Serve: serve.Config{Grafana: "https://grafana.example.com/sub", Loki: "http://loki:3100", Ruler: "http://mimir:8080/prometheus/config/v1/rules"},
		History: source.Prometheus{URL: "http://prometheus:9090"}}
	p := &run{c: c, al: newAllowlist(func() time.Time { return now }), now: now}
	p.allowAll()
	at := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	q := func(expr string, from, to time.Time) string {
		return url.Values{"query": {expr}, "start": {at(from)}, "end": {at(to)}, "step": {"60"}}.Encode()
	}
	labels := "?" + url.Values{"start": {fmt.Sprint(now.Add(-5 * time.Minute).UnixNano())}, "end": {fmt.Sprint(now.UnixNano())}}.Encode()
	for _, x := range []struct {
		method, url, form string
		ok                bool
	}{
		{"POST", "http://prometheus:9090/api/v1/query_range", q("vector(1)", now, now), true},
		{"POST", "http://prometheus:9090/api/v1/query_range", q(alertsCount(serve.AlertsSeries), now, now), true},
		{"POST", "http://prometheus:9090/api/v1/query_range", q("up", now, now), false},                           // a rule's expression
		{"POST", "http://prometheus:9090/api/v1/query_range", q("vector(1)", now.Add(-24*time.Hour), now), false}, // a range
		{"POST", "http://prometheus:9090/api/v1/query_range", q("vector(1)", now, now) + "&query=up", false},      // a second query
		{"GET", "http://prometheus:9090/api/v1/rules?type=alert", "", true},
		{"GET", "http://prometheus:9090/api/v1/rules?type=alert&x=1", "", false},
		{"POST", "http://prometheus:9090/api/v1/admin/tsdb/delete_series", "match[]=up", false},
		{"GET", "http://prometheus:9090/api/v1/status/config", "", true},
		{"GET", "http://loki:3100/loki/api/v1/labels" + labels, "", true},
		{"GET", "http://loki:3100/loki/api/v1/query_range?query=x", "", false},
		{"GET", "https://grafana.example.com/sub/api/v1/provisioning/alert-rules/export?format=yaml", "", true},
		{"POST", "https://grafana.example.com/sub/api/v1/provisioning/alert-rules", "", false},
		{"PUT", "https://grafana.example.com/sub/api/v1/provisioning/policies", "", false},
		{"GET", "https://grafana.example.com/sub/api/v1/provisioning/policies", "", true},
		{"GET", "https://grafana.example.com/api/v1/provisioning/policies", "", false}, // off its path
		{"GET", "http://mimir:8080/prometheus/config/v1/rules", "", true},
		{"POST", "http://mimir:8080/prometheus/config/v1/rules/ns", "", false},
		{"DELETE", "http://mimir:8080/prometheus/config/v1/rules/ns", "", false},
		{"GET", "http://elsewhere:9090/api/v1/status/config", "", false},
	} {
		err := p.al.check(req(t, x.method, x.url, x.form))
		if (err == nil) != x.ok {
			t.Errorf("%s %s %s: %v, want allowed %v", x.method, x.url, x.form, err, x.ok)
		}
	}
	a := p.al.asked()
	if a.Requests != 8 || a.Refused != 12 || a.By["prometheus"] != 4 || a.By["grafana"] != 2 {
		t.Errorf("%+v", a)
	}
}

// A cloud's credential endpoints are let through by host and action alone:
// STS's AssumeRole, not its other actions.
func TestAllowCredentials(t *testing.T) {
	now := time.Now()
	c := Config{Serve: serve.Config{Changes: true, ChangesAWSRole: "arn:aws:iam::123456789012:role/x",
		Alarms: []serve.AlarmsAt{{Source: "cw", URL: "https://monitoring.eu-west-1.amazonaws.com"}}}}
	p := &run{c: c, al: newAllowlist(func() time.Time { return now }), now: now}
	p.allowAll()
	for _, x := range []struct {
		method, url, form string
		ok                bool
	}{
		{"POST", "https://sts.eu-west-1.amazonaws.com/", "Action=AssumeRole&Version=2011-06-15&RoleArn=x", true},
		{"POST", "https://sts.amazonaws.com/", "Action=AssumeRoleWithWebIdentity&Version=2011-06-15", true},
		{"POST", "https://sts.eu-west-1.amazonaws.com/", "Action=CreateRole", false},
		{"POST", "https://sts.eu-west-1.amazonaws.com.evil.example/", "Action=AssumeRole", false},
		{"PUT", "http://169.254.169.254/latest/api/token", "", true},
		{"GET", "http://169.254.169.254/latest/meta-data/iam/security-credentials/role", "", true},
		{"GET", "http://169.254.169.254/latest/user-data", "", false},
	} {
		if err := p.al.check(req(t, x.method, x.url, x.form)); (err == nil) != x.ok {
			t.Errorf("%s %s %s: %v, want allowed %v", x.method, x.url, x.form, err, x.ok)
		}
	}
	// CloudWatch's alarms: the read, not a write.
	r := req(t, "POST", "https://monitoring.eu-west-1.amazonaws.com/", "")
	r.Header.Set("X-Amz-Target", "GraniteServiceVersion20100801.DescribeAlarms")
	if err := p.al.check(r); err != nil {
		t.Error(err)
	}
	r.Header.Set("X-Amz-Target", "GraniteServiceVersion20100801.PutMetricAlarm")
	if err := p.al.check(r); err == nil {
		t.Error("PutMetricAlarm let through")
	}
}

// While a preflight runs, every client of lapilli sends through its
// allowlist: one that would send a request off it sends nothing, and the
// report counts it.
func TestGuardSet(t *testing.T) {
	sent := 0
	rt := egress.Wrap(roundTrip(func(*http.Request) (*http.Response, error) { sent++; return nil, fmt.Errorf("sent") }))
	al := newAllowlist(time.Now)
	release := egress.Guard(al.check)
	_, err := rt.RoundTrip(req(t, "POST", "http://prometheus:9090/api/v1/admin/tsdb/delete_series", "match[]=up"))
	release()
	if err == nil || sent != 0 || al.asked().Refused != 1 {
		t.Errorf("err %v, sent %d, %+v", err, sent, al.asked())
	}
	if _, err := rt.RoundTrip(req(t, "GET", "http://prometheus:9090/", "")); err == nil || sent != 1 {
		t.Errorf("without the guard: err %v, sent %d", err, sent)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// rulesFile writes n rules for 5m, and some for a day, and reads them as
// serve would.
func rulesOf(t *testing.T, n, daily int) []rules.Rule {
	t.Helper()
	var b strings.Builder
	b.WriteString("groups:\n  - name: shop\n    interval: 1m\n    rules:\n")
	for i := range n {
		fmt.Fprintf(&b, "      - alert: Errors%d\n        expr: rate(errors_total{job=\"svc%d\"}[5m]) > 1\n        for: 5m\n", i, i)
	}
	for i := range daily {
		fmt.Fprintf(&b, "      - alert: Slow%d\n        expr: avg_over_time(latency_seconds{job=\"svc%d\"}[1h]) > 2\n        for: 1d\n", i, i)
	}
	rs, err := rules.Parse([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func config() Config {
	return Config{Serve: serve.Config{Every: time.Hour, Settle: time.Hour, Backfill: 28 * 24 * time.Hour, Step: time.Minute, Timeout: 50 * time.Minute,
		Parallel: 8, Watch: time.Minute, Listen: "127.0.0.1:8080", Loc: time.UTC}, History: source.Prometheus{URL: "http://prometheus:9090"}}
}

// The design's deployment: 200 rules for 5m, the defaults, --max-rate 0.5.
// An hourly run asks one request a rule, two when it crosses a piece's
// edge; the backfill two a rule a day; the catch-up hours hold some 9,000
// of its requests a night, two nights at least.
func TestEstimateRequests(t *testing.T) {
	c := config()
	c.MaxRate = 0.5
	c.Serve.CatchUpRate = 0.5
	w, _ := serve.ParseWindow("01:00-06:00")
	c.Serve.CatchUpHours = &w
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	p := &run{c: c, al: newAllowlist(time.Now), r: &Report{}, now: now}
	p.estimate(&read{all: rulesOf(t, 200, 0)})
	h, b, n := p.r.Estimates.Hourly, p.r.Estimates.Backfill, p.r.Estimates.Nights
	// Each run replays from 2h15m back (settle, every, a 15m margin) and
	// 5m more: some 10% of runs cross midnight.
	if h == nil || h.Requests < 200 || h.Requests > 240 || h.Most != 400 || h.Runs != 24 {
		t.Fatalf("hourly %+v", h)
	}
	if b == nil || b.Pieces != 28 || b.Requests < 200*2*27 || b.Requests > 200*2*29 || b.Rate != 0.5 {
		t.Fatalf("backfill %+v", b)
	}
	if n == nil || n.PerNight != 9000 || n.Nights != 2 {
		t.Fatalf("nights %+v", n)
	}
	// Three rules for a day have a margin of seven days: each run reads
	// eight or nine days of each again.
	p = &run{c: c, al: newAllowlist(time.Now), r: &Report{}, now: now}
	p.estimate(&read{all: rulesOf(t, 200, 3)})
	if more := p.r.Estimates.Hourly.Requests - h.Requests; more < 24 || more > 30 {
		t.Errorf("three rules for a day add %d requests a run", more)
	}
}

// An hourly run whose requests at --max-rate take longer than --every
// falls behind, and is a problem; a backfill day past --timeout is said,
// with its pieces, and isn't.
func TestEstimateTooSlow(t *testing.T) {
	c := config()
	c.MaxRate = 0.05
	p := &run{c: c, al: newAllowlist(time.Now), r: &Report{}, now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	p.estimate(&read{all: rulesOf(t, 200, 0)})
	states := map[string]State{}
	for _, it := range p.r.Items {
		states[it.ID] = it.State
	}
	if states["estimate.hourly"] != Problem || states["estimate.backfill"] != OK || p.r.Estimates.Backfill.PieceHours == 0 {
		t.Errorf("%v %+v", states, p.r.Estimates.Backfill)
	}
}

// Memory from what fired: a rule's samples at its group's interval, read
// at --step, the --parallel largest at once; a rule past --max-samples is
// named. With nothing read, the caps' bound. Under a cgroup's limit below
// it, a problem.
func TestEstimateMemory(t *testing.T) {
	c := config()
	c.Serve.Parallel = 2
	c.MaxSamples = 1000
	c.CgroupMemory = filepath.Join(t.TempDir(), "memory.max")
	os.WriteFile(c.CgroupMemory, []byte("max\n"), 0o600)
	rs := rulesOf(t, 3, 0)
	p := &run{c: c, r: &Report{}, alerts: map[string]float64{"Errors0": 100, "Errors1": 300, "Errors2": 5000}}
	p.memory(rs)
	m := p.r.Estimates.Memory
	if m.From != "alerts" || m.PeakBytes != (1000+300)*64+cacheBytes || m.LimitBytes != 2*m.RequestBytes {
		t.Fatalf("%+v", m)
	}
	if it := p.r.Items[0]; !strings.Contains(it.Why, "Errors2") || it.State != OK {
		t.Errorf("%+v", it)
	}
	os.WriteFile(c.CgroupMemory, []byte("1000000\n"), 0o600)
	p = &run{c: c, r: &Report{}}
	p.memory(rs)
	if m := p.r.Estimates.Memory; m.From != "caps" || m.PeakBytes != 2*1000*64+cacheBytes || m.CgroupBytes != 1000000 || p.r.Items[0].State != Problem {
		t.Errorf("%+v %+v", m, p.r.Items[0])
	}
}

// The report's JSON has the fields docs/numbers.md promises, and the exit
// status follows the states.
func TestReportJSON(t *testing.T) {
	r := &Report{Version: 1, Lapilli: "v0.0.0", Items: []Item{{ID: "a", State: OK, What: "x"}, {ID: "b", State: Unknown, What: "y"}}}
	var b bytes.Buffer
	if err := r.JSON(&b); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"version", "lapilli", "items", "estimates", "left_out", "asked"} {
		if _, ok := doc[f]; !ok {
			t.Errorf("no %s", f)
		}
	}
	if r.Exit(false) != 0 || r.Exit(true) != 1 {
		t.Errorf("exits %d %d", r.Exit(false), r.Exit(true))
	}
	r.Items = append(r.Items, Item{ID: "c", State: Problem})
	if r.Exit(false) != 1 {
		t.Error("a problem exits 0")
	}
}

// What a server answered is said as text, 200 bytes at most, whatever it
// held.
func TestSaid(t *testing.T) {
	s := said(fmt.Errorf("HTTP 500: \x1b[31m%s‮", strings.Repeat("é", 300)))
	if len(s) > 200 || strings.ContainsAny(s, "\x1b‮") {
		t.Errorf("%q", s)
	}
}

// With --offline, nothing is asked: the items that would ask are unknown,
// and no request goes out, even one the flags would allow.
func TestOffline(t *testing.T) {
	c := config()
	c.Offline = true
	c.Serve.Grafana = "http://grafana:3000"
	rep := Run(context.Background(), c)
	if rep.Asked.Requests != 0 || rep.Asked.Refused != 0 {
		t.Errorf("%+v", rep.Asked)
	}
	for _, it := range rep.Items {
		if strings.HasPrefix(it.ID, "history.") && it.State != Unknown {
			t.Errorf("%+v", it)
		}
	}
}

// IDs are every item's id a preflight gives.
var IDs = []string{"rules.read", "rules.replayable", "rules.history", "expressions", "history.reach", "history.loki", "history.grafana",
	"history.alerts", "history.intervals", "grafana.notifications", "grafana.write", "changes.cloud", "changes.ruler", "inputs.files",
	"secrets.files", "ledger.sqlite", "ledger.postgres", "signin", "access", "estimate.hourly", "estimate.backfill", "estimate.nights",
	"estimate.beside", "estimate.memory"}

// The report's fields and its items' ids keep their names
// (docs/numbers.md, "lapilli preflight's report"): each the code gives is
// listed there, and each listed is in the JSON.
func TestPreflightStable(t *testing.T) {
	full := &Report{Version: 1, Lapilli: "v0", Items: []Item{{ID: "access", State: OK, What: "x", Why: "y", Fix: "z"}}, LeftOut: []string{"x"},
		Asked: Asked{Requests: 1, By: map[string]int{"prometheus": 1}, Refused: 0},
		Estimates: Estimates{Hourly: &Hourly{Runs: 1, Seconds: 1, Share: 1}, Backfill: &Backfill{Rate: 1, Seconds: 1, PieceHours: 1},
			Nights: &Nights{Hours: "x"}, Beside: &Beside{}, Memory: &Memory{From: "caps", CgroupBytes: 1}}}
	var b bytes.Buffer
	if err := full.JSON(&b); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.Unmarshal(b.Bytes(), &doc)
	at := func(path string) map[string]any {
		v := any(doc)
		for _, part := range strings.Split(path, ".") {
			if part == "" {
				continue
			}
			name, list := strings.CutSuffix(part, "[]")
			v = v.(map[string]any)[name]
			if list {
				v = v.([]any)[0]
			}
		}
		return v.(map[string]any)
	}
	numbers, err := os.ReadFile("../../docs/numbers.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, _ := strings.Cut(string(numbers), "### lapilli preflight's report")
	section = strings.Join(strings.Fields(section), " ")
	fields := map[string][]string{
		"":                   {"version", "lapilli", "items", "estimates", "left_out", "asked"},
		"items[]":            {"id", "state", "what", "why", "fix"},
		"asked":              {"requests", "by", "refused"},
		"estimates":          {"hourly", "backfill", "nights", "beside", "memory"},
		"estimates.hourly":   {"runs", "requests", "most", "day", "by", "guard", "impact", "seconds", "share"},
		"estimates.backfill": {"pieces", "requests", "most", "by", "guard", "impact", "rate", "seconds", "piece_hours"},
		"estimates.nights":   {"hours", "nights", "per_night"},
		"estimates.beside":   {"day", "by"},
		"estimates.memory":   {"from", "peak_bytes", "request_bytes", "limit_bytes", "cgroup_bytes"},
	}
	for where, fs := range fields {
		obj := at(where)
		if len(obj) != len(fs) {
			t.Errorf("%s has %d fields, %d listed: a new one goes in docs/numbers.md too", where, len(obj), len(fs))
		}
		for _, f := range fs {
			if _, ok := obj[f]; !ok {
				t.Errorf("%s: no %q in the JSON", where, f)
			}
			if !strings.Contains(section, "`"+f+"`") {
				t.Errorf("docs/numbers.md doesn't name %s's %s", where, f)
			}
		}
	}
	// Every id the source gives is listed, here and there.
	files, _ := filepath.Glob("*.go")
	given := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(f)
		for _, part := range strings.Split(string(src), `ID: "`)[1:] {
			given[part[:strings.Index(part, `"`)]] = true
		}
		for _, part := range strings.Split(string(src), `p.offline("`)[1:] {
			given[part[:strings.Index(part, `"`)]] = true
		}
		for _, part := range strings.Split(string(src), `"estimate.`)[1:] {
			given["estimate."+part[:strings.Index(part, `"`)]] = true
		}
	}
	for id := range given {
		if !slices.Contains(IDs, id) {
			t.Errorf("the source gives %q, which isn't listed", id)
		}
	}
	for _, id := range IDs {
		if !given[id] {
			t.Errorf("%q is listed, and the source gives none", id)
		}
		if !strings.Contains(section, "`"+id+"`") {
			t.Errorf("docs/numbers.md doesn't name the id %s", id)
		}
	}
}

// itemOf is the item of an id a run added, nil for none.
func itemOf(p *run, id string) *Item {
	for i := range p.r.Items {
		if p.r.Items[i].ID == id {
			return &p.r.Items[i]
		}
	}
	return nil
}

func newRun(c Config) *run {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	return &run{c: c, al: newAllowlist(func() time.Time { return now }), r: &Report{}, now: now}
}

// An expression that doesn't read is a problem; one that does is read, and
// not sent.
func TestExpressions(t *testing.T) {
	c := config()
	c.Serve.Guard = `rate(errors_total{job="a}[5m]) > 0`
	p := newRun(c)
	p.expressions()
	if it := itemOf(p, "expressions"); it == nil || it.State != Problem || !strings.Contains(it.Why, "--guard") {
		t.Fatalf("%+v", it)
	}
	c.Serve.Guard, c.Serve.Impact = `rate(errors_total{job="a"}[5m]) > 0`, `sum(rate(x[5m])) > 1`
	p = newRun(c)
	p.expressions()
	if it := itemOf(p, "expressions"); it == nil || it.State != OK {
		t.Fatalf("%+v", it)
	}
}

// Two token files holding one token, a webhook file holding no https URL,
// a token too short: each a problem, said by path and length, never by
// what the file holds.
func TestSecrets(t *testing.T) {
	dir := t.TempDir()
	file := func(name, body string) string {
		f := filepath.Join(dir, name)
		os.WriteFile(f, []byte(body), 0o600)
		return f
	}
	c := config()
	c.Serve.TasksToken = file("tasks", "shared-token-0123456789")
	c.Serve.ChangesToken = file("changes", "shared-token-0123456789\n")
	c.Serve.Slack = file("slack", "http://hooks.example/x")
	p := newRun(c)
	p.secrets()
	it := itemOf(p, "secrets.files")
	if it == nil || it.State != Problem || !strings.Contains(it.Why, "holds the same token as --tasks-token-file") ||
		!strings.Contains(it.Why, "doesn't hold an https URL") || strings.Contains(it.What+it.Why, "shared-token") || strings.Contains(it.What+it.Why, "hooks.example") {
		t.Fatalf("%+v", it)
	}
	c.Serve.ChangesToken = file("changes", "another-token-0123456789")
	c.Serve.Slack = file("slack", "https://hooks.example/x")
	p = newRun(c)
	p.secrets()
	if it := itemOf(p, "secrets.files"); it == nil || it.State != OK || !strings.Contains(it.What, "23 characters") {
		t.Fatalf("%+v", it)
	}
}

// A file serve would refuse is a problem, with why: an incidents file with
// no incidents key, which would take every incident away.
func TestInputFiles(t *testing.T) {
	c := config()
	c.Serve.Incidents = filepath.Join(t.TempDir(), "incidents.yaml")
	os.WriteFile(c.Serve.Incidents, []byte("# nothing yet\n"), 0o600)
	p := newRun(c)
	p.files(context.Background())
	if it := itemOf(p, "inputs.files"); it == nil || it.State != Problem || !strings.Contains(it.Why, "no incidents key") {
		t.Fatalf("%+v", it)
	}
}

// What a ledger's schema means as serve starts: a new one, this release's,
// one serve upgrades (keep a copy), one a newer lapilli wrote (refused).
func TestSchemaSays(t *testing.T) {
	for _, x := range []struct {
		there          bool
		version, knows int
		state          State
		says           string
	}{
		{false, 0, 19, OK, "a new ledger"},
		{true, 19, 19, OK, "this release's"},
		{true, 18, 19, OK, "upgrades the ledger from 018 to 019"},
		{true, 20, 19, Problem, "newer"},
	} {
		state, what, _ := schemaSays(ledgerProbed(x.there, x.version, x.knows))
		if state != x.state || !strings.Contains(what, x.says) {
			t.Errorf("%+v: %s %q", x, state, what)
		}
	}
}

// An identity serve writes as that can't be taken is a problem; written
// to, nothing.
func TestChangesIdentity(t *testing.T) {
	c := config()
	c.Serve.Changes = true
	c.Serve.ChangesOCIConfig = filepath.Join(t.TempDir(), "oci-config")
	os.WriteFile(c.Serve.ChangesOCIConfig, []byte("[DEFAULT]\nuser=ocid1.user.oc1..example\n"), 0o600)
	c.Serve.Alarms = []serve.AlarmsAt{{Source: "oci", URL: "https://telemetry.eu-frankfurt-1.oraclecloud.com/20180401/alarms?compartmentId=ocid1.compartment.oc1..example"}}
	p := newRun(c)
	p.changes(context.Background())
	if it := itemOf(p, "changes.cloud"); it == nil || it.State != Problem || !strings.Contains(it.Why, "--changes-oci-config") {
		t.Fatalf("%+v", it)
	}
	if a := p.al.asked(); a.Requests+a.Refused != 0 {
		t.Errorf("%+v", a)
	}
}

func ledgerProbed(there bool, version, knows int) ledger.Probed {
	return ledger.Probed{There: there, Version: version, Knows: knows}
}
