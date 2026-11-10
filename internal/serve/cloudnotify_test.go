package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
)

// at06 is when the tests' alarms moved into ALARM, to the second.
var at06 = time.Date(2026, 11, 2, 6, 2, 15, 0, time.UTC)

// copyIn puts a source's firings for the day of at06, as recipes/firings
// would write them.
func copyIn(t *testing.T, s *Server, source string, fs ...firing.Firing) {
	t.Helper()
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	if _, err := s.l.PutFirings(context.Background(), source, day, day.Add(24*time.Hour), fs, ledger.FiringsCopier, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
}

func pagesOf(t *testing.T, s *Server) []ledger.Page {
	t.Helper()
	ps, err := s.l.Pages(context.Background(), at06.Add(-24*time.Hour), now.Add(time.Hour), func(string) time.Duration { return 2 * time.Minute })
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

// signedSNS is a message as SNS posts it for a topic --notify-sns takes.
func signedSNS(t *testing.T, g *snsSigner, id string, message []byte) []byte {
	t.Helper()
	return g.sign(t, snsMessage{Type: "Notification", MessageID: id, TopicArn: snsTopicARN, Message: string(message),
		Timestamp: snsTime(now), SigningCertURL: snsCertURL})
}

// edit changes a fixture's JSON.
func edit(t *testing.T, b []byte, change func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	change(m)
	out, _ := json.Marshal(m)
	return out
}

// A CloudWatch alarm's messages on /v1/sns: told from Amazon Managed
// Service for Prometheus's by their shape, named and labelled as
// recipes/firings/cloudwatch.jq names and labels the alarm's firing, tied to
// the source the URL names though the ledger names its rule apart. A move
// out of ALARM resolves the alert the move in started; a move between OK
// and INSUFFICIENT_DATA is of no firing.
func TestNotifyCloudWatch(t *testing.T) {
	s := notifyServer(t, nil)
	g := newSNSSigner(t)
	s.port.sns.get = g.get
	h := s.NotifyHandler()
	dims := map[string]string{"InstanceId": "i-0123456789abcdef0", "path": "/data"}
	copyIn(t, s, "cw-prod", firing.Firing{Rule: "disk-full", Labels: dims, Start: at06.Add(time.Hour)})
	copyIn(t, s, "cw-dev", firing.Firing{Rule: "disk-full", Labels: dims, Start: at06, End: at06.Add(30 * time.Minute)})
	alarm := notifyFixture(t, "cloudwatch-alarm.json")
	if code, out, _ := send(t, h, "/v1/sns?source=cw-dev", signedSNS(t, g, "m-1", alarm), nil); code != 200 || !strings.Contains(out, `"kept":1`) {
		t.Fatalf("the alarm's message: %d %s", code, out)
	}
	ok := edit(t, alarm, func(m map[string]any) {
		m["NewStateValue"], m["OldStateValue"], m["StateChangeTime"] = "OK", "ALARM", "2026-11-02T06:32:15.000+0000"
	})
	if code, out, _ := send(t, h, "/v1/sns?source=cw-dev", signedSNS(t, g, "m-2", ok), nil); code != 200 || !strings.Contains(out, `"kept":1`) {
		t.Fatalf("its move to OK: %d %s", code, out)
	}
	noData := edit(t, alarm, func(m map[string]any) { m["NewStateValue"], m["OldStateValue"] = "INSUFFICIENT_DATA", "OK" })
	if code, out, _ := send(t, h, "/v1/sns?source=cw-dev", signedSNS(t, g, "m-3", noData), nil); code != 200 || !strings.Contains(out, `"kept":0`) {
		t.Errorf("a move from OK to INSUFFICIENT_DATA: %d %s", code, out)
	}
	// The same message, posted again to another source's URL, is kept once.
	if code, out, _ := send(t, h, "/v1/sns?source=cw-prod", signedSNS(t, g, "m-1", alarm), nil); code != 200 || strings.Contains(out, `"kept":1`) {
		t.Errorf("the same message under another source: %d %s", code, out)
	}
	ns := kept(t, s)
	if len(ns) != 2 {
		t.Fatalf("kept %+v", ns)
	}
	a, b := ns[0].Alerts[0], ns[1].Alerts[0]
	if a.Rule != "disk-full" || a.Labels["InstanceId"] != "i-0123456789abcdef0" || !a.Start.Equal(at06) || a.Resolved ||
		ns[0].Group != "arn:aws:cloudwatch:us-east-1:123456789012:alarm:disk-full" || ns[0].Copied != "cw-dev" {
		t.Errorf("the move in: %+v %+v", ns[0], a)
	}
	if !b.Resolved || !b.Start.Equal(at06) || ns[1].Reason != ledger.ReasonResolved {
		t.Errorf("the move out: %+v %+v", ns[1], b)
	}
	ps := pagesOf(t, s)
	if len(ps) != 2 || ps[0].Rule != "disk-full [cw-dev]" || len(ps[0].Firings) != 1 {
		t.Errorf("pages: %+v", ps)
	}
}

// An alarm on several series sends a message for each contributor, labelled
// with its attributes; so does EventBridge's state change, for the alarm
// and for a contributor.
func TestNotifyCloudWatchShapes(t *testing.T) {
	contributor := "arn:aws:cloudwatch:us-east-1:123456789012:alarm:DynamoDBInsightsAlarm/6d442278dba546f6"
	var s *Server
	var g *snsSigner
	var h http.Handler
	for _, want := range []struct {
		file, rule, group, label, value string
	}{
		{"cloudwatch-contributor.json", "DynamoDBInsightsAlarm", contributor, "TableName", "orders"},
		{"eventbridge-alarm.json", "disk-full", "arn:aws:cloudwatch:us-east-1:123456789012:alarm:disk-full", "path", "/data"},
		{"eventbridge-contributor.json", "DynamoDBInsightsAlarm", contributor, "TableName", "orders"},
	} {
		s = notifyServer(t, nil)
		g = newSNSSigner(t)
		s.port.sns.get = g.get
		h = s.NotifyHandler()
		if code, out, _ := send(t, h, "/v1/sns", signedSNS(t, g, "m-1", notifyFixture(t, want.file)), nil); code != 200 || !strings.Contains(out, `"kept":1`) {
			t.Fatalf("%s: %d %s", want.file, code, out)
		}
		ns := kept(t, s)
		if len(ns) != 1 {
			t.Fatalf("%s: kept %+v", want.file, ns)
		}
		a := ns[0].Alerts[0]
		if a.Rule != want.rule || ns[0].Group != want.group || a.Labels[want.label] != want.value || !a.Start.Equal(at06) {
			t.Errorf("%s: %+v %+v", want.file, ns[0], a)
		}
	}
	// A contributor's change says only where it went: out of ALARM, it
	// resolves.
	out := edit(t, notifyFixture(t, "eventbridge-contributor.json"), func(m map[string]any) {
		m["detail"].(map[string]any)["state"].(map[string]any)["value"] = "OK"
	})
	if code, body, _ := send(t, h, "/v1/sns", signedSNS(t, g, "m-out", out), nil); code != 200 || !strings.Contains(body, `"kept":1`) {
		t.Fatalf("the contributor's move out: %d %s", code, body)
	}
	if ns := kept(t, s); len(ns) != 2 || !ns[1].Alerts[0].Resolved || !ns[1].Alerts[0].Start.Equal(at06) {
		t.Errorf("the contributor's move out: %+v", ns)
	}
}

// A log search alert's resource is its target's, the last part of its id,
// not what the query found; the resource and its group are lower-cased, as
// recipes/firings/azure-monitor.jq writes them, so the two tie.
func TestNotifyAzureOwnAlerts(t *testing.T) {
	s := notifyServer(t, nil)
	h := s.NotifyHandler()
	labels := map[string]string{"severity": "Sev2", "resource": "shop-logs", "resource_group": "shop", "service": "checkout"}
	copyIn(t, s, "azure-shop", firing.Firing{Rule: "payment-errors", Labels: labels, Start: at06})
	if code, out, _ := send(t, h, "/v1/azure?source=azure-shop", notifyFixture(t, "azure-log.json"), basicAuth("x", notifyToken)); code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	ns := kept(t, s)
	if len(ns) != 1 {
		t.Fatalf("kept %+v", ns)
	}
	for k, v := range labels {
		if ns[0].Alerts[0].Labels[k] != v {
			t.Errorf("%s = %q, want %q", k, ns[0].Alerts[0].Labels[k], v)
		}
	}
	if ps := pagesOf(t, s); len(ps) != 1 || len(ps[0].Firings) != 1 {
		t.Errorf("pages: %+v", ps)
	}
	if code, _, _ := send(t, h, "/v1/azure?source=Azure_Shop", notifyFixture(t, "azure-log.json"), basicAuth("x", notifyToken)); code != http.StatusBadRequest {
		t.Errorf("a source no source could be named: %d", code)
	}
}

// Cloud Monitoring sends the same schema for a policy on a threshold, an
// absence or a log match: named by the policy, labelled with its user
// labels, its resource's and its metric's, as the recipe labels its firing.
func TestNotifyGoogleConditions(t *testing.T) {
	s := notifyServer(t, nil)
	h := s.NotifyHandler()
	for _, c := range []struct {
		file, rule, severity, label, value string
	}{
		{"google-threshold.json", "Disk full", "critical", "instance_id", "1234567890"},
		{"google-absence.json", "Heartbeat", "error", "zone", "europe-west1-b"},
		{"google-log.json", "Payment exceptions", "warning", "container_name", "checkout"},
	} {
		if code, out, _ := send(t, h, "/v1/google", notifyFixture(t, c.file), basicAuth("x", notifyToken)); code != 200 {
			t.Fatalf("%s: %d %s", c.file, code, out)
		}
		ns := kept(t, s)
		a := ns[len(ns)-1].Alerts[0]
		if a.Rule != c.rule || a.Labels["severity"] != c.severity || a.Labels[c.label] != c.value || a.Labels["team"] != "shop" ||
			!a.Start.Equal(time.Unix(1793599335, 0)) {
			t.Errorf("%s: %+v", c.file, a)
		}
	}
}

// ociSend posts to /v1/oci/SOURCE as OCI Notifications does, with the token
// as the password and the topic's name.
func ociSend(t *testing.T, h http.Handler, source string, body []byte, hdr map[string]string) (int, string) {
	t.Helper()
	all := map[string]string{"X-OCI-NS-TopicName": "alarms", "X-OCI-NS-MessageId": "id-" + time.Now().Format(time.RFC3339Nano)}
	for k, v := range basicAuth("lapilli", notifyToken) {
		all[k] = v
	}
	for k, v := range hdr {
		all[k] = v
	}
	code, out, _ := send(t, h, "/v1/oci/"+source, body, all)
	return code, out
}

// An OCI alarm's messages: an alert for the alarm (grouped) or for its one
// stream (split), labelled with the alarm's OCID, severity and namespace;
// REPEAT a repeat, FIRING_TO_OK and RESET resolving, each taking its start
// from the message that began it; a message tried again is kept once; the
// alarm's OCID ties it to its source's firing, whatever its title.
func TestNotifyOCI(t *testing.T) {
	s := notifyServer(t, nil)
	h := s.NotifyHandler()
	copyIn(t, s, "oci-prod", firing.Firing{Rule: "cpu-high", Start: at06,
		Labels: map[string]string{"severity": "warning", "namespace": "oci_computeagent", "alarm_id": "ocid1.alarm.oc1.ap-seoul-1.bbbb"}})
	grouped := edit(t, notifyFixture(t, "oci-grouped.json"), func(m map[string]any) { m["title"] = "WARNING alarm at 06:02" })
	if code, out := ociSend(t, h, "oci-prod", grouped, nil); code != 200 || !strings.Contains(out, `"kept":1`) {
		t.Fatalf("grouped: %d %s", code, out)
	}
	// Tried again five minutes on, under another message id: the same
	// alarm's same evaluation.
	clock := now.Add(5 * time.Minute)
	s.now = func() time.Time { return clock }
	if code, out := ociSend(t, h, "oci-prod", grouped, nil); code != 200 || !strings.Contains(out, `"same_as_kept":1`) {
		t.Errorf("tried again: %d %s", code, out)
	}
	later := func(kind string, minutes int64) []byte {
		clock = now.Add(time.Duration(minutes) * time.Minute)
		return edit(t, grouped, func(m map[string]any) {
			m["type"] = kind
			m["timestampEpochMillis"] = (at06.Unix() + minutes*60) * 1000
			m["timestamp"] = at06.Add(time.Duration(minutes) * time.Minute).Format(time.RFC3339)
		})
	}
	for _, c := range []struct {
		kind    string
		minutes int64
	}{{"REPEAT", 60}, {"FIRING_TO_OK", 90}, {"RESET", 120}} {
		if code, out := ociSend(t, h, "oci-prod", later(c.kind, c.minutes), nil); code != 200 || !strings.Contains(out, `"kept":1`) {
			t.Fatalf("%s: %d %s", c.kind, code, out)
		}
	}
	if code, out := ociSend(t, h, "oci-dev", notifyFixture(t, "oci-split.json"), nil); code != 200 {
		t.Fatalf("split: %d %s", code, out)
	}
	ns := kept(t, s)
	if len(ns) != 5 {
		t.Fatalf("kept %d: %+v", len(ns), ns)
	}
	first := ns[0].Alerts[0]
	if first.Rule != "cpu-high" || first.Labels["alarm_id"] != "ocid1.alarm.oc1.ap-seoul-1.bbbb" || first.Labels["severity"] != "warning" ||
		first.Labels["namespace"] != "oci_computeagent" || first.Labels["resourceDisplayName"] != "" || !first.Start.Equal(at06) ||
		ns[0].Receiver != "alarms" || ns[0].Group != "oci-dedupe-cpu-high" || ns[0].Copied != "oci-prod" {
		t.Errorf("grouped: %+v %+v", ns[0], first)
	}
	if ns[1].Reason != ledger.ReasonRepeat || !ns[1].Alerts[0].Start.Equal(at06) || ns[1].Alerts[0].Resolved {
		t.Errorf("repeat: %+v", ns[1])
	}
	for _, n := range ns[2:4] {
		if !n.Alerts[0].Resolved || !n.Alerts[0].Start.Equal(at06) {
			t.Errorf("resolved: %+v", n)
		}
	}
	if split := ns[4].Alerts[0]; split.Labels["resourceDisplayName"] != "web-0" || split.Labels["alarm_id"] == "" {
		t.Errorf("split: %+v", split)
	}
	if ps := pagesOf(t, s); len(ps) < 1 || ps[0].Rule != "cpu-high" || len(ps[0].Firings) != 1 {
		t.Errorf("pages: %+v", ps)
	}
	for name, c := range map[string]struct {
		source string
		body   []byte
	}{
		"a type not an alarm's":   {"oci-prod", edit(t, grouped, func(m map[string]any) { m["type"] = "OK_TO_SOMETHING" })},
		"no alarm in it":          {"oci-prod", edit(t, grouped, func(m map[string]any) { m["alarmMetaData"] = []any{} })},
		"a source never so named": {"OCI_Prod", grouped},
	} {
		if code, out := ociSend(t, h, c.source, c.body, nil); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, code, out)
		}
	}
}

// /v1/oci takes the token as the password, checked before the body is read;
// without it, a 401 asking for it, as Oracle requires of an endpoint.
func TestNotifyOCIProof(t *testing.T) {
	s := notifyServer(t, nil)
	h := s.NotifyHandler()
	for name, hdr := range map[string]map[string]string{
		"no token":    nil,
		"wrong token": basicAuth("lapilli", "not-the-notify-token-at-all"),
		"as bearer":   bearer(notifyToken),
	} {
		body := &readCounter{r: bytes.NewReader(notifyFixture(t, "oci-grouped.json"))}
		r := httptest.NewRequest(http.MethodPost, "/v1/oci/oci-prod", body)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized || !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Basic") || body.read {
			t.Errorf("%s: %d %q, body read %v", name, w.Code, w.Header().Get("WWW-Authenticate"), body.read)
		}
	}
	if len(kept(t, s)) != 0 {
		t.Error("kept something unproven")
	}
}

// A new subscription is confirmed by its URL, from a request that carries
// the token only, only from a Notifications API host over https, each URL
// once; any other is left for someone to confirm by hand.
func TestNotifyOCIConfirm(t *testing.T) {
	s := notifyServer(t, nil)
	h := s.NotifyHandler()
	var mu sync.Mutex
	var got []string
	s.port.oci.get = func(_ context.Context, url string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, url)
		return []byte("{}"), nil
	}
	good := "https://notification.ap-seoul-1.oraclecloud.com/20181201/subscriptions/ocid1.onssubscription.oc1.ap-seoul-1.x/confirmation?token=t&protocol=CUSTOM_HTTPS"
	confirm := func(url string, hdr map[string]string) int {
		t.Helper()
		all := map[string]string{"X-OCI-NS-ConfirmationURL": url}
		for k, v := range hdr {
			all[k] = v
		}
		code, _ := ociSend(t, h, "oci-prod", []byte(`{"ConfirmationURL": "`+url+`"}`), all)
		s.port.oci.wg.Wait()
		return code
	}
	if code := confirm(good, nil); code != 200 {
		t.Fatalf("a confirmation: %d", code)
	}
	confirm(good, nil) // once each
	for _, bad := range []string{
		"http://notification.ap-seoul-1.oraclecloud.com/20181201/subscriptions/x/confirmation?token=t",
		"https://notification.ap-seoul-1.oraclecloud.com.example.net/20181201/subscriptions/x/confirmation",
		"https://notification.ap-seoul-1.oraclecloud.com:8443/20181201/subscriptions/x/confirmation",
		"https://user@notification.ap-seoul-1.oraclecloud.com/20181201/subscriptions/x/confirmation",
		"https://notification.ap-seoul-1.oraclecloud.com%2eexample.net/x",
		"https://example.net/notification.ap-seoul-1.oraclecloud.com/",
		"https://169.254.169.254/latest/meta-data/",
		"https://sns.us-east-1.amazonaws.com/?Action=ConfirmSubscription",
	} {
		if code := confirm(bad, nil); code != 200 {
			t.Errorf("%s: %d", bad, code)
		}
	}
	// Without the token, nothing is read, let alone fetched.
	r := httptest.NewRequest(http.MethodPost, "/v1/oci/oci-prod", strings.NewReader(`{}`))
	r.Header.Set("X-OCI-NS-ConfirmationURL", "https://notification.ap-seoul-1.oraclecloud.com/20181201/subscriptions/other/confirmation?token=u")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	s.port.oci.wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if w.Code != http.StatusUnauthorized || len(got) != 1 || got[0] != good {
		t.Errorf("fetched %q; without the token: %d", got, w.Code)
	}
}

// OCI Notifications gives an endpoint five seconds: a message waits two for
// its turn at the ledger, then is answered 503, which it tries again.
func TestNotifyOCITurn(t *testing.T) {
	s := notifyServer(t, nil)
	h := s.NotifyHandler()
	s.port.busy <- struct{}{}
	defer func() { <-s.port.busy }()
	began := time.Now()
	code, _ := ociSend(t, h, "oci-prod", notifyFixture(t, "oci-grouped.json"), nil)
	if took := time.Since(began); code != http.StatusServiceUnavailable || took > 4*time.Second {
		t.Errorf("%d after %s", code, took)
	}
}
