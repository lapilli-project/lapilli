package serve

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// What the pages show of notifications as they were sent: for each rule,
// how many, how many repeats, how many while people slept, how many for
// firings that caught nothing; for each firing, how many told of it, or,
// told of by none, what muted it (docs/design/mutes.md). Notifications
// can't be replayed for history: each number counts from the first
// notification kept.

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
	// mutes says the ledger keeps a mute.
	mutes bool
	// woken are, by week and rule, those for firings that caught nothing
	// that came while people slept: worked out once (wokenBy).
	wokenOnce sync.Once
	woken     map[int64]map[string]int
	// wokenDay are the same by day and rule, for a week whose days are
	// judged apart (rulesweb.go, wokenOver).
	wokenDay map[int64]map[string]int
	// from is when the notifications read start, the twelve weeks shown;
	// told, the firings they tell of, worked out once (toldOf).
	from     time.Time
	toldOnce sync.Once
	told     map[int64]bool
	// events are the pages of the weeks shown, recorded or worked out
	// (routedview.go); routed says the notifications are worked out here and
	// some hours are, cover saying which (and routedFrom where the first
	// starts), routedMore that there were more firings with pages than are
	// counted; recordedFrom is, by receiver, when the first notification was
	// recorded; upTo is where the ledger's firings are up to: pages are
	// known only for hours covered, up to there (known).
	events       []pageEvent
	routed       bool
	routedMore   bool
	routedCut    time.Time // when routedMore, the time of the last page read
	routedFrom   time.Time
	cover        ledger.RoutedCover
	upTo         time.Time
	recordedFrom map[string]time.Time
	// recorded are, by receiver, the hours (Unix seconds of each start) in
	// which a notification was recorded for it; pagers the receivers the
	// configuration says page (nil where it isn't read): known decides an
	// hour not worked out for each of them.
	recorded map[string]map[int64]bool
	pagers   map[string]bool
	// cfgSum is the hash of the routing configuration the pages were
	// counted with.
	cfgSum string
}

// toldOf reports whether a notification read told of a firing.
func (p *paged) toldOf(id int64) bool {
	p.toldOnce.Do(func() {
		p.told = map[int64]bool{}
		for _, pg := range p.pages {
			for _, f := range pg.Firings {
				p.told[f] = true
			}
		}
	})
	return p.told[id]
}

// counted reports whether notifications were counted for a rule by a
// firing's start: as a firing's page has it, "paged no one" or not.
func (p *paged) counted(rule string, start time.Time) bool {
	since, ok := p.since[rule]
	return ok && !start.Before(since)
}

// wokenWalks counts the walks wokenBy makes over the notifications kept:
// for tests of how often a page makes one.
var wokenWalks atomic.Int64

// wokenBy are, by the week each came in and by rule, the notifications
// for firings that caught nothing that came while people slept, by the
// hours of each rule's team, sc: worked out once for the generation of the
// ledger p is of, for the overview's "woken for nothing" and the rules
// list alike, rather than for each page.
func (s *Server) wokenBy(p *paged, sc schedules) map[int64]map[string]int {
	p.wokenOnce.Do(func() {
		wokenWalks.Add(1)
		p.woken, p.wokenDay = map[int64]map[string]int{}, map[int64]map[string]int{}
		if s.notifies() {
			// Pages, recorded or worked out, not every notification
			// (routedview.go).
			for _, ev := range p.events {
				if !ev.Nothing {
					continue
				}
				if h := sc.of(ev.Rule); h == nil || h.Of(ev.At) != verdict.Asleep {
					continue
				}
				// On the day of its firing's start, as that is judged, not
				// the day it was sent (as below).
				at := ev.At
				if !ev.Start.IsZero() {
					at = ev.Start
				}
				d := s.l.Day(at)
				w := ledger.Week(d)
				if p.woken[w] == nil {
					p.woken[w] = map[string]int{}
				}
				p.woken[w][ev.Rule]++
				if p.wokenDay[d] == nil {
					p.wokenDay[d] = map[string]int{}
				}
				p.wokenDay[d][ev.Rule]++
			}
			return
		}
		for _, pg := range p.pages {
			if pg.Stone != ledger.PageBlack {
				continue
			}
			if h := sc.of(pg.Rule); h == nil || h.Of(pg.At) != verdict.Asleep {
				continue
			}
			// On the day of its firing's start, as that is judged, not the
			// day it was sent.
			at := pg.At
			if !pg.Start.IsZero() {
				at = pg.Start
			}
			d := s.l.Day(at)
			w := ledger.Week(d)
			if p.woken[w] == nil {
				p.woken[w] = map[string]int{}
			}
			p.woken[w][pg.Rule]++
			if p.wokenDay[d] == nil {
				p.wokenDay[d] = map[string]int{}
			}
			p.wokenDay[d][pg.Rule]++
		}
	})
	return p.woken
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
	if s.paged.p != nil && s.paged.gen == gen && s.paged.from.Equal(from) && s.paged.p.cfgSum == s.routingSum() {
		return s.paged.p, nil
	}
	p := &paged{from: from}
	var err error
	if p.first, err = s.l.FirstNotification(ctx); err != nil {
		return nil, err
	}
	if p.within, err = s.within(ctx); err != nil {
		return nil, err
	}
	if p.mutes, err = s.l.HasMutes(ctx); err != nil {
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
	if err = s.loadPageEvents(ctx, p, from, s.now().Add(time.Hour)); err != nil {
		return nil, err
	}
	p.cfgSum = s.routingSum()
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
	// pagesLine is what its pages came to (routedview.go), where pages are
	// counted.
	pagesLine
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
	if err != nil || !p.any() {
		return err
	}
	o, err := s.owners(ctx)
	if err != nil {
		return err
	}
	sc := o.schedules(t.o.Hours)
	to := s.now().Add(time.Hour)
	if t.over {
		to = t.to
	}
	shownFrom := s.l.Midnight(t.weeks[len(t.weeks)-n])
	by := p.lines(shownFrom, to, sc.of)
	pages := p.pageLines(shownFrom, to, sc.of)
	for i := range lines {
		if x := by[lines[i].Name]; x != nil {
			lines[i].pagedLine = *x
		} else if since, ok := p.since[lines[i].Name]; ok {
			lines[i].Notified, lines[i].NotifiedSince = true, since
		}
		if x := pages[lines[i].Name]; x != nil {
			lines[i].pagesLine = *x
		} else if p.routed {
			lines[i].PagedOn = true
		}
		if lines[i].PagedOn && !p.known(shownFrom, to) {
			// Hours not worked out: the pages of the rest aren't the pages.
			lines[i].pagesLine = pagesLine{PagedOn: true, PagedUnknown: true}
		}
		if lines[i].PagedOn {
			lines[i].PagedFrom = p.countedFrom().In(s.cfg.Loc).Format("2006-01-02")
		}
	}
	return nil
}

// notifiedFirings gives firing lines how many notifications told of each,
// and whether notifications were counted for the rule by then; and what
// muted each, read for the lines alone, never cached.
func (s *Server) notifiedFirings(ctx context.Context, lines []firingLine) error {
	p, err := s.notified(ctx)
	if err != nil || len(lines) == 0 {
		return err
	}
	fs := make([]ledger.Firing, len(lines))
	for i, l := range lines {
		fs[i] = l.Firing
	}
	if p.mutes {
		muted, err := s.l.MutedOf(ctx, fs, p.within, s.now())
		if err != nil {
			return err
		}
		for i := range lines {
			lines[i].Muted = muted[lines[i].ID]
		}
	}
	if s.notifies() {
		if err := s.routedFirings(ctx, lines); err != nil {
			return err
		}
	}
	if p.first.IsZero() {
		return nil
	}
	of, err := s.l.NotifiedOf(ctx, fs, p.within)
	if err != nil {
		return err
	}
	for i := range lines {
		lines[i].Counted = p.counted(lines[i].Rule, lines[i].Start)
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
	if err != nil || !p.any() {
		return time.Time{}, false, err
	}
	by := s.wokenBy(p, o.schedules(t.o.Hours))
	from := p.countedFrom()
	for w, wv := range byWeek {
		wv.Asleep = 0
		wv.Uncounted = !s.l.Midnight(w+7).After(from) || s.notifies() && !p.known(s.l.Midnight(w), s.l.Midnight(w+7))
		for rule, n := range by[w] {
			if team == nil || o.rule[rule] == team.ID {
				if t.split[w] && !t.o.Signals.Named() {
					// Of a week whose days are judged apart, those of the
					// days judged: as the rules list counts them.
					n = 0
					for _, u := range t.units(w) {
						if a, b := t.unit(u); t.rec.Kept(a, b) {
							n += p.wokenDay[u][rule]
						}
					}
				}
				wv.Asleep += n
			}
		}
	}
	return from, true, nil
}
