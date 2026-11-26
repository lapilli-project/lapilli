package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/ledger/ledgertest"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// Named impact signals in lapilli serve (docs/design/signals.md, step 2).

// sigHistory is history, with what count by (LABELS) (SELECTOR) answers
// for each selector (series, by their labels: each there all along), the
// expressions that fail, and those whose windows it gives.
type sigHistory struct {
	*history
	mu      sync.Mutex
	counted map[string][]map[string]string
	fail    map[string]bool
	windows map[string][]prom.Series
	asked   []string
}

func (h *sigHistory) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	h.mu.Lock()
	h.asked = append(h.asked, expr)
	fail, ws := h.fail[expr], h.windows[expr]
	h.mu.Unlock()
	if fail {
		return nil, &prom.APIError{Status: 422, Msg: "query timed out in expression evaluation"}
	}
	if rest, ok := strings.CutPrefix(expr, "count by ("); ok {
		by, sel, _ := strings.Cut(rest, ") (")
		sel = strings.TrimSuffix(sel, ")")
		var out []prom.Series
		for _, l := range h.counted[sel] {
			kept := map[string]string{}
			for _, k := range strings.Split(by, ", ") {
				if v, ok := l[k]; ok {
					kept[k] = v
				}
			}
			out = append(out, prom.Series{Labels: kept, Samples: []prom.Sample{{T: start, V: 1}}})
		}
		return out, nil
	}
	if ws != nil {
		var out []prom.Series
		for _, se := range ws {
			x := prom.Series{Labels: se.Labels}
			for _, p := range se.Samples {
				if !p.T.Before(start) && !p.T.After(end) {
					x.Samples = append(x.Samples, p)
				}
			}
			if len(x.Samples) > 0 {
				out = append(out, x)
			}
		}
		return out, nil
	}
	return h.history.QueryRange(ctx, expr, start, end, step)
}

// minutes is a series true each step from from to to.
func minutes(labels map[string]string, from, to time.Time) prom.Series {
	s := prom.Series{Labels: labels}
	for t := from; !t.After(to); t = t.Add(time.Minute) {
		s.Samples = append(s.Samples, prom.Sample{T: t, V: 1})
	}
	return s
}

// signalServer is server, reading named signals from h, with no incidents
// file: what keeps the record is the signals alone.
func signalServer(t *testing.T, h *sigHistory, change func(*Config)) *Server {
	t.Helper()
	dir := t.TempDir()
	rules := filepath.Join(dir, "rules")
	os.MkdirAll(rules, 0o755)
	os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte(ruleFile), 0o644)
	cfg := Config{
		RulesPath: rules,
		Options:   verdict.Options{Lead: 15 * time.Minute, RefireWithin: time.Hour, UnknownLen: time.Hour, Match: []string{"service"}},
		Step:      time.Minute, Loc: time.UTC, Ledger: filepath.Join(dir, "ledger"), LedgerURLFile: ledgertest.File(t),
		Every: time.Hour, Settle: time.Hour, Backfill: 3 * 24 * time.Hour, Watch: time.Minute,
		Digest: Weekly{time.Monday, 9, 0}, Timeout: time.Minute, Parallel: 4, Listen: "127.0.0.1:0", Version: "test",
		AllowedHosts: []string{"example.com"}, UserHeader: "X-Forwarded-Email", Admins: []string{"ana@example.com"},
		Signals: []ledger.SignalDef{
			{Name: "errors", Expr: "http_requests_total > 1e9", For: 5 * time.Minute, Record: true},
			{Name: "probe", Expr: "probe_success == 0", For: 2 * time.Minute, Match: []string{"service"}, Record: true},
		},
	}
	if change != nil {
		change(&cfg)
	}
	if h.history == nil {
		h.history = &history{}
	}
	s, err := New(context.Background(), cfg, h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	t.Cleanup(func() { s.Close() })
	raw := ""
	if err := s.look(context.Background(), &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(context.Background(), true)
	s.ready.Store(true)
	return s
}

// shopHistory: the errors signal reads every service's requests (checkout
// and search), with data all along; the probe probes checkout alone, its
// data from Monday 12:00, so it keeps the record from the Monday after.
func shopHistory() *sigHistory {
	return &sigHistory{
		history: &history{begins: map[string][]time.Time{"probe_success": {time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}}},
		counted: map[string][]map[string]string{
			"http_requests_total": {{"service": "checkout"}, {"service": "search"}},
			"probe_success":       {{"service": "checkout", "instance": "https://checkout.example.com"}},
		},
	}
}

// reportOf is the ledger's report of the week from Monday 2 November.
func reportOf(t *testing.T, s *Server) verdict.Report {
	t.Helper()
	code, body, _ := get(t, s.Handler(), "/api/v1/report?from=2026-11-02T00:00:00Z&to=2026-11-03T12:00:00Z")
	if code != http.StatusOK {
		t.Fatalf("report: %d %s", code, body)
	}
	var rep verdict.Report
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

// Each signal is read on its own, keeps the record from its own start, and
// a firing waits for the signals that could have seen it: those whose
// selectors' series have its values, not only those that returned them. A
// search firing is the errors signal's alone, judged from its start; a
// checkout firing waits for the probe's too, which keeps no record of the
// week its data began in.
func TestSignalsJudgedByTheirOwn(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s := signalServer(t, shopHistory(), nil)
	s.cfg.RecordByWeek = true
	rows, err := s.l.SignalRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	starts := map[string]time.Time{}
	for _, r := range rows {
		starts[r.Name] = r.Kept.From()
	}
	if monday := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC); !starts["probe"].Equal(monday) || starts["errors"].IsZero() || !starts["errors"].Before(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("starts: %v", starts)
	}
	rep := reportOf(t, s)
	var search, checkout []verdict.Judged
	for _, f := range rep.Firings {
		switch f.Labels["service"] {
		case "search":
			search = append(search, f)
		case "checkout":
			checkout = append(checkout, f)
		}
	}
	if len(search) != 6 || len(checkout) != 1 {
		t.Fatalf("firings: %d of search, %d of checkout", len(search), len(checkout))
	}
	for _, f := range search {
		if f.NoRecord || !slices.Equal(f.Signals, []string{"errors"}) {
			t.Errorf("a search firing: no record %v, tied to %v; want judged, by errors alone", f.NoRecord, f.Signals)
		}
	}
	if f := checkout[0]; !f.NoRecord || !slices.Equal(f.Signals, []string{"errors", "probe"}) {
		t.Errorf("the checkout firing: no record %v, tied to %v; want waiting for the probe", f.NoRecord, f.Signals)
	}
	if rep.Settings == nil || len(rep.Settings.ImpactSignals) != 2 || !rep.Settings.ByWeek {
		t.Errorf("settings: %+v", rep.Settings)
	}
	// The summary counts the same: of the week's seven firings, the one
	// waiting for the probe counted, not judged.
	var sum summary
	code, body, _ := get(t, s.Handler(), "/api/v1/summary?from=2026-11-02&to=2026-11-02")
	if code != http.StatusOK || json.Unmarshal([]byte(body), &sum) != nil || len(sum.Rows) != 1 {
		t.Fatalf("summary: %d %s", code, body)
	}
	if r := sum.Rows[0]; r.Firings != 7 || r.Unjudged != 1 || r.NoRecord || r.Precision == nil || *r.Precision != 0 {
		t.Errorf("summary row: %d firings, %d unjudged, no record %v, precision %v", r.Firings, r.Unjudged, r.NoRecord, r.Precision)
	}
	if len(sum.Settings.ImpactSignals) != 2 || sum.Settings.ImpactSignals[1].Name != "probe" || sum.Settings.ImpactSignals[1].Start == nil {
		t.Errorf("summary settings: %+v", sum.Settings.ImpactSignals)
	}
	// What a replay judges in memory (a backtest, the digest) ties a
	// firing as the ledger ties its alert: by what each signal has seen.
	o, err := s.options(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ties := o.Signals.Ties("Errors", map[string]string{"service": "search"}, o.Match); !slices.Equal(o.Signals.Names(ties), []string{"errors"}) {
		t.Errorf("a search firing replayed is tied to %v", o.Signals.Names(ties))
	}
}

// Two signals' windows of one minute are two incidents, each keyed by its
// signal, each matched on its own labels; what is decided of one holds for
// it alone, and a run after it goes on with each signal's own.
func TestSignalWindowsKeyed(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	h := shopHistory()
	at := time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC) // when Errors fires for checkout
	// And again in the hour the next run replays.
	late := now.Add(-40 * time.Minute)
	h.windows = map[string][]prom.Series{
		"http_requests_total > 1e9": {minutes(map[string]string{"service": "checkout"}, at, at.Add(10*time.Minute)),
			minutes(map[string]string{"service": "cart"}, late, late.Add(10*time.Minute))},
		"probe_success == 0": {minutes(map[string]string{"service": "checkout", "instance": "https://checkout.example.com"}, at, at.Add(10*time.Minute)),
			minutes(map[string]string{"service": "cart"}, late, late.Add(10*time.Minute))},
	}
	s := signalServer(t, h, nil)
	ws, err := s.l.ImpactWindows(ctx, at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil || len(ws) != 2 {
		t.Fatalf("windows: %+v %v", ws, err)
	}
	keys := map[string]string{}
	for _, w := range ws {
		keys[w.Signal] = w.ID
		if len(w.CaughtBy) != 1 || w.CaughtBy[0].Rule != "Errors" {
			t.Errorf("%s: caught by %+v", w.ID, w.CaughtBy)
		}
	}
	if keys["errors"] != "impact-errors-20261102T0600Z" || keys["probe"] != "impact-probe-20261102T0600Z" {
		t.Fatalf("keys: %v", keys)
	}
	o, err := s.options(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.Decide(ctx, keys["probe"], "dismissed", "", "ana@example.com", o); err != nil {
		t.Fatal(err)
	}
	lateKey := "impact-probe-" + late.UTC().Format("20060102T1504Z")
	if _, err := s.l.Decide(ctx, lateKey, "dismissed", "", "ana@example.com", o); err != nil {
		t.Fatal(err)
	}
	runDue(s, 2)
	s.jobs(ctx, true)
	// The late windows were found again by the run: the probe's keeps what
	// was decided of it, the errors' of the same minute and labels gets
	// nothing of it.
	lws, err := s.l.ImpactWindows(ctx, late.Add(-time.Minute), late.Add(time.Minute))
	if err != nil || len(lws) != 2 {
		t.Fatalf("late windows: %+v %v", lws, err)
	}
	for _, w := range lws {
		if want := map[string]string{"errors": "", "probe": "dismissed"}[w.Signal]; w.State != want {
			t.Errorf("%s: %q, want %q", w.ID, w.State, want)
		}
	}
	ws, err = s.l.ImpactWindows(ctx, at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil || len(ws) != 2 {
		t.Fatalf("windows after a run: %+v %v", ws, err)
	}
	for _, w := range ws {
		want := map[string]string{"errors": "", "probe": "dismissed"}[w.Signal]
		if w.ID != keys[w.Signal] || w.State != want {
			t.Errorf("%s (%s): %q, want %s, %q", w.ID, w.Signal, w.State, keys[w.Signal], want)
		}
	}
	// Confirming the errors' window writes down an incident carrying its
	// signal and match: caught as the window was.
	made, err := s.l.Decide(ctx, keys["errors"], "confirmed", "", "ana@example.com", o)
	if err != nil {
		t.Fatal(err)
	}
	in, err := s.l.IncidentByID(ctx, made)
	if err != nil || in.Signal != "errors" || in.Match != nil || len(in.CaughtBy) != 1 {
		t.Errorf("confirmed: %+v %v", in, err)
	}
}

// A signal that can't be read is missed for the run, which records the
// rest; a later run reads it from where it was missed, its windows in
// from there, and its record stands meanwhile.
func TestSignalMissedCatchesUp(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	h := shopHistory()
	s := signalServer(t, h, nil)
	runDue(s, 1)
	s.jobs(ctx, true) // errors marked as read all along; the probe read again
	cursor, err := s.l.Cursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := s.l.SignalKept(ctx, "errors")
	if err != nil || !kept.AgainDone("5m http_requests_total > 1e9") {
		t.Fatalf("errors: %+v %v", kept, err)
	}
	// Errors fails for the next run, which replays from an hour before
	// the cursor, over which it had a window.
	from := cursor.Add(-s.cfg.Settle)
	at := from.Add(10 * time.Minute)
	h.mu.Lock()
	h.windows = map[string][]prom.Series{"http_requests_total > 1e9": {minutes(map[string]string{"service": "checkout"}, at, at.Add(10*time.Minute))}}
	h.fail = map[string]bool{"http_requests_total > 1e9": true}
	h.mu.Unlock()
	runDue(s, 3)
	s.jobs(ctx, true)
	missedFrom := time.Time{}
	rows, err := s.l.SignalRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Name == "errors" {
			missedFrom = r.Missed
		}
	}
	if !missedFrom.Equal(from) {
		t.Fatalf("missed from %s, want %s", missedFrom, from)
	}
	if next, _ := s.l.Cursor(ctx); !next.After(cursor) {
		t.Fatalf("the run wasn't recorded: cursor %s", next)
	}
	if k, _ := s.l.SignalKept(ctx, "errors"); !k.From().Equal(kept.From()) {
		t.Errorf("its start moved while missed: %s, not %s", k.From(), kept.From())
	}
	if ws, _ := s.l.ImpactWindows(ctx, at.Add(-time.Hour), at.Add(time.Hour)); len(ws) != 0 {
		t.Fatalf("windows while missed: %+v", ws)
	}
	h.mu.Lock()
	h.fail = nil
	h.mu.Unlock()
	runDue(s, 5)
	s.jobs(ctx, true)
	ws, err := s.l.ImpactWindows(ctx, at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil || len(ws) != 1 || ws[0].ID != "impact-errors-"+at.UTC().Format("20060102T1504Z") {
		t.Fatalf("caught up: %+v %v", ws, err)
	}
	rows, _ = s.l.SignalRows(ctx)
	for _, r := range rows {
		if r.Name == "errors" && !r.Missed.IsZero() {
			t.Errorf("after catching up: missed %s", r.Missed)
		}
	}
}

// A rule entry keeps its rules from the windows of signals it doesn't
// name: they neither catch them nor wait for those signals.
func TestSignalRuleEntry(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	h := shopHistory()
	at := time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)
	h.windows = map[string][]prom.Series{"http_requests_total > 1e9": {minutes(map[string]string{"service": "checkout"}, at, at.Add(10*time.Minute))}}
	s := signalServer(t, h, func(c *Config) {
		c.ImpactRules = []verdict.RuleEntry{{Rules: "^Errors$", Signals: []string{"probe"}}}
	})
	ws, err := s.l.ImpactWindows(ctx, at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil || len(ws) != 1 || len(ws[0].CaughtBy) != 0 {
		t.Fatalf("the errors window, kept from Errors: %+v %v", ws, err)
	}
	rep := reportOf(t, s)
	for _, f := range rep.Firings {
		if !slices.Equal(f.Signals, []string{"probe"}) || !f.NoRecord {
			t.Errorf("%v: tied to %v, no record %v; want the probe alone, before its start", f.Labels, f.Signals, f.NoRecord)
		}
	}
	if i := slices.IndexFunc(rep.Rules, func(r verdict.RuleResult) bool { return r.Rule == "Errors" }); i < 0 || rep.Rules[i].ImpactRule != 1 {
		t.Errorf("rules: %+v", rep.Rules)
	}
	o, err := s.options(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rs, err := s.ruleSignalsOf(ctx, o, "Errors")
	if err != nil || rs.Entry != 1 || len(rs.Against) != 1 || rs.Against[0].Signal != "probe" || !slices.Equal(rs.Waits, []string{"probe"}) ||
		rs.From == nil || !rs.From.Equal(time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("the rule's signals: %+v %v", rs, err)
	}
}

// signalWindowsHistory is shopHistory with a window of each signal over
// Errors' checkout firing, both caught.
func signalWindowsHistory() *sigHistory {
	h := shopHistory()
	at := time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)
	h.windows = map[string][]prom.Series{
		"http_requests_total > 1e9": {minutes(map[string]string{"service": "checkout"}, at, at.Add(10*time.Minute))},
		"probe_success == 0":        {minutes(map[string]string{"service": "checkout", "instance": "https://checkout.example.com"}, at, at.Add(10*time.Minute))},
	}
	return h
}

// /metrics, the summary and MCP say each signal's windows apart: each
// window's series by its signal, the windows by signal and state, from
// when each signal keeps the record; impacts.by_signal; each incident's
// signal, and a rule's signals and what its record waits for.
func TestSignalsShown(t *testing.T) {
	parallel(t)
	s := signalServer(t, signalWindowsHistory(), nil)
	h := s.Handler()
	_, body, _ := get(t, h, "/metrics")
	for _, want := range []string{
		`lapilli_incident_state{incident="impact-errors-20261102T0600Z",source="impact",signal="errors",state="caught"} 1`,
		`lapilli_incident_lead_seconds{incident="impact-probe-20261102T0600Z",source="impact",signal="probe"} `,
		`lapilli_impact_windows{signal="errors",state="caught"} 1`,
		`lapilli_impact_windows{signal="probe",state="missed"} 0`,
		`lapilli_impact_signal_start_timestamp_seconds{signal="probe"} 1794182400`,
		`lapilli_incidents{state="caught",source="impact"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics has no %s", want)
		}
	}
	var sum summary
	code, body, _ := get(t, h, "/api/v1/summary?from=2026-11-02&to=2026-11-02")
	if code != http.StatusOK || json.Unmarshal([]byte(body), &sum) != nil || len(sum.Rows) != 1 {
		t.Fatalf("summary: %d %s", code, body)
	}
	im := sum.Rows[0].Impacts
	if im.Total != 2 || im.Caught != 2 || len(im.BySignal) != 2 || im.BySignal["errors"].Caught != 1 || im.BySignal["probe"].Total != 1 || im.BySignal["probe"].MedianLead == nil {
		t.Errorf("impacts: %+v", im)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()
	cs := connect(t, ts.URL, "viewer@example.com")
	var incs incidentsOut
	if failed, text := call(t, cs, "incidents", nil, &incs); failed {
		t.Fatal(text)
	}
	signals := map[string]string{}
	for _, in := range incs.Incidents {
		signals[in.ID] = in.Signal
	}
	if signals["impact-errors-20261102T0600Z"] != "errors" || signals["impact-probe-20261102T0600Z"] != "probe" {
		t.Errorf("MCP incidents' signals: %v", signals)
	}
	var r ruleDetailOut
	if failed, text := call(t, cs, "rule", map[string]any{"name": "Errors"}, &r); failed {
		t.Fatal(text)
	}
	if r.Signals == nil || len(r.Signals.Against) != 2 || !slices.Equal(r.Signals.Waits, []string{"errors", "probe"}) || r.Signals.From == nil {
		t.Errorf("MCP rule's signals: %+v", r.Signals)
	}
	// The Settings page lists them, as serve was started with them, with
	// no form for one with no name beside them.
	code, body = askAs(t, h, http.MethodGet, "/settings", nil, "ana@example.com", "same-origin")
	if code != http.StatusOK || !strings.Contains(body, "Named impact signals") || !strings.Contains(body, "<code><bdi>probe</bdi></code>") ||
		strings.Contains(body, `action="/settings/impact"`) {
		t.Errorf("settings: %d\n%s", code, body)
	}
	if code, body := askAs(t, h, http.MethodPost, "/settings/impact", url.Values{"impact": {"up == 0"}}, "ana@example.com", "same-origin"); code == http.StatusSeeOther {
		t.Errorf("an unnamed signal saved beside named ones: %d %s", code, body)
	}
	var ms struct {
		Rows []summaryRow `json:"rows"`
	}
	if failed, text := call(t, cs, "summary", map[string]any{"from": "2026-11-02", "to": "2026-11-02"}, &ms); failed || len(ms.Rows) == 0 || len(ms.Rows[len(ms.Rows)-1].Impacts.BySignal) != 2 {
		t.Errorf("MCP summary: %s %+v", text, ms.Rows)
	}
}

// docs/numbers.md's jq, run on lapilli serve's report judged by named
// signals and a rule entry, as on lapilli verdict's: each window matched
// on its signal's labels, each firing judged by its week and the signals
// tied to it; and the summary's unjudged counted again from the report of
// its week.
func TestNumbersRecountSignalsServe(t *testing.T) {
	parallel(t)
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("no jq")
	}
	// The week before, errors hadn't started: its search firings are
	// counted, not judged; this week's are judged, by errors' start, and
	// checkout's by the windows over it.
	h := signalWindowsHistory()
	s := signalServer(t, h, func(c *Config) {
		c.ImpactRules = []verdict.RuleEntry{{Rules: "^Nothing$", Signals: []string{"probe"}}}
	})
	s.cfg.RecordByWeek = true
	dir := t.TempDir()
	fetch := func(name, path string) string {
		code, body, _ := get(t, s.Handler(), path)
		if code != 200 {
			t.Fatalf("%s: %d %s", path, code, body)
		}
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o644)
		return p
	}
	all := fetch("all.json", "/api/v1/report?from=2026-10-26T00:00:00Z&to="+now.Add(time.Hour).UTC().Format(time.RFC3339))
	week := fetch("week.json", "/api/v1/report?from=2026-11-02T00:00:00Z&to=2026-11-09T00:00:00Z")
	sum := fetch("summary.json", "/api/v1/summary?from=2026-11-02&to=2026-11-02")
	compared, unjudged := 0, false
	for _, again := range numbersRecounts(t) {
		if again.as != "counted again" || strings.Contains(again.name, "(lapilli verdict)") {
			continue
		}
		for _, has := range numbersRecounts(t) {
			if has.name != again.name {
				continue
			}
			var got, want string
			switch has.as {
			case "as the report has it":
				got, want = runJQ(t, again.prog, all), runJQ(t, has.prog, all)
			case "as the summary has it":
				got, want = runJQ(t, again.prog, week), runJQ(t, has.prog, sum)
			default:
				continue
			}
			if got != want {
				t.Errorf("%s: counted again %s, %s %s", again.name, got, has.as, want)
			}
			if again.name == "no record (lapilli serve)" && strings.Contains(want, "true") && strings.Contains(want, "false") {
				unjudged = true // some judged and some not: a test of something
			}
			compared++
		}
	}
	if compared < 14 || !unjudged {
		t.Errorf("%d numbers counted again, judged in part %v", compared, unjudged)
	}
}

// The signals' metrics docs/numbers.md lists keep their names and labels,
// as TestMetricsStable holds the rest: on lapilli serve's /metrics, with
// named signals.
func TestSignalMetricsStable(t *testing.T) {
	parallel(t)
	s := signalServer(t, signalWindowsHistory(), nil)
	_, body, _ := get(t, s.Handler(), "/metrics")
	doc, err := os.ReadFile("../../docs/numbers.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		name   string
		labels []string
	}{
		{"lapilli_impact_windows", []string{"signal", "state"}},
		{"lapilli_impact_signal_start_timestamp_seconds", []string{"signal"}},
		{"lapilli_incident_state", []string{"incident", "source", "signal", "state"}},
		{"lapilli_incident_lead_seconds", []string{"incident", "source", "signal"}},
	} {
		re := regexp.MustCompile(`(?m)^` + m.name + `\{([^}]*)\} `)
		found := false
		for _, x := range re.FindAllStringSubmatch(body, -1) {
			var names []string
			for _, l := range regexp.MustCompile(`([a-z_]+)="`).FindAllStringSubmatch(x[1], -1) {
				names = append(names, l[1])
			}
			if slices.Equal(names, m.labels) {
				found = true
			}
		}
		if !found {
			t.Errorf("/metrics: no %s with %v", m.name, m.labels)
		}
		if !strings.Contains(string(doc), "| `"+m.name+"` |") {
			t.Errorf("docs/numbers.md doesn't list %s", m.name)
		}
	}
}

// What a backtest is judged against, with named signals, is every
// signal's criterion held, the rule entries and the match in effect: a
// change to any of them holds the proposals it could change.
func TestSignalsHeld(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s := signalServer(t, shopHistory(), nil)
	a, err := s.impactHeld(ctx)
	if err != nil || !strings.HasPrefix(a, "signals ") {
		t.Fatalf("%q %v", a, err)
	}
	s.cfg.ImpactRules = []verdict.RuleEntry{{Rules: "^Errors$", Signals: []string{"probe"}}}
	b, _ := s.impactHeld(ctx)
	s.cfg.ImpactRules = nil
	s.cfg.Signals[1].Match = []string{"service", "team"}
	c, _ := s.impactHeld(ctx)
	if a == b || a == c || b == c {
		t.Errorf("held alike: %q %q %q", a, b, c)
	}
}

// A signal taken away is turned off: its windows stay, as incidents, with
// its name; it keeps no record, and no firing waits for it.
func TestSignalRemoved(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s := signalServer(t, signalWindowsHistory(), nil)
	s.cfg.Signals = s.cfg.Signals[:1] // started again without the probe
	runDue(s, 1)
	s.jobs(ctx, true)
	rows, err := s.l.SignalRows(ctx)
	if err != nil || slices.ContainsFunc(rows, func(r ledger.SignalRow) bool { return r.Name == "probe" }) {
		t.Fatalf("rows: %+v %v", rows, err)
	}
	ws, err := s.l.ImpactWindows(ctx, now.Add(-72*time.Hour), now)
	if err != nil || !slices.ContainsFunc(ws, func(w ledger.Incident) bool { return w.ID == "impact-probe-20261102T0600Z" && w.Signal == "probe" }) {
		t.Errorf("the probe's window is gone: %+v %v", ws, err)
	}
	for _, f := range reportOf(t, s).Firings {
		if slices.Contains(f.Signals, "probe") {
			t.Errorf("%v still waits for the probe", f.Labels)
		}
	}
}

// A signal serve was started with that the ledger doesn't keep yet (a
// replica that follows a leader started without it, a rolling update that
// adds it, or before the leader's first round) has no bit: every page
// that judges reads it as tied to no alert, and says so, rather than
// failing.
func TestSignalWithNoRowYet(t *testing.T) {
	parallel(t)
	s := signalServer(t, signalWindowsHistory(), nil)
	s.cfg.Signals = append(s.cfg.Signals, ledger.SignalDef{Name: "latency", Expr: "latency_seconds > 1", For: 5 * time.Minute, Record: true})
	s.sigs.Lock()
	s.sigs.sig = nil
	s.sigs.Unlock()
	for _, p := range []string{"/api/v1/report?from=2026-11-02T00:00:00Z&to=2026-11-03T12:00:00Z", "/api/v1/summary", "/rules/Errors", "/", "/metrics", "/incidents"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: panic: %v", p, r)
				}
			}()
			if code, body := askAs(t, s.Handler(), http.MethodGet, p, nil, "ana@example.com", "same-origin"); code != http.StatusOK {
				t.Errorf("%s: %d %.300s", p, code, body)
			}
		}()
	}
	for _, f := range reportOf(t, s).Firings {
		if slices.Contains(f.Signals, "latency") {
			t.Errorf("%v tied to a signal the ledger doesn't keep yet", f.Labels)
		}
	}
}

// Started with named signals, the pages are served before the leader's
// first round keeps them in the ledger.
func TestSignalPagesBeforeFirstSync(t *testing.T) {
	parallel(t)
	dir := t.TempDir()
	rulesDir := filepath.Join(dir, "rules")
	os.MkdirAll(rulesDir, 0o755)
	os.WriteFile(filepath.Join(rulesDir, "shop.yaml"), []byte(ruleFile), 0o644)
	cfg := Config{
		RulesPath: rulesDir,
		Options:   verdict.Options{Lead: 15 * time.Minute, RefireWithin: time.Hour, UnknownLen: time.Hour, Match: []string{"service"}},
		Step:      time.Minute, Loc: time.UTC, Ledger: filepath.Join(dir, "ledger"), LedgerURLFile: ledgertest.File(t),
		Every: time.Hour, Settle: time.Hour, Backfill: 3 * 24 * time.Hour, Watch: time.Minute,
		Digest: Weekly{time.Monday, 9, 0}, Timeout: time.Minute, Parallel: 4, Listen: "127.0.0.1:0", Version: "test",
		AllowedHosts: []string{"example.com"},
		Signals:      []ledger.SignalDef{{Name: "errors", Expr: "x > 1", For: 5 * time.Minute, Record: true}},
	}
	s, err := New(context.Background(), cfg, shopHistory(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.ready.Store(true)
	for _, p := range []string{"/", "/api/v1/summary", "/rules", "/incidents", "/metrics"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: panic: %v", p, r)
				}
			}()
			h := s.Handler()
			if p == "/metrics" {
				h = s.MetricsHandler()
			}
			if code, _, _ := get(t, h, p); code != http.StatusOK {
				t.Errorf("%s: %d", p, code)
			}
		}()
	}
}

// reportSearch are a report's firings of search.
func reportSearch(rep verdict.Report) []verdict.Judged {
	var out []verdict.Judged
	for _, f := range rep.Firings {
		if f.Labels["service"] == "search" {
			out = append(out, f)
		}
	}
	return out
}

// What a signal has seen begins again when its expression changes (a read
// again): the alerts tied to it by what it saw before are tied again by
// what it sees now, and their firings judged again, as one started with
// the new expression ties them.
func TestSignalExprChangedTiesAgain(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	narrow := `http_requests_total{service="checkout"}`
	hist := func() *sigHistory {
		h := shopHistory()
		h.counted[narrow] = []map[string]string{{"service": "checkout"}}
		return h
	}
	s := signalServer(t, hist(), nil)
	if b := reportSearch(reportOf(t, s)); len(b) == 0 || !slices.Equal(b[0].Signals, []string{"errors"}) || b[0].NoRecord {
		t.Fatalf("before: %+v", b)
	}
	s.cfg.Signals[0].Expr = narrow + " > 1e9" // started again with it
	for k := 1; k <= 3; k++ {
		runDue(s, k)
		s.jobs(ctx, true)
	}
	got := reportSearch(reportOf(t, s))
	for _, f := range got {
		if slices.Contains(f.Signals, "errors") && !f.NoRecord {
			t.Errorf("a search firing is judged by errors, which no longer reads search: tied to %v", f.Signals)
		}
	}
	fresh := signalServer(t, hist(), func(c *Config) { c.Signals[0].Expr = narrow + " > 1e9" })
	want := reportSearch(reportOf(t, fresh))
	if len(got) != len(want) {
		t.Fatalf("%d search firings, %d started with it", len(got), len(want))
	}
	for i := range got {
		if got[i].NoRecord != want[i].NoRecord || !slices.Equal(got[i].Signals, want[i].Signals) {
			t.Errorf("%s: no record %v, tied %v; started with it: %v, %v", got[i].Start.Format(time.RFC3339), got[i].NoRecord, got[i].Signals, want[i].NoRecord, want[i].Signals)
		}
	}
}

// The match in effect changed (the Settings page, or --match between
// restarts): what the signals that follow it have seen begins again, every
// alert is tied again on it, and every firing judged again, as one started
// on it ties and judges them.
func TestSignalMatchChangedTiesAgain(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	t.Run("following signals", func(t *testing.T) {
		s := signalServer(t, shopHistory(), func(c *Config) { c.Signals[1].Match = nil })
		if err := s.l.SetSettings(ctx, map[string]string{"match": "team"}, "ana@example.com"); err != nil {
			t.Fatal(err)
		}
		s.askRejudge(ctx)
		for k := 1; k <= 2; k++ {
			runDue(s, k)
			s.jobs(ctx, true)
		}
		o, _ := s.options(ctx)
		want := o.Signals.Names(o.Signals.Ties("Errors", map[string]string{"service": "search"}, o.Match))
		for _, f := range reportSearch(reportOf(t, s)) {
			if !slices.Equal(f.Signals, want) {
				t.Errorf("a search firing is tied to %v as the ledger keeps it, %v on the match in effect %v", f.Signals, want, o.Match)
			}
		}
	})
	t.Run("started on another", func(t *testing.T) {
		hist := func() *sigHistory {
			h := shopHistory()
			h.counted = map[string][]map[string]string{"http_requests_total": {{"service": "checkout"}}, "probe_success": {{"service": "checkout"}}}
			return h
		}
		s := signalServer(t, hist(), func(c *Config) { c.Options.Match = []string{"team"} })
		code, body := askAs(t, s.Handler(), http.MethodPost, "/settings", url.Values{"match": {"service"}}, "ana@example.com", "same-origin")
		if code != http.StatusSeeOther {
			t.Fatalf("settings: %d %s", code, body)
		}
		for k := 1; k <= 3; k++ {
			runDue(s, k)
			s.jobs(ctx, true)
		}
		for _, f := range reportSearch(reportOf(t, s)) {
			if !f.NoRecord || !slices.Equal(f.Signals, []string{"errors", "probe"}) {
				t.Errorf("%s: no record %v, tied %v; one started on service: no record, tied to errors, probe", f.Start.Format(time.RFC3339), f.NoRecord, f.Signals)
			}
		}
	})
}

// A change to the signals pends every firing to be judged again in the
// write that keeps it, so a restart, or another replica taking the lead,
// judges them; and a bit a tally still carries isn't given to a signal
// added in place of the one that had it.
func TestSignalsChangeRejudgeDurable(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	h := signalWindowsHistory()
	s := signalServer(t, h, nil)
	h.mu.Lock()
	h.fail = map[string]bool{"latency_seconds > 1": true}
	h.mu.Unlock()
	// Started again without the probe, with latency.
	s.cfg.Signals = []ledger.SignalDef{s.cfg.Signals[0], {Name: "latency", Expr: "latency_seconds > 1", For: 10 * time.Minute, Record: true}}
	if _, err := s.syncSignals(ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.l.SignalRows(ctx)
	for _, r := range rows {
		if r.Name == "latency" && r.Bit == 1 {
			t.Errorf("latency was given the probe's bit while tallies still carry it")
		}
	}
	// The process ends here: another, or the next leader, takes it up.
	s.rejudge.Store(false)
	s.sigs.Lock()
	s.sigs.sig = nil
	s.sigs.Unlock()
	s.jobs(ctx, false)
	o, _ := s.options(ctx)
	w := ledger.Week(s.l.Day(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)))
	tallied, _ := s.l.WeeksBy(ctx, w-7, w)
	for _, r := range tallied {
		if names := o.Signals.Names(o.Signals.FromBits(r.Signals)); slices.Contains(names, "latency") || r.Signals&2 != 0 {
			t.Errorf("a week's tally is still keyed by the probe's bit, or read as latency's: %b %v", r.Signals, names)
		}
	}
}

// With named signals, a backtest's no record and a proposal's hold know
// them: a week the signals judge has a record to a backtest, and a week
// only a named signal's window keeps doesn't hold a proposal backtested
// just now with a record.
func TestSignalsBacktestRecord(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	from, to := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	// No windows: the week of 2 November is judged by errors' start alone.
	s := signalServer(t, shopHistory(), nil)
	o, err := s.options(ctx)
	if err != nil {
		t.Fatal(err)
	}
	judged := 0
	for _, f := range reportOf(t, s).Firings {
		if !f.NoRecord && !f.White {
			judged++
		}
	}
	if kept, err := s.l.RecordKept(ctx, from, to, o); err != nil || judged == 0 || !kept {
		t.Errorf("the report judges %d of the week's firings; a backtest's record: %v %v", judged, kept, err)
	}
	// A week only a named signal's window keeps.
	s2 := signalServer(t, signalWindowsHistory(), func(c *Config) { c.Signals[0].Record = false })
	o2, _ := s2.options(ctx)
	kept2, err := s2.l.RecordKept(ctx, from, to, o2)
	if err != nil || !kept2 {
		t.Fatalf("a week the probe's window keeps: %v %v", kept2, err)
	}
	five, no := 5*time.Minute, !kept2
	ev, _ := json.Marshal(evidence{From: "person", Edit: &rules.Edit{Name: "Errors", For: &five}, Semantics: semantics, Version: "test",
		Backtest: &backtest{From: from, To: to, Before: side{Firings: 7}, After: side{Firings: 2}, NoRecord: &no}})
	if _, err := s2.l.Propose(ctx, ledger.Proposal{Rule: "Errors", Kind: ledger.KindFor, Path: ledger.PathGit, MadeBy: "bo",
		Before: "Errors ()\nfor: 2m\n", After: "Errors ()\nfor: 5m\n", Evidence: ev}); err != nil {
		t.Fatal(err)
	}
	ps, err := s2.l.Proposals(ctx, ledger.ProposalQuery{States: []string{ledger.StateProposed}})
	if err != nil || len(ps) != 1 {
		t.Fatal(ps, err)
	}
	ls, err := s2.linesNow(ctx, ps)
	if err != nil {
		t.Fatal(err)
	}
	if ls[0].StaleWhy != nil && ls[0].StaleWhy.Format == staleUnrecorded {
		t.Errorf("a proposal backtested just now, with a record by a named signal's window, is held: %s", ls[0].StaleWhy.Format)
	}
}

// With named signals, the report, /metrics and the summary say no record
// alike: none of the period's, or the row's, firings judged.
func TestSignalsNoRecordAlike(t *testing.T) {
	parallel(t)
	s := signalServer(t, shopHistory(), func(c *Config) {
		c.ImpactRules = []verdict.RuleEntry{{Rules: "^Errors$", Signals: []string{"probe"}}}
	})
	rep := reportOf(t, s)
	judged := 0
	for _, f := range rep.Firings {
		if !f.NoRecord {
			judged++
		}
	}
	var sum summary
	_, body, _ := get(t, s.Handler(), "/api/v1/summary?from=2026-11-02&to=2026-11-02")
	if err := json.Unmarshal([]byte(body), &sum); err != nil || len(sum.Rows) != 1 {
		t.Fatal(body)
	}
	if judged != 0 || !rep.NoRecord || !sum.Rows[0].NoRecord {
		t.Errorf("%d of %d firings judged; the report's no record %v, the summary's %v", judged, len(rep.Firings), rep.NoRecord, sum.Rows[0].NoRecord)
	}
	// verdict.Judge, as lapilli verdict and a backtest judge, says so too.
	o, _ := s.options(context.Background())
	o.From, o.To = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	var fs []firing.Firing
	for _, f := range rep.Firings {
		fs = append(fs, f.Firing)
	}
	if r := verdict.Judge(nil, fs, nil, o); !r.NoRecord {
		t.Errorf("verdict.Judge: no record %v, %d unjudged of %d", r.NoRecord, r.Unjudged, len(r.Firings))
	}
}

// With named signals, a white stone is judged whatever its signals, as
// the report counts it (verdict.RuleResult.Judged): of a rule's week with
// no record by its signals, the judged firings, which say whether it is
// noisy, are its white ones.
func TestSignalsJudgedHoldsWhite(t *testing.T) {
	parallel(t)
	mid := func(w int64) time.Time { return time.Unix(w*86400, 0).UTC() }
	w := ledger.Week(20393)                                                                 // a Monday's day
	sig := verdict.NewSignals([]verdict.Signal{{Name: "probe", Record: true, Bit: 0}}, nil) // no start: no record by reading
	tl := &tallies{weeks: []int64{w}, by: map[int64]map[int64]ledger.Tally{w: {}}, rules: map[int64]ledger.RuleRow{1: {ID: 1, Rule: ledger.Rule{Name: "Errors"}}},
		o: verdict.Options{Signals: sig, RecordUnit: func(at time.Time) (time.Time, time.Time) { return mid(w), mid(w + 7) }}, mid: mid}
	part := ledger.Tally{At: w, Rule: 1, Firings: 5, White: 2, Signals: 1}
	tl.parts = []ledger.Tally{part}
	tl.by[w][1] = part
	tl.record(nil)
	tl.judge()
	if j, u := tl.judged(w, 1), tl.unjudged(w, 1); j.Firings != 2 || j.White != 2 || u.Firings != 3 || u.White != 0 {
		t.Errorf("judged %+v, unjudged %+v; want the 2 white judged, the 3 black not", j, u)
	}
	rr := verdict.RuleResult{Firings: 5, White: 2, Unjudged: 3}
	if j := tl.judged(w, 1); j.Firings != rr.Judged() {
		t.Errorf("judged %d, the report's %d", j.Firings, rr.Judged())
	}
}

// With named signals, /settings.yaml doesn't give an impact the Settings
// page kept for one with no name: committed to the chart's values, it
// would take the place of the impact mapping, and every named signal with
// it.
func TestSignalsSettingsYAML(t *testing.T) {
	parallel(t)
	s := signalServer(t, shopHistory(), nil)
	if err := s.l.SetSettings(context.Background(), map[string]string{"impact": "errors > 5", "impact-for": "10m"}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	_, body := askAs(t, s.Handler(), http.MethodGet, "/settings.yaml", nil, "ana@example.com", "same-origin")
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "impact:") || strings.Contains(l, "--impact-for") {
			t.Errorf("/settings.yaml gives %q beside named signals:\n%s", l, body)
		}
	}
}

// A signal that hangs runs out its own share of --timeout, not the
// others': each signal is asked each round, the slow one alone missed,
// from the round it first hangs, first in turn.
func TestSlowSignalStarvesNone(t *testing.T) {
	// Beside the others: what it waits on is the hung signal's share, and
	// the probe it holds to it answers at once.
	parallel(t)
	ctx := context.Background()
	h := shopHistory()
	s := signalServer(t, h, func(c *Config) { c.Timeout = 2 * time.Second })
	// The shares are counted on a clock that moves only as the hung signal
	// runs out its own: how fast the machine answers the probe, which a
	// share's 5% can't hold, isn't in them. The hung read's context still
	// ends by the real time, of the share it was given (told to the half
	// second: the shares here are whole seconds of the clock's).
	var mu sync.Mutex
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.turns.clock = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	s.h = &slowly{sigHistory: h, slow: map[string]time.Duration{"http_requests_total > 1e9": 0},
		ranOut: func(share time.Duration) {
			mu.Lock()
			clock = clock.Add(share.Round(500 * time.Millisecond))
			mu.Unlock()
		}}
	for k := 1; k <= 3; k++ {
		h.mu.Lock()
		h.asked = nil
		h.mu.Unlock()
		runDue(s, k)
		s.jobs(ctx, true)
		h.mu.Lock()
		probeRead := slices.Contains(h.asked, "probe_success == 0")
		h.mu.Unlock()
		rows, _ := s.l.SignalRows(ctx)
		for _, r := range rows {
			if r.Name == "probe" && (!probeRead || !r.Missed.IsZero()) {
				t.Errorf("round %d: the probe asked %v, missed since %s", k, probeRead, r.Missed)
			}
			if r.Name == "errors" && r.Missed.IsZero() {
				t.Errorf("round %d: the slow signal isn't missed", k)
			}
		}
	}
}

// The windows a signal keeps in a week are bounded beside those kept
// before its read alone: those a run reads again, over the hour it
// replays, aren't counted twice.
func TestSignalWeekBoundCountsOnce(t *testing.T) {
	ctx := context.Background()
	was := maxSignalWeek
	maxSignalWeek = 30
	t.Cleanup(func() { maxSignalWeek = was })
	h := shopHistory()
	var ws []prom.Series
	for i := range 24 { // one every five minutes, over the two hours to now
		at := now.Add(-2*time.Hour + time.Duration(i)*5*time.Minute)
		ws = append(ws, minutes(map[string]string{"service": fmt.Sprintf("svc-%02d", i)}, at, at.Add(2*time.Minute)))
	}
	h.windows = map[string][]prom.Series{"probe_success == 0": ws}
	s := signalServer(t, h, nil)
	for k := 1; k <= 2; k++ {
		runDue(s, k)
		s.jobs(ctx, true)
	}
	rows, _ := s.l.SignalRows(ctx)
	for _, r := range rows {
		if r.Name == "probe" && !r.Missed.IsZero() {
			t.Errorf("24 windows, read again by each run's hour, counted as more than %d: missed since %s", maxSignalWeek, r.Missed)
		}
	}
	if got, _ := s.l.SignalWindowsBetween(ctx, "probe", now.Add(-3*time.Hour), now.Add(4*time.Hour)); got != 24 {
		t.Errorf("%d windows kept", got)
	}
}

// The match in effect takes any name, as it always has; what isn't a
// label's name, as PromQL names one, is never put in a query a named
// signal asks (source.SeenFrom).
func TestSignalsMatchNeverInPromQL(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	h := shopHistory()
	s := signalServer(t, h, nil)
	inj := "a)\t(up)\tor\tvector(1)\tor\tcount\tby\t(b"
	for _, v := range []string{"service.name", inj} {
		if code, body := askAs(t, s.Handler(), http.MethodPost, "/settings", url.Values{"match": {v}}, "ana@example.com", "same-origin"); code != http.StatusSeeOther {
			t.Errorf("match %q refused: %d %.200s", v, code, body)
		}
	}
	if o, _ := s.options(ctx); len(o.Match) != 1 || o.Match[0] != inj {
		t.Fatalf("the match kept: %q", o.Match)
	}
	h.mu.Lock()
	h.asked = nil
	h.mu.Unlock()
	runDue(s, 1)
	s.jobs(ctx, true)
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, q := range h.asked {
		if strings.Contains(q, "vector(1)") || strings.Contains(q, "(up)") {
			t.Errorf("asked %q", q)
		}
	}
}

// A rule no entry names that hasn't fired: what its record waits for comes
// with its alerts, as MCP says it, not that no signal keeps the record.
func TestSignalRuleNeverFired(t *testing.T) {
	parallel(t)
	s := signalServer(t, shopHistory(), nil)
	o, _ := s.options(context.Background())
	rs, err := s.ruleSignalsOf(context.Background(), o, "Quiet")
	if err != nil || rs == nil || !rs.NoAlert || len(rs.Waits) != 0 || len(rs.Against) != 2 {
		t.Errorf("a rule with no alert: %+v %v", rs, err)
	}
	if rs, _ := s.ruleSignalsOf(context.Background(), o, "Errors"); rs.NoAlert {
		t.Error("a rule that fired has no alert")
	}
}

// A window's labels, from what Prometheus answered, are kept as an
// incident's are: 4 KB at most in all, each value cut as an alert's.
func TestSignalWindowLabelsBounded(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	h := shopHistory()
	at := time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)
	big := strings.Repeat("x", 100<<10)
	h.windows = map[string][]prom.Series{"probe_success == 0": {minutes(map[string]string{"service": "checkout", "instance": big}, at, at.Add(10*time.Minute))}}
	s := signalServer(t, h, nil)
	ws, err := s.l.ImpactWindows(ctx, at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil || len(ws) != 1 {
		t.Fatalf("%+v %v", ws, err)
	}
	b, _ := json.Marshal(ws[0].Labels)
	if len(b) > 4<<10 || ws[0].Labels["service"][0] != "checkout" {
		t.Errorf("window %s kept with %d bytes of labels: %.200s", ws[0].ID, len(b), b)
	}
}

// With named signals, the try of the criterion with no name is refused,
// and asks nothing: there is no one criterion with no name beside them
// (each signal's form has a try of its own: TestSignalsTry).
func TestSignalsTryRefused(t *testing.T) {
	parallel(t)
	h := shopHistory()
	s := signalServer(t, h, nil)
	h.mu.Lock()
	h.asked = nil
	h.mu.Unlock()
	code, _ := askAs(t, s.Handler(), http.MethodGet, "/settings?impact-try=1&impact="+url.QueryEscape("tried_expr > 1"), nil, "ana@example.com", "same-origin")
	h.mu.Lock()
	defer h.mu.Unlock()
	if code == http.StatusOK || slices.ContainsFunc(h.asked, func(q string) bool { return strings.Contains(q, "tried_expr") }) {
		t.Errorf("a try beside named signals: %d, asked %q", code, h.asked)
	}
	_, body := askAs(t, s.Handler(), http.MethodGet, "/settings", nil, "ana@example.com", "same-origin")
	if !strings.Contains(body, "yes, since ") {
		t.Errorf("the signals' list doesn't say from when each keeps the record")
	}
}

// Eight signals, one replaced (the probe renamed on the command line): its
// bit, which alerts and tallies still carry, is the only one free, and the
// new signal has it, its own; no two share one, and each firing is tied as
// it was, by the probe's new name.
func TestSignalReplaceOneOfEight(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s := signalServer(t, signalWindowsHistory(), func(c *Config) {
		for i := 2; i < 8; i++ {
			c.Signals = append(c.Signals, ledger.SignalDef{Name: fmt.Sprintf("s%d", i), Expr: fmt.Sprintf("m%d > 1", i), For: 5 * time.Minute, Record: true})
		}
	})
	ties := func() map[string][]string {
		out := map[string][]string{}
		for _, f := range reportOf(t, s).Firings {
			out[f.Labels["service"]] = f.Signals
		}
		return out
	}
	before := ties()
	s.cfg.Signals[1].Name = "probe2"
	s.sigs.Lock()
	s.sigs.sig = nil
	s.sigs.Unlock()
	for k := 1; k <= 2; k++ {
		runDue(s, k)
		s.jobs(ctx, true)
	}
	rows, err := s.l.SignalRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bits := map[int]string{}
	names := map[string]bool{}
	for _, r := range rows {
		if r.Name == "" {
			continue
		}
		names[r.Name] = true
		if other, ok := bits[r.Bit]; ok {
			t.Errorf("%s and %s share bit %d", other, r.Name, r.Bit)
		}
		bits[r.Bit] = r.Name
	}
	if !names["probe2"] || names["probe"] || len(names) != 8 {
		t.Errorf("the signals kept: %v", names)
	}
	after := ties()
	for svc, was := range before {
		want := slices.Clone(was)
		for i, n := range want {
			if n == "probe" {
				want[i] = "probe2"
			}
		}
		slices.Sort(want)
		got := slices.Clone(after[svc])
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s's firing tied to %v; before, %v", svc, after[svc], was)
		}
	}
}

// The unnamed signal's windows keep their labels as every release before
// kept them: a window of a ledger an earlier release wrote, with a value
// longer than an alert's is kept, found again (a read again of a new
// expression) is known by them, and keeps its key and what was decided.
func TestUnnamedWindowLongLabelKeepsKey(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	h := shopHistory()
	at := now.Add(-5 * time.Hour)
	long := strings.Repeat("x", 300)
	w := []prom.Series{minutes(map[string]string{"service": "checkout", "instance": long}, at, at.Add(10*time.Minute))}
	h.windows = map[string][]prom.Series{"impact > 1": w, "(impact > 1)": w}
	s := signalServer(t, h, func(c *Config) { c.Signals, c.Impact = nil, "impact > 1" })
	window := func() ledger.Incident {
		t.Helper()
		ws, err := s.l.ImpactWindows(ctx, at.Add(-time.Hour), at.Add(time.Hour))
		if err != nil || len(ws) != 1 {
			t.Fatalf("%+v %v", ws, err)
		}
		return ws[0]
	}
	first := window()
	o, _ := s.options(ctx)
	if _, err := s.l.Decide(ctx, first.ID, "dismissed", "", "ana@example.com", o); err != nil {
		t.Fatal(err)
	}
	// As an earlier release kept it, labels and all.
	b, _ := json.Marshal(map[string][]string{"instance": {long}, "service": {"checkout"}})
	ledgerExec(t, s, "UPDATE incidents SET labels = ? WHERE key = ?", string(b), first.ID)
	if code, body := askAs(t, s.Handler(), http.MethodPost, "/settings/impact", url.Values{"impact": {"(impact > 1)"}}, "ana@example.com", "same-origin"); code != http.StatusSeeOther {
		t.Fatalf("%d %.300s", code, body)
	}
	runDue(s, 1)
	s.jobs(ctx, true)
	again := window()
	if again.ID != first.ID || again.State != "dismissed" || again.Labels["instance"][0] != long {
		t.Errorf("found again: %s %q, instance of %d bytes; was %s", again.ID, again.State, len(again.Labels["instance"][0]), first.ID)
	}
}

// lookStarts notes where each look at what a signal reads starts.
type lookStarts struct {
	*sigHistory
	starts map[string]time.Time
}

func (h *lookStarts) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if strings.HasPrefix(expr, "count by (") {
		h.mu.Lock()
		if h.starts == nil {
			h.starts = map[string]time.Time{}
		}
		h.starts[expr] = start
		h.mu.Unlock()
	}
	return h.sigHistory.QueryRange(ctx, expr, start, end, step)
}

// With the match in effect changed, what a signal reads is looked at
// again from where its reads began, its since: a value read only in the
// weeks before is learned again, not only from the next run on.
func TestSignalSeenAgainFromSince(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	h := shopHistory()
	s := signalServer(t, h, nil)
	k, err := s.l.SignalKept(ctx, "errors")
	if err != nil || k.Since.IsZero() {
		t.Fatalf("%+v %v", k, err)
	}
	w := &lookStarts{sigHistory: h}
	s.h = w
	if err := s.l.SetSettings(ctx, map[string]string{"match": "service,team"}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	runDue(s, 1)
	s.jobs(ctx, true)
	at, ok := w.starts["count by (service, team) (http_requests_total)"]
	if !ok || at.After(k.Since) {
		t.Errorf("looked at again from %s (asked %v); its reads began %s", at, ok, k.Since)
	}
}

// With named signals, a match changed on the Settings page is judged
// once: the signals' change pends every firing, judged with the new match,
// and nothing judges them all again after; a lead changed alone still is.
func TestSignalsMatchChangeJudgedOnce(t *testing.T) {
	ctx := context.Background()
	s := signalServer(t, signalWindowsHistory(), nil)
	n := 0
	was := judgeAll
	judgeAll = func(l *ledger.Ledger, ctx context.Context, o verdict.Options, done func(float64)) error {
		n++
		return was(l, ctx, o, done)
	}
	t.Cleanup(func() { judgeAll = was })
	if code, _ := askAs(t, s.Handler(), http.MethodPost, "/settings", url.Values{"match": {"service,team"}}, "ana@example.com", "same-origin"); code != http.StatusSeeOther {
		t.Fatal(code)
	}
	s.jobs(ctx, false)
	if n != 0 {
		t.Errorf("every firing judged %d more times", n)
	}
	if p, err := s.l.Meta(ctx, "pending"); err != nil || (p != "" && p != "[]" && p != "null") {
		t.Errorf("left to judge: %q %v", p, err)
	}
	if code, _ := askAs(t, s.Handler(), http.MethodPost, "/settings", url.Values{"lead": {"20m"}, "match": {"service,team"}}, "ana@example.com", "same-origin"); code != http.StatusSeeOther {
		t.Fatal(code)
	}
	s.jobs(ctx, false)
	if n != 1 {
		t.Errorf("a lead changed: judged all %d times", n)
	}
}

// slowly answers the expressions in slow after their time, or, at 0,
// never: each waits until its context is done.
type slowly struct {
	*sigHistory
	slow map[string]time.Duration
	// ranOut, if set, is told the share a read that never answers had, as
	// its context ends.
	ranOut func(share time.Duration)
}

func (h *slowly) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if d, ok := h.slow[expr]; ok {
		var after <-chan time.Time
		dl, _ := ctx.Deadline()
		share := time.Until(dl)
		if d > 0 {
			after = time.After(d)
		}
		select {
		case <-after:
		case <-ctx.Done():
			if d == 0 && h.ranOut != nil {
				h.ranOut(share)
			}
			return nil, ctx.Err()
		}
	}
	return h.sigHistory.QueryRange(ctx, expr, start, end, step)
}

// round is how a round went for each signal: whether it was read
// (answered, whole or a piece), and whether it is behind after it (the
// ledger's missed_at).
type round struct{ read, behind map[string]bool }

// readRounds runs rounds of s and says, round by round, which signals
// were read and which are behind, of names.
func readRounds(t *testing.T, s *Server, from, rounds int) []round {
	t.Helper()
	var out []round
	for k := from; k < from+rounds; k++ {
		runDue(s, k)
		s.jobs(context.Background(), true)
		rows, err := s.l.SignalRows(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		r := round{read: map[string]bool{}, behind: map[string]bool{}}
		for _, x := range rows {
			if x.Name != "" {
				r.behind[x.Name] = !x.Missed.IsZero()
			}
		}
		s.sources.Lock()
		for name, x := range s.sources.signals {
			r.read[name] = x.Read.Equal(s.now())
		}
		s.sources.Unlock()
		out = append(out, r)
	}
	return out
}

// A signal that fits in --timeout alone, but not in its share, is read: a
// heavy one beside a quick one each round, the quick one before it handing
// on what it leaves; two heavy ones, too heavy for both to fit, in turn,
// neither unread two rounds running; a heavy one beside one that hangs,
// on its turn, which the one that hangs doesn't keep, until it has caught
// up; a heavy one beside one that fits in its share, on its turn, for
// which what it took before is kept.
func TestHeavySignalsTakeTurns(t *testing.T) {
	const timeout, heavy, hangs = time.Second, 600 * time.Millisecond, time.Duration(0)
	for _, c := range []struct {
		name          string
		errors, probe time.Duration
		both          bool // time for both each round
	}{{"one heavy", heavy, -1, true}, {"two heavy", heavy, heavy, false}, {"one heavy, one hanging", hangs, heavy, false},
		// The one that took 450ms fits in its share beside the heavy one's
		// turn, and would leave it 550ms of the 600ms it needs, but for
		// what is kept for its turn.
		{"one heavy, one in its share", heavy, 450 * time.Millisecond, false}} {
		t.Run(c.name, func(t *testing.T) {
			// Beside each other, each with a server of its own, and alone
			// of the package's other tests: each waits out its own --timeout,
			// using little of the machine while it does.
			parallel(t)
			h := shopHistory()
			s := signalServer(t, h, func(c *Config) { c.Timeout = timeout })
			sl := &slowly{sigHistory: h, slow: map[string]time.Duration{}}
			for e, d := range map[string]time.Duration{"http_requests_total > 1e9": c.errors, "probe_success == 0": c.probe} {
				if d >= 0 {
					sl.slow[e] = d
				}
			}
			s.h = sl
			rounds := readRounds(t, s, 1, 6)
			names := []string{"errors", "probe"}
			if c.errors == hangs {
				names = names[1:]
			}
			for k := 1; k < len(rounds); k++ {
				for _, name := range names {
					if c.both && !rounds[k].read[name] {
						t.Errorf("round %d: %s unread, with time for both", k+1, name)
					}
					if k > 1 && !rounds[k].read[name] && !rounds[k-1].read[name] {
						t.Errorf("rounds %d and %d: %s unread in both", k, k+1, name)
					}
				}
			}
			if last := rounds[len(rounds)-1]; c.errors == hangs && last.behind["probe"] || c.both && (last.behind["errors"] || last.behind["probe"]) {
				t.Errorf("behind at the end: %v", last.behind)
			}
		})
	}
}

// costly answers each expression in cost after its cost an hour of what
// it is asked over, and those in hang never: each waits until its context
// is done.
type costly struct {
	*sigHistory
	cost map[string]time.Duration
	hang map[string]bool
	mu   sync.Mutex
	most time.Duration // the most hours one of cost was asked over
	hung int           // how often one of hang was asked
}

func (h *costly) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	var wait <-chan time.Time
	if c, ok := h.cost[expr]; ok {
		h.mu.Lock()
		h.most = max(h.most, end.Sub(start))
		h.mu.Unlock()
		wait = time.After(time.Duration(float64(c) * end.Sub(start).Hours()))
	}
	if h.hang[expr] {
		h.mu.Lock()
		h.hung++
		h.mu.Unlock()
		wait = nil
	}
	if wait != nil || h.hang[expr] {
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return h.sigHistory.QueryRange(ctx, expr, start, end, step)
}

// A signal whose run read fits in --timeout alone is read, and catches
// up, beside any number of signals that hang: its read after a miss a
// piece at a time, no more than a run's at once; it, having answered its
// last turn, before those that ran out on theirs; and those asked again
// only after a round, then two, four, up to a day of rounds, their turns
// keeping nothing back from it until then. So too after a restart, when
// this process knows nothing of how the last turns went, but the ledger
// keeps where each was missed.
func TestHeavyCatchesUpBesideHung(t *testing.T) {
	const timeout, perHour, rounds = 300 * time.Millisecond, 70 * time.Millisecond, 30 // a run's 3h read: 210ms
	for _, hung := range []int{2, 3} {
		t.Run(fmt.Sprintf("%d hung", hung), func(t *testing.T) {
			parallel(t) // beside each other alone, as TestHeavySignalsTakeTurns's
			h := shopHistory()
			s := signalServer(t, h, func(c *Config) {
				c.Timeout = timeout
				for i := 2; i <= hung; i++ {
					c.Signals = append(c.Signals, ledger.SignalDef{Name: fmt.Sprintf("s%d", i), Expr: fmt.Sprintf("m%d > 1", i), For: 5 * time.Minute, Record: true})
				}
			})
			c := &costly{sigHistory: h, cost: map[string]time.Duration{"http_requests_total > 1e9": perHour}, hang: map[string]bool{"probe_success == 0": true}}
			for i := 2; i <= hung; i++ {
				c.hang[fmt.Sprintf("m%d > 1", i)] = true
			}
			s.h = c
			got := readRounds(t, s, 1, rounds/2)
			c.mu.Lock()
			c.hung = 0 // those that hang, asked in the rounds after they were found to
			c.mu.Unlock()
			got = append(got, readRounds(t, s, rounds/2+1, rounds-rounds/2)...)
			unread := 0
			for _, r := range got[rounds/2:] {
				if !r.read["errors"] {
					unread++
				}
			}
			if last := got[len(got)-1]; last.behind["errors"] || unread > 2 {
				t.Errorf("errors behind after %d rounds %v, unread in %d of the last %d", rounds, last.behind["errors"], unread, rounds-rounds/2)
			}
			c.mu.Lock()
			if c.most > 3*time.Hour || c.hung > 3*hung {
				t.Errorf("errors asked over %s at once (a run's: 3h); those that hang asked %d times in the last %d rounds", c.most, c.hung, rounds-rounds/2)
			}
			c.mu.Unlock()
			// Started again: nothing known of the turns; the ledger keeps
			// errors missed six hours back, the others still hang.
			s.turns = turns{}
			ledgerExec(t, s, "UPDATE impact_signals SET missed_at = ? WHERE name = ?", now.Add(time.Duration(rounds-6)*time.Hour).Unix(), "errors")
			got = readRounds(t, s, rounds+1, 16)
			if last := got[len(got)-1]; last.behind["errors"] {
				t.Errorf("errors behind 16 rounds after a restart")
			}
		})
	}
}

// The look at what each signal reads, and the look for where each one's
// data begins, are each in turn too: one that hangs leaves the others
// their looks, round after round.
func TestSignalLooksTakeTurns(t *testing.T) {
	ctx := context.Background()
	h := shopHistory()
	s := signalServer(t, h, func(c *Config) { c.Timeout = 300 * time.Millisecond })
	was, _ := s.l.SeenOf(ctx, s.cfg.Options.Match)
	s.h = &slowly{sigHistory: h, slow: map[string]time.Duration{"count by (service) (http_requests_total)": 0}}
	for k := 1; k <= 3; k++ {
		runDue(s, k)
		s.jobs(ctx, true)
		st, _ := s.l.SeenOf(ctx, s.cfg.Options.Match)
		cursor, _ := s.l.Cursor(ctx)
		if !st["probe"].To.Equal(cursor) || !st["probe"].To.After(was["probe"].To) {
			t.Errorf("round %d: the probe looked at to %s, the runs at %s", k, st["probe"].To, cursor)
		}
	}
}

// The look for where each signal's data begins is in turn: with the
// errors' look hanging, from the round it first does, first in turn, the
// probe's is made, and its data found.
func TestSignalDataLooksTakeTurns(t *testing.T) {
	ctx := context.Background()
	h := shopHistory()
	// No data yet of either: each look answers, and is made again.
	none := []prom.Series{}
	h.windows = map[string][]prom.Series{"count(http_requests_total)": none, "count(probe_success)": none}
	s := signalServer(t, h, func(c *Config) { c.Timeout = 300 * time.Millisecond })
	if k, _ := s.l.SignalKept(ctx, "probe"); k.Data == nil || k.Data.Complete() {
		t.Fatalf("the probe's data, with none yet: %+v", k.Data)
	}
	h.mu.Lock()
	h.windows = nil
	h.mu.Unlock()
	s.h = &slowly{sigHistory: h, slow: map[string]time.Duration{"count(http_requests_total)": 0}}
	runDue(s, 1)
	s.jobs(ctx, true)
	if k, _ := s.l.SignalKept(ctx, "probe"); k.Data == nil || !k.Data.Complete() {
		t.Errorf("the probe's data not found beside the errors' hanging look: %+v", k.Data)
	}
}

// Signals the ledger doesn't keep (keeping them failed: here, nine) aren't
// read, and no run makes their rows, nor gives them a bit another has.
func TestSignalsNotKeptNotRead(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	h := shopHistory()
	s := signalServer(t, h, nil)
	for i := 2; i < 9; i++ {
		s.cfg.Signals = append(s.cfg.Signals, ledger.SignalDef{Name: fmt.Sprintf("s%d", i), Expr: fmt.Sprintf("m%d > 1", i), For: 5 * time.Minute, Record: true})
	}
	h.mu.Lock()
	h.asked = nil
	h.mu.Unlock()
	runDue(s, 1)
	s.jobs(ctx, true)
	h.mu.Lock()
	for _, q := range h.asked {
		if strings.HasPrefix(q, "m") {
			t.Errorf("asked %q of a signal the ledger doesn't keep", q)
		}
	}
	h.mu.Unlock()
	rows, _ := s.l.SignalRows(ctx)
	for _, r := range rows {
		if strings.HasPrefix(r.Name, "s") {
			t.Errorf("a row made for %s, at bit %d", r.Name, r.Bit)
		}
	}
}

// wide answers each range of wideExpr with per series, a sample a step,
// labels new to each answer; its answers held to most samples a range,
// as a prom.Client's are (MaxSamples).
type wide struct {
	*sigHistory
	per, most, asked int
	// then, if not 0, is per after the first answer; answered, the samples
	// answered in all.
	then, answered int
}

const wideExpr = "pods_down > 1"

func (h *wide) MaxSamples() int { return h.most }

func (h *wide) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if expr != wideExpr {
		return h.sigHistory.QueryRange(ctx, expr, start, end, step)
	}
	h.asked++
	per := h.per
	if h.then > 0 && h.asked > 1 {
		per = h.then
	}
	var out []prom.Series
	for i := range per {
		se := prom.Series{Labels: map[string]string{"pod": fmt.Sprintf("p%d-%d", h.asked, i)}}
		for at := start; !at.After(end); at = at.Add(step) {
			se.Samples = append(se.Samples, prom.Sample{T: at, V: 1})
		}
		h.answered += len(se.Samples)
		out = append(out, se)
	}
	return out, nil
}

// A read catching up a day behind, a piece at a time, holds no more in
// all than one range may (--max-samples): pieces alike, it doesn't ask the
// one past it, and catches up over turns, each going on a little before
// where the last ended (ledger.PieceResume); a piece larger than the one
// before, past it, is let go.
func TestPiecesHeldToMaxSamples(t *testing.T) {
	parallel(t)
	for _, c := range []struct {
		name      string
		per, then int
	}{{"pieces alike", 50, 0}, {"a piece larger than the one before", 20, 400}} {
		t.Run(c.name, func(t *testing.T) {
			h := shopHistory()
			s := signalServer(t, h, nil)
			w := &wide{sigHistory: h, per: c.per, then: c.then, most: 20000} // a piece of 3h: 181 samples a series
			s.h = w
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			to := now
			from := to.Add(-24 * time.Hour)
			turns := 0
			for ; from.Before(to) && turns < 40; turns++ {
				w.answered = 0
				series, end, _, err := s.readPieces(ctx, wideExpr, from, to, 2*time.Hour, true)
				if err != nil {
					t.Fatal(err)
				}
				n := 0
				for _, se := range series {
					n += len(se.Samples)
				}
				if n > w.most || !end.After(from) || c.then == 0 && w.answered > w.most {
					t.Fatalf("turn %d from %s: %d samples held, %d answered, to %s", turns+1, from, n, w.answered, end)
				}
				if c.then > 0 {
					return // the larger pieces' own turns are another's
				}
				if !end.Before(to) {
					from = end
					break
				}
				from = ledger.PieceResume(end, 5*time.Minute, s.cfg.Settle, s.cfg.Step)
			}
			if from.Before(to) {
				t.Errorf("not caught up after %d turns: at %s", turns, from)
			}
		})
	}
}

// oddHangs hangs its signals' reads of hang on odd rounds, and answers at
// once on even ones; cost as costly's.
type oddHangs struct {
	*sigHistory
	cost  map[string]time.Duration
	hang  string
	mu    sync.Mutex
	round int
}

func (h *oddHangs) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	h.mu.Lock()
	odd := h.round%2 == 1
	h.mu.Unlock()
	var wait <-chan time.Time
	switch {
	case expr == h.hang && odd:
		<-ctx.Done()
		return nil, ctx.Err()
	case h.cost[expr] > 0:
		wait = time.After(time.Duration(float64(h.cost[expr]) * end.Sub(start).Hours()))
	}
	if wait != nil {
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return h.sigHistory.QueryRange(ctx, expr, start, end, step)
}

// A quick signal that hangs on odd rounds, beside a heavy one that holds
// the turn on even ones: what the heavy one leaves goes to it, though it
// took its share before, and it is read on the rounds it answers.
func TestQuickSignalReadWithWhatIsLeft(t *testing.T) {
	h := shopHistory()
	s := signalServer(t, h, func(c *Config) { c.Timeout = 600 * time.Millisecond })
	f := &oddHangs{sigHistory: h, cost: map[string]time.Duration{"http_requests_total > 1e9": 140 * time.Millisecond}, hang: "probe_success == 0"}
	s.h = f
	var got []round
	for k := 1; k <= 16; k++ {
		f.mu.Lock()
		f.round = k
		f.mu.Unlock()
		got = append(got, readRounds(t, s, k, 1)...)
	}
	for k := 4; k <= len(got); k += 2 {
		if !got[k-1].read["probe"] {
			t.Errorf("round %d, which the probe answers at once in: unread", k)
		}
	}
}

// The pages by signal (docs/design/signals.md, "Where it shows"): the
// Incidents page picks a signal's windows, says each line's signal and
// the other signals' windows on with it; a window's page says its signal,
// what caught the others at the time, and dismisses them together; a
// confirmation's says it is matched as its window was; a rule's page what
// it is judged against and what its record waits for.
func TestSignalPages(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s := signalServer(t, signalWindowsHistory(), func(c *Config) { c.Editors = []string{"bo@example.com"} })
	h := s.Handler()
	page := func(path string) string {
		t.Helper()
		code, body := askAs(t, h, http.MethodGet, path, nil, "ana@example.com", "same-origin")
		if code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, code, body)
		}
		return body
	}
	body := page("/incidents?source=impact")
	for _, want := range []string{`<a href="/incidents?signal=probe&amp;source=impact&amp;weeks=4"><bdi>probe</bdi></a>`, "found by <bdi>errors</bdi>",
		"with <bdi>probe</bdi>, <bdi>2026-11-02 06:00</bdi> to <bdi>2026-11-02 06:11</bdi>", "user impact (probe)"} {
		if !strings.Contains(body, want) {
			t.Errorf("/incidents?source=impact has no %s\n%s", want, body)
		}
	}
	body = page("/incidents?source=impact&signal=probe")
	if !strings.Contains(body, "impact-probe-20261102T0600Z") || strings.Contains(body, `>impact-errors-20261102T0600Z<`) {
		t.Errorf("the probe's windows alone:\n%s", body)
	}
	body = page("/incidents/impact-probe-20261102T0600Z")
	for _, want := range []string{`<h2 id="signal">Impact signal</h2>`, "Found by <bdi>probe</bdi>: a window once a series returns something for <bdi>2m</bdi>, matched on <bdi>service</bdi>.",
		`impact-errors-20261102T0600Z</a> <bdi>errors</bdi>`, "caught by Errors", `name="together" value="1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the probe's window's page has no %s", want)
		}
	}
	body = page("/rules/Errors")
	if !strings.Contains(body, "Judged against every impact signal&#39;s windows: errors (service), probe (service).") ||
		!strings.Contains(body, "No record by reading them before 2026-11-09: it waits for errors, probe.") {
		t.Errorf("the rule's page:\n%s", body)
	}
	// Dismissed together: the errors' window too, each noted on its own.
	code, body := askAs(t, h, http.MethodPost, "/incidents/impact-probe-20261102T0600Z/decide", url.Values{"state": {"dismissed"}, "together": {"1"}}, "bo@example.com", "same-origin")
	if code != http.StatusSeeOther {
		t.Fatalf("dismissed together: %d %s", code, body)
	}
	for _, id := range []string{"impact-probe-20261102T0600Z", "impact-errors-20261102T0600Z"} {
		if in, err := s.l.IncidentByID(ctx, id); err != nil || in.State != "dismissed" {
			t.Errorf("%s: %q %v", id, in.State, err)
		}
	}
	// Undone and confirmed, the probe's window is written down with its
	// signal: its page says how it is matched.
	o, _ := s.options(ctx)
	if _, err := s.l.Decide(ctx, "impact-probe-20261102T0600Z", "", "", "bo@example.com", o); err != nil {
		t.Fatal(err)
	}
	made, err := s.l.Decide(ctx, "impact-probe-20261102T0600Z", "confirmed", "", "bo@example.com", o)
	if err != nil {
		t.Fatal(err)
	}
	body = page("/incidents/" + made)
	if !strings.Contains(body, "Written down by confirming a window of <bdi>probe</bdi>: matched on <bdi>service</bdi>") ||
		!strings.Contains(body, "matched as its window was; with other labels, on service, as any written down") {
		t.Errorf("the confirmation's page:\n%s", body)
	}
}

// A rule an entry names says so on its page, and what its record waits
// for: the entry's signals alone.
func TestSignalRulePageEntry(t *testing.T) {
	parallel(t)
	s := signalServer(t, shopHistory(), func(c *Config) {
		c.ImpactRules = []verdict.RuleEntry{{Rules: "^Errors$", Signals: []string{"probe"}, Match: []string{}}}
	})
	_, body := askAs(t, s.Handler(), http.MethodGet, "/rules/Errors", nil, "ana@example.com", "same-origin")
	if !strings.Contains(body, "Rule entry #1: judged against the windows of probe (–) alone.") || !strings.Contains(body, "it waits for probe.") {
		t.Errorf("the rule's page:\n%s", body)
	}
	_, body = askAs(t, s.Handler(), http.MethodGet, "/incidents", nil, "ana@example.com", "same-origin")
	if strings.Contains(body, "Signal</") {
		t.Error("signal tabs with no source picked")
	}
}

// windowsWith, by sorted spans, names and counts the same windows as each
// line against each window would: the first withMost to start, the rest
// counted.
func TestWindowsWithAsEachAgainstEach(t *testing.T) {
	parallel(t)
	rnd := rand.New(rand.NewPCG(1, 2))
	base := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	at := func() time.Time { return base.Add(time.Duration(rnd.IntN(600)) * time.Minute) }
	signals := []string{"errors", "probe", "latency", ""}
	for round := range 200 {
		var ws []ledger.Incident
		for i := range rnd.IntN(40) {
			a := at()
			b := a.Add(time.Duration(rnd.IntN(120)) * time.Minute)
			if rnd.IntN(30) == 0 {
				b = a.Add(-time.Minute) // one that ends before it starts
			}
			ws = append(ws, ledger.Incident{Incident: incident.Incident{ID: fmt.Sprintf("w%02d", i), Signal: signals[rnd.IntN(len(signals))], Start: a, End: b}, Origin: "impact"})
		}
		var lines, naive []incidentLine
		for i := range rnd.IntN(20) {
			a := at()
			in := ledger.Incident{Incident: incident.Incident{ID: fmt.Sprintf("l%02d", i), Signal: signals[rnd.IntN(3)], Start: a, End: a.Add(time.Duration(rnd.IntN(90)) * time.Minute)}, Origin: "impact"}
			lines, naive = append(lines, incidentLine{Incident: in}), append(naive, incidentLine{Incident: in})
		}
		windowsWith(lines, slices.Clone(ws))
		sorted := slices.Clone(ws)
		slices.SortFunc(sorted, byStart)
		for i := range naive {
			in := &naive[i]
			for _, w := range sorted {
				if w.Signal == "" || w.Signal == in.Signal || w.Start.After(in.End) || w.End.Before(in.Start) {
					continue
				}
				if len(in.With) < withMost {
					in.With = append(in.With, withWindow{ID: w.ID, Signal: w.Signal, Start: w.Start, End: w.End})
				} else {
					in.WithMore++
				}
			}
		}
		if !reflect.DeepEqual(lines, naive) {
			t.Fatalf("round %d:\n got %+v\nwant %+v", round, lines, naive)
		}
	}
}

// A signal picked on the Incidents page stays picked by its other tabs:
// the period, the week's chip, and how they were caught.
func TestSignalPickedKeptByTabs(t *testing.T) {
	parallel(t)
	s := signalServer(t, signalWindowsHistory(), nil)
	_, body := askAs(t, s.Handler(), http.MethodGet, "/incidents?source=impact&signal=probe&weeks=4", nil, "ana@example.com", "same-origin")
	links := regexp.MustCompile(`href="(/incidents\?[^"]*)"`).FindAllStringSubmatch(body, -1)
	kept := 0
	for _, m := range links {
		q, _ := url.ParseQuery(strings.SplitN(strings.ReplaceAll(m[1], "&amp;", "&"), "?", 2)[1])
		if q.Get("source") != "impact" {
			continue // the Of tabs: another source, no signal
		}
		if _, picks := q["signal"]; !picks && q.Get("state") == "" && q.Get("weeks") == "4" {
			continue // the Signal tab's all
		}
		if q.Get("signal") != "probe" && !(q.Get("signal") != "" && q.Get("weeks") == "4") {
			t.Errorf("%s drops the signal picked", m[1])
		}
		kept++
	}
	if kept < 3 {
		t.Errorf("%d links keep the signal: %q", kept, links)
	}
}

// A rule that hasn't fired says what its record waits for comes with its
// alerts, not that no signal keeps it; a window's page says the other
// signals' windows caught, or not, as states; a confirmation with no
// match in effect says so, not "on ,".
func TestSignalPagesSayIt(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s := signalServer(t, signalWindowsHistory(), func(c *Config) {
		c.Editors = []string{"bo@example.com"}
		os.WriteFile(filepath.Join(c.RulesPath, "quiet.yaml"), []byte("groups:\n- name: quiet\n  rules:\n  - alert: Quiet\n    expr: never_seen > 1\n"), 0o644)
	})
	h := s.Handler()
	_, body := askAs(t, h, http.MethodGet, "/rules/Quiet", nil, "ana@example.com", "same-origin")
	if !strings.Contains(body, "No alert yet: which of them") || strings.Contains(body, "None of them keeps the record") {
		t.Errorf("a rule with no alert: %s", between(body, "Judged against", "</p>"))
	}
	others := func() string {
		t.Helper()
		_, body := askAs(t, h, http.MethodGet, "/incidents/impact-probe-20261102T0600Z", nil, "ana@example.com", "same-origin")
		return between(body, "Other signals&#39; windows at the time", "</ul>")
	}
	// As the Incidents page draws each: caught ok, caught only by noisy
	// rules warn, covered by one written down with no colour.
	if o := others(); !strings.Contains(o, `<span class="status-ok"><span aria-hidden="true">○</span> caught by Errors</span>`) {
		t.Errorf("the other window caught: %s", o)
	}
	if err := s.l.SetSettings(ctx, map[string]string{"noisy-firings": "1", "noisy-precision": "1"}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	if o := others(); !strings.Contains(o, `<span class="status-warn"><span aria-hidden="true">◐</span> caught by Errors, noisy</span>`) {
		t.Errorf("the other window caught by a noisy rule alone: %s", o)
	}
	if err := s.l.SetSettings(ctx, map[string]string{"noisy-firings": "", "noisy-precision": ""}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	o, _ := s.options(ctx)
	at := time.Date(2026, 11, 2, 5, 55, 0, 0, time.UTC)
	if _, err := s.l.Put(ctx, incident.Incident{ID: "INC-9", Title: "checkout down", Start: at, End: at.Add(30 * time.Minute), Labels: incident.Labels{"service": {"checkout"}}}, "ana@example.com", o); err != nil {
		t.Fatal(err)
	}
	if o := others(); !strings.Contains(o, "covered by one written down") || strings.Contains(o, "status-") {
		t.Errorf("the other window covered by one written down: %s", o)
	}
	made, err := s.l.Decide(ctx, "impact-probe-20261102T0600Z", "confirmed", "", "bo@example.com", o)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.l.SetSettings(ctx, map[string]string{"match": "none"}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	_, body = askAs(t, h, http.MethodGet, "/incidents/"+url.PathEscape(made), nil, "bo@example.com", "same-origin")
	if !strings.Contains(body, "matched as its window was; with other labels, on no label, as any written down") {
		t.Errorf("the confirmation with no match in effect: %s", between(body, `name="labels"`, "</label>"))
	}
}

// between is what of s is after from, up to the first to after it.
func between(s, from, to string) string {
	_, after, _ := strings.Cut(s, from)
	x, _, _ := strings.Cut(after, to)
	return x
}
