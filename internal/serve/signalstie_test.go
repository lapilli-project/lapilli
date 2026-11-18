package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/ledger/ledgertest"
	"lapilli.dev/internal/verdict"
)

// Serve started again with named signals on the command line, on a ledger
// that holds the windows of the impact signal with no name: they are tied
// to the signal impact, as the Settings page ties them when the first named
// signal is saved there (docs/design/signals.md, "Step 3, as built"), so
// that they are no incident with no signal, which keeps every rule's
// record.

// logBuffer is a log a test reads, written to by the server's goroutines.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// tiedLine is what the log says of the windows tied, and tiedChange what
// the activity log notes, by tiedBy.
const (
	tiedLine   = "windows of the impact signal with no name tied to a named signal"
	tiedChange = "impact windows of the signal with no name tied to the signal impact"
	tiedBy     = "lapilli"
)

// restarted is a server on the ledger of ledgerDir and ledgerFile (a new
// one for ""), reading h, with no incidents file, logging to logs, its
// first round run: what keeps the record is the impact signals alone.
func restarted(t *testing.T, ledgerDir, ledgerFile string, h *sigHistory, logs io.Writer, change func(*Config)) *Server {
	t.Helper()
	dir := t.TempDir()
	rules := filepath.Join(dir, "rules")
	os.MkdirAll(rules, 0o755)
	os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte(ruleFile), 0o644)
	if ledgerDir == "" {
		ledgerDir, ledgerFile = filepath.Join(dir, "ledger"), ledgertest.File(t)
	}
	cfg := Config{
		RulesPath: rules,
		Options:   verdict.Options{Lead: 15 * time.Minute, RefireWithin: time.Hour, UnknownLen: time.Hour, Match: []string{"service"}},
		Step:      time.Minute, Loc: time.UTC, Ledger: ledgerDir, LedgerURLFile: ledgerFile,
		Every: time.Hour, Settle: time.Hour, Backfill: 3 * 24 * time.Hour, Watch: time.Minute,
		Digest: Weekly{time.Monday, 9, 0}, Timeout: time.Minute, Parallel: 4, Listen: "127.0.0.1:0", Version: "test",
		AllowedHosts: []string{"example.com"}, UserHeader: "X-Forwarded-Email", Admins: []string{"ana@example.com"},
	}
	change(&cfg)
	if h.history == nil {
		h.history = &history{}
	}
	s, err := New(context.Background(), cfg, h, slog.New(slog.NewTextHandler(logs, nil)))
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

// unnamedFirst is a server reading the impact signal with no name
// (--impact "impact > 1": search's window, checkout's, and cart's), its
// windows and the firings of Errors recorded, then stopped: the ledger a
// lapilli serve before named signals leaves, to start again on.
func unnamedFirst(t *testing.T) (string, string) {
	t.Helper()
	s := restarted(t, "", "", &sigHistory{}, io.Discard, func(c *Config) { c.Impact, c.ImpactFor = "impact > 1", 5*time.Minute })
	ws, err := s.l.ImpactWindows(context.Background(), now.Add(-30*24*time.Hour), now)
	if err != nil || len(ws) != 3 || slices.ContainsFunc(ws, func(w ledger.Incident) bool { return w.Signal != "" }) {
		t.Fatalf("the windows of the signal with no name: %+v %v", ws, err)
	}
	dir, file := s.cfg.Ledger, s.cfg.LedgerURLFile
	s.Close()
	return dir, file
}

// payments keeps no record (record: false), and an entry ties Errors to it
// alone: its firings are left unjudged, as the team meant.
var (
	paymentsSignal = ledger.SignalDef{Name: "payments", Expr: "payments_failed_total > 0", For: time.Minute, Record: false}
	paymentsOnly   = []verdict.RuleEntry{{Rules: "^Errors$", Signals: []string{"payments"}}}
)

// errorsFirings are the report's firings of Errors, and the report, from
// the Monday of the week before the windows to now.
func errorsFirings(t *testing.T, s *Server) (verdict.Report, []verdict.Judged) {
	t.Helper()
	code, body, _ := get(t, s.Handler(), "/api/v1/report?from=2026-10-26T00:00:00Z&to=2026-11-03T12:00:00Z")
	if code != 200 {
		t.Fatalf("report: %d %s", code, body)
	}
	var rep verdict.Report
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatal(err)
	}
	var out []verdict.Judged
	for _, f := range rep.Firings {
		if f.Rule == "Errors" {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		t.Fatal("no firing of Errors")
	}
	return rep, out
}

// Started again with signals in a file, impact among them, and Errors tied
// by an entry to payments alone, which keeps no record: the windows of the
// signal with no name are impact's, their keys as they were, its row what
// kept their record; Errors' firings are unjudged, in the windows' weeks
// too, as lapilli verdict judges them with the same signals and no window
// kept from before; the log says it once, and the Run history notes it.
// Started again so, nothing more is done.
func TestUnnamedWindowsTiedToImpact(t *testing.T) {
	ctx := context.Background()
	dir, file := unnamedFirst(t)
	var logs logBuffer
	named := func(c *Config) {
		c.Signals = []ledger.SignalDef{{Name: "impact", Expr: "impact > 1", For: 5 * time.Minute, Record: true}, paymentsSignal}
		c.ImpactRules = paymentsOnly
	}
	s := restarted(t, dir, file, &sigHistory{}, &logs, named)
	rep, fs := errorsFirings(t, s)
	for _, f := range fs {
		if !f.NoRecord || f.White {
			t.Errorf("a firing of Errors at %v: no record %v, white %v; want unjudged", f.Start, f.NoRecord, f.White)
		}
	}
	// lapilli verdict, with the same signals and entries and no window
	// kept from before (a fresh run: payments found none), says the same.
	o, err := s.options(ctx)
	if err != nil {
		t.Fatal(err)
	}
	o.From, o.To = time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), now
	var plain []firing.Firing
	for _, f := range rep.Firings {
		plain = append(plain, f.Firing)
	}
	for _, r := range verdict.Judge(nil, plain, nil, o).Rules {
		if r.Rule == "Errors" && !r.NoRecord {
			t.Errorf("verdict.Judge judges Errors: %d unjudged of %d", r.Unjudged, r.Firings)
		}
	}
	// Impact's windows, keys and all: no read again of it, its criterion
	// that of the one with no name, beside them.
	ws, err := s.l.ImpactWindows(ctx, now.Add(-30*24*time.Hour), now)
	if err != nil || len(ws) != 3 {
		t.Fatalf("windows: %+v %v", ws, err)
	}
	for _, w := range ws {
		if w.Signal != "impact" || strings.HasPrefix(w.ID, "impact-impact-") {
			t.Errorf("window %s of %q: want impact's, its key as it was", w.ID, w.Signal)
		}
	}
	if k, err := s.l.SignalKept(ctx, "impact"); err != nil || k.Criterion != "5m impact > 1" || k.From().IsZero() {
		t.Errorf("what keeps impact's record, the one with no name's: %+v %v", k, err)
	}
	if n := strings.Count(logs.String(), tiedLine); n != 1 || !strings.Contains(logs.String(), "windows=3 signal=impact") {
		t.Errorf("the log, %d lines of it:\n%s", n, logs.String())
	}
	cs, err := s.l.Changes(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	tied := slices.IndexFunc(cs, func(c ledger.Change) bool { return c.What == tiedChange })
	if tied < 0 || cs[tied].By != tiedBy {
		t.Fatalf("the activity log: %+v", cs)
	}
	if _, body, _ := get(t, s.Handler(), "/runs"); !strings.Contains(body, tiedBy+": "+tiedChange) {
		t.Error("the Run history doesn't say it")
	}
	// Started again: nothing more.
	s.Close()
	var logs2 logBuffer
	again := restarted(t, dir, file, &sigHistory{}, &logs2, named)
	if strings.Contains(logs2.String(), tiedLine) {
		t.Errorf("started again, tied again:\n%s", logs2.String())
	}
	if cs2, _ := again.l.Changes(ctx, 100); len(cs2) != len(cs) {
		t.Errorf("started again, the activity log has %d, was %d", len(cs2), len(cs))
	}
}

// Started again with signals in a file and none named impact: the windows
// of the signal with no name are impact's, a signal no longer read, kept
// as the Settings page keeps the windows of one it took away (incidents
// with its name), and the log says so.
func TestUnnamedWindowsTiedToNoneRead(t *testing.T) {
	ctx := context.Background()
	dir, file := unnamedFirst(t)
	var logs logBuffer
	s := restarted(t, dir, file, &sigHistory{}, &logs, func(c *Config) {
		c.Signals = []ledger.SignalDef{{Name: "latency", Expr: "latency_seconds > 1", For: 10 * time.Minute, Record: true}, paymentsSignal}
		c.ImpactRules = paymentsOnly
	})
	ws, err := s.l.ImpactWindows(ctx, now.Add(-30*24*time.Hour), now)
	if err != nil || len(ws) != 3 || slices.ContainsFunc(ws, func(w ledger.Incident) bool { return w.Signal != "impact" }) {
		t.Fatalf("windows: %+v %v", ws, err)
	}
	if rows, err := s.l.SignalRows(ctx); err != nil || slices.ContainsFunc(rows, func(r ledger.SignalRow) bool { return r.Name == "impact" }) {
		t.Errorf("impact has a row, as a signal in effect: %+v %v", rows, err)
	}
	if !strings.Contains(logs.String(), tiedLine) || !strings.Contains(logs.String(), "windows=3 signal=impact read=false") {
		t.Errorf("the log:\n%s", logs.String())
	}
}

// With the signal with no name still given (no named signal), nothing is
// tied, and nothing said.
func TestUnnamedWindowsKeptUnnamed(t *testing.T) {
	ctx := context.Background()
	dir, file := unnamedFirst(t)
	var logs logBuffer
	s := restarted(t, dir, file, &sigHistory{}, &logs, func(c *Config) { c.Impact, c.ImpactFor = "impact > 1", 5*time.Minute })
	ws, err := s.l.ImpactWindows(ctx, now.Add(-30*24*time.Hour), now)
	if err != nil || len(ws) != 3 || slices.ContainsFunc(ws, func(w ledger.Incident) bool { return w.Signal != "" }) {
		t.Errorf("windows: %+v %v", ws, err)
	}
	if strings.Contains(logs.String(), tiedLine) {
		t.Errorf("the log:\n%s", logs.String())
	}
}

// Started with named signals on a ledger with no window of the signal with
// no name: nothing is tied, and nothing said.
func TestNamedWithNoUnnamedWindows(t *testing.T) {
	var logs logBuffer
	s := restarted(t, "", "", &sigHistory{}, &logs, func(c *Config) {
		c.Signals = []ledger.SignalDef{{Name: "impact", Expr: "impact > 1", For: 5 * time.Minute, Record: true}, paymentsSignal}
		c.ImpactRules = paymentsOnly
	})
	if strings.Contains(logs.String(), tiedLine) {
		t.Errorf("the log:\n%s", logs.String())
	}
	cs, err := s.l.Changes(context.Background(), 100)
	if err != nil || slices.ContainsFunc(cs, func(c ledger.Change) bool { return c.What == tiedChange }) {
		t.Errorf("the activity log: %+v %v", cs, err)
	}
}

// The windows tied are judged again from --lead before them, the lead in
// effect: the UI's, when it set one.
func TestSignalsFromLead(t *testing.T) {
	s, _ := server(t, nil)
	if got, _ := s.signalsFrom(nil); got.Lead != 15*time.Minute {
		t.Errorf("the command line's lead: %v", got.Lead)
	}
	if got, _ := s.signalsFrom(map[string]ledger.Setting{"lead": {Value: "40m"}}); got.Lead != 40*time.Minute {
		t.Errorf("the UI's lead: %v", got.Lead)
	}
	if got, _ := s.signalsFrom(map[string]ledger.Setting{"lead": {Value: "soon"}}); got.Lead != 15*time.Minute {
		t.Errorf("a lead that doesn't parse: %v, want the command line's", got.Lead)
	}
}
