package main

import (
	"testing"
	"time"
)

func TestStamp(t *testing.T) {
	at := func(ms int) time.Time { return time.Date(2026, 11, 25, 1, 0, 0, ms*1_000_000, time.UTC) }
	for _, c := range []struct {
		name string
		last []time.Time
		want int64
	}{
		{"no group has run", nil, 950_000_000},
		{"not run yet, as the API says", []time.Time{{}}, 950_000_000},
		{"half way past the latest", []time.Time{at(200), at(540), at(320)}, 770_000_000},
		{"late fractions are capped", []time.Time{at(985)}, 990_000_000},
		{"on the second", []time.Time{at(0)}, 500_000_000},
	} {
		if got := stamp(c.last); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}
