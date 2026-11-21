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
// recorder's http_config.authorization sends it. A silence or an
// inhibition muting an alert (silenceMutedAlert, inhibitionMutedAlert, and
// a notification's mutedAlerts) is kept as a mute (docs/design/mutes.md).
// Every other event is answered and passed over (docs/design/inputs.md).
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
		// A silence's mute and an inhibition's: of a silence, its ID alone
		// is read, not its comment or who made it, which can name people.
		SilenceMuted *struct {
			Silence *struct {
				ID string `json:"id"`
			} `json:"silence"`
			MutedAlert *amMutedAlert `json:"mutedAlert"`
		} `json:"silenceMutedAlert"`
		InhibitionMuted *struct {
			MutedAlert *amMutedAlert `json:"mutedAlert"`
			// The alerts inhibiting it, by fingerprint: a 64-bit number,
			// which protojson writes as a string.
			Inhibiting []json.RawMessage `json:"inhibitingFingerprints"`
		} `json:"inhibitionMutedAlert"`
	} `json:"data"`
}

// amMutedAlert is the alert a mute is of: its labels, alertname among them.
type amMutedAlert struct {
	Labels amLabels `json:"labels"`
}

type amEventNotification struct {
	Alerts []struct {
		// The alert's reference: hash (0.33, 0.34) or fingerprint (0.35).
		Hash        string `json:"hash"`
		Fingerprint string `json:"fingerprint"`
		Details     *struct {
			Name     string   `json:"name"`
			Labels   amLabels `json:"labels"`
			StartsAt string   `json:"startsAt"`
			Resolved bool     `json:"resolved"`
		} `json:"details"`
	} `json:"alerts"`
	// MutedAlerts are the alerts a silence or an inhibition kept out of
	// the delivery: 0.33 and 0.34 leave them out of Alerts, 0.35 lists them
	// there too (and here by fingerprint), so they are taken out.
	MutedAlerts []struct {
		Hash        string `json:"hash"`
		Fingerprint string `json:"fingerprint"`
	} `json:"mutedAlerts"`
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
// recorder) are passed over. Its mutes are kept beside them: each silence's
// and inhibition's, and the alerts a notification says were muted, by what
// it doesn't say. Every other event is passed over.
func parseEvents(r *http.Request, body []byte, now time.Time) ([]ledger.Notification, []ledger.Mute, error) {
	skip := r.URL.Query()["skip"]
	if len(skip) > maxSkip {
		return nil, nil, fmt.Errorf("?skip= names %d receivers; %d at most", len(skip), maxSkip)
	}
	var events []json.RawMessage
	trimmed := bytes.TrimSpace(body)
	switch {
	case len(trimmed) > 0 && trimmed[0] == '[':
		if err := json.Unmarshal(trimmed, &events); err != nil {
			return nil, nil, fmt.Errorf("not the event recorder's JSON: %w", err)
		}
	case len(trimmed) > 0 && trimmed[0] == '{':
		events = []json.RawMessage{trimmed}
	default:
		return nil, nil, errors.New("not the event recorder's JSON: want an event, or an array of them")
	}
	if len(events) > maxEvents {
		return nil, nil, fmt.Errorf("%d events in one request; %d at most (the recorder's batch_max_events can keep it under)", len(events), maxEvents)
	}
	var out []ledger.Notification
	var mutes []ledger.Mute
	var over passedOver
	refuse := func(err error) {
		over.n++
		if over.first == nil {
			over.first = err
		}
	}
	for i, raw := range events {
		// Every event is read as an event, so one that isn't the
		// recorder's is refused, not taken as nothing to keep; it doesn't
		// keep the rest of a batch out.
		var e amEvent
		if err := json.Unmarshal(raw, &e); err != nil {
			refuse(fmt.Errorf("event %d: not the event recorder's JSON: %w", i+1, err))
			continue
		}
		if e.Timestamp == "" {
			refuse(fmt.Errorf("event %d has no @timestamp: not the event recorder's", i+1))
			continue
		}
		if e.Data.SilenceMuted != nil || e.Data.InhibitionMuted != nil {
			x, err := e.mute(now)
			if err != nil {
				refuse(fmt.Errorf("event %d: %w", i+1, err))
				continue
			}
			mutes = append(mutes, x)
			continue
		}
		m := e.Data.Notification
		if m == nil {
			continue
		}
		receiver := m.GroupInfo.ReceiverName
		if receiver == "" {
			refuse(fmt.Errorf("event %d: a notification with no receiver", i+1))
			continue
		}
		if slices.Contains(skip, receiver) {
			continue
		}
		n, muted, err := m.notification(e, now)
		switch {
		case errors.Is(err, errAllMuted):
			mutes = append(mutes, muted...)
			continue
		case err != nil:
			refuse(fmt.Errorf("event %d: %w", i+1, err))
			continue
		}
		out = append(out, n)
		mutes = append(mutes, muted...)
	}
	if over.n > 0 {
		return out, mutes, over
	}
	return out, mutes, nil
}

// passedOver is what of a request couldn't be read, or kept: how many of
// its notifications and mutes, and why the first. The rest are kept.
type passedOver struct {
	n     int
	first error
}

func (e passedOver) Error() string { return e.first.Error() }
func (e passedOver) Unwrap() error { return e.first }

// errAllMuted is a delivery whose every alert was muted: nothing paged.
var errAllMuted = errors.New("every alert muted")

// when is when the recorder says an event happened, within an hour before
// now and clockSkew after: the recorder tries again for a few seconds at
// most, and a time outside that is taken as now.
func (e amEvent) when(now time.Time) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, e.Timestamp); err == nil && !t.Before(now.Add(-time.Hour)) && !t.After(now.Add(clockSkew)) {
		return t
	}
	return now
}

// maxRef is how long a silence's ID is: Alertmanager's are UUIDs.
const maxRef = 64

// mute is a silence's or an inhibition's mute event as the ledger keeps
// it: when, the alert's name and labels, the silence's ID or the first
// inhibiting alert's fingerprint. Nothing else of the event is kept.
func (e amEvent) mute(now time.Time) (ledger.Mute, error) {
	m := ledger.Mute{At: e.when(now)}
	var of *amMutedAlert
	switch d := e.Data; {
	case d.SilenceMuted != nil:
		m.Kind, of = ledger.MutedSilence, d.SilenceMuted.MutedAlert
		if d.SilenceMuted.Silence == nil || !silenceID(d.SilenceMuted.Silence.ID) {
			return m, fmt.Errorf("a silence's mute needs the silence's ID: letters, digits and dashes, %d at most", maxRef)
		}
		m.Ref = d.SilenceMuted.Silence.ID
	default:
		m.Kind, of = ledger.MutedInhibition, d.InhibitionMuted.MutedAlert
		if fps := d.InhibitionMuted.Inhibiting; len(fps) > 0 {
			fp, ok := fingerprint(fps[0])
			if !ok {
				return m, errors.New("an inhibiting alert's fingerprint isn't a number")
			}
			m.Ref = fp
		}
	}
	if of == nil {
		return m, errors.New("a mute that doesn't say which alert it muted")
	}
	if m.Rule = of.Labels["alertname"]; m.Rule == "" {
		return m, errors.New("a mute of an alert with no alertname")
	}
	m.Labels = map[string]string(of.Labels)
	return m, nil
}

// silenceID reports whether an ID is one Alertmanager gives a silence, a
// UUID, or near enough: letters, digits and dashes, maxRef at most.
func silenceID(id string) bool {
	if id == "" || len(id) > maxRef {
		return false
	}
	for _, c := range id {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// fingerprint reads an alert's fingerprint, a 64-bit number, as protojson
// writes it (a string) or as a number.
func fingerprint(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		s = string(bytes.TrimSpace(raw))
	}
	if _, err := strconv.ParseUint(s, 10, 64); err != nil {
		return "", false
	}
	return s, true
}

// notification is a notification event as the ledger keeps it, and the
// mutes of the alerts it says were muted: those it names (0.35's), by what
// it doesn't say. It came when the recorder says it was delivered (when).
func (m *amEventNotification) notification(e amEvent, now time.Time) (ledger.Notification, []ledger.Mute, error) {
	at := e.when(now)
	integration := m.Integration.Name
	if integration == "" || !utf8.ValidString(integration) || len(integration) > 40 || strings.ContainsAny(integration, "/[]") {
		return ledger.Notification{}, nil, errors.New("a notification needs the integration that delivered it, a name of 40 characters at most")
	}
	index, err := strconv.Atoi(cmpOr(m.Integration.Index.String(), "0"))
	if err != nil || index < 0 || index > 1000 {
		return ledger.Notification{}, nil, fmt.Errorf("integration index %q isn't a number", m.Integration.Index)
	}
	receiver := m.GroupInfo.ReceiverName + "/" + integration
	if index > 0 {
		receiver += "[" + strconv.Itoa(index) + "]"
	}
	n := ledger.Notification{At: at, Receiver: receiver, Group: m.GroupInfo.GroupKey, Reason: amEventReasons[m.Reason]}
	if len(m.Alerts) > ledger.MaxAlerts {
		return n, nil, tooMany{len(m.Alerts)}
	}
	if len(m.Alerts) == 0 {
		return n, nil, errors.New("a notification with no alerts")
	}
	// Sent again after a failure, or by the recorder of another replica,
	// the same delivery is the same message: by the instance, its
	// group's instance, the flush and the integration. Hashed, so a long
	// host name keeps it within the ledger's bound.
	h := sha256.Sum256([]byte(strings.Join([]string{e.Instance, m.GroupInfo.GroupUUID, m.FlushID, receiver}, "\x00")))
	if m.GroupInfo.GroupUUID != "" && m.FlushID != "" {
		n.Msg = "amevents:" + hex.EncodeToString(h[:16])
	}
	muted := map[string]bool{}
	for _, x := range m.MutedAlerts {
		muted[cmpOr(x.Fingerprint, x.Hash)] = true
	}
	var mutes []ledger.Mute
	for i, a := range m.Alerts {
		d := a.Details
		if id := cmpOr(a.Fingerprint, a.Hash); id != "" && muted[id] {
			// Silenced or inhibited: not delivered. 0.35 lists it, and
			// it is kept as muted, by what the event doesn't say; 0.33
			// and 0.34 name it by its hash alone, and their own mute
			// events say which it is.
			if d != nil && d.Labels["alertname"] != "" {
				mutes = append(mutes, ledger.Mute{At: at, Rule: d.Labels["alertname"], Labels: map[string]string(d.Labels), Kind: ledger.MutedEither})
			}
			continue
		}
		if d == nil {
			return n, nil, fmt.Errorf("alert %d: the event doesn't say which alert it is", i+1)
		}
		rule := d.Labels["alertname"]
		if rule == "" {
			rule = d.Name
		}
		if rule == "" {
			return n, nil, fmt.Errorf("alert %d has no alertname", i+1)
		}
		start, err := time.Parse(time.RFC3339Nano, d.StartsAt)
		if err != nil || start.Year() <= 1 {
			return n, nil, fmt.Errorf("alert %d doesn't say when it started", i+1)
		}
		n.Alerts = append(n.Alerts, ledger.Alert{Rule: rule, Labels: map[string]string(d.Labels), Start: start, Resolved: d.Resolved})
	}
	if len(n.Alerts) == 0 {
		return n, mutes, errAllMuted
	}
	return n, mutes, nil
}
