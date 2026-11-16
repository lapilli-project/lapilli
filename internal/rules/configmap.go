package rules

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"unicode"
	"unicode/utf8"

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
// spec with groups (a PrometheusRule, a Google Rules), or a merge (<<) that
// may bring them; or a list (of documents, as a cloud's CLI writes) one of
// whose items is a mapping with groups or a kind. Its keys are read as
// yaml.v3 resolves them, escapes and aliases too, and as their text is
// read (keysAsText), so a rule file can't be written so it isn't. A list
// of targets, as a file_sd file is, isn't.
func rulesShaped(n *yaml.Node) bool {
	n = resolved(n)
	switch {
	case n == nil:
		return false
	case n.Kind == yaml.MappingNode:
		if hasKey(n, true, "groups") {
			return true
		}
		spec := resolved(keyed(n, "spec"))
		return spec != nil && spec.Kind == yaml.MappingNode && hasKey(spec, true, "groups")
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

// keyed is the value of a mapping's key k (isKey), nil for none.
func keyed(m *yaml.Node, k string) *yaml.Node {
	for j := 0; j+1 < len(m.Content); j += 2 {
		if isKey(resolved(m.Content[j]), k) {
			return m.Content[j+1]
		}
	}
	return nil
}

// hasKey reports whether a mapping has one of keys (isKey), or, when
// merges, a merge (<<) that may bring it.
func hasKey(m *yaml.Node, merges bool, keys ...string) bool {
	for j := 0; j+1 < len(m.Content); j += 2 {
		k := resolved(m.Content[j])
		if merges && k != nil && isMerge(k) || isKey(k, keys...) {
			return true
		}
	}
	return false
}

// isKey reports whether a key node is one of keys: as written, whatever
// its tag, or as its text is read (scalarText), a !!binary one decoded.
func isKey(k *yaml.Node, keys ...string) bool {
	if k == nil || k.Kind != yaml.ScalarNode {
		return false
	}
	if slices.Contains(keys, k.Value) {
		return true
	}
	t, ok := scalarText(k)
	return ok && slices.Contains(keys, t)
}

// textShaped reports whether a document Lapilli's nodes don't tell the
// shape of (Nodes refused it, an alias brought it in again, or it is the
// input a refused document is of: unreadAs) may be in the shape of rules:
// whether groups may be a key in it, as a key's text is read (keysAsText).
// Such a key is groups written; or written by double-quoted escapes
// (\x67roups, an escaped line break), seen once they are read
// (unescaped); or a !!binary key whose base64 decodes to groups,
// Z3JvdXBz, however its tag is written (!!binary, a %-escape, a %TAG
// directive) and whatever spaces or line breaks it is written across, as
// a key's !!binary is read without them. Only these: a backslash, or
// binary, written in a file of data or an env-style value says nothing of
// rules, and a directory holding such a file isn't refused for it.
func textShaped(b []byte) bool {
	if bytes.Contains(b, groupsText) || bytes.Contains(b, groupsBase64) {
		return true
	}
	if bytes.IndexByte(b, '\\') >= 0 {
		if b = unescaped(b); bytes.Contains(b, groupsText) {
			return true
		}
	}
	return bytes.Contains(unspaced(b), groupsBase64)
}

var (
	groupsText   = []byte("groups")
	groupsBase64 = []byte(base64.StdEncoding.EncodeToString(groupsText))
)

// yamlEscapes are what YAML's double-quoted escapes of one character
// stand for.
var yamlEscapes = map[byte]rune{'0': 0, 'a': 7, 'b': 8, 't': 9, '\t': 9, 'n': 10, 'v': 11, 'f': 12, 'r': 13, 'e': 0x1b,
	' ': ' ', '"': '"', '/': '/', '\\': '\\', '\'': '\'', 'N': 0x85, '_': 0xa0, 'L': 0x2028, 'P': 0x2029}

// unescaped is b with YAML's double-quoted escapes read, wherever they
// are: \x67, \u0067 and \U00000067 as g, \t, \N, \L and the rest as what
// they stand for, an escaped line break and the blanks after it as
// nothing, a backslash before anything else as that.
func unescaped(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' || i+1 == len(b) {
			out = append(out, b[i])
			continue
		}
		i++
		switch e := b[i]; e {
		case 'x', 'u', 'U':
			n := map[byte]int{'x': 2, 'u': 4, 'U': 8}[e]
			if i+n < len(b) {
				if v, err := strconv.ParseUint(string(b[i+1:i+1+n]), 16, 32); err == nil && v <= utf8.MaxRune {
					out = utf8.AppendRune(out, rune(v))
					i += n
					continue
				}
			}
			out = append(out, e)
		case '\n', '\r':
			if e == '\r' && i+1 < len(b) && b[i+1] == '\n' {
				i++
			}
			for i+1 < len(b) && (b[i+1] == ' ' || b[i+1] == '\t') {
				i++
			}
		default:
			if r, ok := yamlEscapes[e]; ok {
				out = utf8.AppendRune(out, r)
			} else {
				out = append(out, e)
			}
		}
	}
	return out
}

// unspaced is b without what unicode calls space, as a !!binary key is
// read (scalarText).
func unspaced(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		if !unicode.IsSpace(r) {
			out = append(out, b[:n]...)
		}
		b = b[n:]
	}
	return out
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
// (MadeError), and a value in the shape of rules a YAML alias brings in
// (errAliased), which would be read into rules again for each alias,
// decoded afresh if !!binary; one that isn't, by its text, is passed over
// unread. binaryData isn't read for rules.
func configMapRules(ctx context.Context, doc map[string]any, rec *[]Recording) (out []Rule, found bool, broken int, err error) {
	in := inputOf(ctx)
	data, _ := doc["data"].(map[string]any)
	for _, k := range sortedKeys(data) {
		s, _ := data[k].(string)
		if in.broughtIn(s) {
			if !textShaped([]byte(s)) {
				continue // an alias of what isn't rules, unread, as before
			}
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
