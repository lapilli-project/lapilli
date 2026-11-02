package incident

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
)

var t0 = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.yaml")
	os.WriteFile(good, []byte(`incidents:
  - id: INC-1
    title: Checkout errors
    start: 2026-10-30T17:00:00+09:00
    end: 2026-10-30T17:40:00+09:00
    labels: {service: checkout}
  - id: INC-2
    start: 2026-10-31T01:00:00Z
`), 0o600)
	got, err := Load(good)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].Start.Equal(time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)) || got[0].Labels["service"] != "checkout" || !got[1].End.IsZero() {
		t.Errorf("%+v", got)
	}
	for body, want := range map[string]string{
		"incidents:\n  - start: 2026-10-31T01:00:00Z\n": "no id",
		"incidents:\n  - id: X\n":                       "no start",
		"incidents:\n  - id: X\n    start: 2026-10-31T01:00:00Z\n    end: 2026-10-30T01:00:00Z\n": "ends before",
	} {
		p := filepath.Join(dir, "bad.yaml")
		os.WriteFile(p, []byte(body), 0o600)
		if _, err := Load(p); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err %v, want %q", body, err, want)
		}
	}
}

func TestFromImpact(t *testing.T) {
	min := func(ms ...int) []prom.Sample {
		var out []prom.Sample
		for _, m := range ms {
			out = append(out, prom.Sample{T: t0.Add(time.Duration(m) * time.Minute), V: 1})
		}
		return out
	}
	series := []prom.Series{
		{Samples: min(10, 11, 12, 14, 15, 16)}, // one gap of a step: one window, 10..17
		{Samples: min(13)},                     // another series filling in
		{Samples: min(40, 41)},                 // too short at 5m
		{Samples: min(50, 51, 52, 53, 54, 55)}, // long enough
	}
	got := FromImpact(series, time.Minute, 5*time.Minute)
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if !got[0].Start.Equal(t0.Add(10*time.Minute)) || !got[0].End.Equal(t0.Add(17*time.Minute)) || !got[0].Candidate {
		t.Errorf("first window %+v", got[0])
	}
}

func TestMerge(t *testing.T) {
	written := []Incident{{ID: "INC-1", Start: t0.Add(10 * time.Minute)}}
	cands := []Incident{
		{ID: "impact-1", Start: t0.Add(30 * time.Minute), End: t0.Add(40 * time.Minute), Candidate: true}, // inside INC-1's assumed hour
		{ID: "impact-2", Start: t0.Add(3 * time.Hour), End: t0.Add(4 * time.Hour), Candidate: true},
	}
	got := Merge(written, cands, time.Hour)
	if len(got) != 2 || got[1].ID != "impact-2" {
		t.Errorf("%+v", got)
	}
}
