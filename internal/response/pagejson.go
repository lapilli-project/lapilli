package response

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/incident"
)

// ParseJSON reads {"responses": [...]} from r a token at a time, page by
// page: it counts the pages and the bytes as it goes and stops at MaxPages
// pages or maxBytes bytes, and holds each page to its bounds (an id's length,
// an incident's alerts, as incident.CheckAlerts does) before it reads the
// next, so what it holds is what the bounds allow, not what a body made it
// build. It refuses a key it doesn't know, a duplicate one, and a value that
// isn't text where it is text. The pages come back as Check puts them.
func ParseJSON(r io.Reader, maxBytes int64) ([]Page, error) {
	return parseJSON(r, maxBytes, "the pages")
}

type limited struct {
	r   io.Reader
	max int64
	n   int64
}

func (l *limited) Read(p []byte) (int, error) {
	if l.n > l.max {
		return 0, io.EOF
	}
	if room := l.max + 1 - l.n; int64(len(p)) > room {
		p = p[:room]
	}
	n, err := l.r.Read(p)
	l.n += int64(n)
	return n, err
}

type jsonPages struct {
	dec *json.Decoder
}

func parseJSON(r io.Reader, maxBytes int64, name string) ([]Page, error) {
	lim := &limited{r: r, max: maxBytes}
	jp := &jsonPages{dec: json.NewDecoder(lim)}
	jp.dec.UseNumber()
	pages, err := jp.responses(name)
	if lim.n > maxBytes {
		return nil, fmt.Errorf("%s: more than %d MiB: a file of responses is less", name, maxBytes>>20)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return Check(pages, name)
}

func (jp *jsonPages) delim(want json.Delim, what string) error {
	t, err := jp.dec.Token()
	if err != nil {
		return jsonErr(err)
	}
	if d, ok := t.(json.Delim); !ok || d != want {
		return errors.New(what)
	}
	return nil
}

func jsonErr(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// key reads an object's next key, "" and false at its end.
func (jp *jsonPages) key() (string, bool, error) {
	if !jp.dec.More() {
		if _, err := jp.dec.Token(); err != nil { // the }
			return "", false, jsonErr(err)
		}
		return "", false, nil
	}
	t, err := jp.dec.Token()
	if err != nil {
		return "", false, jsonErr(err)
	}
	k, ok := t.(string)
	if !ok {
		return "", false, errors.New("an object's keys are text")
	}
	return k, true, nil
}

// text reads a value that is text, or null (ok false).
func (jp *jsonPages) text(what string, nullOK bool) (s string, null bool, err error) {
	t, err := jp.dec.Token()
	if err != nil {
		return "", false, jsonErr(err)
	}
	switch v := t.(type) {
	case string:
		return v, false, nil
	case nil:
		if nullOK {
			return "", true, nil
		}
	}
	return "", false, fmt.Errorf("%s is text", what)
}

func (jp *jsonPages) responses(name string) ([]Page, error) {
	if err := jp.delim('{', `want {"responses": [...]}`); err != nil {
		return nil, err
	}
	var pages []Page
	found := false
	for {
		k, more, err := jp.key()
		if err != nil {
			return nil, err
		}
		if !more {
			break
		}
		if k != "responses" {
			return nil, fmt.Errorf("the top of a responses file is responses: alone, not %q", k)
		}
		if found {
			return nil, errors.New("responses twice")
		}
		found = true
		if err := jp.delim('[', "responses is a list of pages"); err != nil {
			return nil, err
		}
		for jp.dec.More() {
			if len(pages) >= MaxPages {
				return nil, fmt.Errorf("%d pages at most", MaxPages)
			}
			var p Page
			if err := jp.page(&p); err != nil {
				return nil, err
			}
			pages = append(pages, p)
		}
		if _, err := jp.dec.Token(); err != nil { // the ]
			return nil, jsonErr(err)
		}
	}
	if !found {
		return nil, errors.New("no responses key: want responses:, with a list under it, an empty one if there are none")
	}
	if _, err := jp.dec.Token(); err != io.EOF {
		return nil, errors.New("there is more after the responses")
	}
	if pages == nil {
		pages = []Page{}
	}
	return pages, nil
}

// page reads one page and holds it to its bounds.
func (jp *jsonPages) page(p *Page) error {
	if err := jp.delim('{', "a page is {id, alerts, sent, acknowledged, resolved, resolved_by}"); err != nil {
		return err
	}
	seen := map[string]bool{}
	for {
		k, more, err := jp.key()
		if err != nil {
			return err
		}
		if !more {
			break
		}
		if !slices.Contains(pageKeys, k) {
			return fmt.Errorf("a page has %s, not %q", strings.Join(pageKeys, ", "), clip(k))
		}
		if seen[k] {
			return fmt.Errorf("a page has %s twice", k)
		}
		seen[k] = true
		switch k {
		case "id", "resolved_by":
			s, null, err := jp.text("a page's "+k, k == "resolved_by")
			if err != nil {
				return err
			}
			if null {
				break
			}
			if k == "id" {
				if len(s) > MaxID {
					return fmt.Errorf("page %q: an id is %d bytes at most, of letters, digits and -_.:", clip(s), MaxID)
				}
				p.ID = s
			} else {
				p.ResolvedBy = s
			}
		case "alerts":
			if err := jp.alerts(p); err != nil {
				return err
			}
		default:
			s, null, err := jp.text("a page's "+k, true)
			if err != nil {
				return err
			}
			if null {
				break
			}
			var t time.Time
			if err := t.UnmarshalText([]byte(s)); err != nil {
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
	return incident.CheckAlerts(p.Alerts)
}

func (jp *jsonPages) alerts(p *Page) error {
	t, err := jp.dec.Token()
	if err != nil {
		return jsonErr(err)
	}
	if t == nil {
		return nil
	}
	if d, ok := t.(json.Delim); !ok || d != '[' {
		return errors.New("a page's alerts are a list of {rule, labels}")
	}
	for jp.dec.More() {
		if len(p.Alerts) >= incident.MaxAlerts {
			return fmt.Errorf("page %s: an incident lists %d alerts at most", p.ID, incident.MaxAlerts)
		}
		a, err := jp.alert()
		if err != nil {
			return fmt.Errorf("page %s: %w", p.ID, err)
		}
		p.Alerts = append(p.Alerts, a)
	}
	_, err = jp.dec.Token() // the ]
	return jsonErr(err)
}

func (jp *jsonPages) alert() (incident.Alert, error) {
	var a incident.Alert
	if err := jp.delim('{', `an alert is {"rule": NAME, "labels": {LABEL: VALUE}}`); err != nil {
		return a, err
	}
	seen := map[string]bool{}
	for {
		k, more, err := jp.key()
		if err != nil {
			return a, err
		}
		if !more {
			return a, nil
		}
		if k != "rule" && k != "labels" {
			return a, fmt.Errorf("an alert has a rule and labels, not %q", clip(k))
		}
		if seen[k] {
			return a, fmt.Errorf("an alert has %s twice", k)
		}
		seen[k] = true
		if k == "rule" {
			s, _, err := jp.text("an alert's rule", false)
			if err != nil {
				return a, err
			}
			if len(s) > incident.MaxAlertRule {
				return a, fmt.Errorf("an alert's rule is %d bytes at most", incident.MaxAlertRule)
			}
			a.Rule = s
			continue
		}
		t, err := jp.dec.Token()
		if err != nil {
			return a, jsonErr(err)
		}
		if t == nil {
			continue
		}
		if d, ok := t.(json.Delim); !ok || d != '{' {
			return a, errors.New("an alert's labels are {LABEL: VALUE}, text")
		}
		for {
			name, more, err := jp.key()
			if err != nil {
				return a, err
			}
			if !more {
				break
			}
			val, _, err := jp.text("a label's value", false)
			if err != nil {
				return a, err
			}
			switch {
			case len(a.Labels) >= incident.MaxAlertLabels:
				return a, fmt.Errorf("an alert has %d labels at most", incident.MaxAlertLabels)
			case len(name) > 128 || len(val) > 256:
				return a, errors.New("an alert's label is a name of up to 128 bytes and a value of up to 256, neither empty")
			}
			if _, dup := a.Labels[name]; dup {
				return a, fmt.Errorf("label %s twice", clip(name))
			}
			if a.Labels == nil {
				a.Labels = map[string]string{}
			}
			a.Labels[name] = val
		}
	}
}
