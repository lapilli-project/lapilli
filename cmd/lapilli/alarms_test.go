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
