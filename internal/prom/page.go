package prom

import (
	"net/http"
	"regexp"
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
// false for text that isn't a page, which is said as it was. cut says the
// answer went on past text.
//
// What it keeps is the server's text still, so what req carried to sign in
// is taken out of it (pageText) after its entities are read, so that a
// token written as &#115;... is found, and before it is cut, so that no
// start of one is left. Of a text cut short, here or before, the text it
// ends in is left out: what a secret's start, cut, would be in.
func pageSaid(text string, cut bool, req *http.Request) (string, bool) {
	t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "\ufeff"))
	if !isPage(t) {
		return "", false
	}
	if len(t) > pageRead {
		t, cut = t[:pageRead], true
	}
	z := html.NewTokenizer(strings.NewReader(t))
	var title, body []string
	var last *[]string // the text the last token was, if it was
	read := 0
	var in atom.Atom // the element whose raw text the tokenizer gives
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			if cut && last != nil {
				*last = (*last)[:len(*last)-1]
			}
			break
		}
		last = nil
		switch tt {
		case html.StartTagToken:
			name, _ := z.TagName()
			switch a := atom.Lookup(name); a {
			case atom.Title, atom.Script, atom.Style, atom.Noscript, atom.Template, atom.Plaintext:
				in = a
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			if a := atom.Lookup(name); a == in && a != atom.Plaintext {
				in = 0
			}
		case html.TextToken:
			switch in {
			case atom.Title:
				title = append(title, string(z.Text()))
				last = &title
			case 0:
				if read < pageRead {
					s := string(z.Text())
					read += len(s)
					body = append(body, s)
					last = &body
				}
			}
		}
	}
	said := pageText(strings.Join(title, " "), req)
	if said == "" {
		said = pageText(strings.Join(body, " "), req)
	}
	said = clip(said, pageSaidMost)
	if said == "" {
		return "an HTML page", true
	}
	return `an HTML page, "` + said + `"`, true
}

// pageText is a page's text as pageSaid says it: each run of white space
// one space, without what doesn't show or what req carried to sign in; and,
// when one of those is hidden in two by white space or a character that
// doesn't show, with those taken out before it is looked for, as
// serverText does, so that it is found (quotedText's way).
func pageText(raw string, req *http.Request) string {
	spaced := serverText(strings.Join(strings.Fields(raw), " "), req)
	joined := strings.Join(strings.Fields(serverText(raw, req)), " ")
	if strings.Count(joined, "[redacted]") > strings.Count(spaced, "[redacted]") {
		return joined
	}
	return spaced
}

// pageStart and pageElement are how a page begins, and the elements one
// has near its start.
var (
	pageStart   = regexp.MustCompile(`(?i)^<(?:!doctype\s+html|html[\s>/])`)
	pageElement = regexp.MustCompile(`(?i)<(?:html|head|body|title)[\s>/]`)
)

// isPage reports whether t, trimmed, is an HTML page: a doctype or an html
// element first, or markup first with one of a page's elements soon after.
func isPage(t string) bool {
	if !strings.HasPrefix(t, "<") {
		return false
	}
	s := t[:min(len(t), 512)]
	return pageStart.MatchString(s) || pageElement.MatchString(s)
}
