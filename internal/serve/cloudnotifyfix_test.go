package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
)

// An OCI alarm's message comes as the alarm fires; the job that copies its
// firing in runs later. Its alarm's id names its rule when read, so it ties
// once the firing is in: on the rule's pages, its firings, and since when
// the rule's notifications are counted.
func TestNotifyOCIBeforeItsFiring(t *testing.T) {
	s := notifyServer(t, nil)
	h := s.NotifyHandler()
	grouped := edit(t, notifyFixture(t, "oci-grouped.json"), func(m map[string]any) { m["title"] = "WARNING alarm at 06:02" })
	if code, out := ociSend(t, h, "oci-prod", grouped, nil); code != 200 || !strings.Contains(out, `"kept":1`) {
		t.Fatalf("%d %s", code, out)
	}
	if ps := pagesOf(t, s); len(ps) != 1 || ps[0].Stone != ledger.PageUntied {
		t.Fatalf("before the firing is in: %+v", ps)
	}
	copyIn(t, s, "oci-prod", firing.Firing{Rule: "cpu-high", Start: at06,
		Labels: map[string]string{"severity": "warning", "namespace": "oci_computeagent", "alarm_id": "ocid1.alarm.oc1.ap-seoul-1.bbbb"}})
	ps := pagesOf(t, s)
	if len(ps) != 1 || ps[0].Rule != "cpu-high" || ps[0].Stone == ledger.PageUntied || len(ps[0].Firings) != 1 {
		t.Errorf("once the firing is in: %+v", ps)
	}
	ctx := context.Background()
	fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: at06.Add(-time.Hour), To: at06.Add(time.Hour), Rule: "cpu-high"})
	if err != nil || len(fs) != 1 {
		t.Fatalf("firings: %v %v", fs, err)
	}
	of, err := s.l.NotifiedOf(ctx, fs, func(string) time.Duration { return 2 * time.Minute })
	if err != nil || of[fs[0].ID] != 1 {
		t.Errorf("notified of the firing: %v %v", of, err)
	}
	since, err := s.l.NotifiedSince(ctx)
	if _, ok := since["cpu-high"]; err != nil || !ok {
		t.Errorf("notified since: %v %v", since, err)
	}
}

// EventBridge's other CloudWatch events, a change to an alarm's
// configuration among them, are of no firing: kept as nothing.
func TestNotifyEventBridgeOtherEvents(t *testing.T) {
	s := notifyServer(t, nil)
	g := newSNSSigner(t)
	s.port.sns.get = g.get
	h := s.NotifyHandler()
	change := edit(t, notifyFixture(t, "eventbridge-alarm.json"), func(m map[string]any) {
		m["detail-type"] = "CloudWatch Alarm Configuration Change"
		delete(m["detail"].(map[string]any), "previousState")
	})
	if code, out, _ := send(t, h, "/v1/sns", signedSNS(t, g, "m-config", change), nil); code != 200 || !strings.Contains(out, `"kept":0`) {
		t.Errorf("a configuration change: %d %s", code, out)
	}
	if ns := kept(t, s); len(ns) != 0 {
		t.Errorf("kept %+v", ns)
	}
}

// A message that doesn't say its alert's start takes it only from one of
// the same shape and the same labels: not an alert manager's message in
// the same group, nor another alert of the group.
func TestNotifyStartsOnlyFromTheirOwn(t *testing.T) {
	ctx := context.Background()
	s := notifyServer(t, nil)
	arn := "arn:aws:cloudwatch:us-east-1:123456789012:alarm:disk-full"
	put := func(shape string, labels map[string]string, start time.Time, at time.Time) {
		t.Helper()
		if _, err := s.l.Notify(ctx, ledger.Notification{At: at, Source: "sns", Group: arn, Shape: shape,
			Alerts: []ledger.Alert{{Rule: "disk-full", Labels: labels, Start: start, Resolved: start.IsZero()}}}); err != nil {
			t.Fatal(err)
		}
	}
	path := map[string]string{"path": "/data"}
	// Planted: an alert manager's message in the alarm's group, and the
	// alarm's own message for other labels.
	put("", path, at06.Add(-time.Hour), at06.Add(-50*time.Minute))
	put("cloudwatch", map[string]string{"path": "/logs"}, at06.Add(-2*time.Hour), at06.Add(-110*time.Minute))
	put("cloudwatch", path, time.Time{}, at06)
	ns := kept(t, s)
	if last := ns[len(ns)-1].Alerts[0]; !last.Start.IsZero() {
		t.Errorf("a resolve took a start from another's: %+v", last)
	}
	put("cloudwatch", path, at06.Add(-30*time.Minute), at06.Add(-25*time.Minute))
	put("cloudwatch", path, time.Time{}, at06.Add(time.Minute))
	ns = kept(t, s)
	if last := ns[len(ns)-1].Alerts[0]; !last.Start.Equal(at06.Add(-30 * time.Minute)) {
		t.Errorf("a resolve didn't take its own move in's start: %+v", last)
	}
}

// Notifications kept before 0.21 named an Azure resource as Azure wrote it,
// in any case: they still tie to a firing the recipe lower-cased.
func TestNotifyAzureOldCase(t *testing.T) {
	ctx := context.Background()
	s := notifyServer(t, nil)
	copyIn(t, s, "azure-shop", firing.Firing{Rule: "payment-errors", Start: at06,
		Labels: map[string]string{"resource": "shop-logs", "resource_group": "shop"}})
	if _, err := s.l.Notify(ctx, ledger.Notification{At: at06, Source: "azure", Copied: "azure-shop",
		Alerts: []ledger.Alert{{Rule: "payment-errors", Start: at06, Labels: map[string]string{"resource": "Shop-Logs", "resource_group": "SHOP"}}}}); err != nil {
		t.Fatal(err)
	}
	if ps := pagesOf(t, s); len(ps) != 1 || len(ps[0].Firings) != 1 {
		t.Errorf("pages: %+v", ps)
	}
	if got := ledger.ASCIILower("Ünïcode-RG"); got != "Ünïcode-rg" {
		t.Errorf("ASCIILower: %q, want only ASCII lower-cased, as jq's ascii_downcase", got)
	}
}

// What the port fetches it fetches from the internet: an address on this
// machine, a private network or a link is refused, whatever name led there.
// A Notifications API host is Oracle's: a realm's number never starts with
// a zero, so oraclecloud00.com, anyone's to register, isn't one.
func TestFetchesPublicOnly(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:443": false, "[::1]:443": false, "10.0.0.8:443": false, "192.168.1.1:443": false, "172.16.0.1:443": false,
		"169.254.169.254:80": false, "100.64.0.1:443": false, "0.0.0.0:443": false, "[fe80::1]:443": false, "[::ffff:10.0.0.1]:443": false,
		"52.94.236.248:443": true, "[2600:1f18::1]:443": true,
	} {
		if got := dialPublic("tcp", addr, nil) == nil; got != want {
			t.Errorf("%s: public %v, want %v", addr, got, want)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("HTTP_PROXY", "")
	if _, err := fetcher(req).Do(req); err == nil || !strings.Contains(err.Error(), errNotPublic.Error()) {
		t.Errorf("fetching from this machine: %v", err)
	}
	for host, want := range map[string]bool{
		"notification.us-ashburn-1.oraclecloud.com": true, "notification.us-ashburn-1.oraclecloud8.com": true,
		"notification.us-ashburn-1.oraclecloud10.com": true, "notification.us-ashburn-1.oraclecloud00.com": false,
		"notification.us-ashburn-1.oraclecloud01.com": false,
	} {
		if got := ociURL("https://" + host + "/20181201/subscriptions/x/confirmation"); got != want {
			t.Errorf("%s: %v, want %v", host, got, want)
		}
	}
}
