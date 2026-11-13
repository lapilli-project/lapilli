package rules

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// aliased is a YAML document of some 100,000 values, 90% of them what
// aliases expand to: within yaml.v3's bound on one document (99% of one of
// 400,000 values at most), and so each of a stream of them.
func aliased(name string) string {
	return name + ": &a [" + strings.Repeat("x, ", 9999) + "x]\n" + name + "s: [" + strings.Repeat("*a, ", 8) + "*a]\n"
}

// What aliases expand to is counted for an input, not for each of its
// documents: twenty documents of 90% aliases, each of which yaml.v3 reads,
// are some 2,000,000 values in all, and past some 770,000 a stream may be
// less than 90% aliases. So too the files of a directory, the pieces a
// stream is picked in, and the rule files a ConfigMap holds.
func TestAliasesCountedPerInput(t *testing.T) {
	var docs []string
	for i := range 20 {
		docs = append(docs, aliased(fmt.Sprintf("d%d", i)))
	}
	stream := strings.Join(docs, "---\n")
	var one any
	if err := yaml.Unmarshal([]byte(docs[0]), &one); err != nil {
		t.Fatalf("one document, yaml.v3: %v", err)
	}
	if _, err := DecodeYAML(context.Background(), []byte(docs[0])); err != nil {
		t.Fatalf("one document: %v", err)
	}
	excessive := func(what string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "excessive aliasing") {
			t.Errorf("%s: %v", what, err)
		}
	}
	_, err := DecodeYAML(context.Background(), []byte(stream))
	excessive("a stream, decoded", err)
	_, err = Parse([]byte(stream))
	excessive("a stream, parsed", err)
	_, _, err = Pick([]byte(stream), Selector{})
	excessive("a stream, picked", err)
	_, err = FromDocs([]Doc{{Source: "a", Data: []byte(stream[:len(stream)/2])}, {Source: "b", Data: []byte(stream[len(stream)/2:])}})
	excessive("two documents of rules", err)
	// A ConfigMap's rule files, each a value of its own.
	var cm strings.Builder
	cm.WriteString("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: rules}\ndata:\n")
	for i, d := range docs {
		fmt.Fprintf(&cm, "  f%02d.yaml: |\n    %s\n", i, strings.ReplaceAll(strings.TrimSuffix(d, "\n"), "\n", "\n    "))
	}
	// A value that doesn't read is passed over, as not every one holds
	// rules: those past the bound are refused at once, each counted with
	// the rest.
	ctx := OneInput(context.Background())
	parseContext(ctx, []byte(cm.String()), nil)
	if n := ctx.Value(inputKey{}).(*input).decoded; n < 500000 || n > 1500000 {
		t.Errorf("a ConfigMap's rule files: %d values decoded, counted as of the ConfigMap", n)
	}
	// A directory's files.
	dir := t.TempDir()
	for i, d := range docs {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d.yaml", i)), []byte(d), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, _, _, err = ReadPicked(dir, Picking{})
	excessive("a directory's files", err)
}

// Under NoAliases, as what a ruler's API, a Grafana's export or a cloud's
// API writes is read, an alias is refused: as a value, as a key, and as a
// merge.
func TestNoAliases(t *testing.T) {
	for _, doc := range []string{"a: &a [1]\nb: *a\n", "? &k a\n: 1\n*k : 2\n", "a: &a {x: 1}\nb: {<<: *a}\n"} {
		var n yaml.Node
		if err := yaml.Unmarshal([]byte(doc), &n); err != nil {
			t.Fatal(err)
		}
		if _, err := YAMLValueContext(NoAliases(context.Background()), &n); err == nil || !strings.Contains(err.Error(), "an alias") {
			t.Errorf("%q: %v", doc, err)
		}
		if err := yaml.Unmarshal([]byte(doc), &n); err != nil {
			t.Fatal(err)
		}
		if _, err := YAMLValueContext(context.Background(), &n); err != nil && !strings.Contains(err.Error(), "already defined") {
			t.Errorf("%q, aliases read: %v", doc, err)
		}
	}
	if _, err := FromDocsContext(NoAliases(context.Background()), []Doc{{Source: "x", Data: []byte("groups:\n- name: g\n  rules: &r\n  - {alert: A, expr: up}\n- name: h\n  rules: *r\n")}}); err == nil {
		t.Error("a group's rules by alias, read where none is")
	}
}

// A fault in reading a document's nodes is an error, not a panic, which
// lapilli serve's work loop doesn't recover: here a list holding a nil
// node, as yaml.v3 never makes one.
func TestYAMLValueFault(t *testing.T) {
	n := &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{nil}}
	_, err := YAMLValue(n)
	var e errNode
	if !errors.As(err, &e) || !strings.Contains(err.Error(), "fault of lapilli's") {
		t.Errorf("%v", err)
	}
}
