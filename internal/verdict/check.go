package verdict

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/rules"
)

// RuleCheck is one alert's replay beside what really fired.
type RuleCheck struct {
	Rule     string `json:"rule"`
	Replayed int    `json:"replayed"`
	Real     int    `json:"real"`
	// Alike are firings in both: the same alert, with labels that agree,
	// starting within Within of each other.
	Alike int `json:"alike"`
	// OffBy is the median of how far apart the starts of those were.
	OffBy  time.Duration `json:"off_by_ns"`
	Within time.Duration `json:"within_ns"`
	// Unsupported is why the rule couldn't be replayed; then there is
	// nothing to compare.
	Unsupported string `json:"unsupported,omitempty"`
}

// OnlyReplayed is how many firings the replay had that didn't really happen.
func (r RuleCheck) OnlyReplayed() int { return r.Replayed - r.Alike }

// OnlyReal is how many firings really happened that the replay didn't have.
func (r RuleCheck) OnlyReal() int { return r.Real - r.Alike }

// ReplayCheck sets a replay beside the series that say what really fired.
type ReplayCheck struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Series []string  `json:"series"`
	// The totals are of the rules replayed.
	Replayed int `json:"replayed"`
	Real     int `json:"real"`
	Alike    int `json:"alike"`
	// Rules are those that were replayed or really fired, those differing
	// most first.
	Rules []RuleCheck `json:"rules"`
	// Silent is how many rules fired neither way.
	Silent int `json:"silent"`
	// Elsewhere are alerts that really fired that no rule given is named:
	// rules loaded besides those given.
	Elsewhere []string `json:"elsewhere,omitempty"`
	// Unfinished says time ran out before every rule was compared: the
	// counts are of those that were.
	Unfinished bool `json:"unfinished,omitempty"`
}

// CheckReplay compares firings replayed from rules with firings read from
// the series that say what really fired. Each firing is matched at most once,
// to one of the same alert with labels that agree on what both have: what stores it adds to the series, such as a cluster's name, is
// passed over, and so is a rule's label with a template in it, which a replay
// doesn't fill in. Starts may be two evaluations apart: Prometheus places a
// group anywhere in its interval, and the series is read every step.
//
// It stops when ctx is done, and says so: firings that start together with
// labels that disagree are compared each with each.
func CheckReplay(ctx context.Context, known []rules.Rule, replayed, real []firing.Firing, from, to time.Time, step time.Duration) *ReplayCheck {
	c := &ReplayCheck{From: from, To: to, Rules: []RuleCheck{}}
	type alert struct {
		within      time.Duration
		unsupported string
		replayed    []firing.Firing
		real        []firing.Firing
	}
	alerts := map[string]*alert{}
	for _, r := range known {
		a := alerts[r.Name]
		if a == nil {
			a = &alert{}
			alerts[r.Name] = a
		}
		a.within = max(a.within, 2*max(step, r.Interval))
		// Of several rules sharing a name, one not replayed leaves the name
		// with nothing to compare.
		if a.unsupported == "" {
			a.unsupported = r.Unsupported
		}
	}
	clamp := func(fs []firing.Firing) []firing.Firing {
		out := make([]firing.Firing, 0, len(fs))
		for _, f := range fs {
			if !f.Active(from, to) {
				continue
			}
			// Both see one on since before the period as starting with it.
			if f.Start.Before(from) {
				f.Start = from
			}
			out = append(out, f)
		}
		return out
	}
	for _, f := range clamp(replayed) {
		if a := alerts[f.Rule]; a != nil {
			a.replayed = append(a.replayed, f)
		}
	}
	elsewhere := map[string]bool{}
	for _, f := range clamp(real) {
		if a := alerts[f.Rule]; a != nil {
			a.real = append(a.real, f)
		} else {
			elsewhere[f.Rule] = true
		}
	}
	c.Elsewhere = slices.Sorted(maps.Keys(elsewhere))
	for name, a := range alerts {
		if ctx.Err() != nil {
			c.Unfinished = true
			break
		}
		rc := RuleCheck{Rule: name, Within: a.within, Unsupported: a.unsupported, Real: len(a.real)}
		if a.unsupported == "" {
			var done bool
			if rc, done = matchFirings(ctx, rc, a.replayed, a.real, to); !done {
				c.Unfinished = true
				break
			}
			if rc.Replayed+rc.Real == 0 {
				c.Silent++
				continue
			}
			c.Replayed += rc.Replayed
			c.Real += rc.Real
			c.Alike += rc.Alike
		}
		c.Rules = append(c.Rules, rc)
	}
	sort.Slice(c.Rules, func(i, j int) bool {
		x, y := c.Rules[i], c.Rules[j]
		dx, dy := x.OnlyReplayed()+x.OnlyReal(), y.OnlyReplayed()+y.OnlyReal()
		if x.Unsupported != "" || y.Unsupported != "" {
			if (x.Unsupported != "") != (y.Unsupported != "") {
				return y.Unsupported != ""
			}
			return x.Rule < y.Rule
		}
		if dx != dy {
			return dx > dy
		}
		return x.Rule < y.Rule
	})
	return c
}

// matchFirings pairs an alert's replayed firings with those that really
// happened, the earliest of each with the earliest it can go with, which pairs
// as many as can be. A firing with no match that starts too near the end of the
// period to tell is left out: its match could be just past the end.
func matchFirings(ctx context.Context, rc RuleCheck, replayed, real []firing.Firing, to time.Time) (RuleCheck, bool) {
	firing.Sort(replayed)
	firing.Sort(real)
	used := make([]bool, len(real))
	var off []time.Duration
	unsure := to.Add(-rc.Within)
	for n, f := range replayed {
		if n%1024 == 1023 && ctx.Err() != nil {
			return rc, false
		}
		found := false
		i := sort.Search(len(real), func(i int) bool { return !real[i].Start.Before(f.Start.Add(-rc.Within)) })
		for ; i < len(real) && !real[i].Start.After(f.Start.Add(rc.Within)); i++ {
			if used[i] || !labelsAgree(f.Labels, real[i].Labels) {
				continue
			}
			used[i], found = true, true
			off = append(off, real[i].Start.Sub(f.Start).Abs())
			break
		}
		switch {
		case found:
			rc.Alike++
			rc.Replayed++
		case f.Start.Before(unsure):
			rc.Replayed++
		}
	}
	rc.Real = rc.Alike
	for i, f := range real {
		if !used[i] && f.Start.Before(unsure) {
			rc.Real++
		}
	}
	if len(off) > 0 {
		slices.Sort(off)
		rc.OffBy = off[len(off)/2]
	}
	return rc, true
}

// labelsAgree reports whether a replayed firing's labels agree with those of
// one that really happened, on what both have.
func labelsAgree(replayed, real map[string]string) bool {
	for k, v := range replayed {
		if w, ok := real[k]; ok && v != w && !strings.Contains(v, "{{") {
			return false
		}
	}
	return true
}

// CheckText writes a check of a replay for a person, times in loc.
func CheckText(w io.Writer, c *ReplayCheck, loc *time.Location) error {
	const stamp = "2006-01-02 15:04"
	fmt.Fprintf(w, "Replay beside %s  %s → %s %s\n", strings.Join(c.Series, " and "), c.From.In(loc).Format(stamp), c.To.In(loc).Format(stamp), loc)
	share := "–"
	if n := c.Replayed + c.Real - c.Alike; n > 0 {
		share = fmt.Sprintf("%.0f%%", 100*float64(c.Alike)/float64(n))
	}
	fmt.Fprintf(w, "%d firings alike, %d only in the replay, %d only in what really fired: %s alike.\n",
		c.Alike, c.Replayed-c.Alike, c.Real-c.Alike, share)
	var differ, unsupported []RuleCheck
	same := 0
	for _, r := range c.Rules {
		switch {
		case r.Unsupported != "":
			unsupported = append(unsupported, r)
		case r.OnlyReplayed()+r.OnlyReal() > 0:
			differ = append(differ, r)
		default:
			same++
		}
	}
	fmt.Fprintf(w, "%d rules fired alike, %d differ, %d fired neither way.\n", same, len(differ), c.Silent)
	if c.Unfinished {
		fmt.Fprintln(w, "Time ran out before every rule was compared: these are the rules that were.")
	}
	if len(differ) > 0 {
		fmt.Fprintln(w)
		rows := [][]string{{"RULE", "REPLAYED", "REAL", "ALIKE", "ONLY REPLAYED", "ONLY REAL", "OFF BY"}}
		for _, r := range differ {
			off := "–"
			if r.Alike > 0 {
				off = r.OffBy.Round(time.Second).String()
			}
			rows = append(rows, []string{Clean(r.Rule), fmt.Sprint(r.Replayed), fmt.Sprint(r.Real), fmt.Sprint(r.Alike),
				fmt.Sprint(r.OnlyReplayed()), fmt.Sprint(r.OnlyReal()), off})
		}
		if err := table(w, rows); err != nil {
			return err
		}
	}
	if len(unsupported) > 0 {
		fmt.Fprintf(w, "\nCould not replay %d rules:\n", len(unsupported))
		for _, r := range unsupported {
			fmt.Fprintf(w, "  %s: %s\n", Clean(r.Rule), Clean(r.Unsupported))
		}
	}
	if len(c.Elsewhere) > 0 {
		names := make([]string, 0, len(c.Elsewhere))
		for _, n := range c.Elsewhere {
			names = append(names, Clean(n))
		}
		fmt.Fprintf(w, "\nReally fired, but no rule given has the name: %s.\n", strings.Join(names, ", "))
	}
	return nil
}
