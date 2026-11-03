package serve

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"lapilli.dev/internal/ledger"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var verb = regexp.MustCompile(`%(?:\[\d+\])?[-+# 0]*\d*(?:\.\d*)?(?:\[\d+\])?[a-zA-Z%]`)

// verbs are a text's verbs by the argument each takes, ignoring order.
func verbs(s string) []string {
	var out []string
	for _, v := range verb.FindAllString(s, -1) {
		if v == "%%" {
			continue
		}
		out = append(out, v[len(v)-1:])
	}
	slices.Sort(out)
	return out
}

// Every translation takes the same arguments as its English, and gives
// back what it is given.
func TestCatalogs(t *testing.T) {
	for lang, m := range catalogs {
		for en, x := range m {
			if a, b := verbs(en), verbs(x); !slices.Equal(a, b) {
				t.Errorf("%s: %q has %v, its translation %q has %v", lang, en, a, x, b)
			}
			if strings.Contains(fmt.Sprintf(x, 1, 2, 3, 4, 5, 6), "%!") && len(verbs(x)) > 0 {
				args := make([]any, 0, len(verbs(en)))
				for _, v := range verb.FindAllString(en, -1) {
					switch v[len(v)-1] {
					case 'd':
						args = append(args, 1)
					case 'f':
						args = append(args, 1.0)
					case 's', 'q', 'v':
						args = append(args, "x")
					}
				}
				if got := fmt.Sprintf(x, args...); strings.Contains(got, "%!") {
					t.Errorf("%s: %q formats as %q", lang, x, got)
				}
			}
		}
	}
}

// Every page in each language finds a translation for every text it
// shows, says which language it is in, and offers the others.
func TestPagesInEachLanguage(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, nil)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	o := s.cfg.Options
	tid, err := s.l.PutTeam(ctx, ledger.Team{Name: "payments", Rules: "Errors"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.l.PutNote(ctx, ledger.Note{Rule: "Errors", TeamID: tid, Status: "tune", Runbook: "https://x.example", Note: "n"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutWindow(ctx, ledger.Window{Title: "w", Start: now.Add(-50 * time.Hour), End: now.Add(-30 * time.Hour)}, "ana", o); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutTask(ctx, ledger.Task{Title: "t", Kind: "tune", Rule: "Errors", TeamID: tid, Due: now}, "ana"); err != nil {
		t.Fatal(err)
	}
	for _, on := range [][2]string{{"rule", "Errors"}, {"task", "1"}, {"incident", "INC-1"}} {
		if _, err := s.l.PutComment(ctx, ledger.Comment{On: on[0], Key: on[1], Body: "c"}, "anonymous"); err != nil {
			t.Fatal(err)
		}
	}
	s.status.set("replaying %s → %s", "a", "b")
	s.fail("a run", fmt.Errorf("boom"))
	h := s.Handler()
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	paths := []string{"/", "/rules", "/rules?weeks=1", "/rules/Errors", "/incidents", "/incidents/INC-1", "/incidents/impact-20261103T0600Z",
		"/firings?day=2026-11-02", "/settings", "/settings?try=1&lead=1h", "/runs", "/incidents/none",
		"/teams", "/teams?edit=1", "/windows", "/windows/1", "/tasks", "/tasks?status=all", "/tasks/1", "/?team=payments", "/rules?team=payments"}
	for _, n := range names {
		for _, p := range paths {
			r := httptest.NewRequest(http.MethodGet, p, nil)
			r.Header.Set("Accept-Language", n.Code+";q=0.9, de;q=0.5")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if !strings.Contains(w.Body.String(), `<html lang="`+n.Code+`">`) {
				t.Errorf("%s %s: %d, not in %s", n.Code, p, w.Code, n.Code)
			}
		}
	}
	missing.Lock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		t.Errorf("%s has no translation for %q", lang, msg)
	}
	missing.Unlock()

	// The language chosen on the page wins, and is kept.
	r := httptest.NewRequest(http.MethodGet, "/lang?to=ko&back=/rules%3Fweeks%3D12", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/rules?weeks=12" || !strings.Contains(w.Header().Get("Set-Cookie"), "lang=ko") {
		t.Errorf("choosing ko: %d %v", w.Code, w.Header())
	}
	r = httptest.NewRequest(http.MethodGet, "/rules", nil)
	r.Header.Set("Accept-Language", "en")
	r.AddCookie(&http.Cookie{Name: "lang", Value: "ko"})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "규칙") {
		t.Error("the cookie should win over the browser's language")
	}
	// Never back to another site.
	for _, back := range []string{"//evil.example/x", "https://evil.example/", "/\\evil.example", "javascript:alert(1)"} {
		r := httptest.NewRequest(http.MethodGet, "/lang?to=en&back="+back, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/") || strings.HasPrefix(loc, "//") || strings.Contains(loc, "evil") {
			t.Errorf("back=%s went to %q", back, loc)
		}
	}
}

var messages = regexp.MustCompile(`(?:\b(?:invalid|bad|checkText)\(|\btr\([\w.]+,)\s*"((?:[^"\\]|\\.)*)"`)

// Every message a write can be refused with, kept apart from its values,
// and every line told to Slack, has a translation: they are shown only when
// something is refused or told, so rendering the pages doesn't find them.
func TestRefusalsTranslated(t *testing.T) {
	for _, dir := range []string{".", "../ledger"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range messages.FindAllStringSubmatch(string(b), -1) {
				msg, err := strconv.Unquote(`"` + m[1] + `"`)
				if err != nil {
					t.Fatal(err)
				}
				for lang, c := range catalogs {
					if _, ok := c[msg]; !ok {
						t.Errorf("%s: %s has no translation for %q", f, lang, msg)
					}
				}
			}
		}
	}
}
