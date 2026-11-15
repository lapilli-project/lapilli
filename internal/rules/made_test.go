package rules

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// selectors is an expression of k selectors, a+a+…+a.
func selectors(k int) string { return strings.TrimSuffix(strings.Repeat("a+", k), "+") }

// googleBomb is a Google Rules of n rules, each an expression of k
// selectors, in a namespace of nsLen bytes, which goes into each.
func googleBomb(nsLen, n, k int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: monitoring.googleapis.com/v1\nkind: Rules\nmetadata:\n  name: r\n  namespace: %q\nspec:\n  groups:\n  - name: g\n    rules:\n",
		strings.Repeat("n", nsLen))
	for i := range n {
		fmt.Fprintf(&b, "    - {alert: A%d, expr: %q}\n", i, selectors(k))
	}
	return b.String()
}

// azureBomb is an Azure rule group of n rules, each an expression of k
// selectors, on a cluster of clusterLen bytes, which goes into each; all
// but the first recording rules, when record.
func azureBomb(clusterLen, n, k int, record bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "type: Microsoft.AlertsManagement/prometheusRuleGroups\nname: g\nproperties:\n  clusterName: %q\n  rules:\n", strings.Repeat("c", clusterLen))
	for i := range n {
		if record && i > 0 {
			fmt.Fprintf(&b, "  - {record: r%d, expression: %q}\n", i, selectors(k))
		} else {
			fmt.Fprintf(&b, "  - {alert: A%d, expression: %q}\n", i, selectors(k))
		}
	}
	return b.String()
}

// manyLabels are n labels written in flow style, l0000: v and on, and one
// with a direction mark in its value, when mark.
func manyLabels(n int, mark bool) string {
	var b strings.Builder
	b.WriteString("{")
	for i := range n {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "l%04d: v", i)
	}
	if mark {
		b.WriteString(", turn: \"x‎y\"")
	}
	b.WriteString("}")
	return b.String()
}

// groupBomb is a group of labels labels on n rules, an alert rule and then
// recording rules when record, alert rules otherwise: no alias, each rule
// its group's labels.
func groupBomb(labels, n int, record, mark bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "groups:\n- name: g\n  labels: %s\n  rules:\n  - {alert: A, expr: x}\n", manyLabels(labels, mark))
	for i := 1; i < n; i++ {
		if record {
			fmt.Fprintf(&b, "  - {record: r%d, expr: x}\n", i)
		} else {
			fmt.Fprintf(&b, "  - {alert: A%d, expr: x}\n", i)
		}
	}
	return b.String()
}

// configMap is a ConfigMap holding rule files, by their keys.
func configMap(files ...string) string {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: rules}\ndata:\n")
	for i, f := range files {
		fmt.Fprintf(&b, "  f%02d.yaml: |\n    %s\n", i, strings.ReplaceAll(strings.TrimSuffix(f, "\n"), "\n", "\n    "))
	}
	return b.String()
}

// allocated is what f allocates, in bytes, and how long it takes, which is
// logged, never bounded.
func allocated(f func()) (uint64, time.Duration) {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	start := time.Now()
	f()
	took := time.Since(start)
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc, took
}

// readWithRecordings is ReadPicked of doc as a file, the recording rules
// read too, as a comparison of two sets of rules reads them.
func readWithRecordings(t *testing.T, doc string) ([]Rule, []Recording, error) {
	p := filepath.Join(t.TempDir(), "rules.yaml")
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	var rec []Recording
	rs, _, _, err := ReadPicked(p, Picking{Recordings: &rec})
	return rs, rec, err
}

// TestMadeBombs: a rules input that would make far more than it is, by a
// group's labels on each of its rules, or a namespace or a cluster put
// into each selector of each expression, is refused, naming what made it,
// having allocated a few MB, not the GBs it would have made. Before:
// Google's namespace of 40 KB on 1,000 rules of 16 selectors (99 KB), 6.5
// GB parsed and hashed; Azure's cluster, as much; a group of 9,999 labels
// on 6,700 recording rules (286 KB), 4.2 GB; on 6,700 alert rules, one
// label with a direction mark, 4.2 GB as each rule's labels were copied
// to be shown.
func TestMadeBombs(t *testing.T) {
	parse := func(doc string) func() error {
		return func() error { _, err := Parse([]byte(doc)); return err }
	}
	recordings := func(doc string) func() error {
		return func() error { _, _, err := readWithRecordings(t, doc); return err }
	}
	for _, c := range []struct {
		what string
		size int
		read func() error
		says string
	}{
		{"Google's namespace of 40 KB into 1,000 rules' 16 selectors", len(googleBomb(40<<10, 1000, 16)), parse(googleBomb(40<<10, 1000, 16)),
			"the namespace \"nnnn"},
		{"Azure's cluster of 40 KB into 1,000 alert rules' 16 selectors", len(azureBomb(40<<10, 1000, 16, false)), parse(azureBomb(40<<10, 1000, 16, false)),
			"the cluster \"cccc"},
		{"Azure's cluster of 40 KB into 1,000 recording rules' 16 selectors", len(azureBomb(40<<10, 1000, 16, true)), recordings(azureBomb(40<<10, 1000, 16, true)),
			"the cluster \"cccc"},
		{"a group's 9,999 labels on 6,700 recording rules", len(groupBomb(9999, 6700, true, false)), recordings(groupBomb(9999, 6700, true, false)),
			`group "g" puts its 9999 labels`},
		{"a group's 9,999 labels, one with a direction mark, on 6,700 alert rules", len(groupBomb(9999, 6700, false, true)), parse(groupBomb(9999, 6700, false, true)),
			`group "g" puts its 10000 labels`},
	} {
		var err error
		bytes, took := allocated(func() { err = c.read() })
		t.Logf("%s: %d KB in, %d MB allocated, %v: %v", c.what, c.size>>10, bytes>>20, took, err)
		var made *MadeError
		if !errors.As(err, &made) || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v, want a MadeError saying %q", c.what, err, c.says)
		}
		// What is made is charged before it is made: a few MB at most,
		// the input's own reading among them.
		if bytes > 64<<20 {
			t.Errorf("%s: %d MB allocated to refuse it", c.what, bytes>>20)
		}
	}
}

// TestMadeInTimeOfItsBytes: what a document makes is charged as it is
// made, a group's labels worked out once for the group, so a refusal comes
// as soon as the input is past what it may make, not after every rule's
// labels have been looked through: 60,000 rules on a group of 9,999 labels
// (1.8 MB) are refused having allocated what reading them takes. Before,
// they were read, each rule's group labels looked through to be shown, in
// 11.7 s and 294 MB, and hashing them was 2.3 GB a thousand rules.
func TestMadeInTimeOfItsBytes(t *testing.T) {
	doc := groupBomb(9999, 60000, false, false)
	var err error
	bytes, took := allocated(func() { _, err = Parse([]byte(doc)) })
	t.Logf("60,000 rules on a group of 9,999 labels, %d KB: %d MB allocated, %v: %v", len(doc)>>10, bytes>>20, took, err)
	var made *MadeError
	if !errors.As(err, &made) {
		t.Fatalf("%v", err)
	}
	// Charged as it goes: refused at the rule that passed the bound, a few
	// rules in, not at the end.
	group := map[string]string{}
	for i := range 9999 {
		group[fmt.Sprintf("l%04d", i)] = "v"
	}
	if each := labelsCost(group); made.Made > made.Limit+each {
		t.Errorf("refused having counted %d, past a limit of %d by more than one rule's %d", made.Made, made.Limit, each)
	}
	if bytes > 128<<20 {
		t.Errorf("%d MB allocated", bytes>>20)
	}
}

// TestShownOncePerGroup: a group's labels, which each of its rules has,
// are looked through to be shown once for the group, and its rules share
// what is shown, as they shared what was read: eight, one with a
// direction mark, on 2,000 rules, aren't copied for each.
func TestShownOncePerGroup(t *testing.T) {
	doc := groupBomb(7, 2000, false, true)
	rs, err := Parse([]byte(doc))
	if err != nil || len(rs) != 2000 {
		t.Fatalf("%d, %v", len(rs), err)
	}
	if rs[0].GroupLabels["turn"] != "x�y" {
		t.Errorf("shown: %q", rs[0].GroupLabels["turn"])
	}
	for i, r := range rs {
		if !sameMap(r.GroupLabels, rs[0].GroupLabels) {
			t.Fatalf("rule %d has a copy of its group's labels of its own", i)
		}
	}
}

// TestLabelsCost: what a copy of labels is charged is no less than what Go
// allocates for it, as maps.Clone makes one, so the bound is on memory
// made, not less.
func TestLabelsCost(t *testing.T) {
	for _, n := range []int{1, 8, 9, 16, 100, 1000, 9999} {
		l := map[string]string{}
		for i := range n {
			l["l"+strconv.Itoa(i)] = "v"
		}
		const copies = 100
		keep := make([]map[string]string, 0, copies)
		bytes, _ := allocated(func() {
			for range copies {
				keep = append(keep, maps.Clone(l))
			}
		})
		each := int(bytes / copies)
		if cost := labelsCost(l); cost < each {
			t.Errorf("%d labels: charged %d, a copy takes %d", n, cost, each)
		}
		runtime.KeepAlive(keep)
	}
}

// TestScopeRoom: an expression scoped grows by its matchers at each
// selector, given up before it grows past the room it has; what it grows
// by is charged to its input.
func TestScopeRoom(t *testing.T) {
	m := matchersOf(map[string]string{"namespace": "shop"})
	expr := "a + b{x=\"1\"} + c"
	out, err := scopeWith(expr, m, -1)
	if want := `a{namespace="shop"} + b{x="1",namespace="shop"} + c{namespace="shop"}`; err != nil || out != want {
		t.Fatalf("%q, %v", out, err)
	}
	grew := len(out) - len(expr)
	if got, err := scopeWith(expr, m, grew); err != nil || got != out {
		t.Errorf("room enough: %q, %v", got, err)
	}
	if _, err := scopeWith(expr, m, grew-1); !errors.Is(err, errNoRoom) {
		t.Errorf("a byte short: %v", err)
	}
	in := &input{size: 1}
	s := scoping{in: in, matchers: m, what: "namespace", value: "shop"}
	if got, err := s.scope(expr); err != nil || got != out || in.made != grew {
		t.Errorf("charged %d for %q, %v", in.made, got, err)
	}
	in.made = madeLimit(1) - grew + 1
	var made *MadeError
	if _, err := s.scope(expr); !errors.As(err, &made) || !strings.Contains(err.Error(), `the namespace "shop", 4 bytes, put into each selector`) {
		t.Errorf("no room: %v", err)
	}
}
