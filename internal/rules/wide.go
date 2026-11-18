package rules

import "strings"

// Wide is the first series selector of a PromQL expression that reads past
// one metric: matchers with no metric's name ({job="api"}), or a name
// matched as a pattern or a negation (__name__=~"api_.+",
// {__name__!="up"}). A pull request's rule backtested through lapilli serve
// reads with serve's access, of a Prometheus others share, and is held to
// one metric a selector: what it reads is what it names. An expression
// that doesn't read as Selectors reads it is wide as a whole, said as
// itself.
func Wide(expr string) (string, bool) {
	sels, err := Selectors(expr)
	if err != nil {
		return expr, true
	}
	for _, sel := range sels {
		ms, _, ok := selectorParts(sel)
		if !ok {
			return sel, true
		}
		named := false
		for _, m := range ms {
			label, op, value, ok := splitMatcher(m)
			if !ok {
				return sel, true
			}
			if l, err := unquoteLabel(label); err || l != "__name__" {
				continue
			}
			if op != "=" {
				return sel, true
			}
			if v, ok := unquote(value); !ok || v == "" {
				return sel, true
			}
			named = true
		}
		if !named {
			return sel, true
		}
	}
	return "", false
}

// unquoteLabel is a label's name as written, bare or quoted (Prometheus
// 3's {"http.requests", "le"="1"}); err says a quoted one didn't unquote.
func unquoteLabel(label string) (string, bool) {
	label = strings.TrimSpace(label)
	if label != "" && (label[0] == '"' || label[0] == '\'' || label[0] == '`') {
		v, ok := unquote(label)
		return v, !ok
	}
	return label, false
}
