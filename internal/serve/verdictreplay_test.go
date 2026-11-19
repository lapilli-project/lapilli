package serve

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
)

// errorsFiring is Errors' firing on checkout, 30 hours before now, as the
// ledger has it.
func errorsFiring(t *testing.T, s *Server) ledger.Firing {
	t.Helper()
	fs, err := s.l.Firings(context.Background(), ledger.FiringQuery{From: now.Add(-31 * time.Hour), To: now.Add(-28 * time.Hour), Rule: "Errors"})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(fs, func(f ledger.Firing) bool { return f.Labels["service"] == "checkout" })
	if i < 0 {
		t.Fatalf("no checkout firing: %+v", fs)
	}
	return fs[i]
}

// backtestOf is a proposal's backtest, as its evidence keeps it.
func backtestOf(t *testing.T, s *Server, id int64) *backtest {
	t.Helper()
	var ev evidence
	if err := json.Unmarshal(state(t, s, id).Evidence, &ev); err != nil || ev.Backtest == nil {
		t.Fatalf("proposal %d's evidence: %v %+v", id, err, ev)
	}
	return ev.Backtest
}

// A proposal's backtest carries people's verdicts on both sides
// (docs/design/judging.md, "Through judging again"): with a longer for,
// Errors' firing over INC-9 starts later, finds the person's "unrelated"
// again, and stays black, as the rule as it is has it; without the
// verdict it is white on both. A verdict said in its period since it was
// backtested makes it stale.
func TestBacktestCarriesVerdicts(t *testing.T) {
	ctx := context.Background()
	s, _ := changesServer(t, nil)
	h := s.Handler()
	o, err := s.options(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.Put(ctx, incident.Incident{ID: "INC-9", Title: "checkout down", Start: now.Add(-30 * time.Hour), End: now.Add(-29 * time.Hour),
		Labels: incident.Labels{"service": {"checkout"}}}, ana, o); err != nil {
		t.Fatal(err)
	}
	f := errorsFiring(t, s)
	if !f.White {
		t.Fatalf("Errors' checkout firing should catch INC-9: %+v", f)
	}
	first := proposeFor(t, h, "10m")
	without := backtestOf(t, s, first)
	if without.Before.White == 0 || without.After.White != without.Before.White || len(without.Gained)+len(without.Lost) != 0 {
		t.Fatalf("with no verdict: %+v", without)
	}
	staleWhy := func(id int64) *staleWhy {
		t.Helper()
		ls, err := s.linesNow(ctx, []ledger.Proposal{state(t, s, id)})
		if err != nil {
			t.Fatal(err)
		}
		return ls[0].StaleWhy
	}
	if why := staleWhy(first); why != nil {
		t.Fatalf("stale before the verdict: %+v", why)
	}
	said := true
	if _, err := s.l.SetVerdict(ctx, ledger.FiringKey{ID: f.ID, Series: f.Series, Start: f.Start}, ledger.VerdictChange{Unrelated: &said},
		ledger.VerdictBy{By: bo, Now: now.Add(time.Minute)}, o); err != nil {
		t.Fatal(err)
	}
	// Backtested before the verdict: stale.
	if why := staleWhy(first); why == nil || why.Format != staleSaid {
		t.Errorf("backtested before the verdict: stale %+v", why)
	}
	if _, err := s.l.Move(ctx, first, []string{ledger.StateProposed}, ledger.StateDropped, bo, ledger.Step{}); err != nil {
		t.Fatal(err)
	}
	with := backtestOf(t, s, proposeFor(t, h, "10m"))
	if with.Before.White != without.Before.White-1 || with.After.White != with.Before.White || len(with.Gained) != 0 {
		t.Errorf("with the verdict: before %+v after %+v gained %v; want both one white fewer, nothing gained", with.Before, with.After, with.Gained)
	}
}
