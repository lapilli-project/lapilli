package serve

import (
	"fmt"
	"hash/maphash"
	"maps"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
)

// withinSeries counts a rule's alerts by labelsHash, which wrote each
// label as name, NUL, value, NUL with nothing escaped: a value holding NULs
// read as the labels after it; each is written after its length now. A hostile API answers with series whose
// label values hold "\x00" (JSON's "\u0000"), so that 2^(n-1) different
// label sets of n labels hash alike, and are counted as one alert. They
// stay apart in the ledger (keptLabels turns the NUL into U+FFFD, a set
// apart from the one it reads as), so the 20,000 alerts a rule's replay
// is kept with (CHANGELOG 0.34.1, docs/design/serve.md) don't bound it.
func TestWithinSeriesSeparatorCollision(t *testing.T) {
	const n = 16 // 2^15 = 32,768 label sets, past maxRuleSeries (20,000)
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("l%02d", i) // sorted as labelsHash sorts them
	}
	var fs []firing.Firing
	seen := map[string]bool{}
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for mask := range 1 << (n - 1) {
		// Bit i set: names[i+1] starts a label of its own; else it, and
		// its value, are part of the value before it.
		m := map[string]string{}
		key, val := names[0], []string{"v00"}
		for i := 1; i < n; i++ {
			v := fmt.Sprintf("v%02d", i)
			if mask&(1<<(i-1)) != 0 {
				m[key] = strings.Join(val, "\x00")
				key, val = names[i], []string{v}
			} else {
				val = append(val, names[i], v)
			}
		}
		m[key] = strings.Join(val, "\x00")
		seen[fmt.Sprint(m)] = true
		fs = append(fs, firing.Firing{Rule: "r", Labels: m, Start: start})
	}
	if len(seen) != 1<<(n-1) {
		t.Fatalf("made %d label sets apart, want %d", len(seen), 1<<(n-1))
	}
	kept, over := withinSeries(fs)
	if !over["r"] {
		t.Errorf("%d alerts of one rule, told apart by their labels, are kept (%d firings), past the %d a rule's replay in a run keeps: labelsHash takes a value's NULs for the labels after it",
			len(seen), len(kept), maxRuleSeries)
	}
}

// labelsHash is seeded: under two seeds, a set hashes apart, as a hash an
// API could know (FNV-64a, unseeded) doesn't.
func TestLabelsHashSeeded(t *testing.T) {
	a, b := maphash.MakeSeed(), maphash.MakeSeed()
	for _, m := range []map[string]string{{}, {"a": "x"}, {"alertname": "Errors", "pod": "api-7d8f9"}} {
		if labelsHash(a, m) == labelsHash(b, m) {
			t.Errorf("%v hashes alike under two seeds", m)
		}
		if labelsHash(a, m) != labelsHash(a, maps.Clone(m)) {
			t.Errorf("%v hashes apart under one seed", m)
		}
	}
}
