package rules

import (
	"fmt"
	"strings"
	"testing"
)

// TestAliasValuesCharged: what an alias brings in again is charged for
// the values it makes as well as their text: an anchored mapping of 4,000
// short keys aliased by 40 rules, 41 KB, made 72 MB of maps; it is refused
// at what an input may make now.
func TestAliasValuesCharged(t *testing.T) {
	var b strings.Builder
	b.WriteString("groups:\n- name: g\n  rules:\n  - alert: A0\n    expr: x\n    annotations: &m {")
	for i := range 4000 {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "k%d: v", i)
	}
	b.WriteString("}\n")
	for i := 1; i < 40; i++ {
		fmt.Fprintf(&b, "  - {alert: A%d, expr: x, annotations: *m}\n", i)
	}
	var err error
	bytes, took := allocated(func() { _, err = Parse([]byte(b.String())) })
	t.Logf("4,000 keys by 40 aliases: %d MB allocated, %v: %.120v", bytes>>20, took, err)
	if !refused(err) || bytes > 32<<20 {
		t.Errorf("%d MB allocated: %v", bytes>>20, err)
	}
}

// TestUnmarshalYAMLWholeSize: UnmarshalYAML judges what aliases bring in
// again against the whole document's size, as DecodeYAML does, not the
// text read before them: an anchor at the top of a large document,
// aliased after it, reads.
func TestUnmarshalYAMLWholeSize(t *testing.T) {
	var b strings.Builder
	fmt.Fprintf(&b, "a: &x %s\n", strings.Repeat("x", 64<<10))
	for i := range 80 {
		fmt.Fprintf(&b, "r%d: *x\n", i)
	}
	for i := range 300 {
		fmt.Fprintf(&b, "w%d: %s\n", i, strings.Repeat("w", 1<<10))
	}
	if _, err := UnmarshalYAML([]byte(b.String())); err != nil {
		t.Errorf("%d bytes: %v", b.Len(), err)
	}
}

// TestNestedMergeOwnKeys: a mapping's own keys win over what merges bring,
// a merge within a merge too.
func TestNestedMergeOwnKeys(t *testing.T) {
	v, err := UnmarshalYAML([]byte("base: &b {x: base, y: base}\nmid: &m {<<: *b, y: mid, z: mid}\nme: {<<: *m, z: me}\n"))
	if err != nil {
		t.Fatal(err)
	}
	me := v.(map[string]any)["me"].(map[string]any)
	if me["x"] != "base" || me["y"] != "mid" || me["z"] != "me" {
		t.Errorf("%v", me)
	}
}
