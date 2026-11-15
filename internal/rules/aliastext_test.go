package rules

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
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

// groupBomb is a file of n rules in a group whose label's value is size
// bytes: no alias, each rule its group's labels.
func groupBomb(size, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "groups:\n  - name: big\n    labels: {team: \"%s\"}\n    rules:\n", strings.Repeat("x", size))
	for i := range n {
		fmt.Fprintf(&b, "      - {alert: R%d, expr: up == %d}\n", i, i)
	}
	return b.String()
}

// realistic is a large rule set as a real one can be: n rules in groups of
// 50, each group's labels some 300 bytes, and annotations of some 800
// shared by an alias, with their own labels and expressions.
func realistic(n int) string {
	var b strings.Builder
	b.WriteString("groups:\n")
	desc := strings.Repeat("The error budget burns faster than it should; see the runbook. ", 12)
	for i := range n {
		if i%50 == 0 {
			fmt.Fprintf(&b, "  - name: group-%d\n    labels: {team: payments-platform-%d, tier: customer-facing-critical-path, environment: production-primary-region, "+
				"owner: platform-reliability-on-call-rotation, cost_center: engineering-operations-%d, service: settlement-and-reconciliation-%d}\n    rules:\n", i/50, i/50, i/50, i/50)
		}
		if i == 0 {
			fmt.Fprintf(&b, "      - alert: R0\n        expr: sum(rate(errors_total{job=\"api-0\"}[5m])) / sum(rate(requests_total{job=\"api-0\"}[5m])) > 0.05\n        for: 5m\n"+
				"        labels: {severity: page}\n        annotations: &said {summary: \"Errors are high\", description: %q, runbook_url: \"https://runbooks.example.com/errors\"}\n", desc)
			continue
		}
		fmt.Fprintf(&b, "      - alert: R%d\n        expr: sum(rate(errors_total{job=\"api-%d\"}[5m])) / sum(rate(requests_total{job=\"api-%d\"}[5m])) > 0.05\n        for: 5m\n"+
			"        labels: {severity: page}\n        annotations: *said\n", i, i, i)
	}
	return b.String()
}

// TestDocumentBounds: what a document makes beyond its own text is
// bounded, however it makes it: an 8 MB string aliased by 300 rules (2.4
// GB of labels, which yaml.v3's count of values lets through), a 1 MB key
// aliased 40 times, a group label of 8 MB on 300 rules or 3,000, and one
// within a label's bound on so many rules that their labels pass the
// bound: each refused at once, as a file past MaxFile is, naming what
// made it. A large rule set as real ones are, its labels and annotations
// shared, reads as before.
func TestDocumentBounds(t *testing.T) {
	for _, c := range []struct {
		what, doc, says string
	}{
		{"an 8 MB anchor aliased 300 times", bomb(8<<20, 300), "*big the last, bring in"},
		{"a group label of 8 MB, 300 rules", groupBomb(8<<20, 300), `group "big": label "team" is 4 bytes, and its value 8388608: a label's name and value are 16 KiB at most`},
		{"a group label of 8 MB, 3,000 rules", groupBomb(8<<20+512<<10, 3000), "a label's name and value are 16 KiB at most"},
		{"a group label of 16 KB, 3,000 rules", groupBomb(16<<10, 3000), `group "big" puts its labels, 16388 bytes, on each of its 3000 rules`},
	} {
		start := time.Now()
		_, err := Parse([]byte(c.doc))
		if err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v, want %q", c.what, err, c.says)
		}
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("%s: refused in %v", c.what, took)
		}
	}
	var keys strings.Builder
	fmt.Fprintf(&keys, "a:\n  ? &k %s\n  : 1\n", strings.Repeat("k", 1<<20))
	for i := range 40 {
		fmt.Fprintf(&keys, "b%d:\n  ? *k\n  : 1\n", i)
	}
	if _, err := DecodeYAML(t.Context(), []byte(keys.String())); err == nil || !strings.Contains(err.Error(), "*k the last, bring in") {
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
	// 2,000 rules sharing some 600 bytes of annotations by an alias, an
	// anchor near the file's top: some 1.2 MB brought in again by a file
	// of 260 KB, judged against the whole file, not what was read before.
	var ann strings.Builder
	ann.WriteString("groups:\n  - name: g\n    rules:\n")
	fmt.Fprintf(&ann, "      - {alert: R0, expr: up == 0, annotations: &said {summary: high, description: %q}}\n", strings.Repeat("d", 600))
	for i := 1; i < 2000; i++ {
		fmt.Fprintf(&ann, "      - {alert: Rule%04d, expr: %q, for: 5m, annotations: *said}\n", i, fmt.Sprintf(`sum(rate(errors_total{job="api-%04d",env="prod"}[5m])) > %d`, i, i))
	}
	if rs, err := Parse([]byte(ann.String())); err != nil || len(rs) != 2000 {
		t.Errorf("2,000 rules sharing 600 bytes of annotations (%d bytes): %d, %v", ann.Len(), len(rs), err)
	}
	// A 1.5 MB description aliased twice: a rule says 4 KB of it.
	var big strings.Builder
	fmt.Fprintf(&big, "groups:\n  - name: g\n    rules:\n      - {alert: R0, expr: up, annotations: {description: &d %q}}\n", strings.Repeat("d", 3<<19))
	big.WriteString("      - {alert: R1, expr: up, annotations: {description: *d}}\n      - {alert: R2, expr: up, annotations: {description: *d}}\n")
	if rs, err := Parse([]byte(big.String())); err != nil || len(rs) != 3 || len(rs[2].Says.Description) != maxSays {
		t.Errorf("a 1.5 MB anchor aliased twice: %d, %v", len(rs), err)
	}
	// 5,000 rules as a real set is, in a second.
	start := time.Now()
	doc := realistic(5000)
	rs, err := Parse([]byte(doc))
	if err != nil || len(rs) != 5000 || rs[4999].GroupLabels["team"] != "payments-platform-99" || rs[4999].Says.Description == "" {
		t.Fatalf("5,000 rules as a real set is (%d bytes): %d, %v", len(doc), len(rs), err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("5,000 rules as a real set is: read in %v", took)
	}
}

// TestAliasTextWholeDocument: what aliases bring in again is judged
// against the whole document's size, not against what was read before
// it: an anchor near the top, aliased by the rules after it, reads as well
// as one at the end.
func TestAliasTextWholeDocument(t *testing.T) {
	var b strings.Builder
	b.WriteString("groups:\n  - name: g\n    rules:\n")
	fmt.Fprintf(&b, "      - {alert: R0, expr: up, annotations: {description: &d %q}}\n", strings.Repeat("d", 64<<10))
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&b, "      - {alert: R%d, expr: up, annotations: {description: *d}}\n", i)
	}
	for i := range 200 {
		fmt.Fprintf(&b, "      - {alert: S%d, expr: up, annotations: {description: %q}}\n", i, strings.Repeat("s", 5<<10))
	}
	if rs, err := Parse([]byte(b.String())); err != nil || len(rs) != 301 {
		t.Errorf("a 64 KB anchor aliased 100 times, at the top of %d bytes: %d, %v", b.Len(), len(rs), err)
	}
}

// TestMergeKeysCountedOnce: the keys of a mapping with a merge (<<) are
// read twice, the second time to know which the merge doesn't set, and
// their text is counted once.
func TestMergeKeysCountedOnce(t *testing.T) {
	ctx := OneInput(t.Context())
	var n yaml.Node
	if err := yaml.Unmarshal([]byte("k: &k kkkk\nm: &m {x: 1}\nb: {<<: *m, *k : 2, y: 3}\n"), &n); err != nil {
		t.Fatal(err)
	}
	if _, err := YAMLValueContext(ctx, &n); err != nil {
		t.Fatal(err)
	}
	in := ctx.Value(inputKey{}).(*input)
	// Written: k, kkkk, m, x, 1, b, 2, y, 3, and <<'s key isn't text;
	// copied: *k as a key (4), and the merged x and 1 (2).
	if in.written != 1+4+1+1+1+1+1+1+1 || in.copied != 4+2 {
		t.Errorf("written %d, copied %d; want 12, 6", in.written, in.copied)
	}
}
