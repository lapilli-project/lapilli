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
}

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
// key it doesn't know.
func (p *Page) UnmarshalYAML(n *yaml.Node) error {
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
		var err error
		switch k {
		case "id", "resolved_by":
			if v.Kind != yaml.ScalarNode || null && k == "id" {
				return fmt.Errorf("a page's %s is text", k)
			}
			s := v.Value
			if k == "id" {
				p.ID = s
			} else {
				p.ResolvedBy = s
			}
		case "alerts":
			if null {
				break
			}
			err = v.Decode(&p.Alerts)
		case "sent", "acknowledged", "resolved":
			if null {
				break
			}
			var t time.Time
			if err = v.Decode(&t); err != nil {
				return fmt.Errorf("page %s: %s: want a time such as 2026-11-24T09:00:00Z (RFC 3339)", p.ID, k)
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
		if err != nil {
			return err
		}
	}
	return nil
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
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxFile+1))
	f.Close()
	if err != nil {
		return nil, err
	}
	return Parse(b, path)
}

// Parse reads the text of a responses file, as Load does; name is what an
// error calls it.
func Parse(b []byte, name string) ([]Page, error) {
	if len(b) > MaxFile {
		return nil, fmt.Errorf("%s: more than %d MiB: a file of responses is less", name, MaxFile>>20)
	}
	var raw struct {
		Responses yaml.Node `yaml:"responses"`
	}
	if err := yaml.Unmarshal(b, &raw); err != nil {
		var top yaml.Node
		if yaml.Unmarshal(b, &top) == nil && top.Kind == yaml.DocumentNode && len(top.Content) == 1 && top.Content[0].Kind == yaml.SequenceNode {
			return nil, fmt.Errorf("%s: want \"responses:\" at the top, the list under it", name)
		}
		return nil, fmt.Errorf("%s: %w", name, oneLine(err))
	}
	// An empty file isn't taken for no pages: a job that failed leaves one
	// behind, and the file owns what it holds.
	if raw.Responses.Kind == 0 {
		return nil, fmt.Errorf("%s: no responses key: want responses:, with a list under it, an empty one if there are none", name)
	}
	if raw.Responses.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s: responses is a list of pages", name)
	}
	// Counted before they are read: a list made to take a reader's memory.
	if len(raw.Responses.Content) > MaxPages {
		return nil, fmt.Errorf("%s: %d pages at most", name, MaxPages)
	}
	var pages []Page
	if err := raw.Responses.Decode(&pages); err != nil {
		return nil, fmt.Errorf("%s: %w", name, oneLine(err))
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

// Find is the page a firing of rule with labels was sent as: the nearest
// to the firing's start of those that list it and were sent from from to
// to, both included; nil for none. The earlier (then the lesser id) is
// the nearer of two as far apart.
func (ix *Index) Find(rule string, labels map[string]string, from, to time.Time) *Page {
	if ix == nil {
		return nil
	}
	var best *Page
	for _, name := range ruleNames(rule) {
		list := ix.byRule[name]
		lo, _ := slices.BinarySearchFunc(list, from, func(i int, t time.Time) int { return ix.pages[i].Sent.Compare(t) })
		for _, i := range list[lo:] {
			p := &ix.pages[i]
			if p.Sent.After(to) {
				break
			}
			if !p.Lists(rule, labels) {
				continue
			}
			if best == nil || nearer(p, best, from) {
				best = p
			}
		}
	}
	return best
}

func nearer(p, q *Page, at time.Time) bool {
	dp, dq := p.Sent.Sub(at).Abs(), q.Sent.Sub(at).Abs()
	if dp != dq {
		return dp < dq
	}
	return p.ID < q.ID
}

// ruleNames are the names a page's alert may carry for a firing of rule: its
// own, and with a firing told apart from others of its name (HighErrorRate
// [severity=critical]), the name alone (incident.Alert.Names).
func ruleNames(rule string) []string {
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
