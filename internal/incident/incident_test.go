package incident

import (
	"os"
	"path/filepath"
	"slices"
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
    labels: {service: [checkout, cart], team: shop}
`), 0o600)
	got, err := Load(good)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].Start.Equal(time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)) || !slices.Equal(got[0].Labels["service"], []string{"checkout"}) || !got[1].End.IsZero() {
		t.Errorf("%+v", got)
	}
	if l := got[1].Labels; !slices.Equal(l["service"], []string{"checkout", "cart"}) || !slices.Equal(l["team"], []string{"shop"}) {
		t.Errorf("labels %v", l)
	}
	if !got[1].Labels.Agree(map[string]string{"service": "cart"}, []string{"service"}) ||
		got[1].Labels.Agree(map[string]string{"service": "search"}, []string{"service"}) ||
		!got[1].Labels.Agree(map[string]string{"pod": "x"}, []string{"service"}) {
		t.Error("Agree: want any listed value to agree, another to disagree, and no label no constraint")
	}
	for body, want := range map[string]string{
		"incidents:\n  - start: 2026-10-31T01:00:00Z\n": "no id",
		"incidents:\n  - id: X\n":                       "no start",
		"incidents:\n  - id: X\n    start: 2026-10-31T01:00:00Z\n    end: 2026-10-30T01:00:00Z\n":          "ends before",
		"incidents:\n  - id: X\n    start: 2026-10-31T01:00:00Z\n    labels: {service: {a: b}}\n":          "want a value or a list",
		"incidents:\n  - {id: X, start: 2026-10-31T01:00:00Z}\n  - {id: X, start: 2026-10-31T02:00:00Z}\n": "there twice",
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

	// By service: two services hit at once are one incident with both, a
	// third later is another.
	svc := func(name string, ms ...int) prom.Series {
		return prom.Series{Labels: map[string]string{"__name__": "x", "service": name, "env": "prod"}, Samples: min(ms...)}
	}
	got = FromImpact([]prom.Series{
		svc("checkout", 10, 11, 12, 13, 14, 15),
		svc("cart", 13, 14, 15, 16, 17, 18, 19),
		svc("search", 40, 41, 42, 43, 44),
		{Labels: map[string]string{"service": "search"}, Samples: min(41, 42)}, // too short on its own
	}, time.Minute, 5*time.Minute)
	if len(got) != 2 || got[0].ID != "impact-1" || !got[0].End.Equal(t0.Add(20*time.Minute)) || got[1].ID != "impact-2" {
		t.Fatalf("got %+v", got)
	}
	if l := got[0].Labels; !slices.Equal(l["service"], []string{"cart", "checkout"}) || !slices.Equal(l["env"], []string{"prod"}) || l["__name__"] != nil {
		t.Errorf("labels %v: want both services, env, and no __name__", l)
	}
}

func TestMerge(t *testing.T) {
	written := []Incident{{ID: "INC-1", Start: t0.Add(10 * time.Minute)}}
	cands := []Incident{
		{ID: "impact-1", Start: t0.Add(30 * time.Minute), End: t0.Add(40 * time.Minute), Candidate: true}, // inside INC-1's assumed hour
		{ID: "impact-2", Start: t0.Add(3 * time.Hour), End: t0.Add(4 * time.Hour), Candidate: true},
	}
	got := Merge(written, cands, time.Hour, nil)
	if len(got) != 2 || got[1].ID != "impact-2" {
		t.Errorf("%+v", got)
	}
	// Matching on service: a candidate for another service at the same
	// time is another incident.
	written[0].Labels = Labels{"service": {"checkout"}}
	cands[0].Labels = Labels{"service": {"search"}}
	if got := Merge(written, cands, time.Hour, []string{"service"}); len(got) != 3 {
		t.Errorf("%+v: want the search candidate kept", got)
	}
	cands[0].Labels = Labels{"service": {"search", "checkout"}}
	if got := Merge(written, cands, time.Hour, []string{"service"}); len(got) != 2 {
		t.Errorf("%+v: want the candidate that shares checkout dropped", got)
	}
}

// A file that says nothing isn't taken for no incidents: a job that failed
// leaves one, and lapilli serve would take every incident away.
func TestLoadRefusesNothing(t *testing.T) {
	for body, ok := range map[string]bool{
		"":                 false,
		"# a comment\n":    false,
		"{}\n":             false,
		"other: 1\n":       false,
		"incidents: []\n":  true,
		"incidents:\n":     true,
		`{"incidents":[]}`: true,
	} {
		file := filepath.Join(t.TempDir(), "incidents.yaml")
		os.WriteFile(file, []byte(body), 0o600)
		incs, err := Load(file)
		if (err == nil) != ok || len(incs) != 0 {
			t.Errorf("%q: %v, %v", body, incs, err)
		}
	}
}

// A file of incidents is read no further than MaxFile: /dev/zero, or one
// made to take a reader's memory, is refused.
func TestLoadTooBig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "incidents.yaml")
	if err := os.WriteFile(p, append([]byte("incidents: []\n#"), make([]byte, MaxFile)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "more than 64 MiB") {
		t.Errorf("a file of %d bytes: %v", MaxFile+15, err)
	}
}
