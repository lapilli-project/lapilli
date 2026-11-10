package serve

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
)

// SNS hosts are named by a region: the S3 bucket named sns is not one.
func TestSNSHosts(t *testing.T) {
	for host, ok := range map[string]bool{
		"sns.us-east-1.amazonaws.com": true, "sns.ap-northeast-2.amazonaws.com": true, "sns.us-gov-west-1.amazonaws.com": true,
		"sns.cn-north-1.amazonaws.com.cn": true, "sns.il-central-1.amazonaws.com": true, "sns.eusc-de-east-1.amazonaws.eu": true,
		"sns.ap-southeast-5.amazonaws.com": true, "sns.eusc-de-east-1.amazonaws.com": false,
		"sns.s3.amazonaws.com": false, "sns.s3-accelerate.amazonaws.com": false, "sns.s3-external-1.amazonaws.com": false,
		"sns.s3.us-east-1.amazonaws.com": false, "sns.amazonaws.com": false, "sns.us-east-1.amazonaws.com.evil.example": false,
	} {
		if got := snsURL("https://" + host + "/x.pem"); got != ok {
			t.Errorf("%s: %v, want %v", host, got, ok)
		}
	}
}

// What SNS signs, field by field, as AWS's documentation lists them: the
// test signs with signed() too, so this holds it to the text.
func TestSNSSignedText(t *testing.T) {
	subject := "s"
	n := snsMessage{Type: "Notification", MessageID: "m", TopicArn: "arn", Subject: &subject, Message: "body", Timestamp: "ts"}
	if got := string(n.signed()); got != "Message\nbody\nMessageId\nm\nSubject\ns\nTimestamp\nts\nTopicArn\narn\nType\nNotification\n" {
		t.Errorf("notification: %q", got)
	}
	c := snsMessage{Type: "SubscriptionConfirmation", MessageID: "m", TopicArn: "arn", Message: "body", Timestamp: "ts", Token: "tok", SubscribeURL: "u"}
	if got := string(c.signed()); got != "Message\nbody\nMessageId\nm\nSubscribeURL\nu\nTimestamp\nts\nToken\ntok\nTopicArn\narn\nType\nSubscriptionConfirmation\n" {
		t.Errorf("confirmation: %q", got)
	}
}

// Messages that name an allowed topic, unsigned, can't keep a genuine one
// out: a URL with no certificate behind it is fetched once, and costs none
// of the fetches a certificate needs; real certificates are fetched a few
// a minute, and past them the port answers 503, which SNS tries again.
func TestSNSFetchesBounded(t *testing.T) {
	s := notifyServer(t, nil)
	g := newSNSSigner(t)
	s.port.sns.get = g.get
	h := s.NotifyHandler()
	unsigned := func(url string) int {
		t.Helper()
		m := snsMessage{Type: "Notification", MessageID: "x", TopicArn: snsTopicARN, Message: "{}", Timestamp: snsTime(now),
			SignatureVersion: "2", Signature: base64.StdEncoding.EncodeToString(make([]byte, 256)), SigningCertURL: url}
		b, _ := json.Marshal(m)
		code, _, _ := send(t, h, "/v1/sns", b, nil)
		return code
	}
	junk := func(i int) string {
		return fmt.Sprintf("https://sns.us-east-1.amazonaws.com/SimpleNotificationService-%032x.pem", i)
	}
	// As after a restart, with no certificate kept: forty names with none
	// behind them, each fetched once, each refused.
	for i := range 40 {
		if code := unsigned(junk(i)); code != http.StatusForbidden {
			t.Fatalf("an unsigned message: %d", code)
		}
	}
	if len(g.got) != 40 {
		t.Errorf("%d fetches for 40 names", len(g.got))
	}
	before := len(g.got)
	unsigned(junk(0))
	if len(g.got) != before {
		t.Errorf("a URL that gave nothing was fetched again")
	}
	// The genuine message still has its certificate fetched.
	subject := "[FIRING:1]"
	n := snsMessage{Type: "Notification", MessageID: "m-1", TopicArn: snsTopicARN, Subject: &subject, Message: string(notifyFixture(t, "amp-sns-message.json")),
		Timestamp: snsTime(now), SigningCertURL: snsCertURL}
	if code, body, _ := send(t, h, "/v1/sns", g.sign(t, n), nil); code != 200 {
		t.Fatalf("a signed message after them: %d %s", code, body)
	}
	// Names that are certificates are fetched a few at a time: past them,
	// 503, not 403, which SNS would drop.
	s.port.sns.get = func(_ context.Context, url string) ([]byte, error) {
		g.mu.Lock()
		g.got = append(g.got, url)
		g.mu.Unlock()
		return g.cert, nil
	}
	before = len(g.got)
	last := 0
	for i := range 10 {
		last = unsigned(junk(1000 + i))
	}
	if got := len(g.got) - before; got > 5 || last != http.StatusServiceUnavailable {
		t.Errorf("%d certificates fetched for 10 names, the last answered %d; want 5 at most, then 503", got, last)
	}
	// One fetched before stays.
	n.MessageID = "m-2"
	if code, body, _ := send(t, h, "/v1/sns", g.sign(t, n), nil); code != 200 {
		t.Errorf("a signed message after them: %d %s", code, body)
	}
}

// An Azure alert that isn't a Prometheus rule's carries the labels
// recipes/firings/azure-monitor.jq gives its firing, and ties to it.
func TestAzureTiesRecorded(t *testing.T) {
	s := notifyServer(t, nil)
	ctx := context.Background()
	start := time.Date(2026, 11, 2, 7, 0, 0, 0, time.UTC)
	o := s.cfg.Options
	labels := map[string]string{"severity": "Sev3", "resource": "db-1", "resource_group": "data", "disk": "/var"}
	if _, err := s.l.PutFirings(ctx, "azure-shop", start.Add(-time.Hour), start.Add(time.Hour), []firing.Firing{{Rule: "disk-full", Labels: labels, Start: start}}, ledger.FiringsCopier, o); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"schemaId": "azureMonitorCommonAlertSchema", "data": {"essentials": {
		"alertId": "/subscriptions/s/providers/Microsoft.AlertsManagement/alerts/a1", "alertRule": "disk-full", "severity": "Sev3",
		"signalType": "Metric", "monitorCondition": "Fired", "monitoringService": "Platform",
		"alertTargetIDs": ["/subscriptions/s/resourceGroups/data/providers/Microsoft.Compute/virtualMachines/db-1"],
		"configurationItems": ["db-1"], "firedDateTime": %q},
		"alertContext": {"condition": {"allOf": [{"metricName": "disk", "dimensions": [{"name": "disk", "value": "/var"}]}]}}}}`, start.Format(time.RFC3339))
	if code, out, _ := send(t, s.NotifyHandler(), "/v1/azure", []byte(body), basicAuth("x", notifyToken)); code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: start.Add(-time.Hour), To: start.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	var mine []ledger.Firing
	for _, f := range fs {
		if f.Rule == "disk-full" {
			mine = append(mine, f)
		}
	}
	of, err := s.l.NotifiedOf(ctx, mine, func(string) time.Duration { return 2 * time.Minute })
	if err != nil || len(mine) != 1 || of[mine[0].ID] != 1 {
		t.Errorf("the recorded firing told of %v times (%v)", of, err)
	}
}

// The notifications token can't be the incidents or the firings token: it
// sits in Azure's and Google Cloud's webhook URLs.
func TestNotifyTokenOwn(t *testing.T) {
	s := notifyServer(t, func(c *Config) { c.IncidentsListen, c.IncidentsToken = "127.0.0.1:0", tokenFile(t, notifyToken) })
	body := notifyFixture(t, "azure-fired.json")
	if code, _, _ := send(t, s.NotifyHandler(), "/v1/azure", body, basicAuth("x", notifyToken)); code != http.StatusUnauthorized {
		t.Errorf("the incidents token as the notifications token: %d", code)
	}
	os.WriteFile(s.cfg.IncidentsToken, []byte("an-incidents-token-of-its-own"), 0o600)
	if code, out, _ := send(t, s.NotifyHandler(), "/v1/azure", body, basicAuth("x", notifyToken)); code != 200 {
		t.Errorf("each its own: %d %s", code, out)
	}
}

// The certificate cache can't be filled with one certificate under many
// names, and a certificate not cached before still gets in when it is full.
func TestSNSCertCache(t *testing.T) {
	s := notifyServer(t, nil)
	g := newSNSSigner(t)
	s.port.sns.fetches = &limiter{rate: 1000, burst: 1000}
	fetched := 0
	s.port.sns.get = func(_ context.Context, url string) ([]byte, error) {
		fetched++
		return g.cert, nil
	}
	h := s.NotifyHandler()
	subject := "[FIRING:1]"
	msg := func(id, cert string) []byte {
		n := snsMessage{Type: "Notification", MessageID: id, TopicArn: snsTopicARN, Subject: &subject, Message: string(notifyFixture(t, "amp-sns-message.json")),
			Timestamp: snsTime(now), SigningCertURL: cert}
		return g.sign(t, n)
	}
	for i, suffix := range []string{"", "#1.pem", "?x=1.pem"} {
		code, _, _ := send(t, h, "/v1/sns", msg(fmt.Sprint("f-", i), snsCertURL+suffix), nil)
		if want := map[bool]int{true: 200, false: http.StatusForbidden}[suffix == ""]; code != want {
			t.Errorf("%q: %d, want %d", suffix, code, want)
		}
	}
	if fetched != 1 {
		t.Errorf("fetched %d times", fetched)
	}
	for i := range 40 {
		cert := fmt.Sprintf("https://sns.us-east-1.amazonaws.com/SimpleNotificationService-%016x.pem", 1000+i)
		if code, out, _ := send(t, h, "/v1/sns", msg(fmt.Sprint("m-", i), cert), nil); code != 200 {
			t.Fatalf("certificate %d, the cache full: %d %s", i, code, out)
		}
	}
	if n := len(s.port.sns.certs); n > 32 {
		t.Errorf("%d certificates kept", n)
	}
	// With the cache full, a new certificate is fetched once and kept: the
	// next message under it needs no fetch, which the limit would refuse.
	s.port.sns.fetches = &limiter{rate: 0.001, burst: 1}
	cert := "https://sns.us-east-1.amazonaws.com/SimpleNotificationService-000000000000beef.pem"
	for i := range 3 {
		if code, out, _ := send(t, h, "/v1/sns", msg(fmt.Sprint("n-", i), cert), nil); code != 200 {
			t.Errorf("message %d under a certificate new to a full cache: %d %s", i, code, out)
		}
	}
}
