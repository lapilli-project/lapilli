package serve

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// readCounter is a body that says whether anything read it.
type readCounter struct {
	r    *bytes.Reader
	read bool
}

func (b *readCounter) Read(p []byte) (int, error) {
	b.read = true
	return b.r.Read(p)
}

// A request without the token is refused before its body is read, on the
// paths the token alone proves; on the paths a signature of the body
// proves, only a few bodies are read at once before they have.
func TestNotifyBodyAfterProof(t *testing.T) {
	s := notifyServer(t, nil)
	h := s.NotifyHandler()
	am := notifyFixture(t, "alertmanager.json")
	for _, path := range []string{"/v1/alertmanager", "/v1/azure", "/v1/google"} {
		body := &readCounter{r: bytes.NewReader(am)}
		r := httptest.NewRequest(http.MethodPost, path, body)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized || body.read {
			t.Errorf("%s without the token: %d, body read %v", path, w.Code, body.read)
		}
	}
	// The places for bodies from anyone all taken: the next is answered
	// 503, which SNS tries again.
	for range preAuth {
		s.port.preAuth <- struct{}{}
	}
	body := &readCounter{r: bytes.NewReader(am)}
	r := httptest.NewRequest(http.MethodPost, "/v1/sns", body)
	r.Header.Set("X-Amz-Sns-Message-Type", "Notification")
	r.Header.Set("X-Amz-Sns-Topic-Arn", s.cfg.NotifySNS[0])
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || body.read {
		t.Errorf("SNS with every place taken: %d, body read %v", w.Code, body.read)
	}
	// With the token, a body is read whatever else is arriving.
	if code, out, _ := send(t, h, "/v1/alertmanager", am, bearer(notifyToken)); code != 200 {
		t.Errorf("with the token: %d %s", code, out)
	}
	for range preAuth {
		<-s.port.preAuth
	}
}

// A certificate URL is taken only as SNS spells it, on the host of the
// topic's region: another spelling of the same certificate isn't fetched.
func TestSNSCertSpelling(t *testing.T) {
	s := notifyServer(t, nil)
	g := newSNSSigner(t)
	s.port.sns.get = g.get
	h := s.NotifyHandler()
	subject := "[FIRING:1]"
	msg := func(id, cert string) []byte {
		return g.sign(t, snsMessage{Type: "Notification", MessageID: id, TopicArn: snsTopicARN, Subject: &subject,
			Message: string(notifyFixture(t, "amp-sns-message.json")), Timestamp: snsTime(now), SigningCertURL: cert})
	}
	path := strings.TrimPrefix(snsCertURL, "https://sns.us-east-1.amazonaws.com")
	for i, cert := range []string{
		"HTTPS://sns.us-east-1.amazonaws.com" + path,
		"Https://sns.us-east-1.amazonaws.com" + path,
		snsCertURL + "#",
		strings.Replace(snsCertURL, "/Simple", "/%53imple", 1),
		strings.ToUpper(snsCertURL[:len(snsCertURL)-4]) + ".pem",
		strings.Replace(snsCertURL, "us-east-1", "us-west-2", 1), // another region than the topic's
		"https://sns.us-east-1.amazonaws.com:443" + path,
	} {
		if code, out, _ := send(t, h, "/v1/sns", msg(fmt.Sprint("s-", i), cert), nil); code != http.StatusForbidden {
			t.Errorf("%s: %d %s", cert, code, out)
		}
	}
	if len(g.got) != 0 {
		t.Errorf("fetched %v", g.got)
	}
	if code, out, _ := send(t, h, "/v1/sns", msg("ok", snsCertURL), nil); code != 200 {
		t.Errorf("as SNS spells it: %d %s", code, out)
	}
	for arn, host := range map[string]string{
		"arn:aws:sns:us-east-1:123456789012:a":         "sns.us-east-1.amazonaws.com",
		"arn:aws-us-gov:sns:us-gov-west-1:1:a":         "sns.us-gov-west-1.amazonaws.com",
		"arn:aws-cn:sns:cn-north-1:1:a":                "sns.cn-north-1.amazonaws.com.cn",
		"arn:aws-eusc:sns:eusc-de-east-1:1:a":          "sns.eusc-de-east-1.amazonaws.eu",
		"arn:aws:sns:s3:1:a":                           "",
		"arn:aws:sns:us-east-1.evil.example#:1:a":      "",
		"arn:aws:sqs:us-east-1:123456789012:a":         "",
		"arn:aws:sns:us-east-1:123456789012:a:extra":   "",
		"arn:aws-other:sns:us-east-1:123456789012:a":   "",
		"arn:aws:sns:../../x:123456789012:a":           "",
		"arn:aws:sns:us-east-1:123456789012:a\nb:c:d:": "",
	} {
		got, ok := snsHostOf(arn)
		if !ok {
			got = ""
		}
		if got != host {
			t.Errorf("%q: %q, want %q", arn, got, host)
		}
	}
}

// A certificate that has verified a message stays, however many others
// are fetched for messages that don't hold; when it can't be fetched, or
// past the fetches allowed, a message is answered 503, which SNS tries
// again.
func TestSNSProvenStays(t *testing.T) {
	s := notifyServer(t, nil)
	g := newSNSSigner(t)
	s.port.sns.fetches = &limiter{rate: 1000, burst: 1000}
	s.port.sns.get = func(_ context.Context, url string) ([]byte, error) { return g.cert, nil }
	h := s.NotifyHandler()
	subject := "[FIRING:1]"
	good := func(id string) []byte {
		return g.sign(t, snsMessage{Type: "Notification", MessageID: id, TopicArn: snsTopicARN, Subject: &subject,
			Message: string(notifyFixture(t, "amp-sns-message.json")), Timestamp: snsTime(now), SigningCertURL: snsCertURL})
	}
	if code, out, _ := send(t, h, "/v1/sns", good("g-1"), nil); code != 200 {
		t.Fatalf("a signed message: %d %s", code, out)
	}
	// 40 messages that don't hold, each under a certificate of its own.
	for i := range 40 {
		m := snsMessage{Type: "Notification", MessageID: fmt.Sprint("x-", i), TopicArn: snsTopicARN, Message: "{}", Timestamp: snsTime(now),
			SignatureVersion: "2", Signature: base64.StdEncoding.EncodeToString(make([]byte, 256)),
			SigningCertURL: fmt.Sprintf("https://sns.us-east-1.amazonaws.com/SimpleNotificationService-%016x.pem", 5000+i)}
		b, _ := json.Marshal(m)
		if code, _, _ := send(t, h, "/v1/sns", b, nil); code != http.StatusForbidden {
			t.Fatalf("an unsigned message: %d", code)
		}
	}
	if !s.port.sns.proven[snsCertURL] || s.port.sns.certs[snsCertURL] == nil {
		t.Fatalf("the certificate that verified a message is gone")
	}
	// No fetch is allowed now, nor would one work: the certificate kept
	// verifies the next message.
	s.port.sns.fetches = &limiter{rate: 0.0001, burst: 0}
	s.port.sns.get = func(context.Context, string) ([]byte, error) { return nil, errors.New("down") }
	if code, out, _ := send(t, h, "/v1/sns", good("g-2"), nil); code != 200 {
		t.Errorf("a signed message after them: %d %s", code, out)
	}
	// A certificate that can't be fetched now: 503.
	s.port.sns.fetches = &limiter{rate: 1000, burst: 1000}
	m := snsMessage{Type: "Notification", MessageID: "t-1", TopicArn: snsTopicARN, Message: "{}", Timestamp: snsTime(now),
		SignatureVersion: "2", Signature: base64.StdEncoding.EncodeToString(make([]byte, 256)),
		SigningCertURL: "https://sns.us-east-1.amazonaws.com/SimpleNotificationService-00000000000000ff.pem"}
	b, _ := json.Marshal(m)
	if code, _, hdr := send(t, h, "/v1/sns", b, nil); code != http.StatusServiceUnavailable || hdr.Get("Retry-After") == "" {
		t.Errorf("a certificate that can't be fetched now: %d, Retry-After %q", code, hdr.Get("Retry-After"))
	}
}

// A message SNS signed more than an hour ago, or ahead of the clock, isn't
// taken; the same message again is answered as kept without a turn, and
// one source's notifications don't use up another's.
func TestSNSReplays(t *testing.T) {
	s := notifyServer(t, nil)
	g := newSNSSigner(t)
	s.port.sns.get = g.get
	h := s.NotifyHandler()
	subject := "[FIRING:1]"
	msg := func(id string, at time.Time) []byte {
		return g.sign(t, snsMessage{Type: "Notification", MessageID: id, TopicArn: snsTopicARN, Subject: &subject,
			Message: string(notifyFixture(t, "amp-sns-message.json")), Timestamp: snsTime(at), SigningCertURL: snsCertURL})
	}
	for name, at := range map[string]time.Time{"two hours old": now.Add(-2 * time.Hour), "ten minutes ahead": now.Add(10 * time.Minute)} {
		if code, out, _ := send(t, h, "/v1/sns", msg("old-"+name, at), nil); code != http.StatusForbidden {
			t.Errorf("%s: %d %s", name, code, out)
		}
	}
	if len(g.got) != 0 {
		t.Errorf("fetched %v for messages refused before", g.got)
	}
	one := msg("m-1", now)
	if code, out, _ := send(t, h, "/v1/sns", one, nil); code != 200 || !strings.Contains(out, `"kept":1`) {
		t.Fatalf("a message: %d %s", code, out)
	}
	// SNS's turns used up: the same message again is still answered.
	s.port.rates["sns"] = &limiter{rate: 0.0001, burst: 0}
	for range 3 {
		if code, out, _ := send(t, h, "/v1/sns", one, nil); code != 200 || !strings.Contains(out, `"same_as_kept":1`) {
			t.Errorf("the same message again: %d %s", code, out)
		}
	}
	if code, _, _ := send(t, h, "/v1/sns", msg("m-2", now), nil); code != http.StatusTooManyRequests {
		t.Errorf("another message past SNS's turns: %d", code)
	}
	if code, out, _ := send(t, h, "/v1/alertmanager", notifyFixture(t, "alertmanager.json"), bearer(notifyToken)); code != 200 {
		t.Errorf("Alertmanager's, SNS's turns used up: %d %s", code, out)
	}
}

// Whether SNS's topic is taken isn't said: a topic not taken and a
// signature that doesn't hold get the same answer.
func TestSNSTopicNotSaid(t *testing.T) {
	s := notifyServer(t, nil)
	g := newSNSSigner(t)
	s.port.sns.get = g.get
	h := s.NotifyHandler()
	subject := "[FIRING:1]"
	n := snsMessage{Type: "Notification", MessageID: "a", TopicArn: "arn:aws:sns:us-east-1:210987654321:alerts", Subject: &subject,
		Message: "{}", Timestamp: snsTime(now), SigningCertURL: snsCertURL}
	_, other, _ := send(t, h, "/v1/sns", g.sign(t, n), nil)
	n.TopicArn, n.MessageID = snsTopicARN, "b"
	var m snsMessage
	json.Unmarshal(g.sign(t, n), &m)
	m.Message = "changed"
	b, _ := json.Marshal(m)
	code, bad, _ := send(t, h, "/v1/sns", b, nil)
	if code != http.StatusForbidden || other != bad {
		t.Errorf("another topic %q, a bad signature %d %q", other, code, bad)
	}
}

// A Grafana signature is taken once: sent again within its five minutes,
// past the minute two alike are one, it is answered as kept and kept no
// more.
func TestGrafanaReplay(t *testing.T) {
	s := notifyServer(t, nil)
	h := s.NotifyHandler()
	body := notifyFixture(t, "grafana.json")
	ts := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(grafanaSecret))
	mac.Write([]byte(ts + ":"))
	mac.Write(body)
	hdr := map[string]string{"X-Grafana-Alerting-Signature": hex.EncodeToString(mac.Sum(nil)), "X-Grafana-Alerting-Timestamp": ts}
	if code, out, _ := send(t, h, "/v1/grafana", body, hdr); code != 200 || !strings.Contains(out, `"kept":1`) {
		t.Fatalf("signed: %d %s", code, out)
	}
	s.now = func() time.Time { return now.Add(3 * time.Minute) }
	if code, out, _ := send(t, h, "/v1/grafana", body, hdr); code != 200 || !strings.Contains(out, `"same_as_kept":1`) {
		t.Errorf("the same signature three minutes on: %d %s", code, out)
	}
	if ns := kept(t, s); len(ns) != 1 {
		t.Errorf("kept %d", len(ns))
	}
}

// Names a notification carries are shown: one with a character that
// doesn't show, or too long, or with too many labels, is refused 400,
// which an alert manager doesn't send again.
func TestNotifyNames(t *testing.T) {
	s := notifyServer(t, nil)
	h := s.NotifyHandler()
	var base map[string]any
	json.Unmarshal(notifyFixture(t, "alertmanager.json"), &base)
	with := func(change func(alert map[string]any, m map[string]any)) []byte {
		var m map[string]any
		b, _ := json.Marshal(base)
		json.Unmarshal(b, &m)
		change(m["alerts"].([]any)[0].(map[string]any), m)
		out, _ := json.Marshal(m)
		return out
	}
	labels := func(a map[string]any) map[string]any { return a["labels"].(map[string]any) }
	many := map[string]any{"alertname": "Errors"}
	for i := range 64 {
		many[fmt.Sprint("l", i)] = "v"
	}
	for name, body := range map[string][]byte{
		"a zero-width space in the alertname": with(func(a, _ map[string]any) { labels(a)["alertname"] = "Errors​" }),
		"a newline in the receiver":           with(func(_, m map[string]any) { m["receiver"] = "on\ncall" }),
		"a long alertname":                    with(func(a, _ map[string]any) { labels(a)["alertname"] = strings.Repeat("x", 513) }),
		"a long receiver":                     with(func(_, m map[string]any) { m["receiver"] = strings.Repeat("x", 201) }),
		"65 labels":                           with(func(a, _ map[string]any) { a["labels"] = many }),
		"labels over 4 KB":                    with(func(a, _ map[string]any) { labels(a)["big"] = strings.Repeat("x", 4100) }),
		"a label name that doesn't show":      with(func(a, _ map[string]any) { labels(a)["a‮b"] = "v" }),
	} {
		if code, out, _ := send(t, h, "/v1/alertmanager", body, bearer(notifyToken)); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, code, out)
		}
	}
	// The receiver a URL gives, for a source whose notifications don't.
	az := notifyFixture(t, "azure-fired.json")
	if code, out, _ := send(t, h, "/v1/azure?receiver=%FF%FE", az, basicAuth("x", notifyToken)); code != http.StatusBadRequest {
		t.Errorf("?receiver= not UTF-8: %d %s", code, out)
	}
	if ns := kept(t, s); len(ns) != 0 {
		t.Errorf("kept %d", len(ns))
	}
}

// /metrics counts notifications only for rules the ledger has, by a rule's
// busiest receivers and the rest as other; and the alerts kept in a day
// are bounded.
func TestNotifyMetricsBounded(t *testing.T) {
	s := notifyServer(t, nil)
	h := s.NotifyHandler()
	var base map[string]any
	json.Unmarshal(notifyFixture(t, "alertmanager.json"), &base)
	post := func(rule, receiver string) (int, string) {
		var m map[string]any
		b, _ := json.Marshal(base)
		json.Unmarshal(b, &m)
		m["receiver"], m["groupKey"] = receiver, rule+receiver
		m["alerts"].([]any)[0].(map[string]any)["labels"].(map[string]any)["alertname"] = rule
		out, _ := json.Marshal(m)
		code, body, _ := send(t, h, "/v1/alertmanager", out, bearer(notifyToken))
		return code, body
	}
	for i := range 12 {
		if code, out := post("Errors", fmt.Sprint("team-", i)); code != 200 {
			t.Fatalf("%d: %d %s", i, code, out)
		}
	}
	if code, out := post("NoSuchRule", "oncall"); code != 200 {
		t.Fatalf("an unknown rule: %d %s", code, out)
	}
	_, body, _ := get(t, s.MetricsHandler(), "/metrics")
	if strings.Contains(body, "NoSuchRule") {
		t.Errorf("a rule the ledger doesn't have in /metrics")
	}
	n := strings.Count(body, `lapilli_rule_notifications{rule="Errors"`)
	if n != maxReceiverSeries+1 || !strings.Contains(body, `receiver="other"`) {
		t.Errorf("%d series for Errors; other: %v", n, strings.Contains(body, `receiver="other"`))
	}

	defer func(was int) { maxAlertsPerDay = was }(maxAlertsPerDay)
	maxAlertsPerDay = 14
	s.port.day.start = time.Time{} // counted again from the ledger
	if code, out := post("Errors", "team-x"); code != 200 {
		t.Errorf("the fourteenth alert today: %d %s", code, out)
	}
	if code, out := post("Errors", "team-y"); code != http.StatusTooManyRequests {
		t.Errorf("the fifteenth alert today: %d %s", code, out)
	}
}
