package serve

// The bar's right end: the language picker and who is signed in, on every
// page (docs/design/ui.md).

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

var (
	barEnd    = regexp.MustCompile(`(?s)<div class="bar-end">.*?\n</div>`)
	whoMenu   = regexp.MustCompile(`(?s)<details class="picker end"><summary><span class="vh">Signed in as </span>.*?</details>`)
	footerRe  = regexp.MustCompile(`(?s)<footer>.*?</footer>`)
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
	if !whoMenu.MatchString(body) {
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
