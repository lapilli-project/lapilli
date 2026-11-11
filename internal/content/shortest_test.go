package content

import (
	"testing"
	"time"
)

// F2: the interval a replay asks a query's samples at: the shortest any
// series it reads is scraped at, by the jobs its selectors name.
func TestShortest(t *testing.T) {
	s := Scrapes{Global: time.Minute, Jobs: map[string]time.Duration{"api": 15 * time.Second, "db": 30 * time.Second, "batch": 2 * time.Minute}}
	for expr, want := range map[string]time.Duration{
		`up{job="db"}`: 30 * time.Second,
		`max by (job) (rate(x{job=~"db|batch"}[5m]))`: 30 * time.Second,
		`x{job="batch"} / on(job) y{job="db"}`:        30 * time.Second,
		`x{job="batch"}`:                              2 * time.Minute,
		`x{job="unknown"}`:                            time.Minute,
		`x{job!="api"}`:                               15 * time.Second,
		`sum by (instance) (x)`:                       15 * time.Second,
		`x{job="batch"} and on() y`:                   15 * time.Second,
		`vector(1) and on() (count_over_time(vector(1)[1m:1s]) > 100)`: time.Minute,
		`fv_spiky * 0 + 1`: 15 * time.Second,
	} {
		if got := s.Shortest(expr); got != want {
			t.Errorf("%s: %s, want %s", expr, got, want)
		}
	}
	if got := (Scrapes{Global: 30 * time.Second}).Shortest("x"); got != 30*time.Second {
		t.Errorf("no jobs: %s", got)
	}
	if got := (Scrapes{}).Shortest("x"); got != 0 {
		t.Errorf("not known: %s", got)
	}
}

// Any expression, read for the jobs its selectors name: never a panic, and
// an interval among those known.
func FuzzShortest(f *testing.F) {
	for _, seed := range []string{`up{job="db"}`, `max by (job) (rate(x{job=~"db|batch"}[5m]))`, `x{job="a"} / on(job) group_left(z) y`,
		`sum(x{job=~"("})`, `{`, `}`, `by (`, `x{job="a"`, "vector(1)", `count_over_time(vector(1)[1m:1s])`} {
		f.Add(seed)
	}
	s := Scrapes{Global: time.Minute, Jobs: map[string]time.Duration{"api": 15 * time.Second, "db": 30 * time.Second}}
	f.Fuzz(func(t *testing.T, expr string) {
		switch d := s.Shortest(expr); d {
		case time.Minute, 15 * time.Second, 30 * time.Second:
		default:
			t.Errorf("%q: %s", expr, d)
		}
	})
}
