// Package content checks what an alert says when it fires, and how its
// rule is written (docs/design/content.md): a summary, a runbook, a
// template that parses and names labels the alert has, a severity, a
// condition no other rule has.
//
// What a rule says comes from whoever writes the rules, or a cloud's API.
// A template is parsed, never executed; a link is checked to be an http or
// https URL, never visited.
package content

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"text/template/parse"
	"time"
	"unicode/utf8"

	"lapilli.dev/internal/rules"
)

// The kinds of check: a fact says what the rule is; a measured check uses
// what was judged; a heuristic guesses, and is shown apart.
const (
	Fact      = "fact"
	Measured  = "measured"
	Heuristic = "heuristic"
)

// The checks, each of one kind (Class).
const (
	NoText          = "no_text"          // fact: no summary or description
	NoRunbook       = "no_runbook"       // fact: no runbook link
	RunbookNotURL   = "runbook_not_url"  // fact: a runbook that isn't an http or https URL
	TemplateError   = "template_error"   // fact: a template that doesn't parse
	NoSeverity      = "no_severity"      // fact
	UnknownSeverity = "unknown_severity" // fact: not in --severities
	Duplicate       = "duplicate"        // fact: the same condition as another rule
	LabelNeverSeen  = "label_never_seen" // measured: a label no firing had
	NoisyNoFor      = "noisy_no_for"     // measured: noisy, and no for
	LabelNotGiven   = "label_not_given"  // heuristic: a label the expression can't give
	RareSeverity    = "rare_severity"    // heuristic: a severity fewer than two rules use
	ShortRange      = "short_range"      // heuristic: a range shorter than two scrapes
	AbsentNoFor     = "absent_no_for"    // heuristic: absent() with no for
)

// Checks are every check, in the order they are shown: facts, then what is
// measured, then heuristics.
var Checks = []string{NoText, NoRunbook, RunbookNotURL, TemplateError, NoSeverity, UnknownSeverity, Duplicate,
	LabelNeverSeen, NoisyNoFor, LabelNotGiven, RareSeverity, ShortRange, AbsentNoFor}

// Class is the kind of check a check is.
func Class(check string) string {
	switch check {
	case LabelNeverSeen, NoisyNoFor:
		return Measured
	case LabelNotGiven, RareSeverity, ShortRange, AbsentNoFor:
		return Heuristic
	}
	return Fact
}

// A Finding is one problem with what a rule says, or how it is written.
type Finding struct {
	Check string `json:"check"`
	Class string `json:"class"`
	// Detail is what in particular: a label, a severity, another rule, a
	// range, a template's error. It comes from the rules: data, bounded.
	Detail string `json:"detail,omitempty"`
}

// Options are what the checks know besides the rules.
type Options struct {
	// Severities are the severities the team uses (--severities); others
	// are a problem of fact. Empty, a rare one is a guess.
	Severities []string
	// Scrape is the scrape interval the range check counts with; 0 turns
	// it off.
	Scrape time.Duration
	// Seen, when firings were judged, gives the label names a rule's
	// firings had and whether it fired at all; nil when nothing is known.
	Seen func(rule string) (names map[string]bool, fired bool)
	// Noisy, when firings were judged, says whether a rule was noisy.
	Noisy func(rule string) bool
}

// rareAmong is how many rules of a kind must have a severity before one
// fewer than two use is guessed to be a typo: in a small set, every value
// is rare.
const rareAmong = 10

// maxDetail is how much of a detail is kept.
const maxDetail = 200

// maxExpr is the longest expression the expression checks read; a longer
// one is left alone, so no rule costs the checks more than a bound.
const maxExpr = 64 << 10

// Check checks each rule, and gives its findings in the order of Checks,
// nil for a rule with none. A rule turned off or paused fires nothing, so
// what it says costs no one: it isn't checked.
func Check(rs []rules.Rule, o Options) [][]Finding {
	out := make([][]Finding, len(rs))
	sevs := severityCounts(rs)
	dups := duplicates(rs)
	for i, r := range rs {
		if off(r) {
			continue
		}
		var fs []Finding
		add := func(check, detail string) {
			fs = append(fs, Finding{Check: check, Class: Class(check), Detail: bound(detail)})
		}
		// No summary or description: a cloud's alarm is summed up by its
		// name, so there it is the description that counts.
		if r.Says.Description == "" && (r.Says.Summary == "" || r.Says.Template == "") {
			add(NoText, "")
		}
		switch ok, known := linkOK(r.Says.Runbook, r.Says.Template); {
		case r.Says.Runbook == "":
			add(NoRunbook, "")
		case known && !ok:
			add(RunbookNotURL, r.Says.Runbook)
		}
		var named []string
		for _, text := range []string{r.Says.Summary, r.Says.Description, r.Says.Runbook} {
			names, err := Template(r.Says.Template, text)
			if err != nil {
				add(TemplateError, err.Error())
				named = nil
				break
			}
			named = append(named, names...)
		}
		slices.Sort(named)
		named = slices.Compact(named)
		sev := severity(r)
		switch {
		case sev == "" && !r.Says.NoSeverity:
			add(NoSeverity, "")
		case sev != "" && len(o.Severities) > 0 && !known(o.Severities, r, sev):
			add(UnknownSeverity, sev)
		}
		for _, other := range dups[i] {
			add(Duplicate, other)
		}
		var seen map[string]bool
		fired := false
		if o.Seen != nil {
			seen, fired = o.Seen(r.Name)
		}
		if fired {
			for _, n := range named {
				if !seen[n] && r.Labels[n] == "" {
					add(LabelNeverSeen, n)
				}
			}
		}
		if o.Noisy != nil && r.For == 0 && r.Unsupported == "" && o.Noisy(r.Name) {
			add(NoisyNoFor, "")
		}
		if !fired {
			if gives, ok := Gives(r.Expr); ok {
				for _, n := range named {
					if !gives[n] && r.Labels[n] == "" && n != "alertname" {
						add(LabelNotGiven, n)
					}
				}
			}
		}
		if sev != "" && len(o.Severities) == 0 {
			if c := sevs[kindOf(r)]; c.total >= rareAmong && c.by[sev] < 2 {
				add(RareSeverity, sev)
			}
		}
		if o.Scrape > 0 {
			for _, u := range Ranges(r.Expr) {
				if u.Range < 2*o.Scrape {
					add(ShortRange, fmt.Sprintf("%s[%s] < 2 × %s", u.Func, u.Text, promDuration(o.Scrape)))
				}
			}
		}
		if r.For == 0 && Absent(r.Expr) {
			add(AbsentNoFor, "")
		}
		slices.SortStableFunc(fs, func(a, b Finding) int {
			return slices.Index(Checks, a.Check) - slices.Index(Checks, b.Check)
		})
		out[i] = fs
	}
	return out
}

// off is a rule turned off or paused.
func off(r rules.Rule) bool {
	return r.Unsupported == rules.Disabled || r.Unsupported == rules.Paused
}

// severity is a rule's severity: its label, or its source's own field.
func severity(r rules.Rule) string {
	if s := r.Labels["severity"]; s != "" {
		return s
	}
	return r.Says.Severity
}

// known is whether a severity is one of the team's. A label is matched
// exactly, as an alert manager's routes match it; a cloud's own field, in
// any case.
func known(list []string, r rules.Rule, sev string) bool {
	if slices.Contains(list, sev) {
		return true
	}
	if r.Labels["severity"] == "" || r.Labels["severity"] == r.Says.Severity {
		for _, s := range list {
			if strings.EqualFold(s, sev) {
				return true
			}
		}
	}
	return false
}

// kindOf is where a rule comes from, as its severities are written: a
// cloud's own words (Sev0, CRITICAL) aren't counted with a label's.
func kindOf(r rules.Rule) string { return r.Cloud }

type sevCount struct {
	total int
	by    map[string]int
}

func severityCounts(rs []rules.Rule) map[string]*sevCount {
	m := map[string]*sevCount{}
	for _, r := range rs {
		if off(r) {
			continue
		}
		s := severity(r)
		if s == "" {
			continue
		}
		c := m[kindOf(r)]
		if c == nil {
			c = &sevCount{by: map[string]int{}}
			m[kindOf(r)] = c
		}
		c.total++
		c.by[s]++
	}
	return m
}

// duplicates are, for each rule, the other rules with the same condition:
// the same expression (spaces aside), for and labels, under another name.
// Two rules of one name and condition are one alert to an alert manager.
func duplicates(rs []rules.Rule) map[int][]string {
	by := map[string][]int{}
	for i, r := range rs {
		if r.Expr == "" || off(r) {
			continue
		}
		k := fmt.Sprintf("%s\x00%d\x00%v", Squeeze(r.Expr), r.For, sortedLabels(r.Labels))
		by[k] = append(by[k], i)
	}
	out := map[int][]string{}
	for _, is := range by {
		if len(is) < 2 {
			continue
		}
		var names []string
		for _, i := range is {
			names = append(names, rs[i].Name)
		}
		slices.Sort(names)
		names = slices.Compact(names)
		for _, i := range is {
			for _, n := range names {
				if n != rs[i].Name && len(out[i]) < maxDuplicates {
					out[i] = append(out[i], n)
				}
			}
		}
	}
	return out
}

// maxDuplicates is how many other rules of the same condition a rule is
// told of.
const maxDuplicates = 3

func sortedLabels(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
}

// bound cuts a detail to maxDetail bytes, as valid UTF-8, and drops what
// isn't printable.
func bound(s string) string {
	s = strings.Map(func(r rune) rune {
		if Unprintable(r) {
			return ' '
		}
		return r
	}, s)
	if len(s) > maxDetail {
		s = s[:maxDetail]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
		s += "…"
	}
	return s
}

// Unprintable is a control character, or one that turns text around,
// which a detail from the rules is given none of.
func Unprintable(r rune) bool {
	return r < ' ' || r >= 0x7f && r < 0xa0 || r >= 0x200e && r <= 0x200f || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069
}

// linkOK is whether a runbook link is an http or https URL; known is false
// when a template decides where it starts, as {{ $externalURL }}/x does.
func linkOK(link, lang string) (ok, known bool) {
	link = strings.TrimSpace(link)
	if link == "" {
		return false, true
	}
	var act *regexp.Regexp
	switch lang {
	case rules.TemplatePrometheus, rules.TemplateGrafana:
		act = goAction
	case rules.TemplateGoogle:
		act = googleVar
	}
	if act != nil {
		if loc := act.FindStringIndex(link); loc != nil && loc[0] == 0 {
			return false, false
		}
		link = act.ReplaceAllString(link, "x")
	}
	u, err := url.Parse(link)
	if err != nil {
		return false, true
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != "", true
}

var (
	goAction  = regexp.MustCompile(`\{\{.*?\}\}`)
	googleVar = regexp.MustCompile(`\$\{[^}]*\}`)
)

// Template parses a text in its template language, never running it, and
// gives the labels it names ($labels.x, index $labels "x"). A function the
// language may have that isn't known here is taken as one: only what can't
// be parsed whatever the functions is an error.
func Template(lang, text string) ([]string, error) {
	switch lang {
	case rules.TemplatePrometheus, rules.TemplateGrafana:
		return goTemplate(lang, text)
	case rules.TemplateGoogle:
		return nil, googleTemplate(text)
	}
	return nil, nil
}

// The variables each language's templates begin with, as Prometheus and
// Grafana define them, so that $labels and the rest parse.
var preambles = map[string]string{
	rules.TemplatePrometheus: "{{$labels := .Labels}}{{$externalLabels := .ExternalLabels}}{{$externalURL := .ExternalURL}}{{$value := .Value}}",
	rules.TemplateGrafana:    "{{$labels := .Labels}}{{$values := .Values}}{{$value := .Value}}",
}

// funcs are the names of the functions Prometheus's and Grafana's templates
// have, stubbed: none is ever called.
var funcs = []string{"query", "first", "label", "value", "strvalue", "args", "reReplaceAll", "safeHtml", "safeUrl",
	"match", "title", "toUpper", "toLower", "graphLink", "tableLink", "sortByLabel", "humanize", "humanize1024",
	"humanizeDuration", "humanizePercentage", "humanizeTimestamp", "toTime", "toDuration", "pathPrefix",
	"externalURL", "parseDuration", "stripPort", "stripDomain", "now", "urlQueryEscape", "join", "trimSpace"}

var (
	undefinedFunc = regexp.MustCompile(`function "([^"]+)" not defined`)
	identifier    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// maxStubs bounds how many unknown functions a text may name.
const maxStubs = 16

func goTemplate(lang, text string) (names []string, err error) {
	if !strings.Contains(text, "{{") {
		return nil, nil
	}
	// What can't be parsed is the text's fault, and an error; anything
	// else that goes wrong is this check's, and says nothing of the text.
	defer func() {
		if recover() != nil {
			names, err = nil, nil
		}
	}()
	stubs := template.FuncMap{}
	for _, f := range funcs {
		stubs[f] = stub
	}
	for range maxStubs {
		t, err := template.New("t").Funcs(stubs).Parse(preambles[lang] + text)
		if err == nil {
			var names []string
			for _, tt := range t.Templates() {
				// In a template defined in the text, . is whatever it is
				// given.
				if tt.Tree != nil {
					walk(tt.Tree.Root, tt.Name() != "t", &names)
				}
			}
			return names, nil
		}
		m := undefinedFunc.FindStringSubmatch(err.Error())
		if m == nil {
			return nil, templateErr(err)
		}
		if !identifier.MatchString(m[1]) {
			return nil, nil // not a name a function can have: can't tell
		}
		stubs[m[1]] = stub
	}
	return nil, nil // too many unknown functions to tell: not taken as wrong
}

func stub(...any) any { return nil }

var errAt = regexp.MustCompile(`^template: t:(\d+)(?::\d+)?: `)

// templateErr is what a parse error says, without the name given here.
func templateErr(err error) error {
	msg := err.Error()
	if m := errAt.FindStringSubmatch(msg); m != nil {
		msg = "line " + m[1] + ": " + msg[len(m[0]):]
	}
	return fmt.Errorf("%s", msg)
}

// walk finds the labels a template names: $labels.x, index $labels "x",
// and, where . is still the alert, .Labels.x.
func walk(n parse.Node, moved bool, out *[]string) {
	switch n := n.(type) {
	case *parse.ListNode:
		if n == nil {
			return
		}
		for _, c := range n.Nodes {
			walk(c, moved, out)
		}
	case *parse.ActionNode:
		walk(n.Pipe, moved, out)
	case *parse.IfNode:
		walk(n.Pipe, moved, out)
		walk(n.List, moved, out)
		walk(n.ElseList, moved, out)
	case *parse.RangeNode:
		walk(n.Pipe, moved, out)
		walk(n.List, true, out)
		walk(n.ElseList, moved, out)
	case *parse.WithNode:
		walk(n.Pipe, moved, out)
		walk(n.List, true, out)
		walk(n.ElseList, moved, out)
	case *parse.TemplateNode:
		walk(n.Pipe, moved, out)
	case *parse.PipeNode:
		if n == nil {
			return
		}
		for _, c := range n.Cmds {
			walk(c, moved, out)
		}
	case *parse.CommandNode:
		if len(n.Args) == 3 {
			if id, ok := n.Args[0].(*parse.IdentifierNode); ok && id.Ident == "index" && isLabels(n.Args[1], moved) {
				if s, ok := n.Args[2].(*parse.StringNode); ok {
					*out = append(*out, s.Text)
				}
			}
		}
		for _, a := range n.Args {
			walk(a, moved, out)
		}
	case *parse.VariableNode:
		if len(n.Ident) > 1 && n.Ident[0] == "$labels" {
			*out = append(*out, n.Ident[1])
		}
	case *parse.FieldNode:
		if !moved && len(n.Ident) > 1 && n.Ident[0] == "Labels" {
			*out = append(*out, n.Ident[1])
		}
	case *parse.ChainNode:
		walk(n.Node, moved, out)
	}
}

func isLabels(n parse.Node, moved bool) bool {
	switch n := n.(type) {
	case *parse.VariableNode:
		return len(n.Ident) == 1 && n.Ident[0] == "$labels"
	case *parse.FieldNode:
		return !moved && len(n.Ident) == 1 && n.Ident[0] == "Labels"
	}
	return false
}

// googleTemplate checks Cloud Monitoring's ${...} variables are closed;
// $$ is a dollar sign.
func googleTemplate(text string) error {
	for i := 0; i < len(text); i++ {
		if text[i] != '$' || i+1 >= len(text) {
			continue
		}
		switch text[i+1] {
		case '$':
			i++
		case '{':
			end := strings.IndexByte(text[i:], '}')
			if end < 0 {
				return fmt.Errorf("a ${ with no }")
			}
			if strings.TrimSpace(text[i+2:i+end]) == "" {
				return fmt.Errorf("an empty ${}")
			}
			i += end
		}
	}
	return nil
}

// Link is a rule's runbook when it is a link a page can show to follow:
// an http or https URL, with nothing in it a template fills in. It is
// never visited here.
func Link(s rules.Says) string {
	if goAction.MatchString(s.Runbook) || googleVar.MatchString(s.Runbook) || strings.ContainsAny(s.Runbook, " \t\r\n") {
		return ""
	}
	if ok, known := linkOK(s.Runbook, ""); ok && known {
		return s.Runbook
	}
	return ""
}
