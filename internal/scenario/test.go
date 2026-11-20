package scenario

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/rules"
)

// MaxTest is the most bytes a test file is read to, as an expectations
// file is.
const MaxTest = 1 << 20

// ReadTest reads a test file at path, as an expectations file names it,
// inside root, the directory the expectations file is in: a path that
// leads outside it, by .. or by a symbolic link, isn't read (os.Root), nor
// one that isn't a file, nor past MaxTest bytes.
func ReadTest(root *os.Root, path string) ([]byte, error) {
	if root == nil {
		return nil, errors.New("no directory to read it in")
	}
	f, err := root.Open(path)
	if err != nil {
		return nil, unread(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, unread(err)
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("not a file")
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxTest+1))
	if err != nil {
		return nil, unread(err)
	}
	if len(b) > MaxTest {
		return nil, fmt.Errorf("more than %d bytes", MaxTest)
	}
	return b, nil
}

// unread says why a test file couldn't be read, in Lapilli's words: not
// where it is (a path a server's error names), only what kept it.
func unread(err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return errors.New("no such file")
	case errors.Is(err, os.ErrPermission):
		return errors.New("not let in")
	}
	var pe *os.PathError
	if errors.As(err, &pe) && strings.Contains(pe.Err.Error(), "escapes") {
		return errors.New("it leads outside the expectations file's directory")
	}
	return errors.New("it couldn't be read")
}

// testKeys are the keys of promtool's test files: unitTestFile in
// promtool's unittest.go, which reads them strictly.
var testKeys = []string{"rule_files", "evaluation_interval", "group_eval_order", "tests", "fuzzy_compare"}

// Rewrite is a test file as Lapilli runs it: read as the rules are, in
// time linear in it and its aliases bounded; one mapping of promtool's
// keys alone, with tests; its rule_files the group Lapilli writes alone,
// whatever it named; its group_eval_order left out, as there is one group.
// What tests holds is promtool's to read.
func Rewrite(b []byte) ([]byte, error) {
	if _, err := rules.DecodeYAML(context.Background(), b); err != nil {
		return nil, err
	}
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("it is empty")
		}
		return nil, err
	}
	var more yaml.Node
	if err := dec.Decode(&more); !errors.Is(err, io.EOF) {
		return nil, errors.New("one document only, as promtool reads one")
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("want a mapping, as promtool's test files are")
	}
	var kept []*yaml.Node
	tests := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		if k.Kind != yaml.ScalarNode || !slices.Contains(testKeys, k.Value) {
			return nil, fmt.Errorf("line %d: a key promtool's test files don't have: want %s", k.Line, strings.Join(testKeys, ", "))
		}
		switch k.Value {
		case "rule_files", "group_eval_order":
			continue
		case "tests":
			if v.Kind != yaml.SequenceNode || len(v.Content) == 0 {
				return nil, fmt.Errorf("line %d: tests is a list of tests, one at least", k.Line)
			}
			tests = true
		}
		kept = append(kept, k, v)
	}
	if !tests {
		return nil, errors.New("it holds no tests")
	}
	files := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!str", Value: rulesName}}}
	root.Content = append([]*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!str", Value: "rule_files"}, files}, kept...)
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
