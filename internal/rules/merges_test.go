package rules

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// mergeBomb is a document of small size whose merge keys (<<) make a
// decoder walk keys*nb*nc keys: a mapping of keys keys, merged nb times
// into b, b merged nc times into the labels of a rule.
func mergeBomb(keys, nb, nc int) string {
	var b strings.Builder
	b.WriteString("x: &a {")
	for i := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "k%d: v", i)
	}
	b.WriteString("}\ny: &b {<<: [")
	b.WriteString(strings.TrimSuffix(strings.Repeat("*a, ", nb), ", "))
	b.WriteString("]}\ngroups:\n- name: g\n  rules:\n  - alert: A\n    expr: up\n    labels: {<<: [")
	b.WriteString(strings.TrimSuffix(strings.Repeat("*b, ", nc), ", "))
	b.WriteString("]}\n")
	return b.String()
}

// YAMLValue keeps yaml.v3's bound on what aliases expand to, counting the
// keys of a mapping merged again (<<) as it does: a few KB of merge keys
// over aliases would take keys*nb*nc steps, which yaml.v3 refuses at once
// as "excessive aliasing", and so does YAMLValue.
func TestMergeKeyAliasBomb(t *testing.T) {
	doc := mergeBomb(150, 150, 150)
	var v any
	if err := yaml.Unmarshal([]byte(doc), &v); err == nil || !strings.Contains(err.Error(), "excessive aliasing") {
		t.Fatalf("yaml.v3 itself: %v", err)
	}
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &n); err != nil {
		t.Fatal(err)
	}
	if _, err := YAMLValue(&n); err == nil {
		t.Errorf("YAMLValue took a document yaml.v3 refuses as excessive aliasing (%d bytes)", len(doc))
	}
	big := mergeBomb(300, 300, 300) // 5 KB
	start := time.Now()
	Parse([]byte(big))
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("a rule file of %d bytes read in %v: merge keys over aliases take keys*nb*nc steps", len(big), took)
	}
}

// Prometheus 3's group labels (groups[].labels) are read as a rule's own
// are: what turns text around as U+FFFD. The rule page shows them with
// its own (AlertLabels, views.go).
func TestGroupLabelsShown(t *testing.T) {
	rs, err := Parse([]byte("groups:\n- name: g\n  labels: {team: \"a\\u202eb\"}\n  rules:\n  - alert: A\n    expr: up\n"))
	if err != nil || len(rs) != 1 {
		t.Fatalf("%v %v", rs, err)
	}
	if got := rs[0].AlertLabels()["team"]; got != "a�b" {
		t.Errorf("a group's label is read as %q", got)
	}
}
