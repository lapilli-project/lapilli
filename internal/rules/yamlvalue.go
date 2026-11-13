package rules

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"go.yaml.in/yaml/v3"
)

// What a rule document may be, as read from a file, a ruler, a Grafana or
// a cloud: nested so deep at most, and a mapping of so many keys. yaml.v3
// decodes a mapping into Go values checking its keys for duplicates pair by
// pair, in time that grows with the square of its keys: a rule of 100,000
// label keys, a document of a MB, held a CPU for minutes. A document is
// decoded to its nodes, which yaml.v3 does in linear time, and then to Go
// values here (YAMLValue).
const (
	MaxDepth = 200
	MaxKeys  = 10000
)

// What one document may parse into, as Nodes counts it: yaml.v3 makes a
// node of each value, and a value read takes some 320 bytes of heap at
// its peak, the parser's and the nodes' (a flow list of zeros is a value
// every two bytes, some 160 times its size). A document is read at a value
// every NodeBytes bytes at most, past its first MinNodes, and MaxNodes in
// all: what it takes then is bounded by its size, at some 85 times it,
// and by MaxNodes, at some 1.9 GiB, a little more than the largest real
// one a ruler's list can be takes (64 MB of Grafana's classic rules, 5.2
// million values, 1.5 GiB). Real ones are a value every 5 to 15 bytes: a
// Grafana export's classic rule 12.5 (2.5 KB and 200 values), a rule
// file's 13.5, a terse one written in flow style 5.4, a cloud's alarms as
// JSON 5.5 to 6; 25,000 of Grafana's rules are 5 million.
const (
	NodeBytes = 4
	MinNodes  = 100000
	MaxNodes  = 6000000
)

// Nodes says whether b may parse into more values than it is read with:
// each but the first needs one of , : - [ { or a line of its own.
func Nodes(b []byte) error {
	n := 0
	for _, c := range b {
		switch c {
		case ',', ':', '-', '[', '{', '\n':
			n++
		}
	}
	switch {
	case n > MaxNodes:
		return fmt.Errorf("yaml: a document of more than %d values (%d), more than is read", MaxNodes, n)
	case n > MinNodes+len(b)/NodeBytes:
		return fmt.Errorf("yaml: a document of %d values in %d bytes, more than one every %d bytes, which is read", n, len(b), NodeBytes)
	}
	return nil
}

// YAMLValue is a YAML node as yaml.v3 decodes one into an any: a mapping
// as map[string]any (map[any]any when a key isn't a string), a sequence as
// []any, a scalar as yaml.v3 resolves it; merges (<<), aliases, and a key
// given twice refused, as yaml.v3 does. What aliases expand to is bounded
// as yaml.v3 bounds it, each value and each key visited counted, the keys
// a merge (<<) brings in again too: a document yaml.v3 refuses as
// excessive aliasing is refused, though one near that bound may be
// refused by one and not the other, as they count apart. It takes time
// linear in the node, and refuses one nested deeper than MaxDepth, or a
// mapping of more than MaxKeys keys.
func YAMLValue(n *yaml.Node) (any, error) { return YAMLValueContext(context.Background(), n) }

// YAMLValueContext is YAMLValue, given up when ctx ends, what aliases
// expand to counted with the rest of the input ctx says it is of
// (OneInput), or refused (NoAliases). A fault in it is an error, not a
// panic: what is read is outside input, and lapilli serve reads it in a
// loop no panic should end.
func YAMLValueContext(ctx context.Context, n *yaml.Node) (v any, err error) {
	b, _ := ctx.Value(inputKey{}).(*input)
	if b == nil {
		b = &input{}
	}
	b.mu.Lock()
	c := &converting{ctx: ctx, none: b.none, aliases: map[*yaml.Node]bool{}, decoded: b.decoded, aliased: b.aliased}
	b.mu.Unlock()
	decoded, aliased := c.decoded, c.aliased
	defer func() {
		if p := recover(); p != nil {
			v, err = nil, errNode{fmt.Sprintf("a document lapilli failed to read (%v), which is a fault of lapilli's", p)}
		}
		b.mu.Lock()
		b.decoded += c.decoded - decoded
		b.aliased += c.aliased - aliased
		b.mu.Unlock()
	}()
	return c.value(n, 0)
}

// input is what an input's YAML documents have decoded so far, and what of
// it came of aliases: counted together, as yaml.v3 counts one document's,
// so that what aliases expand to is bounded by the input, not by each of
// its documents (a ruler's namespaces, a stream's documents, the rule
// files a ConfigMap or an Amazon namespace holds, the files of a
// directory). none says aliases are refused.
type input struct {
	mu               sync.Mutex
	decoded, aliased int
	none             bool
}

type inputKey struct{}

// OneInput is ctx under which every YAML document read is counted as of
// one input, as one document is, unless ctx says so already.
func OneInput(ctx context.Context) context.Context {
	if _, ok := ctx.Value(inputKey{}).(*input); ok {
		return ctx
	}
	return context.WithValue(ctx, inputKey{}, &input{})
}

// NoAliases is ctx under which a YAML alias (*name) is refused: what a
// ruler's API, a Grafana's export or a cloud's API writes holds none, as
// they write YAML or JSON out of what they hold, and one that does is
// hostile, its aliases expanding a few KB into GBs.
func NoAliases(ctx context.Context) context.Context {
	return context.WithValue(ctx, inputKey{}, &input{none: true})
}

type converting struct {
	ctx context.Context
	// none says an alias is refused (NoAliases).
	none    bool
	aliases map[*yaml.Node]bool
	// decoded and aliased count the values and keys visited, and those
	// visited in an alias's expansion, as yaml.v3 counts them.
	decoded, aliased, inAlias int
	// anchored is how many anchors the node visited is under: one under
	// none is let go of once decoded, as no alias reads it again.
	anchored int
}

// errExcessive is a document whose aliases expand to more than yaml.v3
// allows.
var errExcessive = errNode{"document contains excessive aliasing"}

// step counts a value or a key visited, refusing the document past
// yaml.v3's bound on what aliases expand to, and looks at the context
// every 1,024 steps.
func (c *converting) step() error {
	c.decoded++
	if c.inAlias > 0 {
		c.aliased++
	}
	if c.aliased > 100 && c.decoded > 1000 && float64(c.aliased)/float64(c.decoded) > aliasRatio(c.decoded) {
		return errExcessive
	}
	if c.decoded%1024 == 0 {
		return c.ctx.Err()
	}
	return nil
}

// alias refuses an alias where none is read (NoAliases).
func (c *converting) alias(n *yaml.Node) error {
	if c.none {
		return errNode{fmt.Sprintf("line %d: an alias (*%.40s), which isn't read in what an API writes", n.Line, n.Value)}
	}
	return nil
}

// errNode is a YAML document that isn't one lapilli reads.
type errNode struct{ msg string }

func (e errNode) Error() string { return "yaml: " + e.msg }

func (c *converting) value(n *yaml.Node, depth int) (any, error) {
	if depth > MaxDepth {
		return nil, errNode{fmt.Sprintf("line %d: nested more than %d deep", n.Line, MaxDepth)}
	}
	if err := c.step(); err != nil {
		return nil, err
	}
	if n.Anchor != "" {
		c.anchored++
		defer func() { c.anchored-- }()
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) != 1 {
			return nil, nil
		}
		return c.value(n.Content[0], depth+1)
	case yaml.AliasNode:
		if err := c.alias(n); err != nil {
			return nil, err
		}
		if c.aliases[n] {
			return nil, errNode{fmt.Sprintf("anchor '%s' value contains itself", n.Value)}
		}
		if n.Alias == nil {
			return nil, errNode{fmt.Sprintf("unknown anchor '%s' referenced", n.Value)}
		}
		c.aliases[n] = true
		c.inAlias++
		v, err := c.value(n.Alias, depth+1)
		c.inAlias--
		delete(c.aliases, n)
		return v, err
	case yaml.ScalarNode:
		return scalar(n)
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for i, e := range n.Content {
			v, err := c.value(e, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
			c.letGo(n, i)
		}
		return out, nil
	case yaml.MappingNode:
		return c.mapping(n, depth)
	case 0:
		if n.IsZero() {
			return nil, nil
		}
	}
	return nil, errNode{fmt.Sprintf("cannot decode node with unknown kind %d", n.Kind)}
}

// scalar is a scalar as yaml.v3 resolves it: a string (as the parser
// tags one, quoted or plain) as it is written, a null untagged as nil,
// anything else as yaml.v3 itself decodes it.
func scalar(n *yaml.Node) (any, error) {
	switch {
	case n.Tag == "!!str":
		return n.Value, nil
	case n.Tag == "!!null" && n.Style&yaml.TaggedStyle == 0:
		return nil, nil
	}
	var v any
	if err := n.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// letGo lets go of a node's i'th child once it is decoded, so that the
// tree, some 20 times what it decodes to, goes as the values come: not
// under an anchor, or in an alias's expansion, which read it again.
func (c *converting) letGo(n *yaml.Node, i int) {
	if c.anchored == 0 && c.inAlias == 0 {
		n.Content[i] = nil
	}
}

// mapping is a mapping as yaml.v3 decodes one into an any.
func (c *converting) mapping(n *yaml.Node, depth int) (any, error) {
	if len(n.Content)/2 > MaxKeys {
		return nil, errNode{fmt.Sprintf("line %d: a mapping of more than %d keys", n.Line, MaxKeys)}
	}
	strs := true
	for i := 0; i+1 < len(n.Content); i += 2 {
		if t := n.Content[i].ShortTag(); t != "!!str" && t != "!!merge" {
			strs = false
			break
		}
	}
	m := &mapOut{}
	if strs {
		m.s = make(map[string]any, len(n.Content)/2)
	} else {
		m.g = make(map[any]any, len(n.Content)/2)
	}
	if err := c.fill(n, m, nil, depth); err != nil {
		return nil, err
	}
	if strs {
		return m.s, nil
	}
	return m.g, nil
}

// mapOut is the map a mapping, and those merged into it, are decoded into.
type mapOut struct {
	s map[string]any
	g map[any]any
}

// key is a key node's value as the map holds it: in a string map, the
// text of the scalar it is or stands for, as yaml.v3 decodes a key into a
// string.
func (m *mapOut) key(k *yaml.Node, c *converting, depth int) (any, error) {
	if k.Kind == yaml.AliasNode {
		if err := c.alias(k); err != nil {
			return nil, err
		}
	}
	target := k
	for i := 0; target.Kind == yaml.AliasNode && target.Alias != nil && i <= MaxDepth; i++ {
		target = target.Alias
	}
	if target.Kind != yaml.ScalarNode {
		return nil, errNode{fmt.Sprintf("line %d: invalid map key", k.Line)}
	}
	if m.s != nil {
		return target.Value, nil
	}
	return c.value(k, depth+1)
}

func (m *mapOut) set(k, v any) {
	if m.s != nil {
		m.s[k.(string)] = v
		return
	}
	m.g[k] = v
}

// keyOf is a key node as yaml.v3 tells keys given twice apart.
type keyOf struct {
	kind  yaml.Kind
	value string
}

// fill decodes the pairs of a mapping into m, those of merged mappings
// (<<) after its own; merged, when merging, is the keys set already, which
// a merged mapping doesn't set again.
func (c *converting) fill(n *yaml.Node, m *mapOut, merged map[any]bool, depth int) error {
	seen := make(map[keyOf]int, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i]
		id := keyOf{k.Kind, k.Value}
		if line, ok := seen[id]; ok {
			return errNode{fmt.Sprintf("unmarshal errors:\n  line %d: mapping key %#v already defined at line %d", k.Line, k.Value, line)}
		}
		seen[id] = k.Line
	}
	var merge *yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if isMerge(k) {
			merge = v
			continue
		}
		if err := c.step(); err != nil {
			return err
		}
		key, err := m.key(k, c, depth)
		if err != nil {
			return err
		}
		if merged != nil {
			if merged[key] {
				continue
			}
			merged[key] = true
		}
		val, err := c.value(v, depth+1)
		if err != nil {
			return err
		}
		m.set(key, val)
		c.letGo(n, i+1)
	}
	if merge == nil {
		return nil
	}
	if merged == nil {
		merged = map[any]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			if key, err := m.key(n.Content[i], c, depth); err == nil {
				merged[key] = true
			}
		}
	}
	into := func(x *yaml.Node) error {
		mapping := x
		if x.Kind == yaml.AliasNode {
			mapping = x.Alias
		}
		if mapping == nil || mapping.Kind != yaml.MappingNode {
			return errNode{"map merge requires map or sequence of maps as the value"}
		}
		if x.Kind == yaml.AliasNode {
			if err := c.alias(x); err != nil {
				return err
			}
			if c.aliases[x] {
				return errNode{fmt.Sprintf("anchor '%s' value contains itself", x.Value)}
			}
			c.aliases[x] = true
			c.inAlias++
			defer func() { c.inAlias--; delete(c.aliases, x) }()
		}
		// A merge's mapping written in place under an anchor (<<: &m
		// {...}) is read by an alias after it: not let go of, as one
		// value reads is.
		if mapping.Anchor != "" {
			c.anchored++
			defer func() { c.anchored-- }()
		}
		if len(mapping.Content)/2 > MaxKeys {
			return errNode{fmt.Sprintf("line %d: a mapping of more than %d keys", mapping.Line, MaxKeys)}
		}
		if depth+1 > MaxDepth {
			return errNode{fmt.Sprintf("line %d: nested more than %d deep", mapping.Line, MaxDepth)}
		}
		return c.fill(mapping, m, merged, depth+1)
	}
	switch merge.Kind {
	case yaml.MappingNode, yaml.AliasNode:
		return into(merge)
	case yaml.SequenceNode:
		// So too a merge's list written in place under an anchor (<<: &s
		// [...]).
		if merge.Anchor != "" {
			c.anchored++
			defer func() { c.anchored-- }()
		}
		for _, x := range merge.Content {
			if err := into(x); err != nil {
				return err
			}
		}
		return nil
	}
	return errNode{"map merge requires map or sequence of maps as the value"}
}

// isMerge is yaml.v3's: a key << untagged, or tagged a merge.
func isMerge(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Value == "<<" && (n.Tag == "" || n.Tag == "!" || n.Tag == "!!merge" || n.Tag == "tag:yaml.org,2002:merge")
}

// aliasRatio is how much of what a document decodes to may come of its
// aliases, as yaml.v3 allows: 99% of a small document, 10% of a large one.
func aliasRatio(decoded int) float64 {
	const low, high = 400000, 4000000
	switch {
	case decoded <= low:
		return 0.99
	case decoded >= high:
		return 0.10
	}
	return 0.99 - 0.89*(float64(decoded-low)/float64(high-low))
}

// DecodeYAML is the YAML documents in data as yaml.v3 decodes each into
// an any, decoded to their nodes and then to values (YAMLValue): in time
// linear in data, a document of more values than Nodes allows refused,
// what aliases expand to counted for them all (OneInput), given up when
// ctx ends.
func DecodeYAML(ctx context.Context, data []byte) ([]any, error) {
	if err := Nodes(data); err != nil {
		return nil, err
	}
	ctx = OneInput(ctx)
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var out []any
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var n yaml.Node
		err := dec.Decode(&n)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		v, err := YAMLValueContext(ctx, &n)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
}

// UnmarshalYAML is the first YAML document in data as yaml.Unmarshal
// decodes it into an any, in time linear in data (DecodeYAML).
func UnmarshalYAML(data []byte) (any, error) {
	if err := Nodes(data); err != nil {
		return nil, err
	}
	var n yaml.Node
	if err := yaml.Unmarshal(data, &n); err != nil {
		return nil, err
	}
	return YAMLValue(&n)
}
