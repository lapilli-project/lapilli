package serve

import (
	"net/url"
)

// A diff with display options (docs/design/ui.md), as Argo CD shows a
// change: unified or side by side (?diff=unified|split), the lines around
// each change or the whole (?context=3|all). A proposal's page shows its
// change so, and a rule's versions tab each version beside the one before.

// diffContext is how many lines the same are shown around a change, unless
// the whole is asked for.
const diffContext = 3

// diffOpts are how a diff is shown: Split, side by side; All, every line,
// not only those around a change. Unknown values are the defaults.
type diffOpts struct {
	Split, All bool
}

// diffOptsOf reads ?diff= and ?context=: split and all, or else unified
// and three lines around each change. What else was asked is never shown.
func diffOptsOf(q url.Values) diffOpts {
	return diffOpts{Split: q.Get("diff") == "split", All: q.Get("context") == "all"}
}

// params are the options as a URL has them: the defaults left out.
func (o diffOpts) params() (diff, context string) {
	if o.Split {
		diff = "split"
	}
	if o.All {
		context = "all"
	}
	return diff, context
}

// diffRow is a line of a diff shown unified: Op is "-" for one taken out,
// "+" for one put in, " " for one the same; Skip, when set, is a run of
// lines the same left out, and the row is that alone.
type diffRow struct {
	Op, Text string
	Skip     int
}

// splitRow is a row of a diff shown side by side: the line as it was and
// as it is, either missing where the other was put in or taken out. Op and
// NewOp are each side's sign ("-", "+" or " ", "" for none); Skip as a
// diffRow's.
type splitRow struct {
	Old, New     string
	OldOp, NewOp string
	Skip         int
}

// diffView is a diff as a page shows it, in its options: Rows unified,
// Pairs side by side, one of the two. Whole says a side was past diffMost
// lines: the one taken out whole, then the other put in. Same says nothing
// changed.
type diffView struct {
	Opts  diffOpts
	Rows  []diffRow
	Pairs []splitRow
	Whole bool
	Same  bool
}

// lineDiff is a before b, line by line by their longest common run, as
// versionDiff has it: past diffMost lines a side, whole.
func lineDiff(a, b []string) (out []diffLine, whole bool) {
	if len(a) > diffMost || len(b) > diffMost {
		for _, l := range a {
			out = append(out, diffLine{"-", l})
		}
		for _, l := range b {
			out = append(out, diffLine{"+", l})
		}
		return out, true
	}
	// lcs[i][j] is the longest common run of a[i:] and b[j:].
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			out = append(out, diffLine{" ", a[i]})
			i, j = i+1, j+1
		case j == len(b) || i < len(a) && lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, diffLine{"-", a[i]})
			i++
		default:
			out = append(out, diffLine{"+", b[j]})
			j++
		}
	}
	return out, false
}

// shapeDiff is a diff's lines in the options asked: the lines the same
// more than diffContext from a change left out, as one row saying how many,
// unless All; side by side, a run taken out set beside the run put in
// after it, line by line.
func shapeDiff(lines []diffLine, whole bool, o diffOpts) diffView {
	v := diffView{Opts: o, Whole: whole, Same: true}
	near := make([]bool, len(lines))
	for i, l := range lines {
		if l.Op == " " {
			continue
		}
		v.Same = false
		for k := max(0, i-diffContext); k <= min(len(lines)-1, i+diffContext); k++ {
			near[k] = true
		}
	}
	// Rows unified: a run of lines left out is one row.
	var rows []diffRow
	for i := 0; i < len(lines); {
		if o.All || v.Same || near[i] {
			rows = append(rows, diffRow{Op: lines[i].Op, Text: lines[i].Text})
			i++
			continue
		}
		j := i
		for j < len(lines) && !near[j] {
			j++
		}
		rows = append(rows, diffRow{Skip: j - i})
		i = j
	}
	if !o.Split {
		v.Rows = rows
		return v
	}
	for i := 0; i < len(rows); {
		r := rows[i]
		switch {
		case r.Skip > 0:
			v.Pairs = append(v.Pairs, splitRow{Skip: r.Skip})
			i++
		case r.Op == " ":
			v.Pairs = append(v.Pairs, splitRow{Old: r.Text, New: r.Text, OldOp: " ", NewOp: " "})
			i++
		default:
			// The run taken out, and the run put in after it, side by side.
			var out, in []string
			for i < len(rows) && rows[i].Skip == 0 && rows[i].Op == "-" {
				out = append(out, rows[i].Text)
				i++
			}
			for i < len(rows) && rows[i].Skip == 0 && rows[i].Op == "+" {
				in = append(in, rows[i].Text)
				i++
			}
			for k := 0; k < max(len(out), len(in)); k++ {
				var p splitRow
				if k < len(out) {
					p.Old, p.OldOp = out[k], "-"
				}
				if k < len(in) {
					p.New, p.NewOp = in[k], "+"
				}
				v.Pairs = append(v.Pairs, p)
			}
		}
	}
	return v
}

// diffShown is a diff as a page shows it, with the names of its sides
// (texts to translate), as the side-by-side table heads them.
type diffShown struct {
	D        diffView
	Old, New string
}

// OldClass and NewClass are a side's line's class: taken out, put in, the
// same, or none.
func (p splitRow) OldClass() string { return opClass(p.OldOp) }
func (p splitRow) NewClass() string { return opClass(p.NewOp) }

// Class is a unified line's class.
func (r diffRow) Class() string { return opClass(r.Op) }

func opClass(op string) string {
	switch op {
	case "-":
		return "del"
	case "+":
		return "ins"
	case " ":
		return "same"
	}
	return "none"
}

// diffLink is a way to show a diff, as a link to the same page in it.
type diffLink struct {
	Label, Href string
	Current     bool
}

// diffLinks are the diff's options as two pairs of links, each the page
// (link, given the diff= and context= it is to have) in one of them, the
// other option as it is.
func diffLinks(o diffOpts, link func(diff, context string) string) [2][]diffLink {
	at := func(x diffOpts) string { return link(x.params()) }
	return [2][]diffLink{
		{{"Unified", at(diffOpts{false, o.All}), !o.Split}, {"Side by side", at(diffOpts{true, o.All}), o.Split}},
		{{"Around each change", at(diffOpts{o.Split, false}), !o.All}, {"Every line", at(diffOpts{o.Split, true}), o.All}},
	}
}
