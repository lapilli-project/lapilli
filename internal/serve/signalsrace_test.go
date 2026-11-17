package serve

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/ledger/ledgertest"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/verdict"
)

// Named impact signals changed on the Settings page while the leader's
// rounds work, and what the page takes held to its bounds
// (docs/design/signals.md, "Step 3, as built").

// duringHistory is a sigHistory that calls during once, the first time
// expr is asked for while armed: a request made while a round is under
// way.
type duringHistory struct {
	*sigHistory
	mu     sync.Mutex
	expr   string
	armed  bool
	during func()
}

func (h *duringHistory) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	h.mu.Lock()
	fire := h.armed && expr == h.expr
	if fire {
		h.armed = false
	}
	during := h.during
	h.mu.Unlock()
	if fire && during != nil {
		during()
	}
	return h.sigHistory.QueryRange(ctx, expr, start, end, step)
}

// arm has during called while the next round reads h's expression.
func (h *duringHistory) arm(during func()) {
	h.mu.Lock()
	h.armed, h.during = true, during
	h.mu.Unlock()
}

func (h *duringHistory) fired() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.armed
}

// duringServer is signalServer reading from h, its history the one given,
// or one with the impact signal with no name (impact, set), no named one.
func duringServer(t *testing.T, h *duringHistory, impact string) *Server {
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
	}
	if impact != "" {
		cfg.Impact, cfg.ImpactFor = impact, 5*time.Minute
	} else {
		cfg.Signals = []ledger.SignalDef{
			{Name: "errors", Expr: "http_requests_total > 1e9", For: 5 * time.Minute, Record: true},
			{Name: "probe", Expr: "probe_success == 0", For: 2 * time.Minute, Match: []string{"service"}, Record: true},
		}
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

// searchAtFive is the search firing of Errors at 05:00 on 2 November,
// during a probe window on checkout: matched on service, it catches
// nothing; by the entry ^Errors$ -> probe, on no label, it catches it.
func searchAtFive(t *testing.T, s *Server) ledger.Firing {
	t.Helper()
	at := time.Date(2026, 11, 2, 5, 0, 0, 0, time.UTC)
	fs, err := s.l.Firings(context.Background(), ledger.FiringQuery{From: at.Add(-time.Minute), To: at.Add(9 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if f.Labels["service"] == "search" {
			return f
		}
	}
	t.Fatalf("no search firing at 05:00: %+v", fs)
	return ledger.Firing{}
}

func probeAtFive() *duringHistory {
	h := shopHistory()
	at := time.Date(2026, 11, 2, 5, 0, 0, 0, time.UTC)
	h.windows = map[string][]prom.Series{
		"probe_success == 0": {minutes(map[string]string{"service": "checkout", "instance": "https://checkout.example.com"}, at, at.Add(10*time.Minute))},
	}
	return &duringHistory{sigHistory: h, expr: "probe_success == 0"}
}

const errorsOnProbe = "- rules: ^Errors$\n  signals: [probe]\n  match: []\n"

// The rule entries saved between rounds, and saved while the leader's
// round reads (here; on another replica, any time): either way, they are
// judged with, the second by the round after, the options of the one
// under way, read before, judging nothing the save pended.
func TestSignalEntriesSavedDuringRound(t *testing.T) {
	ctx := context.Background()
	for _, during := range []bool{false, true} {
		h := probeAtFive()
		s := duringServer(t, h, "")
		if f := searchAtFive(t, s); f.White {
			t.Fatalf("before the entry: white")
		}
		save := func() {
			if code, body := adminPost(t, s, "/settings/impact-rules", url.Values{"rules": {errorsOnProbe}}, "ana@example.com"); code != http.StatusSeeOther {
				t.Errorf("%d %s", code, body)
			}
		}
		if during {
			h.arm(save)
		} else {
			save()
		}
		runDue(s, 1)
		s.jobs(ctx, true)
		if !h.fired() && during {
			t.Fatal("not saved during the round")
		}
		runDue(s, 2)
		s.jobs(ctx, true)
		if f := searchAtFive(t, s); !f.White {
			t.Errorf("saved during the round %v: the search firing is still black, judged by the entries before", during)
		}
	}
}

// The first named signal saved while the leader's run reads the one with
// no name: no window of the signal with no name is kept after it was made
// the signal impact.
func TestFirstNamedSavedDuringRun(t *testing.T) {
	ctx := context.Background()
	h := &duringHistory{sigHistory: shopHistory(), expr: "always"}
	s := duringServer(t, h, "always")
	runDue(s, 1)
	s.jobs(ctx, true)
	h.arm(func() {
		if code, body := adminPost(t, s, "/settings/signals", signalPost("", "probe", "probe_success == 0", "2m", "", true), "ana@example.com"); code != http.StatusSeeOther {
			t.Errorf("%d %s", code, body)
		}
	})
	runDue(s, 2)
	s.jobs(ctx, true)
	if !h.fired() {
		t.Fatal("not saved during the run")
	}
	for k := 3; k <= 4; k++ {
		runDue(s, k)
		s.jobs(ctx, true)
	}
	ws, err := s.l.ImpactWindows(ctx, now.Add(-4*24*time.Hour), now.Add(5*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range ws {
		if w.Signal == "" {
			t.Errorf("window %s of the signal with no name after it was made the signal impact", w.ID)
		}
	}
}

// tried is a try of a signal's form, as the Settings page tries it.
func tried(t *testing.T, s *Server, v url.Values) *signalsTry {
	t.Helper()
	ctx := context.Background()
	set, err := s.l.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.planSignal(ctx, set, signalFormOf(v.Get), "save", "ana@example.com", time.Now().UTC().Truncate(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	try := &signalsTry{}
	if err := s.judgeSignalsTried(ctx, "en", "ana@example.com", try, s.signalsIn(set), p); err != nil {
		t.Fatal(err)
	}
	if try.Err != "" {
		t.Fatal(try.Err)
	}
	return try
}

// triedAsSaved tries v, saves it, lets the leader read again, and tries
// what is in effect: what that says in effect is what the first said it
// would be.
func triedAsSaved(t *testing.T, s *Server, v url.Values) {
	t.Helper()
	ctx := context.Background()
	s.jobs(ctx, false)
	s.jobs(ctx, false)
	first := tried(t, s, v)
	if code, body := adminPost(t, s, "/settings/signals", v, "ana@example.com"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	s.jobs(ctx, false)
	s.jobs(ctx, false)
	again := url.Values{}
	for k, vs := range v {
		again[k] = slices.Clone(vs)
	}
	again.Set("was", v.Get("name"))
	second := tried(t, s, again)
	for i, r := range first.Rows {
		if i < len(second.Rows) && r.With != second.Rows[i].Now {
			t.Errorf("%s: the try said %q with it; saved and read again, %q", r.Label, r.With, second.Rows[i].Now)
		}
	}
}

func TestSignalTryAsSavedAdd(t *testing.T) {
	h := signalWindowsHistory()
	at := time.Date(2026, 11, 2, 5, 0, 0, 0, time.UTC)
	h.windows["latency_p99 > 1"] = []prom.Series{minutes(map[string]string{"service": "search"}, at, at.Add(15*time.Minute))}
	triedAsSaved(t, signalServer(t, h, nil), signalPost("", "latency", "latency_p99 > 1", "10m", "service", true))
}

// The probe's match changed to none: the try says what saving it does,
// and saving it judges its windows on it.
func TestSignalTryAsSavedMatch(t *testing.T) {
	h := signalWindowsHistory()
	at := time.Date(2026, 11, 2, 5, 0, 0, 0, time.UTC)
	h.windows["probe_success == 0"] = append(h.windows["probe_success == 0"], minutes(map[string]string{"service": "checkout", "instance": "x"}, at, at.Add(10*time.Minute)))
	s := signalServer(t, h, nil)
	triedAsSaved(t, s, signalPost("probe", "probe", "probe_success == 0", "2m", "-", true))
	ws, _ := s.l.ImpactWindows(context.Background(), now.Add(-30*24*time.Hour), now)
	for _, w := range ws {
		if w.Signal == "probe" && (w.Match == nil || len(w.Match) != 0) {
			t.Errorf("window %s matched on %v, the probe's on none", w.ID, w.Match)
		}
	}
}

func TestSignalTryAsSavedFirstNamed(t *testing.T) {
	s, _ := server(t, func(c *Config) { c.UserHeader = "X-Forwarded-Email"; c.Admins = []string{"ana@example.com"} })
	raw := ""
	if err := s.look(context.Background(), &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(context.Background(), true)
	s.ready.Store(true)
	triedAsSaved(t, s, signalPost("", "probe", "impact > 1", "5m", "", true))
}

// The rule entries the page takes are held as a team's labels are, and
// what is written is what is kept: an anchor and its aliases, which would
// make a 32 KB form megabytes kept, are refused; so are a label's values
// past a team's bounds, and entries kept past maxEntriesJSON.
func TestSignalEntriesBounded(t *testing.T) {
	ctx := context.Background()
	s := signalServer(t, shopHistory(), nil)
	big := strings.Repeat("x", 4000)
	var aliases strings.Builder
	aliases.WriteString("- rules: ^Errors$\n  signals: [probe]\n  labels:\n    a: &v " + big + "\n    b: [")
	for i := 0; i < 2000; i++ {
		if i > 0 {
			aliases.WriteString(",")
		}
		aliases.WriteString("*v")
	}
	aliases.WriteString("]\n")
	for _, c := range []struct{ name, text, want string }{
		{"aliases", aliases.String(), "anchor or alias"},
		{"an anchor alone", "- rules: &r ^Errors$\n  signals: [probe]\n", "anchor or alias"},
		{"a value of 1000 bytes", "- labels:\n    service: [" + strings.Repeat("y", 1000) + "]\n  signals: [probe]\n", "values of up to 256"},
		{"65 values", "- labels:\n    service: [" + strings.TrimSuffix(strings.Repeat("v,", 65), ",") + "]\n  signals: [probe]\n", "values of up to 256"},
	} {
		if len(c.text) > maxEntriesText {
			t.Fatalf("%s: %d bytes", c.name, len(c.text))
		}
		code, body := adminPost(t, s, "/settings/impact-rules", url.Values{"rules": {c.text}}, "ana@example.com")
		if code != http.StatusBadRequest || !strings.Contains(body, c.want) {
			t.Errorf("%s: %d, want %q", c.name, code, c.want)
		}
	}
	if set, _ := s.l.Settings(ctx); set[rulesKey].Value != "" {
		t.Errorf("kept: %d bytes", len(set[rulesKey].Value))
	}
	_, api, _ := get(t, s.Handler(), "/api/v1/settings")
	if len(api) > 1<<20 {
		t.Errorf("/api/v1/settings is %d bytes", len(api))
	}
}

// A signal the rule entries name can't be taken away while they do,
// which says which; renamed, it is renamed in them, the UI's then, noted;
// and what /settings.yaml gives names no signal it doesn't declare, as
// --impact-file reads it.
func TestSignalEntriesNameNoSignalGone(t *testing.T) {
	ctx := context.Background()
	s := signalServer(t, shopHistory(), func(c *Config) {
		c.ImpactRules = []verdict.RuleEntry{{Rules: "^Errors$", Signals: []string{"probe"}}}
	})
	if code, body := adminPost(t, s, "/settings/signals", url.Values{"was": {"probe"}, "remove": {"1"}}, "ana@example.com"); code != http.StatusBadRequest || !strings.Contains(body, "rule entries 1 name the signal probe") {
		t.Errorf("taken away while an entry names it: %d", code)
	}
	if _, ok := inEffect(t, s).def("probe"); !ok {
		t.Fatal("probe taken away")
	}
	// Renamed with another expression, its entries follow too.
	if code, body := adminPost(t, s, "/settings/signals", signalPost("probe", "synthetic", "probe_success < 1", "2m", "service", true), "ana@example.com"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	n := inEffect(t, s)
	if n.EntriesUI == nil || !slices.Equal(n.Entries[0].Signals, []string{"synthetic"}) {
		t.Errorf("entries: %+v", n.Entries)
	}
	cs, _ := s.l.Changes(ctx, 10)
	if !slices.ContainsFunc(cs, func(c ledger.Change) bool {
		return c.What == "setting impact rule entries" && strings.Contains(c.After, "synthetic")
	}) {
		t.Errorf("the entries' change not noted: %+v", cs)
	}
	// The start's entries back name probe, not in effect: refused.
	if code, body := adminPost(t, s, "/settings/impact-rules", url.Values{"reset": {"1"}}, "ana@example.com"); code != http.StatusBadRequest || !strings.Contains(body, "names the signal probe") {
		t.Errorf("the start's entries back, naming probe: %d", code)
	}
	_, y, _ := get(t, s.Handler(), "/settings.yaml")
	var doc struct {
		Impact struct {
			Signals []struct{ Name string }
			Rules   []struct{ Signals []string }
		}
	}
	if err := yaml.Unmarshal([]byte(y), &doc); err != nil {
		t.Fatal(err)
	}
	for _, r := range doc.Impact.Rules {
		for _, name := range r.Signals {
			if !slices.ContainsFunc(doc.Impact.Signals, func(x struct{ Name string }) bool { return x.Name == name }) {
				t.Errorf("settings.yaml's entries name %s, which it doesn't declare:\n%s", name, y)
			}
		}
	}
}

// A name the ledger keeps the windows of, of a signal taken away, is no
// new signal's: its windows (the dismissed one, its comment) would be the
// new one's, and its read again would take their place.
func TestSignalAddOntoKeptNameRefused(t *testing.T) {
	ctx := context.Background()
	s := signalServer(t, signalWindowsHistory(), nil)
	ws, _ := s.l.ImpactWindows(ctx, now.Add(-30*24*time.Hour), now)
	var key string
	for _, w := range ws {
		if w.Signal == "probe" {
			key = w.ID
		}
	}
	o, _ := s.options(ctx)
	if _, err := s.l.Decide(ctx, key, "dismissed", "", "ana@example.com", o); err != nil {
		t.Fatal(err)
	}
	if code, _ := adminPost(t, s, "/settings/signals", url.Values{"was": {"probe"}, "remove": {"1"}}, "ana@example.com"); code != http.StatusSeeOther {
		t.Fatal(code)
	}
	if code, body := adminPost(t, s, "/settings/signals", signalPost("", "probe", "up == 0", "2m", "", true), "ana@example.com"); code != http.StatusBadRequest || !strings.Contains(body, "since taken away") {
		t.Errorf("added onto kept windows: %d", code)
	}
	// Given back as serve was started with it, it is the same signal.
	if code, body := adminPost(t, s, "/settings/signals", url.Values{"was": {"probe"}, "reset": {"1"}}, "ana@example.com"); code != http.StatusSeeOther {
		t.Errorf("given back: %d %s", code, body)
	}
	if in, err := s.l.IncidentByID(ctx, key); err != nil || in.State != "dismissed" {
		t.Errorf("the dismissed window: %+v %v", in, err)
	}
}

// A signal set aside, past the eight at most, can't be renamed over.
func TestSignalRenameOverAsideRefused(t *testing.T) {
	ctx := context.Background()
	s := signalServer(t, shopHistory(), nil)
	values := map[string]string{}
	for _, name := range []string{"u1", "u2", "u3", "u4", "u5", "u6", "u7"} {
		values[signalKey+name] = uiSignal{Expr: name + "_only_metric == 0", For: "5m", Record: true}.text()
	}
	if err := s.l.SetSettings(ctx, values, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.syncSignals(ctx); err != nil {
		t.Fatal(err)
	}
	n := inEffect(t, s)
	if len(n.Defs) != verdict.MaxSignals || len(n.Aside) != 1 {
		t.Fatalf("defs %d, aside %v", len(n.Defs), n.Aside)
	}
	aside := n.Aside[0]
	code, body := adminPost(t, s, "/settings/signals", signalPost("u1", aside, "u1_only_metric == 0", "5m", "", true), "ana@example.com")
	if code != http.StatusBadRequest || !strings.Contains(body, "left aside") {
		t.Errorf("renamed over one left aside: %d", code)
	}
	if set, _ := s.l.Settings(ctx); !strings.Contains(set[signalKey+aside].Value, aside+"_only_metric") {
		t.Errorf("the one left aside written over: %s", set[signalKey+aside].Value)
	}
}

// Tried, a signal read in full stands for saving it within triedFor: the
// save reads it no more.
func TestSignalTryThenSaveReadsOnce(t *testing.T) {
	h := shopHistory()
	s := signalServer(t, h, nil)
	q := signalPost("", "latency", "latency_p99 > 1", "10m", "", true)
	q.Set("signal-try", "1")
	if code, body := askAs(t, s.Handler(), http.MethodGet, "/settings?"+q.Encode(), nil, "ana@example.com", "same-origin"); code != http.StatusOK {
		t.Fatalf("try: %d %.300s", code, body)
	}
	h.mu.Lock()
	before := len(h.asked)
	h.mu.Unlock()
	if code, body := adminPost(t, s, "/settings/signals", signalPost("", "latency", "latency_p99 > 1", "10m", "", true), "ana@example.com"); code != http.StatusSeeOther {
		t.Fatalf("save: %d %.300s", code, body)
	}
	h.mu.Lock()
	after := slices.Clone(h.asked[before:])
	h.mu.Unlock()
	if slices.Contains(after, "latency_p99 > 1") {
		t.Errorf("saved within triedFor of its try, it was read again: %v", after)
	}
}

// Two admins at once: one plans an add from the settings as they were,
// the other renames meanwhile. The first's write, planned on settings
// changed since, is refused, and the rename stands.
func TestSignalChangesPlannedApart(t *testing.T) {
	ctx := context.Background()
	s := signalServer(t, signalWindowsHistory(), nil)
	set, err := s.l.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := s.planSignal(ctx, set, signalForm{Name: "synthetic", Expr: "up == 0", For: "2m", Follows: true, Record: true}, "save", "bo@example.com", time.Now().UTC().Truncate(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	was, _ := signalRow(t, s, "probe")
	if code, body := adminPost(t, s, "/settings/signals", signalPost("probe", "synthetic", "probe_success == 0", "2m", "service", true), "ana@example.com"); code != http.StatusSeeOther {
		t.Fatalf("rename: %d %.200s", code, body)
	}
	pb.Change.Planned = plannedOn(set)
	if _, err := s.l.ChangeSignals(ctx, pb.Change, s.signalsFrom, "bo@example.com"); !errors.Is(err, ledger.ErrSignalsChanged) {
		t.Errorf("planned on settings changed since: %v", err)
	}
	if r, ok := signalRow(t, s, "synthetic"); !ok || r.Bit != was.Bit || r.Expr != "probe_success == 0" {
		t.Errorf("the rename: %+v", r)
	}
	// Posted, it says so.
	if code, _ := adminPost(t, s, "/settings/signals", signalPost("", "synthetic", "up == 0", "2m", "", true), "ana@example.com"); code != http.StatusBadRequest {
		t.Errorf("the name in effect now: %d", code)
	}
}

// An admin's read again that fails is said to them, and is no run's
// failure: the sources panel says nothing of it.
func TestSignalReadAgainFailureTheAdmins(t *testing.T) {
	h := signalWindowsHistory()
	s := signalServer(t, h, nil)
	h.mu.Lock()
	h.fail = map[string]bool{"probe_success == 0": true}
	h.mu.Unlock()
	code, body := adminPost(t, s, "/settings/signals/again", url.Values{"name": {"probe"}}, "ana@example.com")
	if code != http.StatusBadRequest || !strings.Contains(body, "query timed out") {
		t.Errorf("%d", code)
	}
	_, page := askAs(t, s.Handler(), http.MethodGet, "/settings", nil, "viewer@example.com", "same-origin")
	if strings.Contains(between(page, `id="incident-sources"`, "</table>"), "query timed out") {
		t.Error("a viewer reads the admin's read again's failure")
	}
}

// Options are read with the named signals' generation, before the
// settings: those read before a change judge nothing it pended.
func TestOptionsCarrySignalsGen(t *testing.T) {
	ctx := context.Background()
	s := signalServer(t, shopHistory(), nil)
	before, err := s.options(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := adminPost(t, s, "/settings/impact-rules", url.Values{"rules": {errorsOnProbe}}, "ana@example.com"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	after, err := s.options(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gen, _ := s.l.SignalsGen(ctx)
	if before.SignalsGen == 0 || after.SignalsGen != gen || before.SignalsGen == gen {
		t.Errorf("generations: before %d, after %d, the ledger's %d", before.SignalsGen, after.SignalsGen, gen)
	}
	if err := s.l.JudgePending(ctx, before); !errors.Is(err, ledger.ErrSignalsMoved) {
		t.Errorf("judged with options read before: %v", err)
	}
	if err := s.l.JudgePending(ctx, after); err != nil {
		t.Errorf("with options read after: %v", err)
	}
}

// A label's value given as !!binary is bytes, not text: refused, as is
// any tag but a string's, a list's or a mapping's; and a value that isn't
// valid UTF-8, or has a control character, however it came, is refused by
// the bounds a team's labels are held to.
func TestSignalEntriesTextOnly(t *testing.T) {
	ctx := context.Background()
	s := signalServer(t, shopHistory(), nil)
	raw := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\xff", 200)))
	for _, c := range []struct{ name, text, want string }{
		{"!!binary in a list", "- labels: {service: [!!binary " + raw + "]}\n  signals: [probe]\n", "!!binary"},
		{"!!binary alone", "- labels: {service: !!binary " + raw + "}\n  signals: [probe]\n", "!!binary"},
		{"!!int", "- labels: {service: !!int 1}\n  signals: [probe]\n", "!!binary"},
		{"a control character", "- labels: {service: \"a\\x07b\"}\n  signals: [probe]\n", "no control character"},
	} {
		code, body := adminPost(t, s, "/settings/impact-rules", url.Values{"rules": {c.text}}, "ana@example.com")
		if code != http.StatusBadRequest || !strings.Contains(body, c.want) {
			t.Errorf("%s: %d, want %q\n%s", c.name, code, c.want, between(body, "<main", "</main>"))
		}
	}
	// Tags of what they are anyway are as written.
	if code, body := adminPost(t, s, "/settings/impact-rules", url.Values{"rules": {"- labels: {service: !!seq [!!str a, b]}\n  signals: [probe]\n"}}, "ana@example.com"); code != http.StatusSeeOther {
		t.Errorf("!!str, !!seq: %d %s", code, between(body, "<main", "</main>"))
	}
	if set, _ := s.l.Settings(ctx); strings.Contains(set[rulesKey].Value, "\\ufffd") {
		t.Errorf("kept: %s", set[rulesKey].Value)
	}
}

// The start's rule entries, kept as the ledger keeps them, are the same
// read back, a value that isn't text among them: a round keeps them once,
// and the next ones write nothing, judging nothing again.
func TestSignalEntriesStartedKeptOnce(t *testing.T) {
	ctx := context.Background()
	s := signalServer(t, shopHistory(), func(c *Config) {
		c.ImpactRules = []verdict.RuleEntry{{Labels: incident.Labels{"service": {"\xff\xfe"}}, Signals: []string{"probe"}}}
	})
	g0, _ := s.l.SignalsGen(ctx)
	for i := 0; i < 3; i++ {
		if changed, err := s.syncSignals(ctx); err != nil || changed {
			t.Errorf("round %d: written again %v %v", i+1, changed, err)
		}
	}
	if g1, _ := s.l.SignalsGen(ctx); g1 != g0 {
		t.Errorf("signals_gen %d -> %d", g0, g1)
	}
}

// A rename reads nothing, but renames every window, judges every firing
// again and notes the entries naming it before and after: among the reads
// and saves an admin may ask for in an hour.
func TestSignalRenamesCounted(t *testing.T) {
	s := signalServer(t, shopHistory(), nil)
	names := []string{"probe", "synthetic"}
	for i := 0; i < impactTriesAnHour; i++ {
		from, to := names[i%2], names[(i+1)%2]
		if code, body := adminPost(t, s, "/settings/signals", signalPost(from, to, "probe_success == 0", "2m", "service", true), "ana@example.com"); code != http.StatusSeeOther {
			t.Fatalf("rename %d: %d %s", i+1, code, between(body, "<main", "</main>"))
		}
	}
	from, to := names[impactTriesAnHour%2], names[(impactTriesAnHour+1)%2]
	if code, body := adminPost(t, s, "/settings/signals", signalPost(from, to, "probe_success == 0", "2m", "service", true), "ana@example.com"); code != http.StatusBadRequest || !strings.Contains(body, "as many incident criteria") {
		t.Errorf("past the hour's: %d", code)
	}
}

// Two replicas on one ledger: the rule entries saved on one, and the
// other's options read after, of the generation after, are of the entries
// after, not kept from before by its own generation, which hasn't moved:
// what the save pended is judged with them, or left, never with those
// before.
func TestSignalsFollowerOptionsOfTheGeneration(t *testing.T) {
	ctx := context.Background()
	f := sharedLedger(t)
	rc := newSink(t)
	named := func(c *Config) {
		c.Impact = ""
		c.Admins = []string{"ana@example.com"}
		c.Signals = []ledger.SignalDef{
			{Name: "errors", Expr: "http_requests_total > 1e9", For: 5 * time.Minute, Record: true},
			{Name: "probe", Expr: "probe_success == 0", For: 2 * time.Minute, Match: []string{"service"}, Record: true},
		}
	}
	a := replicaOn(t, f, rc, probeAtFive(), named)
	raw := ""
	if err := a.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	a.jobs(ctx, true)
	a.jobs(ctx, false)
	a.ready.Store(true)
	b := replicaOn(t, f, rc, probeAtFive(), named)
	b.ready.Store(true)
	if err := b.l.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := b.options(ctx); err != nil { // b's, kept
		t.Fatal(err)
	}
	if code, body := adminPost(t, a, "/settings/impact-rules", url.Values{"rules": {errorsOnProbe}}, "ana@example.com"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	ob, err := b.options(ctx) // not refreshed: the save committed since
	if err != nil {
		t.Fatal(err)
	}
	if gen, _ := b.l.SignalsGen(ctx); ob.SignalsGen == gen && len(ob.Signals.Entries) != 1 {
		t.Errorf("options of generation %d with %d entries, the save's", ob.SignalsGen, len(ob.Signals.Entries))
	}
	if err := b.l.JudgePending(ctx, ob); err != nil && !errors.Is(err, ledger.ErrSignalsMoved) {
		t.Fatal(err)
	}
	a.jobs(ctx, false)
	if f := searchAtFive(t, a); !f.White {
		t.Error("after the leader's round: the search firing is still black, the entry not judged with")
	}
}

// A signal set on the Settings page has where its data begins looked for
// by its expression in effect, as one serve was started with: it keeps
// the record from its start.
func TestSignalSetHereLooksForItsData(t *testing.T) {
	ctx := context.Background()
	h := shopHistory()
	// Its data begins after it is read again: looked for, it is found later.
	h.begins["latency_p99"] = []time.Time{now.Add(90 * time.Minute)}
	s := signalServer(t, h, nil)
	if code, body := adminPost(t, s, "/settings/signals", signalPost("", "latency", "latency_p99 > 1", "10m", "service", true), "ana@example.com"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	for k := 1; k <= 4; k++ {
		runDue(s, k)
		s.jobs(ctx, true)
	}
	r, ok := signalRow(t, s, "latency")
	if !ok || r.Kept.Data == nil || r.Kept.From().IsZero() {
		t.Errorf("latency's record: %+v", r.Kept)
	}
}
