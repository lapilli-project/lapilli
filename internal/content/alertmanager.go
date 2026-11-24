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

	"go.yaml.in/yaml/v3"

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
	if _, ok := m["route"].(map[string]any); !ok {
		return nil, nil, errors.New("Alertmanager's configuration has no route")
	}
	// The route tree again, as written: Alertmanager reads it with
	// yaml.v2, a value as the text it is written in (1.10, 0x1F), continue
	// as YAML 1.1 reads a boolean (yes, on). It parsed above, within the
	// bounds of a rule file.
	var doc yaml.Node
	if err := yaml.Unmarshal(config, &doc); err != nil || len(doc.Content) != 1 {
		return nil, nil, fmt.Errorf("Alertmanager's configuration: %v", err)
	}
	root := yamlField(doc.Content[0], "route", 0)
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
		n.read.Files++
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
	n.read.Templates = len(n.defs)
	recvs, _ := m["receivers"].([]any)
	fields := 0
	// The defaults the files define in Alertmanager's place, by kind,
	// worked out once; a receiver's templates, each once.
	defaults := map[string][]string{}
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
		n.read.Receivers++
		uses := map[string]bool{}
		use := func(def string) bool {
			if uses[def] {
				return true
			}
			if fields++; fields > maxFields {
				return false
			}
			uses[def] = true
			n.receivers[name] = append(n.receivers[name], def)
			return true
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
					if fields >= maxFields {
						fields++
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
					use(def)
				}
				ds, ok := defaults[kind]
				if !ok {
					ds = n.defaults(kind)
					defaults[kind] = ds
					if len(ds) > maxDefaults {
						ds = ds[:maxDefaults]
						defaults[kind] = ds
						skipped = append(skipped, bound(fmt.Sprintf("templates define more than %d of %s's defaults: those after aren't followed", maxDefaults, kind)))
					}
				}
				for _, d := range overrides(ds, kind, im) {
					if !use(d) {
						break
					}
				}
			}
		}
	}
	if fields > maxFields {
		skipped = append(skipped, fmt.Sprintf("receivers' fields of more than %d templates: those after aren't read", maxFields))
	}
	count := 0
	matchers := 0
	n.route = amRoute(root, &count, &matchers, 0)
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

// maxDefaults is how many of a kind's defaults the files may define in
// Alertmanager's place, and are followed.
const maxDefaults = 200

// defaults are the names the files define in place of Alertmanager's own
// defaults for an integration of a kind (slack.default.title), sorted.
func (n *Notifications) defaults(kind string) []string {
	prefix := kind + ".default."
	var out []string
	for name := range n.defs {
		if f, ok := strings.CutPrefix(name, prefix); ok && f != "" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// overrides are the defaults (n.defaults) an integration is sent with:
// those for a field it doesn't set, slack.default.title for a Slack
// integration with no title, email.default.subject for an email with no
// Subject header. Alertmanager's defaults themselves aren't followed: they
// show every label and annotation there is, and fault nothing.
func overrides(defaults []string, kind string, set map[string]any) []string {
	if len(defaults) == 0 {
		return nil
	}
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
	for _, name := range defaults {
		if !has[squash(strings.TrimPrefix(name, kind+".default."))] {
			out = append(out, name)
		}
	}
	return out
}

// squash is a field's name as a default template's names it: title_link
// and titlelink, client_url and clientURL alike.
func squash(s string) string { return strings.ToLower(strings.ReplaceAll(s, "_", "")) }

// maxMatchers is how many matchers the route tree's routes may have, in
// all: past them, a route matches maybe.
const maxMatchers = 20000

// yamlNode is what n stands for: an alias's anchor, followed a few times.
func yamlNode(n *yaml.Node) *yaml.Node {
	for i := 0; n != nil && n.Kind == yaml.AliasNode && i < 8; i++ {
		n = n.Alias
	}
	return n
}

// yamlField is a mapping's value for a key: its own, or one its merge
// (<<) brings in, as yaml.v2 reads it; nil for none.
func yamlField(m *yaml.Node, key string, depth int) *yaml.Node {
	m = yamlNode(m)
	if m == nil || m.Kind != yaml.MappingNode || depth > 8 {
		return nil
	}
	var merge *yaml.Node
	for i := 0; i+1 < len(m.Content); i += 2 {
		switch m.Content[i].Value {
		case key:
			return yamlNode(m.Content[i+1])
		case "<<":
			merge = yamlNode(m.Content[i+1])
		}
	}
	switch {
	case merge == nil:
	case merge.Kind == yaml.MappingNode:
		return yamlField(merge, key, depth+1)
	case merge.Kind == yaml.SequenceNode:
		for _, x := range merge.Content {
			if v := yamlField(x, key, depth+1); v != nil {
				return v
			}
		}
	}
	return nil
}

// yamlText is a scalar as it is written, as yaml.v2 reads one into a
// string; "" for anything else.
func yamlText(n *yaml.Node) string {
	if n = yamlNode(n); n != nil && n.Kind == yaml.ScalarNode {
		return n.Value
	}
	return ""
}

// yamlTrue is a boolean as YAML 1.1, and so yaml.v2, reads one: yes and on
// are true.
func yamlTrue(n *yaml.Node) bool {
	switch yamlText(n) {
	case "true", "True", "TRUE", "yes", "Yes", "YES", "y", "Y", "on", "On", "ON":
		return true
	}
	return false
}

// amRoute reads a route of Alertmanager's tree: its receiver, its
// matchers (matchers, and the older match and match_re), continue, and the
// routes under it, as many as are read.
func amRoute(m *yaml.Node, count, matchers *int, depth int) *route {
	*count++
	r := &route{}
	r.receiver = yamlText(yamlField(m, "receiver", 0))
	r.cont = yamlTrue(yamlField(m, "continue", 0))
	add := func(name, op, value string) {
		if *matchers++; *matchers > maxMatchers {
			r.matchers = append(r.matchers, routeMatcher{op: "?"})
			return
		}
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
	pairs := func(key, op string) {
		mm := yamlField(m, key, 0)
		if mm == nil || mm.Kind != yaml.MappingNode {
			return
		}
		for i := 0; i+1 < len(mm.Content); i += 2 {
			add(yamlText(mm.Content[i]), op, yamlText(mm.Content[i+1]))
		}
	}
	pairs("match", "=")
	pairs("match_re", "=~")
	if l := yamlField(m, "matchers", 0); l != nil && l.Kind == yaml.SequenceNode {
		for _, x := range l.Content {
			ms, ok := amMatchers(yamlText(x))
			if !ok {
				r.matchers = append(r.matchers, routeMatcher{op: "?"})
				continue
			}
			for _, x := range ms {
				add(x[0], x[1], x[2])
			}
		}
	}
	if routes := yamlField(m, "routes", 0); routes != nil && routes.Kind == yaml.SequenceNode {
		for _, c := range routes.Content {
			if c = yamlNode(c); c == nil || c.Kind != yaml.MappingNode {
				continue
			}
			if *count >= maxRoutes || depth >= maxDepth {
				r.routes = append(r.routes, &route{matchers: []routeMatcher{{op: "?"}}, cut: true})
				break
			}
			r.routes = append(r.routes, amRoute(c, count, matchers, depth+1))
		}
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
			name = amUnquote(name)
		}
		if strings.HasPrefix(value, `"`) {
			if !quoted(value) {
				return nil, false
			}
			value = amUnquote(value)
		}
		out = append(out, [3]string{name, g[2], value})
	}
	return out, len(out) > 0
}

// quoted reports whether s is one quoted text: its first quote closed by
// its last byte, and none between but those escaped.
func quoted(s string) bool {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i == len(s)-1
		}
	}
	return false
}

// amUnquote is a matcher's quoted name or value as Alertmanager reads it:
// \", \\ and \n are what they stand for; any other backslash is kept as
// written, as in a regular expression's \. or \d.
func amUnquote(q string) string {
	in := q[1 : len(q)-1]
	var b strings.Builder
	for i := 0; i < len(in); i++ {
		if in[i] != '\\' || i+1 == len(in) {
			b.WriteByte(in[i])
			continue
		}
		i++
		switch in[i] {
		case 'n':
			b.WriteByte('\n')
		case '"', '\\':
			b.WriteByte(in[i])
		default:
			b.WriteByte('\\')
			b.WriteByte(in[i])
		}
	}
	return b.String()
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
