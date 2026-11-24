package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/rules"
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
	alone(t)
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
	alone(t)
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
	alone(t)
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
	parallel(t)
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

// TestDemoWrite: --write writes the sample's rules and incidents, and a
// README, which verdict reads as it reads anyone's: the incidents come back
// as those the demo judges by, and the rules as the demo's, the same as
// embedded.
func TestDemoWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "sample")
	code, out, errs := demo(t, "--write", dir+"/", "--tz", "UTC")
	if code != 0 || errs != "" {
		t.Fatalf("exit %d: %q", code, errs)
	}
	want := []string{"rules.yaml", "incidents.yaml", "README.txt"}
	for i, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != filepath.Join(dir, want[i]) {
			t.Errorf("line %d: %q", i, line)
		}
	}
	for _, name := range want {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil || fi.Mode().Perm() != 0o644 {
			t.Errorf("%s: %v %v", name, fi, err)
		}
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o755 {
		t.Errorf("dir is %v", fi.Mode())
	}

	// The incidents: those the demo judges by, for the same weeks.
	s, err := readDemoSample()
	if err != nil {
		t.Fatal(err)
	}
	week := demoWeek(time.UTC)
	wantIncs := mustLoadIncidents(t, s.incidents(week.AddDate(0, 0, -7), week.AddDate(0, 0, 7), time.UTC))
	got, err := incident.Load(filepath.Join(dir, "incidents.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 || !reflect.DeepEqual(got, wantIncs) {
		t.Errorf("incidents: got %+v, want %+v", got, wantIncs)
	}
	if !got[3].Start.Equal(time.Date(2026, 11, 10, 12, 1, 0, 0, time.UTC)) || got[3].ID != "INC-41" {
		t.Errorf("INC-41: %+v", got[3])
	}

	// Each of the sample's incidents, by its own record: in the week
	// the demo is laid in, and the week before.
	for k, w := range []int{-7, 0} {
		for j, x := range s.incs {
			g := got[k*len(s.incs)+j]
			at := demoAt(week.AddDate(0, 0, w), x.at, time.UTC)
			if g.ID != fmt.Sprintf("%s%d", x.prefix, x.n+(k-1)*len(s.incs)) || g.Title != x.title || !g.Start.Equal(at) || !g.End.Equal(at.Add(x.lasted)) || !reflect.DeepEqual(g.Labels["service"], []string{x.service}) {
				t.Errorf("incident %d of week %d: %+v", j, k, g)
			}
		}
	}

	// The rules: the embedded file, and as many as the demo has.
	b, _ := os.ReadFile(filepath.Join(dir, "rules.yaml"))
	emb, _ := demoFiles.ReadFile("demo/rules.yaml")
	if string(b) != string(emb) {
		t.Error("rules.yaml is not the embedded rules")
	}
	rs, err := rules.Load(filepath.Join(dir, "rules.yaml"))
	if err != nil || len(rs) != 7 {
		t.Errorf("rules: %d, %v", len(rs), err)
	}

	readme, _ := os.ReadFile(filepath.Join(dir, "README.txt"))
	if !strings.Contains(string(readme), "lapilli verdict --rules "+filepath.Join(dir, "rules.yaml")+" --incidents "+filepath.Join(dir, "incidents.yaml")+" --prometheus URL --match service") {
		t.Errorf("README: %s", readme)
	}

	// Again: none is written over, and the one that is there is named.
	for _, name := range want {
		other := t.TempDir()
		if err := os.WriteFile(filepath.Join(other, name), []byte("mine"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out, errs := demo(t, "--write", other)
		if code != 1 || out != "" || !strings.Contains(errs, filepath.Join(other, name)+" already exists") {
			t.Errorf("%s: exit %d, %q %q", name, code, out, errs)
		}
		if left, _ := os.ReadDir(other); len(left) != 1 {
			t.Errorf("%s: wrote %d files beside it", name, len(left))
		}
		if b, _ := os.ReadFile(filepath.Join(other, name)); string(b) != "mine" {
			t.Errorf("%s was written over", name)
		}
	}

	// A file in the place of the directory.
	file := filepath.Join(t.TempDir(), "f")
	os.WriteFile(file, nil, 0o600)
	if code, _, errs := demo(t, "--write", file); code != 1 || !strings.Contains(errs, "is a file") {
		t.Errorf("file: exit %d, %q", code, errs)
	}
}

func mustLoadIncidents(t *testing.T, b []byte) []incident.Incident {
	t.Helper()
	f := filepath.Join(t.TempDir(), "i.yaml")
	if err := os.WriteFile(f, b, 0o600); err != nil {
		t.Fatal(err)
	}
	incs, err := incident.Load(f)
	if err != nil {
		t.Fatal(err)
	}
	return incs
}

// TestDemoWriteFlags: --write goes with neither --serve nor --json, and
// needs a directory.
func TestDemoWriteFlags(t *testing.T) {
	d := t.TempDir()
	for _, args := range [][]string{{"--write", d, "--serve"}, {"--write", d, "--json"}, {"--write"}, {"--write="}, {"--write", d, "--listen", "127.0.0.1:0"}} {
		if code, out, errs := demo(t, args...); code != 2 || out != "" || !strings.Contains(errs, `Run "lapilli demo -h"`) {
			t.Errorf("%q: exit %d, %q %q", args, code, out, errs)
		}
	}
	if left, _ := os.ReadDir(d); len(left) != 0 {
		t.Errorf("a refused flag wrote %d files", len(left))
	}
}
