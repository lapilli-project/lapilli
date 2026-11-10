package serve

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// The replay beside what really fired, kept (docs/design/fidelity.md).

const (
	alertsSeries        = `ALERTS{alertstate="firing"}`
	grafanaAlertsSeries = `GRAFANA_ALERTS{alertstate="firing"}`
	// maxCheckDays is how many days one round checks: a first start's
	// backfill is checked over a few rounds, so other jobs get their turn.
	maxCheckDays = 7
	// checkWeek is the span a rule is said to differ over.
	checkWeek = 7 * 24 * time.Hour
)

// Differs is when a rule is said to differ from what really fired: with at
// least N firings either way over a week, fewer than Ratio alike.
type Differs struct {
	N     int
	Ratio float64
}

// differs is --check-differs, or its default.
func (s *Server) differs() Differs {
	if s.cfg.CheckDiffers.N > 0 {
		return s.cfg.CheckDiffers
	}
	return Differs{5, 0.9}
}

// checking reports whether the replay is set beside what really fired: on
// with rules to replay, unless --check-replay=false.
func (s *Server) checking() bool {
	return s.cfg.CheckReplay && s.replaying()
}

// checkReplay checks the days the runs won't replay again, a day at a time,
// from where it got to; on a first start, from where the runs began.
func (s *Server) checkReplay(ctx context.Context) error {
	cursor, err := s.l.Cursor(ctx)
	if err != nil || cursor.IsZero() {
		return err
	}
	done, err := s.l.CheckedTo(ctx)
	if err != nil {
		return err
	}
	if done.IsZero() {
		if done, err = s.l.RunsFrom(ctx); err != nil || done.IsZero() {
			return err
		}
	}
	for range maxCheckDays {
		day := s.l.Day(done)
		a := s.l.Midnight(day)
		b := s.l.Midnight(day + 1)
		known, err := s.checkRules(ctx, a, b)
		if err != nil {
			return err
		}
		w := verdict.Window(known, s.cfg.Step)
		// Only what the runs won't replay again, with what may match it.
		if b.Add(w).After(cursor.Add(-s.cfg.Settle)) {
			return nil
		}
		if err := s.checkDay(ctx, known, a, b, w); err != nil {
			return err
		}
		done = b
	}
	return nil
}

// checkRules are the rules in effect over [a, b) whose replay can be set
// beside a series, under the names the ledger judges them by.
func (s *Server) checkRules(ctx context.Context, a, b time.Time) ([]rules.Rule, error) {
	vs, err := s.l.Versions(ctx, a, b, false)
	if err != nil {
		return nil, err
	}
	var known []rules.Rule
	for _, v := range vs {
		all, names, err := s.named(ctx, v.ID)
		if err != nil {
			return nil, err
		}
		for i, r := range all {
			if names[i] == "" || r.Cloud != "" {
				continue
			}
			r.Name = names[i]
			known = append(known, r)
		}
	}
	return known, nil
}

// checkSeries are the series that say what really fired for rules: as
// lapilli verdict --check-replay chooses them, or --check-series.
func (s *Server) checkSeries(known []rules.Rule) []string {
	if len(s.cfg.CheckSeries) > 0 {
		return s.cfg.CheckSeries
	}
	var out []string
	if slices.ContainsFunc(known, func(r rules.Rule) bool { return r.Graph == nil }) {
		out = append(out, alertsSeries)
	}
	if slices.ContainsFunc(known, func(r rules.Rule) bool { return r.Graph != nil }) {
		out = append(out, grafanaAlertsSeries)
	}
	return out
}

// checkDay sets the day [a, b) beside what really fired, and keeps it.
func (s *Server) checkDay(ctx context.Context, known []rules.Rule, a, b time.Time, w time.Duration) error {
	if len(known) == 0 {
		return s.l.SkipCheck(ctx, b)
	}
	s.status.set("setting the replay beside what really fired, %s", a.In(s.cfg.Loc).Format("2006-01-02"))
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	replayed, err := s.l.ReplayedFirings(ctx, a.Add(-w), b.Add(w))
	if err != nil {
		return err
	}
	// Each alert at its own interval, so a slow group's isn't taken to
	// have resolved between evaluations.
	every := map[string]time.Duration{}
	for _, r := range known {
		every[r.Name] = max(every[r.Name], r.Interval, s.cfg.Step)
	}
	// Read from before the span's window, so an alert on since then isn't
	// taken to start at the read's start.
	from, to := a.Add(-w-time.Hour), b.Add(w)
	var fired []firing.Firing
	var empty []string
	for _, sel := range s.checkSeries(known) {
		series, err := s.h.QueryRange(ctx, sel, from, to, s.cfg.Step)
		if err != nil {
			return fmt.Errorf("reading %s: %w", sel, err)
		}
		if len(series) == 0 {
			empty = append(empty, sel)
			continue
		}
		byName := map[string][]prom.Series{}
		for _, ser := range series {
			byName[ser.Labels["alertname"]] = append(byName[ser.Labels["alertname"]], ser)
		}
		for name, group := range byName {
			if name == "" {
				continue
			}
			at := every[name]
			if at == 0 {
				at = s.cfg.Step
			}
			fired = append(fired, firing.FromAlerts(group, at, to)...)
		}
	}
	// A series that held nothing over a day the replay fired says nothing
	// of the rules it is for: they aren't checked that day.
	failed, err := s.l.FailedRules(ctx, a.Add(-w), b.Add(w))
	if err != nil {
		return err
	}
	var checked []rules.Rule
	for _, r := range known {
		if failed[r.Name] {
			continue
		}
		sel := alertsSeries
		if r.Graph != nil {
			sel = grafanaAlertsSeries
		}
		if len(s.cfg.CheckSeries) == 0 && slices.Contains(empty, sel) {
			continue
		}
		checked = append(checked, r)
	}
	s.checkEmpty(empty, a)
	c, ok := verdict.CheckSpan(ctx, checked, replayed, fired, a, b, s.cfg.Step)
	if !ok {
		return ctx.Err()
	}
	if err := s.l.PutCheck(context.WithoutCancel(ctx), a, c, b); err != nil {
		return err
	}
	return s.tellDiffers(context.WithoutCancel(ctx), b)
}

// checkEmpty notes the series that held nothing over the day just checked,
// for the runs page.
func (s *Server) checkEmpty(empty []string, day time.Time) {
	s.status.Lock()
	defer s.status.Unlock()
	s.status.checkEmpty = slices.Clone(empty)
	s.status.checkDay = day
}

// tellDiffers tells, once each time it happens, of a rule whose replay has
// started to differ from what really fired, over the week to to.
func (s *Server) tellDiffers(ctx context.Context, to time.Time) error {
	dests := s.dests()
	if len(dests) == 0 {
		return nil
	}
	cs, err := s.l.Checks(ctx, to.Add(-checkWeek), to)
	if err != nil {
		return err
	}
	d := s.differs()
	for _, name := range slices.Sorted(maps.Keys(cs)) {
		c := cs[name]
		if !c.Differs(d.N, d.Ratio) {
			continue
		}
		told := make([]ledger.Told, 0, len(dests))
		for _, dest := range dests {
			told = append(told, ledger.Told{Dest: dest, About: map[string]any{"rule": name, "check": c, "from": to.Add(-checkWeek), "to": to}})
		}
		// Once a week at most for each rule while it differs.
		week := ledger.Week(s.l.Day(to))
		if err := s.l.TellOnce(ctx, "differs."+name, fmt.Sprint(week), "replay.differs", told); err != nil {
			return err
		}
	}
	return nil
}

// checkLine says in a line how the replay compared with what really fired
// over a span, in --lang's language: "" when nothing was checked.
func (s *Server) checkLine(cs map[string]ledger.Check) string {
	var all ledger.Check
	var differ []string
	d := s.differs()
	for name, c := range cs {
		all.Replayed += c.Replayed
		all.Real += c.Real
		all.Alike += c.Alike
		if c.Differs(d.N, d.Ratio) {
			differ = append(differ, verdict.Clean(name))
		}
	}
	if all.Either() == 0 {
		return ""
	}
	l := s.cfg.Lang
	line := tr(l, "The replay beside what really fired: %d%% alike, of %d firings.", int(math.Round(all.Share()*100)), all.Either())
	if len(differ) > 0 {
		slices.Sort(differ)
		line += " " + tr(l, "Replayed differently from how they really fired: %s.", strings.Join(differ, ", "))
	}
	return line
}
