package source

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// levels answers conditions of a pod's level and load, each compared with a
// number, joined by and, or and unless, with no brackets, and up == 1, which
// holds of every pod: for thirty minutes every two hours over the first
// three days, each pod at its values, and nothing between.
type levels map[string][2]float64

func (l levels) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	var out []prom.Series
	for _, pod := range []string{"a", "b", "c"} {
		v, ok := l[pod]
		if !ok || !holds(expr, v) {
			continue
		}
		s := prom.Series{Labels: map[string]string{"pod": pod}}
		for k := range 36 {
			at := t0.Add(time.Duration(k) * 2 * time.Hour)
			for t := at; t.Before(at.Add(30 * time.Minute)); t = t.Add(step) {
				if !t.Before(start) && !t.After(end) {
					s.Samples = append(s.Samples, prom.Sample{T: t, V: v[0]})
				}
			}
		}
		if len(s.Samples) > 0 {
			out = append(out, s)
		}
	}
	return out, nil
}

// holds reports whether expr holds of a pod at level v[0] and load v[1]:
// or binds least, and and unless from the left.
func holds(expr string, v [2]float64) bool {
	for _, part := range strings.Split(expr, " or ") {
		words := strings.Fields(part)
		ok := leaf(words[:3], v)
		for i := 3; i+4 <= len(words); i += 4 {
			next := leaf(words[i+1:i+4], v)
			if words[i] == "and" {
				ok = ok && next
			} else {
				ok = ok && !next
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func leaf(w []string, v [2]float64) bool {
	x := map[string]float64{"level": v[0], "load": v[1], "up": 1}[w[0]]
	n, _ := strconv.ParseFloat(w[2], 64)
	switch w[1] {
	case ">":
		return x > n
	case "<":
		return x < n
	case "==":
		return x == n
	}
	return false
}

// levelsWeek is the week of l with a rule of expr replayed, and nothing
// written down: what its firings were, as Unrecorded says it.
func levelsWeek(t *testing.T, l levels, expr string) verdict.Unrecorded {
	t.Helper()
	o := verdict.Options{From: t0, To: t0.Add(7 * 24 * time.Hour), Lead: 15 * time.Minute, Match: []string{"service"}, RefireWithin: time.Hour, UnknownLen: time.Hour}
	versions := []replay.Version{{Rules: []rules.Rule{{Name: "Level", Expr: expr, For: time.Minute}}}}
	known, fs := replay.Run(context.Background(), l, versions, o.From, o.To, time.Minute, 2)
	rep := verdict.Judge(known, fs, nil, o)
	us, err := Unrecorded(context.Background(), l, versions, rep, Judged(rep.Firings), time.Minute)
	if err != nil || len(us) != 1 {
		t.Fatalf("%s: %+v %v", expr, us, err)
	}
	return us[0]
}

// With no record, what a rule's firings were is said of a range and of
// comparisons joined by and, or and unless, as of one bound: those that
// held with every bound that decided them a fifth further happened. Each
// is replayed with every bound further the way the condition holds less,
// on the right of unless the way its comparison holds more. Pod a goes well
// past every bound; pod b flickers at one edge, and is counted with a only
// where the rest of the condition holds it.
func TestUnrecordedBounds(t *testing.T) {
	for _, c := range []struct {
		name, expr string
		l          levels
		clear      int
		bound      string
	}{
		{"a range", "level > 1 and level < 5", levels{"a": {3, 0}, "b": {1.1, 0}, "c": {4.9, 0}}, 36, "level > 1, level < 5"},
		{"and", "level > 1 and load > 10", levels{"a": {3, 20}, "b": {3, 10.5}}, 36, "level > 1, load > 10"},
		{"or, either side well past", "level > 2 or load > 10", levels{"a": {3, 0}, "b": {2.1, 0}, "c": {0, 20}}, 72, "level > 2, load > 10"},
		{"unless, its right held further", "level > 1 unless load > 10", levels{"a": {3, 0}, "b": {3, 9}}, 36, "level > 1, unless load > 10"},
		{"and with what isn't compared", "up == 1 and level > 1", levels{"a": {3, 0}, "b": {1.1, 0}}, 36, "> 1"},
	} {
		u := levelsWeek(t, c.l, c.expr)
		if u.Events || u.Clear == nil || *u.Clear != c.clear || u.Bound != c.bound || u.Firings != 36*len(c.l) {
			t.Errorf("%s: %+v", c.name, u)
			continue
		}
		if strings.Contains(c.bound, ",") != (u.Bounds > 1) {
			t.Errorf("%s: %d bounds", c.name, u.Bounds)
		}
		if u.Bounds > 1 && !strings.Contains(u.Text(), strconv.Itoa(c.clear)+" of them went well past the bounds that decided them ("+c.bound+"), not a flicker at an edge: these happened.") {
			t.Errorf("%s: %q", c.name, u.Text())
		}
	}

	// Left unsaid, as it can't be said soundly: or beside a value no number
	// decides, unless with nothing compared on its left, a range narrower
	// than two fifths of its ends, which set further hold nowhere, and a
	// bound of nothing.
	for _, c := range []struct{ name, expr string }{
		{"or beside up", "level > 1 or up == 1"},
		{"unless of up", "up == 1 unless level > 5"},
		{"a narrow range", "level > 1 and level < 1.2"},
		{"a bound of nothing", "level > 0 and load > 10"},
	} {
		u := levelsWeek(t, levels{"a": {3, 20}, "b": {1.1, 20}}, c.expr)
		if u.Clear != nil || u.Bound != "" || u.Bounds != 0 {
			t.Errorf("%s: said %+v", c.name, u)
		}
		if strings.Contains(u.Text(), "well past") || strings.Contains(u.Text(), "would hide") {
			t.Errorf("%s: %q", c.name, u.Text())
		}
	}

	// None that went well past them, said of several: not a fifth inside.
	u := levelsWeek(t, levels{"b": {1.1, 0}, "c": {4.9, 0}}, "level > 1 and level < 5")
	if u.Clear == nil || *u.Clear != 0 || !strings.Contains(u.Text(), "None stayed more than a fifth past the bounds that decide it (level > 1, level < 5) for all of its for.") {
		t.Errorf("none: %+v %q", u, u.Text())
	}
}

// A bound a fifth past which is no float (1.7e308 is +Inf a fifth further)
// can't be set past: the rule with it so would be another, and the bound
// after it left where it was. Nothing is said of it; of one past which is
// a float, as before.
func TestDecidingFinite(t *testing.T) {
	for _, c := range []struct {
		expr string
		ok   bool
	}{
		{"x > 1.7e308 or y > 5", false},
		{"y > 5 and x < -1.7e308", false},
		{"x > 1e307 or y > 5", true},
	} {
		if _, ok := deciding(rules.Rule{Name: "R", Expr: c.expr}); ok != c.ok {
			t.Errorf("%s: %v, want %v", c.expr, ok, c.ok)
		}
	}
}
