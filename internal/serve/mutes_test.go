package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// allMutes are the mutes the ledger keeps.
func allMutes(t *testing.T, s *Server) []ledger.Mute {
	t.Helper()
	ms, err := s.l.Mutes(context.Background(), time.Unix(0, 0), now.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

// What real Alertmanagers told of a silence muting one alert of a group,
// 0.34.2 and 0.35.0, is kept: the silence's mutes, told of three times
// and five, a row each, with the silence's ID and nothing else of it; and
// 0.35.0's notification saying it was muted, by what it doesn't say.
// Sent again, nothing changes.
func TestEventRecorderMutes(t *testing.T) {
	parallel(t)
	for _, c := range []struct {
		file, silence string
		either        bool
	}{
		{"alertmanager-0.34.2-muted.jsonl", "c9839a90-d7ae-456f-b386-08e896968f39", false},
		{"alertmanager-0.35.0-muted.jsonl", "130fe9f1-ae00-42be-a860-cd7bc8a6507f", true},
	} {
		t.Run(c.file, func(t *testing.T) {
			s := notifyServer(t, nil)
			s.now = func() time.Time { return time.Date(2026, 11, 11, 17, 30, 0, 0, time.UTC) }
			h := s.NotifyHandler()
			bodies := recorded(t, c.file)
			for i, b := range bodies {
				if code, body, _ := send(t, h, "/v1/alertmanager/events", b, bearer(notifyToken)); code != http.StatusOK {
					t.Fatalf("request %d: %d %s", i+1, code, body)
				}
			}
			ms := allMutes(t, s)
			var silence, either int
			for _, m := range ms {
				if m.Rule != "DiskFull" || m.Labels["instance"] != "db-2" || m.Labels["alertname"] != "DiskFull" {
					t.Errorf("a mute of %s %v", m.Rule, m.Labels)
				}
				if m.At.Before(time.Date(2026, 11, 11, 16, 0, 0, 0, time.UTC)) || m.At.Equal(s.now()) {
					t.Errorf("a mute at %s, not when the recorder told of it", m.At)
				}
				switch m.Kind {
				case ledger.MutedSilence:
					silence++
					if m.Ref != c.silence || m.Last.Before(m.At) {
						t.Errorf("the silence's mute: %+v", m)
					}
				case ledger.MutedEither:
					either++
					if m.Ref != "" {
						t.Errorf("a notification's mute names %q", m.Ref)
					}
				default:
					t.Errorf("a mute by %s", m.Kind)
				}
			}
			if silence != 1 || either == 1 != c.either {
				t.Errorf("%d rows of the silence's mutes, %d of a notification's: %+v", silence, either, ms)
			}
			// Nothing of the silence but its ID: not its comment, nor who made it.
			b, _ := json.Marshal(ms)
			for _, no := range []string{"maintenance", "tester", `"m"`, "createdBy", "matchers"} {
				if strings.Contains(string(b), no) {
					t.Errorf("a mute keeps %s: %s", no, b)
				}
			}
			for _, b := range bodies {
				send(t, h, "/v1/alertmanager/events", b, bearer(notifyToken))
			}
			if again := allMutes(t, s); len(again) != len(ms) {
				t.Errorf("%d rows after sending them all again, not %d", len(again), len(ms))
			}
		})
	}
}

// One mute event, as 0.35 writes it, changed for each test.
func oneMute(kind string, change func(e map[string]any)) []byte {
	muted := map[string]any{"labels": map[string]any{"alertname": "Errors", "service": "search"}, "fingerprint": "7"}
	var data map[string]any
	if kind == "silence" {
		data = map[string]any{"silenceMutedAlert": map[string]any{
			"silence":    map[string]any{"id": "0b5e4a1c-3f2d-4e8a-9c71-2a6d5f0e9b13", "createdBy": "someone", "comment": "a window"},
			"mutedAlert": muted}}
	} else {
		data = map[string]any{"inhibitionMutedAlert": map[string]any{
			"inhibitRules":           []any{map[string]any{"name": "outage"}},
			"mutedAlert":             muted,
			"inhibitingFingerprints": []any{"18266735891077632309"}}}
	}
	e := map[string]any{"@timestamp": "2026-11-03T11:59:58.5Z", "instance": "am-0", "data": data}
	if change != nil {
		change(e)
	}
	b, _ := json.Marshal(e)
	return b
}

func muteOf(e map[string]any) map[string]any {
	for _, v := range e["data"].(map[string]any) {
		return v.(map[string]any)
	}
	return nil
}

// An inhibition's mute is kept by the first alert inhibiting it, in either
// format; what can't be read as the recorder's is refused, that event
// alone, as a notification's would be, the rest of a batch kept.
func TestEventRecorderMuteReads(t *testing.T) {
	parallel(t)
	s := notifyServer(t, nil)
	h := s.NotifyHandler()
	path := "/v1/alertmanager/events"
	// 0.34's labels, as pairs, and a fingerprint as a number.
	pairs := oneMute("inhibition", func(e map[string]any) {
		m := muteOf(e)
		m["mutedAlert"] = map[string]any{"labels": map[string]any{"labels": []any{
			map[string]any{"key": "alertname", "value": "Errors"}, map[string]any{"key": "service", "value": "cart"}}}}
		m["inhibitingFingerprints"] = []any{42, 43}
	})
	body := "[" + string(oneMute("inhibition", nil)) + "," + string(pairs) + "," + string(oneMute("silence", nil)) + "]"
	code, resp, _ := send(t, h, path, []byte(body), bearer(notifyToken))
	if code != http.StatusOK || !strings.Contains(resp, `"mutes":3`) {
		t.Fatalf("%d %s", code, resp)
	}
	got := map[string]string{}
	for _, m := range allMutes(t, s) {
		got[m.Kind+" "+m.Labels["service"]] = m.Ref
	}
	if got["inhibition search"] != "18266735891077632309" || got["inhibition cart"] != "42" || got["silence search"] != "0b5e4a1c-3f2d-4e8a-9c71-2a6d5f0e9b13" {
		t.Errorf("kept %v", got)
	}

	for name, c := range map[string]struct {
		body []byte
		want string
	}{
		"a silence with no ID":         {oneMute("silence", func(e map[string]any) { muteOf(e)["silence"] = map[string]any{} }), "silence's ID"},
		"no silence":                   {oneMute("silence", func(e map[string]any) { delete(muteOf(e), "silence") }), "silence's ID"},
		"an ID that isn't one":         {oneMute("silence", func(e map[string]any) { muteOf(e)["silence"] = map[string]any{"id": "<b>x</b>"} }), "silence's ID"},
		"an ID far too long":           {oneMute("silence", func(e map[string]any) { muteOf(e)["silence"] = map[string]any{"id": strings.Repeat("a", 65)} }), "silence's ID"},
		"no muted alert":               {oneMute("silence", func(e map[string]any) { delete(muteOf(e), "mutedAlert") }), "which alert"},
		"no alertname":                 {oneMute("inhibition", func(e map[string]any) { muteOf(e)["mutedAlert"] = map[string]any{"labels": map[string]any{"x": "y"}} }), "no alertname"},
		"a fingerprint that isn't one": {oneMute("inhibition", func(e map[string]any) { muteOf(e)["inhibitingFingerprints"] = []any{"-1"} }), "fingerprint"},
		"a label that isn't a string": {oneMute("inhibition", func(e map[string]any) {
			muteOf(e)["mutedAlert"] = map[string]any{"labels": map[string]any{"alertname": "Errors", "n": 3}}
		}), "isn't a string"},
		"labels over 4 KB": {oneMute("silence", func(e map[string]any) {
			muteOf(e)["mutedAlert"] = map[string]any{"labels": map[string]any{"alertname": "Errors", "note": strings.Repeat("x", 5000)}}
		}), "4 KB"},
		"a rule that doesn't show": {oneMute("silence", func(e map[string]any) {
			muteOf(e)["mutedAlert"] = map[string]any{"labels": map[string]any{"alertname": "Err​ors"}}
		}), "rule's name"},
	} {
		code, resp, _ := send(t, h, path, c.body, bearer(notifyToken))
		if code != http.StatusBadRequest || !strings.Contains(resp, c.want) {
			t.Errorf("%s: %d %q, want 400 saying %q", name, code, resp, c.want)
		}
	}
	// In a batch, one refused doesn't keep the rest out.
	bad := oneMute("silence", func(e map[string]any) { muteOf(e)["silence"] = map[string]any{} })
	other := oneMute("silence", func(e map[string]any) {
		muteOf(e)["mutedAlert"] = map[string]any{"labels": map[string]any{"alertname": "Errors", "service": "checkout"}}
	})
	code, resp, _ = send(t, h, path, []byte("["+string(bad)+","+string(other)+"]"), bearer(notifyToken))
	if code != http.StatusOK || !strings.Contains(resp, `"mutes":1`) || !strings.Contains(resp, `"refused":1`) {
		t.Errorf("a batch with one refused: %d %s", code, resp)
	}
	if n := len(allMutes(t, s)); n != 4 {
		t.Errorf("%d mutes kept, want 4", n)
	}
	// Told of again within a minute, a mute is kept already, and takes no
	// write; two minutes off, its row is longer.
	first := func() time.Time {
		for _, m := range allMutes(t, s) {
			if m.Kind == ledger.MutedSilence && m.Labels["service"] == "search" {
				return m.At
			}
		}
		return time.Time{}
	}
	was := first()
	for _, before := range []time.Duration{30 * time.Second, 2 * time.Minute} {
		b := oneMute("silence", func(e map[string]any) { e["@timestamp"] = was.Add(-before).Format(time.RFC3339Nano) })
		if code, resp, _ := send(t, h, path, b, bearer(notifyToken)); code != http.StatusOK || !strings.Contains(resp, `"mutes":1`) {
			t.Fatalf("%d %s", code, resp)
		}
		if want := map[bool]time.Time{true: was, false: was.Add(-before)}[before < muteSlack]; !first().Equal(want) {
			t.Errorf("told of again %s before: the row starts at %s, not %s", before, first(), want)
		}
	}
	// No silence's comment or maker is kept.
	if b, _ := json.Marshal(allMutes(t, s)); strings.Contains(string(b), "someone") || strings.Contains(string(b), "a window") || strings.Contains(string(b), "outage") {
		t.Errorf("kept %s", b)
	}
}

// Past the day's most, mutes are passed over, and a notification with
// them is still kept.
func TestEventRecorderMutesPerDay(t *testing.T) {
	was := maxMutesPerDay
	maxMutesPerDay = 1
	t.Cleanup(func() { maxMutesPerDay = was })
	s := notifyServer(t, nil)
	h := s.NotifyHandler()
	path := "/v1/alertmanager/events"
	send(t, h, path, oneMute("silence", nil), bearer(notifyToken))
	two := oneMute("silence", func(e map[string]any) {
		muteOf(e)["mutedAlert"] = map[string]any{"labels": map[string]any{"alertname": "Errors", "service": "cart"}}
	})
	if code, resp, _ := send(t, h, path, []byte("["+string(two)+","+string(oneEvent(nil))+"]"), bearer(notifyToken)); code != http.StatusOK || !strings.Contains(resp, `"kept":1`) {
		t.Fatalf("a delivery with a mute past the day's most: %d %s", code, resp)
	}
	if n := len(allMutes(t, s)); n != 1 {
		t.Errorf("%d mutes kept, want the day's 1", n)
	}
}

// mutedServer is a server whose three latest firings of Errors no
// notification told of: a silence muted the first, an inhibition the
// second, and a notification of the third's group said it was muted, by
// what it doesn't say.
func mutedServer(t *testing.T) (s *Server, silenced, inhibited, either ledger.Firing) {
	t.Helper()
	ctx := context.Background()
	s = notifyServer(t, nil)
	nh := s.NotifyHandler()
	fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: time.Unix(0, 0), To: now.Add(time.Hour), Rule: "Errors", Limit: 50})
	if err != nil || len(fs) < 3 {
		t.Fatalf("%d firings: %v", len(fs), err)
	}
	mute := func(f ledger.Firing, kind string, change func(e map[string]any)) {
		t.Helper()
		labels := map[string]any{"alertname": "Errors", "cluster": "eu-1"}
		for k, v := range f.Labels {
			labels[k] = v
		}
		b := oneMute(kind, func(e map[string]any) {
			e["@timestamp"] = f.Start.Add(time.Minute).Format(time.RFC3339Nano)
			muteOf(e)["mutedAlert"] = map[string]any{"labels": labels}
			if change != nil {
				change(e)
			}
		})
		s.now = func() time.Time { return f.Start.Add(10 * time.Minute) }
		if code, body, _ := send(t, nh, "/v1/alertmanager/events", b, bearer(notifyToken)); code != http.StatusOK {
			t.Fatalf("%d %s", code, body)
		}
	}
	silenced, inhibited, either = fs[0], fs[1], fs[2]
	mute(silenced, "silence", nil)
	mute(inhibited, "inhibition", nil)
	// A notification of the group of the third that delivered another
	// alert, and said this one was muted.
	b := oneEvent(func(e map[string]any) {
		e["@timestamp"] = either.Start.Add(time.Minute).Format(time.RFC3339Nano)
		labels := map[string]any{"alertname": "Errors", "cluster": "eu-1"}
		for k, v := range either.Labels {
			labels[k] = v
		}
		other := notif(e)["alerts"].([]any)[0]
		notif(e)["alerts"] = []any{other, map[string]any{"fingerprint": "99", "details": map[string]any{"name": "Errors", "labels": labels, "startsAt": either.Start.Format(time.RFC3339)}}}
		notif(e)["mutedAlerts"] = []any{map[string]any{"fingerprint": "99"}}
	})
	s.now = func() time.Time { return either.Start.Add(10 * time.Minute) }
	if code, body, _ := send(t, nh, "/v1/alertmanager/events", b, bearer(notifyToken)); code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	s.now = func() time.Time { return now }
	return s, silenced, inhibited, either
}

// A firing no notification told of says what muted it, where its
// notifications are shown: on the day's firings, the rule's page and MCP's
// rule. A silence by its ID, an inhibition, and a notification's word that
// doesn't say which; a firing nothing muted says what it said before. It
// shows with no notification kept at all.
func TestMutesShown(t *testing.T) {
	ctx := context.Background()
	s, silenced, inhibited, either := mutedServer(t)
	h := s.Handler()

	_, body, _ := get(t, h, "/rules/Errors?tab=firings")
	for _, want := range []string{"muted by a silence <code>0b5e4a1c-3f2d-4e8a-9c71-2a6d5f0e9b13</code>", "inhibited by another alert", "muted by a silence or an inhibition"} {
		if strings.Count(body, want) != 1 {
			t.Errorf("the rule's page says %q %d times: %s", want, strings.Count(body, want), body)
		}
	}
	day := silenced.Start.Format("2006-01-02")
	_, body, _ = get(t, h, "/firings?day="+day)
	if !strings.Contains(body, "muted by a silence <code>0b5e4a1c-3f2d-4e8a-9c71-2a6d5f0e9b13</code>") {
		t.Errorf("the day's firings: %s", body)
	}
	_, out, err := s.toolRule(ctx, nil, ruleIn{Name: "Errors"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range out.Latest {
		for _, m := range f.Muted {
			got[f.Start.String()] = m.By + " " + m.Silence + m.InhibitedBy
		}
	}
	if got[silenced.Start.String()] != "silence 0b5e4a1c-3f2d-4e8a-9c71-2a6d5f0e9b13" || got[inhibited.Start.String()] != "inhibition 18266735891077632309" ||
		got[either.Start.String()] != "either " || len(got) != 3 {
		t.Errorf("mcp: %v", got)
	}

	// In each language, every text found.
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	for _, lang := range []string{"ko", "ja", "es"} {
		for _, p := range []string{"/rules/Errors?tab=firings", "/firings?day=" + day} {
			r := httptest.NewRequest(http.MethodGet, p, nil)
			r.Header.Set("Accept-Language", lang)
			h.ServeHTTP(httptest.NewRecorder(), r)
		}
	}
	missing.Lock()
	for k := range missing.m {
		t.Errorf("no translation: %q", k)
	}
	missing.Unlock()
}

// The digest and the reports count the firings of the span that paged no
// one as their pages say it: those muted, by what muted them first, and
// those nothing was told of muting, of a rule notifications were counted
// for; of the rules they cover alone. Without a mute kept, they say
// nothing of it, as before. In each language.
func TestMutesCounted(t *testing.T) {
	ctx := context.Background()
	o := func(s *Server) verdict.Options {
		o, err := s.options(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	plain := notifyServer(t, nil)
	if code, body, _ := send(t, plain.NotifyHandler(), "/v1/alertmanager/events", oneEvent(nil), bearer(notifyToken)); code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	from := plain.l.Midnight(ledger.Week(plain.l.Day(now)))
	to := from.AddDate(0, 0, 7)
	rep, err := plain.ledgerReportFor(ctx, from, to, o(plain), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if x, err := plain.sent(ctx, rep, from, to); err != nil || x == nil || x.PagedNoOne != nil {
		t.Fatalf("no mute kept: %+v %v", x, err)
	}

	s, silenced, _, either := mutedServer(t)
	if from = s.l.Midnight(ledger.Week(s.l.Day(silenced.Start))); !from.Before(silenced.Start) {
		t.Fatal(from)
	}
	// A notification of the earliest of Errors' firings, the first to
	// come: those after it that none told of, and nothing muted, paged no
	// one.
	fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: from, To: either.Start, Rule: "Errors"})
	if err != nil || len(fs) < 2 {
		t.Fatalf("%d firings: %v", len(fs), err)
	}
	first := fs[len(fs)-1]
	b := oneEvent(func(e map[string]any) {
		e["@timestamp"] = first.Start.Add(time.Minute).Format(time.RFC3339Nano)
		labels := map[string]any{"alertname": "Errors", "cluster": "eu-1"}
		for k, v := range first.Labels {
			labels[k] = v
		}
		notif(e)["alerts"] = []any{map[string]any{"fingerprint": "7", "details": map[string]any{"name": "Errors", "labels": labels, "startsAt": first.Start.Format(time.RFC3339)}}}
		notif(e)["groupInfo"].(map[string]any)["groupKey"] = `{}:{alertname="Errors",service="search"}`
		notif(e)["flushId"] = "2"
	})
	s.now = func() time.Time { return first.Start.Add(10 * time.Minute) }
	if code, body, _ := send(t, s.NotifyHandler(), "/v1/alertmanager/events", b, bearer(notifyToken)); code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	s.now = func() time.Time { return now }
	to = from.AddDate(0, 0, 7)
	rep, err = s.ledgerReportFor(ctx, from, to, o(s), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	// What the pages say of each firing of the span.
	fs, err = s.l.Firings(ctx, ledger.FiringQuery{From: from, To: to})
	if err != nil {
		t.Fatal(err)
	}
	lines := make([]firingLine, len(fs))
	for i, f := range fs {
		lines[i].Firing = f
	}
	if err := s.notifiedFirings(ctx, lines); err != nil {
		t.Fatal(err)
	}
	var want ledger.Unpaged
	for _, l := range lines {
		switch {
		case l.Notified > 0:
		case len(l.Muted) > 0 && l.Muted[0].Kind == ledger.MutedSilence:
			want.Silence++
		case len(l.Muted) > 0 && l.Muted[0].Kind == ledger.MutedInhibition:
			want.Inhibition++
		case len(l.Muted) > 0:
			want.Either++
		case l.Counted:
			want.Nothing++
		}
	}
	if want.Silence != 1 || want.Inhibition != 1 || want.Either != 1 || want.Nothing == 0 {
		t.Fatalf("the pages: %+v", want)
	}
	x, err := s.sent(ctx, rep, from, to)
	if err != nil || x == nil || x.PagedNoOne == nil || *x.PagedNoOne != want {
		t.Fatalf("counted %+v, want %+v (%v)", x, want, err)
	}
	line := fmt.Sprintf("Firings that paged no one: 1 muted by a silence, 1 inhibited by another alert, 1 muted by a silence or an inhibition, %d with no mute told of.", want.Nothing)
	if got := x.text("en"); !strings.Contains(got, line) {
		t.Errorf("the line: %s", got)
	}
	// As the webhook and a report's numbers have it.
	about := map[string]any{}
	if err := s.aboutMore(ctx, about, rep, from, to, nil, x); err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(about["notifications"])
	if !strings.Contains(string(b), fmt.Sprintf(`"paged_no_one":{"silence":1,"inhibition":1,"either":1,"nothing":%d}`, want.Nothing)) {
		t.Errorf("digest.ready: %s", b)
	}
	// A report of rules other than Errors counts none of its.
	other := *rep
	other.Rules = slices.DeleteFunc(slices.Clone(rep.Rules), func(r verdict.RuleResult) bool { return r.Rule == "Errors" })
	if x, err := s.sent(ctx, &other, from, to); err != nil || x.PagedNoOne == nil || x.PagedNoOne.Silence+x.PagedNoOne.Inhibition+x.PagedNoOne.Either != 0 {
		t.Errorf("without Errors: %+v %v", x, err)
	}
	// The report's section and the digest say it.
	r := ledger.Scheduled{Name: "weekly", Schedule: "weekly mon@09:00", Sections: []string{"notifications"}}
	sch, err := r.When()
	if err != nil {
		t.Fatal(err)
	}
	built, err := s.buildReport(ctx, r, nil, sch, time.UTC, to, from, to, o(s), to, to)
	if err != nil || !strings.Contains(built.Text, line) {
		t.Errorf("the report: %v\n%s", err, built.Text)
	}
	digest, err := s.digestText(ctx, rep, nil, from, to, nil)
	if err != nil || !strings.Contains(digest, line) {
		t.Errorf("the digest: %v\n%s", err, digest)
	}

	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	partial := *x.PagedNoOne
	partial.Partial = true
	x.PagedNoOne = &partial
	for _, lang := range []string{"ko", "ja", "es"} {
		x.text(lang)
		x.PagedNoOne.Either = 0
		x.text(lang)
		x.PagedNoOne.Either = 1
	}
	missing.Lock()
	for k := range missing.m {
		t.Errorf("no translation: %q", k)
	}
	missing.Unlock()
}

// A digest reads what the notifications came to, and the firings that
// paged no one, once: for its text and what the webhook is told alike,
// everyone's and each team's.
func TestDigestReadsSentOnce(t *testing.T) {
	ctx := context.Background()
	s, silenced, _, _ := mutedServer(t)
	hook := filepath.Join(t.TempDir(), "hook")
	if err := os.WriteFile(hook, []byte("http://127.0.0.1:1/x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.NotifyWebhook = hook
	s.l.TellTo(s.dests())
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "search", Rules: "^Errors$"}, "ana"); err != nil {
		t.Fatal(err)
	}
	o, err := s.options(ctx)
	if err != nil {
		t.Fatal(err)
	}
	from := s.l.Midnight(ledger.Week(s.l.Day(silenced.Start)))
	to := from.AddDate(0, 0, 7)
	was := sentRead.Load()
	if err := s.tellDigest(ctx, digestIn{On: true, Dests: []string{"webhook"}}, "w", true, from, to, o); err != nil {
		t.Fatal(err)
	}
	if n := sentRead.Load() - was; n != 2 {
		t.Errorf("worked out what the notifications came to %d times for two digests, everyone's and search's", n)
	}
	due, err := s.l.Due(ctx, "webhook", time.Now(), 10)
	if err != nil || len(due) != 2 {
		t.Fatalf("%d told: %v", len(due), err)
	}
	for _, d := range due {
		if !strings.Contains(string(d.Body), `"paged_no_one":{"silence":1,`) || !strings.Contains(string(d.Body), "Firings that paged no one: 1 muted by a silence") {
			t.Errorf("told without what paged no one: %s", d.Body)
		}
	}
}

// A report whose span starts before the twelve weeks the notifications are
// read for says from when its notifications, and the firings that paged
// no one, are counted, in its zone, and its numbers as counted_from; one
// that ends before them says none of its are counted; one within them
// says nothing of it. In each language.
func TestNotificationsCountedFrom(t *testing.T) {
	ctx := context.Background()
	s, silenced, _, _ := mutedServer(t)
	o, err := s.options(ctx)
	if err != nil {
		t.Fatal(err)
	}
	read := s.shownFrom()
	weekly := ledger.Scheduled{Name: "weekly", Schedule: "weekly mon@09:00", Sections: []string{"notifications"}}
	sch, err := weekly.When()
	if err != nil {
		t.Fatal(err)
	}
	report := func(from, to time.Time, loc *time.Location) *built {
		t.Helper()
		b, err := s.buildReport(ctx, weekly, nil, sch, loc, to, from, to, o, to, to)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	end := s.l.Midnight(ledger.Week(s.l.Day(silenced.Start))).AddDate(0, 0, 7)
	if !read.Before(silenced.Start) {
		t.Fatalf("read from %s, after the firings", read)
	}
	seoul, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Fatal(err)
	}
	long := report(read.AddDate(0, 0, -28), end, seoul)
	since := "Counted from " + read.In(seoul).Format("2006-01-02 15:04") + " on: notifications are read for the last twelve weeks."
	if !strings.Contains(long.Text, "Firings that paged no one: 1 muted by a silence") || !strings.Contains(long.Text, since) {
		t.Errorf("a span from before the twelve weeks: %s", long.Text)
	}
	b, _ := json.Marshal(long.Numbers["notifications"])
	if !strings.Contains(string(b), `"counted_from":"`+read.Format(time.RFC3339)+`"`) {
		t.Errorf("its numbers: %s", b)
	}
	within := report(read, end, time.UTC)
	b, _ = json.Marshal(within.Numbers["notifications"])
	if strings.Contains(within.Text, "Counted from") || strings.Contains(string(b), "counted_from") || !strings.Contains(within.Text, "Firings that paged no one") {
		t.Errorf("a span within the twelve weeks: %s %s", within.Text, b)
	}

	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	x, err := s.sent(ctx, &verdict.Report{}, read.AddDate(0, 0, -28), end)
	if err != nil || x == nil || x.CountedFrom.IsZero() {
		t.Fatalf("%+v %v", x, err)
	}
	// Twenty weeks on, the twelve weeks read start after every notification
	// kept: a span that ends as they start has none counted, though some
	// were taken before it ended.
	s.now = func() time.Time { return now.AddDate(0, 0, 140) }
	later := s.shownFrom()
	before := report(later.AddDate(0, 0, -28), later, time.UTC)
	if want := "Notifications are read for the last twelve weeks, from " + later.UTC().Format("2006-01-02 15:04") + " on: none of the span's are counted."; !strings.Contains(before.Text, want) ||
		strings.Contains(before.Text, "No notifications were taken") {
		t.Errorf("a span before the twelve weeks: %s", before.Text)
	}
	y, err := s.sent(ctx, &verdict.Report{}, later.AddDate(0, 0, -28), later)
	if err != nil || y == nil || !y.before {
		t.Fatalf("%+v %v", y, err)
	}
	for _, lang := range []string{"ko", "ja", "es"} {
		x.text(lang)
		y.text(lang)
	}
	missing.Lock()
	for k := range missing.m {
		t.Errorf("no translation: %q", k)
	}
	missing.Unlock()
}
