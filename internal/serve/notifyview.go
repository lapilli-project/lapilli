package serve

import (
	"context"
	"sync"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// What the pages show of notifications as they were sent: for each rule,
// how many, how many repeats, how many while people slept, how many for
// firings that caught nothing; for each firing, how many told of it.
// Notifications can't be replayed for history: each number counts from the
// first notification kept.

// pagedCache keeps the notifications that came in the weeks shown, tied to
// the firings they tell of, for a generation of the ledger.
type pagedCache struct {
	sync.Mutex
	gen  int64
	from time.Time
	p    *paged
}

// paged are the notifications of the weeks shown, as the pages read them.
type paged struct {
	pages []ledger.Page
	// since is, by rule, when the first alert of it a notification told of
	// started; first, when the first notification came, zero for none.
	since  map[string]time.Time
	first  time.Time
	within ledger.Within
}

// shownFrom is when the twelve weeks shown start.
func (s *Server) shownFrom() time.Time {
	return s.l.Midnight(ledger.Week(s.l.Day(s.now())) - 7*(weeks-1))
}

// notified are the notifications since the twelve weeks shown started,
// worked out once for each generation of the ledger.
func (s *Server) notified(ctx context.Context) (*paged, error) {
	from := s.shownFrom()
	gen := s.l.Gen()
	s.paged.Lock()
	defer s.paged.Unlock()
	if s.paged.p != nil && s.paged.gen == gen && s.paged.from.Equal(from) {
		return s.paged.p, nil
	}
	p := &paged{}
	var err error
	if p.first, err = s.l.FirstNotification(ctx); err != nil {
		return nil, err
	}
	if p.within, err = s.within(ctx); err != nil {
		return nil, err
	}
	if !p.first.IsZero() {
		if p.since, err = s.l.NotifiedSince(ctx); err != nil {
			return nil, err
		}
		if p.pages, err = s.l.Pages(ctx, from, s.now().Add(time.Hour), p.within); err != nil {
			return nil, err
		}
	}
	s.paged.gen, s.paged.from, s.paged.p = gen, from, p
	return p, nil
}

// within is how far apart an alert's start and its firing's may be: two
// evaluations of the rule, by its group's interval, the replay's step, or a
// minute, Prometheus's own default, whichever is longest.
func (s *Server) within(ctx context.Context) (ledger.Within, error) {
	step := max(s.cfg.Step, time.Minute)
	every := map[string]time.Duration{}
	if !s.cfg.Alerts {
		latest, ok, err := s.l.Latest(ctx)
		if err != nil {
			return nil, err
		}
		if ok {
			var rs []rules.Rule
			if rs, err = s.parse(ctx, latest); err != nil {
				return nil, err
			}
			for _, r := range rs {
				every[r.Name] = max(every[r.Name], r.Interval)
			}
		}
	}
	return func(rule string) time.Duration { return 2 * max(every[rule], step) }, nil
}

// pagedLine is what a rule's notifications came to over a span.
type pagedLine struct {
	// Notified says notifications are counted for it: some told of it, from
	// NotifiedSince on.
	Notified      bool
	NotifiedSince time.Time
	// Notifications are those sent for it; Repeats, those that told of the
	// same alerts again, and RepeatsForNothing, those of them for firings
	// that caught nothing: what repeat_interval cost; NotifiedAsleep and
	// NotifiedOffHours, those that came while people slept, and off hours;
	// ForNothing, those for firings that caught nothing; Untied, those that
	// tell of no firing the ledger has.
	Notifications, Repeats, RepeatsForNothing int
	NotifiedAsleep, NotifiedOffHours          int
	ForNothing, Untied                        int
	ByReceiver                                map[string]int
	Receivers                                 string
}

// pagedLines are, by rule, what its notifications that came in [from, to)
// came to, counted asleep by the hours of.
func (p *paged) lines(from, to time.Time, of func(rule string) *verdict.Hours) map[string]*pagedLine {
	out := map[string]*pagedLine{}
	for _, pg := range p.pages {
		if pg.At.Before(from) || !pg.At.Before(to) {
			continue
		}
		x := out[pg.Rule]
		if x == nil {
			x = &pagedLine{Notified: true, NotifiedSince: p.since[pg.Rule], ByReceiver: map[string]int{}}
			out[pg.Rule] = x
		}
		x.Notifications++
		x.ByReceiver[pg.Receiver]++
		if pg.Reason == ledger.ReasonRepeat {
			x.Repeats++
			if pg.Stone == ledger.PageBlack {
				x.RepeatsForNothing++
			}
		}
		switch pg.Stone {
		case ledger.PageBlack:
			x.ForNothing++
		case ledger.PageUntied:
			x.Untied++
		}
		if h := of(pg.Rule); h != nil {
			switch h.Of(pg.At) {
			case verdict.Asleep:
				x.NotifiedAsleep++
			case verdict.OffHours:
				x.NotifiedOffHours++
			}
		}
	}
	for _, x := range out {
		x.Receivers = ledger.Receivers(x.ByReceiver)
	}
	return out
}

// pagedOf gives rule lines what their notifications came to over the
// weeks they show.
func (s *Server) pagedOf(ctx context.Context, t *tallies, n int, lines []ruleLine) error {
	p, err := s.notified(ctx)
	if err != nil || p.first.IsZero() {
		return err
	}
	o, err := s.owners(ctx)
	if err != nil {
		return err
	}
	sc := o.schedules(t.o.Hours)
	by := p.lines(s.l.Midnight(t.weeks[len(t.weeks)-n]), s.now().Add(time.Hour), sc.of)
	for i := range lines {
		if x := by[lines[i].Name]; x != nil {
			lines[i].pagedLine = *x
		} else if since, ok := p.since[lines[i].Name]; ok {
			lines[i].Notified, lines[i].NotifiedSince = true, since
		}
	}
	return nil
}

// notifiedFirings gives firing lines how many notifications told of each,
// and whether notifications were counted for the rule by then.
func (s *Server) notifiedFirings(ctx context.Context, lines []firingLine) error {
	p, err := s.notified(ctx)
	if err != nil || p.first.IsZero() || len(lines) == 0 {
		return err
	}
	fs := make([]ledger.Firing, len(lines))
	for i, l := range lines {
		fs[i] = l.Firing
	}
	of, err := s.l.NotifiedOf(ctx, fs, p.within)
	if err != nil {
		return err
	}
	for i := range lines {
		since, ok := p.since[lines[i].Rule]
		lines[i].Counted = ok && !lines[i].Start.Before(since)
		lines[i].Notified = of[lines[i].ID]
	}
	return nil
}

// woken counts, for each week shown, the notifications for firings that
// caught nothing that came while people slept, by the hours of each rule's
// team: the dashboard's "woken for nothing" once there are notifications.
// It reports false while there are none; weeks before the first are left
// uncounted.
func (s *Server) woken(ctx context.Context, t *tallies, o *owners, team *ledger.Team, byWeek map[int64]*weekView) (time.Time, bool, error) {
	p, err := s.notified(ctx)
	if err != nil || p.first.IsZero() {
		return time.Time{}, false, err
	}
	sc := o.schedules(t.o.Hours)
	for w, wv := range byWeek {
		wv.Asleep = 0
		wv.Uncounted = !s.l.Midnight(w + 7).After(p.first)
	}
	for _, pg := range p.pages {
		if pg.Stone != ledger.PageBlack || (team != nil && o.rule[pg.Rule] != team.ID) {
			continue
		}
		wv := byWeek[ledger.Week(s.l.Day(pg.At))]
		if h := sc.of(pg.Rule); wv != nil && h != nil && h.Of(pg.At) == verdict.Asleep {
			wv.Asleep++
		}
	}
	return p.first, true, nil
}
