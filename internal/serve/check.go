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
	// checkLead is how far before a day firings are read: an alert that
	// fired again and again across midnight then pairs alike from either
	// day.
	checkLead = 6 * time.Hour
	// maxElsewhere is how many alerts with no rule given are kept for a
	// day, and maxElsewhereName how long a name of one may be: they come
	// from whatever the store holds.
	maxElsewhere     = 100
	maxElsewhereName = 512
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

// lastWeek is the seven whole days before today, the week a rule is said to
// differ over on the pages, in /metrics and to agents alike.
func (s *Server) lastWeek() (time.Time, time.Time) {
	today := s.l.Day(s.now())
	return s.l.Midnight(today - 7), s.l.Midnight(today)
}

// checkReplay checks the days the runs won't replay again, a day at a time,
// from where it got to; on a first start, from the first whole day the runs
// replayed.
func (s *Server) checkReplay(ctx context.Context) error {
	cursor, err := s.l.Cursor(ctx)
	if err != nil || cursor.IsZero() {
		return err
	}
	done, err := s.l.CheckedTo(ctx)
	if err != nil {
		return err
	}
	day := s.l.Day(done)
	if done.IsZero() {
		began, err := s.l.RunsFrom(ctx)
		if err != nil || began.IsZero() {
			return err
		}
		// A day the runs replayed only part of would count what really
		// fired before them as missed by the replay.
		if day = s.l.Day(began); s.l.Midnight(day).Before(began) {
			day++
		}
	}
	for range maxCheckDays {
		a, b := s.l.Midnight(day), s.l.Midnight(day+1)
		known, names, err := s.checkRules(ctx, a, b)
		if err != nil {
			return err
		}
		w := verdict.Window(known, s.cfg.Step)
		// Only what the runs won't replay again, with what may match it.
		if b.Add(w).After(cursor.Add(-s.cfg.Settle)) {
			return nil
		}
		if err := s.checkDay(ctx, known, names, a, b, w); err != nil {
			return err
		}
		day++
	}
	return nil
}

// checkRules are the rules in effect over [a, b) whose replay can be set
// beside a series, under the name what really fired goes by, the series's
// alertname: rules that share it, named apart in the ledger, what really
// fired doesn't tell apart. names are the names the ledger judges the rules
// of each by.
func (s *Server) checkRules(ctx context.Context, a, b time.Time) ([]rules.Rule, map[string][]string, error) {
	vs, err := s.l.Versions(ctx, a, b, false)
	if err != nil {
		return nil, nil, err
	}
	var known []rules.Rule
	names := map[string][]string{}
	for _, v := range vs {
		all, named, err := s.named(ctx, v.ID)
		if err != nil {
			return nil, nil, err
		}
		for i, r := range all {
			if named[i] == "" || r.Cloud != "" {
				continue
			}
			if !slices.Contains(names[r.Name], named[i]) {
				names[r.Name] = append(names[r.Name], named[i])
			}
			known = append(known, r)
		}
	}
	for k := range names {
		slices.Sort(names[k])
	}
	return known, names, nil
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
func (s *Server) checkDay(ctx context.Context, known []rules.Rule, names map[string][]string, a, b time.Time, w time.Duration) error {
	if len(known) == 0 {
		return s.l.SkipCheck(ctx, b)
	}
	s.status.set("setting the replay beside what really fired, %s", a.In(s.cfg.Loc).Format("2006-01-02"))
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	replayed, err := s.l.ReplayedFirings(ctx, a.Add(-checkLead), b.Add(w))
	if err != nil {
		return err
	}
	// The ledger keeps firings under its names for the rules; what really
	// fired goes by the alert's.
	alert := map[string]string{}
	for raw, ns := range names {
		for _, n := range ns {
			alert[n] = raw
		}
	}
	for i := range replayed {
		if raw, ok := alert[replayed[i].Rule]; ok {
			replayed[i].Rule = raw
		}
	}
	// Each alert at its own interval, so a slow group's isn't taken to
	// have resolved between evaluations.
	every := map[string]time.Duration{}
	for _, r := range known {
		every[r.Name] = max(every[r.Name], r.Interval, s.cfg.Step)
	}
	// Read from before the lead, so an alert on since then isn't taken to
	// start at the read's start.
	from, to := a.Add(-checkLead-time.Hour), b.Add(w)
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
			// Those --rule leaves out are passed over, as verdict does.
			if name == "" || s.cfg.RuleRE != nil && !s.cfg.RuleRE.MatchString(name) {
				continue
			}
			at := every[name]
			if at == 0 {
				at = s.cfg.Step
			}
			fired = append(fired, firing.FromAlerts(group, at, to)...)
		}
	}
	// A rule a run couldn't replay, or whose series held nothing over a day
	// the replay fired, isn't checked that day: it goes as not replayed, so
	// what really fired of it isn't taken for an alert no rule is named.
	failed, err := s.l.FailedRules(ctx, a.Add(-w), b.Add(w))
	if err != nil {
		return err
	}
	checked := make([]rules.Rule, 0, len(known))
	for _, r := range known {
		sel := alertsSeries
		if r.Graph != nil {
			sel = grafanaAlertsSeries
		}
		off := len(s.cfg.CheckSeries) == 0 && slices.Contains(empty, sel)
		for _, n := range names[r.Name] {
			off = off || failed[n]
		}
		if off && r.Unsupported == "" {
			r.Unsupported = "not checked this day"
		}
		checked = append(checked, r)
	}
	s.checkEmpty(empty, a)
	c, ok := verdict.CheckSpan(ctx, checked, replayed, fired, a, b, s.cfg.Step)
	if !ok {
		return ctx.Err()
	}
	// Alerts no rule is named come from whatever the store holds: only
	// names that show are kept, a hundred a day at most.
	keep := map[string]int{}
	for _, name := range slices.Sorted(maps.Keys(c.Elsewhere)) {
		if len(keep) < maxElsewhere && len(name) <= maxElsewhereName && ledger.Plain(name) {
			keep[name] = c.Elsewhere[name]
		}
	}
	c.Elsewhere = keep
	if err := s.l.PutCheck(context.WithoutCancel(ctx), a, c, names, b); err != nil {
		return err
	}
	// Told only for a day just checked: a first start's weeks of backfill
	// aren't news.
	if s.now().Sub(b) > 2*24*time.Hour {
		return nil
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

// tellDiffers tells of each rule whose replay differs from what really
// fired over the seven days to the midnight to, once a week at most while
// it does.
func (s *Server) tellDiffers(ctx context.Context, to time.Time) error {
	dests := s.dests()
	if len(dests) == 0 {
		return nil
	}
	day := s.l.Day(to)
	from := s.l.Midnight(day - 7)
	cs, err := s.l.Checks(ctx, from, to)
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
			told = append(told, ledger.Told{Dest: dest, About: map[string]any{"rule": name, "check": c, "from": from, "to": to}})
		}
		if err := s.l.TellOnce(ctx, "differs."+name, fmt.Sprint(ledger.Week(day)), "replay.differs", told); err != nil {
			return err
		}
	}
	return nil
}

// checkLine says in a line how the replay compared with what really fired
// over a span, in --lang's language: "" when nothing was checked.
func (s *Server) checkLine(cs map[string]ledger.Check) string {
	d := s.differs()
	all, names := ledger.SumChecks(cs, func(string) bool { return true }, d.N, d.Ratio)
	if all.Either() == 0 {
		return ""
	}
	l := s.cfg.Lang
	line := tr(l, "The replay beside what really fired: %d%% alike, of %d firings.", int(math.Round(all.Share()*100)), all.Either())
	if len(names) > 0 {
		differ := make([]string, 0, len(names))
		for _, n := range names {
			differ = append(differ, verdict.Clean(n))
		}
		line += " " + tr(l, "Replayed differently from how they really fired: %s.", strings.Join(differ, ", "))
	}
	return line
}
