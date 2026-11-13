package source

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/rules"
)

// A ruler's list was written out again a namespace at a time
// (encodeGroups), each namespace its own YAML document, each read with its
// own count of what its aliases expand to (yaml.v3's bound lets 99% of a
// document come of aliases), and countRules counted the rules as written,
// not what an alias stands for. A hostile ruler's namespaces each hold one
// group of K rules under an anchor and G groups of `rules: *a`: countRules
// saw K a namespace, Read returned K*(G+1), 99 KiB 230,400 rules in
// 306 MiB. No ruler writes an alias: a list holding one isn't read.
func TestRulerListAliasesRefused(t *testing.T) {
	const namespaces, k, g = 6, 400, 95
	var b strings.Builder
	for ns := range namespaces {
		fmt.Fprintf(&b, "ns%d:\n  - name: g0\n    rules: &a\n", ns)
		for i := range k {
			fmt.Fprintf(&b, "      - alert: a%d\n        expr: b\n", i)
		}
		for j := 1; j <= g; j++ {
			fmt.Fprintf(&b, "  - name: g%d\n    rules: *a\n", j)
		}
	}
	body := b.String()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
	defer ts.Close()
	r, err := NewRuler(ts.URL+"/prometheus/config/v1/rules", "test")
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)
	var peak atomic.Uint64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			if m.HeapInuse > peak.Load() {
				peak.Store(m.HeapInuse)
			}
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	rs, _, err := r.Read(context.Background())
	close(stop)
	<-done
	grew := peak.Load() - min(peak.Load(), m0.HeapInuse)
	t.Logf("list of %d KiB, %d rules as written: %d read, %v; peak heap +%d MiB (%.0fx)",
		len(body)>>10, namespaces*k, len(rs), err, grew>>20, float64(grew)/float64(len(body)))
	if err == nil && len(rs) > MaxRules {
		t.Errorf("a ruler's list of %d KiB, %d rules as written, read as %d rules, past MaxRules (%d): each namespace's aliases expand on their own",
			len(body)>>10, namespaces*k, len(rs), MaxRules)
	}
	if grew > 80*uint64(len(body)) {
		t.Errorf("a list of %d KiB took %d MiB of heap, %.0f times it", len(body)>>10, grew>>20, float64(grew)/float64(len(body)))
	}
	runtime.KeepAlive(rs)
}

// A Grafana's export is read as YAML, which may be several documents
// (---), each read with its own count of what its aliases expand to: a
// hostile Grafana's export of a few hundred KB was held as hundreds of
// thousands of rules before MaxRules refused them. Grafana writes no
// alias: an export holding one isn't read.
func TestGrafanaExportAliasesRefused(t *testing.T) {
	const docs, k, g = 6, 100, 95
	var b strings.Builder
	for d := range docs {
		if d > 0 {
			b.WriteString("---\n")
		}
		b.WriteString("apiVersion: 1\ngroups:\n- orgId: 1\n  name: g0\n  folder: f\n  interval: 1m\n  rules: &a\n")
		for i := range k {
			fmt.Fprintf(&b, "  - uid: u%d\n    title: A%d\n    condition: A\n    data:\n    - refId: A\n      datasourceUid: prom\n      model: {expr: up}\n    for: 1m\n", i, i)
		}
		for j := 1; j <= g; j++ {
			fmt.Fprintf(&b, "- orgId: 1\n  name: g%d\n  folder: f\n  interval: 1m\n  rules: *a\n", j)
		}
	}
	body := b.String()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
	defer ts.Close()
	runtime.GC()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)
	var peak atomic.Uint64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			if m.HeapInuse > peak.Load() {
				peak.Store(m.HeapInuse)
			}
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	rs, _, err := Grafana(context.Background(), ts.URL, "", "test", nil)
	close(stop)
	<-done
	grew := peak.Load() - min(peak.Load(), m0.HeapInuse)
	t.Logf("export of %d KiB, %d rules as written: %d read, %v; peak heap +%d MiB (%.0fx)",
		len(body)>>10, docs*k, len(rs), err, grew>>20, float64(grew)/float64(len(body)))
	if grew > 80*uint64(len(body)) {
		t.Errorf("an export of %d KiB took %d MiB of heap, %.0f times it, before it was refused (%v)", len(body)>>10, grew>>20, float64(grew)/float64(len(body)), err)
	}
	runtime.KeepAlive(rs)
}

// An Amazon namespace holds the rule file someone wrote, which may use
// aliases: they are read, what they expand to counted for every namespace
// together, and the rules as read held to MaxRules. Six namespaces of 400
// rules under an anchor and 30 groups of `rules: *a` each are 74,400
// rules, within yaml.v3's bound on each, and on all of them together:
// past MaxRules, none is read.
func TestReadAmazonAliases(t *testing.T) {
	ns := map[string]string{"labels": "groups:\n  - name: g\n    rules:\n      - alert: A\n        expr: up\n        labels: &l {team: sre}\n      - alert: B\n        expr: up\n        labels: *l\n"}
	modified := map[string]float64{"labels": 1}
	a := &amazon{t: t, ns: ns, modified: modified, described: map[string]int{}}
	ts := httptest.NewServer(a)
	defer ts.Close()
	r, err := NewRuler("https://aps.us-east-1.amazonaws.com/workspaces/ws-1", "test")
	if err != nil {
		t.Fatal(err)
	}
	r.Reads.Sign, r.Transport = signed(), &to{ts: ts}
	if rs, _, err := r.Read(context.Background()); err != nil || len(rs) != 2 || rs[1].Labels["team"] != "sre" {
		t.Fatalf("a namespace whose rules share their labels by an alias: %d rules, %v", len(rs), err)
	}
	a.mu.Lock()
	for n := range 6 {
		var b strings.Builder
		b.WriteString("groups:\n  - name: g0\n    rules: &a\n")
		for i := range 400 {
			fmt.Fprintf(&b, "      - alert: a%d\n        expr: b\n", i)
		}
		for g := 1; g <= 30; g++ {
			fmt.Fprintf(&b, "  - name: g%d\n    rules: *a\n", g)
		}
		name := fmt.Sprintf("n%d", n)
		ns[name], modified[name] = b.String(), 1
	}
	a.mu.Unlock()
	if rs, _, err := r.Read(context.Background()); err == nil || !strings.Contains(err.Error(), "more than 50000 rules") {
		t.Errorf("%d rules as read: %v", len(rs), err)
	}
}

// A list that doesn't parse whole is split at its namespaces, and one that
// doesn't read whole at its groups: each group's rules counted, and held
// to MaxRules with the rest.
func TestRulerListSplitItemsMaxRules(t *testing.T) {
	var b strings.Builder
	b.WriteString("shop:\n    - name: bad\n      rules: {\n")
	for g := 0; g < (MaxRules+10000)/50; g++ {
		fmt.Fprintf(&b, "    - name: g%d\n      rules:\n", g)
		for range 50 {
			b.WriteString("        - alert: a\n          expr: b\n")
		}
	}
	docs, passed, err := ConfigDocs("http://ruler", []byte(b.String()))
	if err == nil {
		rs, _ := rules.FromDocs(docs)
		t.Errorf("a namespace read a group at a time: %d rules, past MaxRules (%d); passed over %v", len(rs), MaxRules, passed)
	}
}

// A list that doesn't parse whole is split at its namespaces, and isn't
// looked through for an alias as a whole list is: a namespace holding one
// is kept as written, its rules counted as written, and Read refuses it as
// it parses it (rules.NoAliases), saying why, as no ruler writes one; the
// rest are read. Read as aliases expand, its 400 rules would be 12,400.
func TestRulerListSplitAliasesRefused(t *testing.T) {
	var b strings.Builder
	b.WriteString("ns:\n    - name: g0\n      rules: &a\n")
	for i := range 400 {
		fmt.Fprintf(&b, "        - alert: a%d\n          expr: b\n", i)
	}
	for g := 1; g <= 30; g++ {
		fmt.Fprintf(&b, "    - name: g%d\n      rules: *a\n", g)
	}
	b.WriteString("shop:\n    - name: s\n      rules:\n        - alert: B\n          expr: up\nx: [\n")
	body := b.String()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
	defer ts.Close()
	r, err := NewRuler(ts.URL+"/prometheus/config/v1/rules", "test")
	if err != nil {
		t.Fatal(err)
	}
	var said []string
	r.Warn = func(m string) { said = append(said, m) }
	rs, _, err := r.Read(context.Background())
	if err != nil || len(rs) != 1 || rs[0].Name != "B" || !slices.ContainsFunc(said, func(m string) bool {
		return strings.Contains(m, "/ns can't be read") && strings.Contains(m, "an alias (*a)")
	}) {
		t.Errorf("%d rules, %v; said %q", len(rs), err, said)
	}
}
