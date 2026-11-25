package routing_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/content"
	"lapilli.dev/internal/routing"
)

// The model is held to what real Alertmanagers did (docs/design/routing.md,
// "Checked against a real Alertmanager"): the alerts and the silence that
// were sent to 0.33.1, 0.34.2 and 0.35.0 (internal/serve/testdata/events),
// each at the time its first event says it arrived, are played through the
// same configuration, and the deliveries must be the same for each receiver
// and integration, in the same order, for the same reasons and alerts.

// events as the recorder wrote them: only what the test reads.
type recEvent struct {
	Timestamp string `json:"@timestamp"`
	Data      struct {
		AlertCreated *struct {
			Alert recAlert `json:"alert"`
		} `json:"alertCreated"`
		AlertGrouped *struct {
			Alert recAlert `json:"alert"`
		} `json:"alertGrouped"`
		SilenceCreated *struct {
			Silence struct {
				ID       string `json:"id"`
				StartsAt string `json:"startsAt"`
				EndsAt   string `json:"endsAt"`
				Matchers []struct {
					Type    string `json:"type"`
					Name    string `json:"name"`
					Pattern string `json:"pattern"`
				} `json:"matchers"`
			} `json:"silence"`
		} `json:"silenceCreated"`
		SilenceMuted *json.RawMessage `json:"silenceMutedAlert"`
		Notification *struct {
			Alerts []struct {
				Details struct {
					Labels   recLabels `json:"labels"`
					Resolved bool      `json:"resolved"`
				} `json:"details"`
			} `json:"alerts"`
			GroupInfo struct {
				ReceiverName string `json:"receiverName"`
			} `json:"groupInfo"`
			Reason      string `json:"reason"`
			Integration struct {
				Name  string          `json:"name"`
				Index json.RawMessage `json:"index"`
			} `json:"integration"`
		} `json:"notification"`
	} `json:"data"`
}

type recAlert struct {
	Details *struct {
		Labels recLabels `json:"labels"`
		EndsAt string    `json:"endsAt"`
	} `json:"details"`
	Labels recLabels `json:"labels"`
	EndsAt string    `json:"endsAt"`
}

func (a recAlert) labels() map[string]string {
	if a.Details != nil {
		return a.Details.Labels
	}
	return a.Labels
}

func (a recAlert) endsAt() string {
	if a.Details != nil {
		return a.Details.EndsAt
	}
	return a.EndsAt
}

// recLabels are labels as 0.33 and 0.34 write them, a list of pairs, or 0.35,
// a map.
type recLabels map[string]string

func (l *recLabels) UnmarshalJSON(b []byte) error {
	var pairs struct {
		Labels []struct{ Key, Value string } `json:"labels"`
	}
	if json.Unmarshal(b, &pairs) == nil && pairs.Labels != nil {
		*l = recLabels{}
		for _, p := range pairs.Labels {
			(*l)[p.Key] = p.Value
		}
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	*l = m
	return nil
}

func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// a delivery as the test compares them: its reason and the alerts it told of.
type delivery struct {
	at       time.Time
	reason   string
	firing   []string
	resolved []string
}

func (d delivery) String() string {
	return fmt.Sprintf("%s firing=%v resolved=%v", d.reason, d.firing, d.resolved)
}

func name(labels map[string]string) string {
	return labels["alertname"] + "/" + labels["instance"]
}

func TestRecordings(t *testing.T) {
	cfg, err := os.ReadFile("../serve/testdata/events/alertmanager-0.35.0.yml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := content.ReadAlertmanagerRouting(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"0.33.1", "0.34.2", "0.35.0"} {
		t.Run(v, func(t *testing.T) {
			f, err := os.Open(filepath.Join("../serve/testdata/events", "alertmanager-"+v+".jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			var (
				alerts   []routing.Alert
				index    = map[string]int{}
				silences []byte
				want     = map[string][]delivery{}
				settled  time.Time
				last     time.Time
				mutedAt  []time.Time
			)
			sc := bufio.NewScanner(f)
			sc.Buffer(nil, 1<<20)
			for sc.Scan() {
				var e recEvent
				if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
					t.Fatal(err)
				}
				ts := at(t, e.Timestamp)
				d := e.Data
				switch {
				case d.AlertCreated != nil:
					// An alert arrives when its first event says so.
					k := name(d.AlertCreated.Alert.labels())
					if _, ok := index[k]; !ok {
						index[k] = len(alerts)
						alerts = append(alerts, routing.Alert{Labels: d.AlertCreated.Alert.labels(), Start: ts})
					}
				case d.AlertGrouped != nil:
					// It resolves when it arrives with an end that has passed.
					k := name(d.AlertGrouped.Alert.labels())
					if i, ok := index[k]; ok && alerts[i].End.IsZero() && !at(t, d.AlertGrouped.Alert.endsAt()).After(ts) {
						alerts[i].End = ts
					}
				case d.SilenceCreated != nil:
					s := d.SilenceCreated.Silence
					type m struct {
						Name    string `json:"name"`
						Value   string `json:"value"`
						IsRegex bool   `json:"isRegex"`
						IsEqual bool   `json:"isEqual"`
					}
					var ms []m
					for _, x := range s.Matchers {
						if x.Type != "TYPE_EQUAL" {
							t.Fatalf("a silence matcher of type %s: the test reads only equal", x.Type)
						}
						ms = append(ms, m{x.Name, x.Pattern, false, true})
					}
					silences, _ = json.Marshal([]any{map[string]any{"id": s.ID, "matchers": ms, "startsAt": s.StartsAt, "endsAt": s.EndsAt,
						"comment": "maintenance", "createdBy": "tester"}})
				case d.SilenceMuted != nil:
					mutedAt = append(mutedAt, ts)
				case d.Notification != nil:
					n := d.Notification
					k := fmt.Sprintf("%s/%s", n.GroupInfo.ReceiverName, n.Integration.Name)
					if string(n.Integration.Index) != "" && string(n.Integration.Index) != "0" && string(n.Integration.Index) != `"0"` {
						k += "[" + strings.Trim(string(n.Integration.Index), `"`) + "]"
					}
					dl := delivery{at: ts, reason: map[string]string{
						"NOTIFY_REASON_FIRST_NOTIFICATION":      "first",
						"NOTIFY_REASON_NEW_ALERTS_IN_GROUP":     "new",
						"NOTIFY_REASON_NEW_RESOLVED_ALERTS":     "resolved",
						"NOTIFY_REASON_ALL_ALERTS_RESOLVED":     "resolved",
						"NOTIFY_REASON_REPEAT_INTERVAL_ELAPSED": "repeat",
					}[n.Reason]}
					for _, a := range n.Alerts {
						if a.Details.Resolved {
							dl.resolved = append(dl.resolved, name(a.Details.Labels))
						} else {
							dl.firing = append(dl.firing, name(a.Details.Labels))
						}
					}
					slices.Sort(dl.firing)
					slices.Sort(dl.resolved)
					want[k] = append(want[k], dl)
					if settled.IsZero() {
						// The first delivery is when the pipeline let the
						// first flush through, after waiting for its cluster
						// to settle.
						settled = ts
					}
					if ts.After(last) {
						last = ts
					}
				}
			}
			if err := sc.Err(); err != nil {
				t.Fatal(err)
			}
			if len(alerts) != 4 || len(want) != 3 || len(mutedAt) < 10 {
				t.Fatalf("read %d alerts, deliveries to %d integrations, %d silenced flushes", len(alerts), len(want), len(mutedAt))
			}
			sil, err := routing.ReadSilences(silences)
			if err != nil {
				t.Fatal(err)
			}
			// Alertmanager's timers run a little late each time: the silenced
			// QueueStuck group's flushes say how late.
			late := mutedAt[len(mutedAt)-1].Sub(mutedAt[1])/time.Duration(len(mutedAt)-2) - 5*time.Second
			if late < 0 || late > 50*time.Millisecond {
				t.Fatalf("the timers ran %v late a tick", late)
			}
			res, err := c.Run(alerts, routing.Options{Silences: sil, Settled: settled, TimerLate: late, Until: last.Add(time.Second)})
			if err != nil {
				t.Fatal(err)
			}
			got := map[string][]delivery{}
			for _, n := range res.Notifications {
				k := fmt.Sprintf("%s/%s", n.Receiver, n.Kind)
				if n.Index > 0 {
					k += fmt.Sprintf("[%d]", n.Index)
				}
				dl := delivery{at: n.At, reason: string(n.Reason)}
				for _, i := range n.Firing {
					dl.firing = append(dl.firing, name(alerts[i].Labels))
				}
				for _, i := range n.Resolved {
					dl.resolved = append(dl.resolved, name(alerts[i].Labels))
				}
				slices.Sort(dl.firing)
				slices.Sort(dl.resolved)
				got[k] = append(got[k], dl)
			}
			for k, w := range want {
				g := got[k]
				if len(g) != len(w) {
					t.Errorf("%s: %d deliveries, recorded %d:\n got %v\nwant %v", k, len(g), len(w), g, w)
					continue
				}
				for i := range w {
					if g[i].String() != w[i].String() {
						t.Errorf("%s #%d: %v, recorded %v", k, i+1, g[i], w[i])
					}
					// And when: within what the recording's own delivery
					// takes, a few milliseconds, and the second the model
					// doesn't know of (Prometheus's send).
					if d := g[i].at.Sub(w[i].at); d < -250*time.Millisecond || d > 250*time.Millisecond {
						t.Errorf("%s #%d (%s): at %v, recorded %v", k, i+1, w[i].reason, g[i].at.Format("15:04:05.000"), w[i].at.Format("15:04:05.000"))
					}
				}
			}
			for k := range got {
				if _, ok := want[k]; !ok {
					t.Errorf("%s: deliveries the recording doesn't have: %v", k, got[k])
				}
			}
			// What muted what: the QueueStuck alert was silenced, the rest not.
			for i, a := range alerts {
				muted := len(res.Alerts[i].Muted) > 0
				if muted != (a.Labels["alertname"] == "QueueStuck") {
					t.Errorf("%s: muted %v", name(a.Labels), res.Alerts[i].Muted)
				}
			}
		})
	}
}
