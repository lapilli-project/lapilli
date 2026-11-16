package serve

import (
	"maps"
	"slices"

	"lapilli.dev/internal/incident"
)

// What labels summed up show at most: the values of a name with several,
// on its line; and the labels listed behind them, one a line.
const (
	labelsShown  = 3
	labelsListed = 200
)

// labelSum is a window's or an incident's labels as a page sums them up.
// A window found in the impact signal merges every series it overlaps,
// with every value of each label they all have: as one line of name=value
// it runs on for as many series as it merged. Summed up, each name is one
// line, by name: one with one value as name=value, one with more as its
// name, how many it has (N), and the first labelsShown of them. When a
// name has more than it shows (Cut), All is every name=value, by name and
// value, the first labelsListed of Total, and More how many past them.
type labelSum struct {
	Names []summedLabel
	Cut   bool
	All   []string
	Total int
	More  int
}

// summedLabel is a name of a labelSum, with how many values it has and
// those shown.
type summedLabel struct {
	Name  string
	N     int
	Shown []string
}

// shownValue is a value shown of a name with several, with what follows it
// on its line (After): a comma before the next, ", …" after the last when
// the name has more. The page keeps them together, so that no line starts
// with a comma.
type shownValue struct{ V, After string }

// Values are the values shown, each with what follows it.
func (n summedLabel) Values() []shownValue {
	out := make([]shownValue, len(n.Shown))
	for i, v := range n.Shown {
		out[i].V = v
		switch {
		case i < len(n.Shown)-1:
			out[i].After = ","
		case n.N > len(n.Shown):
			out[i].After = ", …"
		}
	}
	return out
}

// labelsum sums up an incident's labels (incident.Labels: each name with
// its values), or a window's found in the impact signal; the template of
// the same name shows it. Each name's values are sorted, each once,
// whatever order they were written in, so that the same labels sum up the
// same.
func labelsum(l incident.Labels) labelSum {
	var s labelSum
	for _, k := range slices.Sorted(maps.Keys(l)) {
		vs := slices.Compact(slices.Sorted(slices.Values(l[k])))
		if len(vs) == 0 {
			continue
		}
		s.Names = append(s.Names, summedLabel{Name: k, N: len(vs), Shown: vs[:min(len(vs), labelsShown)]})
		s.Cut = s.Cut || len(vs) > labelsShown
		for _, v := range vs[:min(len(vs), max(0, labelsListed-len(s.All)))] {
			s.All = append(s.All, k+"="+v)
		}
		s.Total += len(vs)
	}
	s.More = s.Total - len(s.All)
	return s
}
