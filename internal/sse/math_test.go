package sse

import (
	"math"
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
	a := set(Item{lab("pod", "a", "ns", "x"), 10}, Item{lab("pod", "b", "ns", "x"), 1})
	byNS := set(Item{lab("ns", "x"), 4})
	got := eval(t, "$A > $T", map[string]Set{"A": a, "T": byNS})
	if len(got) != 2 || got[0].V != 1 || got[1].V != 0 || got[0].Labels["pod"] != "a" {
		t.Errorf("subset join: %+v", got)
	}
	none := eval(t, "$A / $B", map[string]Set{"A": a, "B": set(Item{lab("pod", "zzz"), 1})})
	if len(none) != 0 {
		t.Errorf("no labels in common should pair nothing: %+v", none)
	}
	div := eval(t, "$A / 0", map[string]Set{"A": set(Item{V: 0})})
	if !math.IsNaN(div[0].V) {
		t.Errorf("0/0 = %v, want NaN", div[0].V)
	}
}
