package serve

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// waiting makes a proposal waiting for approval for a rule, for a person to
// make.
func waiting(t *testing.T, s *Server, rule string) int64 {
	t.Helper()
	id, err := s.l.Propose(context.Background(), ledger.Proposal{Rule: rule, Kind: ledger.KindFor, Path: ledger.PathGit, MadeBy: "ed",
		Before: "for: 2m\n", After: "for: 10m\n", Diff: "-for: 2m\n+for: 10m\n", BeforeHash: "x"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Off by default: a proposal nobody approves waits, and holds its rule,
// however long. With --proposals-expire, the leader's work closes one that
// waited longer as expired, in its history and the activity log, and its
// rule is free for another; one approved, or made since, is left. Its page
// says when it would.
func TestProposalsExpire(t *testing.T) {
	ctx := context.Background()
	s, _, _ := rolesServer(t, nil)
	old := waiting(t, s, "Errors")
	s.wall = func() time.Time { return time.Now().Add(400 * 24 * time.Hour) }
	s.jobs(ctx, false)
	if p, _ := s.l.ProposalByID(ctx, old); p.State != ledger.StateProposed {
		t.Fatalf("expired with --proposals-expire off: %+v", p)
	}
	if code, body := getAs(t, s.Handler(), fmt.Sprintf("/proposals/%d", old), "ed@example.com"); code != http.StatusOK || strings.Contains(body, "closes as expired") {
		t.Errorf("its page, off: %d", code)
	}

	s.cfg.ProposalsExpire = 30 * 24 * time.Hour
	s.wall = time.Now
	approved := waiting(t, s, "Search")
	if _, err := s.l.Move(ctx, approved, []string{ledger.StateProposed}, ledger.StateApproved, "boss", ledger.Step{}); err != nil {
		t.Fatal(err)
	}
	code, body := getAs(t, s.Handler(), fmt.Sprintf("/proposals/%d", old), "ed@example.com")
	if code != http.StatusOK || !strings.Contains(body, "closes as expired on") {
		t.Errorf("its page doesn't say when it closes: %d", code)
	}
	// Twenty days on: not yet.
	s.wall = func() time.Time { return time.Now().Add(20 * 24 * time.Hour) }
	s.jobs(ctx, false)
	if p, _ := s.l.ProposalByID(ctx, old); p.State != ledger.StateProposed {
		t.Errorf("expired after twenty days of thirty: %+v", p)
	}
	// Thirty-one: closed.
	s.wall = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	s.jobs(ctx, false)
	p, _ := s.l.ProposalByID(ctx, old)
	if p.State != ledger.StateExpired || !strings.Contains(p.Reason, "--proposals-expire, 30d") {
		t.Errorf("after thirty-one days: %+v", p)
	}
	if a, _ := s.l.ProposalByID(ctx, approved); a.State != ledger.StateApproved {
		t.Errorf("an approved one expired: %+v", a)
	}
	steps, _, _ := s.l.ProposalSteps(ctx, old, 10)
	if len(steps) == 0 || steps[len(steps)-1].What != fmt.Sprintf("proposal #%d expired", old) || steps[len(steps)-1].By != ledger.Proposer {
		t.Errorf("its history: %+v", steps)
	}
	if err := s.l.MayPropose(ctx, "Errors", "ed"); err != nil {
		t.Errorf("its rule, after it expired: %v", err)
	}
	// Its page says so, and the list finds it closed.
	_, body = getAs(t, s.Handler(), fmt.Sprintf("/proposals/%d", old), "ed@example.com")
	if !strings.Contains(body, "expired") || strings.Contains(body, "closes as expired on") {
		t.Errorf("its page after it expired")
	}
	if _, body := getAs(t, s.Handler(), "/proposals?state=closed", "ed@example.com"); !strings.Contains(body, fmt.Sprintf(`/proposals/%d"`, old)) {
		t.Errorf("the closed ones don't hold it")
	}
}

// A proposal someone approved isn't closed as expired: one of two
// approvals, or, --approvals lowered, the one it now needs, which carrying
// out approves. Nor is one being backtested again here, until it is done.
// Its page says when it would expire only while no one approved it.
func TestProposalsExpireLeaves(t *testing.T) {
	ctx := context.Background()
	s, _, _ := rolesServer(t, func(c *Config) { c.ProposalsExpire = 30 * 24 * time.Hour; c.Approvals = 2 })
	approved := waiting(t, s, "Errors")
	if _, err := s.l.Approve(ctx, approved, "boss", 2, time.Time{}); err != nil {
		t.Fatal(err)
	}
	busy := waiting(t, s, "Search")
	done := s.backtesting.add(busy)
	s.cfg.Approvals, s.cfg.Changes = 1, true
	s.wall = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	s.jobs(ctx, false)
	if p, _ := s.l.ProposalByID(ctx, approved); p.State == ledger.StateExpired {
		t.Errorf("approved, and closed as expired: %+v", p)
	}
	if p, _ := s.l.ProposalByID(ctx, busy); p.State != ledger.StateProposed {
		t.Errorf("closed while backtested again: %+v", p)
	}
	if _, body := getAs(t, s.Handler(), fmt.Sprintf("/proposals/%d", approved), "ed@example.com"); strings.Contains(body, "closes as expired") {
		t.Error("an approved one's page says it would expire")
	}
	done()
	s.jobs(ctx, false)
	if p, _ := s.l.ProposalByID(ctx, busy); p.State != ledger.StateExpired {
		t.Errorf("once its backtest is done: %+v", p)
	}
}

// Lapilli doesn't backtest again itself a stale proposal past
// --proposals-expire, which someone approved: backtested again, its
// approvals withdrawn, it would wait anew, and close once more had passed
// than anyone meant. Before it is past, it does.
func TestBacktestStaleLeavesPastExpiry(t *testing.T) {
	ctx := context.Background()
	s, ids := staleServer(t, 1, func(c *Config) { c.Approvals = 2; c.ProposalsExpire = 30 * 24 * time.Hour })
	if _, err := s.l.Approve(ctx, ids[0], ana, 2, time.Time{}); err != nil {
		t.Fatal(err)
	}
	s.wall = func() time.Time { return time.Now().Add(40 * 24 * time.Hour) }
	if err := s.backtestStale(ctx); err != nil {
		t.Fatal(err)
	}
	if p := state(t, s, ids[0]); againCount(t, s, ids) != 0 || len(p.Approvals) != 1 {
		t.Fatalf("past expiry, backtested again: %+v", p)
	}
	s.wall = time.Now
	if err := s.backtestStale(ctx); err != nil {
		t.Fatal(err)
	}
	if againCount(t, s, ids) != 1 {
		t.Error("before expiry, not backtested again")
	}
}
