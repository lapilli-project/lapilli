package rules

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// A rule of many label keys is read in time that grows with them, not with
// their square, as yaml.v3's own decoding to Go values does; a mapping of
// more than MaxKeys keys, or nested past MaxDepth, is refused.
func TestYAMLManyKeys(t *testing.T) {
	var b strings.Builder
	b.WriteString("groups:\n- name: g\n  rules:\n")
	for r := range 20 {
		fmt.Fprintf(&b, "  - alert: A%d\n    expr: up\n    labels: {", r)
		for i := range MaxKeys - 1 {
			fmt.Fprintf(&b, "k%d: v, ", i)
		}
		b.WriteString("z: y}\n")
	}
	start := time.Now()
	rs, err := Parse([]byte(b.String()))
	took := time.Since(start)
	if err != nil || len(rs) != 20 || len(rs[0].Labels) != MaxKeys {
		t.Fatalf("%d rules, %v", len(rs), err)
	}
	t.Logf("20 rules of %d label keys, %d KiB: %v", MaxKeys, b.Len()>>10, took)
	if took > 1500*time.Millisecond*time.Duration(slow) {
		t.Errorf("20 rules of %d label keys read in %v: the time grows with the square of a mapping's keys", MaxKeys, took)
	}
	over := "groups:\n- name: g\n  rules:\n  - alert: A\n    expr: up\n    labels: {" + strings.Repeat("x: y, ", 0)
	for i := range MaxKeys + 1 {
		over += fmt.Sprintf("k%d: v, ", i)
	}
	if _, err := Parse([]byte(over + "z: y}\n")); err == nil || !strings.Contains(err.Error(), "more than 10000 keys") {
		t.Errorf("a mapping of %d keys: %v", MaxKeys+2, err)
	}
	if _, err := Parse([]byte("groups: " + strings.Repeat("[", MaxDepth+1) + strings.Repeat("]", MaxDepth+1) + "\n")); err == nil || !strings.Contains(err.Error(), "nested more than") {
		t.Errorf("nested %d deep: %v", MaxDepth+1, err)
	}
	if err := Nodes([]byte(strings.Repeat("0,", MaxNodes+1))); err == nil {
		t.Error("a million and one values counted as fewer")
	}
}

// Reading documents is given up when its context ends, between them.
func TestFromDocsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	doc := Doc{Source: "a", Data: []byte("groups:\n- name: g\n  rules:\n  - {alert: A, expr: up}\n")}
	if _, err := FromDocsContext(ctx, []Doc{doc}); !errors.Is(err, context.Canceled) {
		t.Errorf("an ended context: %v", err)
	}
	if rs, err := FromDocsContext(context.Background(), []Doc{doc, doc}); err != nil || len(rs) != 2 {
		t.Errorf("%d rules, %v", len(rs), err)
	}
}

// YAMLValue decodes as yaml.v3 does into an any: merges, aliases, tags,
// keys of any kind, nulls, a key given twice refused.
func TestYAMLValueAsYAML(t *testing.T) {
	for _, doc := range []string{
		"a: 1\nb: [x, 2.5, true, null, ~, '', \"q\"]\nc: {d: 2006-01-02, e: 0x10, f: .inf}\n",
		"base: &b {x: 1, y: 2}\nm:\n  <<: *b\n  y: 3\nn:\n  <<: [*b, {z: 4, x: 9}]\n",
		"1: one\ntrue: t\nnull: n\n2.5: f\n",
		"a: !!str 1\nb: !!binary aGk=\nc: !!float 1\nd: !custom x\n",
		"? [complex]\n: v\n",
		"a: 1\na: 2\n",
		"a: &x {b: *x}\n",
		"x: |\n  line\n   more\ny: >-\n  folded\n  text\n",
		"\"<<\": {a: 1}\n",
		"<<: {a: 1}\nb: 2\n",
		"m: {<<: {a: 1}, <<: {b: 2}}\n",
	} {
		var want any
		werr := yaml.Unmarshal([]byte(doc), &want)
		var n yaml.Node
		if err := yaml.Unmarshal([]byte(doc), &n); err != nil {
			t.Fatalf("%q: %v", doc, err)
		}
		got, gerr := YAMLValue(&n)
		if (werr == nil) != (gerr == nil) || werr == nil && !sameValue(got, want) {
			t.Errorf("%q:\n%#v %v\nwant %#v %v", doc, got, gerr, want, werr)
		}
	}
}

// FuzzYAMLValue: whatever a document holds, YAMLValue decodes it as yaml.v3
// does into an any, or refuses it as yaml.v3 does, but for what is past
// MaxKeys or MaxDepth, and near the bound on what aliases expand to.
func FuzzYAMLValue(f *testing.F) {
	for _, s := range []string{grafanaFile, "base: &b {x: 1}\nm: {<<: *b, y: [1, {a: b}]}\n", "a: 1\na: 2\n", "- !!binary aGk=\n- 2001-12-14t21:59:43.10-05:00\n",
		// Anchors and merges, in place and by alias, read again after.
		"a: {<<: &m {x: 1}}\nb: *m\n",
		"a:\n  <<: [&m {x: 1}, {y: 2}]\nb: *m\n",
		"a: {<<: &s [{x: 1}]}\nb: *s\n",
		"k: &k {<<: {x: 1}, y: &v [1, {z: *v}]}\nl: {<<: *k}\nm: *k\n",
		"- &a [1, &b {c: 1, <<: &d {e: 2}}]\n- *a\n- *b\n- {<<: [*b, *d]}\n",
		"? &key k\n: v\n*key : w\n",
		"a: &a {<<: &b {x: &c [1]}, y: *c}\nb: {<<: [*a, *b]}\nc: *b\n"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 1<<16 {
			return
		}
		var n yaml.Node
		if yaml.Unmarshal(b, &n) != nil {
			return
		}
		var want any
		werr := yaml.Unmarshal(b, &want)
		got, gerr := YAMLValue(&n)
		var limit errNode
		if gerr != nil && errors.As(gerr, &limit) && (strings.Contains(gerr.Error(), "keys") || strings.Contains(gerr.Error(), "nested")) {
			return
		}
		// Near the bound on what aliases expand to, the two count apart.
		if werr != nil && strings.Contains(werr.Error(), "excessive aliasing") || gerr != nil && strings.Contains(gerr.Error(), "excessive aliasing") {
			return
		}
		if (werr == nil) != (gerr == nil) {
			t.Fatalf("%q: yaml.v3 %v, YAMLValue %v", b, werr, gerr)
		}
		if werr == nil && !sameValue(got, want) {
			t.Fatalf("%q:\n%#v\nwant %#v", b, got, want)
		}
	})
}

// sameValue is reflect.DeepEqual, NaN equal to itself.
func sameValue(a, b any) bool {
	if x, ok := a.(float64); ok {
		y, ok := b.(float64)
		return ok && (x == y || math.IsNaN(x) && math.IsNaN(y))
	}
	switch x := a.(type) {
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !sameValue(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !sameValue(v, w) {
				return false
			}
		}
		return true
	case map[any]any:
		y, ok := b.(map[any]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			if f, ok := k.(float64); ok && math.IsNaN(f) {
				continue
			}
			w, ok := y[k]
			if !ok || !sameValue(v, w) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a, b)
}

// A rule is read with what turns the text around it, in its name, group,
// labels and what it says, as U+FFFD: they go on pages beside other text.
func TestRulesReadShown(t *testing.T) {
	rs, err := Parse([]byte("groups:\n- name: \"shop\\u2066\"\n  rules:\n  - alert: \"Errors\\u202e\"\n    expr: up\n    labels: {team: \"a\\u200fb\"}\n    annotations: {summary: \"disk \\u202efull\", runbook_url: \"https://x\\u2067\"}\n"))
	if err != nil || len(rs) != 1 {
		t.Fatalf("%v %v", rs, err)
	}
	r := rs[0]
	if r.Name != "Errors\uFFFD" || r.Group != "shop\uFFFD" || r.Labels["team"] != "a\uFFFDb" || r.Says.Summary != "disk \uFFFDfull" || r.Says.Runbook != "https://x\uFFFD" {
		t.Errorf("%q %q %q %q %q", r.Name, r.Group, r.Labels, r.Says.Summary, r.Says.Runbook)
	}
	api, err := FromAPI([]byte(`{"status":"success","data":{"groups":[{"name":"g","interval":60,"rules":[{"type":"alerting","name":"Up\u202e","query":"up","labels":{"a":"\u2066"},"annotations":{"summary":"x\u202e"}}]}]}}`))
	if err != nil || len(api) != 1 || api[0].Name != "Up\uFFFD" || api[0].Labels["a"] != "\uFFFD" || api[0].Says.Summary != "x\uFFFD" {
		t.Errorf("from the API: %+v %v", api, err)
	}
	if Shown("Errors") != "Errors" || Shown("a\tb\nc") != "a\tb\nc" || Shown("a\x1bb") != "a\uFFFDb" {
		t.Error("Shown changed what shows")
	}
}

// countdown is a context whose Err says nothing for its first n calls,
// and Canceled after: a context that ends at a given look at it.
type countdown struct {
	context.Context
	n atomic.Int32
}

func (c *countdown) Err() error {
	if c.n.Add(-1) < 0 {
		return context.Canceled
	}
	return nil
}

// A context that ends while documents are read is looked at between the
// YAML documents of one, and in a document's values.
func TestParseLooksAtContext(t *testing.T) {
	doc := Doc{Source: "a", Data: []byte("groups:\n- name: g\n  rules:\n  - {alert: A, expr: up}\n")}
	ctx := &countdown{Context: context.Background()}
	ctx.n.Store(1) // the documents' look; the YAML document's ends it
	if _, err := FromDocsContext(ctx, []Doc{doc}); !errors.Is(err, context.Canceled) {
		t.Errorf("a context ended before its YAML document: %v", err)
	}
	var b strings.Builder
	b.WriteString("groups:\n- name: g\n  rules:\n")
	for i := range 2000 {
		fmt.Fprintf(&b, "  - {alert: A%d, expr: up}\n", i)
	}
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(b.String()), &n); err != nil {
		t.Fatal(err)
	}
	ctx.n.Store(0)
	if _, err := YAMLValueContext(ctx, &n); !errors.Is(err, context.Canceled) {
		t.Errorf("a context ended within a document of 2,000 rules: %v", err)
	}
}

// What a document may parse into is bounded per byte: a value every 3
// bytes, past the first 100,000; real documents, a value every 5 to 15.
func TestNodesPerByte(t *testing.T) {
	if err := Nodes([]byte("pad: [" + strings.Repeat("0,", MinNodes*10) + "0]\n")); err == nil || !strings.Contains(err.Error(), "one every 4 bytes") {
		t.Errorf("a value every two bytes: %v", err)
	}
	if err := Nodes([]byte("pad: [" + strings.Repeat("00,", MinNodes*10) + "0]\n")); err == nil || !strings.Contains(err.Error(), "one every 4 bytes") {
		t.Errorf("a value every three bytes: %v", err)
	}
	// A value every four bytes is read, but not past MaxNodes.
	if err := Nodes([]byte("pad: [" + strings.Repeat("000,", MinNodes*10) + "0]\n")); err != nil {
		t.Errorf("a value every four bytes: %v", err)
	}
	if err := Nodes([]byte("pad: [" + strings.Repeat("000,", MaxNodes) + "0]\n")); err == nil || !strings.Contains(err.Error(), "more than 6000000 values") {
		t.Errorf("%d values, one every four bytes: %v", MaxNodes, err)
	}
	if err := Nodes([]byte(strings.Repeat("- {alert: X12, expr: up == 0, for: 5m, labels: {severity: page}}\n", 100000))); err != nil {
		t.Errorf("a terse rule file: %v", err)
	}
}
