package serve

// The bar's right end: the language picker and who is signed in, on every
// page (docs/design/ui.md).

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

var (
	barEnd    = regexp.MustCompile(`(?s)<div class="bar-end">.*?\n</div>`)
	whoMenu   = regexp.MustCompile(`(?s)<details class="picker end"><summary><span class="vh">Signed in as </span>.*?</details>`)
	langMenu  = regexp.MustCompile(`(?s)<details class="picker end"><summary><span class="vh">Language: </span>.*?</details>`)
	footerRe  = regexp.MustCompile(`(?s)<footer>.*?</footer>`)
	ariaCurr  = `aria-current="true"`
	signOutFm = `<form class="signout" method="post" action="/auth/logout">`
)

func TestUserMenuSignedIn(t *testing.T) {
	s, idp := oidcServer(t, nil)
	b := newBrowser(t, s.Handler())
	b.signIn(idp, "/", claimsOf("boss@example.com", "ops"))
	w := b.do("GET", "/rules", nil)
	body := w.Body.String()
	menu := whoMenu.FindString(body)
	if menu == "" {
		t.Fatalf("no user menu in %s", barEnd.FindString(body))
	}
	for _, want := range []string{"boss@example.com", ">admin<", signOutFm, "Sign out"} {
		if !strings.Contains(menu, want) {
			t.Errorf("the menu lacks %q: %s", want, menu)
		}
	}
	if f := footerRe.FindString(body); strings.Contains(f, "boss@example.com") || strings.Contains(f, "Sign out") || strings.Contains(f, "form") {
		t.Errorf("the footer still says who is signed in: %s", f)
	}
	if !strings.Contains(footerRe.FindString(body), "times in") {
		t.Error("the footer lost the time zone")
	}
	// The menu is before the page's main, not after it.
	if strings.Index(body, `class="picker end"`) > strings.Index(body, "<footer>") {
		t.Error("the menu is not at the top")
	}
}

// A page with no context bar (an error) has the menu too.
func TestUserMenuWithoutBar(t *testing.T) {
	s, idp := oidcServer(t, nil)
	b := newBrowser(t, s.Handler())
	b.signIn(idp, "/", claimsOf("boss@example.com", "ops"))
	w := b.do("GET", "/rules/NoSuchRule", nil)
	body := w.Body.String()
	if w.Code != http.StatusNotFound || strings.Contains(body, `class="ctx"`) {
		t.Fatalf("not a page without a bar: %d", w.Code)
	}
	if !whoMenu.MatchString(body) || !langMenu.MatchString(body) {
		t.Errorf("the page has no menu: %s", barEnd.FindString(body))
	}
}

func TestUserMenuReadOnlyAndNobody(t *testing.T) {
	s, _, _ := rolesServer(t, nil)
	h := s.Handler()
	// Named by the proxy, but no sign-out: the person, no button.
	_, body := getAs(t, h, "/rules", "ed@example.com")
	menu := whoMenu.FindString(body)
	if !strings.Contains(menu, "ed@example.com") || strings.Contains(menu, "<form") {
		t.Errorf("a proxy's user: %s", menu)
	}
	// A viewer is read-only, and no one named has the quiet label at most.
	_, body = getAs(t, h, "/rules", "")
	end := barEnd.FindString(body)
	if strings.Contains(end, "Sign out") || strings.Contains(end, "<form") {
		t.Errorf("nobody signed in, and a sign-out: %s", end)
	}
	s2, _ := server(t, func(c *Config) { c.ReadOnly = true })
	_, body = getAs(t, s2.Handler(), "/rules", "")
	end = barEnd.FindString(body)
	if !strings.Contains(end, `<span class="bar-k">read-only</span>`) || whoMenu.MatchString(end) {
		t.Errorf("read-only, nobody named: %s", end)
	}
	s3, _ := server(t, nil)
	_, body = getAs(t, s3.Handler(), "/rules", "")
	if end := barEnd.FindString(body); strings.Contains(end, "read-only") || whoMenu.MatchString(end) {
		t.Errorf("a server anyone writes to says who or read-only: %s", end)
	}
}

func TestLanguagePicker(t *testing.T) {
	h := pagesServer(t).Handler()
	get := func(path, accept, cookie string) string {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if accept != "" {
			r.Header.Set("Accept-Language", accept)
		}
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: "lang", Value: cookie})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Body.String()
	}
	body := get("/rules?weeks=12", "", "ko")
	// In Korean the hidden label is translated.
	menu := regexp.MustCompile(`(?s)<details class="picker end"><summary><span class="vh">언어: </span>.*?</details>`).FindString(body)
	if menu == "" {
		t.Fatal("no language picker")
	}
	for _, n := range names {
		if !strings.Contains(menu, `lang="`+n.Code+`" hreflang="`+n.Code+`"`) || !strings.Contains(menu, ">"+n.Name+"</a>") {
			t.Errorf("no %s in the picker", n.Code)
		}
	}
	if n := strings.Count(menu, ariaCurr); n != 1 {
		t.Errorf("%d current languages, not one", n)
	}
	if !strings.Contains(menu, `href="/lang?to=ko&amp;back=%2Frules%3Fweeks%3D12" lang="ko" hreflang="ko" aria-current="true"`) {
		t.Errorf("the chosen language isn't current: %s", menu)
	}
	if !strings.Contains(menu, "브라우저 언어") || strings.Contains(menu, `to=auto&amp;back=%2Frules%3Fweeks%3D12" aria-current`) {
		t.Errorf("Browser's language: %s", menu)
	}

	// Choosing the browser's language forgets the cookie, and goes back.
	r := httptest.NewRequest(http.MethodGet, "/lang?to=auto&back=/rules%3Fweeks%3D12", nil)
	r.AddCookie(&http.Cookie{Name: "lang", Value: "ko"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var gone bool
	for _, c := range w.Result().Cookies() {
		gone = gone || (c.Name == "lang" && c.MaxAge < 0 && c.Value == "" && c.Path == "/")
	}
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/rules?weeks=12" || !gone {
		t.Errorf("to=auto: %d %v", w.Code, w.Header())
	}
	r = httptest.NewRequest(http.MethodGet, "/lang?to=auto&back=//evil.example/", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Errorf("an unsafe back: %q", loc)
	}
	r = httptest.NewRequest(http.MethodGet, "/lang?to=xx", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("an unknown language: %d", w.Code)
	}

	// With no cookie, the browser's language is answered in, and is the
	// current choice; the language it picked is in the summary only.
	body = get("/rules", "ja", "")
	jmenu := regexp.MustCompile(`(?s)<details class="picker end"><summary><span class="vh">言語: </span>.*?</details>`).FindString(body)
	if !strings.Contains(body, `<html lang="ja">`) || jmenu == "" {
		t.Fatalf("not in Japanese: %s", jmenu)
	}
	if n := strings.Count(jmenu, ariaCurr); n != 1 || !strings.Contains(jmenu, `to=auto&amp;back=%2Frules" aria-current="true">ブラウザの言語`) {
		t.Errorf("Browser's language isn't the one current: %s", jmenu)
	}
	if !strings.Contains(jmenu, `<span class="picked"><span lang="ja">日本語</span></span>`) {
		t.Errorf("the language in use isn't in the summary: %s", jmenu)
	}
}
