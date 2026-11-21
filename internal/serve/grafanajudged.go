package serve

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
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

// fromHistory are the rules of the version in effect now that are judged
// from Grafana's history, each under the name it is judged under, and all
// the rules of that version's documents, --rule or not: what a name in
// the history may be of.
func (s *Server) fromHistory(ctx context.Context) (judged, all []rules.Rule, err error) {
	if s.cfg.JudgedHistory == nil || s.cfg.Grafana == "" {
		return nil, nil, nil
	}
	at := s.now()
	vs, err := s.versions(ctx, at, at)
	if err != nil || len(vs) == 0 {
		return nil, nil, err
	}
	v := vs[len(vs)-1]
	for _, r := range v.Rules {
		if r.FromHistory() && r.Source == s.cfg.Grafana {
			judged = append(judged, r)
		}
	}
	if len(judged) == 0 {
		return nil, nil, nil
	}
	all, _, err = s.named(ctx, v.ID)
	return judged, all, err
}

// noteFromHistory keeps, before a run, the names of the rules judged from
// Grafana's history, which the run, a new version and each copy tie to
// the history copied in for them; none, when it isn't read.
func (s *Server) noteFromHistory(ctx context.Context) error {
	rs, _, err := s.fromHistory(ctx)
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
// can't be used, changes of the rules it passed over or alerts it doesn't
// tell apart, firings a write can't hold): left a gap, not taken for a
// quiet day, logged, and the day after copied as a first one is. Only a
// failure worth asking again fails the round, which asks again the next.
// A source of its name a job wrote to before isn't written over.
func (s *Server) copyFromHistory(ctx context.Context, o verdict.Options) error {
	rs, all, err := s.fromHistory(ctx)
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
			err = s.copyDay(ctx, hw.Serves, rs, all, a, b, first, o)
		}
		var invalid ledger.ErrInvalid
		switch {
		case err == nil && !reaches:
			skip(b)
			first = true
		case errors.Is(err, source.ErrUnusable) || errors.As(err, &invalid):
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
func (s *Server) copyDay(ctx context.Context, where string, rs, all []rules.Rule, a, b time.Time, first bool, o verdict.Options) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	// Read from just before the write, so an alert on since before it,
	// on at the read's start, isn't taken for one that started with it,
	// at an evaluation on the second.
	p := a.Add(-s.cfg.Settle).Truncate(time.Second)
	read := p.Add(-time.Millisecond)
	if first {
		p, read = a, a.Add(-source.JudgedLead)
	}
	j, err := s.cfg.JudgedHistory.Judge(ctx, where, rs, all, read, b)
	if err != nil {
		return err
	}
	if j.Passed > 0 {
		s.log.Warn("changes of state in Grafana's alert state history passed over, past what lapilli reads of one", "changes", j.Passed)
	}
	// A rule the history can't say all it fired of makes the day of no
	// use: taken out alone, its firings already copied in would go, and
	// the day would read as one it was quiet, as the source's record is
	// every rule's. So does one whose name the history gives another of
	// Grafana's rules too (one title in two folders), which it never says
	// which of them fired: a rule of the same title, even paused, made in
	// any folder Lapilli reads, would otherwise take its firings away.
	if len(j.Unsure) > 0 {
		return fmt.Errorf("%w: it passed over changes of %d of the rules, or doesn't tell their alerts apart: %s", source.ErrUnusable, len(j.Unsure), someNames(j.Unsure))
	}
	if len(j.Shared) > 0 {
		return fmt.Errorf("%w: it gives the name of %d of the rules to another of Grafana's rules too, and doesn't say which fired: %s", source.ErrUnusable, len(j.Shared), someNames(j.Shared))
	}
	// A firing a write can't hold (more than 32 labels, labels past 4 KB,
	// a name that doesn't show) makes the day of no use, as it would the
	// write; looked at before anything is made of its labels.
	for _, x := range j.Firings {
		if err := recordable(x); err != nil {
			return fmt.Errorf("%w: a firing a write can't hold: %v", source.ErrUnusable, err)
		}
	}
	fs := j.Firings
	if len(fs) > ledger.MaxRecorded {
		return fmt.Errorf("%w: more than %d firings in a day", source.ErrTooManyChanges, ledger.MaxRecorded)
	}
	open, err := s.l.OpenFirings(ctx, ledger.GrafanaSource, p)
	if err != nil {
		return err
	}
	on := map[[32]byte]firing.Firing{}
	for _, f := range open {
		on[alertOf(f)] = f
	}
	ended := map[[32]byte]time.Time{}
	var send []firing.Firing
	for _, f := range fs {
		var k [32]byte
		var was firing.Firing
		isOn := false
		if len(on) > 0 {
			k = alertOf(f)
			was, isOn = on[k]
		}
		switch {
		case !f.Start.Before(p) && f.Start.Before(b):
			// Firing again, it had stopped: where, the history didn't say.
			if _, done := ended[k]; isOn && !done {
				ended[k] = f.Start.Truncate(time.Second)
			}
			send = append(send, f)
		case !f.Start.Before(p):
		case isOn:
			// The firing the ledger has on: the same start, or, on since
			// before what was read, on at its start.
			same := f.Start.Truncate(time.Second).Equal(was.Start) || f.Start.Equal(read) && was.Start.Before(read)
			if _, done := ended[k]; same && !done && !f.End.IsZero() && f.End.After(was.Start) {
				ended[k] = f.End
			}
		case first && (f.End.IsZero() || f.End.After(p)):
			f.Start = p
			send = append(send, f)
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
	if from.Before(p) {
		if before, err = s.l.RecordedFirings(ctx, ledger.GrafanaSource, from, p); err != nil {
			return err
		}
		for i, x := range before {
			k := alertOf(x)
			if end, ok := ended[k]; ok && x.End.IsZero() && x.Start.Equal(on[k].Start) {
				before[i].End = end
			}
		}
	}
	// A rule judged so no more, paused, deleted or renamed, keeps what was
	// copied in for it: the write is of the rules judged now alone.
	judging := make(map[string]bool, len(rs))
	for _, r := range rs {
		judging[r.Name] = true
	}
	had, err := s.l.RecordedFirings(ctx, ledger.GrafanaSource, p, b)
	if err != nil {
		return err
	}
	var kept []firing.Firing
	for _, x := range had {
		if !judging[x.Rule] {
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
	// A firing that began in a gap, still on where the copies cover again,
	// is written from there, as the first day after a gap writes one on at
	// its start: not left out, which would take away the copy of it a
	// round before made from there.
	writes := coveredWrites(spans, from, a, b)
	of := make([][]firing.Firing, len(writes))
	for _, x := range slices.Concat(before, send, kept) {
		for i, w := range writes {
			if x.Start.Before(w[0]) {
				if x.End.IsZero() || x.End.After(w[0]) {
					x.Start = w[0]
					of[i] = append(of[i], x)
				}
				break
			}
			if x.Start.Before(w[1]) {
				of[i] = append(of[i], x)
				break
			}
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
