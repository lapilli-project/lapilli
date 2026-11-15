package rules

import (
	"context"
	"errors"
	"fmt"

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
// merge (<<) that may bring them, or a list (of documents, as a cloud's CLI
// writes). Its keys are read as yaml.v3 resolves them, escapes and aliases
// too, so a rule file can't be written so it isn't.
func rulesShaped(n *yaml.Node) bool {
	for i := 0; n != nil && i <= MaxDepth; i++ {
		switch n.Kind {
		case yaml.DocumentNode:
			if len(n.Content) != 1 {
				return false
			}
			n = n.Content[0]
		case yaml.AliasNode:
			n = n.Alias
		case yaml.SequenceNode:
			return true
		case yaml.MappingNode:
			for j := 0; j+1 < len(n.Content); j += 2 {
				k := n.Content[j]
				for d := 0; k != nil && k.Kind == yaml.AliasNode && d <= MaxDepth; d++ {
					k = k.Alias
				}
				if k != nil && (isMerge(k) || k.Kind == yaml.ScalarNode && k.Value == "groups") {
					return true
				}
			}
			return false
		default:
			return false
		}
	}
	return false
}

// configMapRules reads the rule files a ConfigMap holds, a key each. A key
// whose value isn't YAML (yaml.v3 can't decode it to its nodes), or holds
// no rules, is passed over, as not every key holds rules; one whose rules
// don't read, a duration that isn't one, is passed over and counted in
// broken, as Prometheus wouldn't load it either. One that Lapilli's own
// reader refuses, in the shape of rules, is an error naming its key:
// Prometheus may load what Lapilli doesn't read, and its rules mustn't be
// left out unsaid. So is one past what the input may make (MadeError), and
// a value read as rules already in the input, which only an alias makes:
// it would be read into rules again (errReadAgain); one read already that
// held none isn't read again.
func configMapRules(ctx context.Context, doc map[string]any, rec *[]Recording) (out []Rule, found bool, broken int, err error) {
	in := inputOf(ctx)
	data, _ := doc["data"].(map[string]any)
	for _, k := range sortedKeys(data) {
		s, _ := data[k].(string)
		switch seen, rules := in.once(s); {
		case seen && rules:
			return nil, true, broken, fmt.Errorf("%.80q: %w", k, unread{errReadAgain, true})
		case seen:
			continue
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
			in.read(s, true)
		case refused(err), errors.As(err, &un) && un.rules:
			return nil, true, broken, fmt.Errorf("%.80q: %w", k, err)
		case errors.As(err, &nx), errors.As(err, &un), errors.Is(err, errNoRules):
			in.read(s, false)
			continue // not every key holds rules
		default:
			in.read(s, false)
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
