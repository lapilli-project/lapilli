package rules

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"go.yaml.in/yaml/v3"
)

// notYAML is a document yaml.v3 can't decode to its nodes: not YAML, as a
// ConfigMap's value that isn't rules may be.
type notYAML struct{ error }

func (e notYAML) Unwrap() error { return e.error }

// unread is a document yaml.v3 decodes to its nodes that Lapilli's own
// reader refuses (Nodes, YAMLValue): past MaxKeys or MaxDepth, aliases past
// what yaml.v3 or the input allows as Lapilli counts them. Prometheus may
// load it. rules says it is in the shape of rules (rulesShaped).
type unread struct {
	error
	rules bool
}

func (e unread) Unwrap() error { return e.error }

// rulesShaped reports whether a document, as its nodes, is in the shape of
// rules, as a rule file a ConfigMap holds is: a mapping with groups, or a
// merge (<<) that may bring them; or a list (of documents, as a cloud's CLI
// writes) one of whose items is a mapping with groups or a kind. Its keys
// are read as yaml.v3 resolves them, escapes and aliases too, so a rule
// file can't be written so it isn't. A list of targets, as a file_sd file
// is, isn't.
func rulesShaped(n *yaml.Node) bool {
	n = resolved(n)
	switch {
	case n == nil:
		return false
	case n.Kind == yaml.MappingNode:
		return hasKey(n, true, "groups")
	case n.Kind == yaml.SequenceNode:
		for _, it := range n.Content {
			if it = resolved(it); it != nil && it.Kind == yaml.MappingNode && hasKey(it, false, "groups", "kind") {
				return true
			}
		}
	}
	return false
}

// resolved is a node with the document it is and the aliases it is of
// read through: what it is.
func resolved(n *yaml.Node) *yaml.Node {
	for i := 0; n != nil && i <= MaxDepth; i++ {
		switch {
		case n.Kind == yaml.DocumentNode && len(n.Content) == 1:
			n = n.Content[0]
		case n.Kind == yaml.AliasNode:
			n = n.Alias
		default:
			return n
		}
	}
	return nil
}

// hasKey reports whether a mapping has one of keys, or, when merges, a
// merge (<<) that may bring it.
func hasKey(m *yaml.Node, merges bool, keys ...string) bool {
	for j := 0; j+1 < len(m.Content); j += 2 {
		k := resolved(m.Content[j])
		if k == nil || k.Kind != yaml.ScalarNode {
			continue
		}
		if merges && isMerge(k) || slices.Contains(keys, k.Value) {
			return true
		}
	}
	return false
}

// textShaped reports whether a document yaml.v3 wasn't asked to decode, as
// Nodes refused it, may be in the shape of rules: whether groups is
// written in it, or a backslash, which could write it in a quoted key.
func textShaped(b []byte) bool {
	return bytes.Contains(b, []byte("groups")) || bytes.IndexByte(b, '\\') >= 0
}

// configMapRules reads the rule files a ConfigMap holds, a key each. A key
// whose value isn't YAML (yaml.v3 can't decode it to its nodes), or isn't
// in the shape of rules, or holds no rules, is passed over, as not every
// key holds rules, whatever Lapilli's reader says of it; one whose rules
// don't read, a duration that isn't one or a key given twice, is passed
// over and counted in broken, as Prometheus wouldn't load it either. One
// in the shape of rules that Lapilli's own reader refuses is an error
// naming its key: Prometheus may load what Lapilli doesn't read, and its
// rules mustn't be left out unsaid. So is one past what the input may make
// (MadeError), and a value a YAML alias brings in (errAliased), which would
// be read into rules again for each alias, decoded afresh if !!binary.
// binaryData isn't read for rules.
func configMapRules(ctx context.Context, doc map[string]any, rec *[]Recording) (out []Rule, found bool, broken int, err error) {
	in := inputOf(ctx)
	data, _ := doc["data"].(map[string]any)
	for _, k := range sortedKeys(data) {
		s, _ := data[k].(string)
		if in.broughtIn(s) {
			return nil, true, broken, fmt.Errorf("%.80q: %w", k, unread{errAliased, true})
		}
		var more *[]Recording
		if rec != nil {
			more = &[]Recording{}
		}
		rs, err := parseContext(ctx, []byte(s), more)
		var nx notYAML
		var un unread
		switch {
		case err == nil:
		case refused(err), errors.As(err, &un) && un.rules:
			return nil, true, broken, fmt.Errorf("%.80q: %w", k, err)
		case errors.As(err, &nx), errors.As(err, &un), errors.Is(err, errNoRules):
			continue // not every key holds rules
		default:
			broken++
			continue
		}
		if rec != nil {
			*rec = append(*rec, *more...)
		}
		found = true
		out = append(out, rs...)
	}
	return out, found, broken, nil
}
