package serve

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A destination's own words (docs/design/notifications.md, "Who is told
// what"): a template --config's notify: gives it, of text and fields, each
// {{.Field}} and nothing else, which no function, loop or call can be
// written in. What a field holds comes from outside (a rule's name, a
// task's title, a label's value, an incident's) and is escaped for where it
// goes; the template's own text is the admin's, as written, so that a
// Slack template may mention a channel or link where it means to.

// templateFields are the fields a template may name, in the order the
// docs list them.
var templateFields = []string{"Event", "Line", "Link", "Rule", "Team", "Title", "ID", "By", "Labels", "At"}

// Bounds on a template: its text, a field's value in it, and what it
// gives, in bytes. A value or a text past its bound is cut at a letter's
// end, with an ellipsis.
const (
	maxTemplate   = 2000
	maxFieldValue = 500
	maxTemplated  = 4000
)

// noticeTemplate is a template read: its parts in order, each a text as
// written or a field.
type noticeTemplate struct {
	parts []templatePart
}

type templatePart struct{ text, field string }

// parseTemplate reads a template, or says what is wrong with it, as a
// text to translate and its values, of the destination it is for.
func parseTemplate(dest, text string) (*noticeTemplate, error) {
	if len(text) > maxTemplate {
		return nil, bad("the template for %s is more than %d bytes", dest, maxTemplate)
	}
	t := &noticeTemplate{}
	rest := text
	for rest != "" {
		i := strings.Index(rest, "{{")
		if i < 0 {
			t.parts = append(t.parts, templatePart{text: rest})
			break
		}
		if i > 0 {
			t.parts = append(t.parts, templatePart{text: rest[:i]})
		}
		j := strings.Index(rest[i:], "}}")
		if j < 0 {
			return nil, bad("the template for %s opens {{ and doesn't close it", dest)
		}
		inner := strings.TrimSpace(rest[i+2 : i+j])
		field, ok := strings.CutPrefix(inner, ".")
		if !ok || !slices.Contains(templateFields, field) {
			return nil, bad("the template for %s takes {{.Field}} alone, a field one of %s: not {{%s}}", dest, strings.Join(templateFields, ", "), inner)
		}
		t.parts = append(t.parts, templatePart{field: field})
		rest = rest[i+j+2:]
	}
	return t, nil
}

// noticeFields are what a template's fields hold, as a notice tells them.
type noticeFields struct {
	Event, Line, Link, Rule, Team, Title, ID, By, Labels, At string
}

func (f noticeFields) get(name string) string {
	switch name {
	case "Event":
		return f.Event
	case "Line":
		return f.Line
	case "Link":
		return f.Link
	case "Rule":
		return f.Rule
	case "Team":
		return f.Team
	case "Title":
		return f.Title
	case "ID":
		return f.ID
	case "By":
		return f.By
	case "Labels":
		return f.Labels
	case "At":
		return f.At
	}
	return ""
}

// render is the template with its fields, each value cut to
// maxFieldValue and escaped by esc, for where it goes; maxTemplated bytes
// at most.
func (t *noticeTemplate) render(f noticeFields, esc func(string) string) string {
	var b strings.Builder
	for _, p := range t.parts {
		if p.field == "" {
			b.WriteString(p.text)
		} else {
			b.WriteString(esc(cutBytes(f.get(p.field), maxFieldValue)))
		}
		if b.Len() > maxTemplated {
			break
		}
	}
	return cutBytes(b.String(), maxTemplated)
}

// cutBytes is s, n bytes at most: cut at a letter's end, with an ellipsis,
// when it is longer.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	end := n - len("…")
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "…"
}

// oneLine is a value from outside on one line: each control character, a
// line break among them, and Unicode's line and paragraph separators, a
// space. A title can't start a line of its own that reads as Lapilli's.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, s)
}

// slackEscape is a value from outside as Slack's markup takes it: &, <
// and > as the entities Slack reads them as, so no value makes a link, a
// mention (<!channel>, <@U…>) or a channel's name; three backquotes as
// three quotes, so none ends a code block the template opened; on one line.
var slackEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "```", "'''")

func slackEscape(s string) string { return slackEscaper.Replace(oneLine(s)) }

// jsonEscape is a value from outside as a JSON webhook's text takes it: on
// one line. The text is a JSON string, which encoding/json escapes; a
// receiver that reads it as markup escapes it as it reads it.
func jsonEscape(s string) string { return oneLine(s) }
