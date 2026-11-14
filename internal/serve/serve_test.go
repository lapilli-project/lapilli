package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/ledger/ledgertest"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/verdict"
)

// now is a Tuesday, so the week before is whole.
var now = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// history answers "errors > 5" with a checkout series true for an hour
// from 30h ago, and a search series true for ten minutes in each of the
// twenty hours before that; "impact" with ten minutes 6h ago.
type history struct {
	sync.Mutex
	asked int
}

func (h *history) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	h.Lock()
	h.asked++
	h.Unlock()
	run := func(labels map[string]string, from, to time.Time) prom.Series {
		s := prom.Series{Labels: labels}
		for t := from; !t.After(to); t = t.Add(step) {
			if !t.Before(start) && !t.After(end) {
				s.Samples = append(s.Samples, prom.Sample{T: t, V: 1})
			}
		}
		return s
	}
	var out []prom.Series
	switch expr {
	case "errors > 5", "errors > 1":
		out = append(out, run(map[string]string{"service": "checkout"}, now.Add(-30*time.Hour), now.Add(-29*time.Hour)))
		for k := 0; k < 20; k++ {
			at := now.Add(-50*time.Hour + time.Duration(k)*time.Hour)
			out = append(out, run(map[string]string{"service": "search"}, at, at.Add(10*time.Minute)))
		}
	case "busy > 1":
		// Two series at once, then one: as a whole, one firing.
		out = append(out, run(map[string]string{"service": "checkout"}, now.Add(-30*time.Hour), now.Add(-29*time.Hour)),
			run(map[string]string{"service": "cart"}, now.Add(-29*time.Hour-30*time.Minute), now.Add(-28*time.Hour)))
	case "slow":
		<-ctx.Done()
		return nil, ctx.Err()
	case "turned":
		// Turned away to the end, as a quota does.
		return nil, fmt.Errorf("%w (asked 7 times over 3m2s)", &prom.APIError{Status: 429, Msg: "slow down"})
	case "waiting":
		// Told to wait past the time the run has, as prom gives it up.
		return nil, fmt.Errorf("the API said to wait %w, after HTTP 429: slow down", prom.ErrPastDeadline)
	case "impact":
		out = append(out, run(map[string]string{"service": "search"}, now.Add(-6*time.Hour), now.Add(-6*time.Hour+10*time.Minute)))
	}
	var kept []prom.Series
	for _, s := range out {
		if len(s.Samples) > 0 {
			kept = append(kept, s)
		}
	}
	return kept, nil
}

const ruleFile = `groups:
  - name: shop
    rules:
      - alert: Errors
        expr: errors > 5
        for: 2m
        labels: {severity: page}
`

func server(t *testing.T, change func(*Config)) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	rules := filepath.Join(dir, "rules")
	os.MkdirAll(rules, 0o755)
	os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte(ruleFile), 0o644)
	incidents := filepath.Join(dir, "incidents.yaml")
	os.WriteFile(incidents, []byte("incidents:\n  - {id: INC-1, title: Checkout down, start: 2026-11-02T06:01:00Z, end: 2026-11-02T06:40:00Z, labels: {service: checkout}}\n"), 0o644)
	cfg := Config{
		RulesPath: rules, Incidents: incidents, Impact: "impact", ImpactFor: 5 * time.Minute,
		Options: verdict.Options{Lead: 15 * time.Minute, RefireWithin: time.Hour, UnknownLen: time.Hour, Match: []string{"service"}},
		Step:    time.Minute, Loc: time.UTC, Ledger: filepath.Join(dir, "ledger"), LedgerURLFile: ledgertest.File(t),
		Every: time.Hour, Settle: time.Hour, Backfill: 3 * 24 * time.Hour, Watch: time.Minute,
		Digest: Weekly{time.Monday, 9, 0}, Timeout: time.Minute, Parallel: 4, Listen: "127.0.0.1:0", Version: "test",
		AllowedHosts: []string{"example.com"}, // httptest's
	}
	if change != nil {
		change(&cfg)
	}
	s, err := New(context.Background(), cfg, &history{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	t.Cleanup(func() { s.Close() })
	return s, rules
}

// get asks the handler for a page, as a browser on the same site would.
func get(t *testing.T, h http.Handler, path string) (int, string, http.Header) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.String(), w.Header()
}

func post(t *testing.T, h http.Handler, path string, form url.Values, site string) (int, string, http.Header) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", site)
	r.Header.Set("X-Forwarded-Email", "ana@example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.String(), w.Header()
}

func TestServe(t *testing.T) {
	ctx := context.Background()
	s, rules := server(t, func(c *Config) { c.UserHeader = "X-Forwarded-Email" })
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	s.ready.Store(true)
	h := s.Handler()

	// The backfill found the checkout firing, which caught INC-1, and the
	// search firings, which caught nothing.
	code, body, _ := get(t, h, "/api/v1/report?from=2026-10-31T00:00:00Z&to=2026-11-03T12:00:00Z")
	var rep verdict.Report
	if err := json.Unmarshal([]byte(body), &rep); code != 200 || err != nil {
		t.Fatalf("%d %v\n%s", code, err, body)
	}
	if len(rep.Rules) != 1 || rep.Rules[0].Firings != 21 || rep.Rules[0].White != 1 || !rep.Rules[0].Noisy || rep.Rules[0].Severity != "page" {
		t.Errorf("rules %+v", rep.Rules)
	}
	var ids []string
	for _, in := range rep.Incidents {
		ids = append(ids, in.ID)
	}
	if strings.Join(ids, " ") != "INC-1 impact-20261103T0600Z" || len(rep.Incidents[0].CaughtBy) != 1 {
		t.Errorf("incidents %+v", rep.Incidents)
	}

	for _, path := range []string{"/", "/rules", "/rules?weeks=12", "/rules/Errors", "/incidents", "/incidents/INC-1", "/firings?day=2026-11-02",
		"/settings", "/runs", "/api/v1/rules", "/api/v1/incidents", "/api/v1/runs", "/api/v1/settings", "/metrics", "/healthz", "/readyz", "/static/lapilli.css"} {
		code, body, hd := get(t, h, path)
		if code != 200 {
			t.Errorf("%s: %d\n%s", path, code, body)
		}
		if !strings.Contains(hd.Get("Content-Security-Policy"), "default-src 'none'") {
			t.Errorf("%s: no CSP", path)
		}
	}
	_, body, _ = get(t, h, "/")
	if !strings.Contains(body, "Errors") || !strings.Contains(body, "impact-20261103T0600Z") {
		t.Errorf("the overview should name the noisy rule and the missed impact window:\n%s", body)
	}
	_, body, _ = get(t, h, "/metrics")
	if !strings.Contains(body, `lapilli_rule_firings{rule="Errors",severity="page"} 21`) || !strings.Contains(body, "lapilli_serve_cursor_timestamp_seconds 1793707200") {
		t.Errorf("metrics:\n%s", body)
	}
	// Serve puts together its own report for these, so what a rule adds is
	// checked here as well as in verdict's.
	for _, want := range []string{`lapilli_rule_incidents{rule="Errors",severity="page"} 1`, `lapilli_rule_incidents_first{rule="Errors",severity="page"} 1`, `lapilli_rule_incidents_alone{rule="Errors",severity="page"} 1`} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in metrics:\n%s", want, body)
		}
	}
	if code, _, _ := get(t, h, "/rules/Nothing"); code != 404 {
		t.Errorf("an unknown rule: %d", code)
	}

	// A page asked for again, unchanged, isn't sent again.
	r := httptest.NewRequest(http.MethodGet, "/rules", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	r.Header.Set("If-None-Match", w.Header().Get("ETag"))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotModified {
		t.Errorf("If-None-Match: %d", w.Code)
	}

	// A black stone was real: written down from the UI, it turns white.
	_, body, _ = get(t, h, "/firings?day=2026-11-01&black=1")
	i := strings.Index(body, `action="/firings/`)
	if i < 0 {
		t.Fatalf("no black stone to say was real:\n%s", body)
	}
	action := body[i+len(`action="`):]
	action = action[:strings.Index(action, `"`)]
	if code, body, _ := post(t, h, action, nil, "cross-site"); code != http.StatusForbidden {
		t.Errorf("a post from another site: %d %s", code, body)
	}
	code, _, hd := post(t, h, action, nil, "same-origin")
	if code != http.StatusSeeOther || hd.Get("Location") != "/incidents/L-1" {
		t.Fatalf("this was real: %d %v", code, hd)
	}
	_, body, _ = get(t, h, "/api/v1/incidents?from=2026-10-31T00:00:00Z&to=2026-11-03T12:00:00Z")
	if !strings.Contains(body, `"id": "L-1"`) || !strings.Contains(body, `"by": "ana@example.com"`) {
		t.Errorf("the incident written:\n%s", body)
	}

	// Settings changed in the UI judge everything again.
	if code, body, _ := post(t, h, "/settings", url.Values{"lead": {"bad"}}, "same-origin"); code != 400 {
		t.Errorf("a bad setting: %d %s", code, body)
	}
	if code, _, _ := post(t, h, "/settings", url.Values{"match": {"none"}, "noisy-firings": {"30"}}, "same-origin"); code != http.StatusSeeOther {
		t.Fatalf("settings: %d", code)
	}
	// Here, or, with replicas, through the ledger, for whichever leads.
	if _, asked := s.rejudgeAsked(ctx); !s.rejudge.Load() && !asked {
		t.Error("a change to --match should judge again")
	}
	s.jobs(ctx, false)
	_, body, _ = get(t, h, "/api/v1/report?from=2026-10-31T00:00:00Z&to=2026-11-03T12:00:00Z&firings=false")
	rep = verdict.Report{}
	json.Unmarshal([]byte(body), &rep)
	if rep.Rules[0].Noisy || rep.NoisyAt.Firings != 30 {
		t.Errorf("with 30 firings to be noisy, it isn't: %+v %+v", rep.Rules[0], rep.NoisyAt)
	}
	_, body, _ = get(t, h, "/settings.yaml")
	if !strings.Contains(body, `match: []`) || !strings.Contains(body, `"--noisy-firings=30"`) {
		t.Errorf("settings.yaml:\n%s", body)
	}

	// A change to the rules is a new version, reviewed over the week
	// before.
	os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte(strings.Replace(ruleFile, "for: 2m", "for: 15m", 1)), 0o644)
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, false)
	_, body, _ = get(t, h, "/rules/Errors")
	if !strings.Contains(body, "Changes reviewed") || !strings.Contains(body, "fired 21 → 1") || !strings.Contains(body, "Versions") {
		t.Errorf("the rule's page should show the review and the versions:\n%s", body)
	}
}

// A run before the rules have been read replays nothing and leaves the
// cursor where it was, so the backfill isn't passed over.
func TestRulesFirst(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, nil)
	s.jobs(ctx, true)
	if c, err := s.l.Cursor(ctx); err != nil || !c.IsZero() {
		t.Fatalf("cursor %s %v: moved with no rules to replay", c, err)
	}
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	lo, _, err := s.l.Bounds(ctx)
	if err != nil || lo.IsZero() || lo.After(now.Add(-40*time.Hour)) {
		t.Errorf("the backfill should have found the firings 50h ago: earliest %s %v", lo, err)
	}
}

func TestReadOnly(t *testing.T) {
	s, _ := server(t, func(c *Config) { c.ReadOnly = true })
	h := s.Handler()
	if code, _, _ := post(t, h, "/incidents", url.Values{"title": {"x"}, "start": {"2026-11-02T10:00"}}, "same-origin"); code != http.StatusForbidden {
		t.Errorf("read-only: %d", code)
	}
	_, body, _ := get(t, h, "/incidents")
	if strings.Contains(body, "<form method=\"post\"") {
		t.Error("a read-only page shouldn't offer to write")
	}
}

func TestDigest(t *testing.T) {
	var got string
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
	}))
	defer slack.Close()
	hook := filepath.Join(t.TempDir(), "hook")
	os.WriteFile(hook, []byte(slack.URL+"/services/T/B/SECRET"), 0o600)
	s, _ := server(t, func(c *Config) { c.Slack = hook; c.Backfill = 10 * 24 * time.Hour })
	ctx := context.Background()
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.l.PutTask(ctx, ledger.Task{Title: "Tune Errors", Due: now.Add(-48 * time.Hour)}, "ana")
	s.jobs(ctx, true)
	// The week before is Monday to Monday; its noisy rule gets a longer
	// "for"; and how the tasks stand.
	if !strings.Contains(got, "Lapilli  2026-10-26 00:00 → 2026-11-02 00:00") || !strings.Contains(got, `2m → 15m`) ||
		!strings.Contains(got, "Tasks: 1 open, 1 overdue, 0 done in the week.") {
		t.Errorf("the digest of the week before:\n%s", got)
	}
	got = ""
	s.jobs(ctx, true)
	if got != "" {
		t.Error("the digest went twice")
	}
}

func TestWeekly(t *testing.T) {
	w, err := ParseWeekly("mon@09:00")
	if err != nil {
		t.Fatal(err)
	}
	for at, want := range map[time.Time]time.Time{
		now: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 2, 8, 59, 0, 0, time.UTC): time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC):  time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC),
	} {
		if got := w.Last(at, time.UTC); !got.Equal(want) {
			t.Errorf("Last(%s) = %s, want %s", at, got, want)
		}
	}
	for _, bad := range []string{"mon", "monday@9", "xyz@09:00", "mon@25:00"} {
		if _, err := ParseWeekly(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// A run that runs out of time is tried over less, but not the same hours
// again and again; the rule that couldn't finish is replayed later from
// where it was missed, and the rest, impact included, are recorded.
func TestTimeout(t *testing.T) {
	ctx := context.Background()
	s, rules := server(t, func(c *Config) { c.Timeout = 100 * time.Millisecond; c.Backfill = 3 * time.Hour })
	os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte(ruleFile+"      - alert: Slow\n        expr: slow\n"), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	h := s.h.(*history)
	s.jobs(ctx, true)
	if c, _ := s.l.Cursor(ctx); !c.Equal(now) {
		t.Errorf("cursor %s, want %s: a slow rule shouldn't stall the rest", c, now)
	}
	if h.asked > 40 {
		t.Errorf("asked Prometheus %d times for three hours", h.asked)
	}
	missed, _ := s.l.Missed(ctx)
	if at, ok := missed["Slow"]; !ok || at.After(now.Add(-3*time.Hour)) {
		t.Errorf("missed %v: Slow should be replayed from the backfill's start, or before", missed)
	}
	runs, _ := s.l.Runs(ctx, 10)
	for _, r := range runs {
		if r.Kind == "hourly" && r.Failed["Slow"] == "" {
			t.Errorf("run %+v: should say Slow wasn't replayed", r)
		}
	}
}

// A rule keeps its name when a rule of the same name comes or goes.
func TestNames(t *testing.T) {
	ctx := context.Background()
	s, rules := server(t, nil)
	write := func(severities ...string) {
		var b strings.Builder
		b.WriteString("groups:\n  - name: shop\n    rules:\n")
		for _, sev := range severities {
			b.WriteString("      - alert: Errors\n        expr: errors > 5\n        labels: {severity: " + sev + "}\n")
		}
		b.WriteString("      - alert: Disk\n        expr: disk > 0.9\n")
		os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte(b.String()), 0o644)
		raw := ""
		if err := s.look(ctx, &raw); err != nil {
			t.Fatal(err)
		}
	}
	names := func() []string {
		v, _, _ := s.l.Latest(ctx)
		return v.Names
	}
	write("page", "ticket")
	write("page")
	write("page")
	if got := names(); got[0] != "Errors [severity=page]" || got[1] != "Disk" {
		t.Errorf("names %q", got)
	}
}

// A request given up on isn't kept as the page.
func TestCancelledNotCached(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, nil)
	raw := ""
	s.look(ctx, &raw)
	s.jobs(ctx, true)
	h := s.Handler()
	gone, cancel := context.WithCancel(ctx)
	cancel()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/rules", nil).WithContext(gone))
	if _, body, _ := get(t, h, "/rules"); !strings.Contains(body, "Errors") {
		t.Errorf("after a cancelled request, the page is:\n%s", body)
	}
}
