package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
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

// A firing no notification told of says what muted it, where its
// notifications are shown: on the day's firings, the rule's page and MCP's
// rule. A silence by its ID, an inhibition, and a notification's word that
// doesn't say which; a firing nothing muted says what it said before. It
// shows with no notification kept at all.
func TestMutesShown(t *testing.T) {
	ctx := context.Background()
	s := notifyServer(t, nil)
	h, nh := s.Handler(), s.NotifyHandler()
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
	silenced, inhibited, either := fs[0], fs[1], fs[2]
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
