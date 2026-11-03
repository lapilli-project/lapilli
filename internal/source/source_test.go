package source

import (
	"slices"
	"testing"

	"lapilli.dev/internal/rules"
)

func TestTellApart(t *testing.T) {
	rs := []rules.Rule{
		{Name: "Errors", Group: "api", Labels: map[string]string{"severity": "page"}},
		{Name: "Errors", Group: "api", Labels: map[string]string{"severity": "ticket"}},
		{Name: "Disk", Group: "a"},
		{Name: "Disk", Group: "b"},
		{Name: "Twin", Group: "a"},
		{Name: "Twin", Group: "a"},
		{Name: "Alone", Group: "a"},
	}
	TellApart(rs)
	var got []string
	for _, r := range rs {
		got = append(got, r.Name)
	}
	want := []string{"Errors [severity=page]", "Errors [severity=ticket]", "Disk [a]", "Disk [b]", "Twin", "Twin", "Alone"}
	if !slices.Equal(got, want) {
		t.Errorf("names %q, want %q", got, want)
	}
}

func TestTellApartAcrossSets(t *testing.T) {
	page := map[string]string{"severity": "page"}
	was := []rules.Rule{{Name: "Errors", Expr: "x", Labels: page}, {Name: "Errors", Expr: "x", Labels: map[string]string{"severity": "ticket"}}}
	is := []rules.Rule{{Name: "Errors", Expr: "x", Labels: page}}
	TellApart(is, was)
	same, before, after := Split(was, is)
	if len(same) != 1 || len(before) != 1 || len(after) != 0 || is[0].Name != "Errors [severity=page]" {
		t.Errorf("the page rule, unchanged, is the same in both: same %v, before %v, after %v", same, before, after)
	}
	// A rule whose severity changed, alone in each set, keeps its name.
	a, b := []rules.Rule{{Name: "Disk", Labels: page}}, []rules.Rule{{Name: "Disk", Labels: map[string]string{"severity": "ticket"}}}
	TellApart(a, b)
	if a[0].Name != "Disk" || b[0].Name != "Disk" {
		t.Errorf("names %q %q", a[0].Name, b[0].Name)
	}
}
