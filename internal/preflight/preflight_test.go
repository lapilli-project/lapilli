package preflight

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

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
	c := Config{Serve: serve.Config{Grafana: "https://grafana.example.com/sub", Loki: "http://loki:3100", Ruler: "http://mimir:8080/prometheus/config/v1/rules",
		Loaded: true, CheckContent: true},
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
	p.estimate(context.Background(), &read{all: rulesOf(t, 200, 0)})
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
	p.estimate(context.Background(), &read{all: rulesOf(t, 200, 3)})
	if more := p.r.Estimates.Hourly.Requests - h.Requests; more < 24 || more > 30 {
		t.Errorf("three rules for a day add %d requests a run", more)
	}
}

// timeUp is a context whose time is up once Err has been asked n times:
// the preflight's time running out where a test says, by count, not by a
// clock.
type timeUp struct {
	context.Context
	n, asked int
}

func (c *timeUp) Err() error {
	c.asked++
	if c.asked > c.n {
		return context.DeadlineExceeded
	}
	return nil
}

// The estimates are worked out within the preflight's time: once it is up,
// what isn't counted yet is unknown, and no more is counted, whatever a
// pull request's flags and rules ask (ten years of backfill, a run every
// minute). Each run and each day of backfill looks at the time.
func TestEstimateWithinTime(t *testing.T) {
	c := config()
	c.MaxRate, c.Serve.CatchUpRate = 0.5, 0.5
	w, _ := serve.ParseWindow("01:00-06:00")
	c.Serve.CatchUpHours = &w
	rs := rulesOf(t, 200, 0)
	states := func(p *run) map[string]State {
		out := map[string]State{}
		for _, it := range p.r.Items {
			out[it.ID] = it.State
			if it.State == Unknown && strings.HasPrefix(it.ID, "estimate.") && it.ID != "estimate.memory" && !strings.Contains(it.What, "--preflight-timeout") {
				t.Errorf("%s: %q", it.ID, it.What)
			}
		}
		return out
	}
	// Up from the start: nothing counted, ten years of runs every minute
	// or not; the hourly runs and the backfill each look once in their
	// first piece and once after it.
	big := c
	big.Serve.Backfill, big.Serve.Every = serve.MaxBackfill, time.Minute
	for _, c := range []Config{c, big} {
		ctx := &timeUp{Context: context.Background()}
		p := newRun(c)
		p.estimate(ctx, &read{all: rs})
		if s := states(p); s["estimate.hourly"] != Unknown || s["estimate.backfill"] != Unknown || s["estimate.nights"] != Unknown ||
			p.r.Estimates.Hourly != nil || p.r.Estimates.Backfill != nil || ctx.asked > 4 {
			t.Errorf("up from the start: %v %+v, Err asked %d times", s, p.r.Estimates, ctx.asked)
		}
	}
	// Up in the backfill: the hourly runs are counted (a day of them, one
	// rule's group each: two looks a run), the backfill stops at once.
	ctx := &timeUp{Context: context.Background(), n: 2*24 + 10}
	p := newRun(c)
	p.estimate(ctx, &read{all: rs})
	if s := states(p); s["estimate.hourly"] != OK || s["estimate.backfill"] != Unknown || s["estimate.nights"] != Unknown || ctx.asked > ctx.n+2 {
		t.Errorf("up in the backfill: %v, Err asked %d times of %d", s, ctx.asked, ctx.n)
	}
	// With time enough, all of it.
	p = newRun(c)
	p.estimate(context.Background(), &read{all: rs})
	if s := states(p); s["estimate.hourly"] != OK || s["estimate.backfill"] != OK || s["estimate.nights"] != OK {
		t.Errorf("with time enough: %v", s)
	}
}

// An hourly run whose requests at --max-rate take longer than --every
// falls behind, and is a problem; a backfill day past --timeout is said,
// with its pieces, and isn't.
func TestEstimateTooSlow(t *testing.T) {
	c := config()
	c.MaxRate = 0.05
	p := &run{c: c, al: newAllowlist(time.Now), r: &Report{}, now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	p.estimate(context.Background(), &read{all: rulesOf(t, 200, 0)})
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
// the estimate, a problem; below the caps' bound, unknown: it is a bound,
// not what a run holds.
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
	if m, it := p.r.Estimates.Memory, p.r.Items[0]; m.From != "caps" || m.PeakBytes != 2*1000*64+cacheBytes || m.CgroupBytes != 1000000 ||
		it.State != Unknown || !strings.Contains(it.Why, "under the caps' bound") || !strings.Contains(it.Why, "2 reads at once") || it.Fix == "" {
		t.Errorf("the caps' bound over the limit: %+v %+v", m, it)
	}
	// The bound is said to be the most a run can take, with no request or
	// limit advised from it; what fired advises both.
	if it := p.r.Items[0]; !strings.Contains(it.What, "no firing read: at most ") || strings.Contains(it.What, "a request of") || p.r.Estimates.Memory.RequestBytes == 0 {
		t.Errorf("the caps' bound's text: %q", it.What)
	}
	// What fired, over the same limit, is a problem still.
	p = &run{c: c, r: &Report{}, alerts: map[string]float64{"Errors0": 100, "Errors1": 300, "Errors2": 5000}}
	p.memory(rs)
	if m, it := p.r.Estimates.Memory, p.r.Items[0]; m.From != "alerts" || m.PeakBytes <= 1000000 || it.State != Problem ||
		!strings.Contains(it.Why, "under the estimate") || !strings.Contains(it.Why, "Errors2") {
		t.Errorf("the estimate over the limit: %+v %+v", m, it)
	}
	if it := p.r.Items[0]; !strings.Contains(it.What, "a request of") {
		t.Errorf("what fired advises a request: %q", it.What)
	}
	// Over the limit, the caps' bound and the estimate are ok.
	os.WriteFile(c.CgroupMemory, []byte("1000000000\n"), 0o600)
	for _, alerts := range []map[string]float64{nil, {"Errors0": 100}} {
		p = &run{c: c, r: &Report{}, alerts: alerts}
		p.memory(rs)
		if it := p.r.Items[0]; it.State != OK {
			t.Errorf("%v under the limit: %+v", alerts, it)
		}
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
	s := cutSaid(said(fmt.Errorf("HTTP 500: \x1b[31m%s‮", strings.Repeat("é", 300))))
	if len(s) > 200 || strings.ContainsAny(s, "\x1b‮") {
		t.Errorf("%q", s)
	}
}

// controls are what a text from outside may hold to act on a terminal: an
// OSC that retitles it, a clear and a colour, BEL, a direction mark, and
// the marks a preflight's own were before they held a random text.
const controls = "\x1b]0;title\x07\x1b[2J\x1b[31mFAKE ok\x1b[0m‮\x00\x01"

// acting says whether s holds a character that acts on a terminal or
// turns text around.
func acting(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) })
}

// A rule file a pull request changes, read with --offline in CI, says
// why a rule can't be replayed in its own words: they are printed as
// text, in the text and in the JSON, never as escape sequences.
func TestRuleReasonShown(t *testing.T) {
	esc := strings.NewReplacer("\x1b", `\e`, "\x07", `\a`, "‮", `‮`, "\x00", `\0`, "\x01", `\x01`).Replace(controls)
	file := `apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    interval: 1m
    rules:
      - uid: u1
        title: Plain
        condition: C
        data:
          - refId: A
            relativeTimeRange: {from: 600, to: 0}
            datasourceUid: prom
            model: {expr: up, refId: A}
          - refId: C
            datasourceUid: __expr__
            model:
              type: classic_conditions
              refId: C
              conditions:
                - {evaluator: {type: gt, params: ["` + esc + `"]}, operator: {type: and}, query: {params: [A]}, reducer: {type: avg}}
        for: 5m
`
	path := filepath.Join(t.TempDir(), "rules.yaml")
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	c := config()
	c.Offline = true
	c.Serve.RulesPath = path
	rep := Run(context.Background(), c)
	var text, js bytes.Buffer
	if err := rep.Text(&text); err != nil {
		t.Fatal(err)
	}
	if err := rep.JSON(&js); err != nil {
		t.Fatal(err)
	}
	if acting(strings.ReplaceAll(text.String(), "\n", "")) {
		t.Errorf("the text acts on a terminal:\n%q", text.String())
	}
	if !strings.Contains(text.String(), `\u{1b}]0;title\u{7}`) {
		t.Errorf("the reason isn't shown as text:\n%s", text.String())
	}
	var doc Report
	if err := json.Unmarshal(js.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, it := range doc.Items {
		if acting(it.What + it.Why + it.Fix) {
			t.Errorf("the JSON's %s acts on a terminal: %q", it.ID, it.Why)
		}
	}
}

// Every text of an item a check fills is shown as it reads in one place,
// redact, so that a field added to Item, or a check that puts a name in
// one as it came, can't print it otherwise. A mark a text holds of its
// own is text, and cuts nothing.
func TestRedactShowsEveryText(t *testing.T) {
	c := config()
	c.Scrubber = func(...[2]string) func(string) string { return func(s string) string { return s } }
	p := newRun(c)
	it := Item{ID: "rules.read", State: OK}
	v := reflect.ValueOf(&it).Elem()
	var filled []string
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if f.Type.Kind() != reflect.String || f.Name == "ID" || f.Name == "State" {
			continue // the id and the state are the code's own
		}
		v.Field(i).SetString(f.Name + controls + strings.Repeat("x", 300) + "\x00\x02 end")
		filled = append(filled, f.Name)
	}
	p.add(it)
	p.r.LeftOut = []string{"left" + controls}
	p.redact()
	got := reflect.ValueOf(p.r.Items[0])
	for _, name := range filled {
		s := got.FieldByName(name).String()
		if acting(s) || !strings.HasSuffix(s, " end") || !strings.Contains(s, `\u{0}\u{1}`) {
			t.Errorf("%s: %q", name, s)
		}
	}
	if s := p.r.LeftOut[0]; acting(s) {
		t.Errorf("left out: %q", s)
	}
	// What a server said is shown and cut as it was.
	p = newRun(c)
	p.add(Item{ID: "history.reach", State: Problem, What: "x", Why: "not let in: " + said(fmt.Errorf("HTTP 403: %s%s", controls, strings.Repeat("y", 300)))})
	p.redact()
	if why := p.r.Items[0].Why; acting(why) || len(why) > 250 || !strings.HasSuffix(why, "…") {
		t.Errorf("what a server said: %q", why)
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
var IDs = []string{"rules.read", "rules.replayable", "rules.sources", "rules.history", "expressions", "history.reach", "history.loki", "history.grafana",
	"history.alerts", "history.intervals", "queries.history", "queries.loki", "queries.grafana", "grafana.notifications", "grafana.alertmanagers", "content.alertmanager", "grafana.write", "changes.cloud", "changes.aws", "changes.azure",
	"changes.google", "changes.oci", "changes.ruler", "changes.loki", "inputs.files",
	"secrets.files", "ledger.sqlite", "ledger.postgres", "signin", "backtest.trust", "backtest.issuers", "access", "estimate.hourly",
	"estimate.backfill", "estimate.nights", "estimate.beside", "estimate.backtests", "estimate.memory"}

// The report's fields and its items' ids keep their names
// (docs/numbers.md, "lapilli preflight's report"): each the code gives is
// listed there, and each listed is in the JSON.
func TestPreflightStable(t *testing.T) {
	full := &Report{Version: 1, Lapilli: "v0", Items: []Item{{ID: "access", State: OK, What: "x", Why: "y", Fix: "z", Failed: []Failed{{Rule: "r", Reason: "parse error", Why: "w"}}}}, LeftOut: []string{"x"},
		Asked: Asked{Requests: 1, By: map[string]int{"prometheus": 1}, Refused: 0},
		Estimates: Estimates{Hourly: &Hourly{Runs: 1, Seconds: 1, Share: 1, WhileBacktest: &WhileBacktest{Share: 1}}, Backfill: &Backfill{Rate: 1, Seconds: 1, PieceHours: 1},
			Nights: &Nights{Hours: "x"}, Beside: &Beside{}, Backtests: &Backtests{Rate: 1, Fit: 1, WeekRules: 1, DayRules: 1}, Memory: &Memory{From: "caps", CgroupBytes: 1, BacktestBytes: 1}}}
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
		"":                                {"version", "lapilli", "items", "estimates", "left_out", "asked"},
		"items[]":                         {"id", "state", "what", "why", "fix", "failed"},
		"items[].failed[]":                {"rule", "reason", "why"},
		"asked":                           {"requests", "by", "refused"},
		"estimates":                       {"hourly", "backfill", "nights", "beside", "backtests", "memory"},
		"estimates.hourly":                {"runs", "requests", "most", "day", "by", "guard", "impact", "seconds", "share", "while_backtest"},
		"estimates.hourly.while_backtest": {"rate", "seconds", "share"},
		"estimates.backfill":              {"pieces", "requests", "most", "by", "guard", "impact", "rate", "seconds", "piece_hours"},
		"estimates.nights":                {"hours", "nights", "per_night"},
		"estimates.beside":                {"day", "by"},
		"estimates.backtests":             {"share", "rate", "parallel", "timeout_seconds", "fit", "rules", "days", "changed", "shared", "changed_days", "shared_days", "week_rules", "day_rules"},
		"estimates.memory":                {"from", "peak_bytes", "request_bytes", "limit_bytes", "cgroup_bytes", "backtest_bytes"},
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
		for _, part := range strings.Split(string(src), `id: "`)[1:] {
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
	// 31 bytes and a line break, which isn't of the key.
	c.Serve.NotifyWebhookSecret = file("signing", "signing-key-0123456789abcdefghi\n")
	p := newRun(c)
	p.secrets()
	it := itemOf(p, "secrets.files")
	if it == nil || it.State != Problem || !strings.Contains(it.Why, "holds the same token as --tasks-token-file") ||
		!strings.Contains(it.Why, "doesn't hold an https URL") || strings.Contains(it.What+it.Why, "shared-token") || strings.Contains(it.What+it.Why, "hooks.example") ||
		!strings.Contains(it.Why, "--notify-webhook-secret-file "+c.Serve.NotifyWebhookSecret+": 31 characters, under 32") || strings.Contains(it.What+it.Why, "signing-key") {
		t.Fatalf("%+v", it)
	}
	c.Serve.ChangesToken = file("changes", "another-token-0123456789")
	c.Serve.Slack = file("slack", "https://hooks.example/x")
	c.Serve.NotifyWebhookSecret = file("signing", "signing-key-0123456789abcdefghij\n")
	p = newRun(c)
	p.secrets()
	if it := itemOf(p, "secrets.files"); it == nil || it.State != OK || !strings.Contains(it.What, "23 characters") ||
		!strings.Contains(it.What, "--notify-webhook-secret-file "+c.Serve.NotifyWebhookSecret+": 32 characters") {
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

// A SQLite ledger whose file is cut short, a write-ahead log beside it, is
// a problem the item names as such, and its log is left as it was: SQLite
// would delete it, opening the file.
func TestLedgerCutShort(t *testing.T) {
	c := config()
	c.Serve.Ledger = t.TempDir()
	wal := filepath.Join(c.Serve.Ledger, "ledger.db-wal")
	log := bytes.Repeat([]byte("w"), 4096)
	if err := os.WriteFile(filepath.Join(c.Serve.Ledger, "ledger.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wal, log, 0o600); err != nil {
		t.Fatal(err)
	}
	p := newRun(c)
	p.ledger(context.Background())
	it := itemOf(p, "ledger.sqlite")
	if it == nil || it.State != Problem || !strings.Contains(it.What, "the ledger's file is cut short") || !strings.Contains(it.Why, "deletes the log") {
		t.Fatalf("%+v", it)
	}
	if b, err := os.ReadFile(wal); err != nil || !bytes.Equal(b, log) {
		t.Errorf("the log: %d bytes, %v", len(b), err)
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

// What a ruler may write can't be asked without writing: a Mimir's, and
// Loki's ruler's once --changes-in names a namespace of it (loki:NAMESPACE),
// are unknown, and nothing is asked.
func TestChangesRulers(t *testing.T) {
	c := config()
	c.Serve.Changes = true
	c.Serve.Loki, c.Serve.LokiLoaded, c.Serve.ChangesIn = "http://loki:3100", true, []string{"loki:shop"}
	p := newRun(c)
	p.changes(context.Background())
	if it := itemOf(p, "changes.loki"); it == nil || it.State != Unknown || !strings.Contains(it.Why, "local") {
		t.Fatalf("%+v", it)
	}
	if itemOf(p, "changes.ruler") != nil {
		t.Error("a Mimir's item without --ruler")
	}
	if a := p.al.asked(); a.Requests+a.Refused != 0 {
		t.Errorf("%+v", a)
	}
	c.Serve.ChangesIn = []string{"shop"}
	c.Serve.Ruler = "http://mimir:8080/prometheus/config/v1/rules"
	p = newRun(c)
	p.changes(context.Background())
	if itemOf(p, "changes.loki") != nil || itemOf(p, "changes.ruler") == nil {
		t.Errorf("--changes-in shop: %+v", p.r.Items)
	}
}

func ledgerProbed(there bool, version, knows int) ledger.Probed {
	return ledger.Probed{There: there, Version: version, Knows: knows}
}

// oneRule is a rule of PromQL with no for, evaluated every minute: its
// replay starts 15 minutes before its period (serve.Margin).
func oneRule() []rules.Rule {
	return []rules.Rule{{Name: "Down", Expr: `up{job="api"} == 0`, Interval: time.Minute}}
}

// A run replays from the last one's end less --settle: an hour more of
// settle is one more run a day whose replay crosses midnight, a request
// more. Each run asks --guard once, and each day of the backfill once.
func TestEstimateSettleAndGuard(t *testing.T) {
	day := func(settle time.Duration) *Hourly {
		c := config()
		c.Serve.Settle, c.Serve.Guard = settle, "x > 0"
		p := newRun(c)
		p.hourly(context.Background(), oneRule())
		return p.r.Estimates.Hourly
	}
	with, without := day(time.Hour), day(0)
	if with.Day-without.Day != 1 || with.Guard != 24 || without.Guard != 24 {
		t.Errorf("settle 1h: %+v; none: %+v", with, without)
	}
	c := config()
	c.Serve.Settle, c.Serve.Guard, c.Serve.Backfill = 0, "x > 0", 3*24*time.Hour
	p := newRun(c)
	p.backfill(context.Background(), oneRule())
	if b := p.r.Estimates.Backfill; b.Pieces != 3 || b.Guard != 3 {
		t.Errorf("%+v", b)
	}
}

// --impact is read over a run's period and the hour before it: a day's
// piece, 25 hours of minutes, is two requests of 1,441 steps.
func TestEstimateImpact(t *testing.T) {
	c := config()
	c.Serve.Settle, c.Serve.Impact, c.Serve.Backfill = 0, "x > 1", 3*24*time.Hour
	p := newRun(c)
	p.backfill(context.Background(), oneRule())
	if b := p.r.Estimates.Backfill; b.Impact != 6 {
		t.Errorf("%+v", b)
	}
}

// Named signals are each read as --impact is, over each run; beside the
// runs, each round looks at what each reads, a request a selector it
// counts (the 5xx selector through the requests it narrows, once), and a
// first start looks for where the data of each selector, and stand-in,
// begins.
func TestEstimateSignals(t *testing.T) {
	c := config()
	c.Serve.Settle, c.Serve.Backfill = 0, 3*24*time.Hour
	c.Serve.Options.Match = []string{"service"}
	c.Serve.Signals = []ledger.SignalDef{
		{Name: "errors", Expr: `sum by (service) (rate(http_requests_total{code=~"5.."}[5m])) / sum by (service) (rate(http_requests_total[5m])) > 0.05`, For: 5 * time.Minute, Record: true},
		{Name: "payments", Expr: "sum(rate(payments_succeeded_total[5m])) == 0", For: time.Minute, Match: []string{}},
	}
	p := newRun(c)
	p.backfill(context.Background(), oneRule())
	// Each day's piece two requests a signal; once, the errors' three data
	// looks (two selectors and a stand-in) and payments' one, and the
	// errors' one look at what it reads (payments is matched on none).
	if b := p.r.Estimates.Backfill; b.Impact != 2*6+3+1+1 {
		t.Errorf("backfill: %+v", b)
	}
	p.hourly(context.Background(), oneRule())
	if h := p.r.Estimates.Hourly; h.Impact != h.Runs*(2+1) {
		t.Errorf("hourly: %d runs, %d impact requests; want a read of each signal and a look at errors' a run", h.Runs, h.Impact)
	}
}

// The backfill's days more than a day behind are a catch-up, at the lower
// of --max-rate and --catch-up-rate; the rest at --max-rate, or, without
// it, at no rate the estimate can say: no more than the catch-up's time.
func TestEstimateCatchUp(t *testing.T) {
	at := func(maxRate, catchUp float64) *Backfill {
		c := config()
		c.Serve.Settle, c.Serve.Backfill, c.Serve.CatchUpRate, c.MaxRate = 0, 3*24*time.Hour, catchUp, maxRate
		p := newRun(c)
		p.backfill(context.Background(), oneRule()) // each day two requests: it crosses midnight
		return p.r.Estimates.Backfill
	}
	if b := at(0, 1); b.Requests != 6 || b.Rate != 1 || b.Seconds != 4 {
		t.Errorf("no --max-rate: %+v", b)
	}
	if b := at(2, 0.5); b.Rate != 0.5 || b.Seconds != 2*2/0.5+2/2.0 {
		t.Errorf("--catch-up-rate the lower: %+v", b)
	}
	if b := at(0.5, 2); b.Rate != 0.5 || b.Seconds != 6/0.5 {
		t.Errorf("--max-rate the lower: %+v", b)
	}
}

// While the catch-up waits outside its hours, a day more is behind each
// day: 27 days of 4,000 requests at 0.5 a second, three a night of five
// hours, take 13 nights, not 9. A night that holds no more than a day
// never catches up, and is a problem.
func TestEstimateNights(t *testing.T) {
	w, _ := serve.ParseWindow("01:00-06:00")
	nights := func(per int) (*Nights, *Item) {
		c := config()
		c.Serve.CatchUpHours = &w
		p := newRun(c)
		var days []map[string]int
		for range 27 {
			days = append(days, map[string]int{"prometheus": per})
		}
		p.nights(days, 0.5)
		return p.r.Estimates.Nights, itemOf(p, "estimate.nights")
	}
	if n, it := nights(4000); n.Nights != 13 || it.State != OK {
		t.Errorf("%+v %+v", n, it)
	}
	for _, per := range []int{9000, 10000} {
		if n, it := nights(per); n.Nights != -1 || it.State != Problem {
			t.Errorf("%d a day: %+v %+v", per, n, it)
		}
	}
}

// Each API is paced of its own: a run asking Prometheus and Loki as much
// takes as long as the one asked most takes, not both together.
func TestEstimateEachAPI(t *testing.T) {
	var rs []rules.Rule
	for i := range 100 {
		rs = append(rs, rules.Rule{Name: fmt.Sprintf("P%d", i), Expr: fmt.Sprintf(`up{job="p%d"} == 0`, i), Interval: time.Minute},
			rules.Rule{Name: fmt.Sprintf("L%d", i), Expr: fmt.Sprintf(`sum(rate({app="l%d"} |= "error" [5m])) > 1`, i), Interval: time.Minute,
				Logs: rules.LokiRuler})
	}
	c := config()
	c.Serve.Loki = "http://loki:3100"
	c.MaxRate = 0.07 // a run crossing midnight: 200 to each, 2,857s; 400 together, 5,714s
	p := newRun(c)
	p.hourly(context.Background(), rs)
	h, it := p.r.Estimates.Hourly, itemOf(p, "estimate.hourly")
	if h.By["loki"] == 0 || h.By["prometheus"] == 0 || it.State != OK || h.Seconds > 3600 {
		t.Errorf("%+v %+v", h, it)
	}
}

// What a server said is redacted whole, then cut: a host that the cut at
// 200 bytes would leave in two is redacted all the same.
func TestRedactThenCut(t *testing.T) {
	host := "internal-k8s-monitoring-prometheus-0123456789abcdef.eu-west-1.elb.amazonaws.example.com"
	c := config()
	c.Scrubber = func(...[2]string) func(string) string {
		return func(s string) string { return strings.ReplaceAll(s, host, "[--prometheus]") }
	}
	p := newRun(c)
	p.add(Item{ID: "history.reach", State: Problem, What: "x", Why: "not let in: " + said(fmt.Errorf("HTTP 403: %s at %s", strings.Repeat("x", 150), host))})
	p.redact()
	why := p.r.Items[0].Why
	if strings.Contains(why, host[:20]) || !strings.Contains(why, "[--prometheus]") && !strings.HasSuffix(why, "…") || len(why) > 250 {
		t.Errorf("%q", why)
	}
}

// The session that holds a Postgres ledger's lock is said by its number
// and when it connected: never the role it connected as, the name it gave
// itself or its address.
func TestHeldBy(t *testing.T) {
	h := &ledger.LockedError{PID: 4242, Role: "ledger_writer_x", App: "copy-job-x", From: "10.9.8.7", Alone: true,
		Since: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	s := cutSaid(heldBy(h))
	if !strings.Contains(s, "session 4242") || !strings.Contains(s, "2026-11-16T12:00:00Z") || !strings.Contains(s, "alone") {
		t.Errorf("%q", s)
	}
	for _, hidden := range []string{"ledger_writer_x", "copy-job-x", "10.9.8.7"} {
		if strings.Contains(s, hidden) {
			t.Errorf("%q says %q", s, hidden)
		}
	}
	if h.Role == "" {
		t.Error("the holder given was changed")
	}
}

// TestHiddenInSaidOnly: what is to be kept out of what a server said only
// (hideSaid: a Postgres user's or database's name, which may be lapilli or
// postgres) is taken out of what the server said, and left in the report's
// own words.
func TestHiddenInSaidOnly(t *testing.T) {
	c := config()
	c.Scrubber = func(more ...[2]string) func(string) string {
		return func(s string) string {
			for _, m := range more {
				s = strings.ReplaceAll(s, m[1], "["+m[0]+"]")
			}
			return s
		}
	}
	p := newRun(c)
	p.hideSaid("user", "lapilli")
	p.add(Item{ID: "ledger.postgres", State: Problem, What: "lapilli serve can't read the ledger",
		Why: "it said: " + said(errors.New(`password authentication failed for user "lapilli"`)), Fix: "start lapilli serve again"})
	p.redact()
	it := p.r.Items[0]
	if it.What != "lapilli serve can't read the ledger" || it.Fix != "start lapilli serve again" ||
		it.Why != `it said: password authentication failed for user "[user]"` {
		t.Errorf("%q\n%q\n%q", it.What, it.Why, it.Fix)
	}
}

// A Postgres ledger's schema is said as the server said it: a schema named
// after the user, which the search_path ("$user") makes the ledger's, is
// redacted as the user's name is in the rest of what the server said, and
// the report's own words are left. Another schema is named.
func TestPostgresSchemaSaid(t *testing.T) {
	c := config()
	c.Scrubber = func(more ...[2]string) func(string) string {
		return func(s string) string {
			for _, m := range more {
				s = strings.ReplaceAll(s, m[1], "["+m[0]+"]")
			}
			return s
		}
	}
	for _, x := range []struct {
		pr        ledger.Probed
		what, fix string
	}{
		{ledger.Probed{There: true, Version: 18, Knows: 19, Schema: "lapilli"}, `in schema "[user]"`, `may not create in schema "[user]",`},
		{ledger.Probed{There: true, Version: 20, Knows: 19, Schema: "lapilli", Create: true}, `in schema "[user]"`, "a ledger a newer lapilli wrote"},
		{ledger.Probed{There: true, Version: 19, Knows: 19, Schema: "ledger"}, `in schema "ledger"`, ""},
	} {
		p := newRun(c)
		p.hideSaid("user", "lapilli")
		p.postgresProbed(x.pr)
		p.redact()
		it := itemOf(p, "ledger.postgres")
		if it == nil || !strings.HasSuffix(it.What, x.what) || !strings.Contains(it.Fix, x.fix) || strings.Contains(it.What, `"lapilli"`) {
			t.Errorf("%+v: %+v", x.pr, it)
		}
	}
}

// Grafana's rules on more than one data source for their PromQL are said
// to be replayed against one history, unknown; on one, or none, nothing is
// said, and the other items are the same.
func TestRulesSources(t *testing.T) {
	grafana := func(uids ...string) []rules.Rule {
		var b strings.Builder
		b.WriteString("apiVersion: 1\ngroups:\n  - orgId: 1\n    name: g\n    folder: F\n    interval: 1m\n    rules:\n")
		for i, uid := range uids {
			fmt.Fprintf(&b, `      - uid: r%d
        title: Rule%d
        condition: B
        data:
          - refId: A
            relativeTimeRange: {from: 600, to: 0}
            datasourceUid: %s
            model: {expr: up, instant: true}
          - refId: B
            datasourceUid: __expr__
            model: {type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [0]}}]}
`, i, i, uid)
		}
		rs, err := rules.Parse([]byte(b.String()))
		if err != nil {
			t.Fatal(err)
		}
		return rs
	}
	sources := func(rs []rules.Rule) []Item {
		p := &run{c: config(), r: &Report{}}
		p.replayable(rs, rules.Picked{})
		return p.r.Items
	}
	items := sources(grafana("prom-a", "prom-b"))
	if len(items) != 2 || items[1].ID != "rules.sources" || items[1].State != Unknown ||
		items[1].What != "Grafana's rules query 2 data sources: prom-a, prom-b" ||
		items[1].Why != "each is replayed against one history, --prometheus; whether they are one Prometheus isn't known" {
		t.Errorf("two data sources: %+v", items)
	}
	if items := sources(grafana("prom-a", "prom-a")); len(items) != 1 {
		t.Errorf("one data source: %+v", items)
	}
	if items := sources(rulesOf(t, 2, 0)); len(items) != 1 {
		t.Errorf("rules that aren't Grafana's: %+v", items)
	}
	many := sources(grafana("p1", "p2", "p3", "p4", "p5", "p6", "p7"))
	if !strings.HasSuffix(many[1].What, "7 data sources: p1, p2, p3, p4, p5 and 2 more") {
		t.Errorf("seven: %q", many[1].What)
	}
}

// Rules read from Terraform's JSON are counted as Grafana's, from Terraform,
// a paused one too.
func TestCountedTerraform(t *testing.T) {
	rs := []rules.Rule{{Name: "a", Address: "grafana_rule_group.g", Graph: &rules.Graph{}}, {Name: "b", Address: "grafana_rule_group.g", Unsupported: rules.Paused}}
	if got, want := counted(rs, -1), "2 alert rules (Grafana, from Terraform)"; got != want {
		t.Errorf("%q, want %q", got, want)
	}
}
