package sse

import "testing"

func TestCanon(t *testing.T) {
	canon := func(s string) string {
		t.Helper()
		e, err := ParseMath(s)
		if err != nil {
			t.Fatal(err)
		}
		c, ok := Canon(e)
		if !ok {
			t.Fatalf("%q: no Canon", s)
		}
		return c
	}
	want := canon("$A / $B > 0.5")
	for _, same := range []string{"${A}/${B}>.5", "($A / $B) > 0.50", "$A  /  $B  >  5e-1", "(($A / $B) > 5E-1)"} {
		if got := canon(same); got != want {
			t.Errorf("%q: %s, want %s", same, got, want)
		}
	}
	for _, other := range []string{"$A / $B > 0.6", "$A * $B > 0.5", "$B / $A > 0.5", "$A / ($B > 0.5)", "abs($A) / $B > 0.5", "-$A / $B > 0.5", "!$A"} {
		if got := canon(other); got == want {
			t.Errorf("%q: the same Canon", other)
		}
	}
	if _, ok := Canon(forced{e: ref("A")}); ok {
		t.Error("a forced comparison has a Canon")
	}
	if _, ok := Canon(nil); ok {
		t.Error("nil has a Canon")
	}
}
