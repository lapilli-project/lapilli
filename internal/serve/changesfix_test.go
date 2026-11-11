package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/prom"
)

// What an attacker, or bad luck, tried against changing rules, each fixed
// (CONTRIBUTING.md): the review before v0.22.0.

// grafanaFlaky is Grafana's stand-in, failing as Grafana and what is in
// front of it can.
type grafanaFlaky struct {
	g *grafanaStandIn
	// unreadable fails every read of a rule once one was written.
	unreadable atomic.Bool
	// deleteAnswer answers a DELETE, done, with this status.
	deleteAnswer int
	// pauseOn is made, once armed, when the rule abc is read: an admin
	// pausing changes between the read and the write.
	pauseOn string
	armed   atomic.Bool
}

func (f *grafanaFlaky) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/provisioning/alert-rules")
	if f.pauseOn != "" && r.Method == http.MethodGet && rest == "/abc" && f.armed.CompareAndSwap(true, false) {
		os.WriteFile(f.pauseOn, []byte("1"), 0o644)
	}
	if f.deleteAnswer != 0 && r.Method == http.MethodDelete {
		f.g.ServeHTTP(httptest.NewRecorder(), r)
		http.Error(w, "upstream timeout", f.deleteAnswer)
		return
	}
	if f.unreadable.Load() && r.Method == http.MethodGet && rest != "/export" && len(f.g.writes) > 0 {
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	f.g.ServeHTTP(w, r)
}

func flakyServer(t *testing.T, f *grafanaFlaky, more func(*Config)) *Server {
	t.Helper()
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	s, _ := changesServer(t, func(c *Config) {
		c.RulesPath, c.Grafana = "", ts.URL
		c.ChangesGrafanaToken = tokenFile(t, "grafana-writes-0123456789")
		if more != nil {
			more(c)
		}
	})
	return s
}

func locID(hd http.Header) int64 {
	var id int64
	json.Unmarshal([]byte(strings.TrimPrefix(hd.Get("Location"), "/proposals/")), &id)
	return id
}

// Grafana takes a change, then can't be read: the change is kept as
// applying, with the rule as it was, and counted against the limits; it is
// read back later, and never sent again.
func TestGrafanaReadBackFails(t *testing.T) {
	ctx := context.Background()
	g := &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}}
	f := &grafanaFlaky{g: g}
	s := flakyServer(t, f, func(c *Config) { c.ChangesLimits = Limits{Deletions: 5, Changes: 1} })
	h := s.Handler()
	a := proposeFor(t, h, "15m")
	_, _, hd := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"Other"}, "kind": {"for"}, "for": {"20m"}})
	b := locID(hd)
	approve(t, h, a, ana)
	approve(t, h, b, ana)
	f.unreadable.Store(true)
	s.jobs(ctx, false)
	pa, pb := state(t, s, a), state(t, s, b)
	if pa.State != ledger.StateApplying || pa.Backup == "" || pa.Applied.IsZero() || !strings.Contains(pa.Reason, "not read back yet") {
		t.Fatalf("written, not read back: %+v", pa)
	}
	if pb.State != ledger.StateApproved || len(g.writes) != 1 || g.rules["abc"]["for"] != "15m" {
		t.Fatalf("one change an hour: %s, written %v", pb.State, g.writes)
	}
	// It blocks another for the rule, as any change under way does.
	if code, _, _ := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"Errors"}, "kind": {"for"}, "for": {"30m"}}); code < 400 {
		t.Errorf("another proposal for a rule being changed: %d", code)
	}
	// Read later: applied, and its undo has what it needs.
	f.unreadable.Store(false)
	s.jobs(ctx, false)
	if pa = state(t, s, a); pa.State != ledger.StateApplied || pa.AfterHash == "" || len(g.writes) != 1 || strings.Contains(pa.Reason, "not read back yet") {
		t.Fatalf("read back: %+v, written %v", pa, g.writes)
	}
	if keyFor(t, h, a, ana, "undo") == "" {
		t.Error("no undo for a change read back late")
	}
}

// Grafana deletes a rule and answers 504: the deletion is applied, the
// rule as it was kept, and it can be undone.
func TestGrafanaDeleteAnswersBadly(t *testing.T) {
	ctx := context.Background()
	g := &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}}
	f := &grafanaFlaky{g: g}
	s := flakyServer(t, f, nil)
	h := s.Handler()
	_, _, hd := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"Other"}, "kind": {"disable"}})
	off := locID(hd)
	approve(t, h, off, ana)
	s.jobs(ctx, false)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Now().Add(5 * weekLen) }
	id, err := s.propose(ctx, ask{Rule: "Other", Kind: ledger.KindDelete, Follows: off, From: "off four weeks"}, ledger.Proposer, false, true)
	if err != nil {
		t.Fatal(err)
	}
	approve(t, h, id, cy)
	f.deleteAnswer = http.StatusGatewayTimeout
	s.jobs(ctx, false)
	p := state(t, s, id)
	if p.State != ledger.StateApplied || p.Backup == "" || g.rules["def"] != nil || !strings.Contains(p.Reason, "504") {
		t.Fatalf("deleted, answered 504: %+v, Grafana has %v", p, g.rules["def"])
	}
	key := keyFor(t, h, id, ana, "undo")
	_, _, hd = ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/undo", ana, "same-origin", url.Values{"key": {key}})
	s.jobs(ctx, false)
	if u := state(t, s, locID(hd)); u.State != ledger.StateApplied || g.rules["def"] == nil {
		t.Errorf("undone: %+v", u)
	}
}

// Paused while a change is checked again: nothing is sent to Grafana, or
// handed to a recipe; it waits, approved.
func TestPausedWhileChecked(t *testing.T) {
	ctx := context.Background()
	t.Run("grafana", func(t *testing.T) {
		stop := filepath.Join(t.TempDir(), "paused")
		g := &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}}
		f := &grafanaFlaky{g: g, pauseOn: stop}
		s := flakyServer(t, f, func(c *Config) { c.ChangesPaused = stop })
		h := s.Handler()
		id := proposeFor(t, h, "15m")
		approve(t, h, id, ana)
		f.armed.Store(true)
		if err := s.carryOut(ctx); err != nil {
			t.Fatal(err)
		}
		if p := state(t, s, id); p.State != ledger.StateApproved || len(g.writes) != 0 {
			t.Fatalf("paused before the write: %s, written %v", p.State, g.writes)
		}
		os.Remove(stop)
		s.jobs(ctx, false)
		if p := state(t, s, id); p.State != ledger.StateApplied || len(g.writes) != 1 {
			t.Errorf("resumed: %s, written %v", p.State, g.writes)
		}
	})
	t.Run("git", func(t *testing.T) {
		stop := filepath.Join(t.TempDir(), "paused")
		s, _ := changesServer(t, func(c *Config) { c.ChangesPaused = stop })
		h, port := s.Handler(), s.IncidentsHandler()
		id := proposeFor(t, h, "15m")
		approve(t, h, id, ana)
		ph := &pausingHistory{stop: stop}
		ph.armed.Store(true)
		s.h = ph
		if err := s.carryOut(ctx); err != nil {
			t.Fatal(err)
		}
		os.Remove(stop)
		if p := state(t, s, id); p.State != ledger.StateApproved || ready(t, port) != 0 {
			t.Fatalf("paused during the backtest: %s", p.State)
		}
		s.jobs(ctx, false)
		if p := state(t, s, id); p.State != ledger.StateReady {
			t.Errorf("resumed: %s", p.State)
		}
	})
}

// pausingHistory is history, making the pause file when it is first asked
// once armed: an admin pausing changes during a backtest.
type pausingHistory struct {
	history
	stop  string
	armed atomic.Bool
}

func (h *pausingHistory) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if h.armed.CompareAndSwap(true, false) {
		os.WriteFile(h.stop, []byte("1"), 0o644)
	}
	return h.history.QueryRange(ctx, expr, start, end, step)
}

// The limits count a change once, when it is handed out or sent, never
// again when it takes effect; and a change a person made never.
func TestLimitsCountOnce(t *testing.T) {
	ctx := context.Background()
	s, dir := changesServer(t, nil)
	h, port := s.Handler(), s.IncidentsHandler()
	id := proposeFor(t, h, "15m")
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	handedOut := state(t, s, id).Applied
	if handedOut.IsZero() {
		t.Fatal("not counted when handed out")
	}
	time.Sleep(1100 * time.Millisecond) // the ledger keeps seconds
	mark := time.Now()
	recipe(t, port, http.MethodPatch, "/api/v1/changes/"+itoa(id), changesToken, map[string]string{"state": "opened", "link": "https://git.example/pull/1"})
	recipe(t, port, http.MethodPatch, "/api/v1/changes/"+itoa(id), changesToken, map[string]string{"state": "merged"})
	os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(state(t, s, id).Document), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	p := state(t, s, id)
	if n, _ := s.l.Carried(ctx, mark, ""); p.State != ledger.StateApplied || n != 0 || !p.Applied.Equal(handedOut) {
		t.Errorf("counted again when it took effect: %s, %d since, applied %v (handed out %v)", p.State, n, p.Applied, handedOut)
	}
	// A deletion for a person to make, made: applied, never counted.
	code, _, hd := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"Errors"}, "kind": {"delete"}})
	if code != http.StatusSeeOther {
		t.Fatal(code)
	}
	d := locID(hd)
	if p := state(t, s, d); p.Path != ledger.PathNone {
		t.Fatalf("path %s", p.Path)
	}
	os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte("groups:\n  - name: shop\n    rules:\n      - alert: Kept\n        expr: errors > 7\n"), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	if p := state(t, s, d); p.State != ledger.StateApplied || !p.Applied.IsZero() {
		t.Errorf("a person's change: %s, counted at %v", p.State, p.Applied)
	}
}
