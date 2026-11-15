package rules

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// bomb is a file of rules whose first defines a string of size bytes under
// an anchor, and whose others, n in all, alias it in their labels: a few MB
// written, n times that read.
func bomb(size, n int) string {
	var b strings.Builder
	b.WriteString("groups:\n  - name: g\n    rules:\n")
	fmt.Fprintf(&b, "      - {alert: R0, expr: up == 0, labels: {team: &big \"%s\"}}\n", strings.Repeat("x", size))
	for i := 1; i < n; i++ {
		fmt.Fprintf(&b, "      - {alert: R%d, expr: up == %d, labels: {team: *big}}\n", i, i)
	}
	return b.String()
}

// TestAliasText: what aliases bring in again is bounded by its text, not
// only by how many values it is: an 8 MB string aliased by 300 rules (2.4
// GB of labels, which yaml.v3's count of values lets through) is refused,
// at once, as a file past MaxFile is; so is a key aliased so. A file whose
// rules share their labels by an alias, or a big value aliased a few
// times, reads as before.
func TestAliasText(t *testing.T) {
	start := time.Now()
	if _, err := Parse([]byte(bomb(8<<20, 300))); err == nil || !strings.Contains(err.Error(), "aliases expand to") {
		t.Errorf("an 8 MB anchor aliased 300 times: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("refusing it took %v", took)
	}
	var keys strings.Builder
	fmt.Fprintf(&keys, "a:\n  ? &k %s\n  : 1\n", strings.Repeat("k", 1<<20))
	for i := range 40 {
		fmt.Fprintf(&keys, "b%d:\n  ? *k\n  : 1\n", i)
	}
	if _, err := DecodeYAML(t.Context(), []byte(keys.String())); err == nil || !strings.Contains(err.Error(), "aliases expand to") {
		t.Errorf("a 1 MB key aliased 40 times: %v", err)
	}

	// Labels shared by an alias, in 2,000 rules.
	var shared strings.Builder
	shared.WriteString("groups:\n  - name: g\n    rules:\n      - {alert: R0, expr: up == 0, labels: &common {team: payments, env: production, tier: critical-path}}\n")
	for i := 1; i < 2000; i++ {
		fmt.Fprintf(&shared, "      - {alert: R%d, expr: up == %d, labels: *common}\n", i, i)
	}
	if rs, err := Parse([]byte(shared.String())); err != nil || len(rs) != 2000 || rs[1999].Labels["team"] != "payments" {
		t.Errorf("2,000 rules sharing their labels: %d, %v", len(rs), err)
	}
	// A 1.5 MB value aliased twice: three times what is written, at most.
	if rs, err := Parse([]byte(bomb(3<<19, 3))); err != nil || len(rs) != 3 || len(rs[2].Labels["team"]) != 3<<19 {
		t.Errorf("a 1.5 MB anchor aliased twice: %d, %v", len(rs), err)
	}
}
