package content

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"text/template/parse"

	"lapilli.dev/internal/rules"
)

// Grafana's notification templates (docs/design/serveops.md): a Grafana
// rule's notification says what its contact point's templates say, which
// can name labels and annotations the rule's alerts don't have. Read from
// Grafana's provisioning API: the templates, the contact points that use
// them, and the notification policies that route a rule to a contact
// point. Each template is parsed, never executed.

// Notifications are what Grafana's notification templates show, and which
// contact point uses which, and how rules are routed to them.
type Notifications struct {
	// defs are the templates defined, by name: what each shows, and the
	// templates it includes.
	defs map[string]*shown
	// receivers are the templates each contact point's fields include,
	// and what its fields show of their own, by the contact point's name.
	receivers map[string][]string
	route     *route
}

// shown is what a template shows: the labels and annotations it names,
// each guarded where it renders nothing when missing, and the templates it
// includes.
type shown struct {
	labels, annotations []label
	includes            []string
}

// route is a notification policy: its contact point (its parent's when
// empty), its matchers, whether matching goes on to its siblings, and its
// own. cut stands for the policies past the most read, which may route a
// rule anywhere: a rule that may reach one isn't routed at all.
type route struct {
	receiver string
	matchers []routeMatcher
	cont     bool
	routes   []*route
	cut      bool
}

type routeMatcher struct {
	name, op, value string
	re              *regexp.Regexp
}

// maxTemplate is how much of a template is read; maxRoutes, how many
// policies; maxOutcomes, how many ways a rule's routing can go, and
// maxSteps, how much work routing one rule may take, before it is given up
// on.
const (
	maxTemplate = 64 << 10
	maxRoutes   = 2000
	maxOutcomes = 64
	maxDepth    = 32
	maxSteps    = 1000000
)

// ReadNotifications reads Grafana's answers: GET
// /api/v1/provisioning/templates, /contact-points and /policies. A template
// that doesn't parse is passed over, and said in skipped; Grafana refuses
// one when it is saved.
func ReadNotifications(templates, contactPoints, policies []byte) (n *Notifications, skipped []string, err error) {
	var ts []struct {
		Name     string `json:"name"`
		Template string `json:"template"`
	}
	if err := json.Unmarshal(templates, &ts); err != nil {
		return nil, nil, fmt.Errorf("the templates: %w", err)
	}
	var cps []struct {
		Name     string         `json:"name"`
		Settings map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(contactPoints, &cps); err != nil {
		return nil, nil, fmt.Errorf("the contact points: %w", err)
	}
	var tree policyJSON
	if err := json.Unmarshal(policies, &tree); err != nil {
		return nil, nil, fmt.Errorf("the notification policies: %w", err)
	}
	n = &Notifications{defs: map[string]*shown{}, receivers: map[string][]string{}}
	for _, t := range ts {
		defs, err := notifyTemplates(t.Template)
		if err != nil {
			skipped = append(skipped, bound(t.Name+": "+err.Error()))
			continue
		}
		maps.Copy(n.defs, defs)
	}
	for _, cp := range cps {
		// A field's text is a template of its own, which may include named
		// ones: {{ template "shop.message" . }}.
		for _, text := range strings_(cp.Settings, 0) {
			if !strings.Contains(text, "{{") {
				continue
			}
			name := fmt.Sprintf("%s/%d", cp.Name, len(n.receivers[cp.Name]))
			defs, err := notifyTemplates(`{{ define "` + name + `" }}` + text + `{{ end }}`)
			if err != nil {
				continue // a field that isn't a template: Grafana keeps it as text
			}
			n.defs[name] = defs[name]
			n.receivers[cp.Name] = append(n.receivers[cp.Name], name)
		}
	}
	count := 0
	n.route = tree.read(&count, 0)
	return n, skipped, nil
}

// strings_ are the texts in a contact point's settings, every one, in a
// fixed order.
func strings_(v any, depth int) []string {
	if depth > maxDepth {
		return nil
	}
	switch v := v.(type) {
	case string:
		return []string{v}
	case map[string]any:
		var out []string
		for _, k := range slices.Sorted(maps.Keys(v)) {
			out = append(out, strings_(v[k], depth+1)...)
		}
		return out
	case []any:
		var out []string
		for _, x := range v {
			out = append(out, strings_(x, depth+1)...)
		}
		return out
	}
	return nil
}

type policyJSON struct {
	Receiver       string     `json:"receiver"`
	ObjectMatchers [][]string `json:"object_matchers"`
	Matchers       []string   `json:"matchers"`
	// Match and MatchRe are Alertmanager's older matchers, which Grafana
	// keeps and gives back: = and =~.
	Match    map[string]string `json:"match"`
	MatchRe  map[string]string `json:"match_re"`
	Continue bool              `json:"continue"`
	Routes   []policyJSON      `json:"routes"`
}

var matcherText = regexp.MustCompile(`^\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*(=~|!~|!=|=)\s*(.*?)\s*$`)

func (p policyJSON) read(count *int, depth int) *route {
	*count++
	r := &route{receiver: p.Receiver, cont: p.Continue}
	add := func(name, op, value string) {
		m := routeMatcher{name: name, op: op, value: value}
		if op == "=~" || op == "!~" {
			re, err := regexp.Compile("^(?:" + value + ")$")
			if err != nil {
				m.op = "?" // unreadable: may or may not match
			}
			m.re = re
		}
		r.matchers = append(r.matchers, m)
	}
	for _, m := range p.ObjectMatchers {
		if len(m) == 3 {
			add(m[0], m[1], m[2])
		}
	}
	for _, k := range slices.Sorted(maps.Keys(p.Match)) {
		add(k, "=", p.Match[k])
	}
	for _, k := range slices.Sorted(maps.Keys(p.MatchRe)) {
		add(k, "=~", p.MatchRe[k])
	}
	for _, m := range p.Matchers {
		if g := matcherText.FindStringSubmatch(m); g != nil {
			v := g[3]
			if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
				v = strings.ReplaceAll(v[1:len(v)-1], `\"`, `"`)
			}
			add(g[1], g[2], v)
		} else {
			r.matchers = append(r.matchers, routeMatcher{op: "?"})
		}
	}
	for _, c := range p.Routes {
		if *count >= maxRoutes || depth >= maxDepth {
			r.routes = append(r.routes, &route{matchers: []routeMatcher{{op: "?"}}, cut: true})
			break
		}
		r.routes = append(r.routes, c.read(count, depth+1))
	}
	return r
}

type tri int

const (
	no tri = iota
	maybe
	yes
)

// match is whether a policy's matchers match a rule's alerts, by the
// labels it is known to have: a label it doesn't set may come from its
// series, or not.
func (r *route) match(known map[string]string) tri {
	out := yes
	for _, m := range r.matchers {
		v, ok := known[m.name]
		if !ok || m.op == "?" {
			out = maybe
			continue
		}
		var holds bool
		switch m.op {
		case "=":
			holds = v == m.value
		case "!=":
			holds = v != m.value
		case "=~":
			holds = m.re.MatchString(v)
		case "!~":
			holds = !m.re.MatchString(v)
		}
		if !holds {
			return no
		}
	}
	return out
}

// outcomes are the sets of contact points a rule's alerts can be sent to,
// one a way the routing can go; ok is false when there are too many to
// tell, it would take more than steps allows, or a way reaches policies
// past those read.
func (r *route) outcomes(known map[string]string, parent string, depth int, steps *int) ([][]string, bool) {
	recv := r.receiver
	if recv == "" {
		recv = parent
	}
	if depth > maxDepth {
		return nil, false
	}
	// Each way the children can go: the contact points so far, as a
	// sorted set, and whether one matched for certain, so the policy
	// itself isn't used. Ways alike are kept once.
	type way struct {
		recvs   []string
		matched bool
		stopped bool
	}
	key := func(w way) string {
		return fmt.Sprintf("%t%t", w.matched, w.stopped) + strings.Join(w.recvs, "\x00")
	}
	ways := []way{{}}
	for _, c := range r.routes {
		m := c.match(known)
		if m == no || !slices.ContainsFunc(ways, func(w way) bool { return !w.stopped }) {
			continue
		}
		if c.cut {
			return nil, false
		}
		// What the child gives is the same whichever way led to it.
		sub, ok := c.outcomes(known, recv, depth+1, steps)
		if !ok {
			return nil, false
		}
		var next []way
		seen := map[string]bool{}
		keep := func(w way) {
			if k := key(w); !seen[k] {
				seen[k] = true
				next = append(next, w)
			}
		}
		for _, w := range ways {
			if *steps -= 1 + len(sub); *steps < 0 {
				return nil, false
			}
			if w.stopped {
				keep(w)
				continue
			}
			for _, s := range sub {
				keep(way{recvs: union(w.recvs, s), matched: true, stopped: !c.cont})
			}
			if m == maybe {
				keep(w) // it didn't match
			}
		}
		if len(next) > maxOutcomes {
			return nil, false
		}
		ways = next
	}
	var out [][]string
	for _, w := range ways {
		if !w.matched {
			w.recvs = union(w.recvs, []string{recv})
		}
		out = append(out, w.recvs)
	}
	return out, true
}

// union is two sorted sets of names as one.
func union(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	out = append(append(out, a...), b...)
	slices.Sort(out)
	return slices.Compact(out)
}

// A NotifyUse is a template a rule's notifications are certainly written
// with, and what it shows.
type NotifyUse struct {
	Template    string
	labels      []label
	annotations []label
}

// For are the templates a Grafana rule's notifications are written with,
// whichever way its routing goes: its own contact point
// (notification_settings), or those the policies can route it to, by its
// labels, alertname and grafana_folder. A template counts when every way
// reaches a contact point that uses it.
func (n *Notifications) For(r rules.Rule) []NotifyUse {
	if n == nil || r.Says.Template != rules.TemplateGrafana {
		return nil
	}
	var ways [][]string
	if r.Receiver != "" {
		ways = [][]string{{r.Receiver}}
	} else {
		if n.route == nil {
			return nil
		}
		known := map[string]string{"alertname": r.AlertName()}
		if r.Folder != "" {
			known["grafana_folder"] = r.Folder
		}
		for k, v := range r.Labels {
			// Grafana fills a label's template in from the series: what
			// it comes to isn't known, so policies on it go either way.
			if !strings.Contains(v, "{{") {
				known[k] = v
			}
		}
		steps := maxSteps
		var ok bool
		if ways, ok = n.route.outcomes(known, "", 0, &steps); !ok {
			return nil
		}
	}
	var common map[string]bool
	for _, w := range ways {
		used := map[string]bool{}
		for _, recv := range w {
			for _, t := range n.receivers[recv] {
				n.include(t, used, 0)
			}
		}
		if common == nil {
			common = used
			continue
		}
		maps.DeleteFunc(common, func(t string, _ bool) bool { return !used[t] })
	}
	var out []NotifyUse
	for _, t := range slices.Sorted(maps.Keys(common)) {
		if d := n.defs[t]; d != nil && (len(d.labels) > 0 || len(d.annotations) > 0) {
			out = append(out, NotifyUse{Template: t, labels: d.labels, annotations: d.annotations})
		}
	}
	return out
}

// include adds a template and those it includes, in turn.
func (n *Notifications) include(t string, used map[string]bool, depth int) {
	if used[t] || depth > maxDepth {
		return
	}
	d := n.defs[t]
	if d == nil {
		return // one of Grafana's own, such as default.message
	}
	used[t] = true
	for _, x := range d.includes {
		n.include(x, used, depth+1)
	}
}

// notifyTemplates parses a group of notification templates, never
// running them, and gives what each it defines shows.
func notifyTemplates(text string) (defs map[string]*shown, err error) {
	if len(text) > maxTemplate {
		return nil, fmt.Errorf("longer than %d KB", maxTemplate>>10)
	}
	defer func() {
		if recover() != nil {
			defs, err = nil, nil
		}
	}()
	stubs := template.FuncMap{}
	for _, f := range append(slices.Clone(funcs), notifyFuncs...) {
		stubs[f] = stub
	}
	var t *template.Template
	for range maxStubs {
		t, err = template.New("").Funcs(stubs).Parse(text)
		if err == nil {
			break
		}
		m := undefinedFunc.FindStringSubmatch(err.Error())
		if m == nil {
			return nil, err
		}
		stubs[m[1]] = stub
	}
	if err != nil {
		return nil, nil // too many unknown functions to tell
	}
	defs = map[string]*shown{}
	for _, tt := range t.Templates() {
		if tt.Name() == "" || tt.Tree == nil {
			continue
		}
		s := &shown{}
		notifyWalk(tt.Tree.Root, nil, s)
		defs[tt.Name()] = s
	}
	return defs, nil
}

// notifyFuncs are the functions Alertmanager's and Grafana's notification
// templates have, beyond those a rule's have.
var notifyFuncs = []string{"toJson", "stringSlice", "date", "tz", "since", "humanizeDuration", "safeHtml", "reReplaceAll",
	"trimSpace", "list", "append", "contains", "data", "printf", "html", "urlquery"}

// kv are the methods of a template's labels and annotations, which name no
// label: .CommonLabels.SortedPairs.
var kv = map[string]bool{"SortedPairs": true, "Names": true, "Values": true, "Remove": true}

// notifyWalk finds what a notification template names: .Labels.x,
// .CommonLabels.x, .GroupLabels.x and index .Labels "x"; .Annotations.y
// and .CommonAnnotations.y; and the templates it includes. Guarded, those
// named in an if's or a with's condition, or under one whose condition
// names them.
func notifyWalk(n parse.Node, guards map[string]bool, s *shown) {
	addLabel := func(x string) { s.labels = append(s.labels, label{x, guards["l:"+x]}) }
	addNote := func(x string) { s.annotations = append(s.annotations, label{x, guards["a:"+x]}) }
	chain := func(ident []string) {
		for i := 0; i+1 < len(ident); i++ {
			if kv[ident[i+1]] {
				continue
			}
			switch ident[i] {
			case "Labels", "CommonLabels", "GroupLabels":
				addLabel(ident[i+1])
			case "Annotations", "CommonAnnotations":
				addNote(ident[i+1])
			}
		}
	}
	switch n := n.(type) {
	case *parse.ListNode:
		if n == nil {
			return
		}
		for _, c := range n.Nodes {
			notifyWalk(c, guards, s)
		}
	case *parse.ActionNode:
		notifyWalk(n.Pipe, guards, s)
	case *parse.IfNode:
		notifyBranch(&n.BranchNode, guards, s)
	case *parse.WithNode:
		notifyBranch(&n.BranchNode, guards, s)
	case *parse.RangeNode:
		notifyWalk(n.Pipe, guards, s)
		notifyWalk(n.List, guards, s)
		notifyWalk(n.ElseList, guards, s)
	case *parse.TemplateNode:
		s.includes = append(s.includes, n.Name)
		notifyWalk(n.Pipe, guards, s)
	case *parse.PipeNode:
		if n == nil {
			return
		}
		// What or is given falls back on what follows it, or on what is
		// piped into it: missing, it shows that, not nothing.
		if slices.ContainsFunc(n.Cmds, isOr) {
			var in shown
			for _, c := range n.Cmds {
				notifyWalk(c, guards, &in)
			}
			for _, l := range in.labels {
				s.labels = append(s.labels, label{l.label, true})
			}
			for _, a := range in.annotations {
				s.annotations = append(s.annotations, label{a.label, true})
			}
			s.includes = append(s.includes, in.includes...)
			return
		}
		for _, c := range n.Cmds {
			notifyWalk(c, guards, s)
		}
	case *parse.CommandNode:
		if len(n.Args) == 3 {
			if id, ok := n.Args[0].(*parse.IdentifierNode); ok && id.Ident == "index" {
				if str, ok := n.Args[2].(*parse.StringNode); ok {
					switch last(n.Args[1]) {
					case "Labels", "CommonLabels", "GroupLabels":
						addLabel(str.Text)
					case "Annotations", "CommonAnnotations":
						addNote(str.Text)
					}
				}
			}
		}
		for _, a := range n.Args {
			notifyWalk(a, guards, s)
		}
	case *parse.FieldNode:
		chain(n.Ident)
	case *parse.VariableNode:
		chain(n.Ident)
	case *parse.ChainNode:
		notifyWalk(n.Node, guards, s)
		if f, ok := n.Node.(*parse.FieldNode); ok {
			chain(append(slices.Clone(f.Ident), n.Field...))
		}
	}
}

// isOr is whether a command calls or.
func isOr(c *parse.CommandNode) bool {
	if len(c.Args) == 0 {
		return false
	}
	id, ok := c.Args[0].(*parse.IdentifierNode)
	return ok && id.Ident == "or"
}

// promQL is a rule's PromQL, for what its expression can give: its own,
// or, for a Grafana rule, its one query's, which the threshold, the reduce
// or the math over it keeps the labels of. "" when there is none, or more
// than one.
func promQL(r rules.Rule) string {
	if r.Expr != "" || r.Graph == nil || len(r.Graph.Queries) != 1 {
		return r.Expr
	}
	for _, q := range r.Graph.Queries {
		return q
	}
	return ""
}

// last is the last name of a field or variable: Labels, of .Labels.
func last(n parse.Node) string {
	switch n := n.(type) {
	case *parse.FieldNode:
		return n.Ident[len(n.Ident)-1]
	case *parse.VariableNode:
		return n.Ident[len(n.Ident)-1]
	}
	return ""
}

// notifyBranch walks an if or a with: what its condition names is guarded
// there and in what it shows when the condition holds, not in its else.
func notifyBranch(b *parse.BranchNode, guards map[string]bool, s *shown) {
	var cond shown
	notifyWalk(b.Pipe, guards, &cond)
	in := guards
	if len(cond.labels)+len(cond.annotations) > 0 {
		in = maps.Clone(guards)
		if in == nil {
			in = map[string]bool{}
		}
		for _, l := range cond.labels {
			in["l:"+l.label] = true
			s.labels = append(s.labels, label{l.label, true})
		}
		for _, a := range cond.annotations {
			in["a:"+a.label] = true
			s.annotations = append(s.annotations, label{a.label, true})
		}
	}
	s.includes = append(s.includes, cond.includes...)
	notifyWalk(b.List, in, s)
	notifyWalk(b.ElseList, guards, s)
}

// annotated are the annotations a notification template can show that the
// checks know a rule's own of: what it says.
var annotated = map[string]func(rules.Says) string{
	"summary":     func(s rules.Says) string { return s.Summary },
	"description": func(s rules.Says) string { return s.Description },
	"runbook_url": func(s rules.Says) string { return s.Runbook },
}

// notifyChecks checks what the templates a Grafana rule's notifications
// are written with show: an annotation the rule hasn't, a label its alerts
// never had or its expression can't give. A label the rule sets, and those
// Grafana gives every alert, are never faulted.
func notifyChecks(r rules.Rule, o Options, seen map[string]bool, fired bool, add func(check, detail string)) {
	uses := o.Notify.For(r)
	if len(uses) == 0 {
		return
	}
	gives, givesOK := map[string]bool(nil), false
	if !fired {
		gives, givesOK = Gives(promQL(r))
	}
	for _, u := range uses {
		said := map[string]bool{}
		for _, a := range u.annotations {
			if get, ok := annotated[a.label]; ok && !a.guarded && !said[a.label] && strings.TrimSpace(get(r.Says)) == "" {
				said[a.label] = true
				add(NotifyNoText, fmt.Sprintf("%q: %s", u.Template, a.label))
			}
		}
		named := map[string]bool{}
		for _, l := range u.labels {
			x := l.label
			if l.guarded || named[x] || always[x] || x == "grafana_folder" || strings.HasPrefix(x, "__") || r.Labels[x] != "" {
				continue
			}
			named[x] = true
			switch {
			case fired && !seen[x]:
				add(NotifyLabelNeverSeen, fmt.Sprintf("%q: %s", u.Template, x))
			case !fired && givesOK && !gives[x]:
				add(NotifyLabelNotGiven, fmt.Sprintf("%q: %s", u.Template, x))
			}
		}
	}
}
