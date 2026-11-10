package serve

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lapilli.dev/internal/ledger"
)

// The notifications port takes notifications as an alert manager sent
// them, and nothing else, so a NetworkPolicy can let the alert manager
// reach it and no one else. Each source has a path of its own, since each
// proves itself its own way:
//
//	/v1/alertmanager  a bearer token (Alertmanager's http_config.authorization)
//	/v1/grafana       the same token, or Grafana's HMAC-SHA256 signature
//	/v1/sns           Amazon SNS's signature, from a topic in --notify-sns
//	/v1/azure         the token as the password in the action group's URL
//	/v1/google        the token as the password, after a 401 asking for it
//
// It doesn't check the host name the request names, as the UI's port does
// against DNS rebinding: every request here carries its own proof, which a
// browser doesn't add of itself, and the alert managers reach it by names
// of their own, Google Cloud's by a public one.

// notifyMax is the largest body taken.
const notifyMax = 1 << 20

// notifyPort is what the port keeps between requests.
type notifyPort struct {
	rate   *limiter
	busy   chan struct{}
	warned atomic.Bool // a token or secret that can't be used was logged
	sns    snsTrust
}

// snsTrust is how SNS messages are checked: get fetches a signing
// certificate or confirms a subscription, from an SNS host only; certs keeps
// the certificates fetched. Tests give get a stand-in.
type snsTrust struct {
	get   func(ctx context.Context, url string) ([]byte, error)
	mu    sync.Mutex
	certs map[string]*x509.Certificate
}

func newNotifyPort() *notifyPort {
	p := &notifyPort{rate: &limiter{rate: 20, burst: 100}, busy: make(chan struct{}, 1)}
	p.sns.get = snsGet
	p.sns.certs = map[string]*x509.Certificate{}
	return p
}

// NotifyHandler is the notifications port.
func (s *Server) NotifyHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/alertmanager", s.takeNotifications("alertmanager", s.bearer, parseAlertmanager))
	mux.HandleFunc("POST /v1/grafana", s.takeNotifications("grafana", s.grafanaSigned, parseGrafana))
	mux.HandleFunc("POST /v1/sns", s.takeNotifications("sns", s.snsSigned, s.parseSNS))
	mux.HandleFunc("POST /v1/azure", s.takeNotifications("azure", s.basic, parseAzure))
	mux.HandleFunc("POST /v1/google", s.takeNotifications("google", s.basic, parseGoogle))
	return headers(mux)
}

// errRefused is a request that didn't prove itself; the check has answered.
var errRefused = errors.New("refused")

// tooMany is a notification with more alerts than are taken.
type tooMany struct{ n int }

func (e tooMany) Error() string {
	return fmt.Sprintf("%d alerts in one notification; %d at most (Alertmanager's max_alerts can keep it under)", e.n, ledger.MaxAlerts)
}

// takeNotifications reads a request's body, has check say whether it
// proves itself (answering it when it doesn't), parse read the
// notifications in it, and keeps them, one write at a time.
func (s *Server) takeNotifications(source string, check func(http.ResponseWriter, *http.Request, []byte) bool,
	parse func(r *http.Request, body []byte, now time.Time) ([]ledger.Notification, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.ReadOnly {
			http.Error(w, "lapilli serve is read-only (--read-only)", http.StatusForbidden)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, notifyMax))
		if err != nil {
			http.Error(w, "a notification of 1 MB at most", http.StatusRequestEntityTooLarge)
			return
		}
		if !check(w, r, body) {
			return
		}
		// An alert manager sends in bursts, and tries again; someone with the
		// token shouldn't get more than that.
		if !s.port.rate.allow(time.Now()) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many notifications: try again in a second", http.StatusTooManyRequests)
			return
		}
		ns, err := parse(r, body, s.now())
		var many tooMany
		switch {
		case errors.As(err, &many):
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		case errors.Is(err, errRefused):
			http.Error(w, "refused", http.StatusForbidden)
			return
		case err != nil:
			http.Error(w, source+": "+err.Error(), http.StatusBadRequest)
			return
		}
		// One write at a time, waiting a little for the turn: a burst is
		// taken, not refused.
		t := time.NewTimer(5 * time.Second)
		defer t.Stop()
		select {
		case s.port.busy <- struct{}{}:
			defer func() { <-s.port.busy }()
		case <-t.C:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "busy keeping other notifications: try again in a second", http.StatusTooManyRequests)
			return
		case <-r.Context().Done():
			return
		}
		kept := 0
		for _, n := range ns {
			n.Source = source
			ok, err := s.l.Notify(r.Context(), n)
			switch {
			case errors.Is(err, ledger.ErrBadNotification):
				http.Error(w, source+": "+err.Error(), http.StatusBadRequest)
				return
			case errors.Is(err, context.Canceled):
				return
			case err != nil:
				s.log.Error("keeping a notification", "source", source, "err", err)
				http.Error(w, "the notification couldn't be kept: try again", http.StatusInternalServerError)
				return
			}
			if ok {
				kept++
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "{\"kept\":%d,\"same_as_kept\":%d}\n", kept, len(ns)-kept)
	}
}

// secret reads a file holding a token or a secret, for each request, so it
// can be rotated: "" when it can't be read or is shorter than 16
// characters, which is logged once.
func (s *Server) secret(file, what string) string {
	b, err := os.ReadFile(file)
	v := strings.TrimSpace(string(b))
	if err != nil || len(v) < minToken {
		if s.port.warned.CompareAndSwap(false, true) {
			s.log.Warn("the "+what+" can't be read, or is shorter than 16 characters: no notifications are taken until it is put right", "err", err)
		}
		return ""
	}
	s.port.warned.Store(false)
	return v
}

// same compares two tokens, hashed first, so it takes as long whatever
// their lengths.
func same(got, want string) bool {
	if got == "" || want == "" {
		return false
	}
	x, y := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}

// bearer takes the token in --notify-token-file as a bearer token.
func (s *Server) bearer(w http.ResponseWriter, r *http.Request, _ []byte) bool {
	got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if s.cfg.NotifyToken != "" && same(got, s.secret(s.cfg.NotifyToken, "notifications token")) {
		return true
	}
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, "this path takes the token in --notify-token-file, as a bearer token", http.StatusUnauthorized)
	return false
}

// basic takes the token as the password of basic authentication, whatever
// the user, as an action group's or a webhook channel's URL carries it.
// Google Cloud sends the first request without it, and the password only
// once asked for with a 401.
func (s *Server) basic(w http.ResponseWriter, r *http.Request, _ []byte) bool {
	_, got, ok := r.BasicAuth()
	if ok && s.cfg.NotifyToken != "" && same(got, s.secret(s.cfg.NotifyToken, "notifications token")) {
		return true
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="lapilli notifications", charset="UTF-8"`)
	http.Error(w, "this path takes the token in --notify-token-file as the password of basic authentication", http.StatusUnauthorized)
	return false
}

// grafanaSigned takes the token as a bearer token, or Grafana's HMAC-SHA256
// signature of the body with the secret in --notify-grafana-secret-file:
// in X-Grafana-Alerting-Signature, and, when Grafana sends a timestamp in
// X-Grafana-Alerting-Timestamp, signed with it, and no more than five
// minutes off.
func (s *Server) grafanaSigned(w http.ResponseWriter, r *http.Request, body []byte) bool {
	if got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && s.cfg.NotifyToken != "" &&
		same(got, s.secret(s.cfg.NotifyToken, "notifications token")) {
		return true
	}
	if sig := r.Header.Get("X-Grafana-Alerting-Signature"); sig != "" && s.cfg.NotifyGrafanaSecret != "" {
		if secret := s.secret(s.cfg.NotifyGrafanaSecret, "Grafana signing secret"); secret != "" {
			mac := hmac.New(sha256.New, []byte(secret))
			ts := r.Header.Get("X-Grafana-Alerting-Timestamp")
			fresh := true
			if ts != "" {
				n, err := strconv.ParseInt(ts, 10, 64)
				d := s.now().Sub(time.Unix(n, 0))
				fresh = err == nil && d < 5*time.Minute && d > -5*time.Minute
				mac.Write([]byte(ts + ":"))
			}
			mac.Write(body)
			want := mac.Sum(nil)
			got, err := hex.DecodeString(strings.TrimSpace(sig))
			if err == nil && fresh && hmac.Equal(got, want) {
				return true
			}
		}
	}
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, "this path takes the token in --notify-token-file as a bearer token, or Grafana's HMAC signature with the secret in --notify-grafana-secret-file", http.StatusUnauthorized)
	return false
}

// The alerts of Alertmanager's and Grafana's webhooks, and of what AWS's
// JSON template for Amazon Managed Service for Prometheus sends to SNS.
type amMessage struct {
	Version            string            `json:"version"`
	GroupKey           string            `json:"groupKey"`
	Status             string            `json:"status"`
	Receiver           string            `json:"receiver"`
	GroupLabels        map[string]string `json:"groupLabels"`
	NotificationReason string            `json:"notification_reason"`
	Alerts             []amAlert         `json:"alerts"`
}

type amAlert struct {
	Status   string            `json:"status"`
	Labels   map[string]string `json:"labels"`
	StartsAt string            `json:"startsAt"`
}

// amReasons are Alertmanager's notification reasons (0.32.0 on), as the
// ledger keeps them; one not here is worked out.
var amReasons = map[string]string{
	"first notification":      ledger.ReasonFirst,
	"new alerts added":        ledger.ReasonNew,
	"some alerts resolved":    ledger.ReasonResolved,
	"all alerts resolved":     ledger.ReasonResolved,
	"repeat interval elapsed": ledger.ReasonRepeat,
}

// startsAt reads an alert's start: RFC 3339, or as a Go template writes a
// time, as AWS's JSON template for SNS does ("2026-11-09 03:10:00 +0000
// UTC").
func startsAt(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	if i := strings.Index(s, " m="); i > 0 {
		s = s[:i] // a monotonic reading
	}
	for _, layout := range []string{"2006-01-02 15:04:05.999999999 -0700 MST", "2006-01-02 15:04:05.999999999 -0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("startsAt %q isn't a time", s)
}

// receiverOf is the receiver a notification names, or the one the URL
// gives (?receiver=) for a source whose notifications don't.
func receiverOf(r *http.Request, named string) string {
	if named != "" {
		return named
	}
	return r.URL.Query().Get("receiver")
}

func (m amMessage) notification(r *http.Request, now time.Time) (ledger.Notification, error) {
	n := ledger.Notification{At: now, Receiver: receiverOf(r, m.Receiver), Group: m.GroupKey, Reason: amReasons[m.NotificationReason]}
	if len(m.Alerts) > ledger.MaxAlerts {
		return n, tooMany{len(m.Alerts)}
	}
	if len(m.Alerts) == 0 {
		return n, errors.New("no alerts")
	}
	if n.Group == "" {
		// What it was grouped by stands for the group key.
		b, _ := json.Marshal(noneIfNil(m.GroupLabels))
		n.Group = string(b)
	}
	for i, a := range m.Alerts {
		rule := a.Labels["alertname"]
		if rule == "" {
			return n, fmt.Errorf("alert %d has no alertname", i+1)
		}
		start, err := startsAt(a.StartsAt)
		if err != nil {
			return n, fmt.Errorf("alert %d: %w", i+1, err)
		}
		n.Alerts = append(n.Alerts, ledger.Alert{Rule: rule, Labels: a.Labels, Start: start, Resolved: a.Status == "resolved"})
	}
	return n, nil
}

func noneIfNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func parseAlertmanager(r *http.Request, body []byte, now time.Time) ([]ledger.Notification, error) {
	var m amMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("not Alertmanager's webhook JSON: %w", err)
	}
	if m.Version != "4" {
		return nil, fmt.Errorf("version %q: want Alertmanager's webhook, version 4", m.Version)
	}
	n, err := m.notification(r, now)
	return []ledger.Notification{n}, err
}

// parseGrafana reads Grafana's webhook. Its alerts are named by alertname,
// the rule's title, as Lapilli names Grafana's rules; it says no reason.
func parseGrafana(r *http.Request, body []byte, now time.Time) ([]ledger.Notification, error) {
	var m amMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("not Grafana's webhook JSON: %w", err)
	}
	if m.Version != "1" {
		return nil, fmt.Errorf("version %q: want Grafana's webhook, version 1, without a custom payload", m.Version)
	}
	m.NotificationReason = ""
	n, err := m.notification(r, now)
	return []ledger.Notification{n}, err
}

// azureAlert is an Azure Monitor alert in the common alert schema.
type azureAlert struct {
	SchemaID string `json:"schemaId"`
	Data     struct {
		Essentials struct {
			AlertID          string `json:"alertId"`
			AlertRule        string `json:"alertRule"`
			Severity         string `json:"severity"`
			MonitorCondition string `json:"monitorCondition"`
			FiredDateTime    string `json:"firedDateTime"`
		} `json:"essentials"`
		AlertContext struct {
			Labels map[string]string `json:"labels"`
		} `json:"alertContext"`
	} `json:"data"`
}

// parseAzure reads an action group's webhook in the common alert schema: a
// Prometheus rule group's alert carries its labels, the rule's severity
// among them as Sev0 to Sev4; other alerts carry none.
func parseAzure(r *http.Request, body []byte, now time.Time) ([]ledger.Notification, error) {
	var a azureAlert
	if err := json.Unmarshal(body, &a); err != nil {
		return nil, fmt.Errorf("not Azure Monitor's alert JSON: %w", err)
	}
	if a.SchemaID != "azureMonitorCommonAlertSchema" {
		return nil, fmt.Errorf("schemaId %q: turn on the common alert schema for this action", a.SchemaID)
	}
	e := a.Data.Essentials
	if e.AlertRule == "" {
		return nil, errors.New("no alertRule")
	}
	start, err := time.Parse(time.RFC3339Nano, e.FiredDateTime)
	if err != nil {
		return nil, fmt.Errorf("firedDateTime %q isn't a time", e.FiredDateTime)
	}
	labels := map[string]string{}
	for k, v := range a.Data.AlertContext.Labels {
		labels[k] = v
	}
	if _, ok := labels["severity"]; !ok && e.Severity != "" {
		labels["severity"] = e.Severity
	}
	return []ledger.Notification{{At: now, Receiver: receiverOf(r, ""), Group: e.AlertID,
		Alerts: []ledger.Alert{{Rule: e.AlertRule, Labels: labels, Start: start, Resolved: e.MonitorCondition == "Resolved"}}}}, nil
}

// googleIncident is Cloud Monitoring's webhook, schema 1.2.
type googleIncident struct {
	Version  string `json:"version"`
	Incident struct {
		IncidentID       string            `json:"incident_id"`
		Renotify         bool              `json:"renotify"`
		StartedAt        *int64            `json:"started_at"`
		State            string            `json:"state"`
		Severity         string            `json:"severity"`
		PolicyName       string            `json:"policy_name"`
		PolicyUserLabels map[string]string `json:"policy_user_labels"`
		Resource         struct {
			Labels map[string]string `json:"labels"`
		} `json:"resource"`
		Metric struct {
			Labels map[string]string `json:"labels"`
		} `json:"metric"`
		Condition struct {
			PromQL *struct {
				AlertRule string `json:"alertRule"`
			} `json:"conditionPrometheusQueryLanguage"`
		} `json:"condition"`
	} `json:"incident"`
}

// parseGoogle reads a webhook channel's notification: a policy's incident,
// named as Lapilli names the policy (its PromQL condition's alertRule, or
// the policy's display name), labelled as recipes/firings labels it.
func parseGoogle(r *http.Request, body []byte, now time.Time) ([]ledger.Notification, error) {
	var g googleIncident
	if err := json.Unmarshal(body, &g); err != nil {
		return nil, fmt.Errorf("not Cloud Monitoring's webhook JSON: %w", err)
	}
	if g.Version != "1.2" {
		return nil, fmt.Errorf("version %q: want the webhook's schema 1.2", g.Version)
	}
	in := g.Incident
	rule := in.PolicyName
	if in.Condition.PromQL != nil && in.Condition.PromQL.AlertRule != "" {
		rule = in.Condition.PromQL.AlertRule
	}
	if rule == "" || in.StartedAt == nil {
		return nil, errors.New("an incident needs policy_name and started_at")
	}
	labels := map[string]string{}
	for _, m := range []map[string]string{in.PolicyUserLabels, in.Resource.Labels, in.Metric.Labels} {
		for k, v := range m {
			labels[k] = v
		}
	}
	if sev := strings.ToLower(in.Severity); slices.Contains([]string{"critical", "error", "warning"}, sev) {
		labels["severity"] = sev
	}
	n := ledger.Notification{At: now, Receiver: receiverOf(r, ""), Group: in.IncidentID,
		Alerts: []ledger.Alert{{Rule: rule, Labels: labels, Start: time.Unix(*in.StartedAt, 0).UTC(), Resolved: in.State == "closed"}}}
	if in.Renotify {
		n.Reason = ledger.ReasonRepeat
	}
	return []ledger.Notification{n}, nil
}

// snsMessage is what Amazon SNS posts to an HTTPS subscription.
type snsMessage struct {
	Type             string  `json:"Type"`
	MessageID        string  `json:"MessageId"`
	Token            string  `json:"Token"`
	TopicArn         string  `json:"TopicArn"`
	Subject          *string `json:"Subject"`
	Message          string  `json:"Message"`
	Timestamp        string  `json:"Timestamp"`
	SignatureVersion string  `json:"SignatureVersion"`
	Signature        string  `json:"Signature"`
	SigningCertURL   string  `json:"SigningCertURL"`
	SubscribeURL     string  `json:"SubscribeURL"`
}

// signed is what SNS signs: the message's fields, each name and value on a
// line of its own, in this order.
func (m snsMessage) signed() []byte {
	var b strings.Builder
	add := func(k, v string) { b.WriteString(k + "\n" + v + "\n") }
	add("Message", m.Message)
	add("MessageId", m.MessageID)
	if m.Type == "Notification" {
		if m.Subject != nil {
			add("Subject", *m.Subject)
		}
	} else {
		add("SubscribeURL", m.SubscribeURL)
	}
	add("Timestamp", m.Timestamp)
	if m.Type != "Notification" {
		add("Token", m.Token)
	}
	add("TopicArn", m.TopicArn)
	add("Type", m.Type)
	return []byte(b.String())
}

// snsHost is an Amazon SNS endpoint's host name: certificates come from
// one, subscriptions are confirmed at one, and nowhere else.
var snsHost = regexp.MustCompile(`^sns\.[a-z0-9-]+\.amazonaws\.com(\.cn)?$`)

// snsURL reports whether a URL is https to an SNS host.
func snsURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.User == nil && u.Port() == "" && snsHost.MatchString(u.Host)
}

// snsGet fetches from an SNS host, following no redirect.
func snsGet(ctx context.Context, raw string) ([]byte, error) {
	if !snsURL(raw) {
		return nil, fmt.Errorf("%q isn't an SNS host", raw)
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
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", raw, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<10))
}

// cert is the certificate at an SNS URL, fetched once.
func (t *snsTrust) cert(ctx context.Context, raw string) (*x509.Certificate, error) {
	if !snsURL(raw) || !strings.HasSuffix(raw, ".pem") {
		return nil, fmt.Errorf("SigningCertURL %q isn't a certificate on an SNS host", raw)
	}
	t.mu.Lock()
	c := t.certs[raw]
	t.mu.Unlock()
	if c != nil {
		return c, nil
	}
	b, err := t.get(ctx, raw)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("the signing certificate isn't PEM")
	}
	if c, err = x509.ParseCertificate(block.Bytes); err != nil {
		return nil, err
	}
	t.mu.Lock()
	if len(t.certs) >= 32 {
		clear(t.certs)
	}
	t.certs[raw] = c
	t.mu.Unlock()
	return c, nil
}

// verify checks a message's signature against its certificate.
func (t *snsTrust) verify(ctx context.Context, m snsMessage, now time.Time) error {
	c, err := t.cert(ctx, m.SigningCertURL)
	if err != nil {
		return err
	}
	if now.Before(c.NotBefore) || now.After(c.NotAfter) {
		return errors.New("the signing certificate is out of date")
	}
	pub, ok := c.PublicKey.(*rsa.PublicKey)
	if !ok {
		return errors.New("the signing certificate's key isn't RSA")
	}
	sig, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil {
		return errors.New("the signature isn't base64")
	}
	switch m.SignatureVersion {
	case "1":
		h := sha1.Sum(m.signed())
		return rsa.VerifyPKCS1v15(pub, crypto.SHA1, h[:], sig)
	case "2":
		h := sha256.Sum256(m.signed())
		return rsa.VerifyPKCS1v15(pub, crypto.SHA256, h[:], sig)
	}
	return fmt.Errorf("SignatureVersion %q", m.SignatureVersion)
}

// snsSigned takes a message SNS signed, from a topic --notify-sns names. A
// subscription to such a topic is confirmed here, by a GET of its
// SubscribeURL, on an SNS host only: the one request the port makes.
func (s *Server) snsSigned(w http.ResponseWriter, r *http.Request, body []byte) bool {
	if len(s.cfg.NotifySNS) == 0 {
		http.Error(w, "SNS isn't taken: give its topics in --notify-sns", http.StatusNotFound)
		return false
	}
	var m snsMessage
	if err := json.Unmarshal(body, &m); err != nil {
		http.Error(w, "not an SNS message", http.StatusBadRequest)
		return false
	}
	if !slices.Contains(s.cfg.NotifySNS, m.TopicArn) {
		http.Error(w, "a topic not in --notify-sns", http.StatusForbidden)
		return false
	}
	if err := s.port.sns.verify(r.Context(), m, s.now()); err != nil {
		s.log.Warn("refused an SNS message whose signature doesn't hold", "topic", m.TopicArn, "err", err)
		http.Error(w, "the signature doesn't hold", http.StatusForbidden)
		return false
	}
	switch m.Type {
	case "Notification":
		return true
	case "SubscriptionConfirmation":
		if !snsURL(m.SubscribeURL) {
			http.Error(w, "SubscribeURL isn't on an SNS host", http.StatusForbidden)
			return false
		}
		if _, err := s.port.sns.get(r.Context(), m.SubscribeURL); err != nil {
			s.log.Warn("couldn't confirm an SNS subscription", "topic", m.TopicArn, "err", err)
			http.Error(w, "couldn't confirm the subscription", http.StatusBadGateway)
			return false
		}
		s.log.Info("confirmed an SNS subscription", "topic", m.TopicArn)
	case "UnsubscribeConfirmation":
		s.log.Info("an SNS subscription was taken away", "topic", m.TopicArn)
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, "{\"kept\":0,\"same_as_kept\":0}\n")
	return false
}

// parseSNS reads the alerts in an SNS notification: what Amazon Managed
// Service for Prometheus's alert manager sends with AWS's JSON template.
// The message's id tells the same message sent again.
func (s *Server) parseSNS(r *http.Request, body []byte, now time.Time) ([]ledger.Notification, error) {
	var m snsMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	if m.MessageID == "" {
		return nil, errors.New("an SNS message without its MessageId")
	}
	var am amMessage
	if err := json.Unmarshal([]byte(m.Message), &am); err != nil {
		return nil, errors.New("the SNS message isn't JSON: give the alert manager AWS's JSON template for SNS")
	}
	n, err := am.notification(r, now)
	n.Msg = "sns:" + m.MessageID
	return []ledger.Notification{n}, err
}
