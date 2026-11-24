package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// demo runs lapilli demo with args, TMPDIR a directory of the test's own,
// and fails the test if it asks for a history: the sample is its history.
// It returns what it printed, and checks it left nothing behind.
func demo(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	was := newHistory
	newHistory = func(context.Context, *verdictFlags) (history, error) {
		t.Error("lapilli demo asked for a history")
		return fakeHistory{}, nil
	}
	defer func() { newHistory = was }()
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"demo"}, args...), &stdout, &stderr)
	if left, err := os.ReadDir(tmp); err != nil || len(left) > 0 {
		t.Errorf("lapilli demo left %v behind (%v)", left, err)
	}
	return code, stdout.String(), stderr.String()
}

// TestDemo: lapilli demo prints a verdict of the sample, the same each
// run: the noisy rule and its longer "for", the rules that caught the
// incidents, the one missed and the rule that nearly caught it, the
// firings that woke people; then how to run it on one's own rules. In
// another zone it is the same but for the zone it names.
func TestDemo(t *testing.T) {
	code, out, errs := demo(t, "--tz", "UTC")
	if code != 0 || errs != "" {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{
		"Lapilli  2026-11-09 00:00 → 2026-11-16 00:00 UTC\n23 firings, 3 incidents written down\n",
		"CheckoutLatencyP99  warning   14     1         13         7%",
		"CheckoutLatencyP99  1m → 5m  14 → 2  1          up to 4m",
		"of the firings that caught nothing, 8 came while people slept, and 4 more off hours.",
		"NodeCPUHigh         warning   7         0            0",
		"INC-41 Checkout errors  2026-11-10 12:01  38m     CheckoutErrors +3m, CheckoutLatencyP99 +4m (noisy)",
		"INC-42 Search timeouts  2026-11-12 21:15  35m     SearchErrors +3m",
		"INC-43 Orders stuck     2026-11-14 03:05  25m     ✗ nothing fired",
		"✗ 1 of 3 incidents had no alert.",
		"INC-43    OrdersQueueBacklog  9m from +2m  for 15m → 8m  +10m        0 → 1  0 → 0",
		"3 rules never fired:",
		"\nOn your own: lapilli verdict --rules alert-rules.yaml --prometheus http://prometheus:9090 --incidents incidents.yaml\n",
		"lapilli impact suggest --prometheus http://prometheus:9090",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if !strings.HasSuffix(out, demoNext) {
		t.Errorf("doesn't end with what to run next:\n%s", out)
	}
	if _, again, _ := demo(t, "--tz", "UTC"); again != out {
		t.Errorf("not the same twice:\n%s\n---\n%s", out, again)
	}
	_, seoul, _ := demo(t, "--tz=Asia/Seoul")
	header, rest, _ := strings.Cut(seoul, "\n")
	rest = strings.Replace(rest, "Out of hours, in Asia/Seoul", "Out of hours, in UTC", 1)
	if header != "Lapilli  2026-11-09 00:00 → 2026-11-16 00:00 Asia/Seoul (+09:00)" || rest != strings.SplitN(out, "\n", 2)[1] {
		t.Errorf("in Asia/Seoul, not the same but for its zone:\n%s", seoul)
	}
}

// TestDemoJSON: with --json, stdout is verdict's JSON alone, and what to
// run next is said on stderr.
func TestDemoJSON(t *testing.T) {
	code, out, errs := demo(t, "--json", "--tz", "UTC")
	if code != 0 || errs != demoNext {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	var rep struct {
		Rules      []struct{ Rule string } `json:"rules"`
		Incidents  []struct{ ID string }   `json:"incidents"`
		NearMisses []struct {
			Incident, Rule string
		} `json:"near_misses"`
		Suggestions []struct{ Rule string } `json:"suggestions"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	if len(rep.Rules) != 7 || len(rep.Incidents) != 3 || len(rep.NearMisses) != 1 || rep.NearMisses[0].Rule != "OrdersQueueBacklog" ||
		len(rep.Suggestions) != 1 || rep.Suggestions[0].Rule != "CheckoutLatencyP99" {
		t.Errorf("rules %v, incidents %v, near misses %v, suggestions %v", rep.Rules, rep.Incidents, rep.NearMisses, rep.Suggestions)
	}
}

// TestDemoFlags: lapilli demo takes --json and --tz, and refuses anything
// else before it reads anything.
func TestDemoFlags(t *testing.T) {
	for _, args := range [][]string{{"--rules", "x.yaml"}, {"--tz"}, {"--tz", "Nowhere/Else"}, {"--tz="}, {"--tz", ""}, {"--json=1"}, {"--prometheus", "http://p"}} {
		if code, out, errs := demo(t, args...); code != 2 || out != "" || !strings.Contains(errs, `Run "lapilli demo -h"`) {
			t.Errorf("%q: exit %d, %q %q", args, code, out, errs)
		}
	}
	if code, out, _ := demo(t, "-h"); code != 0 || out != demoUsage {
		t.Errorf("-h: exit %d", code)
	}
	// --json is on or off as any flag that is: =false the report as text,
	// =true as JSON.
	if code, out, _ := demo(t, "--tz", "UTC", "--json=false"); code != 0 || !strings.HasPrefix(out, "Lapilli  ") {
		t.Errorf("--json=false: exit %d, %.40q", code, out)
	}
	if code, out, _ := demo(t, "--tz", "UTC", "--json=true"); code != 0 || !strings.HasPrefix(out, "{") {
		t.Errorf("--json=true: exit %d, %.40q", code, out)
	}
}

// TestDemoSample: each of the sample's series is one a rule compares, and
// each rule but those meant never to fire compares one: the sample has no
// series left over from a rule since changed.
func TestDemoSample(t *testing.T) {
	s, err := readDemoSample()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := demoFiles.ReadFile("demo/rules.yaml")
	for expr := range s.series {
		if !strings.Contains(string(b), "expr: "+expr+" ") {
			t.Errorf("no rule compares %s", expr)
		}
	}
	if n := strings.Count(string(b), "expr: "); n-len(s.series) != 2 {
		t.Errorf("%d rules, %d series: want two rules that never fire", n, len(s.series))
	}
}
