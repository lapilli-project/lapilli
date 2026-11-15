package serve

import (
	"context"
	"maps"
	"slices"
	"time"

	"lapilli.dev/internal/ledger"
)

// Teams own rules, and incidents by their labels: a rule one team owns that
// fires on what another owns, a namespace of its, is on none of that
// team's pages. The weekly digest counts each rule's firings by the team
// whose labels they had (owners.labelTeam), from one grouped read of the
// week's firings (ledger.AlertCounts), and keeps those most of whose
// firings came from what another team owns than the one its alerts go to
// as the week's fired_on result. The pages read it from there.

// firedOn is a rule most of whose firings in a week, more than half, came
// from what another team owns than the one its alerts go to.
type firedOn struct {
	Rule string `json:"rule"`
	// Owner is the team its alerts go to, "" for none; Team, the one most
	// of its firings came from what it owns, Firings of them, of Of.
	Owner   string `json:"owner,omitempty"`
	Team    string `json:"team"`
	Firings int    `json:"firings"`
	Of      int    `json:"of"`
	// Unread are, of Of, the firings of its alerts past the firedOnRead
	// that fired most, which weren't read: counted as no team's.
	Unread int `json:"unread,omitempty"`
}

// firedOnWithin is the most time the weekly digest gives the read of the
// firings by team, past its replays' --timeout; --timeout, when shorter.
const firedOnWithin = time.Minute

// firedOnRead is how many of the alerts that fired most in a week are read
// to count firings by team: those past them are counted as no team's.
// Replaced in tests.
var firedOnRead = 20000

// firedOnOf are the rules most of whose firings in [from, to) came from
// what another team owns than theirs, by name; none when no team owns by
// labels. unread are the firings of the alerts past the firedOnRead that
// fired most, all told, which weren't read.
func (s *Server) firedOnOf(ctx context.Context, from, to time.Time) (out []firedOn, unread int, err error) {
	own, err := s.owners(ctx)
	if err != nil || !slices.ContainsFunc(own.teams, func(t ledger.Team) bool { return len(t.Labels) > 0 }) {
		return nil, 0, err
	}
	counts, all, err := s.l.AlertCounts(ctx, from, to, firedOnRead)
	if err != nil {
		return nil, 0, err
	}
	type tally struct {
		of, read int
		by       map[string]int
	}
	by := map[string]*tally{}
	for rule, n := range all {
		by[rule] = &tally{of: n, by: map[string]int{}}
		unread += n
	}
	for _, a := range counts {
		x := by[a.Rule]
		if x == nil {
			continue
		}
		x.read += a.Firings
		unread -= a.Firings
		if team := own.labelTeam(a.Labels); team != "" {
			x.by[team] += a.Firings
		}
	}
	for _, rule := range slices.Sorted(maps.Keys(by)) {
		x := by[rule]
		best, n := "", 0
		for _, t := range own.teams { // the first by name, of as many
			if c := x.by[t.Name]; c > n {
				best, n = t.Name, c
			}
		}
		if owner := own.teamOf(rule); best != "" && best != owner && 2*n > x.of {
			out = append(out, firedOn{Rule: rule, Owner: owner, Team: best, Firings: n, Of: x.of, Unread: max(x.of-x.read, 0)})
		}
	}
	return out, max(unread, 0), nil
}

// firedOnWeek is what the last digest kept of the rules that fired on what
// another team owns, by rule, and the week it was kept for; each with the
// team its alerts go to now, and none whose team that is now.
func (s *Server) firedOnWeek(ctx context.Context) (map[string]firedOn, time.Time, error) {
	rs, err := s.l.Results(ctx, "fired_on", 1)
	if err != nil || len(rs) == 0 {
		return nil, time.Time{}, err
	}
	return s.firedOnIn(ctx, rs[0])
}

// firedOnFor is what the digest kept for the week from from: as
// firedOnWeek.
func (s *Server) firedOnFor(ctx context.Context, from time.Time) (map[string]firedOn, error) {
	r, ok, err := s.l.ForPeriod(ctx, "fired_on", from)
	if err != nil || !ok {
		return nil, err
	}
	out, _, err := s.firedOnIn(ctx, r)
	return out, err
}

func (s *Server) firedOnIn(ctx context.Context, r ledger.Result) (map[string]firedOn, time.Time, error) {
	var xs []firedOn
	if !s.decodeKept(r, &xs) || len(xs) == 0 {
		return nil, time.Time{}, nil
	}
	own, err := s.owners(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	out := make(map[string]firedOn, len(xs))
	for _, x := range xs {
		if x.Owner = own.teamOf(x.Rule); x.Owner != x.Team {
			out[x.Rule] = x
		}
	}
	return out, r.From, nil
}

// firedOnView is a rule that fired on what another team owns, in the week
// of Week, as a page says it.
type firedOnView struct {
	firedOn
	Week time.Time
}

// firedOnLines are, of what was kept, the rules that fired on what team
// owns, by name.
func firedOnLines(on map[string]firedOn, week time.Time, team string) []firedOnView {
	var out []firedOnView
	for _, rule := range slices.Sorted(maps.Keys(on)) {
		if on[rule].Team == team {
			out = append(out, firedOnView{on[rule], week})
		}
	}
	return out
}
