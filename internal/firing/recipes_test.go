package firing

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const recipes = "../../recipes/firings/"

// needTools skips a test where what the recipes use isn't installed.
func needTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"sh", "curl", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
}

// fakeCLI puts a script named name first on PATH, standing in for a cloud's
// CLI: it writes each call's arguments, one a line, to the file it returns,
// and answers with what script prints.
func fakeCLI(t *testing.T, name, script string) (path, calls string) {
	t.Helper()
	bin := t.TempDir()
	calls = filepath.Join(bin, "calls")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >>" + calls + "\nd=" + recipes + "testdata\n" + script
	if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin + string(os.PathListSeparator) + os.Getenv("PATH"), calls
}

// runRecipe runs a recipe and reads what it writes the way --firings does.
func runRecipe(t *testing.T, script string, env ...string) []Firing {
	t.Helper()
	cmd := exec.Command("sh", recipes+script)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.Output()
	if err != nil {
		var stderr []byte
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = ee.Stderr
		}
		t.Fatalf("%s: %v\n%s", script, err, stderr)
	}
	fs, err := ReadJSONL(strings.NewReader(string(out)))
	if err != nil {
		t.Fatalf("what %s wrote doesn't read as firings: %v\n%s", script, err, out)
	}
	return fs
}

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func wantFirings(t *testing.T, got []Firing, want ...Firing) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("firings: %+v, want %+v", got, want)
	}
	for i, w := range want {
		g := got[i]
		if g.Rule != w.Rule || !g.Start.Equal(w.Start) || !g.End.Equal(w.End) || len(g.Labels) != len(w.Labels) {
			t.Errorf("firing %d: %+v, want %+v", i, g, w)
			continue
		}
		for k, v := range w.Labels {
			if g.Labels[k] != v {
				t.Errorf("firing %d: label %s = %q, want %q", i, k, g.Labels[k], v)
			}
		}
	}
}

func calledWith(t *testing.T, calls string, want ...string) {
	t.Helper()
	b, _ := os.ReadFile(calls)
	for _, w := range want {
		if !strings.Contains(string(b), w) {
			t.Errorf("no call with %q in:\n%s", w, b)
		}
	}
}

func TestCloudWatchRecipe(t *testing.T) {
	needTools(t)
	path, calls := fakeCLI(t, "aws", `case "$*" in
  *describe-alarm-history*) cat "$d/cloudwatch-history.json" ;;
  *describe-alarms*) cat "$d/cloudwatch-alarms.json" ;;
  *) exit 2 ;;
esac`)
	got := runRecipe(t, "cloudwatch.sh", "PATH="+path, "SINCE=2026-11-01T00:00:00Z", "UNTIL=2026-11-08T00:00:00Z")
	lb := map[string]string{"LoadBalancer": "app/checkout/50dc6c495c0c9188", "service": "checkout"}
	// The first move out of ALARM began before the period: left out. The
	// moves on the 4th have HistoryData cut short, or not an object, and are
	// read from their summaries. flappy's times are written two ways within a second, and
	// sort as times.
	wantFirings(t, got,
		Firing{Rule: "checkout-5xx", Labels: lb, Start: ts("2026-11-02T03:10:00.123Z"), End: ts("2026-11-02T03:40:00Z")},
		Firing{Rule: "checkout-5xx", Labels: lb, Start: ts("2026-11-04T01:00:00Z"), End: ts("2026-11-04T01:20:00Z")},
		Firing{Rule: "flappy", Start: ts("2026-11-05T03:10:00Z"), End: ts("2026-11-05T03:10:00.5Z")},
		Firing{Rule: "shop-down", Start: ts("2026-11-03T05:00:00Z")})
	calledWith(t, calls, "--start-date 2026-11-01T00:00:00Z --end-date 2026-11-08T00:00:00Z", "--alarm-types MetricAlarm CompositeAlarm")
}

func TestAzureMonitorRecipe(t *testing.T) {
	needTools(t)
	path, calls := fakeCLI(t, "az", `case "$*" in
  *"--skip-token page-2"*) cat "$d/azure-monitor-2.json" ;;
  *) cat "$d/azure-monitor-1.json" ;;
esac`)
	got := runRecipe(t, "azure-monitor.sh", "PATH="+path, "SINCE=2026-11-01T00:00:00Z", "UNTIL=2026-11-08T00:00:00Z", "SUBSCRIPTIONS=s1 s2")
	wantFirings(t, got,
		Firing{Rule: "checkout-5xx", Labels: map[string]string{"severity": "Sev1", "resource": "checkout-app", "resource_group": "shop", "service": "checkout", "code": "500"},
			Start: ts("2026-11-02T03:10:00Z"), End: ts("2026-11-02T03:40:00Z")},
		// Closed by hand: ends when it was closed.
		Firing{Rule: "payment-errors", Labels: map[string]string{"severity": "Sev2", "resource": "shop-logs", "resource_group": "shop"},
			Start: ts("2026-11-03T05:00:00Z"), End: ts("2026-11-03T05:30:00Z")},
		Firing{Rule: "disk-full", Labels: map[string]string{"severity": "Sev3", "resource": "db-1", "resource_group": "data"}, Start: ts("2026-11-04T01:00:00Z")},
		// An activity log alert is stateless: it ends as it fires, though
		// someone closed it two days later.
		Firing{Rule: "vm-deleted", Labels: map[string]string{"severity": "Sev4", "resource": "vm-7", "resource_group": "shop"},
			Start: ts("2026-11-05T09:00:00Z"), End: ts("2026-11-05T09:00:00Z")})
	calledWith(t, calls, "datetime(2026-11-01T00:00:00Z)", "datetime(2026-11-08T00:00:00Z)", "--skip-token page-2", "--subscriptions s1 s2")
}

func TestGoogleCloudRecipe(t *testing.T) {
	needTools(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.URL.Path != "/v3/projects/shop/alerts" {
			t.Errorf("%s %s", r.Header.Get("Authorization"), r.URL.Path)
		}
		if f := r.URL.Query().Get("filter"); f != `open_time>="2026-11-01T00:00:00+09:00" AND open_time<"2026-11-08T00:00:00Z"` {
			t.Errorf("filter %q", f)
		}
		page := "google-cloud-1.json"
		if r.URL.Query().Get("pageToken") == "page-2" {
			page = "google-cloud-2.json"
		}
		b, _ := os.ReadFile(recipes + "testdata/" + page)
		w.Write(b)
	}))
	defer srv.Close()
	token := filepath.Join(t.TempDir(), "token")
	os.WriteFile(token, []byte("tok"), 0o600)
	got := runRecipe(t, "google-cloud.sh", "GOOGLE_MONITORING_API="+srv.URL, "GOOGLE_TOKEN_FILE="+token, "GOOGLE_CLOUD_PROJECT=shop",
		"SINCE=2026-11-01T00:00:00+09:00", "UNTIL=2026-11-08T00:00:00Z")
	// The alert from before the period, which the API's filter should have
	// left out, is left out here too.
	wantFirings(t, got,
		// The metric's service, not the policy's: the policy's labels only
		// where the alert's don't say.
		Firing{Rule: "shop/CheckoutErrors", Labels: map[string]string{"team": "shop", "service": "checkout", "project_id": "shop", "location": "asia-northeast3",
			"cluster": "prod", "namespace": "shop", "job": "checkout", "code": "500", "severity": "critical"},
			Start: ts("2026-11-02T03:10:00Z"), End: ts("2026-11-02T03:44:30Z")},
		Firing{Rule: "CPU high", Labels: map[string]string{"project_id": "shop", "instance_id": "1234", "zone": "asia-northeast3-a"}, Start: ts("2026-11-04T01:00:00Z")},
		// After the period's start at +09:00, though before it in UTC's date.
		Firing{Rule: "CPU high", Labels: map[string]string{"instance_id": "5"}, Start: ts("2026-10-31T16:00:00Z"), End: ts("2026-10-31T16:30:00Z")})
}

func TestOCIRecipe(t *testing.T) {
	needTools(t)
	path, calls := fakeCLI(t, "oci", `case "$*" in
  *"alarm list"*) cat "$d/oci-alarms.json" ;;
  *aaaa*"--page page-2"*) cat "$d/oci-history-a2.json" ;;
  *aaaa*) cat "$d/oci-history-a.json" ;;
  *bbbb*) cat "$d/oci-history-b.json" ;;
  *) exit 2 ;;
esac`)
	got := runRecipe(t, "oci.sh", "PATH="+path, "COMPARTMENT_ID=ocid1.compartment.oc1..x", "SINCE=2026-11-01T00:00:00Z", "UNTIL=2026-11-08T00:00:00Z")
	// The alarm's OCID too, by which /v1/oci ties its notifications.
	l := map[string]string{"severity": "critical", "namespace": "oci_apigateway", "alarm_id": "ocid1.alarm.oc1.ap-seoul-1.aaaa"}
	// From when each transition was triggered, not when it was written,
	// three minutes later; the first, out of firing, began before the period.
	// cpu-high was firing before it too: Firing to Firing starts nothing. On
	// the 4th, times written two ways within a second sort as times. A firing
	// is of the period it was triggered in: cpu-high's triggered before SINCE
	// is the period before's, one triggered before UNTIL but written after is
	// this one's, and one triggered after UNTIL the next's.
	wantFirings(t, got,
		Firing{Rule: "api-5xx", Labels: l, Start: ts("2026-11-02T03:10:00Z"), End: ts("2026-11-02T03:40:00Z")},
		Firing{Rule: "api-5xx", Labels: l, Start: ts("2026-11-04T01:00:00Z"), End: ts("2026-11-04T01:00:00.5Z")},
		Firing{Rule: "cpu-high", Labels: map[string]string{"severity": "warning", "namespace": "oci_computeagent", "alarm_id": "ocid1.alarm.oc1.ap-seoul-1.bbbb"},
			Start: ts("2026-11-07T23:59:00Z"), End: ts("2026-11-08T00:20:00Z")})
	calledWith(t, calls, "--alarm-historytype STATE_TRANSITION_HISTORY", "--timestamp-greater-than-or-equal-to 2026-11-01T00:00:00Z",
		"--timestamp-less-than 2026-11-08T01:00:00Z", "--page page-2")
}

// TestFiringRecipesFailLoudly: a CLI that fails stops a recipe with nothing
// written, so a verdict doesn't read a week of nothing.
func TestFiringRecipesFailLoudly(t *testing.T) {
	needTools(t)
	for _, c := range []struct {
		script, cli string
		env         []string
	}{
		{"cloudwatch.sh", "aws", []string{"SINCE=2026-11-01T00:00:00Z"}},
		{"azure-monitor.sh", "az", []string{"SINCE=2026-11-01T00:00:00Z"}},
		{"oci.sh", "oci", []string{"SINCE=2026-11-01T00:00:00Z", "COMPARTMENT_ID=x"}},
		{"google-cloud.sh", "gcloud", []string{"SINCE=2026-11-01T00:00:00Z", "GOOGLE_CLOUD_PROJECT=shop"}},
	} {
		path, _ := fakeCLI(t, c.cli, "echo 'denied' >&2; exit 1")
		cmd := exec.Command("sh", recipes+c.script)
		cmd.Env = append(append(os.Environ(), "PATH="+path), c.env...)
		var stdout strings.Builder
		cmd.Stdout = &stdout
		if err := cmd.Run(); err == nil || stdout.Len() != 0 {
			t.Errorf("%s %v: %v, wrote %q", c.script, c.env, err, stdout.String())
		}
	}
	// A move CloudWatch writes without saying from what to what stops the
	// conversion, rather than a firing going missing.
	path, _ := fakeCLI(t, "aws", `case "$*" in
  *describe-alarm-history*) echo '{"AlarmHistoryItems": [{"AlarmName": "a", "HistoryItemType": "StateUpdate", "Timestamp": "2026-11-02T00:00:00Z", "HistoryData": "{", "HistorySummary": "?"}]}' ;;
  *) echo '{}' ;;
esac`)
	cmd := exec.Command("sh", recipes+"cloudwatch.sh")
	cmd.Env = append(os.Environ(), "PATH="+path, "SINCE=2026-11-01T00:00:00Z")
	if out, err := cmd.Output(); err == nil || len(out) != 0 {
		t.Errorf("an unreadable move: %v, wrote %q", err, out)
	}
}

// TestFiringRecipesRefuse: what isn't a time, or a project id, or https, is
// refused before any CLI or API is asked, and the refusal says why.
func TestFiringRecipesRefuse(t *testing.T) {
	needTools(t)
	for _, c := range []struct {
		script, cli string
		env         []string
		says        string
	}{
		{"azure-monitor.sh", "az", []string{"SINCE=2026-11-01T00:00:00Z) or (1==1"}, "is not a time"},
		{"azure-monitor.sh", "az", []string{"SINCE=2026"}, "is not a time"},
		// grep reads lines: a time on the first doesn't let the second through.
		{"azure-monitor.sh", "az", []string{"SINCE=2026-11-01T00:00:00Z\n) or (1==1"}, "is not a time"},
		{"google-cloud.sh", "gcloud", []string{"SINCE=2026-11-01T00:00:00Z\n\" OR state=\"open", "GOOGLE_CLOUD_PROJECT=shop"}, "is not a time"},
		// Resource Graph is given UTC only.
		{"azure-monitor.sh", "az", []string{"SINCE=2026-11-01T00:00:00+09:00"}, "is not a time in UTC"},
		{"cloudwatch.sh", "aws", []string{"SINCE=11-01-2026"}, "is not a time"},
		{"oci.sh", "oci", []string{"SINCE=2026-11-01T00:00:00Z", "UNTIL=soon", "COMPARTMENT_ID=x"}, "is not a time"},
		{"google-cloud.sh", "gcloud", []string{"SINCE=2026-11-01T00:00:00Z\" OR state=\"open", "GOOGLE_CLOUD_PROJECT=shop"}, "is not a time"},
		{"google-cloud.sh", "gcloud", []string{"SINCE=2026-11-01T00:00:00Z", "GOOGLE_CLOUD_PROJECT=shop/../x"}, "is not a project id"},
		{"google-cloud.sh", "gcloud", []string{"SINCE=2026-11-01T00:00:00Z", "GOOGLE_CLOUD_PROJECT=shop", "GOOGLE_MONITORING_API=http://example.com"}, "is not https"},
		{"google-cloud.sh", "gcloud", []string{"SINCE=2026-11-01T00:00:00Z", "GOOGLE_CLOUD_PROJECT=shop", "GOOGLE_MONITORING_API=http://localhost:1@example.com"}, "is not https"},
	} {
		path, calls := fakeCLI(t, c.cli, "echo '{}'")
		cmd := exec.Command("sh", recipes+c.script)
		cmd.Env = append(append(os.Environ(), "PATH="+path), c.env...)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if _, called := os.Stat(calls); err == nil || stdout.Len() != 0 || called == nil || !strings.Contains(stderr.String(), c.says) {
			t.Errorf("%s %v: %v, wrote %q, said %q, %s called: %v", c.script, c.env, err, stdout.String(), stderr.String(), c.cli, called == nil)
		}
	}
}
