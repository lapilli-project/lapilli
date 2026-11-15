package rules

import (
	"errors"
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

// TestAliasText: what aliases bring in again is bounded by its text, not
// only by how many values it is, for the whole input, with what else it
// makes (madeLimit): an 8 MB string aliased by 300 rules (2.4 GB of labels,
// which yaml.v3's count of values lets through) is refused at once, as is
// a 1 MB key aliased 40 times, each naming the alias. Labels and
// annotations shared by an alias, an anchor near the top of a file, and a
// big value aliased twice, read as before.
func TestAliasText(t *testing.T) {
	var made *MadeError
	start := time.Now()
	if _, err := Parse([]byte(bomb(8<<20, 300))); !errors.As(err, &made) || !strings.Contains(err.Error(), "*big the last, bring in their text and values again") {
		t.Errorf("an 8 MB anchor aliased 300 times: %v", err)
	}
	t.Logf("an 8 MB anchor aliased 300 times: refused in %v", time.Since(start))
	var keys strings.Builder
	fmt.Fprintf(&keys, "a:\n  ? &k %s\n  : 1\n", strings.Repeat("k", 1<<20))
	for i := range 40 {
		fmt.Fprintf(&keys, "b%d:\n  ? *k\n  : 1\n", i)
	}
	if _, err := DecodeYAML(t.Context(), []byte(keys.String())); !errors.As(err, &made) || !strings.Contains(err.Error(), "*k the last, bring in") {
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
// read once, kept as they are to know which the merge doesn't set, and
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
	// made: *k as a key (4), and the merged x and 1 (2), each a value
	// brought in again (aliasValue).
	if want := 4 + 2 + 2*aliasValue; in.written != 1+4+1+1+1+1+1+1+1 || in.made != want {
		t.Errorf("written %d, made %d; want 12, %d", in.written, in.made, want)
	}
}

// TestMergeKeysNearTheBound: a mapping's own keys, read again to know which
// a merge (<<) doesn't set, aren't counted again, so near what the input
// may make, none of them is left out of what the merge doesn't set: here
// 62 copies of a 64 KiB key by an alias, and a mapping whose own *s, read
// again past the bound, was taken as unset, the merge's value set in its
// place.
func TestMergeKeysNearTheBound(t *testing.T) {
	var b strings.Builder
	fmt.Fprintf(&b, "k: &k %s\ns: &s s\n", strings.Repeat("k", 64<<10))
	for i := range 62 {
		fmt.Fprintf(&b, "c%d: {*k : 1}\n", i)
	}
	b.WriteString("m: &m {*s : merged}\nx: {*k : 1, *s : own, <<: *m}\n")
	docs, err := DecodeYAML(t.Context(), []byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	x, _ := docs[0].(map[string]any)["x"].(map[string]any)
	if x["s"] != "own" {
		t.Errorf("x's own s: %v", x["s"])
	}
}
