package ledger

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"sort"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// An alert after the incident (docs/how-it-judges.md): a black firing that
// came once an incident was over, its condition having held in it, its
// labels agreeing, is that incident's alert, too late. lapilli verdict says
// so of a report's firings (verdict.Judged.After); the ledger works it out
// as it is read, from the firings and incidents it keeps and how long before
// its start each rule's condition held (verdict.Options.Held), so nothing
// new is kept and no stone changes.

// Late are, over a span, the firings that came after an incident was over.
type Late struct {
	// Fired are, of the incidents that count that started in the span, by
	// key, the rules that didn't catch each and fired after it, each by its
	// first such firing, its delay from the incident's start: as
	// verdict.IncidentResult.FiredAfter, in order of their delay.
	Fired map[string][]verdict.Catch
	// Rules are, by rule, how many of its black firings that started in the
	// span came after an incident: verdict.RuleResult.Late.
	Rules map[string]int
	// After are, by firing id, of those, the incidents each came after, in
	// order of their start (verdict.Judged.After); only when asked for.
	After map[int64][]string
	// Cut says there was more to look at than one read looks at
	// (LateChecks, LateRows, LateSpans): what was found by then is given,
	// the rest isn't, and the caller says so where it shows them.
	Cut bool
}

// How much one read of Late looks at, at most: an incident written down
// can end anywhere, and a span read is a page's or a report's, a year's
// with no firings in it. LateChecks are firings, each against an incident
// it may have come after; LateRows, the firings read near the incidents'
// ends; LateSpans, the reads of them, one where the incidents meet.
// Replaced in tests.
var (
	LateChecks = 4_000_000
	LateRows   = 1_000_000
	LateSpans  = 10_000
)

// LateOf are the firings of [from, to) that came after an incident was
// over, and the rules that fired after each incident that started in it:
// of the incidents that count, every one the ledger has, as a firing's
// stones are given against every one; each firing judged as lapilli
// verdict judges a report's (verdict.AfterEnd), black (neither white nor
// excused), its condition held from its start less its rule's o.Held, of
// a rule that didn't catch the incident otherwise and isn't judged by
// nothing. With after, each such firing's incidents too. With no o.Held,
// none.
func (l *Ledger) LateOf(ctx context.Context, from, to time.Time, o verdict.Options, after bool) (Late, error) {
	out := Late{Fired: map[string][]verdict.Catch{}, Rules: map[string]int{}}
	if after {
		out.After = map[int64][]string{}
	}
	var most time.Duration
	for _, d := range o.Held {
		most = max(most, d)
	}
	if most <= 0 || !to.After(from) {
		return out, nil
	}
	// The incidents that count that started before the span's end, and
	// ended no further before its start than a rule's condition held: those
	// a firing in it can have come after, and those that started in it.
	unknown := int64(o.UnknownLen / time.Second)
	rows, err := l.r.QueryContext(ctx, `SELECT `+incidentRowCols+` FROM incidents
		WHERE counts = 1 AND start_at < ? AND coalesce(end_at, start_at + ?) >= ? ORDER BY start_at, key`,
		to.Unix(), unknown, from.Add(-most).Unix()-1)
	if err != nil {
		return out, err
	}
	var ins []ended
	for rows.Next() {
		r, err := scanIncident(rows)
		if err != nil {
			rows.Close()
			return out, err
		}
		ins = append(ins, ended{row: r.row, in: r.Incident, end: r.EndOr(o.UnknownLen), order: len(ins)})
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(ins) == 0 {
		return out, err
	}
	caught, err := l.caughtRules(ctx, ins)
	if err != nil {
		return out, err
	}
	// In order of their end: a firing came after those that ended before
	// it, no longer before it than its rule's condition held.
	slices.SortStableFunc(ins, func(a, b ended) int { return a.end.Compare(b.end) })
	fired := map[string]map[string]time.Duration{}
	checks, read, spans := 0, 0, 0
	// The spans the firings are read in, those of the incidents that meet
	// as one, so that each firing is read once.
	for i := 0; i < len(ins) && !out.Cut; {
		if spans++; spans > LateSpans {
			out.Cut = true
			break
		}
		lo, hi := ins[i].end, ins[i].end.Add(most)
		j := i + 1
		for j < len(ins) && !ins[j].end.After(hi) {
			hi = ins[j].end.Add(most)
			j++
		}
		span := ins[i:j]
		i = j
		rows, err := l.r.QueryContext(ctx, `SELECT f.id, r.name, f.series_id, s.labels, f.start_at FROM firings f
			JOIN series s ON s.id = f.series_id JOIN rules r ON r.id = f.rule_id
			WHERE f.start_at > ? AND f.start_at <= ? AND f.white = 0 AND f.excused = 0 ORDER BY f.start_at, f.id`, lo.Unix(), hi.Unix())
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var id, sid, start int64
			var name, text string
			if err := rows.Scan(&id, &name, &sid, &text, &start); err != nil {
				rows.Close()
				return out, err
			}
			if read++; read > LateRows {
				out.Cut = true
				break
			}
			h := o.Held[name]
			if h <= 0 || o.NotJudged(name) {
				continue
			}
			f := firing.Firing{Rule: name, Start: unix(start)}
			held := f.Start.Add(-h)
			// Those that ended in [held, start): from the first that ended
			// at held or after.
			k := sort.Search(len(span), func(k int) bool { return !span[k].end.Before(held) })
			var of []ended
			for ; k < len(span) && span[k].end.Before(f.Start); k++ {
				n := span[k]
				if caught[n.row][name] {
					continue
				}
				if checks++; checks > LateChecks {
					out.Cut = true
					break
				}
				if f.Labels == nil {
					if f.Labels, err = l.labels(sid, text); err != nil {
						rows.Close()
						return out, err
					}
				}
				if !verdict.AfterEnd(f, held, n.in, o) {
					continue
				}
				of = append(of, n)
				if !n.in.Start.Before(from) && n.in.Start.Before(to) {
					d := f.Start.Sub(n.in.Start)
					m := fired[n.in.ID]
					if m == nil {
						m = map[string]time.Duration{}
						fired[n.in.ID] = m
					}
					if was, ok := m[name]; !ok || d < was {
						m[name] = d
					}
				}
			}
			if len(of) > 0 && !f.Start.Before(from) && f.Start.Before(to) {
				out.Rules[name]++
				if after {
					slices.SortFunc(of, func(a, b ended) int { return cmp.Compare(a.order, b.order) })
					ids := make([]string, len(of))
					for x, n := range of {
						ids[x] = n.in.ID
					}
					out.After[id] = ids
				}
			}
			if out.Cut {
				break
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return out, err
		}
	}
	for key, m := range fired {
		cs := make([]verdict.Catch, 0, len(m))
		for rule, d := range m {
			cs = append(cs, verdict.Catch{Rule: rule, Delay: d})
		}
		sortCatches(cs)
		out.Fired[key] = cs
	}
	return out, nil
}

// ended is an incident a firing may have come after: its row, its end
// (incident.Incident.EndOr), and its place in order of start, then of key.
type ended struct {
	row   int64
	in    incident.Incident
	end   time.Time
	order int
}

// caughtRules are, by the row of each of ins, the names of the rules that
// caught it, as catches keeps them.
func (l *Ledger) caughtRules(ctx context.Context, ins []ended) (map[int64]map[string]bool, error) {
	out := map[int64]map[string]bool{}
	for len(ins) > 0 {
		part := ins[:min(len(ins), pgRows)]
		ins = ins[len(part):]
		ids := make([]int64, len(part))
		for i, n := range part {
			ids[i] = n.row
		}
		b, err := json.Marshal(ids)
		if err != nil {
			return nil, err
		}
		rows, err := l.r.QueryContext(ctx, `SELECT c.incident_id, r.name FROM catches c JOIN rules r ON r.id = c.rule_id
			WHERE c.incident_id IN (`+l.d.ints("?")+`)`, string(b))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				rows.Close()
				return nil, err
			}
			if out[id] == nil {
				out[id] = map[string]bool{}
			}
			out[id][name] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
