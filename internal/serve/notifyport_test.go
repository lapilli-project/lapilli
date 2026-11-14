package serve

import (
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

const (
	notifyToken   = "a-notify-token-long-enough"
	grafanaSecret = "a-grafana-secret-long-enough"
	snsTopicARN   = "arn:aws:sns:us-east-1:123456789012:alerts"
	snsCertURL    = "https://sns.us-east-1.amazonaws.com/SimpleNotificationService-0123456789abcdef.pem"
)

func notifyFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "notify", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// notifyServer is a server taking notifications on every path, with the
// backfill run: the Errors firing for checkout from 06:02 on 2 November,
// which caught INC-1, and twenty for search that caught nothing.
func notifyServer(t *testing.T, change func(*Config)) *Server {
	t.Helper()
	ctx := context.Background()
	token, secret := tokenFile(t, notifyToken), tokenFile(t, grafanaSecret)
	s, _ := server(t, func(c *Config) {
		c.NotifyListen, c.NotifyToken, c.NotifyGrafanaSecret, c.NotifySNS = "127.0.0.1:0", token, secret, []string{snsTopicARN}
		if change != nil {
			change(c)
		}
	})
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	return s
}

// send posts a notification to the port, with headers, as the source would.
func send(t *testing.T, h http.Handler, path string, body []byte, hdr map[string]string) (int, string, http.Header) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if strings.HasPrefix(path, "/v1/sns") {
		snsHeaders(r, body)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.String(), w.Header()
}

// snsHeaders are the headers SNS sends with a message: its type and topic.
func snsHeaders(r *http.Request, body []byte) {
	var m struct{ Type, TopicArn string }
	json.Unmarshal(body, &m)
	r.Header.Set("X-Amz-Sns-Message-Type", m.Type)
	r.Header.Set("X-Amz-Sns-Topic-Arn", m.TopicArn)
}

func bearer(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

func basicAuth(user, pass string) map[string]string {
	return map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))}
}

func kept(t *testing.T, s *Server) []ledger.Notification {
	t.Helper()
	ns, err := s.l.Notifications(context.Background(), time.Unix(0, 0), now.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return ns
}

// Each path takes what proves itself its own way, and nothing else.
func TestNotifyPortChecks(t *testing.T) {
	s := notifyServer(t, nil)
	h := s.NotifyHandler()
	am := notifyFixture(t, "alertmanager.json")

	for name, hdr := range map[string]map[string]string{
		"no token":      nil,
		"a wrong token": bearer("a-wrong-token-long-enough"),
		"basic auth":    basicAuth("x", notifyToken),
	} {
		if code, _, hd := send(t, h, "/v1/alertmanager", am, hdr); code != http.StatusUnauthorized || hd.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("alertmanager, %s: %d %v", name, code, hd)
		}
	}
	for _, path := range []string{"/v1/azure", "/v1/google"} {
		if code, _, hd := send(t, h, path, am, bearer(notifyToken)); code != http.StatusUnauthorized || !strings.HasPrefix(hd.Get("WWW-Authenticate"), "Basic ") {
			t.Errorf("%s with a bearer token: %d %v", path, code, hd)
		}
		if code, _, _ := send(t, h, path, am, basicAuth("lapilli", "a-wrong-token-long-enough")); code != http.StatusUnauthorized {
			t.Errorf("%s with a wrong password: %d", path, code)
		}
	}
	if code, _, _ := send(t, h, "/v1/grafana", notifyFixture(t, "grafana.json"), nil); code != http.StatusUnauthorized {
		t.Errorf("grafana, nothing: %d", code)
	}
	if len(kept(t, s)) != 0 {
		t.Fatal("something refused was kept")
	}

	// Too big, too many alerts, not the source's JSON.
	if code, _, _ := send(t, h, "/v1/alertmanager", bytes.Repeat([]byte(" "), notifyMax+1), bearer(notifyToken)); code != http.StatusRequestEntityTooLarge {
		t.Errorf("a body over 1 MB: %d", code)
	}
	var m map[string]any
	json.Unmarshal(am, &m)
	alerts := m["alerts"].([]any)
	for len(alerts) <= ledger.MaxAlerts {
		alerts = append(alerts, alerts[0])
	}
	m["alerts"] = alerts
	many, _ := json.Marshal(m)
	if code, body, _ := send(t, h, "/v1/alertmanager", many, bearer(notifyToken)); code != http.StatusRequestEntityTooLarge || !strings.Contains(body, "max_alerts") {
		t.Errorf("1,001 alerts: %d %s", code, body)
	}
	if code, _, _ := send(t, h, "/v1/alertmanager", notifyFixture(t, "grafana.json"), bearer(notifyToken)); code != http.StatusBadRequest {
		t.Errorf("Grafana's JSON as Alertmanager's: %d", code)
	}
	if code, _, _ := send(t, h, "/v1/azure", am, basicAuth("lapilli", notifyToken)); code != http.StatusBadRequest {
		t.Errorf("Alertmanager's JSON as Azure's: %d", code)
	}
	zero := bytes.Replace(am, []byte("2026-11-02T06:02:15.123Z"), []byte("0001-01-01T00:00:00Z"), 1)
	if code, body, _ := send(t, h, "/v1/alertmanager", zero, bearer(notifyToken)); code != http.StatusBadRequest || !strings.Contains(body, "when it started") {
		t.Errorf("an alert that never started: %d %s", code, body)
	}
	if len(kept(t, s)) != 0 {
		t.Fatal("something refused was kept")
	}

	// A token that can't be used lets nothing in.
	short := tokenFile(t, "short")
	s.cfg.NotifyToken = short
	if code, _, _ := send(t, h, "/v1/alertmanager", am, bearer("short")); code != http.StatusUnauthorized {
		t.Errorf("a token under 16 characters: %d", code)
	}
}

func TestNotifyAlertmanager(t *testing.T) {
	s := notifyServer(t, nil)
	h := s.NotifyHandler()
	am := notifyFixture(t, "alertmanager.json")
	s.now = func() time.Time { return time.Date(2026, 11, 2, 6, 2, 30, 0, time.UTC) }
	if code, body, _ := send(t, h, "/v1/alertmanager", am, bearer(notifyToken)); code != 200 || !strings.Contains(body, `"kept":1`) {
		t.Fatalf("alertmanager: %d %s", code, body)
	}
	// The second replica, 20 s later: the same notification.
	s.now = func() time.Time { return time.Date(2026, 11, 2, 6, 2, 50, 0, time.UTC) }
	if code, body, _ := send(t, h, "/v1/alertmanager", am, bearer(notifyToken)); code != 200 || !strings.Contains(body, `"same_as_kept":1`) {
		t.Fatalf("the second replica: %d %s", code, body)
	}
	// Four hours on, the same again, from an Alertmanager that doesn't say
	// why: a repeat, worked out. Then resolved.
	var m map[string]any
	json.Unmarshal(am, &m)
	delete(m, "notification_reason")
	again, _ := json.Marshal(m)
	s.now = func() time.Time { return time.Date(2026, 11, 2, 10, 2, 30, 0, time.UTC) }
	send(t, h, "/v1/alertmanager", again, bearer(notifyToken))
	s.now = func() time.Time { return time.Date(2026, 11, 2, 10, 5, 0, 0, time.UTC) }
	send(t, h, "/v1/alertmanager", notifyFixture(t, "alertmanager-0.27.json"), bearer(notifyToken))

	ns := kept(t, s)
	var got []string
	for _, n := range ns {
		got = append(got, n.Source+" "+n.Receiver+" "+n.Reason)
	}
	if want := []string{"alertmanager oncall first", "alertmanager oncall repeat", "alertmanager oncall resolved"}; !reflect.DeepEqual(got, want) {
		t.Errorf("kept %v, want %v", got, want)
	}
	a := ns[0].Alerts[0]
	if a.Rule != "Errors" || a.Labels["cluster"] != "eu-1" || !a.Start.Equal(time.Date(2026, 11, 2, 6, 2, 15, 0, time.UTC)) {
		t.Errorf("alert %+v", a)
	}
}

// Grafana's webhook, with the token, or signed: with a timestamp, or
// without one; not with a stale timestamp or a wrong signature.
func TestNotifyGrafana(t *testing.T) {
	s := notifyServer(t, nil)
	h := s.NotifyHandler()
	body := notifyFixture(t, "grafana.json")
	sign := func(ts string, b []byte, secret string) string {
		mac := hmac.New(sha256.New, []byte(secret))
		if ts != "" {
			mac.Write([]byte(ts + ":"))
		}
		mac.Write(b)
		return hex.EncodeToString(mac.Sum(nil))
	}
	at := now.Unix()
	for _, c := range []struct {
		name string
		hdr  map[string]string
		ok   bool
	}{
		{"the token", bearer(notifyToken), true},
		{"signed with a timestamp", map[string]string{"X-Grafana-Alerting-Signature": sign(strconv.FormatInt(at, 10), body, grafanaSecret),
			"X-Grafana-Alerting-Timestamp": strconv.FormatInt(at, 10)}, true},
		{"signed without one", map[string]string{"X-Grafana-Alerting-Signature": sign("", body, grafanaSecret)}, true},
		{"a stale timestamp", map[string]string{"X-Grafana-Alerting-Signature": sign(strconv.FormatInt(at-600, 10), body, grafanaSecret),
			"X-Grafana-Alerting-Timestamp": strconv.FormatInt(at-600, 10)}, false},
		{"a timestamp left out of the signature", map[string]string{"X-Grafana-Alerting-Signature": sign("", body, grafanaSecret),
			"X-Grafana-Alerting-Timestamp": strconv.FormatInt(at, 10)}, false},
		{"another secret", map[string]string{"X-Grafana-Alerting-Signature": sign("", body, "another-secret-long-enough")}, false},
		{"another body", map[string]string{"X-Grafana-Alerting-Signature": sign("", append([]byte(" "), body...), grafanaSecret)}, false},
	} {
		code, out, _ := send(t, h, "/v1/grafana?x="+strconv.Itoa(len(c.name)), body, c.hdr)
		if c.ok != (code == 200) {
			t.Errorf("%s: %d %s", c.name, code, out)
		}
	}
	ns := kept(t, s)
	if len(ns) != 1 || ns[0].Receiver != "My Super Webhook" || ns[0].Reason != ledger.ReasonFirst ||
		!ns[0].Alerts[0].Start.Equal(time.Date(2026, 11, 2, 6, 2, 15, 0, time.UTC)) {
		t.Errorf("kept %+v: want one, the others the same within a minute", ns)
	}
}

// Azure's action group webhook, the token as the URL's password; Google
// Cloud's, which sends without it until asked.
func TestNotifyAzureGoogle(t *testing.T) {
	s := notifyServer(t, nil)
	h := s.NotifyHandler()
	auth := basicAuth("lapilli", notifyToken)
	if code, body, _ := send(t, h, "/v1/azure?receiver=ag-oncall", notifyFixture(t, "azure-fired.json"), auth); code != 200 {
		t.Fatalf("azure fired: %d %s", code, body)
	}
	s.now = func() time.Time { return now.Add(time.Hour) }
	if code, body, _ := send(t, h, "/v1/azure?receiver=ag-oncall", notifyFixture(t, "azure-resolved.json"), auth); code != 200 {
		t.Fatalf("azure resolved: %d %s", code, body)
	}
	code, _, hd := send(t, h, "/v1/google", notifyFixture(t, "google.json"), nil)
	if code != http.StatusUnauthorized || !strings.HasPrefix(hd.Get("WWW-Authenticate"), "Basic realm=") {
		t.Fatalf("google's first request: %d %v", code, hd)
	}
	s.now = func() time.Time { return now.Add(2 * time.Hour) }
	if code, body, _ := send(t, h, "/v1/google", notifyFixture(t, "google.json"), auth); code != 200 {
		t.Fatalf("google: %d %s", code, body)
	}
	s.now = func() time.Time { return now.Add(3 * time.Hour) }
	send(t, h, "/v1/google", notifyFixture(t, "google-renotify.json"), auth)

	var got []string
	for _, n := range kept(t, s) {
		a := n.Alerts[0]
		got = append(got, fmt.Sprintf("%s %s %s %s %s %v %s", n.Source, n.Receiver, n.Reason, a.Rule, a.Start.Format(time.RFC3339), a.Resolved, a.Labels["service"]+"/"+a.Labels["severity"]))
	}
	want := []string{
		"azure ag-oncall first Errors 2026-11-02T06:02:40Z false checkout/page",
		"azure ag-oncall resolved Errors 2026-11-02T06:02:40Z true checkout/page",
		"google  first Errors 2026-11-02T06:02:15Z false checkout/page",
		"google  repeat Errors 2026-11-02T06:02:15Z false checkout/page",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("kept\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// snsSigner signs SNS messages as SNS does, with a certificate the test
// serves from an SNS URL.
type snsSigner struct {
	key  *rsa.PrivateKey
	cert []byte
	mu   sync.Mutex
	got  []string // the URLs fetched
}

func newSNSSigner(t *testing.T) *snsSigner {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "sns.amazonaws.com"},
		NotBefore: now.AddDate(-1, 0, 0), NotAfter: now.AddDate(1, 0, 0)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &snsSigner{key: key, cert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (g *snsSigner) get(_ context.Context, url string) ([]byte, error) {
	g.mu.Lock()
	g.got = append(g.got, url)
	g.mu.Unlock()
	if !snsURL(url) {
		return nil, errors.New("not an SNS host")
	}
	if url == snsCertURL {
		return g.cert, nil
	}
	return []byte("<ConfirmSubscriptionResponse/>"), nil
}

// snsTime is a time as SNS writes a message's Timestamp.
func snsTime(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05.000Z") }

func (g *snsSigner) sign(t *testing.T, m snsMessage) []byte {
	t.Helper()
	var sig []byte
	var err error
	switch m.SignatureVersion {
	case "1":
		h := sha1.Sum(m.signed())
		sig, err = rsa.SignPKCS1v15(rand.Reader, g.key, crypto.SHA1, h[:])
	default:
		m.SignatureVersion = "2"
		h := sha256.Sum256(m.signed())
		sig, err = rsa.SignPKCS1v15(rand.Reader, g.key, crypto.SHA256, h[:])
	}
	if err != nil {
		t.Fatal(err)
	}
	m.Signature = base64.StdEncoding.EncodeToString(sig)
	b, _ := json.Marshal(m)
	return b
}

func TestNotifySNS(t *testing.T) {
	s := notifyServer(t, nil)
	g := newSNSSigner(t)
	s.port.sns.get = g.get
	h := s.NotifyHandler()
	confirm := snsMessage{Type: "SubscriptionConfirmation", MessageID: "c-1", Token: "tok", TopicArn: snsTopicARN,
		Message: "You have chosen to subscribe to the topic", SubscribeURL: "https://sns.us-east-1.amazonaws.com/?Action=ConfirmSubscription&Token=tok",
		Timestamp: snsTime(now), SigningCertURL: snsCertURL}
	if code, body, _ := send(t, h, "/v1/sns", g.sign(t, confirm), nil); code != 200 {
		t.Fatalf("confirmation: %d %s", code, body)
	}
	if len(g.got) != 2 || g.got[1] != confirm.SubscribeURL {
		t.Fatalf("fetched %v: want the certificate, then SubscribeURL", g.got)
	}
	subject := "[FIRING:1]"
	n := snsMessage{Type: "Notification", MessageID: "m-1", TopicArn: snsTopicARN, Subject: &subject, Message: string(notifyFixture(t, "amp-sns-message.json")),
		Timestamp: snsTime(now), SignatureVersion: "1", SigningCertURL: snsCertURL}
	if code, body, _ := send(t, h, "/v1/sns", g.sign(t, n), nil); code != 200 || !strings.Contains(body, `"kept":1`) {
		t.Fatalf("notification: %d %s", code, body)
	}
	s.now = func() time.Time { return now.Add(time.Hour) }
	if code, body, _ := send(t, h, "/v1/sns", g.sign(t, n), nil); code != 200 || !strings.Contains(body, `"same_as_kept":1`) {
		t.Errorf("the same message an hour later: %d %s", code, body)
	}

	// Refused: a message changed after it was signed; one from another
	// topic; a certificate or a SubscribeURL off SNS's hosts.
	signed := g.sign(t, n)
	var tampered snsMessage
	json.Unmarshal(signed, &tampered)
	tampered.MessageID = "m-2"
	tb, _ := json.Marshal(tampered)
	other := n
	other.TopicArn, other.MessageID = "arn:aws:sns:us-east-1:210987654321:alerts", "m-3"
	offHost := n
	offHost.MessageID, offHost.SigningCertURL = "m-4", "https://sns.us-east-1.amazonaws.com.evil.example/x.pem"
	badConfirm := confirm
	badConfirm.MessageID, badConfirm.SubscribeURL = "c-2", "https://evil.example/?Action=ConfirmSubscription"
	before := len(g.got)
	for name, body := range map[string][]byte{"tampered": tb, "another topic": g.sign(t, other), "a certificate off SNS": g.sign(t, offHost),
		"a SubscribeURL off SNS": g.sign(t, badConfirm)} {
		if code, out, _ := send(t, h, "/v1/sns", body, nil); code != http.StatusForbidden {
			t.Errorf("%s: %d %s", name, code, out)
		}
	}
	if len(g.got) != before {
		t.Errorf("fetched %v for what was refused", g.got[before:])
	}
	if ns := kept(t, s); len(ns) != 1 || ns[0].Source != "sns" || ns[0].Receiver != "oncall" ||
		!ns[0].Alerts[0].Start.Equal(time.Date(2026, 11, 2, 6, 2, 15, 0, time.UTC)) {
		t.Errorf("kept %+v", ns)
	}

	// Without --notify-sns, nothing from SNS.
	s2 := notifyServer(t, func(c *Config) { c.NotifySNS = nil })
	s2.port.sns.get = g.get
	if code, _, _ := send(t, s2.NotifyHandler(), "/v1/sns", g.sign(t, n), nil); code != http.StatusNotFound {
		t.Errorf("SNS without --notify-sns: %d", code)
	}
}

// What the notifications came to shows beside the firings: on the rules,
// a rule's page, its firings, the dashboard, the metrics and MCP. The stones
// don't change.
func TestNotificationsShown(t *testing.T) {
	ctx := context.Background()
	s := notifyServer(t, func(c *Config) { c.Options.Hours = verdict.DefaultHours(time.UTC) })
	h, nh := s.Handler(), s.NotifyHandler()
	_, reportBefore, _ := get(t, h, "/api/v1/report?from=2026-10-31T00:00:00Z&to=2026-11-03T12:00:00Z")

	// The checkout firing that caught INC-1, paged at once (at 06:02, while
	// people slept), then again four hours on; the search firings that caught nothing at 22:02 and
	// 23:02 on Sunday, while people slept, and at 15:02, while they didn't.
	page := func(at time.Time, service string, start time.Time, reason string) {
		t.Helper()
		m := map[string]any{"version": "4", "groupKey": "{}:{alertname=\"Errors\",service=\"" + service + "\"}", "status": "firing", "receiver": "oncall",
			"notification_reason": reason, "alerts": []map[string]any{{"status": "firing", "startsAt": start.Format(time.RFC3339),
				"labels": map[string]string{"alertname": "Errors", "service": service, "severity": "page", "cluster": "eu-1"}}}}
		b, _ := json.Marshal(m)
		s.now = func() time.Time { return at }
		if code, body, _ := send(t, nh, "/v1/alertmanager", b, bearer(notifyToken)); code != 200 {
			t.Fatalf("%d %s", code, body)
		}
	}
	co := time.Date(2026, 11, 2, 6, 2, 0, 0, time.UTC)
	page(co.Add(30*time.Second), "checkout", co.Add(15*time.Second), "first notification")
	page(co.Add(4*time.Hour), "checkout", co.Add(15*time.Second), "repeat interval elapsed")
	for _, at := range []time.Time{time.Date(2026, 11, 1, 22, 2, 0, 0, time.UTC), time.Date(2026, 11, 1, 23, 2, 0, 0, time.UTC), time.Date(2026, 11, 1, 15, 2, 0, 0, time.UTC)} {
		page(at.Add(20*time.Second), "search", at, "first notification")
	}
	s.now = func() time.Time { return now }

	if _, body, _ := get(t, h, "/rules"); !strings.Contains(body, "5 notifications, 1 repeats") {
		t.Errorf("rules: %s", body)
	}
	_, body, _ := get(t, h, "/rules/Errors?tab=firings")
	if !strings.Contains(body, "5 notifications were sent for it: 1 of them repeats, 3 while people slept and 1 more off hours; 3 were for firings that caught nothing, and 0 tell of no firing") ||
		!strings.Contains(body, "By receiver: oncall 5.") || !strings.Contains(body, "counted from 2026-11-01") {
		t.Errorf("rule: %s", body)
	}
	// Of Sunday's firings for search, those at 15:02, 22:02 and 23:02 were
	// paged; the others since 15:02 paged no one; those before weren't
	// counted.
	_, body, _ = get(t, h, "/firings?day=2026-11-01")
	if n := strings.Count(body, "paged no one"); n != 6 {
		t.Errorf("Sunday: %d paged no one, want 6 (16:02 to 21:02): %s", n, body)
	}
	_, body, _ = get(t, h, "/firings?day=2026-11-02")
	if !strings.Contains(body, `<td class="n">2</td>`) {
		t.Errorf("Monday: the checkout firing told of twice: %s", body)
	}
	// The last whole week ended that Sunday: two notifications woke people
	// for nothing.
	_, body, _ = get(t, h, "/")
	i := strings.Index(body, "Woken for nothing")
	if i < 0 || !strings.Contains(body[i:min(len(body), i+400)], ">2<") ||
		!strings.Contains(body, "notifications for firings that caught nothing, while people slept, since 2026-11-01") {
		t.Errorf("overview: %s", body)
	}
	_, body, _ = get(t, s.MetricsHandler(), "/metrics")
	for _, want := range []string{`lapilli_rule_notifications{rule="Errors",receiver="oncall",reason="first"} 4`,
		`lapilli_rule_notifications{rule="Errors",receiver="oncall",reason="repeat"} 1`, "lapilli_serve_notifications_since_timestamp_seconds 17"} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %s: %s", want, body)
		}
	}
	_, out, err := s.toolRule(ctx, nil, ruleIn{Name: "Errors"})
	if err != nil {
		t.Fatal(err)
	}
	if n := out.Rule.Notifications; n == nil || n.Sent != 5 || n.Repeats != 1 || n.Asleep != 3 || n.ForNothing != 3 || n.ByReceiver["oncall"] != 5 {
		t.Errorf("mcp rule: %+v", n)
	}
	told := 0
	for _, f := range out.Latest {
		if f.Notified != nil {
			told++
		}
	}
	if told == 0 {
		t.Errorf("mcp latest firings say nothing of notifications: %+v", out.Latest)
	}

	_, reportAfter, _ := get(t, h, "/api/v1/report?from=2026-10-31T00:00:00Z&to=2026-11-03T12:00:00Z")
	if reportBefore != reportAfter {
		t.Error("notifications changed the stones")
	}

	// In each language, every text found.
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	for _, lang := range []string{"en", "ko"} {
		for _, p := range []string{"/", "/rules", "/rules/Errors", "/firings?day=2026-11-01"} {
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

// Without notifications, nothing of them shows, and the tile counts firings.
func TestNoNotificationsShown(t *testing.T) {
	s := notifyServer(t, func(c *Config) { c.Options.Hours = verdict.DefaultHours(time.UTC) })
	h := s.Handler()
	for _, p := range []string{"/rules", "/rules/Errors", "/firings?day=2026-11-01"} {
		if _, body, _ := get(t, h, p); strings.Contains(body, "notifications") || strings.Contains(body, "paged no one") {
			t.Errorf("%s: %s", p, body)
		}
	}
	if _, body, _ := get(t, h, "/"); !strings.Contains(body, "firings that caught nothing, 22:00–08:00") {
		t.Errorf("overview: %s", body)
	}
	if _, body, _ := get(t, s.MetricsHandler(), "/metrics"); strings.Contains(body, "lapilli_rule_notifications") {
		t.Errorf("metrics: %s", body)
	}
}
