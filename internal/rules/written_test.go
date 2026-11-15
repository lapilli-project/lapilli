package rules

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// TestPickWrittenCharged: a List picked from, some items left out, is
// written afresh, its aliases in full, and that is what is kept: in the
// ledger, read again. What it is written in is charged first: a 64 KiB
// string aliased by 60 rules, 69 KB, was kept as 3.9 MB, 57 times the file;
// it is refused now, and one within the bound is kept as written.
func TestPickWrittenCharged(t *testing.T) {
	list := func(size, refs int) string {
		var b strings.Builder
		fmt.Fprintf(&b, "apiVersion: v1\nkind: List\nitems:\n- {apiVersion: v1, kind: Secret, metadata: {name: s}}\n- apiVersion: monitoring.coreos.com/v1\n"+
			"  kind: PrometheusRule\n  metadata: {name: p}\n  spec:\n    groups:\n    - name: g\n      rules:\n      - alert: A0\n        expr: x\n        annotations: {d: &s %q}\n",
			strings.Repeat("s", size))
		for i := 1; i < refs; i++ {
			fmt.Fprintf(&b, "      - {alert: A%d, expr: x, annotations: {d: *s}}\n", i)
		}
		return b.String()
	}
	dir := t.TempDir()
	read := func(doc string) ([]Doc, error) {
		p := filepath.Join(dir, "rules.yaml")
		if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		_, docs, _, err := ReadPicked(p, Picking{Trim: true})
		return docs, err
	}
	if _, err := read(list(64<<10, 60)); !refused(err) || !strings.Contains(err.Error(), "written again") {
		t.Errorf("a 64 KiB string by 60 aliases: %v", err)
	}
	doc := list(1<<10, 60)
	docs, err := read(doc)
	if err != nil || len(docs) != 1 || strings.Contains(string(docs[0].Data), "Secret") {
		t.Fatalf("%v", err)
	}
	if rs, err := FromDocs(docs); err != nil || len(rs) != 60 {
		t.Errorf("kept, read again: %d, %v", len(rs), err)
	}
	// What is written is no more than was charged.
	var v any
	if err := yaml.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatal(err)
	}
	if out, _ := yaml.Marshal(v); len(out) > emitted(v, 0) {
		t.Errorf("written in %d bytes, charged %d", len(out), emitted(v, 0))
	}
}

// TestEmittedNoLess: what a document is charged as written afresh is no
// less than what yaml.Marshal writes it in: text that isn't UTF-8, control
// characters, marks it escapes, lines of a block of text, nesting.
func TestEmittedNoLess(t *testing.T) {
	deep := any("x\ny\nz")
	for range 40 {
		deep = []any{map[string]any{"k\n": deep, "q": "\"\\"}}
	}
	for _, v := range []any{
		"plain", "a\x01b\x02\x7f", "\xff\xfe not UTF-8 \xc3", "\u200e\ufeff\u2028 marks", strings.Repeat("line\n", 300), "\"quoted\" \\back\\",
		map[string]any{"a\x00b": []any{1, 2.5, true, nil, "s"}}, deep, map[any]any{1: "one", true: []any{"t"}},
	} {
		out, err := yaml.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if n := emitted(v, 0); len(out) > n {
			t.Errorf("%.60q: written in %d bytes, charged %d", out, len(out), n)
		}
	}
}

// TestPickStreamOnce: a stream not cut where YAML cuts it is read again
// whole, what its pieces counted given back first: Pick read a stream of a
// group's labels on 20 rules, a document end and a directive (some 2.4 MB
// made, within the 4 MiB it may), twice, and refused what Parse reads; and
// what the inputs read together made was counted twice.
func TestPickStreamOnce(t *testing.T) {
	b := groupBomb(1000, 20, false, false) + "...\n%YAML 1.1\n---\ngroups: [{name: h, rules: [{alert: B, expr: up}]}]\n"
	if rs, err := Parse([]byte(b)); err != nil || len(rs) != 21 {
		t.Fatalf("Parse: %d, %v", len(rs), err)
	}
	if kept, _, err := Pick([]byte(b), Selector{}); err != nil {
		t.Errorf("Pick: %v", err)
	} else if rs, err := Parse(kept); err != nil || len(rs) != 21 {
		t.Errorf("what Pick kept: %d, %v", len(rs), err)
	}
	// What the inputs read together made is given back too: the stream is
	// counted once there as well.
	ctx := OneInput(Together(t.Context()))
	if _, _, err := pick(ctx, []byte(b), Selector{}); err != nil {
		t.Fatal(err)
	}
	if made, total := inputOf(ctx).made, totalOf(ctx).made; made == 0 || total != made {
		t.Errorf("the input made %d, the inputs together %d", made, total)
	}
}
