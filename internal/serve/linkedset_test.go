package serve

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// --linked-only is a setting an admin changes on the Settings page and
// through the API (POST /settings): the flag defines it until then, and a
// change judges every firing again once, as a start with the flag changed
// does; the activity log says who changed it; settings.yaml gives it as the
// chart's linkedOnly; a form that holds only it leaves the other settings
// alone.
func TestLinkedOnlySetInTheUI(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, nil)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	h := s.Handler()
	n := 0
	was := judgeAll
	judgeAll = func(l *ledger.Ledger, ctx context.Context, o verdict.Options, done func(float64)) error {
		n++
		return was(l, ctx, o, done)
	}
	t.Cleanup(func() { judgeAll = was })
	on := func() bool {
		o, err := s.options(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return o.LinkedOnly
	}
	save := func(form url.Values) {
		t.Helper()
		if code, body, _ := post(t, h, "/settings", form, "same-origin"); code != http.StatusSeeOther {
			t.Fatalf("POST /settings %v: %d %s", form, code, body)
		}
	}
	// The flag defines it.
	if on() {
		t.Fatal("on with the flag off")
	}
	_, page, _ := get(t, h, "/settings")
	if !strings.Contains(page, `<td><code id="setting-linked-only">--linked-only</code></td><td>off</td>`) || !strings.Contains(page, `name="linked-only" value="off"`) {
		t.Errorf("the row of --linked-only:\n%s", page)
	}
	// Another setting is changed first: --linked-only stays as it is.
	save(url.Values{"lead": {"20m"}})
	if set, _ := s.l.Settings(ctx); set["lead"].Value != "20m" || on() {
		t.Errorf("a form of lead alone: %+v", set)
	}
	// An admin sets it, through the API's form of it alone: lead stays.
	save(url.Values{"linked-only": {"on"}})
	if set, _ := s.l.Settings(ctx); set["linked-only"].Value != "on" || set["linked-only"].By == "" || set["lead"].Value != "20m" || !on() {
		t.Fatalf("set: %+v", set)
	}
	// Judged again once, and the Settings page says so until it is done.
	_, js, _ := get(t, h, "/api/v1/settings")
	if !strings.Contains(js, `"linked_only": true`) || !strings.Contains(js, `"linked_only_judging": true`) {
		t.Errorf("settings API after the change: %s", js)
	}
	base := n
	s.jobs(ctx, false)
	s.jobs(ctx, false)
	if v, err := s.l.Meta(ctx, linkedMeta); n != base+1 || v != judgedBy+"1" || err != nil {
		t.Errorf("judged all %d times more, the ledger says %q (%v); want once", n-base, v, err)
	}
	if _, js, _ = get(t, h, "/api/v1/settings"); !strings.Contains(js, `"linked_only_judging": false`) {
		t.Errorf("judged, the API still says judging: %s", js)
	}
	// Who changed it: the activity log.
	set, _ := s.l.Settings(ctx)
	_, act, _ := get(t, h, "/api/v1/activity?kind=setting")
	if !strings.Contains(act, `"what":"setting linked-only"`) || !strings.Contains(act, `"by":"`+set["linked-only"].By+`"`) {
		t.Errorf("the activity log:\n%s", act)
	}
	// As Helm values.
	if _, y, _ := get(t, h, "/settings.yaml"); !strings.Contains(y, "linkedOnly: true\n") {
		t.Errorf("settings.yaml:\n%s", y)
	}
	// A word that isn't on or off is refused, and nothing is kept.
	if code, _, _ := post(t, h, "/settings", url.Values{"linked-only": {"maybe"}}, "same-origin"); code != http.StatusBadRequest {
		t.Errorf("maybe: %d", code)
	}
	// Off, then back to the flag's: each judges again once.
	save(url.Values{"linked-only": {"off"}})
	if on() {
		t.Error("still on")
	}
	s.jobs(ctx, false)
	save(url.Values{"linked-only": {"on"}, "reset-linked-only": {"1"}})
	if set, _ := s.l.Settings(ctx); set["linked-only"].Value != "" || on() {
		t.Errorf("back to the flag: %+v", set)
	}
	s.jobs(ctx, false)
	if v, _ := s.l.Meta(ctx, linkedMeta); v != judgedBy {
		t.Errorf("the ledger says %q after the flag's", v)
	}
	// The flag on, the UI turns it off: the UI's stands over the flag.
	s.cfg.Options.LinkedOnly = true
	if !on() {
		t.Fatal("the flag given: not on")
	}
	save(url.Values{"linked-only": {"off"}})
	if on() {
		t.Error("the UI's off doesn't stand over the flag")
	}
	if code, _, _ := post(t, h, "/settings", url.Values{"linked-only": {"on"}}, "cross-site"); code == http.StatusSeeOther {
		t.Error("a cross-site form changed it")
	}
}
