package sse

import (
	"math"
	"testing"
)

func TestFormat(t *testing.T) {
	for _, s := range []string{
		"$A", "${A B}", "$A + $B * $C", "($A + $B) * $C", "$A - ($B - $C)", "($A ^ $B) ^ $C", "$A ^ $B ^ $C",
		"abs($A - $B) > 5 && $C < 1 || !($D == 1)", "-($A + 1)", "$A > 0.9",
	} {
		e, err := ParseMath(s)
		if err != nil {
			t.Fatal(err)
		}
		f := Format(e)
		again, err := ParseMath(f)
		if err != nil {
			t.Fatalf("%s wrote %q, which doesn't parse: %v", s, f, err)
		}
		// Written again, it reads the same: the same value on any input.
		refs := func(r string) (Set, bool) { return Set{{V: float64(len(r)) + 1.5}}, true }
		if a, b := e.Eval(refs), again.Eval(refs); len(a) != 1 || len(b) != 1 || a[0].V != b[0].V && !(math.IsNaN(a[0].V) && math.IsNaN(b[0].V)) {
			t.Errorf("%s wrote %q: %v, then %v", s, f, a, b)
		}
	}
}

func TestComparisons(t *testing.T) {
	for s, want := range map[string][]string{
		"$A > 0.9":                       {"$A > 0.9"},
		"$A > 0.9 && $B < 10":            {"$A > 0.9", "$B < 10"},
		"($A > 1 || 2 <= $B) && $C >= 3": {"$A > 1", "$B >= 2", "$C >= 3"},
		"!($A > 1) && $B < 2":            {"$B < 2"}, // under ! it decides the other way
		"($A > 1) * $B":                  nil,        // inside arithmetic, not alone
		"$A > $B && $C > 1":              {"$C > 1"},
		"$A == 1 && $B > 1":              {"$B > 1"},
	} {
		e, err := ParseMath(s)
		if err != nil {
			t.Fatal(err)
		}
		cs := Comparisons(e)
		if len(cs) != len(want) {
			t.Errorf("%s: %+v, want %v", s, cs, want)
			continue
		}
		for i, c := range cs {
			if got := Format(Compare(c.Value, c.Op, c.N)); got != want[i] {
				t.Errorf("%s comparison %d: %s, want %s", s, i, got, want[i])
			}
		}
	}
}

func TestSetNumber(t *testing.T) {
	e, _ := ParseMath("$A > 0.9 && (2 <= $B || $C < 3)")
	got := Format(SetNumber(e, 1, 1.5))
	if got != "$A > 0.9 && ($B >= 1.5 || $C < 3)" {
		t.Errorf("set: %s", got)
	}
	if Format(SetNumber(e, 7, 1)) != Format(e) {
		t.Error("no comparison 7: the expression as it was")
	}
	if Format(e) != "$A > 0.9 && (2 <= $B || $C < 3)" {
		t.Errorf("the expression itself changed: %s", Format(e))
	}
}

func TestDecides(t *testing.T) {
	pods := func(vs ...float64) Set {
		var s Set
		for i, v := range vs {
			s = append(s, Item{Labels: lab("pod", string(rune('a'+i))), V: v})
		}
		return s
	}
	refs := func(m map[string]Set) func(string) (Set, bool) {
		return func(r string) (Set, bool) { s, ok := m[r]; return s, ok }
	}
	and, _ := ParseMath("$A > 0.9 && $B < 10")
	d, ok := Decides(and, 0)
	if !ok {
		t.Fatal("no comparison 0")
	}
	// pod a: B holds, so A decides; pod b: B doesn't, nothing would.
	got := d.Eval(refs(map[string]Set{"A": pods(0.8, 0.85), "B": pods(5, 50)}))
	if len(got) != 2 || got[0].V != 0.8 || !math.IsNaN(got[1].V) {
		t.Errorf("and: %+v", got)
	}
	or, _ := ParseMath("$A > 0.9 || $B < 10")
	d, _ = Decides(or, 0)
	// pod a: B holds, so it holds either way; pod b: A decides.
	got = d.Eval(refs(map[string]Set{"A": pods(0.8, 0.85), "B": pods(5, 50)}))
	if len(got) != 2 || !math.IsInf(got[0].V, 1) || got[1].V != 0.85 {
		t.Errorf("or: %+v", got)
	}
	d, _ = Decides(or, 1)
	got = d.Eval(refs(map[string]Set{"A": pods(0.95, 0.85), "B": pods(5, 50)}))
	if len(got) != 2 || !math.IsInf(got[0].V, -1) || got[1].V != 50 {
		t.Errorf("or, below: %+v", got)
	}
	// A threshold by namespace set beside values by pod.
	byNS := Set{{Labels: lab("ns", "x"), V: 1}}
	withNS := Set{{Labels: lab("pod", "a", "ns", "x"), V: 0.5}, {Labels: lab("pod", "b", "ns", "x"), V: 2}}
	gated, _ := ParseMath("$A > 1 && $G > 0")
	d, _ = Decides(gated, 0)
	got = d.Eval(refs(map[string]Set{"A": withNS, "G": byNS}))
	if len(got) != 2 || got[0].V != 0.5 || got[1].V != 2 {
		t.Errorf("joined: %+v", got)
	}
	if _, ok := Decides(and, 2); ok {
		t.Error("comparison 2 of two")
	}
}
