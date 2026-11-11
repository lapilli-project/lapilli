package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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

// The chart reads a cloud's alarms with rules.alarms, beside any other
// place or alone, and changes them with serve.changes: each cloud's write
// identity only for an alarm source of its cloud, the OCI key's Secret
// mounted only with changes on. It needs helm.
func TestChartAlarms(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("no helm")
	}
	chart := filepath.Join("..", "..", "charts", "lapilli")
	template := func(more ...string) (string, error) {
		args := append([]string{"template", "ops", chart, "--set", "mode=serve", "--set", "serve.userHeader=X-Forwarded-Email",
			"--set", "serve.networkPolicy.othersKeptOut=true", "--set", "serve.roles.admins={ana@example.com}", "--set", "prometheus.url=http://p"}, more...)
		cmd := exec.Command("helm", args...)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+filepath.Join(t.TempDir(), "none"))
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	cw := []string{"--set", "rules.alarms[0].source=cw", "--set", "rules.alarms[0].url=https://monitoring.eu-west-1.amazonaws.com"}
	oci := []string{"--set", "rules.alarms[0].source=oci",
		"--set", "rules.alarms[0].url=https://telemetry.ap-seoul-1.oraclecloud.com/20180401/alarms?compartmentId=ocid1.compartment.oc1..shop"}
	gcp := []string{"--set", "rules.alarms[1].source=gcp", "--set", "rules.alarms[1].url=https://monitoring.googleapis.com/v3/projects/shop/alertPolicies"}
	on := []string{"--set", "serve.changes.enabled=true"}
	for name, c := range map[string]struct {
		args []string
		want []string
		not  []string
	}{
		"alone, read only": {append(cw, "--set", "rules.alarmsEvery=5m"), []string{`"--alarms=cw=https://monitoring.eu-west-1.amazonaws.com"`, `"--alarms-every=5m"`},
			[]string{"--changes"}},
		"beside a rule file": {append(cw, "--set", "rules.configMaps={rules}"), []string{`"--alarms=cw=`, `"--rules=/etc/lapilli/rules/0"`}, nil},
		"CloudWatch, changes": {append(append(cw, on...), "--set", "serve.changes.in={cw:checkout-*}", "--set", "serve.changes.awsRole=arn:aws:iam::123456789012:role/lapilli-alarms"),
			[]string{`"--changes-in=cw:checkout-*"`, `"--changes-aws-role=arn:aws:iam::123456789012:role/lapilli-alarms"`}, []string{"changes-oci"}},
		"OCI, the tenancy's subtree": {[]string{"--set", "rules.alarms[0].source=oci", "--set",
			"rules.alarms[0].url=https://telemetry.ap-seoul-1.oraclecloud.com/20180401/alarms?compartmentIdInSubtree=true&compartmentId=ocid1.tenancy.oc1..t"},
			[]string{`"--alarms=oci=https://telemetry.ap-seoul-1.oraclecloud.com/20180401/alarms?compartmentIdInSubtree=true&compartmentId=ocid1.tenancy.oc1..t"`}, nil},
		"OCI and Google, changes": {append(append(append(oci, gcp...), on...), "--set", "serve.changes.in={oci:checkout-*,gcp:Checkout*}",
			"--set", "serve.changes.ociConfigSecret.name=oci-writes", "--set", "serve.changes.googleServiceAccount=lapilli-a@shop.iam.gserviceaccount.com"),
			[]string{"--changes-oci-config=/etc/lapilli/oci/config", "secretName: oci-writes", "mountPath: /etc/lapilli/oci",
				`"--changes-google-service-account=lapilli-a@shop.iam.gserviceaccount.com"`}, nil},
	} {
		out, err := template(c.args...)
		if err != nil {
			t.Errorf("%s: %v\n%s", name, err, out)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: no %s", name, w)
			}
		}
		for _, w := range c.not {
			if strings.Contains(out, w) {
				t.Errorf("%s: %s", name, w)
			}
		}
	}
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"a source twice":          {append(cw, "--set", "rules.alarms[1].source=cw", "--set", "rules.alarms[1].url=https://monitoring.us-east-1.amazonaws.com"), "named twice"},
		"not a cloud's":           {[]string{"--set", "rules.alarms[0].source=x", "--set", "rules.alarms[0].url=https://example.com/"}, "want CloudWatch's"},
		"in, another source":      {append(append(cw, on...), "--set", "serve.changes.in={az:x}"), "which rules.alarms doesn't"},
		"an OCI key, changes off": {append(oci, "--set", "serve.changes.ociConfigSecret.name=oci-writes"), "go with serve.changes.enabled"},
		"an OCI key, CloudWatch":  {append(append(cw, on...), "--set", "serve.changes.ociConfigSecret.name=oci-writes"), "is for OCI's alarms"},
		"a Google SA, CloudWatch": {append(append(cw, on...), "--set", "serve.changes.googleServiceAccount=lapilli-a@shop.iam.gserviceaccount.com"), "is for Google Cloud's"},
		"an Azure ID, CloudWatch": {append(append(cw, on...), "--set", "serve.changes.azureClientId=11111111-2222-3333-4444-555555555555"), "azureClientId is for Azure's"},
		"every, without alarms":   {[]string{"--set", "rules.configMaps={rules}", "--set", "rules.alarmsEvery=5m"}, "how often serve reads rules.alarms"},
	} {
		if out, err := template(c.args...); err == nil || !strings.Contains(out, c.want) {
			t.Errorf("%s: %v, want %q\n%s", name, err, c.want, out)
		}
	}
}

// --check-replay sets a CloudWatch PromQL alarm's replay beside its history
// from the recipes (--check-firings): tied by its ARN, its series put
// together as CloudWatch keeps the alarm, as a whole; a firing of another
// alarm of its name isn't taken for its. Without the history, the alarm
// isn't compared, and says why.
func TestCheckReplayAlarms(t *testing.T) {
	newHistory = func(context.Context, *verdictFlags) (history, error) { return fakeHistory{}, nil }
	now = func() time.Time { return t0 }
	cw := alarmtest.NewCloudWatch("eu-west-1")
	cw.Add(map[string]any{"AlarmName": "CheckoutErrors", "EvaluationInterval": 60,
		"EvaluationCriteria": map[string]any{"PromQLCriteria": map[string]any{"Query": "errors > 5", "PendingPeriod": 300}}}, "OK", nil)
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
	arn := cw.ARN("CheckoutErrors")
	// As cloudwatch.jq writes it: a PromQL alarm's firings carry its ARN and
	// no dimensions. The replay fires from 2h55m before t0 to 2h before.
	firings := write(t, "firings.jsonl",
		`{"rule":"CheckoutErrors","labels":{"alarm_id":"`+arn+`"},"start":"2026-11-01T21:06:00Z","end":"2026-11-01T22:01:00Z"}`+"\n"+
			`{"rule":"CheckoutErrors","labels":{"alarm_id":"arn:aws:cloudwatch:us-east-1:999999999999:alarm:CheckoutErrors"},"start":"2026-11-01T18:00:00Z","end":"2026-11-01T18:30:00Z"}`+"\n")
	args := []string{"verdict", "--alarms", "cloudwatch-prod=https://monitoring.eu-west-1.amazonaws.com", "--prometheus", "http://p",
		"--check-replay", "--tz", "UTC", "--since", "12h"}
	var stdout, stderr bytes.Buffer
	if code := run(append(args, "--check-firings", firings), &stdout, &stderr); code != 0 ||
		!strings.Contains(stdout.String(), "1 firings alike, 0 only in the replay, 0 only in what really fired: 100% alike.") ||
		!strings.Contains(stdout.String(), "the history in "+firings) || strings.Contains(stdout.String(), "ALERTS") {
		t.Fatalf("exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(args, &stdout, &stderr); code == 0 || !strings.Contains(stdout.String(), "keeps no series of what it fired") {
		t.Errorf("without --check-firings: exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"verdict", "--alarms", "cloudwatch-prod=https://monitoring.eu-west-1.amazonaws.com", "--prometheus", "http://p",
		"--check-firings", firings, "--incidents", firings}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "--check-firings goes with --check-replay") {
		t.Errorf("--check-firings alone: exit %d %s", code, stderr.String())
	}
}
