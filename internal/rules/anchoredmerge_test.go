package rules

import (
	"fmt"
	"testing"

	"go.yaml.in/yaml/v3"
)

// letGo lets go of a mapping's values once decoded unless an anchor is
// over them (anchored) or an alias reads them (inAlias). A merge's own
// mapping, written in place (<<: &m {...}, or one of <<: [&m {...}, ...]),
// is filled by into, not value, and its anchor was counted by neither: its
// values were let go of, and an alias to it after (*m) read a nil node, a
// panic out of Parse on YAML yaml.v3 reads. So too a merge's list written
// in place under an anchor (<<: &s [{...}]).
func TestMergeAnchoredInPlace(t *testing.T) {
	for _, doc := range []string{
		"a: {<<: &m {x: 1}}\nb: *m\n",
		"a:\n  <<: [&m {x: 1}, {y: 2}]\nb: *m\n",
		"a: {<<: &s [{x: 1}]}\nb: *s\n",
		"groups:\n- name: g\n  rules:\n  - alert: A\n    expr: up\n    labels:\n      <<: &common {team: sre}\n  - alert: B\n    expr: up\n    labels: *common\n",
	} {
		var want any
		if err := yaml.Unmarshal([]byte(doc), &want); err != nil {
			t.Fatalf("%q: yaml.v3: %v", doc, err)
		}
		var n yaml.Node
		if err := yaml.Unmarshal([]byte(doc), &n); err != nil {
			t.Fatal(err)
		}
		got, err := func() (v any, err error) {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("panic: %v", p)
				}
			}()
			return YAMLValue(&n)
		}()
		if err != nil || !sameValue(got, want) {
			t.Errorf("%q: YAMLValue %#v, %v; yaml.v3 %#v", doc, got, err, want)
		}
	}
	// A rule file so, as Parse reads one from a file, a ruler or a
	// Grafana: a panic out of Parse, which lapilli serve's work loop
	// doesn't recover.
	file := "groups:\n- name: g\n  rules:\n  - alert: A\n    expr: up\n    labels:\n      <<: &common {team: sre}\n  - alert: B\n    expr: up\n    labels: *common\n"
	rs, err := func() (rs []Rule, err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("panic: %v", p)
			}
		}()
		return Parse([]byte(file))
	}()
	if err != nil || len(rs) != 2 {
		t.Errorf("a rule file whose labels merge an anchored mapping, used again: %d rules, %v", len(rs), err)
	}
}
