package source

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/rules"
)

// slow is more than 1 under the race detector (race_test.go), which the
// tests of the biggest reads skip under. No budget is of time: a slow
// runner fails none.
var slow = 1

// A ruler's unit (GET .../NAMESPACE/GROUP, read when a change to one of its
// rules is proposed or applied, 4 MB at most) is decoded to its nodes and
// then to values (rules.UnmarshalYAML, DecodeYAML): straight into Go
// values, WrapGroup and rules.TreeHash took time that grows with the
// square of a mapping's keys, a hostile ruler's 4 MB unit minutes. Held to
// a few times what decoding the unit to its nodes allocates (allocs), not
// to a multiple of its time, so that a slow runner fails none.
func TestRulerUnitDecodeLinear(t *testing.T) {
	var b strings.Builder
	b.WriteString("name: g\nrules:\n- alert: A\n  expr: up\n  labels: {")
	for i := 0; b.Len() < 256<<10; i++ {
		fmt.Fprintf(&b, "k%d: v, ", i)
	}
	b.WriteString("z: y}\n")
	unit := []byte(b.String())
	// What decoding the unit to its nodes allocates, in linear time.
	var n yaml.Node
	if err := yaml.Unmarshal(unit, &n); err != nil {
		t.Fatal(err)
	}
	nodes := allocs(func() { var n yaml.Node; yaml.Unmarshal(unit, &n) })
	// Read in linear time, or refused: a mapping of more than
	// rules.MaxKeys keys is, as when the rules are read.
	var raw []byte
	var err error
	took := allocs(func() {
		if raw, err = WrapGroup(unit); err == nil {
			_, err = rules.TreeHash(raw)
		}
	})
	t.Logf("a unit of 256 KiB, one mapping of %d keys: %.1f times its nodes' allocations", len(n.Content[0].Content[3].Content[0].Content[5].Content)/2, took/nodes)
	if took > 3*nodes {
		t.Errorf("a unit of 256 KiB, one mapping of %d keys: WrapGroup and TreeHash allocated %.0f (%v), %.1f times its nodes' %.0f: quadratic in its keys",
			len(n.Content[0].Content[3].Content[0].Content[5].Content)/2, took, err, took/nodes, nodes)
	}
	// Under MaxKeys, read whole, in linear time: a unit of 20 rules of
	// 9,000 keys each.
	b.Reset()
	b.WriteString("name: g\nrules:\n")
	for r := range 20 {
		fmt.Fprintf(&b, "- alert: A%d\n  expr: up\n  labels: {", r)
		for i := range 9000 {
			fmt.Fprintf(&b, "k%d: v, ", i)
		}
		b.WriteString("z: y}\n")
	}
	unit = []byte(b.String())
	nodes = allocs(func() { var n yaml.Node; yaml.Unmarshal(unit, &n) })
	took = allocs(func() {
		if raw, err = WrapGroup(unit); err == nil {
			_, err = rules.TreeHash(raw)
		}
	})
	t.Logf("a unit of 20 rules of 9,000 label keys each: %.1f times its nodes' allocations", took/nodes)
	if err != nil || took > 10*nodes {
		t.Errorf("a unit of 20 rules of 9,000 label keys each, %d KiB: %.0f allocations, %.1f times its nodes' %.0f, %v", b.Len()>>10, took, took/nodes, nodes, err)
	}
}

// One rule of a Grafana export, as Grafana 10-12 writes a classic
// query, reduce and threshold alert: some 2.5 KB and 200 of what
// rules.Nodes counts.
const verifyGrafanaRule = `    - uid: r%[1]d
      title: High CPU usage %[1]d
      condition: C
      data:
        - refId: A
          relativeTimeRange:
            from: 600
            to: 0
          datasourceUid: PBFA97CFB590B2093
          model:
            datasource:
              type: prometheus
              uid: PBFA97CFB590B2093
            editorMode: code
            expr: 100 - (avg by(instance) (rate(node_cpu_seconds_total{mode="idle", job="node-exporter"}[5m])) * 100)
            instant: true
            intervalMs: 1000
            legendFormat: __auto
            maxDataPoints: 43200
            range: false
            refId: A
        - refId: B
          relativeTimeRange:
            from: 600
            to: 0
          datasourceUid: __expr__
          model:
            conditions:
              - evaluator:
                  params: []
                  type: gt
                operator:
                  type: and
                query:
                  params:
                    - B
                reducer:
                  params: []
                  type: last
                type: query
            datasource:
              type: __expr__
              uid: __expr__
            expression: A
            intervalMs: 1000
            maxDataPoints: 43200
            reducer: last
            refId: B
            type: reduce
        - refId: C
          relativeTimeRange:
            from: 600
            to: 0
          datasourceUid: __expr__
          model:
            conditions:
              - evaluator:
                  params:
                    - 80
                  type: gt
                operator:
                  type: and
                query:
                  params:
                    - C
                reducer:
                  params: []
                  type: last
                type: query
            datasource:
              type: __expr__
              uid: __expr__
            expression: B
            intervalMs: 1000
            maxDataPoints: 43200
            refId: C
            type: threshold
      noDataState: NoData
      execErrState: Error
      for: 5m
      annotations:
        description: CPU usage on {{ $labels.instance }} is above 80%%, for 5 minutes, which means the node is saturated.
        runbook_url: https://runbooks.example.com/alerts/high-cpu-usage
        summary: High CPU
      labels:
        severity: warning
        team: platform-sre
      isPaused: false
`

// An export of 5,100 such rules, 12.7 MB, is read: a value every 12.5
// bytes, well within rules.Nodes' one every 4.
func TestGrafanaExportOf5100Rules(t *testing.T) {
	const n = 5100
	var b strings.Builder
	b.WriteString("apiVersion: 1\ngroups:\n")
	for g := 0; g*10 < n; g++ {
		fmt.Fprintf(&b, "- orgId: 1\n  name: g%d\n  folder: Infra\n  interval: 1m\n  rules:\n", g)
		for i := g * 10; i < min(n, g*10+10); i++ {
			fmt.Fprintf(&b, verifyGrafanaRule, i)
		}
	}
	if b.Len() > ListLimit {
		t.Fatalf("%d bytes, over ListLimit", b.Len())
	}
	rs, err := rules.FromDocs([]rules.Doc{{Source: "grafana", Data: []byte(b.String())}})
	if err != nil || len(rs) != n {
		t.Errorf("an export of %d rules, %.1f MB: %d rules, %v", n, float64(b.Len())/1e6, len(rs), err)
	}
}

// A ruler's list that doesn't parse whole is split at its namespaces, and
// each group read by groupsRead, in linear time, and held to MaxRules and
// maxGroupNodes as a list that parses whole is: a hostile ruler adds a
// line that doesn't parse to the list it answers every minute.
func TestRulerListSplitLinear(t *testing.T) {
	var b strings.Builder
	// As Mimir writes it: groups indented under their namespace.
	b.WriteString("shop:\n    - name: g\n      rules:\n        - alert: A\n          expr: up\n          labels: {")
	for i := 0; b.Len() < 384<<10; i++ {
		fmt.Fprintf(&b, "k%d: v, ", i)
	}
	b.WriteString("z: y}\n")
	whole := []byte(b.String())
	var n yaml.Node
	if err := yaml.Unmarshal(whole, &n); err != nil {
		t.Fatal(err)
	}
	nodes := allocs(func() { var n yaml.Node; yaml.Unmarshal(whole, &n) })
	split := append(whole, "x: [\n"...) // a namespace that doesn't parse
	var err error
	took := allocs(func() { _, _, err = ConfigDocs("http://ruler", split) })
	t.Logf("a list of 384 KiB split at its namespaces: %.1f times its nodes' allocations", took/nodes)
	if took > 10*nodes {
		t.Errorf("a list of 384 KiB split at its namespaces: %.0f allocations (%v), %.1f times its nodes' %.0f: quadratic in its keys", took, err, took/nodes, nodes)
	}
}

// The split counts rules: 60,000 tiny rules, 2.3 MB, and a line that
// doesn't parse, are refused, past MaxRules, as the list that parses whole
// is; before, 4 MB of them took 880 MB.
func TestRulerListSplitMaxRules(t *testing.T) {
	var b strings.Builder
	b.WriteString("shop:\n")
	for g := 0; g < (MaxRules+10000)/50; g++ {
		fmt.Fprintf(&b, "    - name: g%d\n      rules:\n", g)
		for range 50 {
			b.WriteString("        - alert: a\n          expr: b\n")
		}
	}
	if _, _, err := ConfigDocs("http://ruler", []byte(b.String())); err == nil {
		t.Fatal("the list that parses whole is taken")
	}
	docs, _, err := ConfigDocs("http://ruler", []byte(b.String()+"x: [\n"))
	if err != nil {
		return
	}
	rs, err := rules.FromDocs(docs)
	if err == nil && len(rs) > MaxRules {
		t.Errorf("a list split at its namespaces gave %d rules, more than MaxRules (%d)", len(rs), MaxRules)
	}
}

// An export of 25,000 of those rules, 62 MB, as large as 0.33.0 read, is
// read whole, holding at its peak some 14 times its size live, under 18:
// 21 without the nodes let go of as they are decoded. What is held to is
// the live heap as each collection marks it, collections at GOGC 10. A
// collection counts as live what is allocated while it marks, more the
// less CPU it gets, and GOGC bounds that: at 10, 14 to 15 times with one
// to three CPUs and 24 others busy, at 50 up to 17.5. The heap in use
// isn't held to: at GOGC 100 it was 22 to 29 times, past what is live by
// what the collector hadn't yet freed, and 30 to 34 without the nodes let
// go of.
func TestGrafanaExportOf25000Rules(t *testing.T) {
	if testing.Short() || slow > 1 {
		t.Skip("25,000 rules, 62 MB: a few seconds, 1.5 GB")
	}
	const n = 25000
	var b strings.Builder
	b.WriteString("apiVersion: 1\ngroups:\n")
	for g := 0; g*10 < n; g++ {
		fmt.Fprintf(&b, "- orgId: 1\n  name: g%d\n  folder: Infra\n  interval: 1m\n  rules:\n", g)
		for i := g * 10; i < min(n, g*10+10); i++ {
			fmt.Fprintf(&b, verifyGrafanaRule, i)
		}
	}
	body := b.String()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
	defer ts.Close()
	defer debug.SetGCPercent(debug.SetGCPercent(10))
	runtime.GC()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)
	live := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	metrics.Read(live)
	live0 := live[0].Value.Uint64()
	var inuse, held uint64 // written by the sampler only, read once it is done
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			inuse = max(inuse, m.HeapInuse)
			metrics.Read(live)
			held = max(held, live[0].Value.Uint64())
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
	times := func(grew uint64) float64 { return float64(grew) / float64(len(body)) }
	kept, grew := held-min(held, live0), inuse-min(inuse, m0.HeapInuse)
	t.Logf("%d rules, %.1f MB: %d read, %v; peak live heap +%d MiB (%.1fx), peak heap in use +%d MiB (%.1fx)",
		n, float64(len(body))/1e6, len(rs), err, kept>>20, times(kept), grew>>20, times(grew))
	if err != nil || len(rs) != n {
		t.Errorf("an export of %d rules: %d read, %v", n, len(rs), err)
	}
	if kept > 18*uint64(len(body)) {
		t.Errorf("an export of %.1f MB held %d MiB live, %.1f times it", float64(len(body))/1e6, kept>>20, times(kept))
	}
	runtime.KeepAlive(rs)
}

// allocs is how many allocations f makes, as testing.AllocsPerRun counts
// them: a measure of its work that doesn't hang on the runner's clock.
func allocs(f func()) float64 { return testing.AllocsPerRun(1, f) }
