package serve

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// recorded are the bodies a real Alertmanager's event recorder posted, a
// request a line, in the order they came (testdata/events/README.md).
func recorded(t *testing.T, name string) [][]byte {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "events", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		out = append(out, bytes.Clone(sc.Bytes()))
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// notificationEvents counts the notification events in what was recorded.
func notificationEvents(t *testing.T, bodies [][]byte) int {
	t.Helper()
	n := 0
	for _, b := range bodies {
		var events []map[string]any
		if b[0] == '[' {
			json.Unmarshal(b, &events)
		} else {
			var e map[string]any
			json.Unmarshal(b, &e)
			events = append(events, e)
		}
		for _, e := range events {
			if d, _ := e["data"].(map[string]any); d["notification"] != nil {
				n++
			}
		}
	}
	return n
}

// What three versions of a real Alertmanager recorded is kept: each
// delivery, PagerDuty's and Slack's as well as a webhook's, once, with its
// reason, its alerts' starts and whether each had resolved; every other
// event is answered and passed over.
func TestEventRecorder(t *testing.T) {
	for _, file := range []string{"alertmanager-0.33.1.jsonl", "alertmanager-0.34.2.jsonl", "alertmanager-0.35.0.jsonl", "alertmanager-0.35.0-batch.jsonl"} {
		t.Run(file, func(t *testing.T) {
			s := notifyServer(t, nil)
			s.now = func() time.Time { return time.Date(2026, 11, 11, 16, 0, 0, 0, time.UTC) }
			h := s.NotifyHandler()
			bodies := recorded(t, file)
			want := notificationEvents(t, bodies)
			if want < 9 {
				t.Fatalf("the recording has %d notification events", want)
			}
			kept := 0
			for i, b := range bodies {
				code, body, _ := send(t, h, "/v1/alertmanager/events", b, bearer(notifyToken))
				if code != http.StatusOK {
					t.Fatalf("request %d: %d %s", i+1, code, body)
				}
				var got struct{ Kept int }
				json.Unmarshal([]byte(body), &got)
				kept += got.Kept
			}
			if kept != want {
				t.Errorf("kept %d notifications of %d delivered", kept, want)
			}
			ns, err := s.l.Notifications(t.Context(), time.Unix(0, 0), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatal(err)
			}
			byReceiver := map[string]int{}
			reasons := map[string]int{}
			for _, n := range ns {
				if n.Source != "alertmanager-events" {
					t.Fatalf("kept under %q", n.Source)
				}
				byReceiver[n.Receiver]++
				reasons[n.Reason]++
				if n.At.Before(time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC)) || n.At.Equal(s.now()) {
					t.Errorf("a notification at %s, not when the recorder says it was delivered", n.At)
				}
				for _, a := range n.Alerts {
					if a.Rule == "" || a.Start.IsZero() || a.Labels["alertname"] != a.Rule || a.Labels["service"] == "" {
						t.Errorf("an alert read as %+v", a)
					}
				}
			}
			for _, r := range []string{"oncall/pagerduty", "oncall/slack", "team-web/webhook"} {
				if byReceiver[r] == 0 {
					t.Errorf("nothing kept for %s: %v", r, byReceiver)
				}
			}
			for _, r := range []string{ledger.ReasonFirst, ledger.ReasonNew, ledger.ReasonRepeat, ledger.ReasonResolved} {
				if reasons[r] == 0 {
					t.Errorf("no notification kept as %s: %v", r, reasons)
				}
			}
			// The recorder tries again after a failure: the same again is
			// the same deliveries.
			for _, b := range bodies {
				_, body, _ := send(t, h, "/v1/alertmanager/events", b, bearer(notifyToken))
				if strings.Contains(body, `"kept":1`) || strings.Contains(body, `"kept":2`) {
					t.Fatalf("a delivery kept twice: %s", body)
				}
			}
			if again := kept2(t, s); again != len(ns) {
				t.Errorf("%d notifications after sending them all again, not %d", again, len(ns))
			}
		})
	}
}

func kept2(t *testing.T, s *Server) int {
	t.Helper()
	ns, err := s.l.Notifications(t.Context(), time.Unix(0, 0), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return len(ns)
}

// One delivery, as 0.35 writes it, changed for each test.
func oneEvent(change func(e map[string]any)) []byte {
	e := map[string]any{
		"@timestamp": "2026-11-03T11:59:58.5Z",
		"instance":   "am-0",
		"data": map[string]any{"notification": map[string]any{
			"alerts": []any{map[string]any{"fingerprint": "1", "details": map[string]any{
				"name": "Errors", "labels": map[string]any{"alertname": "Errors", "service": "checkout"},
				"startsAt": "2026-11-02T06:02:00Z"}}},
			"groupInfo":   map[string]any{"groupKey": `{}:{alertname="Errors"}`, "receiverName": "oncall", "groupUuid": "u-1"},
			"reason":      "NOTIFY_REASON_FIRST_NOTIFICATION",
			"flushId":     "1",
			"integration": map[string]any{"name": "pagerduty"},
		}},
	}
	if change != nil {
		change(e)
	}
	b, _ := json.Marshal(e)
	return b
}

func notif(e map[string]any) map[string]any {
	return e["data"].(map[string]any)["notification"].(map[string]any)
}

func alert0(e map[string]any) map[string]any {
	return notif(e)["alerts"].([]any)[0].(map[string]any)["details"].(map[string]any)
}

// Each refusal of the events path, as an attacker would try it: the body
// isn't read before the token, and what can't be read is refused rather
// than kept as something else.
func TestEventRecorderRefuses(t *testing.T) {
	s := notifyServer(t, nil)
	h := s.NotifyHandler()
	path := "/v1/alertmanager/events"

	for name, hdr := range map[string]map[string]string{
		"no token":          nil,
		"a wrong token":     bearer("a-wrong-token-long-enough"),
		"basic, not bearer": basicAuth("x", notifyToken),
	} {
		if code, _, hd := send(t, h, path, oneEvent(nil), hdr); code != http.StatusUnauthorized || hd.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s: %d", name, code)
		}
	}
	// The body isn't read before the token: one larger than taken is
	// refused as unauthorised, not as too large.
	big := bytes.Repeat([]byte(" "), notifyMax+10)
	if code, _, _ := send(t, h, path, big, nil); code != http.StatusUnauthorized {
		t.Errorf("a large body without the token: %d", code)
	}
	if code, _, _ := send(t, h, path, big, bearer(notifyToken)); code != http.StatusRequestEntityTooLarge {
		t.Errorf("a body over 1 MB: %d", code)
	}
	many := "[" + strings.TrimSuffix(strings.Repeat(`{"@timestamp":"2026-11-03T11:00:00Z","data":{}},`, maxEvents+1), ",") + "]"
	if code, body, _ := send(t, h, path, []byte(many), bearer(notifyToken)); code != http.StatusBadRequest || !strings.Contains(body, "at most") {
		t.Errorf("%d events: %d %s", maxEvents+1, code, body)
	}
	for name, c := range map[string]struct {
		body []byte
		want string
	}{
		"not JSON":                     {[]byte("hello"), "not the event recorder's JSON"},
		"a number":                     {[]byte("42"), "not the event recorder's JSON"},
		"no timestamp":                 {oneEvent(func(e map[string]any) { delete(e, "@timestamp") }), "no @timestamp"},
		"no receiver":                  {oneEvent(func(e map[string]any) { notif(e)["groupInfo"].(map[string]any)["receiverName"] = "" }), "no receiver"},
		"no integration":               {oneEvent(func(e map[string]any) { notif(e)["integration"] = map[string]any{} }), "integration"},
		"an integration with a /":      {oneEvent(func(e map[string]any) { notif(e)["integration"] = map[string]any{"name": "a/b"} }), "integration"},
		"an index that isn't one":      {oneEvent(func(e map[string]any) { notif(e)["integration"] = map[string]any{"name": "slack", "index": "x"} }), "not the event recorder's JSON"},
		"an index far too large":       {oneEvent(func(e map[string]any) { notif(e)["integration"] = map[string]any{"name": "slack", "index": 99999} }), "index"},
		"no alerts":                    {oneEvent(func(e map[string]any) { notif(e)["alerts"] = []any{} }), "no alerts"},
		"an alert without details":     {oneEvent(func(e map[string]any) { notif(e)["alerts"] = []any{map[string]any{"fingerprint": "1"}} }), "which alert"},
		"no alertname":                 {oneEvent(func(e map[string]any) { a := alert0(e); a["name"] = ""; a["labels"] = map[string]any{"x": "y"} }), "no alertname"},
		"no start":                     {oneEvent(func(e map[string]any) { alert0(e)["startsAt"] = "0001-01-01T00:00:00Z" }), "when it started"},
		"a label that isn't a string":  {oneEvent(func(e map[string]any) { alert0(e)["labels"] = map[string]any{"alertname": "Errors", "n": 3} }), "isn't a string"},
		"pairs that aren't":            {oneEvent(func(e map[string]any) { alert0(e)["labels"] = map[string]any{"labels": []any{1, 2}} }), "pairs"},
		"a receiver that doesn't show": {oneEvent(func(e map[string]any) { notif(e)["groupInfo"].(map[string]any)["receiverName"] = "on\ncall" }), "receiver"},
		"too many to skip":             {nil, "?skip="},
	} {
		p := path
		if c.body == nil {
			c.body = oneEvent(nil)
			p += "?" + strings.Repeat("skip=a&", maxSkip+1)
		}
		code, body, _ := send(t, h, p, c.body, bearer(notifyToken))
		if code != http.StatusBadRequest || !strings.Contains(body, c.want) {
			t.Errorf("%s: %d %q, want 400 saying %q", name, code, body, c.want)
		}
	}
	tooMany := oneEvent(func(e map[string]any) {
		a := notif(e)["alerts"].([]any)[0]
		var all []any
		for range ledger.MaxAlerts + 1 {
			all = append(all, a)
		}
		notif(e)["alerts"] = all
	})
	if code, _, _ := send(t, h, path, tooMany, bearer(notifyToken)); code != http.StatusRequestEntityTooLarge {
		t.Errorf("%d alerts: %d", ledger.MaxAlerts+1, code)
	}
	if n := kept2(t, s); n != 0 {
		t.Fatalf("%d kept from what was refused", n)
	}
}

// What the path takes: a label named labels in 0.35's map; a receiver
// ?skip= names passed over; a second integration of a receiver named
// apart; a time the recorder can't have sent taken as now.
func TestEventRecorderReads(t *testing.T) {
	s := notifyServer(t, nil)
	h := s.NotifyHandler()
	path := "/v1/alertmanager/events"
	post := func(p string, b []byte) string {
		t.Helper()
		code, body, _ := send(t, h, p, b, bearer(notifyToken))
		if code != http.StatusOK {
			t.Fatalf("%d %s", code, body)
		}
		return body
	}
	post(path, oneEvent(func(e map[string]any) {
		alert0(e)["labels"] = map[string]any{"alertname": "Errors", "service": "checkout", "labels": "a label named labels"}
		e["@timestamp"] = "2020-01-01T00:00:00Z" // long before now: taken as now
	}))
	if body := post(path+"?skip=lapilli", oneEvent(func(e map[string]any) {
		notif(e)["groupInfo"].(map[string]any)["receiverName"] = "lapilli"
		notif(e)["flushId"] = "2"
	})); !strings.Contains(body, `"kept":0`) {
		t.Errorf("a receiver ?skip= names: %s", body)
	}
	post(path, oneEvent(func(e map[string]any) {
		notif(e)["integration"] = map[string]any{"name": "pagerduty", "index": 1}
		notif(e)["reason"] = "NOTIFY_REASON_SOMETHING_NEW"
	}))
	// The same delivery told again two minutes on, as after the recorder
	// was restarted, is the one kept: by its instance, group and flush.
	if body := post(path, oneEvent(func(e map[string]any) { e["@timestamp"] = "2026-11-03T11:57:58.5Z" })); !strings.Contains(body, `"kept":0`) {
		t.Errorf("the same delivery again: %s", body)
	}
	ns := kept(t, s)
	if len(ns) != 2 {
		t.Fatalf("kept %d: %+v", len(ns), ns)
	}
	slices.SortFunc(ns, func(a, b ledger.Notification) int { return strings.Compare(a.Receiver, b.Receiver) })
	if ns[0].Receiver != "oncall/pagerduty" || ns[1].Receiver != "oncall/pagerduty[1]" {
		t.Errorf("receivers %q, %q", ns[0].Receiver, ns[1].Receiver)
	}
	if !ns[0].At.Equal(now) {
		t.Errorf("a delivery dated 2020 kept at %s, not now", ns[0].At)
	}
	if ns[0].Alerts[0].Labels["labels"] != "a label named labels" {
		t.Errorf("labels %v", ns[0].Alerts[0].Labels)
	}
	if ns[1].Reason == "" {
		t.Errorf("a reason the recorder doesn't name isn't worked out")
	}
}

// Events that carry no notification don't use up the rate notifications
// are taken at: a burst of them, and a delivery after it is still kept.
func TestEventRecorderRate(t *testing.T) {
	s := notifyServer(t, nil)
	h := s.NotifyHandler()
	other := []byte(`{"@timestamp":"2026-11-03T11:59:00Z","instance":"am-0","data":{"silenceMutedAlert":{}}}`)
	for i := range 300 {
		if code, body, _ := send(t, h, "/v1/alertmanager/events", other, bearer(notifyToken)); code != http.StatusOK {
			t.Fatalf("event %d: %d %s", i+1, code, body)
		}
	}
	if code, body, _ := send(t, h, "/v1/alertmanager/events", oneEvent(nil), bearer(notifyToken)); code != http.StatusOK || !strings.Contains(body, `"kept":1`) {
		t.Fatalf("a delivery after a burst of other events: %d %s", code, body)
	}
	// Past their own rate, they are refused too.
	refused := 0
	for range 2000 {
		if code, _, _ := send(t, h, "/v1/alertmanager/events", other, bearer(notifyToken)); code == http.StatusTooManyRequests {
			refused++
		}
	}
	if refused == 0 {
		t.Errorf("2,000 events at once, none refused")
	}
}
