package rules

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
}

// deepBinary is a List of a Secret and a PrometheusRule whose metadata
// holds, under an anchor, depth mappings around raw bytes that aren't
// UTF-8 (written as !!binary), aliased n times: written afresh, yaml.v3
// writes the base64 in lines, each indented four spaces a level.
func deepBinary(raw, depth, n int) string {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: List\nitems:\n- {apiVersion: v1, kind: Secret, metadata: {name: s}}\n- apiVersion: monitoring.coreos.com/v1\n" +
		"  kind: PrometheusRule\n  metadata:\n    name: p\n    x: &x ")
	b.WriteString(strings.Repeat("{a: ", depth))
	b.WriteString("!!binary " + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\xff", raw))))
	b.WriteString(strings.Repeat("}", depth))
	b.WriteString("\n    y: [" + strings.TrimSuffix(strings.Repeat("*x, ", n), ", ") + "]\n")
	b.WriteString("  spec:\n    groups:\n    - name: g\n      rules:\n      - {alert: A, expr: up}\n")
	return b.String()
}

// TestWrittenBounded: what a document written afresh is written in is
// what is charged, written into no more than the input may make: it was
// estimated, and the estimate fell short (four spaces a level, base64 in
// lines), so a List of 820 KB, 600 KiB of bytes that aren't UTF-8 under
// 190 mappings aliased ten times, was kept as 109 MB, 133 times the file,
// having allocated 861 MB, with or without Trim. Refused now, having
// written no more than the room; and what is written is yaml.Marshal's.
func TestWrittenBounded(t *testing.T) {
	doc := deepBinary(600<<10, 190, 10)
	p := filepath.Join(t.TempDir(), "list.yaml")
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, trim := range []bool{false, true} {
		var docs []Doc
		var err error
		bytes, took := allocated(func() { _, docs, _, err = ReadPicked(p, Picking{Trim: trim}) })
		kept := 0
		for _, d := range docs {
			kept += len(d.Data)
		}
		t.Logf("%d KB, trim %t: kept %d KB, %d MB allocated, %v: %.120v", len(doc)>>10, trim, kept>>10, bytes>>20, took, err)
		if !refused(err) || bytes > 192<<20 {
			t.Errorf("trim %t: %d MB allocated, kept %d: %v", trim, bytes>>20, kept, err)
		}
	}
	deep := any("x\ny\nz")
	for range 40 {
		deep = []any{map[string]any{"k\n": deep, "q": "\"\\"}}
	}
	for _, v := range []any{"plain", "a\x01b\x7f", "\xff\xfe not UTF-8", strings.Repeat("line\n", 300), deep, map[any]any{1: "one", true: []any{"t"}}} {
		ctx := OneInput(t.Context())
		in := inputOf(ctx)
		in.size = 1 << 20
		out, err := written(ctx, v)
		want, _ := yaml.Marshal(v)
		if err != nil || string(out) != string(want) || in.made != len(want) {
			t.Errorf("%.40q: written %d bytes, charged %d, yaml.Marshal %d: %v", want, len(out), in.made, len(want), err)
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

// TestPickAliasedRefused: a ConfigMap whose data a YAML alias brings in is
// refused where a stream is picked, as Parse refuses it, with Trim and
// with a selector leaving an item out: counted broken and kept, it was
// written afresh with each alias in full, and read so, the aliases gone, a
// rule file read again for each key.
func TestPickAliasedRefused(t *testing.T) {
	small := strconv.Quote("groups: [{name: g, rules: [{alert: A, expr: up}]}]")
	doc := "apiVersion: v1\nkind: List\nitems:\n- {apiVersion: v1, kind: Secret, metadata: {name: s}}\n" +
		"- {apiVersion: v1, kind: ConfigMap, metadata: {name: c, labels: {team: a}}, data: {a.yaml: &f " + small + ", b.yaml: *f, c.yaml: *f}}\n" +
		"- {apiVersion: v1, kind: ConfigMap, metadata: {name: d, labels: {team: b}}, data: {d.yaml: " + small + "}}\n"
	p := filepath.Join(t.TempDir(), "list.yaml")
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	sel, err := ParseSelector("team=a")
	if err != nil {
		t.Fatal(err)
	}
	for what, pk := range map[string]Picking{"trimmed": {Trim: true}, "selected": {Select: sel}} {
		if rs, _, _, err := ReadPicked(p, pk); !errors.Is(err, errAliased) {
			t.Errorf("%s: %d rules, %v", what, len(rs), err)
		}
	}
}

// TestPickCopyChargedAsTheFile: what is kept of a file, written afresh, is
// read as the file, with Trim and with a selector leaving an item out:
// what it may make is judged by the file's size, and what the copy holds
// beyond the file, its aliases written out, is charged as made. Read as a
// file of its own, judged by the copy's size, 40 times the file's, it
// made recording rules' labels (made only when asked for, as a comparison
// asks) past what the file may make, and was read.
func TestPickCopyChargedAsTheFile(t *testing.T) {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: List\nitems:\n- {apiVersion: v1, kind: Secret, metadata: {name: s}}\n" +
		"- apiVersion: monitoring.coreos.com/v1\n  kind: PrometheusRule\n  metadata:\n    name: p\n    labels: {team: a}\n    annotations:\n")
	fmt.Fprintf(&b, "      a00: &s %q\n", strings.Repeat("s", 30<<10))
	for i := 1; i < 50; i++ {
		fmt.Fprintf(&b, "      a%02d: *s\n", i)
	}
	b.WriteString("  spec:\n    groups:\n    - name: g\n      labels: " + manyLabels(100, false) + "\n      rules:\n")
	for i := range 300 {
		fmt.Fprintf(&b, "      - {record: r%d, expr: x}\n", i)
	}
	b.WriteString("    - name: h\n      rules:\n      - {alert: A, expr: up}\n" +
		"- {apiVersion: monitoring.coreos.com/v1, kind: PrometheusRule, metadata: {name: q, labels: {team: b}}, spec: {groups: [{name: k, rules: [{alert: B, expr: up}]}]}}\n")
	p := filepath.Join(t.TempDir(), "list.yaml")
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	sel, err := ParseSelector("team=a")
	if err != nil {
		t.Fatal(err)
	}
	for what, pk := range map[string]Picking{"trimmed": {Trim: true}, "selected": {Select: sel}} {
		var rec []Recording
		pk.Recordings = &rec
		if _, _, _, err := ReadPicked(p, pk); !refused(err) || !strings.Contains(err.Error(), "recording rules") {
			t.Errorf("%s, %d KB: %v", what, b.Len()>>10, err)
		}
	}
	if rs, _, _, err := ReadPicked(p, Picking{Trim: true}); err != nil || len(rs) != 2 {
		t.Errorf("its alert rules alone: %d, %v", len(rs), err)
	}
}

// TestPickCopyHeldToMaxFile: what is kept of a file, written afresh, is
// held to MaxFile, as the file it is read as: a file of 1.4 MB whose
// aliases, written out, are 19 MB, within what it may make, was kept so
// and read, below a directory's top as at it. It is refused now, said.
func TestPickCopyHeldToMaxFile(t *testing.T) {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: v1\nkind: List\nitems:\n- {apiVersion: v1, kind: Secret, metadata: {name: s}, data: {pad: %q}}\n", strings.Repeat("p", 1200<<10))
	b.WriteString("- apiVersion: monitoring.coreos.com/v1\n  kind: PrometheusRule\n  metadata:\n    name: p\n    annotations:\n")
	fmt.Fprintf(&b, "      a000: &s %q\n", strings.Repeat("s", 170<<10))
	for i := 1; i < 110; i++ {
		fmt.Fprintf(&b, "      a%03d: *s\n", i)
	}
	b.WriteString("  spec: {groups: [{name: g, rules: [{alert: A, expr: up}]}]}\n")
	dir := t.TempDir()
	for name, body := range map[string]string{"top.yaml": "groups: [{name: ok, rules: [{alert: Seen, expr: x}]}]\n", "sub/list.yaml": b.String()} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var made *MadeError
	if _, _, _, err := ReadPicked(dir, Picking{Trim: true}); !errors.As(err, &made) || !made.File || !strings.Contains(err.Error(), "list.yaml") {
		t.Errorf("%d KB: %v", b.Len()>>10, err)
	}
}
