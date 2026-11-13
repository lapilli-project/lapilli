package source

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/rules"
)

// A rule whose labels (or annotations) are one mapping of many keys.
// go.yaml.in/yaml/v3 v3.0.6 checks a mapping's keys for duplicates pair by
// pair (decode.go, d.uniqueKeys), so decoding one of n keys into Go values
// costs n²/2 comparisons: rules.Parse decodes every rule document (a
// ruler's, a Grafana's export, a cloud's alarms, as JSON is YAML) to its
// nodes and then to Go values itself (rules.YAMLValue), in linear time, a
// mapping of more than 10,000 keys refused.

// manyLabels is a ruler's config API answer, or a Grafana export, of one
// rule whose labels are a mapping of n bytes or so.
func manyLabels(n int, grafana bool) string {
	var b strings.Builder
	if grafana {
		b.WriteString("apiVersion: 1\ngroups:\n- orgId: 1\n  name: g\n  folder: f\n  interval: 1m\n  rules:\n  - uid: abc\n    title: A\n    condition: A\n" +
			"    data:\n    - refId: A\n      datasourceUid: prom\n      model: {expr: up}\n    for: 1m\n    labels: {")
	} else {
		b.WriteString("shop:\n- name: g\n  rules:\n  - alert: A\n    expr: up\n    labels: {")
	}
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "k%d: v, ", i)
	}
	b.WriteString("z: y}\n")
	return b.String()
}

func TestRuleAnswerOfManyKeys(t *testing.T) {
	const size = 1 << 20 // 1 MiB: 1/64 of what a ruler's list may be
	for _, c := range []struct {
		name string
		read func(ctx context.Context, url string) error
		body string
	}{
		{"ruler", func(ctx context.Context, url string) error {
			r, err := NewRuler(url+"/prometheus/config/v1/rules", "test")
			if err != nil {
				return err
			}
			rs, _, err := r.Read(ctx)
			if err == nil && len(rs) != 1 {
				err = fmt.Errorf("%d rules", len(rs))
			}
			return err
		}, manyLabels(size, false)},
		{"grafana", func(ctx context.Context, url string) error {
			rs, _, err := Grafana(ctx, url, "", "test", nil)
			if err == nil && len(rs) != 1 {
				err = fmt.Errorf("%d rules", len(rs))
			}
			return err
		}, manyLabels(size, true)},
	} {
		t.Run(c.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/yaml")
				w.Write([]byte(c.body))
			}))
			defer ts.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			start := time.Now()
			err := c.read(ctx, ts.URL)
			took := time.Since(start)
			t.Logf("%s: a %d KiB answer (one rule, %d label keys) read in %s, err=%v",
				c.name, len(c.body)>>10, strings.Count(c.body, ": v,"), took.Round(time.Millisecond), err)
			if took > 3*time.Second {
				t.Errorf("%s: reading a %d KiB answer took %s, past its context's 1s deadline: the time grows with the square of a mapping's keys (x4 for each doubling), so the 64 MB allowed is hours",
					c.name, len(c.body)>>10, took.Round(time.Millisecond))
			}
		})
	}
}

// A ruler's list is read whole, as a yaml.Node tree, written out again,
// parsed into maps and then into rules: an answer of many small rules cost
// some 280 times its size in heap, 4 MiB over a GiB, the 64 MB a list
// could be some 17 GiB. A list is 64 MB at most, of 50,000 rules, a value
// every 4 bytes at most and 6 million in all (rules.Nodes), and a group is
// written out alone, of 100,000 values.
func TestRulerAnswerMemory(t *testing.T) {
	// 4 MiB of rules in one group: refused, counted before they are
	// written out again.
	var b strings.Builder
	b.WriteString("shop:\n- name: g\n  rules:\n")
	for i := 0; b.Len() < 4<<20; i++ {
		fmt.Fprintf(&b, "  - {alert: A%d, expr: up}\n", i)
	}
	rs, grew, err := readPeak(t, b.String())
	if grew > 512<<20 {
		t.Errorf("reading a %d KiB ruler answer took %d MiB of heap: %.0f times its size, so the %d MB a list may be is some %d GiB",
			b.Len()>>10, grew>>20, float64(grew)/float64(b.Len()), ListLimit>>20, (grew*uint64(ListLimit)/uint64(b.Len()))>>30)
	}
	if err == nil || !strings.Contains(err.Error(), "more than 50000 rules") {
		t.Errorf("%d rules: %v", len(rs), err)
	}
	// 40,000 rules in 2,000 groups, read: each group written out again on
	// its own, as the YAML emitter holds every event of what it writes
	// (some 540 MiB written out whole).
	b.Reset()
	b.WriteString("shop:\n")
	for g := range 2000 {
		fmt.Fprintf(&b, "- name: g%d\n  rules:\n", g)
		for r := range 20 {
			fmt.Fprintf(&b, "  - {alert: A%d, expr: up, labels: {a: b}}\n", r)
		}
	}
	rs, grew, err = readPeak(t, b.String())
	if err != nil || len(rs) != 40000 || grew > 384<<20 {
		t.Errorf("40,000 rules in 2,000 groups, %d KiB: %d read, %v, the heap up %d MiB", b.Len()>>10, len(rs), err, grew>>20)
	}
}

// readPeak reads a ruler's list, and gives how far the heap went up.
func readPeak(t *testing.T, body string) ([]rules.Rule, uint64, error) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Write([]byte(body))
	}))
	defer ts.Close()
	r, err := NewRuler(ts.URL+"/prometheus/config/v1/rules", "test")
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)
	var peak atomic.Uint64
	stop, sampled := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(sampled)
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			if m.HeapInuse > peak.Load() {
				peak.Store(m.HeapInuse)
			}
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	start := time.Now()
	rs, _, err := r.Read(context.Background())
	took := time.Since(start)
	close(stop)
	<-sampled
	grew := peak.Load() - min(peak.Load(), m0.HeapInuse)
	t.Logf("a %d KiB answer: %d rules read in %s, err=%v; peak heap in use grew by %d MiB (%.0fx the answer)",
		len(body)>>10, len(rs), took.Round(time.Millisecond), err, grew>>20, float64(grew)/float64(len(body)))
	return rs, grew, err
}

// What a ruler's list may be, and how it is written out again.
func TestRulerListBounds(t *testing.T) {
	var body atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Write([]byte(body.Load().(string)))
	}))
	defer ts.Close()
	read := func(b string) ([]rules.Rule, error) {
		body.Store(b)
		r, err := NewRuler(ts.URL+"/prometheus/config/v1/rules", "test")
		if err != nil {
			t.Fatal(err)
		}
		var said []string
		r.Warn = func(m string) { said = append(said, m) }
		rs, _, err := r.Read(context.Background())
		if err == nil && len(said) > 0 {
			err = errors.New(strings.Join(said, "; "))
		}
		return rs, err
	}
	// More values than a document is read with: refused before it is
	// parsed.
	if _, err := read("shop:\n- name: g\n  rules: []\n  pad: [" + strings.Repeat("0,", rules.MinNodes*10) + "0]\n"); err == nil || !strings.Contains(err.Error(), "more than one every 4 bytes") {
		t.Errorf("a value every two bytes: %v", err)
	}
	// A group of more values than is written out again: its namespace is
	// passed over, the rest read.
	var big strings.Builder
	big.WriteString("big:\n- name: g\n  rules:\n")
	for i := range 20001 {
		fmt.Fprintf(&big, "  - {alert: A%d, expr: up}\n", i)
	}
	big.WriteString("shop:\n- name: g\n  rules:\n  - {alert: B, expr: up}\n")
	if rs, err := read(big.String()); len(rs) != 1 || rs[0].Name != "B" || err == nil || !strings.Contains(err.Error(), `namespace "big": a group of more than 100000 values`) {
		t.Errorf("a group of 100,005 values: %d rules, %v", len(rs), err)
	}
	// A namespace written whole, in flow style, is held to it too: each of
	// its items, a group of one value, was within it, and the emitter held
	// every event of them all.
	if rs, err := read("big: [" + strings.Repeat("0, ", 100005) + "0]\nshop:\n- name: g\n  rules:\n  - {alert: B, expr: up}\n"); len(rs) != 1 || rs[0].Name != "B" || err == nil || !strings.Contains(err.Error(), `namespace "big": its groups, written whole, are of more than 100000 values`) {
		t.Errorf("a namespace of 100,006 values in flow style: %d rules, %v", len(rs), err)
	}
	// So too in a list that doesn't parse whole, split at its namespaces:
	// its groups indented, as Mimir writes them.
	var split strings.Builder
	split.WriteString("big:\n    - name: g\n      rules:\n")
	for i := range 20001 {
		fmt.Fprintf(&split, "        - {alert: A%d, expr: up}\n", i)
	}
	split.WriteString("shop:\n    - name: g\n      rules:\n        - {alert: B, expr: up}\nx: [\n")
	if rs, err := read(split.String()); len(rs) != 1 || rs[0].Name != "B" || err == nil || !strings.Contains(err.Error(), `namespace "big": a group of more than 100000 values`) {
		t.Errorf("a group of 100,005 values, split: %d rules, %v", len(rs), err)
	}
	// Over 64 MB.
	if _, err := read("shop:\n- name: g\n  rules: []\n#" + strings.Repeat("x", ListLimit) + "\n"); err == nil || !strings.Contains(err.Error(), "more than 64 MB") {
		t.Errorf("a list over 64 MB: %v", err)
	}
}

// A namespace's groups are written out a group at a time, as the whole
// would be; in flow style, or with an anchor, whole.
func TestEncodeGroups(t *testing.T) {
	for _, body := range []string{
		"shop:\n- name: g1 # c\n  interval: 1m\n  rules:\n  - alert: A\n    expr: |4\n          up\n    labels: {a: \"b\"}\n  - record: r\n    expr: sum(x)\n- name: g2\n  rules:\n  - alert: 'B'\n    expr: >-\n      foo\n      bar\n    annotations:\n      summary: \"x: y\"\n",
		"shop: [{name: g1, rules: [{alert: A, expr: up}]}, {name: g2, rules: []}]\n",
		"shop:\n- name: g1\n  labels: &l {team: a}\n  rules: []\n- name: g2\n  labels: *l\n  rules: []\n",
		"shop: []\n",
	} {
		var root yaml.Node
		if err := yaml.Unmarshal([]byte(body), &root); err != nil {
			t.Fatal(err)
		}
		groups := root.Content[0].Content[1]
		want, err := encodeYAML(&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Value: "groups"}, groups}})
		if err != nil {
			t.Fatal(err)
		}
		if got, err := encodeGroups(groups); err != nil || string(got) != string(want) {
			t.Errorf("%s:\n%s\nwant\n%s (%v)", body, got, want, err)
		}
	}
}

// A Grafana's export is 64 MB at most, of 50,000 rules.
func TestGrafanaExportBounds(t *testing.T) {
	var body atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body.Load().(string))) }))
	defer ts.Close()
	body.Store("apiVersion: 1\ngroups: []\n#" + strings.Repeat("x", ListLimit))
	if _, _, err := Grafana(context.Background(), ts.URL, "", "test", nil); err == nil || !strings.Contains(err.Error(), "over 64 MB") {
		t.Errorf("an export over 64 MB: %v", err)
	}
	var b strings.Builder
	b.WriteString("apiVersion: 1\ngroups:\n- orgId: 1\n  name: g\n  folder: f\n  interval: 1m\n  rules:\n")
	for i := range MaxRules + 1 {
		fmt.Fprintf(&b, "  - {uid: u%d, title: A%d, condition: A, data: [{refId: A, datasourceUid: p, model: {expr: up}}]}\n", i, i)
	}
	body.Store(b.String())
	if _, _, err := Grafana(context.Background(), ts.URL, "", "test", nil); err == nil || !strings.Contains(err.Error(), "more than 50000 rules") {
		t.Errorf("50,001 rules: %v", err)
	}
	// A context ended is given up between documents.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	body.Store("apiVersion: 1\ngroups: []\n")
	if _, _, err := Grafana(ctx, ts.URL, "", "test", nil); err == nil {
		t.Error("read with its context ended")
	}
}
