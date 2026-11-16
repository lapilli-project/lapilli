package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// catchUpAfter is how far behind a run is a catch-up, which
// --catch-up-hours keeps to its hours and --catch-up-rate paces: a day,
// so that a run back after a short outage, hours behind, runs at once.
const catchUpAfter = 24 * time.Hour

// CatchUpAfter is how far behind a run is a catch-up.
const CatchUpAfter = catchUpAfter

// heldReasons are why a run waits, as /metrics says it
// (lapilli_serve_runs_held).
var heldReasons = []string{"catch_up_hours", "guard", "yield"}

// catchingUp reports whether a run from cursor to now catches up: more
// than catchUpAfter behind where the ledger's firings were up to (ahead),
// which a run replaying again from when a version of the rules recorded
// since took effect goes back from, and isn't a catch-up for.
func catchingUp(cursor, ahead, now time.Time) bool {
	if ahead.After(cursor) {
		cursor = ahead
	}
	return now.Sub(cursor) > catchUpAfter
}

// wait is why a run waits for a later one: a text for the run history,
// the same each time for the same reason, and which of heldReasons. A
// guard refused as written isn't a wait: the run fails (refused).
type wait struct {
	why, reason string
	refused     error
}

// yieldedWait is the wait of a run whose request err was yielded.
func yieldedWait(err error) wait {
	api := "prometheus"
	var y *prom.Yielded
	if errors.As(err, &y) {
		api = y.API
	}
	return wait{why: "the " + api + " API answered HTTP 429, so the run waits for a later one (--yield-on-429)", reason: "yield"}
}

// waits says why the run from cursor to now waits, a zero wait for none: a
// catch-up outside --catch-up-hours; the prometheus API yielded to, still
// in --yield-for; --guard returning anything, or unread.
func (s *Server) waits(ctx context.Context, cursor, ahead, now time.Time) wait {
	catchUp := catchingUp(cursor, ahead, now)
	if w := s.cfg.CatchUpHours; w != nil && catchUp && !w.Has(s.now(), s.cfg.Loc) {
		s.log.Warn("a catch-up waits for --catch-up-hours", "hours", w.String(), "behind", now.Sub(cursor).Round(time.Minute))
		return wait{why: fmt.Sprintf("a catch-up waits for --catch-up-hours %s", w), reason: "catch_up_hours"}
	}
	if s.yield != nil {
		if until := s.yield.Until("prometheus"); !until.IsZero() {
			return wait{why: fmt.Sprintf("the prometheus API answered HTTP 429, and isn't asked until %s (--yield-for)", until.In(s.cfg.Loc).Format("2006-01-02 15:04")), reason: "yield"}
		}
	}
	if s.cfg.Guard == "" || s.h == nil {
		return wait{}
	}
	// Asked as the run asks: a 429 yields, with --yield-on-429.
	gctx, cancel := context.WithTimeout(s.asking(ctx, catchUp), guardTime)
	defer cancel()
	at := s.now().Truncate(time.Second)
	series, err := s.h.QueryRange(gctx, s.cfg.Guard, at, at, s.cfg.Step)
	switch {
	case err != nil && ctx.Err() != nil:
		return wait{} // stopping: not a reason to keep
	case err != nil && prom.IsYielded(err):
		return yieldedWait(err)
	case err != nil && prom.Refused(err):
		// A typo isn't a reason to wait quietly: it is mended.
		return wait{refused: fmt.Errorf("--guard was refused as written, and the runs wait until it is mended: %s", prom.Steady(err.Error()))}
	case err != nil:
		s.log.Warn("--guard couldn't be read: the run waits", "err", err)
		return wait{why: "--guard couldn't be read, so the run waits", reason: "guard"}
	}
	n := 0
	for _, x := range series {
		if len(x.Samples) > 0 {
			n++
		}
	}
	if n > 0 {
		s.log.Warn("--guard returned series: the run waits", "series", n)
		return wait{why: "--guard returned series, so the run waits", reason: "guard"}
	}
	return wait{}
}

// hold keeps a run that waits, of kind (hourly, backfill) over [from, to],
// in the run history with why: once while the last run kept is held for
// the same reason. A guard refused as written is kept as a run that
// failed, each time. Either way the runs wait, and what reads the history
// after them (runsWait).
func (s *Server) hold(ctx context.Context, kind string, from, to time.Time, w wait) {
	s.holding.Store(true)
	at := s.now()
	if w.refused != nil {
		s.count(kind, "failed")
		s.fail("a run", w.refused)
		if err := s.l.AddRun(context.WithoutCancel(ctx), ledger.Run{Kind: kind, Started: at, Finished: at, From: from, To: to, Error: w.refused.Error()}); err != nil {
			s.fail("a run", err)
		}
		return
	}
	s.heldFor.Store(w.reason)
	s.count(kind, heldKind)
	s.log.Warn("a run waits", "kind", kind, "why", w.why)
	if runs, err := s.l.Runs(ctx, 1); err == nil && len(runs) > 0 && runs[0].Kind == heldKind && runs[0].Error == w.why {
		return
	}
	if err := s.l.AddRun(context.WithoutCancel(ctx), ledger.Run{Kind: heldKind, Started: at, Finished: at, From: from, To: to, Error: w.why}); err != nil {
		s.fail("a run", err)
	}
}

// asking is ctx for a run's requests: yielding, with --yield-on-429, and,
// for a catch-up, at --catch-up-rate, one pace for all the runs.
func (s *Server) asking(ctx context.Context, catchUp bool) context.Context {
	if prom.CountsOf(ctx) == nil {
		ctx = prom.Counting(ctx, &s.asked) // as the leader's work is
	}
	if s.yield != nil {
		ctx = prom.Yielding(ctx, s.yield)
	}
	if catchUp {
		ctx = prom.Pacing(ctx, s.catchUpPace)
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

// runsWait reports whether this round's run waited, or yielded to an API:
// what reads the history after the runs waits for a later round then.
func (s *Server) runsWait() bool {
	return s.holding.Load() || len(s.yielded()) > 0
}

// missedEnd is where a run's piece from cursor ends, to at most: with
// --yield-on-429, no later than ledger.MissedFor after the earliest a rule
// was left missed from by a yield, so that a run that misses it again
// doesn't give it up. At cursor itself, the piece moves the cursor
// nowhere: it replays the rules missed, and the next pieces wait for them.
func (s *Server) missedEnd(ctx context.Context, to time.Time) (time.Time, error) {
	if s.yield == nil {
		return to, nil
	}
	missed, err := s.l.Missed(ctx)
	if err != nil || len(missed) == 0 {
		return to, err
	}
	table, err := s.l.RuleTable(ctx)
	if err != nil {
		return to, err
	}
	var oldest time.Time
	for _, r := range table {
		if at, ok := missed[r.Name]; ok && r.Source == "" && prom.IsYieldedReason(r.Unsupported) && (oldest.IsZero() || at.Before(oldest)) {
			oldest = at
		}
	}
	if end := oldest.Add(ledger.MissedFor).Truncate(s.cfg.Step); !oldest.IsZero() && end.Before(to) {
		return end, nil
	}
	return to, nil
}

// replayOrder is the order a run replays its rules in, by where each
// starts: the latest first, so that one far behind doesn't keep the rest
// from their turn; with --yield-on-429, the earliest first, so that a
// rule a 429 left missed is asked before a later one cuts the run short.
func (s *Server) replayOrder(a, b time.Time) int {
	if s.yield != nil {
		return a.Compare(b)
	}
	return b.Compare(a)
}

// askedFor counts, into s.askedFor, a replay asked for by a person or an
// agent: one the leader's work asks is counted in s.asked already.
func (s *Server) askedFor(ctx context.Context) context.Context {
	if prom.CountsOf(ctx) != nil {
		return ctx
	}
	return prom.Counting(ctx, &s.othersAsked)
}

// historyMetrics writes what this lapilli serve process asked of each
// history API since it started: the leader's work, and what people and
// agents asked for; and why its last run waited. prometheus is always
// read once there is a history: serve replays only with --prometheus.
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
	if s.yield != nil || s.cfg.Guard != "" || s.cfg.CatchUpHours != nil {
		held, _ := s.heldFor.Load().(string)
		b.WriteString("# HELP lapilli_serve_runs_held Whether the last run waited for a later one, by why: 1 for the reason it waited for (catch_up_hours, guard or yield, docs/design/quota.md), 0 for the others.\n# TYPE lapilli_serve_runs_held gauge\n")
		for _, r := range heldReasons {
			n := 0
			if r == held {
				n = 1
			}
			fmt.Fprintf(&b, "lapilli_serve_runs_held{reason=%q} %d\n", r, n)
		}
	}
	io.WriteString(w, b.String())
}
