package prom

import (
	"net/http"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// pageRead is how much of an answer pageSaid reads: a page's title is
// near its start, and what follows isn't said.
const pageRead = 16 << 10

// pageSaidMost is how much of a page's title, or of its text, pageSaid
// keeps.
const pageSaidMost = 120

// pageSaid is what an answer's text, from the API's address, says when it
// is an HTML page, as a frontend's, a proxy's or a sign-in's is: "an HTML
// page" and its title (or, with none, the start of its text, without what
// its scripts and styles hold), rather than its markup, which says nothing
// in a line a person reads and pushes the rest of a report out of it. ok is
// false for text that isn't a page, which is said as it was.
//
// What it keeps is the server's text still, so what req carried to sign in
// is taken out of it (serverText) after its entities are read, so that a
// token written as &#115;... is found, and before it is cut, so that no
// start of one is left.
func pageSaid(text string, req *http.Request) (string, bool) {
	t := strings.TrimSpace(text)
	if !isPage(t) {
		return "", false
	}
	z := html.NewTokenizer(strings.NewReader(t[:min(len(t), pageRead)]))
	var title, body strings.Builder
	var in atom.Atom // the element whose raw text the tokenizer gives: title, script, style
	for done := false; !done; {
		switch z.Next() {
		case html.ErrorToken:
			done = true
		case html.StartTagToken:
			name, _ := z.TagName()
			switch a := atom.Lookup(name); a {
			case atom.Title, atom.Script, atom.Style, atom.Noscript, atom.Template:
				in = a
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			if atom.Lookup(name) == in {
				in = 0
			}
		case html.TextToken:
			switch in {
			case atom.Title:
				title.WriteString(" " + string(z.Text()))
			case 0:
				if body.Len() < pageRead {
					body.WriteString(" " + string(z.Text()))
				}
			}
		}
	}
	said := strings.Join(strings.Fields(title.String()), " ")
	if said == "" {
		said = strings.Join(strings.Fields(body.String()), " ")
	}
	said = clip(serverText(said, req), pageSaidMost)
	if said == "" {
		return "an HTML page", true
	}
	return `an HTML page, "` + said + `"`, true
}

// isPage reports whether t, trimmed, is an HTML page: a doctype or an html
// element first, or markup first with one of a page's elements soon after.
func isPage(t string) bool {
	if !strings.HasPrefix(t, "<") {
		return false
	}
	s := strings.ToLower(t[:min(len(t), 512)])
	if strings.HasPrefix(s, "<!doctype html") || strings.HasPrefix(s, "<html") {
		return true
	}
	for _, e := range []string{"<html", "<head", "<body", "<title"} {
		if strings.Contains(s, e) {
			return true
		}
	}
	return false
}
