package content

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/template"

	"lapilli.dev/internal/rules"
)

// Alertmanager's notification templates (docs/design/content.md,
// "Alertmanager's templates"): a rule Prometheus, a ruler or Loki sends to
// Alertmanager is told with what the fields of the receivers its route tree
// reaches say, which can name labels and annotations the rule's alerts
// don't have, or not parse. Read from Alertmanager's configuration: its
// route tree, its receivers, and the template files it names; or from
// Mimir's, Cortex's or Amazon's, which keep the files beside it. Only what
// is needed is read, never a secret: no field whose name says it holds
// one is parsed. Each template is parsed, never executed.

// A TemplateFile is a file of Alertmanager's notification templates: its
// name, as Lapilli read it, and what it holds.
type TemplateFile struct {
	Name string
	Text []byte
}

// What of Alertmanager's configuration is read: a template file of
// MaxTemplateFile bytes at most, MaxTemplateFiles of them and
// MaxTemplateBytes in all; maxReceivers receivers, and maxFields of their
// fields that are templates, for all of them.
const (
	MaxTemplateFile  = 256 << 10
	MaxTemplateFiles = 256
	MaxTemplateBytes = 8 << 20
	maxReceivers     = 5000
	maxFields        = 20000
)

// ReadAlertmanager reads Alertmanager's configuration, as alertmanager.yml
// has it, and the template files it names, in the order Alertmanager loads
// them: a name a later file defines is that file's. A file that doesn't
// parse is said in skipped, as Alertmanager won't load the configuration;
// the names it defines are taken as templates that don't parse, so a rule
// sent with one is told. A receiver's field that doesn't parse fails when
// Alertmanager sends with it, and is told likewise.
func ReadAlertmanager(config []byte, files []TemplateFile) (n *Notifications, skipped []string, err error) {
	m, err := yamlMap(config)
	if err != nil {
		return nil, nil, fmt.Errorf("Alertmanager's configuration: %w", err)
	}
	root, ok := m["route"].(map[string]any)
	if !ok {
		return nil, nil, errors.New("Alertmanager's configuration has no route")
	}
	n = &Notifications{alertmanager: true, defs: map[string]*shown{}, receivers: map[string][]string{}}
	total := 0
	for i, f := range files {
		if i >= MaxTemplateFiles {
			skipped = append(skipped, fmt.Sprintf("more than %d template files: those after are passed over", MaxTemplateFiles))
			break
		}
		if len(f.Text) > MaxTemplateFile {
			skipped = append(skipped, bound(fmt.Sprintf("%s: longer than %d KB, passed over", f.Name, MaxTemplateFile>>10)))
			continue
		}
		if total += len(f.Text); total > MaxTemplateBytes {
			skipped = append(skipped, fmt.Sprintf("template files of more than %d MB in all: those after are passed over", MaxTemplateBytes>>20))
			break
		}
		defs, err := parseNotify(string(f.Text), MaxTemplateFile)
		if err != nil {
			why := bound(fmt.Sprintf("%s: %s", f.Name, notifyErr(err)))
			skipped = append(skipped, why+": it doesn't parse, and Alertmanager won't load the configuration")
			for _, name := range defined(f.Text) {
				n.defs[name] = &shown{err: why}
			}
			continue
		}
		maps.Copy(n.defs, defs)
	}
	recvs, _ := m["receivers"].([]any)
	fields := 0
	for i, x := range recvs {
		if i >= maxReceivers {
			skipped = append(skipped, fmt.Sprintf("more than %d receivers: those after aren't read", maxReceivers))
			break
		}
		rm, _ := x.(map[string]any)
		name, _ := rm["name"].(string)
		if name == "" || len(name) > 1024 {
			continue
		}
		for _, k := range slices.Sorted(maps.Keys(rm)) {
			kind, ok := strings.CutSuffix(k, "_configs")
			if !ok {
				continue
			}
			list, _ := rm[k].([]any)
			for j, it := range list {
				im, _ := it.(map[string]any)
				if im == nil {
					continue
				}
				for _, f := range amFields(im, fmt.Sprintf("%s[%d]", k, j), 0) {
					if fields++; fields > maxFields {
						break
					}
					def := name + "/" + f.path
					s, err := parseField(f.text)
					if err != nil {
						s = &shown{err: bound(notifyErr(err))}
					}
					if s == nil {
						continue
					}
					n.defs[def] = s
					n.receivers[name] = append(n.receivers[name], def)
				}
				n.receivers[name] = append(n.receivers[name], n.overrides(kind, im)...)
			}
		}
	}
	if fields > maxFields {
		skipped = append(skipped, fmt.Sprintf("receivers' fields of more than %d templates: those after aren't read", maxFields))
	}
	count := 0
	n.route = amRoute(root, &count, 0)
	n.number()
	return n, skipped, nil
}

// yamlMap reads a YAML document that is a map, as rule files are read:
// bounded in what its aliases and values may make.
func yamlMap(b []byte) (map[string]any, error) {
	v, err := rules.UnmarshalYAML(b)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("not a map")
	}
	return m, nil
}

// field is a receiver's field that is a template: where, and its text.
type field struct{ path, text string }

// amFields are a receiver's integration's fields that are templates: each
// text with {{ in it, by its path (slack_configs[0].title), in a fixed
// order. Nothing whose name says it is a secret or a connection's (a
// password, a token, a key, a URL, a file, http_config, tls_config) is
// read: it isn't a template, and what it holds isn't Lapilli's to see.
func amFields(v any, path string, depth int) []field {
	if depth > maxDepth {
		return nil
	}
	switch v := v.(type) {
	case string:
		if strings.Contains(v, "{{") {
			return []field{{path, v}}
		}
	case map[string]any:
		var out []field
		for _, k := range slices.Sorted(maps.Keys(v)) {
			if secretish(k) {
				continue
			}
			out = append(out, amFields(v[k], path+"."+k, depth+1)...)
		}
		return out
	case []any:
		var out []field
		for i, x := range v {
			out = append(out, amFields(x, fmt.Sprintf("%s[%d]", path, i), depth+1)...)
		}
		return out
	}
	return nil
}

// secretish is a field's name that says it holds a secret, or where a
// request goes, or how: never a template's to read.
func secretish(k string) bool {
	k = strings.ToLower(k)
	if k == "http_config" || k == "tls_config" || k == "send_resolved" || strings.HasSuffix(k, "_file") {
		return true
	}
	for _, w := range []string{"password", "secret", "token", "key", "url", "credential", "authorization", "smarthost", "auth_"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

// parseField parses a receiver's field as Alertmanager does when it sends:
// as a template of its own, which may use those the files define. nil when
// it can't be told, as with too many functions not known, or a field over
// maxTemplate, which isn't read: Alertmanager would send it.
func parseField(text string) (s *shown, err error) {
	if len(text) > maxTemplate {
		return nil, nil
	}
	defer func() {
		if recover() != nil {
			s, err = nil, nil
		}
	}()
	stubs := template.FuncMap{}
	for _, f := range append(slices.Clone(funcs), notifyFuncs...) {
		stubs[f] = stub
	}
	for range maxStubs {
		t, err := template.New("").Funcs(stubs).Parse(text)
		if err == nil {
			s = &shown{}
			notifyWalk(t.Tree.Root, nil, s)
			return s, nil
		}
		m := undefinedFunc.FindStringSubmatch(err.Error())
		if m == nil {
			return nil, err
		}
		stubs[m[1]] = stub
	}
	return nil, nil // too many unknown functions to tell
}

var notifyAt = regexp.MustCompile(`^template: [^:]*:(\d+)(?::\d+)?: `)

// notifyErr is what a parse error says, its line first, without the
// template's name.
func notifyErr(err error) string {
	msg := err.Error()
	if m := notifyAt.FindStringSubmatch(msg); m != nil {
		msg = "line " + m[1] + ": " + msg[len(m[0]):]
	}
	return msg
}

// defineName is where a template file defines a template, by its name.
var defineName = regexp.MustCompile("\\{\\{-?\\s*define\\s+(\"(?:[^\"\\\\\\n]|\\\\.)*\"|`[^`]*`)")

// maxDefined is how many names a file that doesn't parse is read for.
const maxDefined = 1000

// defined are the names a template file that doesn't parse defines, as
// far as its text says: what Alertmanager can't load.
func defined(text []byte) []string {
	var out []string
	for _, m := range defineName.FindAllSubmatch(text, maxDefined) {
		q := string(m[1])
		name := strings.Trim(q, "`")
		if q[0] == '"' {
			var err error
			if name, err = strconv.Unquote(q); err != nil {
				continue
			}
		}
		out = append(out, name)
	}
	return out
}

// overrides are the templates a file defines in place of Alertmanager's
// own default for a field an integration doesn't set: slack.default.title
// for a Slack integration with no title, email.default.subject for an
// email with no Subject header. Alertmanager's defaults themselves aren't
// followed: they show every label and annotation there is, and fault
// nothing.
func (n *Notifications) overrides(kind string, set map[string]any) []string {
	has := map[string]bool{}
	for k := range set {
		has[squash(k)] = true
	}
	if h, ok := set["headers"].(map[string]any); ok {
		for k := range h {
			if strings.EqualFold(k, "subject") {
				has["subject"] = true
			}
		}
	}
	var out []string
	prefix := kind + ".default."
	for _, name := range slices.Sorted(maps.Keys(n.defs)) {
		if f, ok := strings.CutPrefix(name, prefix); ok && f != "" && !has[squash(f)] {
			out = append(out, name)
		}
	}
	return out
}

// squash is a field's name as a default template's names it: title_link
// and titlelink, client_url and clientURL alike.
func squash(s string) string { return strings.ToLower(strings.ReplaceAll(s, "_", "")) }

// amRoute reads a route of Alertmanager's tree: its receiver, its
// matchers (matchers, and the older match and match_re), continue, and the
// routes under it, as many as are read.
func amRoute(m map[string]any, count *int, depth int) *route {
	*count++
	r := &route{}
	r.receiver, _ = m["receiver"].(string)
	r.cont, _ = m["continue"].(bool)
	add := func(name, op, value string) {
		x := routeMatcher{name: name, op: op, value: value}
		if op == "=~" || op == "!~" {
			re, err := regexp.Compile("^(?:" + value + ")$")
			if err != nil {
				x.op = "?" // unreadable: may or may not match
			}
			x.re = re
		}
		r.matchers = append(r.matchers, x)
	}
	if mm, ok := m["match"].(map[string]any); ok {
		for _, k := range slices.Sorted(maps.Keys(mm)) {
			v, _ := mm[k].(string)
			add(k, "=", v)
		}
	}
	if mm, ok := m["match_re"].(map[string]any); ok {
		for _, k := range slices.Sorted(maps.Keys(mm)) {
			v, _ := mm[k].(string)
			add(k, "=~", v)
		}
	}
	if l, ok := m["matchers"].([]any); ok {
		for _, x := range l {
			line, _ := x.(string)
			ms, ok := amMatchers(line)
			if !ok {
				r.matchers = append(r.matchers, routeMatcher{op: "?"})
				continue
			}
			for _, x := range ms {
				add(x[0], x[1], x[2])
			}
		}
	}
	routes, _ := m["routes"].([]any)
	for _, c := range routes {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if *count >= maxRoutes || depth >= maxDepth {
			r.routes = append(r.routes, &route{matchers: []routeMatcher{{op: "?"}}, cut: true})
			break
		}
		r.routes = append(r.routes, amRoute(cm, count, depth+1))
	}
	return r
}

// amMatcher is one of Alertmanager's matchers as text: a name, quoted or
// not, an operator and a value, quoted or not.
var amMatcher = regexp.MustCompile(`^\s*("(?:[^"\\]|\\.)*"|[^\s"=!~,{}]+)\s*(=~|!~|!=|=)\s*(.*?)\s*$`)

// amMatchers reads a line of a route's matchers, as Alertmanager does: one
// matcher or more, split by commas, in braces or not
// (severity="critical", {team=~"shop|pay", env!="dev"}); each as name,
// operator and value. ok is false for a line that can't be read so.
func amMatchers(line string) (out [][3]string, ok bool) {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") {
		line = line[1 : len(line)-1]
	}
	for _, part := range splitMatchers(line) {
		if strings.TrimSpace(part) == "" {
			continue
		}
		g := amMatcher.FindStringSubmatch(part)
		if g == nil {
			return nil, false
		}
		name, value := g[1], g[3]
		if strings.HasPrefix(name, `"`) {
			var err error
			if name, err = strconv.Unquote(name); err != nil {
				return nil, false
			}
		}
		if strings.HasPrefix(value, `"`) {
			v, err := strconv.Unquote(value)
			if err != nil {
				return nil, false
			}
			value = v
		}
		out = append(out, [3]string{name, g[2], value})
	}
	return out, len(out) > 0
}

// splitMatchers splits matchers at the commas outside a quoted text.
func splitMatchers(s string) []string {
	var out []string
	start, quoted := 0, false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\' && quoted:
			i++
		case c == '"':
			quoted = !quoted
		case c == ',' && !quoted:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
