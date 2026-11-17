package incident

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
	got, err := FromImpact(context.Background(), series, time.Minute, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
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
	got, _ = FromImpact(context.Background(), []prom.Series{
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
	// Each series' own window is kept: checkout lasted 6m, cart 7m, the
	// incident from the first's start to the last's end, 10m.
	sp := got[0].Spans
	if len(sp) != 2 || sp[0].Labels["service"] != "checkout" || sp[0].End.Sub(sp[0].Start) != 6*time.Minute ||
		sp[1].Labels["service"] != "cart" || sp[1].End.Sub(sp[1].Start) != 7*time.Minute || sp[0].Labels["__name__"] != "" {
		t.Errorf("spans %+v", sp)
	}
	if len(got[1].Spans) != 1 {
		t.Errorf("one series, one span: %+v", got[1].Spans)
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

// A named signal's windows carry its name and its match, and ids of their
// own; whether an incident written down is the same one is by the
// window's match, when it has one.
func TestFromSignal(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	var s prom.Series
	s.Labels = map[string]string{"service": "checkout", "instance": "https://checkout.example"}
	for i := range 10 {
		s.Samples = append(s.Samples, prom.Sample{T: t0.Add(time.Duration(i) * time.Minute), V: 1})
	}
	ws, _ := FromSignal(context.Background(), []prom.Series{s}, time.Minute, 2*time.Minute, "probe", []string{"service"})
	if len(ws) != 1 || ws[0].ID != "impact-probe-1" || ws[0].Signal != "probe" || !slices.Equal(ws[0].Match, []string{"service"}) ||
		ws[0].Title != "user impact (probe)" || !ws[0].Candidate {
		t.Fatalf("%+v", ws)
	}
	written := []Incident{{ID: "INC-1", Start: t0, End: t0.Add(time.Hour), Labels: Labels{"service": {"checkout"}, "instance": {"node-1"}}}}
	// By --match service,instance, a probe's URL isn't the node's: two.
	plain := ws[0]
	plain.Match = nil
	if got := Merge(written, []Incident{plain}, time.Hour, []string{"service", "instance"}); len(got) != 2 {
		t.Errorf("on --match: %d incidents", len(got))
	}
	// By its own, service alone: the incident written down.
	if got := Merge(written, ws, time.Hour, []string{"service", "instance"}); len(got) != 1 || got[0].ID != "INC-1" {
		t.Errorf("on its own match: %+v", got)
	}
}

// An expression that isn't summed up, over a metric whose labels a
// workload sets (a path, a client's id), returns as many series as one
// range is read with (100,000), each with values of its own, all in the
// same minute: one window. A label of more values than MaxWindowValues is
// left out, as a label not all of them have, each value looked for among
// those kept until then: a window of all 800,000 took minutes to merge,
// each looked for among all before it. A label they share stays.
func TestFromSignalAtTheSeriesCap(t *testing.T) {
	const n, labels = 100000, 8
	series := make([]prom.Series, n)
	for i := range series {
		l := map[string]string{"job": "api"}
		for k := range labels {
			l[fmt.Sprintf("l%d", k)] = fmt.Sprintf("value-%06d", n-1-i)
		}
		series[i] = prom.Series{Labels: l, Samples: []prom.Sample{{T: t0, V: 1}}}
	}
	ws, err := FromSignal(context.Background(), series, time.Minute, time.Minute, "errors", nil)
	if err != nil || len(ws) != 1 || len(ws[0].Spans) != n {
		t.Fatalf("%d windows, %v", len(ws), err)
	}
	if want := (Labels{"job": {"api"}}); !reflect.DeepEqual(ws[0].Labels, want) {
		t.Errorf("labels %v, want %v", ws[0].Labels, want)
	}
	// A firing of any of the series agrees with the window, the last one
	// merged too: none is left out of it by a value it didn't keep.
	if !ws[0].Labels.Agree(map[string]string{"job": "api", "l0": "value-000000"}, []string{"job", "l0"}) {
		t.Error("a firing of the last series doesn't agree with the window")
	}
}

// KeepValue keeps each value once, in order, MaxWindowValues of them at
// most: one more says the label is to be left out.
func TestKeepValue(t *testing.T) {
	var vs []string
	ok := true
	for i := MaxWindowValues; i > 0; i-- {
		if vs, ok = KeepValue(vs, fmt.Sprintf("v%04d", i)); !ok {
			t.Fatalf("v%04d: refused", i)
		}
		if vs, ok = KeepValue(vs, fmt.Sprintf("v%04d", i)); !ok || !slices.IsSorted(vs) { // the same again
			t.Fatalf("after v%04d: %d values, sorted %v", i, len(vs), slices.IsSorted(vs))
		}
	}
	if len(vs) != MaxWindowValues || vs[0] != "v0001" {
		t.Errorf("%d values, from %s", len(vs), vs[0])
	}
	if _, ok := KeepValue(vs, "v0001"); !ok {
		t.Error("a value kept already is refused")
	}
	if _, ok := KeepValue(vs, "v9999"); ok {
		t.Errorf("a value past %d is kept", MaxWindowValues)
	}
	// Within the bound, every value, in order, as before.
	got, _ := KeepValue(nil, "search")
	got, _ = KeepValue(got, "cart")
	if got, _ = KeepValue(got, "search"); !slices.Equal(got, []string{"cart", "search"}) {
		t.Errorf("%v", got)
	}
}

// Windows are found within ctx: one that is done stops it.
func TestFromImpactStopsWhenDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	series := []prom.Series{{Samples: []prom.Sample{{T: t0, V: 1}}}}
	if ws, err := FromImpact(ctx, series, time.Minute, time.Minute); !errors.Is(err, context.Canceled) || ws != nil {
		t.Errorf("%d windows, %v", len(ws), err)
	}
}
