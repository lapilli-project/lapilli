package serve

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
// of that version's rules.
func (s *Server) fromHistory(ctx context.Context) (judged, all []rules.Rule, err error) {
	if s.cfg.JudgedHistory == nil || s.cfg.Grafana == "" {
		return nil, nil, nil
	}
	at := s.now()
	vs, err := s.versions(ctx, at, at)
	if err != nil || len(vs) == 0 {
		return nil, nil, err
	}
	all = vs[len(vs)-1].Rules
	for _, r := range all {
		if r.FromHistory() && r.Source == s.cfg.Grafana {
			judged = append(judged, r)
		}
	}
	return judged, all, nil
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
// round. Before the first copy, a day the history holds no change up to
// is passed over. A day the history is of no use for (refused, more than
// is read at once, answers that can't be used, firings a write can't
// hold) is passed over, its firings left out, and logged; only a failure
// worth asking again fails the round, which asks again the next.
func (s *Server) copyFromHistory(ctx context.Context, o verdict.Options) error {
	rs, all, err := s.fromHistory(ctx)
	if err != nil || len(rs) == 0 {
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
	first := a.IsZero()
	if first {
		if a, err = s.l.RunsFrom(ctx); err != nil || a.IsZero() {
			return err
		}
	}
	s.status.Lock()
	if s.status.copyFrom.After(a) {
		a = s.status.copyFrom
	}
	s.status.Unlock()
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
		// Before the first copy, a day the history holds no change up to
		// says nothing of what fired then (it begins after it, kept so far
		// back no more, or not yet): passed over, not taken for a quiet one.
		reaches, err := true, error(nil)
		if first {
			reaches, err = s.cfg.JudgedHistory.Reaches(ctx, hw.Serves, b)
		}
		if err == nil && reaches {
			s.status.set("copying in what Grafana's alert state history says its rules fired, %s", a.In(s.cfg.Loc).Format("2006-01-02 15:04"))
			err = s.copyDay(ctx, hw.Serves, rs, all, a, b, first, o)
		}
		var invalid ledger.ErrInvalid
		switch {
		case err == nil && !reaches:
			skip(b)
		case errors.Is(err, source.ErrUnusable) || errors.As(err, &invalid):
			s.log.Warn("Grafana's alert state history of the rules Lapilli can't replay wasn't copied in for a day: their firings then are left out",
				"from", a, "to", b, "err", err)
			skip(b)
		case err != nil:
			return err
		default:
			first = false
		}
		a = b
	}
	return nil
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
	if len(j.Unsure) > 0 {
		s.log.Warn("Grafana's rules its alert state history doesn't tell apart, or passed over changes of, aren't copied in for a day",
			"rules", len(j.Unsure), "from", p, "to", b)
	}
	// What a write can't hold of a firing (more than 32 labels, a name
	// that doesn't show) leaves it out, as it would the write; the rest are
	// as many as one write holds at most.
	passed := 0
	fs := slices.DeleteFunc(j.Firings, func(x firing.Firing) bool {
		if recordable(x) != nil {
			passed++
			return true
		}
		return false
	})
	if passed > 0 {
		s.log.Warn("firings in Grafana's alert state history a write can't hold left out", "firings", passed, "from", p, "to", b)
	}
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
	if from.Before(p) {
		before, err := s.l.RecordedFirings(ctx, ledger.GrafanaSource, from, p)
		if err != nil {
			return err
		}
		for i, x := range before {
			k := alertOf(x)
			if end, ok := ended[k]; ok && x.End.IsZero() && x.Start.Equal(on[k].Start) {
				before[i].End = end
			}
		}
		send = append(before, send...)
	}
	got, err := s.l.PutFirings(context.WithoutCancel(ctx), ledger.GrafanaSource, from, b, send, ledger.FiringsCopier, o)
	if err == nil && got.Changed() {
		s.log.Info("Grafana's alert state history copied in", "from", from, "to", b, "firings", got.Firings, "added", got.Added, "removed", got.Removed)
	}
	return err
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
