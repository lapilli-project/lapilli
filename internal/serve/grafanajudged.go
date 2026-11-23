package serve

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
	"lapilli.dev/internal/verdict"
)

// The rules --grafana holds that Lapilli can't replay, judged from what
// Grafana's own alert state history says they fired
// (docs/design/grafanajudged.md): each round, the history since the last
// copy is copied in under ledger.GrafanaSource, a day at a time, as a job
// copies a cloud's alarms in (docs/design/recorded.md), and each such rule
// is one rule with its history, as a cloud's alarm is with its own.

// maxCopyDays is how many days of the history one round copies in: a
// first start's backfill is copied over a few rounds.
const maxCopyDays = 7

// judgedBack is how far before a day copied in the history is read back
// from at most, for a rule judged again from it: to where it was left out.
const judgedBack = 7 * 24 * time.Hour

// fromHistory are the rules of the version in effect now that are judged
// from Grafana's history, each under the name it is judged under, and all
// the rules of that version's documents, --rule or not: what a name in
// the history may be of. twice are those of the judged rules whose name
// the version gives two rules of different titles: which of them the
// history's firings are of isn't known.
func (s *Server) fromHistory(ctx context.Context) (judged, all []rules.Rule, twice map[string]bool, err error) {
	if s.cfg.JudgedHistory == nil || s.cfg.Grafana == "" {
		return nil, nil, nil, nil
	}
	at := s.now()
	vs, err := s.versions(ctx, at, at)
	if err != nil || len(vs) == 0 {
		return nil, nil, nil, err
	}
	v := vs[len(vs)-1]
	for _, r := range v.Rules {
		if r.FromHistory() && r.Source == s.cfg.Grafana {
			judged = append(judged, r)
		}
	}
	if len(judged) == 0 {
		return nil, nil, nil, nil
	}
	all, names, err := s.named(ctx, v.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	// The history names a rule by its title: a rule judged under a name
	// made to tell it apart, from another of its title or from a rule read
	// elsewhere, is read by the title it has in Grafana (rules.Rule.Alert).
	titles := make(map[string]string, len(all))
	twice = map[string]bool{}
	for i, r := range all {
		if i < len(names) && names[i] != "" {
			if t, ok := titles[names[i]]; ok && t != r.AlertName() {
				twice[names[i]] = true
			}
			titles[names[i]] = r.AlertName()
		}
	}
	for i, r := range judged {
		if title, ok := titles[r.Name]; ok && title != r.AlertName() && !twice[r.Name] {
			judged[i].Alert = title
		}
	}
	return judged, all, twice, nil
}

// everRules are the Grafana rules of every version in effect over [from,
// to], before any --rule, as each version's documents read: what a name
// the history gives across a read may have been of. Kept by version, as a
// version doesn't change, of each rule its uid and title alone.
func (s *Server) everRules(ctx context.Context, from, to time.Time) ([]rules.Rule, error) {
	vs, err := s.l.Versions(ctx, from, to, false)
	if err != nil {
		return nil, err
	}
	var out []rules.Rule
	for _, v := range vs {
		k := fmt.Sprint(s.cfg.Ledger, "\x00", s.cfg.LedgerURLFile, "\x00", v.ID, "\x00", v.Digest)
		everRead.Lock()
		rs, ok := everRead.m[k]
		everRead.Unlock()
		if !ok {
			all, _, err := s.named(ctx, v.ID)
			if err != nil {
				return nil, err
			}
			rs = []rules.Rule{}
			for _, r := range all {
				if r.UID != "" {
					rs = append(rs, rules.Rule{Name: r.Name, Alert: r.Alert, UID: r.UID})
				}
			}
			everRead.Lock()
			if len(everRead.m) >= 128 {
				clear(everRead.m)
			}
			everRead.m[k] = rs
			everRead.Unlock()
		}
		out = append(out, rs...)
	}
	return out, nil
}

// everRead keeps what everRules read of each version.
var everRead = struct {
	sync.Mutex
	m map[string][]rules.Rule
}{m: map[string][]rules.Rule{}}

// noteFromHistory keeps, before a run, the names of the rules judged from
// Grafana's history, which the run, a new version and each copy tie to
// the history copied in for them; none, when it isn't read.
func (s *Server) noteFromHistory(ctx context.Context) error {
	rs, _, _, err := s.fromHistory(ctx)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(rs))
	for _, r := range rs {
		names = append(names, r.Name)
	}
	return s.l.SetFromHistory(ctx, names)
}

// copyFromHistory copies in what Grafana's alert state history says the
// rules judged by it fired, from where the copies got to, or else where
// the runs began, up to the runs' cursor: a day at a time, maxCopyDays a
// round. A day the history holds no change up to (it begins after it,
// kept so far back no more, or not yet) is passed over, and so is one it
// is of no use for (refused, more than is read at once, answers that
// can't be used, more firings than a write holds, more rules to leave out
// than the ledger keeps, or none of the rules it can say all of): left a
// gap, not taken for a quiet day, logged, and the day after copied as a
// first one is. A rule it can't say all of, while it can of another, is
// left out of the day alone (copyDay). Only a failure worth asking again
// fails the round, which asks again the next. A source of its name a job
// wrote to before isn't written over.
func (s *Server) copyFromHistory(ctx context.Context, o verdict.Options) error {
	rs, all, twice, err := s.fromHistory(ctx)
	if err != nil || len(rs) == 0 {
		return err
	}
	if theirs, err := s.sourceTheirs(ctx); err != nil || theirs {
		return err
	}
	hw, err := s.historyWhere(ctx)
	if err != nil || hw.Serves == "" {
		return err
	}
	cursor, err := s.l.Cursor(ctx)
	if err != nil || cursor.IsZero() {
		return err
	}
	_, a, err := s.l.Coverage(ctx, ledger.GrafanaSource)
	if err != nil {
		return err
	}
	s.status.Lock()
	past := s.status.copyFrom
	s.status.Unlock()
	// The first day, or the first after a gap: alerts on at its start are
	// read from a day before it.
	first := a.IsZero() || past.After(a)
	if a.IsZero() {
		if a, err = s.l.RunsFrom(ctx); err != nil || a.IsZero() {
			return err
		}
	}
	if past.After(a) {
		a = past
	}
	a = a.Truncate(time.Second)
	skip := func(to time.Time) {
		s.status.Lock()
		s.status.copyFrom = to
		s.status.Unlock()
	}
	for range maxCopyDays {
		b := a.Add(24 * time.Hour)
		if cursor.Before(b) {
			b = cursor.Truncate(time.Second)
		}
		if !b.After(a) {
			return nil
		}
		reaches, err := s.cfg.JudgedHistory.Reaches(ctx, hw.Serves, b)
		if err == nil && reaches {
			s.status.set("copying in what Grafana's alert state history says its rules fired, %s", a.In(s.cfg.Loc).Format("2006-01-02 15:04"))
			err = s.copyDay(ctx, hw.Serves, rs, all, twice, a, b, first, o)
		}
		var invalid ledger.ErrInvalid
		switch {
		case err == nil && !reaches:
			skip(b)
			first = true
		case errors.Is(err, source.ErrUnusable) || errors.Is(err, ledger.ErrLeftOutFull) || errors.As(err, &invalid):
			s.log.Warn("Grafana's alert state history of the rules Lapilli can't replay wasn't copied in for a day: it is left a gap",
				"from", a, "to", b, "err", err)
			skip(b)
			first = true
		case err != nil:
			return err
		default:
			first = false
		}
		a = b
	}
	return nil
}

// sourceTheirs reports whether the source Grafana's history is copied in
// under was written by a job, as one could before 0.68: it isn't serve's
// to write over, and it says so once.
func (s *Server) sourceTheirs(ctx context.Context) (bool, error) {
	srcs, err := s.l.Sources(ctx)
	if err != nil {
		return false, err
	}
	i := slices.IndexFunc(srcs, func(x ledger.Source) bool { return x.Name == ledger.GrafanaSource })
	if i < 0 || srcs[i].By == ledger.GrafanaCopier {
		return false, nil
	}
	s.status.Lock()
	said := s.status.copyTheirs
	s.status.copyTheirs = true
	s.status.Unlock()
	if !said {
		s.log.Error("Grafana's alert state history isn't copied in: a job copied firings in under the source grafana, now lapilli serve's own, before 0.68 (docs/upgrading.md)",
			"written_by", srcs[i].By)
	}
	return true, nil
}

// copyDay copies in what the history says the rules fired from a to b:
// on the first day, read from a day before a (source.JudgedLead), each
// firing on at a from a; after it, the settle before a read and written
// again, so a change Grafana kept late is seen. A firing starting in the
// write is written as the history has it. One on since before it that the
// ledger has as still on is ended where the history says it ended, or
// where it fired again: written again from its start, with all the ledger
// has from there as it is.
//
// A rule the history can't say all it fired of is left out of the day
// alone: one whose name the history gives another of Grafana's rules too,
// where it doesn't say which fired (source.GrafanaHistory.Judge), one it
// passed over changes of or doesn't tell the alerts of apart, one with a
// firing a write can't hold, one whose name twice holds. Nothing of it is
// written or taken away, an alert of it on stays on, and the period is
// kept for it (ledger.LeaveOut): no record of it there, whatever is copied
// in for the other rules, and whatever the history says of the period
// later. Judged again, it is read back from where it was left out, a week
// at most (judgedBack), and written from a as on a first day; one of its
// alerts on since before then, that the read says nothing of, is ended
// where it was left out. A day none of the rules is judged for is of no
// use, and left a gap.
func (s *Server) copyDay(ctx context.Context, where string, rs, all []rules.Rule, twice map[string]bool, a, b time.Time, first bool, o verdict.Options) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	// The periods each rule was left out of the copies before.
	out, err := s.l.LeftOut(ctx, ledger.GrafanaSource)
	if err != nil {
		return err
	}
	inHole := func(rule string, t time.Time) bool {
		return slices.ContainsFunc(out[rule], func(h [2]time.Time) bool { return !t.Before(h[0]) && t.Before(h[1]) })
	}
	// The rules left out up to a, from when: judged again from a, each is
	// read as on a first day, an alert of it on at a written from there.
	back := map[string]time.Time{}
	for _, r := range rs {
		for _, h := range out[r.Name] {
			if h[0].Before(a) && !h[1].Before(a) {
				back[r.Name] = h[0]
			}
		}
	}
	// Read from just before the write, so an alert on since before it,
	// on at the read's start, isn't taken for one that started with it,
	// at an evaluation on the second.
	p := a.Add(-s.cfg.Settle).Truncate(time.Second)
	read := p.Add(-time.Millisecond)
	if first {
		p, read = a, a.Add(-source.JudgedLead)
	}
	// The rules of every version in effect across what is written, from
	// the settle before it, for a history that doesn't say which rule each
	// change is of: a name two of them gave is neither's. Not across the
	// day a first day is read from besides, which would leave a rule out
	// for a day once its title was shared.
	ever, err := s.everRules(ctx, p, b)
	if err != nil {
		return err
	}
	judge := func(read time.Time) (source.FromHistory, error) {
		return s.cfg.JudgedHistory.Judge(ctx, where, rs, all, ever, read, b)
	}
	j, err := judge(read)
	if err != nil {
		return err
	}
	// A rule judged again is read again from where it was left out, a day
	// before a at least, a week at most: what began while it was left out
	// and is on still is seen with its start, and what was on then and
	// ended meanwhile, with its end. Past a week, its alerts on since
	// before it was left out are ended there, as what the read says
	// nothing of (past). The rules a name may be of are those of the read
	// first made: what the read again adds is of the period the rule was
	// left out, no record of it.
	left := func(j source.FromHistory, name string) bool {
		return slices.Contains(j.Unsure, name) || slices.Contains(j.Shared, name) || twice[name]
	}
	lead := a.Add(-source.JudgedLead)
	past := map[string]time.Time{}
	// readOf is where the read a rule was judged by began, read but for
	// those of the read from a day before a.
	readOf := map[string]time.Time{}
	far := read
	for name, h0 := range back {
		if left(j, name) {
			continue
		}
		from := lead
		if t := h0.Add(-time.Millisecond); t.Before(from) {
			from = t
		}
		if limit := a.Add(-judgedBack); from.Before(limit) {
			from, past[name] = limit, h0
		}
		if from.Before(far) {
			far = from
		}
	}
	if far.Before(read) {
		again, err := judge(far)
		if err != nil {
			return err
		}
		// What the read again holds of the period a rule was left out may
		// be what left it out (a change passed over): such a rule, judged by
		// the first read and unsure by this one, is judged by a read from a
		// day before a alone, as what is further back is past what is read,
		// so that it isn't left out for as long as a read holds that change.
		var lost []string
		for _, r := range rs {
			if !left(j, r.Name) && slices.Contains(again.Unsure, r.Name) {
				lost = append(lost, r.Name)
			}
		}
		near := lead
		if read.Before(near) {
			near = read
		}
		if len(lost) > 0 && near.After(far) {
			alone, err := judge(near)
			if err != nil {
				return err
			}
			again = mergeJudged(again, alone, lost)
			for _, name := range lost {
				readOf[name] = near
				if h0, ok := back[name]; ok && h0.Add(-time.Millisecond).Before(near) {
					past[name] = h0
				}
			}
		}
		j, read = again, far
	}
	readFrom := func(rule string) time.Time {
		if t, ok := readOf[rule]; ok {
			return t
		}
		return read
	}
	if j.Passed > 0 {
		s.log.Warn("changes of state in Grafana's alert state history passed over, past what lapilli reads of one", "changes", j.Passed)
	}
	for _, r := range rs {
		if twice[r.Name] && !slices.Contains(j.Shared, r.Name) {
			j.Shared = append(j.Shared, r.Name)
		}
	}
	// The rules left out of the day. A firing a write can't hold (more
	// than 32 labels, labels past 4 KB, a name that doesn't show) leaves
	// its rule out where it would be written, starting in the write or on
	// at a, as it would refuse the write; looked at before anything is
	// made of its labels. One over before the write is of nothing the
	// ledger has, and nothing writes it: it is dropped alone, so that a read
	// back from where its rule was left out, which holds it, doesn't leave
	// the rule out again.
	leave := map[string]bool{}
	for _, n := range slices.Concat(j.Unsure, j.Shared) {
		leave[n] = true
	}
	var unheld []string
	j.Firings = slices.DeleteFunc(j.Firings, func(x firing.Firing) bool {
		switch {
		case leave[x.Rule] || recordable(x) == nil:
			return false
		case x.Start.Before(p) && !x.End.IsZero() && !x.End.After(a):
			return true
		}
		leave[x.Rule] = true
		unheld = append(unheld, x.Rule)
		return false
	})
	if len(leave) > 0 {
		// A name the ledger keeps no rule copied in by has no record to
		// be left out of: no firing of it was ever written.
		names := slices.DeleteFunc(slices.Sorted(maps.Keys(leave)), func(n string) bool { return !ledger.RuleName(n) })
		// The rules of the source now, whose periods are kept however many
		// rules have some: those judged, and every Grafana rule, paused
		// ones among them, by its title.
		now := make([]string, 0, len(rs)+len(all))
		for _, r := range rs {
			now = append(now, r.Name)
		}
		for _, r := range all {
			if r.UID != "" {
				now = append(now, r.Name, r.AlertName())
			}
		}
		if err := s.l.LeaveOut(context.WithoutCancel(ctx), ledger.GrafanaSource, names, now, a, b); err != nil {
			return err
		}
		why := leftOutWhy(j.Unsure, j.Shared, unheld)
		if len(leave) >= len(rs) {
			return fmt.Errorf("%w: %s", source.ErrUnusable, why)
		}
		s.log.Warn("rules judged from Grafana's alert state history were left out of a day copied in: they have no record for it",
			"from", a, "to", b, "why", why)
	}
	// A rule judged so no more, paused, deleted or renamed, keeps what was
	// copied in for it, and so does one left out: the write is of the
	// rules judged now alone.
	judging := make(map[string]bool, len(rs))
	for _, r := range rs {
		judging[r.Name] = !leave[r.Name]
	}
	fs := slices.DeleteFunc(j.Firings, func(x firing.Firing) bool { return leave[x.Rule] })
	if len(fs) > ledger.MaxRecorded {
		return fmt.Errorf("%w: more than %d firings in a day", source.ErrTooManyChanges, ledger.MaxRecorded)
	}
	open, err := s.l.OpenFirings(ctx, ledger.GrafanaSource, p)
	if err != nil {
		return err
	}
	had, err := s.l.RecordedFirings(ctx, ledger.GrafanaSource, p, b)
	if err != nil {
		return err
	}
	// The alerts the ledger has on since before the write (on); and its
	// firings from within it (within), on or ended, where a round before
	// wrote one on since a gap, or a period its rule was left out, from
	// where the copies covered it again: one that ended before a round read
	// past where the history has it begin is one of them too.
	on, within := map[[32]byte]firing.Firing{}, map[[32]byte][]firing.Firing{}
	for _, f := range open {
		if judging[f.Rule] {
			on[alertOf(f)] = f
		}
	}
	for _, f := range had {
		if judging[f.Rule] && !inHole(f.Rule, f.Start) {
			k := alertOf(f)
			within[k] = append(within[k], f)
		}
	}
	// told are the alerts on within the write the history tells of at all:
	// one it doesn't changed state in none of what was read, and is on as
	// it was.
	told := map[[32]byte]bool{}
	ended := map[[32]byte]time.Time{}
	var send []firing.Firing
	for _, f := range fs {
		var k [32]byte
		var was, in firing.Firing
		isOn, isIn := false, false
		if len(on)+len(within) > 0 {
			k = alertOf(f)
			was, isOn = on[k]
			if len(within[k]) > 0 {
				told[k] = true
			}
		}
		// The firing the ledger has from within the write that one on since
		// before it is: the first it was on over the start of.
		if f.Start.Before(p) {
			for _, x := range within[k] {
				if (f.End.IsZero() || x.Start.Before(f.End)) && (!isIn || x.Start.Before(in.Start)) {
					in, isIn = x, true
				}
			}
		}
		// The firing the ledger has on: the one on over its start, which
		// the ledger may have from where the copies covered it again; or,
		// on since before what was read, on at its start.
		same := isOn && (!f.Start.Truncate(time.Second).After(was.Start) && (f.End.IsZero() || f.End.After(was.Start)) ||
			f.Start.Equal(readFrom(f.Rule)) && was.Start.Before(readFrom(f.Rule)))
		switch {
		case !f.Start.Before(b):
		case same:
			told[k] = true
			if _, done := ended[k]; !done && !f.End.IsZero() && f.End.After(was.Start) {
				ended[k] = f.End
			}
		case isIn:
			// On since before the write, and the ledger has it from within
			// it, on or ended: that firing, from where the ledger has it, as
			// the history says it is. Not taken away by a read that begins
			// after where the history has it begin.
			f.Start = in.Start
			send = append(send, f)
		case !f.Start.Before(p) || (first || !back[f.Rule].IsZero()) && (f.End.IsZero() || f.End.After(a)):
			// Firing again, it had stopped: where, the history didn't say.
			if _, done := ended[k]; isOn && !done {
				ended[k] = f.Start.Truncate(time.Second)
			}
			// On at a since before the write, on a first day or its rule's:
			// written from a.
			if f.Start.Before(p) {
				f.Start = a
			}
			send = append(send, f)
		}
	}
	// An alert of a rule judged again, read back a week alone, on since
	// before it was left out, that the read says nothing of: ended where it
	// was left out, as whether it was on after isn't known.
	for k, f := range on {
		if h0, ok := past[f.Rule]; ok && judging[f.Rule] && f.Start.Before(h0) && !told[k] {
			if _, done := ended[k]; !done {
				ended[k] = h0
			}
		}
	}
	// A firing ended is written again from its start, with all the ledger
	// has from there, as it is: no further back than a write reaches, nor
	// past as many firings as it holds.
	from := p
	for k := range ended {
		if st := on[k].Start; st.Before(from) && b.Sub(st) <= ledger.MaxRecordedPeriod {
			from = st
		}
	}
	if from.Before(p) {
		n, err := s.l.RecordedCount(ctx, ledger.GrafanaSource, from, p)
		if err != nil {
			return err
		}
		if n+len(send) > ledger.MaxRecorded {
			s.log.Warn("firings Grafana's alert state history says ended, on since long before, are left on: a write from their start would hold too many",
				"from", from, "firings", n)
			from = p
		}
	}
	var before []firing.Firing
	// closed are the starts, to the second, of those of before ended now.
	closed := map[int64]bool{}
	if from.Before(p) {
		if before, err = s.l.RecordedFirings(ctx, ledger.GrafanaSource, from, p); err != nil {
			return err
		}
		for i, x := range before {
			if !x.End.IsZero() || !judging[x.Rule] {
				continue
			}
			k := alertOf(x)
			if end, ok := ended[k]; ok && x.Start.Equal(on[k].Start) {
				before[i].End = end
				closed[x.Start.Unix()] = true
			}
		}
	}
	// What the ledger has in the write stays as it is for a rule the write
	// isn't of, over a period a rule was left out, and for an alert on
	// that the history tells nothing of.
	var kept []firing.Firing
	for _, x := range had {
		if !judging[x.Rule] || inHole(x.Rule, x.Start) || x.End.IsZero() && !told[alertOf(x)] {
			kept = append(kept, x)
		}
	}
	// What is written before the day is what the copies cover of it, span
	// by span as the ledger keeps them, and the day itself from a: a firing
	// ended, written again from its start, is written within the spans it
	// lies across, and a gap between them stays one, as what began in it
	// was never read whole; a change the history gave in a gap is left out
	// with it, but for an alert still on after it.
	spans, err := s.l.Spans(ctx, ledger.GrafanaSource)
	if err != nil {
		return err
	}
	writes := coveredWrites(spans, from, a, b)
	of := make([][]firing.Firing, len(writes))
	place := func(x firing.Firing) bool {
		for i, w := range writes {
			if !x.Start.Before(w[0]) && x.Start.Before(w[1]) {
				of[i] = append(of[i], x)
				return true
			}
		}
		return false
	}
	// What the ledger has is written where it starts. One that starts in
	// no span the copies cover is in one the ledger let go, past as many
	// as it keeps: it stays as it is, but that one ended now is ended
	// where it is, in a write of the second it started in that isn't taken
	// as covered again (ledger.AmendFirings).
	amend := map[int64][]firing.Firing{}
	for _, x := range before {
		if !place(x) && closed[x.Start.Unix()] {
			amend[x.Start.Unix()] = append(amend[x.Start.Unix()], x)
		}
	}
	for _, x := range kept {
		place(x)
	}
	// What the history gives is written where the copies cover its rule:
	// the spans, but for the periods the rule was left out. A firing that
	// began in a gap, or in such a period, still on where the copies cover
	// its rule again, is written from there, as the first day after writes
	// one on at its start: not left out, which would take away the copy of
	// it a round before made from there.
	allowed := map[string][][2]time.Time{}
	for _, x := range send {
		ws := writes
		if hs := out[x.Rule]; len(hs) > 0 {
			var ok bool
			if ws, ok = allowed[x.Rule]; !ok {
				ws = ledger.Without(writes, hs)
				allowed[x.Rule] = ws
			}
		}
		for _, w := range ws {
			if x.Start.Before(w[0]) {
				if x.End.IsZero() || x.End.After(w[0]) {
					x.Start = w[0]
					place(x)
				}
				break
			}
			if x.Start.Before(w[1]) {
				place(x)
				break
			}
		}
	}
	for _, at := range slices.Sorted(maps.Keys(amend)) {
		st := time.Unix(at, 0).UTC()
		got, err := s.l.AmendFirings(context.WithoutCancel(ctx), ledger.GrafanaSource, st, st.Add(time.Second), amend[at], ledger.GrafanaCopier, o)
		if err != nil {
			return err
		}
		if got.Changed() {
			s.log.Info("a firing Grafana's alert state history says ended was ended where it is, in a span the copies no longer cover", "start", st)
		}
	}
	for i, w := range writes {
		fs := of[i]
		got, err := s.l.PutFirings(context.WithoutCancel(ctx), ledger.GrafanaSource, w[0], w[1], fs, ledger.GrafanaCopier, o)
		if err != nil {
			return err
		}
		if got.Changed() {
			s.log.Info("Grafana's alert state history copied in", "from", w[0], "to", w[1], "firings", got.Firings, "added", got.Added, "removed", got.Removed)
		}
	}
	return nil
}

// mergeJudged is what j says, but for the rules of names, which alone
// says of.
func mergeJudged(j, alone source.FromHistory, names []string) source.FromHistory {
	of := func(n string) bool { return slices.Contains(names, n) }
	out := source.FromHistory{Passed: max(j.Passed, alone.Passed)}
	for _, x := range [2]struct {
		h    source.FromHistory
		mine bool
	}{{j, false}, {alone, true}} {
		for _, f := range x.h.Firings {
			if of(f.Rule) == x.mine {
				out.Firings = append(out.Firings, f)
			}
		}
		for _, n := range x.h.Unsure {
			if of(n) == x.mine {
				out.Unsure = append(out.Unsure, n)
			}
		}
		for _, n := range x.h.Shared {
			if of(n) == x.mine {
				out.Shared = append(out.Shared, n)
			}
		}
	}
	firing.Sort(out.Firings)
	slices.Sort(out.Unsure)
	slices.Sort(out.Shared)
	return out
}

// leftOutWhy says why rules are left out of a day copied in, for a log:
// those the history can't say all of (unsure), those whose name another
// of Grafana's rules has too (shared), and those with a firing a write
// can't hold (unheld).
func leftOutWhy(unsure, shared, unheld []string) string {
	var why []string
	if len(unsure) > 0 {
		why = append(why, fmt.Sprintf("it passed over changes of %d of the rules, or doesn't tell their alerts apart: %s", len(unsure), someNames(unsure)))
	}
	if len(shared) > 0 {
		why = append(why, fmt.Sprintf("it gives the name of %d of the rules to another of Grafana's rules too, and doesn't say which fired: %s", len(shared), someNames(shared)))
	}
	if len(unheld) > 0 {
		why = append(why, fmt.Sprintf("a firing of %d of the rules is one a write can't hold: %s", len(unheld), someNames(unheld)))
	}
	return strings.Join(why, "; ")
}

// coveredWrites are the periods a copy of [from, b) writes, the day it
// copies beginning at a: before a, those of spans, what the copies cover,
// within [from, a); then [a, b), joined to the last when it ends at a.
func coveredWrites(spans [][2]time.Time, from, a, b time.Time) [][2]time.Time {
	var out [][2]time.Time
	for _, sp := range spans {
		lo, hi := sp[0], sp[1]
		if lo.Before(from) {
			lo = from
		}
		if hi.After(a) {
			hi = a
		}
		if lo.Before(hi) {
			out = append(out, [2]time.Time{lo, hi})
		}
	}
	if a.Before(b) {
		if n := len(out); n > 0 && out[n-1][1].Equal(a) {
			out[n-1][1] = b
		} else {
			out = append(out, [2]time.Time{a, b})
		}
	}
	return out
}

// someNames are the first few of names, for a log: as Grafana titles
// them, cut.
func someNames(names []string) string {
	shown := names[:min(len(names), 5)]
	out := verdict.Clean(clip(strings.Join(shown, ", "), 300))
	if n := len(names) - len(shown); n > 0 {
		out += fmt.Sprintf(" and %d more", n)
	}
	return out
}

// recordable is ledger.Recordable; replaced in tests, to count.
var recordable = ledger.Recordable

// alertOf is the alert a firing is of, its rule and its labels, as a
// hash: what is kept of each is small, however long its labels. Replaced
// in tests, to count.
var alertOf = func(f firing.Firing) [32]byte {
	b, _ := json.Marshal(f.Labels) // in the order of their names
	return sha256.Sum256(append([]byte(f.Rule+"\x00"), b...))
}
