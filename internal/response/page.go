// Package response reads what people did when a rule fired
// (docs/design/responses.md): the pages a paging tool sent and what was done
// with them, the changes made to an alert's target (rollouts, rollbacks,
// scales and deploys), and from both, each firing's response and whether it
// was useful.
//
// It is a library: it reads files and a Prometheus-compatible API, and
// judges a firing handed to it. Wiring it into lapilli verdict and lapilli
// serve, and the ledger, are left to them.
package response

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/incident"
)

// Who resolved a page (Page.ResolvedBy).
const (
	ByAlert  = "alert"  // its alert manager resolved it
	ByPerson = "person" // someone did
)

// The bounds of what is read: a file or a write holds MaxPages pages at
// most, MaxFile bytes, and a page's id is MaxID bytes at most.
const (
	MaxPages = 100000
	MaxFile  = 64 << 20
	MaxID    = 256
)

// Page is what the paging tool says of one page: the alerts it was sent
// for, when, and what a person or the alert manager did with it.
type Page struct {
	// ID is the tool's id of the page: letters, digits and -_.: , 256
	// bytes at most.
	ID string `json:"id"`
	// Alerts are the alerts it was sent for, as an incident's linked alerts
	// are written and held to the same bounds.
	Alerts []incident.Alert `json:"alerts"`
	// Sent is when it was sent; Acknowledged when a person acknowledged it,
	// Resolved when it was resolved, each zero when it hasn't happened.
	Sent         time.Time `json:"sent"`
	Acknowledged time.Time `json:"acknowledged,omitzero"`
	Resolved     time.Time `json:"resolved,omitzero"`
	// ResolvedBy is ByAlert or ByPerson, set with Resolved.
	ResolvedBy string `json:"resolved_by,omitempty"`
	// Source is who sent the page to the ledger: FileSource for the
	// --responses file, and for a page read without a source (lapilli
	// verdict), or the name a PUT's path gave. Never read from a page.
	Source string `json:"-"`
}

// FileSource is the source of the pages --responses reads. When a firing has
// a page from it and one from a source a token writes, the file's is its
// page: what the operator keeps in a file outranks what a job writes over
// the API.
const FileSource = "file"

// MaxFindScan is how many pages sharing a rule's name, from the firing's
// start on, Index.Find looks at for one firing at most (per name it may carry,
// see ruleNames). A rule whose alerts were paged more than that in a
// firing's window, none of them for this firing, finds the page among the
// first of them or none.
const MaxFindScan = 1000

func (p *Page) fromFile() bool { return p.Source == "" || p.Source == FileSource }

// Lists reports whether the page was sent for a firing of rule with labels:
// one of its alerts names the rule and agrees with the labels, as an
// incident's linked alerts are held to a firing (incident.Alert.Links).
func (p *Page) Lists(rule string, labels map[string]string) bool {
	return incident.Incident{Alerts: p.Alerts}.Links(rule, labels)
}

// Open reports whether the page is neither acknowledged nor resolved.
func (p *Page) Open() bool { return p.Acknowledged.IsZero() && p.Resolved.IsZero() }

func validID(id string) bool {
	if id == "" || len(id) > MaxID {
		return false
	}
	for _, c := range []byte(id) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == ':':
		default:
			return false
		}
	}
	return true
}

var pageKeys = []string{"id", "alerts", "sent", "acknowledged", "resolved", "resolved_by"}

// check says what is wrong with a page, nil for nothing, once its alerts
// are sorted.
func (p *Page) check() error {
	switch {
	case p.ID == "":
		return errors.New("a page needs an id")
	case !validID(p.ID):
		return fmt.Errorf("page %q: an id is %d bytes at most, of letters, digits and -_.:", clip(p.ID), MaxID)
	case len(p.Alerts) == 0:
		return fmt.Errorf("page %s needs one alert at least", p.ID)
	case p.Sent.IsZero():
		return fmt.Errorf("page %s needs a sent time", p.ID)
	case !p.Resolved.IsZero() && p.ResolvedBy == "":
		return fmt.Errorf("page %s is resolved: say by whom, resolved_by: %s or %s", p.ID, ByAlert, ByPerson)
	case p.Resolved.IsZero() && p.ResolvedBy != "":
		return fmt.Errorf("page %s has resolved_by and no resolved time", p.ID)
	case p.ResolvedBy != "" && p.ResolvedBy != ByAlert && p.ResolvedBy != ByPerson:
		return fmt.Errorf("page %s: resolved_by is %s or %s", p.ID, ByAlert, ByPerson)
	}
	if err := incident.CheckAlerts(p.Alerts); err != nil {
		return fmt.Errorf("page %s: %w", p.ID, err)
	}
	return nil
}

func clip(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

// UnmarshalYAML takes a page as the responses file writes it, refusing a
// key it doesn't know, a value that isn't text where it is text (an id of 123
// is not "123"), and YAML anchors, aliases and merge keys.
func (p *Page) UnmarshalYAML(n *yaml.Node) error {
	if err := refuseReferences(n); err != nil {
		return err
	}
	return p.fromNode(n)
}

var errReferences = errors.New("YAML anchors, aliases and merge keys (&, *, <<) are not taken in a responses file: write each page out")

// refuseReferences walks the node tree, and refuses an anchor, an alias or a
// merge key anywhere in it: what a small file expands to a large one by.
func refuseReferences(root *yaml.Node) error {
	stack := []*yaml.Node{root}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n.Kind == yaml.AliasNode || n.Anchor != "" || n.Tag == "!!merge" {
			return errReferences
		}
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				if k := n.Content[i]; k.Kind == yaml.ScalarNode && k.Value == "<<" && k.Style == 0 {
					return errReferences
				}
			}
		}
		stack = append(stack, n.Content...)
	}
	return nil
}

// text is a scalar node's value when it is text: a quoted string, or one
// that isn't a number, a boolean or another kind of value.
func text(n *yaml.Node) (string, bool) {
	if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
		return "", false
	}
	// A plain scalar that reads as a number is one, though it is too large
	// for YAML to take it for one (1e999999): as in JSON, it is quoted to
	// be text.
	if n.Style == 0 && n.Value != "" && strings.ContainsRune("+-.0123456789", rune(n.Value[0])) {
		if _, err := strconv.ParseFloat(n.Value, 64); err == nil || errors.Is(err, strconv.ErrRange) {
			return "", false
		}
	}
	return n.Value, true
}

func (p *Page) fromNode(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return errors.New("a page is {id, alerts, sent, acknowledged, resolved, resolved_by}")
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i].Value
		if !slices.Contains(pageKeys, k) {
			return fmt.Errorf("a page has %s, not %q", strings.Join(pageKeys, ", "), k)
		}
		if seen[k] {
			return fmt.Errorf("a page has %s twice", k)
		}
		seen[k] = true
	}
	*p = Page{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i].Value, n.Content[i+1]
		null := v.Kind == yaml.ScalarNode && v.Tag == "!!null"
		switch k {
		case "id", "resolved_by":
			if null && k == "resolved_by" {
				break
			}
			s, ok := text(v)
			if !ok {
				return fmt.Errorf("a page's %s is text", k)
			}
			if k == "id" {
				p.ID = s
			} else {
				p.ResolvedBy = s
			}
		case "alerts":
			if null {
				break
			}
			if v.Kind != yaml.SequenceNode {
				return errors.New("a page's alerts are a list of {rule, labels}")
			}
			if len(v.Content) > incident.MaxAlerts {
				return fmt.Errorf("page %s: an incident lists %d alerts at most", p.ID, incident.MaxAlerts)
			}
			for _, a := range v.Content {
				alert, err := alertFromNode(a)
				if err != nil {
					return fmt.Errorf("page %s: %w", p.ID, err)
				}
				p.Alerts = append(p.Alerts, alert)
			}
		case "sent", "acknowledged", "resolved":
			if null {
				break
			}
			var t time.Time
			if err := v.Decode(&t); err != nil {
				return fmt.Errorf("page %s: %s: want a time such as 2026-11-20T09:00:00Z (RFC 3339)", p.ID, k)
			}
			switch k {
			case "sent":
				p.Sent = t
			case "acknowledged":
				p.Acknowledged = t
			default:
				p.Resolved = t
			}
		}
	}
	return nil
}

// alertFromNode takes {rule: NAME, labels: {k: v}}, every name and value text.
func alertFromNode(n *yaml.Node) (incident.Alert, error) {
	var a incident.Alert
	if n.Kind != yaml.MappingNode {
		return a, errors.New("an alert is {rule: NAME, labels: {LABEL: VALUE}}")
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i].Value, n.Content[i+1]
		if k != "rule" && k != "labels" {
			return a, fmt.Errorf("an alert has a rule and labels, not %q", k)
		}
		if seen[k] {
			return a, fmt.Errorf("an alert has %s twice", k)
		}
		seen[k] = true
		if k == "rule" {
			s, ok := text(v)
			if !ok {
				return a, errors.New("an alert's rule is text")
			}
			a.Rule = s
			continue
		}
		if v.Kind == yaml.ScalarNode && v.Tag == "!!null" {
			continue
		}
		if v.Kind != yaml.MappingNode {
			return a, fmt.Errorf("alert %s: labels are {LABEL: VALUE}", a.Rule)
		}
		if len(v.Content)/2 > incident.MaxAlertLabels {
			return a, fmt.Errorf("an alert has %d labels at most", incident.MaxAlertLabels)
		}
		for j := 0; j+1 < len(v.Content); j += 2 {
			name, ok1 := text(v.Content[j])
			val, ok2 := text(v.Content[j+1])
			if !ok1 || !ok2 {
				return a, fmt.Errorf("alert %s: label %s: a label's name and value are text", a.Rule, v.Content[j].Value)
			}
			if _, dup := a.Labels[name]; dup {
				return a, fmt.Errorf("alert %s: label %s twice", a.Rule, name)
			}
			if a.Labels == nil {
				a.Labels = map[string]string{}
			}
			a.Labels[name] = val
		}
	}
	return a, nil
}

// UnmarshalJSON takes a page as JSON, refusing, as UnmarshalYAML does, a key
// it doesn't know and a value of the wrong kind.
func (p *Page) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return errors.New(`a page is {"id", "alerts", "sent", "acknowledged", "resolved", "resolved_by"}`)
	}
	for k := range raw {
		if !slices.Contains(pageKeys, k) {
			return fmt.Errorf("a page has %s, not %q", strings.Join(pageKeys, ", "), k)
		}
	}
	type plain Page
	var x plain
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&x); err != nil {
		return fmt.Errorf("a page's id and resolved_by are text, its times RFC 3339, its alerts as an incident's: %w", err)
	}
	*p = Page(x)
	return nil
}

// Load reads a responses file:
//
//	responses:
//	  - id: PD-Q1W2E3
//	    alerts: [{rule: KubePodCrashLooping, labels: {namespace: shop}}]
//	    sent: 2026-11-20T03:12:00Z
//	    acknowledged: 2026-11-20T03:15:00Z
//	    resolved: 2026-11-20T03:20:00Z
//	    resolved_by: alert
//
// YAML or JSON (JSON is YAML). The pages come back in the order of their
// sent time, and each page's alerts sorted, each once.
func Load(path string) ([]Page, error) {
	// A pipe or a device would hang an open or a read, or never end: looked
	// at before it is opened, and again once it is.
	if st, err := os.Stat(path); err != nil {
		return nil, err
	} else if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err != nil {
		f.Close()
		return nil, err
	} else if !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxFile+1))
	f.Close()
	if err != nil {
		return nil, err
	}
	return Parse(b, path)
}

// Parse reads the text of a responses file, as Load does; name is what an
// error calls it. A file that starts with { is JSON, read as ParseJSON reads
// it; any other is YAML, in which anchors, aliases and merge keys are refused
// before anything is decoded.
func Parse(b []byte, name string) ([]Page, error) {
	if len(b) > MaxFile {
		return nil, fmt.Errorf("%s: more than %d MiB: a file of responses is less", name, MaxFile>>20)
	}
	if t := bytes.TrimLeft(b, " \t\r\n\ufeff"); len(t) > 0 && t[0] == '{' {
		return parseJSON(bytes.NewReader(b), MaxFile, name)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", name, oneLine(err))
	}
	if err := refuseReferences(&doc); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	// An empty file isn't taken for no pages: a job that failed leaves one
	// behind, and the file owns what it holds.
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return nil, fmt.Errorf("%s: no responses key: want responses:, with a list under it, an empty one if there are none", name)
	}
	top := doc.Content[0]
	if top.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: want \"responses:\" at the top, the list under it", name)
	}
	var list *yaml.Node
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value == "responses" {
			if list != nil {
				return nil, fmt.Errorf("%s: responses twice", name)
			}
			list = top.Content[i+1]
		}
	}
	if list == nil {
		return nil, fmt.Errorf("%s: no responses key: want responses:, with a list under it, an empty one if there are none", name)
	}
	if len(top.Content) > 2 {
		return nil, fmt.Errorf("%s: the top of a responses file is responses: alone", name)
	}
	if list.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s: responses is a list of pages", name)
	}
	// Counted before they are read: a list made to take a reader's memory.
	if len(list.Content) > MaxPages {
		return nil, fmt.Errorf("%s: %d pages at most", name, MaxPages)
	}
	pages := make([]Page, len(list.Content))
	for i, n := range list.Content {
		if err := pages[i].fromNode(n); err != nil {
			return nil, fmt.Errorf("%s: %w", name, oneLine(err))
		}
	}
	return Check(pages, name)
}

// Check holds pages to what a file of them is held to, and puts them in
// order: each id once, each page's alerts sorted and each once, the bounds
// kept, in the order of their sent time (the id between those sent at once).
// name is what an error calls them.
func Check(pages []Page, name string) ([]Page, error) {
	if len(pages) > MaxPages {
		return nil, fmt.Errorf("%s: %d pages at most", name, MaxPages)
	}
	seen := make(map[string]bool, len(pages))
	for i := range pages {
		p := &pages[i]
		p.Alerts = incident.SortAlerts(p.Alerts)
		if err := p.check(); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if seen[p.ID] {
			return nil, fmt.Errorf("%s: page %s is there twice; give each its own id", name, p.ID)
		}
		seen[p.ID] = true
	}
	slices.SortFunc(pages, func(a, b Page) int {
		if c := a.Sent.Compare(b.Sent); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return pages, nil
}

func oneLine(err error) error {
	var te *yaml.TypeError
	if errors.As(err, &te) {
		list := te.Errors
		more := ""
		if len(list) > 5 {
			more = fmt.Sprintf("; and %d more", len(list)-5)
			list = list[:5]
		}
		return errors.New(strings.Join(list, "; ") + more)
	}
	return err
}

// Index is pages by the rule their alerts name, in the order of their sent
// time, for finding the page a firing was sent as.
type Index struct {
	pages  []Page
	byRule map[string][]int // indexes into pages, by sent time
}

// NewIndex indexes pages, which Load or Check put in order.
func NewIndex(pages []Page) *Index {
	ix := &Index{pages: pages, byRule: map[string][]int{}}
	for i := range pages {
		seen := map[string]bool{}
		for _, a := range pages[i].Alerts {
			if !seen[a.Rule] {
				seen[a.Rule] = true
				ix.byRule[a.Rule] = append(ix.byRule[a.Rule], i)
			}
		}
	}
	return ix
}

// Len is how many pages there are.
func (ix *Index) Len() int {
	if ix == nil {
		return 0
	}
	return len(ix.pages)
}

// Pages are the pages, in the order of their sent time.
func (ix *Index) Pages() []Page {
	if ix == nil {
		return nil
	}
	return ix.pages
}

// Find is the page a firing of rule with labels was sent as: of those that
// list it and were sent from from to to, both included, the one the
// --responses file sent if there is one, else the nearest to the firing's
// start; nil for none. Of two as far apart the one of the lesser source,
// then the lesser id is the nearer. At most MaxFindScan pages per name are
// looked at.
func (ix *Index) Find(rule string, labels map[string]string, from, to time.Time) *Page {
	if ix == nil {
		return nil
	}
	var best *Page
	for _, name := range ruleNames(rule) {
		list := ix.byRule[name]
		lo, _ := slices.BinarySearchFunc(list, from, func(i int, t time.Time) int { return ix.pages[i].Sent.Compare(t) })
		list = list[lo:]
		if len(list) > MaxFindScan {
			list = list[:MaxFindScan]
		}
		for _, i := range list {
			p := &ix.pages[i]
			if p.Sent.After(to) {
				break
			}
			// Sent in order, so every later page is farther than a file's.
			if best != nil && best.fromFile() && p.Sent.After(best.Sent) {
				break
			}
			if !p.Lists(rule, labels) {
				continue
			}
			if best == nil || preferred(p, best, from) {
				best = p
			}
		}
	}
	return best
}

// preferred reports whether page p is the firing's page over q: a file's over
// any other, then the nearer.
func preferred(p, q *Page, at time.Time) bool {
	if pf, qf := p.fromFile(), q.fromFile(); pf != qf {
		return pf
	}
	return nearer(p, q, at)
}

func nearer(p, q *Page, at time.Time) bool {
	dp, dq := p.Sent.Sub(at).Abs(), q.Sent.Sub(at).Abs()
	if dp != dq {
		return dp < dq
	}
	if p.Source != q.Source {
		return p.Source < q.Source
	}
	return p.ID < q.ID
}

// ruleNames are the names a page's alert may carry for a firing of rule: its
// own, and with a firing told apart from others of its name (HighErrorRate
// [severity=critical]), the name alone (incident.Alert.Names).
func ruleNames(rule string) []string { return RuleNames(rule) }

// RuleNames are the names a page's alert may carry for a firing of rule: its
// own, and the name alone of one told apart from others of its name.
func RuleNames(rule string) []string {
	names := []string{rule}
	if !strings.HasSuffix(rule, "]") {
		return names
	}
	for i := 1; i+1 < len(rule); i++ {
		if rule[i:i+2] == " [" {
			names = append(names, rule[:i])
		}
	}
	return names
}

// Unmatched is how many pages are not among used, a set of page ids a
// caller kept of the pages Find gave it: those that matched no firing.
func (ix *Index) Unmatched(used map[string]bool) int {
	n := 0
	for _, p := range ix.Pages() {
		if !used[p.ID] {
			n++
		}
	}
	return n
}
