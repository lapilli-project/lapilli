package sse

import (
	"maps"
	"math"
	"strconv"
	"testing"
)

func set(items ...Item) Set { return items }

func lab(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func eval(t *testing.T, expr string, refs map[string]Set) Set {
	t.Helper()
	e, err := ParseMath(expr)
	if err != nil {
		t.Fatal(err)
	}
	return e.Eval(func(r string) (Set, bool) { s, ok := refs[r]; return s, ok })
}

func TestPrecedenceAndComparisons(t *testing.T) {
	one := func(v float64) Set { return set(Item{V: v}) }
	for expr, want := range map[string]float64{
		"1 + 2 * 3":            7,
		"(1 + 2) * 3":          9,
		"2 ^ 3 ^ 2":            512, // right-associative
		"-2 ^ 2":               4,
		"10 > 3 && 2 > 5":      0,
		"10 > 3 || 2 > 5":      1,
		"!(1 > 2)":             1,
		"$A * ($B >= 20)":      30, // A=30, B=25
		"abs(-$A) + round(.4)": 30,
		"1e3 / 4":              250,
	} {
		got := eval(t, expr, map[string]Set{"A": one(30), "B": one(25)})
		if len(got) != 1 || got[0].V != want {
			t.Errorf("%s = %v, want %v", expr, got, want)
		}
	}
}

func TestRefsAndErrors(t *testing.T) {
	e, err := ParseMath("$D > 10 && ${F} > ($H + 5)")
	if err != nil {
		t.Fatal(err)
	}
	if r := e.Refs(); len(r) != 3 || r[0] != "D" || r[1] != "F" || r[2] != "H" {
		t.Errorf("refs %v", r)
	}
	for _, bad := range []string{"$A >", "($A", "nope($A)", "$A $B", "$", "${A", "1 @ 2"} {
		if _, err := ParseMath(bad); err == nil {
			t.Errorf("ParseMath(%q) took it", bad)
		}
	}
}

func TestJoin(t *testing.T) {
	a := set(Item{Labels: lab("pod", "a", "ns", "x"), V: 10}, Item{Labels: lab("pod", "b", "ns", "x"), V: 1})
	byNS := set(Item{Labels: lab("ns", "x"), V: 4})
	got := eval(t, "$A > $T", map[string]Set{"A": a, "T": byNS})
	if len(got) != 2 || got[0].V != 1 || got[1].V != 0 || got[0].Labels["pod"] != "a" {
		t.Errorf("subset join: %+v", got)
	}
	none := eval(t, "$A / $B", map[string]Set{"A": a, "B": set(Item{Labels: lab("pod", "zzz"), V: 1})})
	if len(none) != 0 {
		t.Errorf("no labels in common should pair nothing: %+v", none)
	}
	div := eval(t, "$A / 0", map[string]Set{"A": set(Item{V: 0})})
	if !math.IsNaN(div[0].V) {
		t.Errorf("0/0 = %v, want NaN", div[0].V)
	}
}

// TestJoinEven checks the pairing by key against pairing each with each.
func TestJoinEven(t *testing.T) {
	slow := func(l, r Set) Set {
		var out Set
		for _, a := range l {
			for _, b := range r {
				switch {
				case maps.Equal(a.Labels, b.Labels), subset(b.Labels, a.Labels):
					out = append(out, Item{Labels: a.Labels, V: a.V - b.V})
				case subset(a.Labels, b.Labels):
					out = append(out, Item{Labels: b.Labels, V: a.V - b.V})
				}
			}
		}
		return out
	}
	mk := func(seed int) Set {
		var s Set
		for i := range 12 {
			v := (i*7 + seed*3) % 5
			l := lab("pod", strconv.Itoa(v), "ns", strconv.Itoa(v%2))
			if i%4 == 0 {
				l = lab("pod", strconv.Itoa(v), "zone", "a") // same size, other names
			}
			if i%5 == 0 {
				l = lab("pod", strconv.Itoa(v), "node", "") // an empty value is not a missing label
			}
			it := Item{Labels: l, V: float64(i)}
			if i%3 == 0 {
				it.Key = LabelKey(l)
			}
			s = append(s, it)
		}
		return s
	}
	for seed := range 5 {
		l, r := mk(seed), mk(seed+1)
		got := Join(l, r, func(x, y float64) float64 { return x - y })
		want := slow(l, r)
		if len(got) != len(want) {
			t.Fatalf("seed %d: %d pairs, want %d", seed, len(got), len(want))
		}
		for i := range got {
			if !maps.Equal(got[i].Labels, want[i].Labels) || got[i].V != want[i].V {
				t.Errorf("seed %d pair %d: %+v, want %+v", seed, i, got[i], want[i])
			}
		}
	}
}
