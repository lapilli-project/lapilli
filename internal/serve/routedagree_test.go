package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
)

// recordedEvent is an event of a recording: its time and what it holds.
type recordedEvent struct {
	Timestamp string `json:"@timestamp"`
	Data      map[string]map[string]any
}

// recordedEvents are the events of a recording, a request a line, or, with
// batch, an array of them.
func recordedEvents(t *testing.T, name string) []recordedEvent {
	t.Helper()
	var out []recordedEvent
	for _, line := range recorded(t, name) {
		var es []recordedEvent
		if line[0] == '[' {
			if err := json.Unmarshal(line, &es); err != nil {
				t.Fatal(err)
			}
		} else {
			var e recordedEvent
			if err := json.Unmarshal(line, &e); err != nil {
				t.Fatal(err)
			}
			es = append(es, e)
		}
		out = append(out, es...)
	}
	return out
}

// recordedAlerts reads from a real Alertmanager's recording the alerts it
// was sent and when each resolved, as firings: the rule is the alertname,
// the labels the rest.
func recordedAlerts(t *testing.T, name string) []firing.Firing {
	t.Helper()
	type label = map[string]string
	var out []firing.Firing
	index := map[string]int{}
	pairs := func(v any) label {
		l := label{}
		m, _ := v.(map[string]any)
		if list, ok := m["labels"].([]any); ok { // 0.33 and 0.34: {"labels": [{"key", "value"}]}
			for _, p := range list {
				kv := p.(map[string]any)
				l[kv["key"].(string)] = kv["value"].(string)
			}
			return l
		}
		for k, x := range m { // 0.35: a map
			l[k], _ = x.(string)
		}
		return l
	}
	for _, e := range recordedEvents(t, name) {
		ts, _ := time.Parse(time.RFC3339Nano, e.Timestamp)
		alert := func(kind string) (label, string, bool) {
			x, ok := e.Data[kind]
			if !ok {
				return nil, "", false
			}
			a, _ := x["alert"].(map[string]any)
			d := a
			if det, ok := a["details"].(map[string]any); ok {
				d = det
			}
			ends, _ := d["endsAt"].(string)
			if d["labels"] == nil {
				return nil, "", false
			}
			return pairs(d["labels"]), ends, true
		}
		if l, _, ok := alert("alertCreated"); ok {
			k := l["alertname"] + "/" + l["instance"]
			if _, seen := index[k]; !seen {
				index[k] = len(out)
				rule := l["alertname"]
				delete(l, "alertname")
				out = append(out, firing.Firing{Rule: rule, Labels: l, Start: ts})
			}
		}
		if l, ends, ok := alert("alertGrouped"); ok {
			k := l["alertname"] + "/" + l["instance"]
			if i, seen := index[k]; seen && out[i].End.IsZero() {
				if end, err := time.Parse(time.RFC3339, ends); err == nil && !end.After(ts) {
					out[i].End = ts
				}
			}
		}
	}
	return out
}

// Held to what a real Alertmanager sent: the recordings' notifications are
// kept as the recorder posted them, their alerts are the ledger's firings,
// the silence it muted one by is told by the mutes it recorded (no --silences
// is given), and the notifications worked out agree with those recorded for
// each receiver, on the Notifications page and in /metrics.
func TestRoutedAgreesWithRecordings(t *testing.T) {
	parallel(t)
	cfg, err := os.ReadFile("testdata/events/alertmanager-0.35.0.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"alertmanager-0.34.2.jsonl", "alertmanager-0.35.0.jsonl", "alertmanager-0.35.0-batch.jsonl"} {
		t.Run(file, func(t *testing.T) {
			am := routedFile(t, string(cfg))
			s := notifyServer(t, func(c *Config) { c.Notifications, c.AlertmanagerConfig = true, am })
			end := time.Date(2026, 11, 11, 16, 30, 0, 0, time.UTC)
			s.now = func() time.Time { return end }
			ctx := context.Background()
			h := s.NotifyHandler()
			for i, b := range recorded(t, file) {
				if code, body, _ := send(t, h, "/v1/alertmanager/events", b, bearer(notifyToken)); code != http.StatusOK {
					t.Fatalf("request %d: %d %s", i+1, code, body)
				}
			}
			// The recording's own alerts, as firings.
			alerts := recordedAlerts(t, file)
			if len(alerts) != 4 {
				t.Fatalf("%d alerts in the recording", len(alerts))
			}
			// To the last of what the recording says, a few seconds after the
			// last alert resolved: the silenced one never does, and the recorder
			// told of its mutes while Alertmanager held it back.
			from, to := time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC), time.Time{}
			for _, e := range recordedEvents(t, file) {
				if ts, err := time.Parse(time.RFC3339Nano, e.Timestamp); err == nil {
					to = later(to, ts)
				}
			}
			to = to.Add(time.Second)
			if err := s.l.Record(ctx, ledger.Found{From: from, To: to, Firings: alerts,
				Run: ledger.Run{Kind: "test", Started: from, Finished: from, From: from, To: to}}, s.cfg.Options); err != nil {
				t.Fatal(err)
			}
			s.workOutRouted(ctx, from, to)
			v, err := s.notificationsNow(ctx)
			if err != nil {
				t.Fatal(err)
			}
			rows := map[string]receiverRow{}
			for _, r := range v.Rows {
				rows[r.Receiver] = r
			}
			for _, name := range []string{"oncall", "team-web"} {
				r, ok := rows[name]
				if !ok || !r.Compared || r.Recorded == 0 || r.WorkedOut == 0 {
					t.Fatalf("%s: %+v (rows %+v)", name, r, v.Rows)
				}
				// Alertmanager's own delivery takes seconds the model
				// doesn't know of: the counts agree, give or take the one a
				// timer on the edge of the period makes.
				if r.Agreement() < 0.75 {
					t.Errorf("%s: worked out %d, recorded %d", name, r.WorkedOut, r.Recorded)
				}
			}
			if v.Compared != 2 || v.Agreement < 0.75 {
				t.Errorf("agreement %.2f over %d receivers", v.Agreement, v.Compared)
			}
			// The silenced alert, told only by the recorded mutes.
			muted := 0
			fs, _, _ := s.l.FiringsOverlapping(ctx, from, to, from, 100)
			ids := make([]int64, len(fs))
			for i, f := range fs {
				ids[i] = f.ID
			}
			got, _ := s.l.RoutedOf(ctx, ids, now)
			for _, f := range fs {
				if f.Rule == "QueueStuck" {
					r := got[f.ID]
					for _, m := range r.Muted {
						if m.Kind == "silence" {
							muted++
						}
					}
					// Alertmanager's first flush waits for its cluster to settle,
					// seconds the model's doesn't: the one flush before the first
					// mute the recorder told of is the model's, and no more.
					if r.Notified > 2 {
						t.Errorf("the silenced QueueStuck was notified: %+v", r)
					}
				}
			}
			if muted == 0 {
				t.Error("the silence the recorder told of didn't mute QueueStuck")
			}
			// The page and the metrics say it.
			hh := s.Handler()
			_, body, _ := get(t, hh, "/notifications")
			for _, want := range []string{"<code>oncall</code>", "<code>team-web</code>", "Agreement:"} {
				if !strings.Contains(body, want) {
					t.Errorf("/notifications lacks %q", want)
				}
			}
			_, body, _ = get(t, hh, "/metrics")
			if !strings.Contains(body, "lapilli_notifications_agreement ") || !strings.Contains(body, `lapilli_notifications_agreement_receiver{receiver="oncall"}`) {
				t.Errorf("metrics: %s", grepLines(body, "agreement"))
			}
			// Recorded where recorded: a rule's pages count the recorded ones.
			p, err := s.notified(ctx)
			if err != nil {
				t.Fatal(err)
			}
			rec, worked := 0, 0
			for _, ev := range p.events {
				if ev.At.Before(from) {
					continue // the test history's firings, days before: worked out, none recorded then
				}
				if ev.Recorded {
					rec++
				} else {
					worked++
				}
			}
			if rec == 0 || worked != 0 {
				t.Errorf("pages recorded %d, worked out %d: where the recorded ones are, they count alone", rec, worked)
			}
		})
	}
}
