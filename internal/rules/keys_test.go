package rules

import (
	"encoding/base64"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// TestKeysAsText: the rules readers read a mapping's keys as text, as
// Prometheus decodes a rule file's: a key that isn't a string made the
// mapping a map[any]any, which no rule is read from, so a ConfigMap's data
// with a key 0 hid its rule files, a rule file whose groups key is
// !!binary hid its rules, and labels with a key true lost every label,
// unsaid. A key that isn't a scalar, a null one, one of another tag, and
// two keys of one text are refused, naming the line; and a ConfigMap's
// rule file so refused is an error naming its key, not passed over.
func TestKeysAsText(t *testing.T) {
	small := strconv.Quote("groups: [{name: g, rules: [{alert: A, expr: up}]}]")
	cm := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata:\n  0: x\n  r.yaml: " + small + "\n"
	p := filepath.Join(t.TempDir(), "cm.yaml")
	if err := os.WriteFile(p, []byte(cm), 0o600); err != nil {
		t.Fatal(err)
	}
	if rs, err := Parse([]byte(cm)); err != nil || len(rs) != 1 {
		t.Errorf("a ConfigMap's data with a key 0: %d, %v", len(rs), err)
	}
	if rs, _, _, err := ReadPicked(p, Picking{Trim: true}); err != nil || len(rs) != 1 {
		t.Errorf("a ConfigMap's data with a key 0, picked: %d, %v", len(rs), err)
	}
	binary := "!!binary " + base64.StdEncoding.EncodeToString([]byte("groups")) +
		":\n- name: g\n  rules:\n  - alert: B\n    expr: up\n    labels: {true: t, 0: zero, 1.5: f, 2001-12-14: d}\n"
	want := map[string]string{"true": "t", "0": "zero", "1.5": "f", "2001-12-14": "d"}
	if rs, err := Parse([]byte(binary)); err != nil || len(rs) != 1 || !maps.Equal(rs[0].Labels, want) {
		t.Errorf("a !!binary groups key, labels of keys not strings: %d, %v", len(rs), err)
	} else if rs[0].Name != "B" {
		t.Errorf("a !!binary groups key: %q", rs[0].Name)
	}

	rule := "groups:\n- name: g\n  rules:\n  - alert: A\n    expr: up\n    labels: "
	for _, c := range []struct{ what, labels, says string }{
		{"a key that isn't a scalar", "{? [a, b] : c}", "line 6: invalid map key"},
		{"a null key", "{null: x}", "line 6: a key that is null"},
		{"a key of another tag", "{!x team: a}", "line 6: a key tagged !x"},
		{"two keys of one text", "{a: x, !!binary " + base64.StdEncoding.EncodeToString([]byte("a")) + ": y}", `line 6: mapping key "a" already defined`},
	} {
		if rs, err := Parse([]byte(rule + c.labels + "\n")); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %d rules, %v", c.what, len(rs), err)
		}
	}
	// In a ConfigMap, its groups key as written: an error naming the key,
	// beside a file that reads.
	tagged := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata:\n  ok.yaml: " + small + "\n  r.yaml: " +
		strconv.Quote("!x groups: [{name: g, rules: [{alert: B, expr: up}]}]") + "\n"
	if rs, err := Parse([]byte(tagged)); err == nil || !strings.Contains(err.Error(), `"r.yaml"`) || !strings.Contains(err.Error(), "tagged !x") {
		t.Errorf("a ConfigMap's rule file whose groups key is tagged !x: %d, %v", len(rs), err)
	}
	// Two keys of one text, an alias of a key beside the key, which
	// Prometheus loads, the last set: an error naming the key, not a rule
	// file that doesn't read, counted broken and left out.
	dup := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata:\n  ok.yaml: " + small + "\n  dup.yaml: " +
		strconv.Quote("groups: [{name: g, rules: [{alert: Hidden, expr: up, labels: {&k team: a, *k: b}}]}]") + "\n"
	if rs, err := Parse([]byte(dup)); err == nil || !strings.Contains(err.Error(), `"dup.yaml"`) || !strings.Contains(err.Error(), "already defined") {
		t.Errorf("a ConfigMap's rule file with an alias of a key beside it: %d, %v", len(rs), err)
	}
}

// TestRulesShaped: a document's nodes are in the shape of rules by its
// keys as their text is read: a groups key written as !!binary, and a
// spec with groups, as a PrometheusRule or a Google Rules has, are; a
// spec without, and a list of targets, aren't.
func TestRulesShaped(t *testing.T) {
	for doc, want := range map[string]bool{
		"!!binary " + base64.StdEncoding.EncodeToString([]byte("groups")) + ": []\n": true,
		"kind: PrometheusRule\nspec: {groups: []}\n":                                 true,
		"kind: Deployment\nspec: {replicas: 1}\n":                                    false,
		"[{targets: [a], labels: {env: p}}]\n":                                       false,
	} {
		var n yaml.Node
		if err := yaml.Unmarshal([]byte(doc), &n); err != nil {
			t.Fatal(err)
		}
		if got := rulesShaped(&n); got != want {
			t.Errorf("%q: %t", doc, got)
		}
	}
}
