package serve

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"lapilli.dev/internal/ledger"
)

// Alertmanager's event recorder (0.33.0 on, behind
// --enable-feature=event-recorder) posts what Alertmanager did, an event a
// request, or a JSON array of them with batch: true. Of its events, a
// notification is recorded after each delivery an integration made:
// PagerDuty's, Slack's and email's as well as a webhook's. Those are kept
// as notifications, one for each integration that delivered, on
// /v1/alertmanager/events, with the token as a bearer token, as the
// recorder's http_config.authorization sends it. Every other event is
// answered and passed over (docs/design/inputs.md).
//
// The format changed once: 0.33 and 0.34 write labels as a list of key and
// value pairs and an alert's hash as hash; 0.35 (events/v2) writes labels
// as a map and the hash as fingerprint. Both are read.

// maxEvents is how many events one request may carry: the recorder's
// batches are 100 by default.
const maxEvents = 1000

// amEvent is one event, as protojson writes it.
type amEvent struct {
	Timestamp string `json:"@timestamp"`
	Instance  string `json:"instance"`
	Data      struct {
		Notification *amEventNotification `json:"notification"`
	} `json:"data"`
}

type amEventNotification struct {
	Alerts []struct {
		Details *struct {
			Name     string   `json:"name"`
			Labels   amLabels `json:"labels"`
			StartsAt string   `json:"startsAt"`
			Resolved bool     `json:"resolved"`
		} `json:"details"`
	} `json:"alerts"`
	GroupInfo struct {
		GroupKey     string `json:"groupKey"`
		ReceiverName string `json:"receiverName"`
		GroupUUID    string `json:"groupUuid"`
	} `json:"groupInfo"`
	Reason      string `json:"reason"`
	FlushID     string `json:"flushId"`
	Integration struct {
		Name  string `json:"name"`
		Index json.Number
	} `json:"integration"`
}

// amLabels are labels as either format writes them: {"labels": [{"key":
// "k", "value": "v"}]} (0.33, 0.34), or {"k": "v"} (0.35).
type amLabels map[string]string

func (l *amLabels) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return errors.New("labels aren't an object")
	}
	out := amLabels{}
	// A list of pairs is the one key "labels" holding an array: a label
	// named labels in a map holds a string.
	if v, ok := raw["labels"]; ok && len(raw) == 1 && len(bytes.TrimSpace(v)) > 0 && bytes.TrimSpace(v)[0] == '[' {
		var pairs []struct{ Key, Value string }
		if err := json.Unmarshal(v, &pairs); err != nil {
			return errors.New("labels aren't key and value pairs")
		}
		for _, p := range pairs {
			out[p.Key] = p.Value
		}
		*l = out
		return nil
	}
	for k, v := range raw {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return fmt.Errorf("label %q isn't a string", k)
		}
		out[k] = s
	}
	*l = out
	return nil
}

// amEventReasons are the recorder's reasons, as the ledger keeps them.
var amEventReasons = map[string]string{
	"NOTIFY_REASON_FIRST_NOTIFICATION":      ledger.ReasonFirst,
	"NOTIFY_REASON_NEW_ALERTS_IN_GROUP":     ledger.ReasonNew,
	"NOTIFY_REASON_NEW_RESOLVED_ALERTS":     ledger.ReasonResolved,
	"NOTIFY_REASON_ALL_ALERTS_RESOLVED":     ledger.ReasonResolved,
	"NOTIFY_REASON_REPEAT_INTERVAL_ELAPSED": ledger.ReasonRepeat,
}

// maxSkip is how many receivers ?skip= may name.
const maxSkip = 20

// parseEvents reads what the event recorder posts: its notification
// events, each a notification, sent to RECEIVER/INTEGRATION; the receivers
// ?skip= names (Lapilli's own webhook receiver, while it is kept beside the
// recorder) are passed over, as is every other event.
func parseEvents(r *http.Request, body []byte, now time.Time) ([]ledger.Notification, error) {
	skip := r.URL.Query()["skip"]
	if len(skip) > maxSkip {
		return nil, fmt.Errorf("?skip= names %d receivers; %d at most", len(skip), maxSkip)
	}
	var events []json.RawMessage
	trimmed := bytes.TrimSpace(body)
	switch {
	case len(trimmed) > 0 && trimmed[0] == '[':
		if err := json.Unmarshal(trimmed, &events); err != nil {
			return nil, fmt.Errorf("not the event recorder's JSON: %w", err)
		}
	case len(trimmed) > 0 && trimmed[0] == '{':
		events = []json.RawMessage{trimmed}
	default:
		return nil, errors.New("not the event recorder's JSON: want an event, or an array of them")
	}
	if len(events) > maxEvents {
		return nil, fmt.Errorf("%d events in one request; %d at most (the recorder's batch_max_events can keep it under)", len(events), maxEvents)
	}
	var out []ledger.Notification
	for i, raw := range events {
		// Every event is read as an event, so a body that isn't the
		// recorder's is refused, not taken as nothing to keep.
		var e amEvent
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("event %d: not the event recorder's JSON: %w", i+1, err)
		}
		if e.Timestamp == "" {
			return nil, fmt.Errorf("event %d has no @timestamp: not the event recorder's", i+1)
		}
		m := e.Data.Notification
		if m == nil {
			continue
		}
		receiver := m.GroupInfo.ReceiverName
		if receiver == "" {
			return nil, fmt.Errorf("event %d: a notification with no receiver", i+1)
		}
		if slices.Contains(skip, receiver) {
			continue
		}
		n, err := m.notification(e, now)
		if err != nil {
			return nil, fmt.Errorf("event %d: %w", i+1, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// notification is a notification event as the ledger keeps it. It came
// when the recorder says it was delivered, within an hour before now and
// clockSkew after: the recorder tries again for a few seconds at most, and
// a time outside that is taken as now.
func (m *amEventNotification) notification(e amEvent, now time.Time) (ledger.Notification, error) {
	at := now
	if t, err := time.Parse(time.RFC3339Nano, e.Timestamp); err == nil && !t.Before(now.Add(-time.Hour)) && !t.After(now.Add(clockSkew)) {
		at = t
	}
	integration := m.Integration.Name
	if integration == "" || !utf8.ValidString(integration) || len(integration) > 40 || strings.ContainsAny(integration, "/[]") {
		return ledger.Notification{}, errors.New("a notification needs the integration that delivered it, a name of 40 characters at most")
	}
	index, err := strconv.Atoi(cmpOr(m.Integration.Index.String(), "0"))
	if err != nil || index < 0 || index > 1000 {
		return ledger.Notification{}, fmt.Errorf("integration index %q isn't a number", m.Integration.Index)
	}
	receiver := m.GroupInfo.ReceiverName + "/" + integration
	if index > 0 {
		receiver += "[" + strconv.Itoa(index) + "]"
	}
	n := ledger.Notification{At: at, Receiver: receiver, Group: m.GroupInfo.GroupKey, Reason: amEventReasons[m.Reason]}
	if len(m.Alerts) > ledger.MaxAlerts {
		return n, tooMany{len(m.Alerts)}
	}
	if len(m.Alerts) == 0 {
		return n, errors.New("a notification with no alerts")
	}
	// Sent again after a failure, or by the recorder of another replica,
	// the same delivery is the same message: by the instance, its
	// group's instance, the flush and the integration. Hashed, so a long
	// host name keeps it within the ledger's bound.
	h := sha256.Sum256([]byte(strings.Join([]string{e.Instance, m.GroupInfo.GroupUUID, m.FlushID, receiver}, "\x00")))
	if m.GroupInfo.GroupUUID != "" && m.FlushID != "" {
		n.Msg = "amevents:" + hex.EncodeToString(h[:16])
	}
	for i, a := range m.Alerts {
		d := a.Details
		if d == nil {
			return n, fmt.Errorf("alert %d: the event doesn't say which alert it is", i+1)
		}
		rule := d.Labels["alertname"]
		if rule == "" {
			rule = d.Name
		}
		if rule == "" {
			return n, fmt.Errorf("alert %d has no alertname", i+1)
		}
		start, err := time.Parse(time.RFC3339Nano, d.StartsAt)
		if err != nil || start.Year() <= 1 {
			return n, fmt.Errorf("alert %d doesn't say when it started", i+1)
		}
		n.Alerts = append(n.Alerts, ledger.Alert{Rule: rule, Labels: map[string]string(d.Labels), Start: start, Resolved: d.Resolved})
	}
	return n, nil
}
