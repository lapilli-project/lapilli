package serve

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/language"
)

// The UI is written in English; i18n/LANG.json gives, for each English
// text, the same in another language. A text missing there is shown in
// English. To add a language, add its file and its name to names.
//
//go:embed i18n/*.json
var catalogFiles embed.FS

// names are the languages the UI speaks, as each calls itself, English
// first.
var names = []struct{ Code, Name string }{{"en", "English"}, {"ko", "한국어"}}

var (
	catalogs = map[string]map[string]string{}
	matcher  language.Matcher
	// pagesIn are the templates for each language, "t" giving its texts.
	pagesIn = map[string]*template.Template{}
)

func init() {
	var tags []language.Tag
	for _, n := range names {
		tags = append(tags, language.MustParse(n.Code))
		if n.Code == "en" {
			continue
		}
		b, err := catalogFiles.ReadFile("i18n/" + n.Code + ".json")
		if err != nil {
			panic(err)
		}
		m := map[string]string{}
		if err := json.Unmarshal(b, &m); err != nil {
			panic(fmt.Sprintf("i18n/%s.json: %v", n.Code, err))
		}
		catalogs[n.Code] = m
	}
	matcher = language.NewMatcher(tags)
	for _, n := range names {
		lang := n.Code
		t := template.Must(pages.Clone())
		pagesIn[lang] = t.Funcs(template.FuncMap{
			"t": func(msg string, args ...any) string { return tr(lang, msg, args...) },
			// who is who did something; "anonymous" in the reader's
			// language when no sign-in proxy said.
			"who": func(by string) string {
				if by == "" || by == "anonymous" {
					return tr(lang, "anonymous")
				}
				return by
			},
		})
	}
}

// missing notes texts asked for in a language that has none for them, for
// the tests to find.
var missing = struct {
	sync.Mutex
	m map[string]bool
}{m: map[string]bool{}}

// tr is a text in a language, with args put in as fmt does.
func tr(lang, msg string, args ...any) string {
	if lang != "en" {
		if t, ok := catalogs[lang][msg]; ok {
			msg = t
		} else {
			missing.Lock()
			missing.m[lang+"\x00"+msg] = true
			missing.Unlock()
		}
	}
	if len(args) == 0 {
		return msg
	}
	return fmt.Sprintf(msg, args...)
}

// lang is the language to answer a request in: the one chosen on the page,
// or else the browser's.
func lang(r *http.Request) string {
	if c, err := r.Cookie("lang"); err == nil && known(c.Value) {
		return c.Value
	}
	tag, _ := language.MatchStrings(matcher, r.Header.Get("Accept-Language"))
	base, _ := tag.Base()
	if known(base.String()) {
		return base.String()
	}
	return "en"
}

func known(code string) bool {
	for _, n := range names {
		if n.Code == code {
			return true
		}
	}
	return false
}

// chooseLang keeps the language chosen on the page, and goes back to the
// page it was chosen on.
func chooseLang(w http.ResponseWriter, r *http.Request) {
	to := r.URL.Query().Get("to")
	if !known(to) {
		http.Error(w, "unknown language", http.StatusBadRequest)
		return
	}
	// Secure when the page came over HTTPS, here or at a proxy in front.
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{Name: "lang", Value: to, Path: "/", MaxAge: int(365 * 24 * time.Hour / time.Second),
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, safeBack(r.URL.Query().Get("back"), "/"), http.StatusSeeOther)
}

// safeBack is a page on this server to go back to, or else.
func safeBack(back, or string) string {
	u, err := url.Parse(back)
	if err != nil || u.Scheme != "" || u.Host != "" || !strings.HasPrefix(back, "/") ||
		strings.HasPrefix(back, "//") || strings.HasPrefix(back, "/\\") {
		return or
	}
	back = path.Clean(u.EscapedPath())
	if u.RawQuery != "" {
		back += "?" + u.RawQuery
	}
	return back
}
