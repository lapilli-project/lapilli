package main

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/alarmtest"
	"lapilli.dev/internal/source"
)

// lapilli verdict reads a cloud's alarms with --alarms, beside a rule file:
// a CloudWatch alarm and a rule of one name told apart by the source, the
// PromQL alarm replayed, the alarm that isn't PromQL listed.
func TestAlarmsRules(t *testing.T) {
	newHistory = func(context.Context, *verdictFlags) (history, error) { return fakeHistory{}, nil }
	now = func() time.Time { return t0 }
	cw := alarmtest.NewCloudWatch("eu-west-1")
	cw.Add(map[string]any{"AlarmName": "CheckoutErrors", "EvaluationInterval": 60,
		"EvaluationCriteria": map[string]any{"PromQLCriteria": map[string]any{"Query": "errors > 5", "PendingPeriod": 300}}}, "OK", nil)
	cw.Add(map[string]any{"AlarmName": "cpu-high", "Namespace": "AWS/EC2", "MetricName": "CPUUtilization"}, "OK", nil)
	was := newAlarms
	t.Cleanup(func() { newAlarms = was })
	newAlarms = func(ctx context.Context, at alarmsFlag) (*source.Alarms, error) {
		a, err := source.NewAlarms(at.source, at.url, "lapilli/test")
		if err != nil {
			return nil, err
		}
		a.Transport = &alarmtest.Transport{H: cw}
		a.Reads.Sign = func(r *http.Request) error {
			r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261111/eu-west-1/monitoring/aws4_request")
			return nil
		}
		return a, nil
	}
	rules := write(t, "rules.yaml", "groups:\n  - name: shop\n    rules:\n      - alert: CheckoutErrors\n        expr: errors > 7\n        for: 5m\n")
	incidents := write(t, "incidents.yaml", "incidents:\n  - {id: INC-7, title: Checkout down, start: 2026-11-01T21:00:00Z, end: 2026-11-01T22:00:00Z}\n")
	var stdout, stderr bytes.Buffer
	args := []string{"verdict", "--rules", rules, "--alarms", "cloudwatch-prod=https://monitoring.eu-west-1.amazonaws.com",
		"--prometheus", "http://p", "--incidents", incidents, "--tz", "UTC"}
	if code := run(args, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "CheckoutErrors [cloudwatch-prod] +5m") ||
		!strings.Contains(stdout.String(), "1 rules never fired.") || !strings.Contains(stdout.String(), "cpu-high: a CloudWatch metric alarm isn't PromQL") {
		t.Fatalf("exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
	for _, e := range cw.Errs {
		t.Error(e)
	}
}

// --alarms is refused, before anything is read, where it can't go.
func TestAlarmsFlags(t *testing.T) {
	incidents := write(t, "incidents.yaml", "incidents: []\n")
	base := []string{"verdict", "--prometheus", "http://p", "--incidents", incidents}
	cw := "cloudwatch-prod=https://monitoring.eu-west-1.amazonaws.com"
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"no source":           {[]string{"--alarms", "https://monitoring.eu-west-1.amazonaws.com"}, "SOURCE=URL"},
		"a bad source":        {[]string{"--alarms", "Prod=https://monitoring.eu-west-1.amazonaws.com"}, "SOURCE=URL"},
		"not a cloud":         {[]string{"--alarms", "x=https://example.com/alarms"}, "not CloudWatch's"},
		"Amazon's ruler":      {[]string{"--alarms", "x=https://aps.eu-west-1.amazonaws.com/workspaces/ws-1"}, "monitoring.REGION"},
		"named twice":         {[]string{"--alarms", cw, "--alarms", cw}, "named twice"},
		"with --alerts":       {[]string{"--alarms", cw, "--alerts"}, "doesn't go with --alerts"},
		"with --compare":      {[]string{"--alarms", cw, "--rules", incidents, "--compare", incidents}, "leave out --alarms"},
		"OCI, no compartment": {[]string{"--alarms", "oci=https://telemetry.ap-seoul-1.oraclecloud.com/20180401/alarms"}, "compartmentId"},
	} {
		var out, errs strings.Builder
		if code := run(append(append([]string{}, base...), c.args...), &out, &errs); code != 2 || !strings.Contains(errs.String(), c.want) {
			t.Errorf("%s: %d, want 2 and %q\n%s", name, code, c.want, errs.String())
		}
	}
	var out, errs strings.Builder
	if code := run([]string{"serve", "--alarms-every", "5m", "--rules", incidents, "--prometheus", "http://p"}, &out, &errs); code != 2 ||
		!strings.Contains(errs.String(), "give --alarms") {
		t.Errorf("--alarms-every alone: %s", errs.String())
	}
}

// lapilli serve's changes to a cloud's alarms: each write identity for an
// --alarms of its cloud, with --changes; a SOURCE: entry of --changes-in
// for an --alarms source.
func TestServeAlarmsFlags(t *testing.T) {
	signed := []string{"--changes", "--user-header", "X-Forwarded-Email", "--admins", "ana@example.com", "--prometheus", "http://p"}
	cw := []string{"--alarms", "cw=https://monitoring.eu-west-1.amazonaws.com"}
	gcp := []string{"--alarms", "gcp=https://monitoring.googleapis.com/v3/projects/shop/alertPolicies"}
	cfg := write(t, "config", "[DEFAULT]\n")
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"in, another source":             {append(append([]string{"--changes-in", "az:shop-rg/*"}, cw...), signed...), "no --alarms names the source az"},
		"a Google account, CW":           {append(append([]string{"--changes-google-service-account", "lapilli-a@shop.iam.gserviceaccount.com"}, cw...), signed...), "is for Google Cloud's"},
		"a Google account, not one":      {append(append([]string{"--changes-google-service-account", "ana@example.com"}, gcp...), signed...), "a service account's email"},
		"an OCI config, Google":          {append(append([]string{"--changes-oci-config", cfg}, gcp...), signed...), "is for OCI's alarms"},
		"an OCI config, not there":       {append(append([]string{"--changes-oci-config", cfg + ".x"}, gcp...), signed...), "--changes-oci-config"},
		"an Azure client ID, CW":         {append(append([]string{"--changes-azure-client-id", "00000000-0000-0000-0000-000000000000"}, cw...), signed...), "--changes-azure-client-id is for"},
		"a Google account, no --changes": {append([]string{"--changes-google-service-account", "lapilli-a@shop.iam.gserviceaccount.com", "--prometheus", "http://p"}, gcp...), "go with --changes"},
	} {
		var out, errs strings.Builder
		if code := run(append([]string{"serve"}, c.args...), &out, &errs); code != 2 || !strings.Contains(errs.String(), c.want) {
			t.Errorf("%s: %d, want 2 and %q\n%s", name, code, c.want, errs.String())
		}
	}
	// What is allowed parses: an AWS role for CloudWatch's alarms, and
	// --changes-in naming them.
	f, err := parseFlags(append(append([]string{"--changes-in", "cw:checkout-*", "--changes-aws-role", "arn:aws:iam::111122223333:role/r"}, cw...), "--prometheus", "http://p"),
		func(name, val string) (bool, error) {
			return name == "--changes-in" || name == "--changes-aws-role", nil
		})
	if err != nil || len(f.alarms) != 1 || !alarmsOf(f, source.AlarmsAWS) || badChangesIn([]string{"cw:checkout-*"}, f) != "" {
		t.Errorf("allowed: %+v %v", f, err)
	}
}
