package serve

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/prom"
)

// What lapilli serve does so as not to take the quota of the history API
// it shares with the alerts (docs/design/quota.md): it yields to an API
// that answers HTTP 429 (--yield-on-429, --yield-for), holds its runs while
// --guard returns anything, and catches up only in --catch-up-hours, at
// --catch-up-rate.

// Window is a span of the day, in --tz: from From to To, minutes after
// midnight. One that ends before it starts (22:00-04:00) runs on into the
// next day.
type Window struct{ From, To int }

// ParseWindow reads HH:MM-HH:MM.
func ParseWindow(s string) (Window, error) {
	a, b, ok := strings.Cut(s, "-")
	from, err1 := minuteOf(a)
	to, err2 := minuteOf(b)
	if !ok || err1 != nil || err2 != nil || from == to {
		return Window{}, fmt.Errorf("%q: want hours of the day, like 01:00-06:00", s)
	}
	return Window{from, to}, nil
}

// minuteOf reads HH:MM as minutes after midnight.
func minuteOf(s string) (int, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil || len(m) != 2 || hh < 0 || hh > 24 || mm < 0 || mm > 59 || hh == 24 && mm != 0 {
		return 0, fmt.Errorf("%q: want HH:MM", s)
	}
	return hh*60 + mm, nil
}

// Len is how long the window is.
func (w Window) Len() time.Duration {
	n := w.To - w.From
	if n <= 0 {
		n += 24 * 60
	}
	return time.Duration(n) * time.Minute
}

// Has reports whether t falls in the window, in loc.
func (w Window) Has(t time.Time, loc *time.Location) bool {
	t = t.In(loc)
	m := t.Hour()*60 + t.Minute()
	if w.From < w.To {
		return m >= w.From && m < w.To
	}
	return m >= w.From || m < w.To
}

func (w Window) String() string {
	return fmt.Sprintf("%02d:%02d-%02d:%02d", w.From/60, w.From%60, w.To/60, w.To%60)
}

// heldKind is the kind of a run held, kept in the run history with why.
const heldKind = "held"

// guardTime is how long --guard is waited for.
const guardTime = time.Minute

// behind reports whether a run from cursor to now catches up: more than
// two runs behind, as the overview's backlog says. The first start's
// --backfill is one, and a run after being down, or replaying again from
// when a version of the rules recorded since took effect.
func (s *Server) behind(cursor, now time.Time) bool {
	return now.Sub(cursor) > 2*s.cfg.Every+s.cfg.Settle
}

// waits says why the run from cursor to now waits for a later one, "" for
// none: a catch-up outside --catch-up-hours; the history yielded to (still
// in --yield-for); --guard returning anything, or unread.
func (s *Server) waits(ctx context.Context, cursor, now time.Time) string {
	if w := s.cfg.CatchUpHours; w != nil && s.behind(cursor, now) && !w.Has(s.now(), s.cfg.Loc) {
		return fmt.Sprintf("a catch-up of %s waits for --catch-up-hours %s", shownDur(now.Sub(cursor)), w)
	}
	if s.yield != nil {
		if until := s.yield.Until("prometheus"); !until.IsZero() {
			return fmt.Sprintf("the prometheus API answered HTTP 429, and isn't asked until %s (--yield-for)", until.In(s.cfg.Loc).Format("2006-01-02 15:04"))
		}
	}
	if s.cfg.Guard == "" || s.h == nil {
		return ""
	}
	// Asked as the run asks: a 429 yields, with --yield-on-429.
	gctx, cancel := context.WithTimeout(s.asking(ctx, cursor, now), guardTime)
	defer cancel()
	at := s.now().Truncate(time.Second)
	series, err := s.h.QueryRange(gctx, s.cfg.Guard, at, at, s.cfg.Step)
	switch {
	case err != nil && ctx.Err() != nil:
		return "" // stopping: not a reason to keep
	case err != nil:
		return "--guard couldn't be read, so the run waits: " + prom.Steady(err.Error())
	}
	n := 0
	for _, x := range series {
		if len(x.Samples) > 0 {
			n++
		}
	}
	if n > 0 {
		s.log.Info("--guard returned series: the run waits", "series", n)
		return "--guard returned series, so the run waits"
	}
	return ""
}

// hold keeps a run held, of kind (hourly, backfill) over [from, to], in
// the run history with why: once while the last run kept is held for the
// same reason. It says so in the status and the log.
func (s *Server) hold(ctx context.Context, kind string, from, to time.Time, why string) {
	s.holding.Store(true)
	s.count(kind, heldKind)
	s.log.Info("a run waits", "kind", kind, "why", why)
	if runs, err := s.l.Runs(ctx, 1); err == nil && len(runs) > 0 && runs[0].Kind == heldKind && runs[0].Error == why {
		return
	}
	at := s.now()
	if err := s.l.AddRun(context.WithoutCancel(ctx), ledger.Run{Kind: heldKind, Started: at, Finished: at, From: from, To: to, Error: why}); err != nil {
		s.fail("a run", err)
	}
}

// asking is ctx for a run's requests from cursor to now: yielding, with
// --yield-on-429, and at --catch-up-rate while catching up.
func (s *Server) asking(ctx context.Context, cursor, now time.Time) context.Context {
	if prom.CountsOf(ctx) == nil {
		ctx = prom.Counting(ctx, &s.asked) // as the leader's work is
	}
	if s.yield != nil {
		ctx = prom.Yielding(ctx, s.yield)
	}
	if s.cfg.CatchUpRate > 0 && s.behind(cursor, now) {
		ctx = prom.Pacing(ctx, s.cfg.CatchUpRate)
	}
	return ctx
}

// yielded are the APIs the runs yield to now, said once a run's piece is
// kept: the run stops there, and the rules it didn't replay are missed,
// replayed from where they were by a later run.
func (s *Server) yielded() []string {
	if s.yield == nil {
		return nil
	}
	return s.yield.Yielded()
}

// runsWait reports whether this round's run waited, or yielded to the
// prometheus API, which what follows a run asks too.
func (s *Server) runsWait() bool {
	return s.holding.Load() || slices.Contains(s.yielded(), "prometheus")
}

// askedFor counts, into s.askedFor, a replay asked for by a person or an
// agent: one the leader's work asks is counted in s.asked already.
func (s *Server) askedFor(ctx context.Context) context.Context {
	if prom.CountsOf(ctx) != nil {
		return ctx
	}
	return prom.Counting(ctx, &s.othersAsked)
}

// historyMetrics writes what lapilli serve asked of each history API since
// it started: the leader's work, and what people and agents asked for.
func (s *Server) historyMetrics(w io.Writer) {
	if s.h == nil {
		return // firings copied in alone: no history is asked
	}
	apis := []string{"prometheus"}
	if s.cfg.Loki != "" {
		apis = append(apis, "loki")
	}
	if s.cfg.Grafana != "" {
		apis = append(apis, "grafana") // its data source proxy, for its rules on Loki
	}
	loads := map[string]prom.Load{}
	for _, api := range apis {
		a, b := s.asked.Of(api), s.othersAsked.Of(api)
		for k, v := range b.Answers {
			a.Answers[k] += v
		}
		a.TooMany += b.TooMany
		a.Retries += b.Retries
		a.Yields += b.Yields
		a.Waited += b.Waited
		a.Paced += b.Paced
		loads[api] = a
	}
	var b strings.Builder
	b.WriteString("# HELP lapilli_serve_history_requests_total Requests lapilli serve sent to each history API since it started, by the kind of API and the class of the answer: 2xx, 3xx, 4xx, 5xx, or none (refused, dropped, timed out).\n# TYPE lapilli_serve_history_requests_total counter\n")
	for _, api := range apis {
		for _, class := range prom.Answers {
			fmt.Fprintf(&b, "lapilli_serve_history_requests_total{api=%q,answer=%q} %d\n", api, class, loads[api].Answers[class])
		}
	}
	b.WriteString("# HELP lapilli_serve_history_throttled_total Answers of HTTP 429, too many requests, from each history API since lapilli serve started.\n# TYPE lapilli_serve_history_throttled_total counter\n")
	for _, api := range apis {
		fmt.Fprintf(&b, "lapilli_serve_history_throttled_total{api=%q} %d\n", api, loads[api].TooMany)
	}
	b.WriteString("# HELP lapilli_serve_history_retries_total Requests to each history API asked again after an answer that said to (HTTP 429, 5xx) or a connection dropped, since lapilli serve started.\n# TYPE lapilli_serve_history_retries_total counter\n")
	for _, api := range apis {
		fmt.Fprintf(&b, "lapilli_serve_history_retries_total{api=%q} %d\n", api, loads[api].Retries)
	}
	b.WriteString("# HELP lapilli_serve_history_wait_seconds_total Seconds requests to each history API waited before they were sent, summed over the requests, since lapilli serve started: for the API (reason api: a wait it asked for, Retry-After, and before asking again) and for a turn by a rate (reason rate: --max-rate, --catch-up-rate).\n# TYPE lapilli_serve_history_wait_seconds_total counter\n")
	for _, api := range apis {
		fmt.Fprintf(&b, "lapilli_serve_history_wait_seconds_total{api=%q,reason=\"api\"} %g\n", api, loads[api].Waited.Seconds())
		fmt.Fprintf(&b, "lapilli_serve_history_wait_seconds_total{api=%q,reason=\"rate\"} %g\n", api, loads[api].Paced.Seconds())
	}
	b.WriteString("# HELP lapilli_serve_history_yields_total Times the runs yielded to each history API, as it answered HTTP 429 (--yield-on-429), since lapilli serve started.\n# TYPE lapilli_serve_history_yields_total counter\n")
	for _, api := range apis {
		fmt.Fprintf(&b, "lapilli_serve_history_yields_total{api=%q} %d\n", api, loads[api].Yields)
	}
	io.WriteString(w, b.String())
}
