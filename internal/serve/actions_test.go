package serve

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"lapilli.dev/internal/ledger"
)

// TestOverviewTaskDisabled: "Make it a task", under What to look at, is a
// form for whoever may make the task, and for anyone else the button
// disabled, with why: a team's editor makes tasks of their team's rules
// and none of another's, nor about an incident; those disabled for one
// reason share the sentence that says it.
func TestOverviewTaskDisabled(t *testing.T) {
	ctx := context.Background()
	s, _, search := bulkServer(t, nil)
	if _, err := s.l.PutGrant(ctx, ledger.Grant{Who: "user:sam@example.com", TeamID: search}, "boss"); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	const rule, incident = `<input type="hidden" name="rule" value="Errors">`, `<input type="hidden" name="kind" value="write">`
	for _, c := range []struct {
		who  string
		form []string // the actions' forms there are
		why  []string // the sentences said, once each
	}{
		{"ed@example.com", []string{rule, incident}, nil},
		{"pat@example.com", []string{rule}, []string{"Editors only"}},
		{"sam@example.com", nil, []string{"Only <bdi>payments</bdi>&#39;s editors", "Editors only"}},
		{"vi@example.com", nil, []string{"Editors only"}},
		{"", nil, []string{"Read-only: no one is signed in"}},
	} {
		_, body := getAs(t, h, "/?team=", c.who)
		i := strings.Index(body, "What to look at")
		j := strings.Index(body[i:], "</section>")
		look := body[i : i+j]
		for _, f := range []string{rule, incident} {
			has := strings.Contains(look, f)
			if want := strings.Contains(strings.Join(c.form, ""), f); has != want {
				t.Errorf("%q: the form with %s: %v", c.who, f, has)
			}
		}
		disabled := strings.Count(look, `type="button" disabled aria-describedby="look-why-`)
		if want := 2 - len(c.form); disabled != want {
			t.Errorf("%q: %d disabled, not %d", c.who, disabled, want)
		}
		for k, w := range c.why {
			id := `<span id="look-why-` + string(rune('1'+k)) + `" class="why">` + w + `</span>`
			if strings.Count(look, id) != 1 {
				t.Errorf("%q: not once: %s\n%s", c.who, id, look)
			}
		}
		if n := strings.Count(look, `class="why"`); n != len(c.why) {
			t.Errorf("%q: %d reasons said, not %d", c.who, n, len(c.why))
		}
	}
	// What is pressed still goes through the task's own checks.
	if code, _, _ := postAs(t, h, "/tasks", url.Values{"title": {"x"}, "kind": {"tune"}, "rule": {"Errors"}}, "sam@example.com", "same-origin"); code != 403 {
		t.Errorf("another team's rule's task: %d", code)
	}
}
