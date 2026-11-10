package serve

import (
	"context"
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
		"sns.cn-north-1.amazonaws.com.cn": true, "sns.il-central-1.amazonaws.com": true,
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

// Messages that name an allowed topic, unsigned, can't make the port fetch
// without end: a few certificates a minute, one at a time, and a URL that
// gave none isn't tried again soon. A certificate fetched before stays.
func TestSNSFetchesBounded(t *testing.T) {
	s := notifyServer(t, nil)
	g := newSNSSigner(t)
	s.port.sns.get = g.get
	h := s.NotifyHandler()
	subject := "[FIRING:1]"
	n := snsMessage{Type: "Notification", MessageID: "m-1", TopicArn: snsTopicARN, Subject: &subject, Message: string(notifyFixture(t, "amp-sns-message.json")),
		Timestamp: "2026-11-02T06:02:30.000Z", SigningCertURL: snsCertURL}
	if code, body, _ := send(t, h, "/v1/sns", g.sign(t, n), nil); code != 200 {
		t.Fatalf("a signed message: %d %s", code, body)
	}
	unsigned := func(url string) {
		t.Helper()
		m := snsMessage{Type: "Notification", MessageID: "x", TopicArn: snsTopicARN, Message: "{}", Timestamp: "2026-11-02T06:00:00Z",
			SignatureVersion: "2", Signature: "AAAA", SigningCertURL: url}
		b, _ := json.Marshal(m)
		if code, _, _ := send(t, h, "/v1/sns", b, nil); code != http.StatusForbidden {
			t.Fatalf("an unsigned message: %d", code)
		}
	}
	before := len(g.got)
	for i := range 40 {
		unsigned(fmt.Sprintf("https://sns.us-east-1.amazonaws.com/SimpleNotificationService-%d.pem", i))
	}
	if got := len(g.got) - before; got > 5 {
		t.Errorf("%d fetches for 40 unsigned messages, each naming its own URL", got)
	}
	// One that gave nothing isn't tried again.
	before = len(g.got)
	unsigned("https://sns.us-east-1.amazonaws.com/SimpleNotificationService-0.pem")
	if len(g.got) != before {
		t.Errorf("a URL that gave nothing was fetched again")
	}
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
