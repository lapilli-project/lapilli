package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// A signal not in effect judges no one (docs/design/judging.md, item 5):
// taken away on the Settings page, dropped by a start without it, or set
// aside at a start past eight, its windows keep no rule's record, and only
// the rules with no entry catch them, on the labels they were found with;
// confirmed, a window is written down, and keeps everyone's record. On the
// record, serve says what lapilli verdict, which never read the signal,
// says.

// twoRules are Errors and Other, which fire alike: an entry ties Errors to
// latency alone, which keeps no record; Other has none.
const twoRules = `groups:
  - name: shop
    rules:
      - alert: Errors
        expr: errors > 5
        for: 2m
        labels: {severity: page}
      - alert: Other
        expr: errors > 1
        for: 2m
`

var (
	goneLatency = ledger.SignalDef{Name: "latency", Expr: "latency_seconds > 1", For: 10 * time.Minute, Record: false}
	goneProbe   = ledger.SignalDef{Name: "probe", Expr: "probe_success == 0", For: 2 * time.Minute, Match: []string{"service"}, Record: true}
	goneErrors  = ledger.SignalDef{Name: "errors", Expr: "http_requests_total > 1e9", For: 5 * time.Minute, Record: true}
)

// goneServer reads errors, probe and latency, a window of errors and one
// of probe over the checkout firings of Errors and Other, Monday at 06:00,
// with entries (none for nil).
func goneServer(t *testing.T, entries []verdict.RuleEntry) *Server {
	t.Helper()
	h := signalWindowsHistory()
	h.history.begins["latency_seconds"] = []time.Time{}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(twoRules), 0o644)
	return signalServer(t, h, func(c *Config) {
		c.RulesPath = dir
		c.Signals = []ledger.SignalDef{goneErrors, goneProbe, goneLatency}
		c.ImpactRules = entries
	})
}

// caughtBy are the rules that caught in, by name.
func caughtBy(in ledger.Incident) []string {
	var out []string
	for _, c := range in.CaughtBy {
		out = append(out, c.Rule)
	}
	slices.Sort(out)
	return out
}

// latencyOnly ties Errors to latency alone.
var latencyOnly = []verdict.RuleEntry{{Rules: "^Errors$", Signals: []string{"latency"}}}

// probeWindowOf is probe's window, as the ledger keeps it.
func probeWindowOf(t *testing.T, s *Server) ledger.Incident {
	t.Helper()
	ws, err := s.l.ImpactWindows(context.Background(), now.Add(-30*24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(ws, func(w ledger.Incident) bool { return w.Signal == "probe" })
	if i < 0 {
		t.Fatalf("no window of probe: %+v", ws)
	}
	in, err := s.l.IncidentByID(context.Background(), ws[i].ID)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// restartWithout is serve started again without the signals named.
func restartWithout(s *Server, names ...string) {
	s.cfg.Signals = slices.DeleteFunc(slices.Clone(s.cfg.Signals), func(d ledger.SignalDef) bool { return slices.Contains(names, d.Name) })
	runDue(s, 1)
	s.jobs(context.Background(), true)
}

// agreesWithVerdict holds the record serve keeps to what lapilli verdict
// judges with the same signals and entries: firing by firing, and rule by
// rule, judged or not, verdict.Judge given every incident but the windows
// of signals not in effect, which lapilli verdict never reads. Their
// stones differ only where serve holds such a window: a firing of a rule
// with no entry that caught one is white in serve, and so judged, and may
// be black in lapilli verdict, and unjudged; its rule is left out of the
// rules compared.
func agreesWithVerdict(t *testing.T, s *Server, rep verdict.Report) {
	t.Helper()
	ctx := context.Background()
	o, err := s.options(ctx)
	if err != nil {
		t.Fatal(err)
	}
	o.From, o.To = rep.From, rep.To
	around, err := s.l.RecordIncidents(ctx, verdict.RecordFrom(rep.From.Add(-7*24*time.Hour), rep.To), rep.To.Add(7*24*time.Hour), o.UnknownLen)
	if err != nil {
		t.Fatal(err)
	}
	var incs []incident.Incident
	gone := map[string]bool{}
	for _, in := range around {
		if o.NotInEffect(in.Incident) {
			gone[in.ID] = true
		} else {
			incs = append(incs, in.Incident)
		}
	}
	var fs []firing.Firing
	for _, f := range rep.Firings {
		fs = append(fs, f.Firing)
	}
	fresh := verdict.Judge(nil, fs, incs, o)
	if len(fresh.Firings) != len(rep.Firings) {
		t.Fatalf("verdict judges %d firings, serve %d", len(fresh.Firings), len(rep.Firings))
	}
	key := func(f verdict.Judged) string {
		return f.Rule + " " + f.Labels["service"] + " " + f.Start.UTC().String()
	}
	byKey := map[string]verdict.Judged{}
	for _, f := range fresh.Firings {
		byKey[key(f)] = f
	}
	differ := map[string]bool{}
	for _, f := range rep.Firings {
		v := byKey[key(f)]
		if f.White != v.White {
			if !f.White || !slices.ContainsFunc(f.Incidents, func(id string) bool { return gone[id] }) || o.Signals.Entry(f.Rule) != nil {
				t.Errorf("%s: white %v, lapilli verdict's %v, caught %v", key(f), f.White, v.White, f.Incidents)
			}
			differ[f.Rule] = true
			continue
		}
		if v.NoRecord != f.NoRecord {
			t.Errorf("%s: no record %v, lapilli verdict's %v", key(f), f.NoRecord, v.NoRecord)
		}
	}
	for _, r := range rep.Rules {
		if differ[r.Rule] {
			continue
		}
		i := slices.IndexFunc(fresh.Rules, func(x verdict.RuleResult) bool { return x.Rule == r.Rule })
		if i < 0 || fresh.Rules[i].NoRecord != r.NoRecord || fresh.Rules[i].Unjudged != r.Unjudged {
			t.Errorf("%s: no record %v, %d unjudged; lapilli verdict's %+v", r.Rule, r.NoRecord, r.Unjudged, fresh.Rules)
		}
	}
}

// probeGone holds what a probe no longer in effect judges: its window
// caught by Other, which has no entry, and not Errors, whose entry names
// latency alone; Errors' firings unjudged, as no window of latency, and no
// incident written down, keeps their record, and lapilli verdict says so;
// Other's checkout firing white.
func probeGone(t *testing.T, s *Server) {
	t.Helper()
	o, err := s.options(context.Background())
	if err != nil || o.InEffect("probe") {
		t.Fatalf("probe in effect: %v", err)
	}
	if got := caughtBy(probeWindowOf(t, s)); !slices.Equal(got, []string{"Other"}) {
		t.Errorf("probe's window, caught by %v: want Other alone", got)
	}
	rep := reportOf(t, s)
	for _, f := range rep.Firings {
		switch {
		case f.Rule == "Errors" && (f.White || !f.NoRecord):
			t.Errorf("Errors at %v: white %v, no record %v; want unjudged", f.Start, f.White, f.NoRecord)
		case f.Rule == "Other" && f.Labels["service"] == "checkout" && !f.White:
			t.Errorf("Other's checkout firing black")
		}
	}
	agreesWithVerdict(t, s, rep)
}

// Started again without probe: its window keeps no rule's record, and is
// caught by the rule with no entry alone. The window's page, MCP and the
// Incident sources panel say so.
func TestSignalRemovedWindowsCaughtByRulesWithNoEntry(t *testing.T) {
	parallel(t)
	s := goneServer(t, latencyOnly)
	// In effect, its window is Other's alone too: Errors' entry keeps it
	// from probe's windows, as it does once probe is gone.
	if got := caughtBy(probeWindowOf(t, s)); !slices.Equal(got, []string{"Other"}) {
		t.Fatalf("probe in effect, its window caught by %v", got)
	}
	restartWithout(s, "probe")
	probeGone(t, s)
	w := probeWindowOf(t, s)
	_, page := askAs(t, s.Handler(), http.MethodGet, "/incidents/"+w.ID, nil, "viewer@example.com", "same-origin")
	if !strings.Contains(page, "a signal no longer read") || !strings.Contains(page, "is caught by the rules with no rule entry alone") {
		t.Errorf("the window's page doesn't say it:\n%s", page)
	}
	if !strings.Contains(page, "Rule entries keep their rules from its windows") || !strings.Contains(page, "#1 ^Errors$") {
		t.Error("the window's page doesn't say the entry keeps Errors from it")
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	var incs incidentsOut
	if failed, text := call(t, connect(t, ts.URL, "viewer@example.com"), "incidents", nil, &incs); failed {
		t.Fatal(text)
	}
	for _, in := range incs.Incidents {
		if in.NotInEffect != (in.Signal == "probe") {
			t.Errorf("MCP: %s of %q, signal not in effect %v", in.ID, in.Signal, in.NotInEffect)
		}
	}
	_, settings := askAs(t, s.Handler(), http.MethodGet, "/settings", nil, "ana@example.com", "same-origin")
	if !strings.Contains(settings, "Impact signals no longer read") || !strings.Contains(settings, "<code><bdi>probe</bdi></code>") {
		t.Error("the Incident sources panel has no row of probe's windows")
	}
}

// Taken away on the Settings page: the same.
func TestSignalTakenAwayOnPageJudgesNoOne(t *testing.T) {
	parallel(t)
	s := goneServer(t, latencyOnly)
	if code, body := adminPost(t, s, "/settings/signals", url.Values{"was": {"probe"}, "remove": {"1"}}, "ana@example.com"); code != http.StatusSeeOther {
		t.Fatalf("taken away: %d %s", code, body)
	}
	s.jobs(context.Background(), true)
	probeGone(t, s)
}

// Set aside at a start past eight, the UI's newest: the same.
func TestSignalSetAsideJudgesNoOne(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s := goneServer(t, latencyOnly)
	values := map[string]string{}
	for _, name := range []string{"a1", "a2", "a3", "a4", "a5"} {
		values[signalKey+name] = uiSignal{Expr: name + "_total > 0", For: "5m", Record: true}.text()
	}
	if err := s.l.SetSettings(ctx, values, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	// Probe the UI's, as serve was started with it, and the newest.
	if err := s.l.SetSettings(ctx, map[string]string{signalKey + "probe": uiSignalOf(goneProbe).text()}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	restartWithout(s, "probe")
	if n := inEffect(t, s); len(n.Defs) != verdict.MaxSignals || len(n.Aside) != 0 {
		t.Fatalf("defs %d, aside %v", len(n.Defs), n.Aside)
	}
	if o, _ := s.options(ctx); !o.InEffect("probe") {
		t.Fatal("probe, the UI's, not in effect")
	}
	s.cfg.Signals = append(s.cfg.Signals, ledger.SignalDef{Name: "extra", Expr: "extra_total > 0", For: 5 * time.Minute, Record: true})
	runDue(s, 2)
	s.jobs(ctx, true)
	if n := inEffect(t, s); !slices.Equal(n.Aside, []string{"probe"}) {
		t.Fatalf("aside %v", n.Aside)
	}
	probeGone(t, s)
}

// Confirmed, by an editor or an admin, probe's window is written down: it
// keeps everyone's record, Errors' firings of its week judged, black, as
// Errors catches it no more than it caught the window; Other catches it.
func TestSignalGoneWindowConfirmed(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s := goneServer(t, latencyOnly)
	restartWithout(s, "probe")
	w := probeWindowOf(t, s)
	if code, _ := askAs(t, s.Handler(), http.MethodPost, "/incidents/"+w.ID+"/decide", url.Values{"state": {"confirmed"}}, "viewer@example.com", "same-origin"); code != http.StatusForbidden {
		t.Errorf("a viewer confirmed it: %d", code)
	}
	if code, body := adminPost(t, s, "/incidents/"+w.ID+"/decide", url.Values{"state": {"confirmed"}}, "ana@example.com"); code != http.StatusSeeOther {
		t.Fatalf("confirmed: %d %s", code, body)
	}
	incs, err := s.l.RecordIncidents(ctx, w.Start, w.End, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(incs, func(in ledger.Incident) bool { return in.Origin == "ui" && in.Signal == "probe" })
	if i < 0 {
		t.Fatalf("no incident written down of probe's window: %+v", incs)
	}
	if got := caughtBy(incs[i]); !slices.Equal(got, []string{"Other"}) {
		t.Errorf("the confirmation, caught by %v: want Other alone, as its window was", got)
	}
	rep := reportOf(t, s)
	for _, f := range rep.Firings {
		if f.Rule == "Errors" && (f.White || f.NoRecord) {
			t.Errorf("Errors at %v: white %v, no record %v; want judged, black", f.Start, f.White, f.NoRecord)
		}
	}
	agreesWithVerdict(t, s, rep)
	_, page := askAs(t, s.Handler(), http.MethodGet, "/incidents/"+incs[i].ID, nil, "viewer@example.com", "same-origin")
	if !strings.Contains(page, "by the rules with no rule entry alone") {
		t.Error("the confirmation's page doesn't say how it is caught")
	}
}

// An entry kept in the UI naming probe alone, probe dropped by a start:
// the entry names no signal in effect, so Errors is judged by incidents
// written down alone, and catches no window, probe's neither. The rule's
// page and MCP say so. An incident written down judges it.
func TestEntryNamingNoSignalInEffectServe(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s := goneServer(t, nil)
	es, _ := json.Marshal([]verdict.RuleEntry{{Rules: "^Errors$", Signals: []string{"probe"}}})
	if err := s.l.SetSettings(ctx, map[string]string{rulesKey: string(es)}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	if got := caughtBy(probeWindowOf(t, s)); !slices.Equal(got, []string{"Errors", "Other"}) {
		t.Fatalf("probe in effect, its window caught by %v", got)
	}
	restartWithout(s, "probe")
	if got := caughtBy(probeWindowOf(t, s)); !slices.Equal(got, []string{"Other"}) {
		t.Errorf("probe's window, caught by %v: want Other alone", got)
	}
	rep := reportOf(t, s)
	for _, f := range rep.Firings {
		if f.Rule == "Errors" && (f.White || !f.NoRecord) {
			t.Errorf("Errors at %v: white %v, no record %v; want unjudged", f.Start, f.White, f.NoRecord)
		}
	}
	agreesWithVerdict(t, s, rep)
	_, page := askAs(t, s.Handler(), http.MethodGet, "/rules/Errors", nil, "viewer@example.com", "same-origin")
	if !strings.Contains(page, "Rule entry #1 names <bdi>probe</bdi>, not read: judged by incidents written down alone.") {
		t.Errorf("the rule's page doesn't say it:\n%s", page)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	var r ruleDetailOut
	if failed, text := call(t, connect(t, ts.URL, "viewer@example.com"), "rule", map[string]any{"name": "Errors"}, &r); failed {
		t.Fatal(text)
	}
	if r.Signals == nil || r.Signals.Entry != 1 || len(r.Signals.Against) != 0 || !slices.Equal(r.Signals.NotRead, []string{"probe"}) {
		t.Errorf("MCP rule's signals: %+v", r.Signals)
	}
	o, err := s.options(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.Put(ctx, incident.Incident{Title: "Cart down", Start: now.Add(-31 * time.Hour), End: now.Add(-31*time.Hour + 10*time.Minute),
		Labels: incident.Labels{"service": {"cart"}}}, "ana@example.com", o); err != nil {
		t.Fatal(err)
	}
	rep = reportOf(t, s)
	for _, f := range rep.Firings {
		if f.Rule == "Errors" && (f.White || f.NoRecord) {
			t.Errorf("after an incident written down, Errors at %v: white %v, no record %v; want judged, black", f.Start, f.White, f.NoRecord)
		}
	}
	agreesWithVerdict(t, s, rep)
}

// Started again with no named signal, beside rule entries the UI keeps:
// the entries are read for the windows of named signals, now none's, which
// keep no record, and which Errors, an entry's, doesn't catch.
func TestSignalsAllGone(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s := goneServer(t, nil)
	es, _ := json.Marshal(latencyOnly)
	if err := s.l.SetSettings(ctx, map[string]string{rulesKey: string(es)}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	restartWithout(s, "errors", "probe", "latency")
	o, err := s.options(ctx)
	if err != nil || o.Signals.Named() || o.Signals == nil || len(o.Signals.Entries) != 1 {
		t.Fatalf("options: %+v %v", o.Signals, err)
	}
	if got := caughtBy(probeWindowOf(t, s)); !slices.Equal(got, []string{"Other"}) {
		t.Errorf("probe's window, caught by %v: want Other alone", got)
	}
	rep := reportOf(t, s)
	if !rep.NoRecord {
		t.Error("the windows of signals none of which is in effect keep the record")
	}
	if kept, err := s.l.RecordKept(ctx, rep.From, rep.To, o); err != nil || kept {
		t.Errorf("the record kept %v, %v: by windows of signals not in effect", kept, err)
	}
	agreesWithVerdict(t, s, rep)
}

// docs/numbers.md's jq, run on the report of a ledger that keeps windows of
// a signal not in effect, and an entry naming it beside one in effect:
// what they catch and the record they keep counted again as the report
// has them.
func TestNumbersRecountNotInEffect(t *testing.T) {
	parallel(t)
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("no jq")
	}
	s := goneServer(t, []verdict.RuleEntry{{Rules: "^Errors$", Signals: []string{"probe", "latency"}}})
	if got := caughtBy(probeWindowOf(t, s)); !slices.Equal(got, []string{"Errors", "Other"}) {
		t.Fatalf("probe in effect, its window caught by %v", got)
	}
	restartWithout(s, "probe")
	code, body, _ := get(t, s.Handler(), "/api/v1/report?from=2026-10-26T00:00:00Z&to="+now.Add(2*time.Hour).UTC().Format(time.RFC3339))
	if code != http.StatusOK {
		t.Fatalf("report: %d %s", code, body)
	}
	all := filepath.Join(t.TempDir(), "all.json")
	os.WriteFile(all, []byte(body), 0o644)
	compared := 0
	for _, again := range numbersRecounts(t) {
		if again.as != "counted again" || strings.Contains(again.name, "(lapilli verdict)") {
			continue
		}
		for _, has := range numbersRecounts(t) {
			if has.name != again.name || has.as != "as the report has it" {
				continue
			}
			if got, want := runJQ(t, again.prog, all), runJQ(t, has.prog, all); got != want {
				t.Errorf("%s: counted again %s, as the report has it %s", again.name, got, want)
			}
			compared++
		}
	}
	if compared < 10 {
		t.Errorf("%d numbers counted again", compared)
	}
}
