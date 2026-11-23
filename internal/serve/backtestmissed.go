package serve

import (
	"context"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// holdMissed holds a backtest's change to the incidents, as lapilli
// verdict --fail-missed does (docs/design/backtests.md, "Held to the
// incidents"): the incidents a rule that differs caught before the change
// that no rule catches after it. The rules that differ are judged by the
// backtest; the rules the change leaves alone by serve's record of them,
// the catches the ledger keeps of each incident, never replayed. An
// incident is still caught when the ledger says a rule the change doesn't
// name caught it: one the pull request leaves alone, or one serve runs now
// that the pull request doesn't hold, which the change doesn't touch. It
// is not known, not missed, when:
//
//   - a rule that caught it in the record has a name told apart from one
//     the pull request holds ("Errors [shop]" beside "Errors"), which may
//     be the rule the change changes, or the name of one it changes and
//     one it leaves alone, alike in group and labels, which the ledger
//     counts as one;
//   - a rule the pull request leaves alone may have caught it unrecorded:
//     serve's runs didn't cover its time (before the first, past the
//     cursor), the rule wasn't in effect in serve over it, or serve
//     couldn't replay it, the last time or in a run meeting its time;
//   - a rule after the change couldn't be replayed, as in lapilli verdict.
//
// Every read is of the ledger, once a backtest: nothing is asked of the
// history.
func (s *Server) holdMissed(ctx context.Context, cmp *verdict.Comparison, pr *prepared, from, to time.Time, o verdict.Options) error {
	if cmp.Before == nil || cmp.After == nil {
		// Nothing replayed: no incident caught before is missed after.
		cmp.HoldMissedBy(nil)
		return nil
	}
	rec, err := s.leftAloneRecord(ctx, pr, from, to, o)
	if err != nil {
		return err
	}
	cmp.HoldMissedBy(rec.catches)
	return nil
}

// leftAlone is what serve's record holds of the rules a backtest's change
// leaves alone, over its period.
type leftAlone struct {
	o verdict.Options
	// by are the ledger's catches of each incident in the period, by its
	// id and start.
	by map[caughtAt][]verdict.Catch
	// changed, same: the names of the rules the change takes away or
	// brings, and those it leaves alike; bases, the names of all of them
	// before what told them apart; running, those of the rules serve runs
	// now, of its latest version, and those whose firings it copies in.
	changed, same, bases, running map[string]bool
	// unreplayable says a rule the pull request leaves alone is one serve
	// couldn't replay the last time it did: its record holds nothing of it
	// from then.
	unreplayable bool
	// runsFrom and cursor are where serve's runs began and have got to.
	runsFrom, cursor time.Time
	// lacking are the spans of the versions of the rules in effect over the
	// period that lack a rule the pull request leaves alone, and failed
	// those of the runs that couldn't replay one; versions says the ledger
	// has a version in effect over the period at all.
	lacking, failed [][2]time.Time
	versions        bool
}

type caughtAt struct {
	id    string
	start int64
}

// leftAloneRecord reads what leftAlone holds: the incidents of [from, to)
// and the rules, runs and versions of the ledger, each once.
func (s *Server) leftAloneRecord(ctx context.Context, pr *prepared, from, to time.Time, o verdict.Options) (*leftAlone, error) {
	a := &leftAlone{o: o, by: map[caughtAt][]verdict.Catch{}, changed: map[string]bool{}, same: map[string]bool{}, bases: map[string]bool{},
		running: map[string]bool{}}
	for _, r := range slices.Concat(pr.gone, pr.came) {
		a.changed[r.Name], a.bases[nameBase(r.Name)] = true, true
	}
	for _, r := range pr.same {
		a.same[r.Name], a.bases[nameBase(r.Name)] = true, true
	}
	incs, err := s.l.Incidents(ctx, from, to, false)
	if err != nil {
		return nil, err
	}
	for _, in := range incs {
		a.by[caughtAt{in.ID, in.Start.Unix()}] = in.CaughtBy
	}
	latest, ok, err := s.l.Latest(ctx)
	if err != nil {
		return nil, err
	}
	if ok {
		for _, n := range latest.Names {
			a.running[n] = true
		}
	}
	table, err := s.l.RuleTable(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range table {
		if r.Source != "" {
			a.running[r.Name] = true
		}
		if a.same[r.Name] && r.Unsupported != "" {
			a.unreplayable = true
		}
	}
	if len(a.same) == 0 {
		return a, nil // nothing of the rest is asked
	}
	if a.runsFrom, err = s.l.RunsFrom(ctx); err != nil {
		return nil, err
	}
	if a.cursor, err = s.l.Cursor(ctx); err != nil {
		return nil, err
	}
	if a.cursor.IsZero() {
		return a, nil // no run: nothing is covered
	}
	// From the longest lead before the period to the cursor: every catch
	// window the record covers of an incident in it.
	first := from.Add(-o.MaxLead())
	vs, err := s.l.Versions(ctx, first, a.cursor, false)
	if err != nil {
		return nil, err
	}
	a.versions = len(vs) > 0
	for i, v := range vs {
		names := make(map[string]bool, len(v.Names))
		for _, n := range v.Names {
			names[n] = true
		}
		for n := range a.same {
			if !names[n] {
				end := a.cursor.Add(time.Second)
				if i+1 < len(vs) {
					end = vs[i+1].Since
				}
				a.lacking = append(a.lacking, [2]time.Time{v.Since, end})
				break
			}
		}
	}
	runs, err := s.l.RunsFailing(ctx, first, a.cursor.Add(time.Second))
	if err != nil {
		return nil, err
	}
	for _, r := range runs {
		for n := range r.Rules {
			if a.same[n] {
				a.failed = append(a.failed, [2]time.Time{r.From, r.To})
				break
			}
		}
	}
	return a, nil
}

// catches says whether, by serve's record, a rule the change leaves alone
// caught in, and whether the record says so at all (verdict.LeftAlone).
func (a *leftAlone) catches(in incident.Incident) (caught, known bool) {
	known = true
	for _, k := range a.by[caughtAt{in.ID, in.Start.Unix()}] {
		switch {
		case a.changed[k.Rule] && a.same[k.Rule]:
			// Two rules of one name, group and labels, one changed and one
			// left alone, which the ledger counts as one: either caught it.
			known = false
		case a.changed[k.Rule]:
			// The rule as it was, which the backtest judged.
		case a.same[k.Rule]:
			return true, true
		case a.bases[nameBase(k.Rule)]:
			// Told apart from a rule the pull request holds, or one it is
			// told apart from: maybe the rule the change changes.
			known = false
		case a.running[k.Rule]:
			return true, true
		default:
			// A rule serve runs no longer, which the pull request doesn't
			// hold: it catches nothing after the change.
		}
	}
	if !known {
		return false, false
	}
	if len(a.same) == 0 {
		// No rule the pull request leaves alone: serve's own rules beside
		// it, which lapilli verdict wouldn't know of, can only keep an
		// incident caught, never leave it not known.
		return false, true
	}
	// The catch window, as the ledger works it out (its window): from the
	// longest lead before the start to the end, or the start and
	// --unknown-length.
	from, to := in.Start.Add(-a.o.MaxLead()), in.EndOr(a.o.UnknownLen)
	if a.unreplayable || !a.versions || a.runsFrom.IsZero() || a.runsFrom.After(from) || a.cursor.Before(to) {
		return false, false
	}
	meets := func(span [2]time.Time) bool { return !span[0].After(to) && span[1].After(from) }
	if slices.ContainsFunc(a.lacking, meets) || slices.ContainsFunc(a.failed, meets) {
		return false, false
	}
	return false, true
}

// nameBase is a rule's name before what told it apart from another of the
// same name (source.TellApart: "Errors [shop]", "Errors [severity=page]").
func nameBase(name string) string {
	if i := strings.Index(name, " ["); i > 0 && strings.HasSuffix(name, "]") {
		return name[:i]
	}
	return name
}
