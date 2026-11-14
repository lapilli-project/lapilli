package serve

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"lapilli.dev/internal/ledger"
)

// The proposals list's facets (docs/design/ui.md).

// listServer is a server with 30 proposals of rules R00 to R29, of every
// kind, way, maker and state, and a team, payments, owning those whose
// names start R0.
func listServer(t *testing.T) *Server {
	t.Helper()
	s, _ := changesServer(t, nil)
	ctx := context.Background()
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "payments", Rules: "^R0"}, ana); err != nil {
		t.Fatal(err)
	}
	kinds := []string{ledger.KindFor, ledger.KindThreshold, ledger.KindDisable, ledger.KindDelete, ledger.KindSays}
	paths := []string{ledger.PathGit, ledger.PathGrafana, ledger.PathRuler, ledger.PathAlarm, ledger.PathNone}
	states := []string{ledger.StateProposed, ledger.StateApproved, ledger.StateReady, ledger.StateOpened, ledger.StateApplied,
		ledger.StateUndone, ledger.StateRefused, ledger.StateDropped, ledger.StateAmended, ledger.StateFailed}
	for i := range 30 {
		by := []string{bo, cy, ledger.Proposer}[i%3]
		id, err := s.l.Propose(ctx, ledger.Proposal{Rule: fmt.Sprintf("R%02d", i), Kind: kinds[i%5], Path: paths[(i/2)%5], MadeBy: by, Agent: i%7 == 3})
		if err != nil {
			t.Fatal(err)
		}
		if st := states[i%10]; st != ledger.StateProposed {
			if _, err := s.l.Move(ctx, id, []string{ledger.StateProposed}, st, bo, ledger.Step{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return s
}

// proposalsFor is the proposals list for a query, as cy sees it.
func proposalsFor(t *testing.T, s *Server, query string) proposalsView {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/proposals?"+query, nil)
	r.Header.Set("X-Forwarded-Email", cy)
	_, vr := s.withContext(r, "proposals")
	v, err := s.proposalsPage(vr)
	if err != nil {
		t.Fatal(err)
	}
	return v.(proposalsView)
}

// TestProposalsFacets: the proposals list's facets, as the rules list's:
// the open ones unless asked, an older link's state as it was (open, all,
// one state), each value counted as the list would show it picked alone in
// its facet with what the others pick, a chip for each filter in effect,
// and the team's proposals alone, picked in the bar.
func TestProposalsFacets(t *testing.T) {
	s := listServer(t)
	ids := func(v proposalsView) []string {
		var out []string
		for _, l := range v.Lines {
			out = append(out, l.Rule)
		}
		return out
	}
	open := proposalsFor(t, s, "")
	if open.Total != 30 || open.Shown != 12 || open.Pick.Active || len(open.Pick.Chips) != 0 {
		t.Errorf("unless asked: %d of %d, active %v", open.Shown, open.Total, open.Pick.Active)
	}
	for _, l := range open.Lines {
		if !l.Open() {
			t.Errorf("unless asked, %s %s is shown", l.Rule, l.State)
		}
	}
	// Waiting for approval first.
	if l := open.Lines; len(l) < 4 || l[0].State != ledger.StateProposed || l[1].State != ledger.StateProposed || l[2].State != ledger.StateProposed || l[3].State == ledger.StateProposed {
		t.Errorf("not those waiting for approval first: %q", ids(open))
	}
	for q, want := range map[string]int{"state=open": 12, "state=all": 30, "state=": 12, "state=refused": 3, "state=closed": 12, "state=bogus": 30,
		"state=all&kind=for": 6, "state=all&by=agent": 4, "state=all&by=lapilli": 9, "state=all&path=none": 6} {
		if v := proposalsFor(t, s, q); v.Shown != want {
			t.Errorf("%s: %d shown, want %d", q, v.Shown, want)
		}
	}
	// An older link's single state is offered as it is, ticked.
	v := proposalsFor(t, s, "state=refused")
	if o := v.Pick.Facets[0].Options; o[len(o)-1].Value != "refused" || !o[len(o)-1].Checked || o[len(o)-1].Count != 3 {
		t.Errorf("state=refused: %+v", o[len(o)-1])
	}
	// The counts.
	all := proposalsFor(t, s, "state=all")
	r := rand.New(rand.NewPCG(4, 7))
	checked := 0
	for range 40 {
		q := url.Values{}
		if r.IntN(3) == 0 {
			q.Set("team", "payments")
		}
		for _, f := range all.Pick.Facets {
			for _, o := range f.Options {
				if r.IntN(4) == 0 {
					q.Add(f.Param, o.Value)
				}
			}
		}
		if len(q["state"]) == 0 {
			q.Set("state", "all")
		}
		v := proposalsFor(t, s, q.Encode())
		for _, f := range v.Pick.Facets {
			for _, o := range f.Options {
				alone := url.Values{}
				for k, vs := range q {
					alone[k] = vs
				}
				alone[f.Param] = []string{o.Value}
				if got := proposalsFor(t, s, alone.Encode()).Shown; got != o.Count {
					t.Errorf("%s: %s=%s counts %d, and shows %d", q.Encode(), f.Param, o.Value, o.Count, got)
				}
				checked++
			}
		}
		for _, l := range v.Lines {
			for _, f := range proposalFacets {
				if picked := q[f.param]; len(picked) > 0 && !slices.Contains(picked, "all") && !slices.ContainsFunc(proposalValues(l.Proposal, f.param), func(x string) bool { return slices.Contains(picked, x) }) {
					t.Errorf("%s: %s shown, its %s %q", q.Encode(), l.Rule, f.param, proposalValues(l.Proposal, f.param))
				}
			}
			if q.Get("team") != "" && !strings.HasPrefix(l.Rule, "R0") {
				t.Errorf("%s: %s isn't payments'", q.Encode(), l.Rule)
			}
		}
	}
	if checked < 400 {
		t.Errorf("only %d counts checked", checked)
	}
	// The chips: each the same list without it; Clear, all of them.
	v = proposalsFor(t, s, "team=payments&kind=for&kind=says&by=agent")
	var chips []string
	for _, c := range v.Pick.Chips {
		chips = append(chips, c.Group+": "+c.Value+" "+c.Href)
	}
	if want := []string{"Team: payments /proposals?by=agent&kind=for&kind=says&team=", "Kind: a new for /proposals?by=agent&kind=says&team=payments",
		"Kind: a fix to what it says /proposals?by=agent&kind=for&team=payments", "Made by: an agent /proposals?kind=for&kind=says&team=payments"}; !slices.Equal(chips, want) {
		t.Errorf("chips:\n%q\nwant\n%q", chips, want)
	}
	if v.Pick.Clear != "/proposals?state=all&team=payments" || !v.Pick.Active {
		t.Errorf("clear: %q, active %v", v.Pick.Clear, v.Pick.Active)
	}
	// The form: none ticked is all of them, not the open ones.
	_, body, _ := ui(t, s.Handler(), http.MethodGet, "/proposals?team=payments", cy, "", nil)
	if !strings.Contains(body, `<form method="get" action="/proposals" class="facets pick" aria-label="Filters"><input type="hidden" name="team" value="payments"><input type="hidden" name="state" value="all">`) {
		t.Error("the form doesn't keep the team and say all")
	}
	if !strings.Contains(body, `<label>Team <select name="team">`) {
		t.Error("no team in the bar")
	}
	if v := proposalsFor(t, s, "state=all&state=proposed"); v.Shown != 3 {
		t.Errorf("all and one state: %d, want the one", v.Shown)
	}
}
