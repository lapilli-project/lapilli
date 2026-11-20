package verdict

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// TakeIn is an expectations file with a draft taken in (docs/design/
// expectations.md, step 3): each of the draft's expectations, of a rule by
// rule:, holds the rule to what it says in place of what the file held it
// to by that rule:, each budget field, catch and scenario by itself, and
// adds what the file didn't hold it to; an entry left holding nothing is
// taken out. What else the file holds, its comments among it, stays as it
// was read. It is the file's tree, changed and written by the YAML
// encoder, never text pieced together; read back, it must be an
// expectations file (ReadExpectations), or it is refused. A file with
// anchors or aliases, which a change to one place would change in
// another, is refused: it is for a person to take in.
func TakeIn(file []byte, draft []Expectation) ([]byte, error) {
	for _, x := range draft {
		if x.Rule == "" || x.Group != "" || x.Labels != "" {
			return nil, errors.New("a draft holds a rule by rule: alone")
		}
	}
	if len(bytes.TrimSpace(file)) == 0 {
		return ExpectationsYAML(draft)
	}
	if _, err := ReadExpectations(bytes.NewReader(file)); err != nil {
		return nil, fmt.Errorf("the expectations file can't be read: %w", err)
	}
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(file))
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	var more yaml.Node
	if err := dec.Decode(&more); !errors.Is(err, io.EOF) {
		return nil, errors.New("the expectations file holds more than one document")
	}
	if aliased(&doc) {
		return nil, errors.New("the expectations file has anchors or aliases: take the draft in by hand")
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("the expectations file isn't a mapping")
	}
	seq := valueOf(root, "expectations")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		seq = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		setValue(root, "expectations", seq)
	}
	seq.Style = 0
	for _, x := range draft {
		for _, e := range seq.Content {
			if e.Kind != yaml.MappingNode || scalarOf(e, "rule") != x.Rule {
				continue
			}
			if b := valueOf(e, "budget"); b != nil && b.Kind == yaml.MappingNode && x.Budget != nil {
				for k, v := range map[string]*int{"week": x.Budget.Week, "asleep": x.Budget.Asleep, "refires": x.Budget.Refires} {
					if v != nil {
						remove(b, k)
					}
				}
				if len(b.Content) == 0 {
					remove(e, "budget")
				}
			}
			if c := valueOf(e, "catches"); c != nil && c.Kind == yaml.SequenceNode {
				c.Content = slices.DeleteFunc(c.Content, func(n *yaml.Node) bool {
					return slices.ContainsFunc(x.Catches, func(k ExpectCatch) bool { return scalarOf(n, "incident") == k.Incident })
				})
				if len(c.Content) == 0 {
					remove(e, "catches")
				}
			}
			if s := valueOf(e, "scenarios"); s != nil && s.Kind == yaml.SequenceNode {
				s.Content = slices.DeleteFunc(s.Content, func(n *yaml.Node) bool { return slices.Contains(x.Scenarios, n.Value) })
				if len(s.Content) == 0 {
					remove(e, "scenarios")
				}
			}
		}
		// An entry left holding its rule to nothing is taken out.
		seq.Content = slices.DeleteFunc(seq.Content, func(e *yaml.Node) bool {
			return e.Kind == yaml.MappingNode && scalarOf(e, "rule") == x.Rule && valueOf(e, "budget") == nil && valueOf(e, "catches") == nil && valueOf(e, "scenarios") == nil
		})
		n, err := expectationNode(x)
		if err != nil {
			return nil, err
		}
		oneLine(n)
		at := slices.IndexFunc(seq.Content, func(e *yaml.Node) bool { return e.Kind == yaml.MappingNode && scalarOf(e, "rule") == x.Rule })
		if at < 0 {
			seq.Content = append(seq.Content, n)
			continue
		}
		e := seq.Content[at]
		for _, key := range []string{"budget", "catches", "scenarios"} {
			v := valueOf(n, key)
			if v == nil {
				continue
			}
			have := valueOf(e, key)
			switch {
			case have == nil:
				setValue(e, key, v)
			case key == "budget" && have.Kind == yaml.MappingNode:
				have.Style = 0
				have.Content = append(have.Content, v.Content...)
			case have.Kind == yaml.SequenceNode:
				have.Content = append(have.Content, v.Content...)
			default:
				return nil, fmt.Errorf("%s's %s isn't as an expectations file writes it: take the draft in by hand", x.Rule, key)
			}
		}
	}
	out, err := encodeYAML(&doc)
	if err != nil {
		return nil, err
	}
	if _, err := ReadExpectations(bytes.NewReader(out)); err != nil {
		return nil, fmt.Errorf("the expectations file with the draft taken in doesn't read: %w", err)
	}
	return out, nil
}

// aliased says a tree has an anchor or an alias.
func aliased(n *yaml.Node) bool {
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return true
	}
	return slices.ContainsFunc(n.Content, aliased)
}

// valueOf is a mapping's value of key, nil for none.
func valueOf(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// scalarOf is a mapping's value of key, a scalar's text; "" for none.
func scalarOf(m *yaml.Node, key string) string {
	if m.Kind != yaml.MappingNode {
		return ""
	}
	if v := valueOf(m, key); v != nil && v.Kind == yaml.ScalarNode {
		return v.Value
	}
	return ""
}

// setValue sets a mapping's value of key, added at its end when it has
// none.
func setValue(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
}

// remove takes key out of a mapping.
func remove(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = slices.Delete(m.Content, i, i+2)
			return
		}
	}
}

// DraftEntries are a draft's expectations, read as an expectations file
// is, held to its rule by rule: alone: what an agent's draft, or one kept,
// holds. Any other entry is refused.
func DraftEntries(text, rule string) ([]Expectation, error) {
	e, err := ReadExpectations(strings.NewReader(text))
	if err != nil {
		return nil, err
	}
	for _, x := range e.Expectations {
		if x.Rule != rule {
			return nil, fmt.Errorf("a draft holds %s by rule: %s alone, not %s", Clean(rule), Clean(rule), Clean(x.Selector()))
		}
	}
	if len(e.Expectations) == 0 {
		return nil, fmt.Errorf("the draft holds %s to nothing", Clean(rule))
	}
	return e.Expectations, nil
}
