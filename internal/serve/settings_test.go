package serve

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"lapilli.dev/internal/ledger"
)

// A try of the settings on the last week, which judges seven days again in
// the heavy turn, is an admin's, from this site: a viewer's or an
// editor's is refused, and an admin's from another site, before anything
// is judged; the page offers it to an admin alone.
func TestSettingsTryAdminsFromHere(t *testing.T) {
	s := criteriaServer(t, true, nil)
	h := s.Handler()
	try := func(refire, who, site string) (int, string) {
		t.Helper()
		q := url.Values{"try": {"1"}, "refire": {refire}}
		return askAs(t, h, http.MethodGet, "/settings?"+q.Encode(), nil, who, site)
	}
	for _, c := range []struct {
		name, who, site string
		code            int
	}{
		{"a viewer", "carl@example.com", "same-origin", http.StatusForbidden},
		{"a viewer, from another site", "carl@example.com", "cross-site", http.StatusForbidden},
		{"an editor", "bo@example.com", "same-origin", http.StatusForbidden},
		{"an admin, from another site", "ana@example.com", "cross-site", http.StatusBadRequest},
		{"an admin, from a client that says nothing", "ana@example.com", "", http.StatusBadRequest},
	} {
		code, body := try("2h", c.who, c.site)
		if code != c.code || strings.Contains(body, "Tried on") {
			t.Errorf("%s: %d, want %d; tried %v", c.name, code, c.code, strings.Contains(body, "Tried on"))
		}
	}
	if len(s.settingsTries.at) != 0 {
		t.Errorf("tries refused before they were counted were counted: %v", s.settingsTries.at)
	}
	if code, body := try("3h", "ana@example.com", "same-origin"); code != http.StatusOK || !strings.Contains(body, "Tried on") {
		t.Errorf("an admin, from the Settings page: %d, tried %v", code, strings.Contains(body, "Tried on"))
	}
	for who, offered := range map[string]bool{"carl@example.com": false, "bo@example.com": false, "ana@example.com": true} {
		_, page := askAs(t, h, http.MethodGet, "/settings", nil, who, "same-origin")
		if got := strings.Contains(page, `name="try"`); got != offered {
			t.Errorf("%s is offered the try: %v, want %v", who, got, offered)
		}
	}
}

// An admin tries the settings settingsTriesAnHour times an hour, made or
// refused for a value that doesn't read, past which they are refused: and
// the tries of the incident criteria, which read Prometheus, are their own.
func TestSettingsTriesCounted(t *testing.T) {
	s := criteriaServer(t, true, nil)
	h := s.Handler()
	for i := range settingsTriesAnHour + 1 {
		q := url.Values{"try": {"1"}, "refire": {fmt.Sprint(i+2, "h")}}
		code, body := askAs(t, h, http.MethodGet, "/settings?"+q.Encode(), nil, "ana@example.com", "same-origin")
		switch {
		case i < settingsTriesAnHour && code != http.StatusOK:
			t.Fatalf("try %d: %d", i+1, code)
		case i == settingsTriesAnHour && (code != http.StatusBadRequest || !strings.Contains(body, "as many times in the last hour")):
			t.Fatalf("try %d, past the most: %d %s", i+1, code, body)
		}
	}
	q := url.Values{"impact-try": {"1"}, "impact": {"impact > 0"}}
	if code, body := askAs(t, h, http.MethodGet, "/settings?"+q.Encode(), nil, "ana@example.com", "same-origin"); code != http.StatusOK || strings.Contains(body, "as many incident criteria") {
		t.Errorf("a try of the incident criteria after the settings' were used up: %d", code)
	}
}

// The Settings page's changes are the settings' own, asked for apart from
// the others: a hundred and more notes an editor writes after an admin
// changed a setting don't push that change off the page.
func TestSettingsChangesTheirOwn(t *testing.T) {
	s, _ := server(t, nil)
	ctx := context.Background()
	if err := s.l.SetSettings(ctx, map[string]string{"lead": "20m"}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	for i := range 120 {
		if err := s.l.PutNote(ctx, ledger.Note{Rule: "Errors", Note: fmt.Sprint("note ", i)}, "bo@example.com"); err != nil {
			t.Fatal(err)
		}
	}
	v, err := s.settingsView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Changes) != 1 || v.Changes[0].What != "setting lead" || v.Changes[0].By != "ana@example.com" {
		t.Errorf("the Settings page's changes: %+v, want the admin's change of lead", v.Changes)
	}
}
