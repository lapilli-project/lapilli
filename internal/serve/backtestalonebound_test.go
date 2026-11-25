package serve

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// askedLike is how many requests asked an expression holding sub.
func (h *history) askedLike(sub string) int {
	h.Lock()
	defer h.Unlock()
	n := 0
	for e, c := range h.exprs {
		if strings.Contains(e, sub) {
			n += c
		}
	}
	return n
}

// A rule left alone is held to what a rule that differs is before it is
// replayed: one that reads what the port refuses is never asked of the
// history, and the incident that needs it is not known, the request not
// refused for it.
func TestBacktestFailMissedLeftAloneChecked(t *testing.T) {
	parallel(t)
	held := map[string]any{"days": 3, "fail_missed": true}
	for name, c := range map[string]struct{ expr, sub string }{
		"every metric": {`count({__name__=~".+"}) > 0`, "__name__"},
		"400 days":     {`rate(errors[400d]) > 1`, "400d"},
		"a subquery":   {`max_over_time(errors[30d:1s]) > 1`, "30d:1s"},
	} {
		t.Run(name, func(t *testing.T) {
			h := &history{}
			b := btServingOn(t, h, rulesOf(errorsRule))
			rule := "      - alert: Reads\n        expr: " + c.expr + "\n        for: 5m\n"
			md, rep := b.done(t, b.plain, btBody(rulesOf(errorsRule, rule), rulesOf(errorsLate, rule), held))
			sum, missed := missedOf(t, rep)
			if sum["incidents_missed"] != float64(0) || sum["missed_not_known"] != float64(1) || len(missed) != 0 {
				t.Errorf("summary %v, missed %v\n%s", sum, missed, md)
			}
			if sum["missed_limited"] != float64(0) {
				t.Errorf("a limit said: %v", sum)
			}
			if n := h.askedLike(c.sub); n != 0 {
				t.Errorf("%q was asked of the history %d times", c.expr, n)
			}
		})
	}
}

// vet holds a rule left alone to taken and checkRule: a Loki ruler's rule
// where none in effect is one, a Grafana rule on a data source none reads,
// and a rule past a maximum are refused; one within them, and one already
// unsupported as read, are not.
func TestLeftAloneVet(t *testing.T) {
	parallel(t)
	a := &leftAlone{alone: []aloneRule{
		{rule: rules.Rule{Name: "Fine", Expr: "errors > 1"}},
		{rule: rules.Rule{Name: "Loki", Expr: `count_over_time({app="a"}[5m]) > 1`, Logs: rules.LokiRuler}},
		{rule: rules.Rule{Name: "Long", Expr: "errors > 1", For: 400 * 24 * time.Hour}},
		{rule: rules.Rule{Name: "Keeps", Expr: "errors > 1", KeepFiringFor: 400 * 24 * time.Hour}},
		{rule: rules.Rule{Name: "Wide", Expr: `{__name__=~".+"}`}},
		{rule: rules.Rule{Name: "Paused", Expr: "errors > 1", Unsupported: rules.Paused}},
	}}
	a.vet(&prepared{})
	var got []string
	for _, r := range a.alone {
		if r.refused {
			got = append(got, r.rule.Name)
		}
	}
	if fmt.Sprint(got) != "[Loki Long Keeps Wide]" {
		t.Errorf("refused: %v", got)
	}
	// With a Loki ruler's rule in effect the Loki rule is taken.
	a = &leftAlone{alone: []aloneRule{{rule: rules.Rule{Name: "Loki", Expr: `count_over_time({app="a"}[5m]) > 1`, Logs: rules.LokiRuler}}}}
	a.vet(&prepared{eff: inEffectReads{loki: true}})
	if a.alone[0].refused {
		t.Error("a Loki ruler's rule, one in effect, refused")
	}
}

// The replay of rules left alone is bounded by the rules, not only by the
// requests: past leftAloneRules nothing is replayed and the incidents open
// are not known, counted limited; within it, rules alike are planned and
// asked as one.
func TestBacktestFailMissedLeftAloneRules(t *testing.T) {
	parallel(t)
	held := map[string]any{"days": 3, "fail_missed": true}
	alike := func(n int) string {
		var sb strings.Builder
		for i := range n {
			fmt.Fprintf(&sb, "      - alert: Alike%d\n        expr: errors > 1\n        for: 5m\n", i)
		}
		return sb.String()
	}
	asked := func(n int) (int, map[string]any) {
		h := &history{}
		b := btServingOn(t, h, rulesOf(errorsRule))
		_, rep := b.done(t, b.plain, btBody(rulesOf(errorsRule)+alike(n), rulesOf(errorsLate)+alike(n), held))
		sum, _ := missedOf(t, rep)
		return h.askedOf("errors > 1"), sum
	}
	one, sum := asked(1)
	if one == 0 || sum["missed_not_known"] != float64(0) {
		t.Fatalf("one rule: asked %d, %v", one, sum)
	}
	if many, sum := asked(leftAloneRules - 5); many != one || sum["incidents_missed"] != float64(0) || sum["missed_not_known"] != float64(0) {
		t.Errorf("%d alike rules asked %d times, one asked %d: %v", leftAloneRules-5, many, one, sum)
	}
	// Past the cap: nothing is asked, the incident is not known and limited.
	if past, sum := asked(leftAloneRules + 1); past != 0 || sum["missed_limited"] != float64(1) || sum["missed_not_known"] != float64(1) || sum["incidents_missed"] != float64(0) {
		t.Errorf("%d rules asked %d times: %v", leftAloneRules+1, past, sum)
	}
}

// Judging each incident with the catch rule alone costs a few allocations,
// however many alike rules caught it: Report.Credit's work is not done.
func TestJudgeAloneBounded(t *testing.T) {
	t0 := time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)
	in := incident.Incident{ID: "INC-1", Start: t0, End: t0.Add(30 * time.Minute)}
	o := verdict.Options{Lead: 15 * time.Minute, UnknownLen: time.Hour, From: t0.Add(-time.Hour), To: t0.Add(time.Hour)}
	build := func(n int) ([]rules.Rule, []firing.Firing) {
		var rs []rules.Rule
		var fs []firing.Firing
		for i := range n {
			name := fmt.Sprintf("Alike%d", i)
			rs = append(rs, rules.Rule{Name: name})
			fs = append(fs, firing.Firing{Rule: name, Start: t0.Add(time.Minute), End: t0.Add(10 * time.Minute)})
		}
		return rs, fs
	}
	allocs := func(n int) float64 {
		rs, fs := build(n)
		return testing.AllocsPerRun(3, func() {
			caught, unclear, err := judgeAlone(context.Background(), rs, fs, nil, []incident.Incident{in}, o)
			if err != nil || !caught[0] || unclear[0] {
				t.Fatalf("caught %v, unclear %v, %v", caught, unclear, err)
			}
		})
	}
	small, large := allocs(100), allocs(5000)
	if large > small*100 {
		t.Errorf("allocations grew faster than the rules: %v for 100, %v for 5000", small, large)
	}
}

// A firing already on at the replay's first evaluation, where it began
// unknown, never catches an incident, and one it was about that no other
// catches is not known, not missed.
func TestJudgeAloneCold(t *testing.T) {
	parallel(t)
	t0 := time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)
	in := incident.Incident{ID: "INC-1", Start: t0, End: t0.Add(30 * time.Minute)}
	o := verdict.Options{Lead: 15 * time.Minute, UnknownLen: time.Hour, From: t0.Add(-time.Hour), To: t0.Add(time.Hour)}
	cold := []firing.Firing{{Rule: "Old", Start: t0.Add(-time.Hour)}}
	caught, unclear, err := judgeAlone(context.Background(), nil, nil, cold, []incident.Incident{in}, o)
	if err != nil || caught[0] || !unclear[0] {
		t.Errorf("on at the start: caught %v, unclear %v, %v", caught, unclear, err)
	}
	// One that had ended before the incident could not have caught it.
	cold = []firing.Firing{{Rule: "Old", Start: t0.Add(-3 * time.Hour), End: t0.Add(-2 * time.Hour)}}
	if caught, unclear, _ = judgeAlone(context.Background(), nil, nil, cold, []incident.Incident{in}, o); caught[0] || unclear[0] {
		t.Errorf("ended before: caught %v, unclear %v", caught, unclear)
	}
	// A catch settles it, whatever else was on.
	fs := []firing.Firing{{Rule: "New", Start: t0.Add(time.Minute), End: t0.Add(5 * time.Minute)}}
	cold = []firing.Firing{{Rule: "Old", Start: t0.Add(-time.Hour)}}
	if caught, unclear, _ = judgeAlone(context.Background(), nil, fs, cold, []incident.Incident{in}, o); !caught[0] || unclear[0] {
		t.Errorf("caught: %v, unclear %v", caught, unclear)
	}
	// Past the context: stops.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err = judgeAlone(ctx, nil, fs, nil, []incident.Incident{in}, o); err == nil {
		t.Error("judging went on past its context")
	}
}

// A rule's reach before a catch window holds its keep_firing_for, which
// holds a firing on through gaps the replay of the window alone doesn't see.
func TestAloneReachKeepsFiring(t *testing.T) {
	parallel(t)
	plain := rules.Rule{Name: "R", Expr: "errors > 1", For: 5 * time.Minute, Interval: time.Minute}
	keeps := plain
	keeps.KeepFiringFor = 30 * time.Minute
	if d := aloneTextOf(keeps).reach - aloneTextOf(plain).reach; d != 30*time.Minute {
		t.Errorf("keep_firing_for adds %s to the reach", d)
	}
	t0 := time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)
	in := incident.Incident{ID: "INC-1", Start: t0, End: t0.Add(30 * time.Minute)}
	a := &leftAlone{o: verdict.Options{Lead: 15 * time.Minute, UnknownLen: time.Hour}}
	s := &Server{now: func() time.Time { return t0.Add(48 * time.Hour) }}
	from := func(r rules.Rule) time.Time {
		f, _, _ := s.aloneSpan(a, aloneRule{rule: r, reach: aloneTextOf(r).reach}, in, time.Minute)
		return f
	}
	if d := from(plain).Sub(from(keeps)); d != 30*time.Minute {
		t.Errorf("the window starts %s earlier with keep_firing_for", d)
	}
}

// Of serve's own rules, the record answers only where the runs covered the
// time, none failed there to replay the rule, and it was replayable the
// last time, as of the pull request's.
func TestLeftAloneAsksServesOwn(t *testing.T) {
	parallel(t)
	t0 := time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)
	in := incident.Incident{ID: "INC-1", Start: t0, End: t0.Add(30 * time.Minute)}
	far := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	text := aloneText{sum: [32]byte{1}, reach: 6 * time.Minute}
	mk := func() *leftAlone {
		return &leftAlone{o: verdict.Options{Lead: 15 * time.Minute, UnknownLen: time.Hour}, read: true,
			want:     map[string]aloneText{"Own": text},
			versions: []aloneVersion{{until: far, texts: map[string]aloneText{"Own": text}}},
			runsFrom: t0.Add(-24 * time.Hour), cursor: t0.Add(time.Hour)}
	}
	own := aloneRule{rule: rules.Rule{Name: "Own"}}
	c := aloneCase{in: in}
	if a := mk(); a.asks(own, c) {
		t.Error("covered, held: asked")
	}
	a := mk()
	a.cursor = t0 // the runs haven't got to the incident's end
	if !a.asks(own, c) {
		t.Error("past the cursor, held: not asked")
	}
	a = mk()
	a.runsFrom = t0 // began after the catch window starts
	if !a.asks(own, c) {
		t.Error("before the first run, held: not asked")
	}
	a = mk()
	a.failedServe = [][2]time.Time{{t0, t0.Add(time.Hour)}}
	if !a.asks(own, c) {
		t.Error("a run that couldn't replay it, held: not asked")
	}
	a = mk()
	a.bad = map[string]bool{"Own": true}
	if !a.asks(own, c) {
		t.Error("not replayable the last time, held: not asked")
	}
}

// Nothing is read, nor replayed, where no incident was lost, or the
// backtest's own replay didn't finish; with no time left, nothing is
// planned: the incidents are not known, counted limited.
func TestHoldPreparedNothingToHold(t *testing.T) {
	parallel(t)
	var s *Server // read nowhere: it would panic
	cmp := &verdict.Comparison{Before: &verdict.Report{}, After: &verdict.Report{}}
	held, err := s.holdPrepared(context.Background(), cmp, nil, time.Time{}, time.Time{}, verdict.Options{}, time.Hour)
	if err != nil || held == nil || held.rec != nil || held.limited != 0 {
		t.Errorf("no incident lost: %+v, %v", held, err)
	}
	if caught, known := held.catches(incident.Incident{}); caught || known {
		t.Error("an empty record said something")
	}
	cmp.Rules = []verdict.RuleChange{{After: &verdict.RuleResult{Unsupported: rules.Unfinished}}}
	if held, err = s.holdPrepared(context.Background(), cmp, nil, time.Time{}, time.Time{}, verdict.Options{}, time.Hour); held != nil || err != nil {
		t.Errorf("unfinished: %+v, %v", held, err)
	}
	s = &Server{btPort: &backtestPort{}}
	rec := &leftAlone{alone: []aloneRule{{rule: rules.Rule{Name: "A"}}}}
	h := &leftAloneHeld{rec: rec, answers: map[caughtAt]aloneAnswer{}}
	open := []aloneCase{{in: incident.Incident{ID: "INC-1"}}, {in: incident.Incident{ID: "INC-2"}, need: replayNeed{hard: true}}}
	if err := s.replayLeftAlone(context.Background(), h, nil, open, time.Time{}, time.Time{}, verdict.Options{}, 0); err != nil {
		t.Fatal(err)
	}
	if h.limited != 1 || len(h.answers) != 2 {
		t.Errorf("no time left: limited %d, answers %v", h.limited, h.answers)
	}
}

// Only a Grafana rule's firing on at the replay's start is cold, up to the
// first evaluation on its group's grid; a Prometheus rule's, whose state
// from the start is exact, began there.
func TestSplitCold(t *testing.T) {
	parallel(t)
	from := time.Date(2026, 11, 2, 5, 57, 0, 0, time.UTC)
	step := time.Minute
	graph := rules.Rule{Name: "G", Graph: &rules.Graph{}, Interval: 5 * time.Minute}
	prom := rules.Rule{Name: "Watchdog", Expr: "vector(1)"}
	fs := []firing.Firing{
		{Rule: "G", Start: from.Add(3 * time.Minute)},  // reported at 06:00, the first evaluation
		{Rule: "G", Start: from.Add(10 * time.Minute)}, // began later
		{Rule: "Watchdog", Start: from},
	}
	kept, cold := splitCold(fs, []rules.Rule{graph, prom}, from, step)
	if len(cold) != 1 || cold[0].Rule != "G" || cold[0].Start != fs[0].Start || len(kept) != 2 {
		t.Errorf("cold %v, kept %v", cold, kept)
	}
	// Every minute, a Grafana rule's first evaluation is the first step.
	graph.Interval = time.Minute
	_, cold = splitCold(fs[:1], []rules.Rule{graph}, from, step)
	if len(cold) != 0 {
		t.Errorf("a firing three steps in is cold: %v", cold)
	}
}

// A Prometheus rule on the whole time (for: 0) is no firing of unknown
// start: the incident it can't catch is missed, as lapilli verdict says.
func TestBacktestFailMissedWholeTimeRule(t *testing.T) {
	parallel(t)
	held := map[string]any{"days": 3, "fail_missed": true}
	always := "      - alert: Watchdog\n        expr: always\n        for: 0s\n"
	b := btServingOn(t, &history{}, rulesOf(errorsRule))
	_, rep := b.done(t, b.plain, btBody(rulesOf(errorsRule, always), rulesOf(errorsLate, always), held))
	sum, missed := missedOf(t, rep)
	if sum["incidents_missed"] != float64(1) || sum["missed_not_known"] != float64(0) || len(missed) != 1 {
		t.Errorf("summary %v, missed %v", sum, missed)
	}
}

// The rules a replay would ask are counted without those the record holds.
func TestReplayCountWithoutHeld(t *testing.T) {
	parallel(t)
	t0 := time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)
	in := incident.Incident{ID: "INC-1", Start: t0, End: t0.Add(30 * time.Minute)}
	far := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	text := aloneText{sum: [32]byte{1}, reach: 6 * time.Minute}
	a := &leftAlone{o: verdict.Options{Lead: 15 * time.Minute, UnknownLen: time.Hour}, read: true,
		want: map[string]aloneText{}, versions: []aloneVersion{{until: far, texts: map[string]aloneText{}}},
		runsFrom: t0.Add(-24 * time.Hour), cursor: t0.Add(time.Hour)}
	for i := range leftAloneRules + 100 {
		name := fmt.Sprintf("Own%d", i)
		a.serve = append(a.serve, rules.Rule{Name: name})
		a.want[name] = text
		a.versions[0].texts[name] = text
	}
	open := []aloneCase{{in: in}}
	if n := a.replayCount(open); n != 0 {
		t.Errorf("rules the record holds counted: %d", n)
	}
	a.cursor = t0 // the runs don't cover it: every one is replayed
	if n := a.replayCount(open); n != leftAloneRules+100 {
		t.Errorf("counted %d", n)
	}
}

// A read of the versions that fails leaves the record as the one before
// it gave it, which widen's longer read must not take away.
func TestReadVersionsFailsLeavesRecord(t *testing.T) {
	parallel(t)
	b := btServingOn(t, &history{}, rulesOf(errorsRule))
	t0 := time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)
	kept := []aloneVersion{{since: t0}}
	a := &leftAlone{o: verdict.Options{Lead: 15 * time.Minute}, from: t0, versions: kept, read: true,
		want: map[string]aloneText{}, same: map[string]bool{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.s.readVersions(ctx, a); err == nil {
		t.Fatal("a read past its context went on")
	}
	if !a.read || len(a.versions) != 1 {
		t.Errorf("the record was taken away: read %v, %d versions", a.read, len(a.versions))
	}
	if err := b.s.readVersions(context.Background(), a); err != nil || !a.read {
		t.Errorf("read again: %v, read %v", err, a.read)
	}
}
