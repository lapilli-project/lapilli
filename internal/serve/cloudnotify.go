package serve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
)

// A cloud's own alarms' notifications (docs/design/notifications.md, "A
// cloud's own alarms' notifications"): what CloudWatch sends to SNS for an
// alarm, directly or through EventBridge, and what OCI Notifications posts
// for an OCI Monitoring alarm. Each is named and labelled as
// recipes/firings names and labels the alarm's firing, so the two tie.

// cwAlarm is a CloudWatch alarm's message to SNS: a metric, composite or
// log alarm's, or one contributor's of an alarm on several series.
type cwAlarm struct {
	AlarmName       string         `json:"AlarmName"`
	AlarmArn        string         `json:"AlarmArn"`
	NewStateValue   string         `json:"NewStateValue"`
	OldStateValue   string         `json:"OldStateValue"`
	StateChangeTime string         `json:"StateChangeTime"`
	ContributorID   string         `json:"AlarmContributorId"`
	Contributor     map[string]any `json:"AlarmContributorAttributes"`
	Trigger         struct {
		Dimensions []struct {
			Name  string `json:"name"`
			Value any    `json:"value"`
		} `json:"Dimensions"`
	} `json:"Trigger"`
}

// cwEvent is a CloudWatch alarm's state change as EventBridge writes it,
// sent on to SNS.
type cwEvent struct {
	DetailType string   `json:"detail-type"`
	Resources  []string `json:"resources"`
	Detail     struct {
		AlarmName   string `json:"alarmName"`
		Contributor *struct {
			ID         string         `json:"id"`
			Attributes map[string]any `json:"attributes"`
		} `json:"alarmContributor"`
		State struct {
			Value     string `json:"value"`
			Timestamp string `json:"timestamp"`
		} `json:"state"`
		Previous *struct {
			Value string `json:"value"`
		} `json:"previousState"`
		Configuration struct {
			Metrics []struct {
				MetricStat *struct {
					Metric struct {
						Dimensions map[string]any `json:"dimensions"`
					} `json:"metric"`
				} `json:"metricStat"`
			} `json:"metrics"`
		} `json:"configuration"`
	} `json:"detail"`
}

// cwTime reads a time as CloudWatch writes one: 2025-12-01T13:42:04.919+0000,
// with no colon in the zone, or RFC 3339.
func cwTime(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999-0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q isn't a time", s)
}

// cwMove is the alert a move of a CloudWatch alarm's state tells of: into
// ALARM, starting then; out of it, resolving, its start the ledger's to
// work out from the move in. A move between OK and INSUFFICIENT_DATA is of
// no firing, and tells of no alert.
func cwMove(name string, labels map[string]string, to, from, at string) (ledger.Alert, bool, error) {
	a := ledger.Alert{Rule: name, Labels: labels}
	switch {
	case to == "ALARM" && from != "ALARM":
		t, err := cwTime(at)
		if err != nil {
			return a, false, fmt.Errorf("the alarm's state changed at %w", err)
		}
		a.Start = t
		return a, true, nil
	case from == "ALARM" && to != "ALARM":
		a.Resolved = true
		return a, true, nil
	}
	return a, false, nil
}

// strings of a map from JSON: each value as text.
func textOf(m map[string]any, into map[string]string) {
	for k, v := range m {
		if s, ok := v.(string); ok {
			into[k] = s
		} else {
			b, _ := json.Marshal(v)
			into[k] = string(b)
		}
	}
}

// cloudWatch reads a CloudWatch alarm's message: its name, the dimensions it
// watches and a contributor's attributes as labels, its ARN and contributor
// as the group.
func cloudWatch(r *http.Request, msg []byte, now time.Time) ([]ledger.Notification, error) {
	var c cwAlarm
	if err := json.Unmarshal(msg, &c); err != nil {
		return nil, fmt.Errorf("not a CloudWatch alarm's message: %w", err)
	}
	if c.AlarmName == "" {
		return nil, errors.New("a CloudWatch alarm's message without its AlarmName")
	}
	labels := map[string]string{}
	for _, d := range c.Trigger.Dimensions {
		if d.Name != "" {
			textOf(map[string]any{d.Name: d.Value}, labels)
		}
	}
	textOf(c.Contributor, labels)
	a, ok, err := cwMove(c.AlarmName, labels, c.NewStateValue, c.OldStateValue, c.StateChangeTime)
	if err != nil || !ok {
		return nil, err
	}
	group := c.AlarmArn
	if c.ContributorID != "" {
		group += "/" + c.ContributorID
	}
	return []ledger.Notification{{At: now, Receiver: receiverOf(r, ""), Group: group, Alerts: []ledger.Alert{a}}}, nil
}

// cloudWatchEvent reads a CloudWatch alarm's state change as EventBridge
// writes it: for the alarm, or, for an alarm on several series, one
// contributor, whose change says no state before it.
func cloudWatchEvent(r *http.Request, msg []byte, now time.Time) ([]ledger.Notification, error) {
	var e cwEvent
	if err := json.Unmarshal(msg, &e); err != nil {
		return nil, fmt.Errorf("not EventBridge's CloudWatch alarm state change: %w", err)
	}
	d := e.Detail
	if d.AlarmName == "" {
		return nil, errors.New("an alarm state change without its alarmName")
	}
	labels := map[string]string{}
	if ms := d.Configuration.Metrics; len(ms) == 1 && ms[0].MetricStat != nil {
		textOf(ms[0].MetricStat.Metric.Dimensions, labels)
	}
	group := d.AlarmName
	if len(e.Resources) > 0 {
		group = e.Resources[0]
	}
	from := ""
	if d.Previous != nil {
		from = d.Previous.Value
	}
	if d.Contributor != nil {
		textOf(d.Contributor.Attributes, labels)
		group += "/" + d.Contributor.ID
		if from == "" && d.State.Value != "ALARM" {
			from = "ALARM" // a contributor's change from ALARM: it says only where it went
		}
	}
	a, ok, err := cwMove(d.AlarmName, labels, d.State.Value, from, d.State.Timestamp)
	if err != nil || !ok {
		return nil, err
	}
	return []ledger.Notification{{At: now, Receiver: receiverOf(r, ""), Group: group, Alerts: []ledger.Alert{a}}}, nil
}

// ociMessage is what OCI Notifications posts for an OCI Monitoring alarm,
// in the alarm message format, version 1.x; or, for a new subscription, the
// URL that confirms it.
type ociMessage struct {
	DedupeKey        string `json:"dedupeKey"`
	Title            string `json:"title"`
	Type             string `json:"type"`
	Severity         string `json:"severity"`
	Millis           *int64 `json:"timestampEpochMillis"`
	Timestamp        string `json:"timestamp"`
	NotificationType string `json:"notificationType"`
	AlarmMetaData    []struct {
		ID               string           `json:"id"`
		Severity         string           `json:"severity"`
		Namespace        string           `json:"namespace"`
		Dimensions       []map[string]any `json:"dimensions"`
		NotificationType string           `json:"notificationType"`
	} `json:"alarmMetaData"`
	ConfirmationURL string `json:"ConfirmationURL"`
}

// ociTypes are an alarm message's types, as the ledger counts them: one
// that starts a firing, one that repeats, one that resolves.
var ociTypes = map[string]string{
	"OK_TO_FIRING": "start", "REPEAT": "repeat", "FIRING_TO_OK": "resolved", "RESET": "resolved",
}

// parseOCI reads what OCI Notifications posts to /v1/oci/SOURCE: an alarm's
// message, an alert for the alarm (a grouped message) or for its one metric
// stream (a split message), named by its title and labelled as
// recipes/firings/oci.jq labels the alarm's firings, its OCID as alarm_id
// among them; or a new subscription's confirmation, which is fetched after
// the answer, as a request that carries the token is the only one read.
func (s *Server) parseOCI(r *http.Request, body []byte, now time.Time) ([]ledger.Notification, error) {
	var m ociMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("not OCI Notifications' JSON: %w", err)
	}
	confirm := r.Header.Get("X-OCI-NS-ConfirmationURL")
	if confirm == "" {
		confirm = m.ConfirmationURL
	}
	if confirm != "" {
		s.confirmOCI(confirm, r.Header.Get("X-OCI-NS-TopicName"))
		if m.DedupeKey == "" && len(m.AlarmMetaData) == 0 {
			return nil, nil
		}
	}
	kind, ok := ociTypes[m.Type]
	switch {
	case !ok:
		return nil, fmt.Errorf("type %q: want an alarm message's, OK_TO_FIRING, FIRING_TO_OK, REPEAT or RESET", m.Type)
	case m.DedupeKey == "" || m.Title == "":
		return nil, errors.New("an alarm message needs its dedupeKey and title")
	case len(m.AlarmMetaData) == 0:
		return nil, errors.New("an alarm message without alarmMetaData")
	case len(m.AlarmMetaData) > ledger.MaxAlerts:
		return nil, tooMany{len(m.AlarmMetaData)}
	}
	n := ledger.Notification{At: now, Receiver: receiverOf(r, r.Header.Get("X-OCI-NS-TopicName")), Group: m.DedupeKey}
	var start time.Time
	if kind == "start" {
		switch {
		case m.Millis != nil:
			start = time.UnixMilli(*m.Millis).UTC()
		default:
			t, err := time.Parse(time.RFC3339Nano, m.Timestamp)
			if err != nil {
				return nil, fmt.Errorf("timestamp %q isn't a time", m.Timestamp)
			}
			start = t
		}
	}
	if kind == "repeat" {
		n.Reason = ledger.ReasonRepeat
	}
	// The same message tried again: by the alarm, or its stream, and the
	// evaluation it is of, as Oracle suggests telling copies apart.
	at := m.Timestamp
	if m.Millis != nil {
		at = strconv.FormatInt(*m.Millis, 10)
	}
	n.Msg = "oci:" + m.DedupeKey + "/" + at
	if len(n.Msg) > 200 {
		h := sha256.Sum256([]byte(n.Msg))
		n.Msg = "oci:" + hex.EncodeToString(h[:])
	}
	for _, a := range m.AlarmMetaData {
		if a.ID == "" {
			return nil, errors.New("an alarm in alarmMetaData without its id")
		}
		labels := map[string]string{"alarm_id": a.ID}
		if sev := strings.ToLower(cmpOr(a.Severity, m.Severity)); sev != "" {
			labels["severity"] = sev
		}
		if a.Namespace != "" {
			labels["namespace"] = a.Namespace
		}
		// A split message is of one metric stream, named by its dimensions;
		// a grouped one, of the alarm as a whole, as its firing is.
		if strings.HasPrefix(cmpOr(a.NotificationType, m.NotificationType), "Split") && len(a.Dimensions) == 1 {
			textOf(a.Dimensions[0], labels)
		}
		n.Alerts = append(n.Alerts, ledger.Alert{Rule: m.Title, Labels: labels, Start: start, Resolved: kind == "resolved"})
	}
	return []ledger.Notification{n}, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ociHost is a host of OCI Notifications' API, notification.REGION.DOMAIN,
// in one of Oracle's realms: oraclecloud.com, oraclecloudN.com,
// oraclegovcloud.com or .uk, oraclecloud.eu. Oracle shows a confirmation
// URL only as a placeholder, so a cell in front, cellN., is taken too: all
// of them are Oracle's names.
var ociHost = regexp.MustCompile(`^(?:cell[0-9]{1,2}\.)?notification\.[a-z]{2}(?:-[a-z]+){1,2}-[0-9]{1,2}\.(?:oraclecloud[0-9]{0,2}\.com|oraclegovcloud\.(?:com|uk)|oraclecloud\.eu)$`)

// ociURL reports whether a URL is https to a Notifications API host, as
// written, with nothing to send elsewhere: no user, no port, nothing
// escaped in the host.
func ociURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.User == nil && u.Port() == "" && ociHost.MatchString(u.Host) &&
		strings.HasPrefix(raw, "https://"+u.Host+"/")
}

// ociGet fetches a subscription's confirmation URL, following no redirect,
// within ten seconds and 64 KB.
func ociGet(ctx context.Context, raw string) ([]byte, error) {
	if !ociURL(raw) {
		return nil, errors.New("not a Notifications API host")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		// The error names the URL, which carries the subscription's token.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return b, err
}

// confirmOCI confirms a subscription after the request that brought its URL
// is answered: only a URL on a Notifications API host, each once in a
// while, a few a minute, one at a time. Any other is logged by its host
// alone, for someone to confirm the subscription with `oci ons subscription
// confirm`: the URL carries a token, and the unsubscribe URL that comes
// with each message is never logged.
func (s *Server) confirmOCI(raw, topic string) {
	host := ""
	if u, err := url.Parse(raw); err == nil {
		host = u.Host
	}
	if !ociURL(raw) {
		s.log.Warn("an OCI Notifications subscription's confirmation URL isn't on a Notifications API host: confirm it with oci ons subscription confirm", "topic", topic, "host", host)
		return
	}
	c := &s.port.oci
	c.mu.Lock()
	if at, ok := c.done[raw]; ok && time.Since(at) < 10*time.Minute {
		c.mu.Unlock()
		return
	}
	if !c.fetches.allow(time.Now()) {
		c.mu.Unlock()
		s.log.Warn("too many OCI subscriptions confirmed lately: Notifications sends the confirmation again", "topic", topic)
		return
	}
	if len(c.done) >= 64 {
		clear(c.done)
	}
	c.done[raw] = time.Now()
	c.mu.Unlock()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.fetchMu.Lock() // one at a time
		defer c.fetchMu.Unlock()
		if _, err := c.get(context.Background(), raw); err != nil {
			s.log.Warn("couldn't confirm an OCI Notifications subscription: confirm it with oci ons subscription confirm", "topic", topic, "host", host, "err", err)
			return
		}
		s.log.Info("confirmed an OCI Notifications subscription", "topic", topic, "host", host)
	}()
}
