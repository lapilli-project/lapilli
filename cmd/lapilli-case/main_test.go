package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/golang/snappy"
	"github.com/lapilli-project/lapilli/internal/casefile"
	"github.com/lapilli-project/lapilli/internal/guard"
	"github.com/lapilli-project/lapilli/internal/metrics"
	"github.com/lapilli-project/lapilli/internal/replay"
	"github.com/prometheus/prometheus/prompb"
)

// The test binary stands in for lapilli-case when bin/kubectl hands over to it, so the chain an agent
// goes through — the shim on its PATH, the guard, the real kubectl — is run as it is installed.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "guard-kubectl" {
		os.Exit(guardKubectl(os.Args[2:]))
	}
	os.Exit(m.Run())
}

const kubeconfig = "current-context: case\ncontexts:\n- {name: case, context: {cluster: frozen, user: nobody}}\n- {name: prod, context: {cluster: prod, user: me}}\n" +
	"clusters:\n- {name: frozen, cluster: {server: 'http://127.0.0.1:5000/kubernetes'}}\n- {name: prod, cluster: {server: 'https://prod.example'}}\n"

func TestAnAgentsKubectlGoesThroughTheGuardToTheCase(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A directory with a quote and a space in it, as a path may have; and a stand-in for the real
	// kubectl that says what it was run with.
	dir := filepath.Join(t.TempDir(), "it's here")
	real, bin := filepath.Join(dir, "real"), filepath.Join(dir, "bin")
	os.MkdirAll(real, 0o755)
	fake := "#!/bin/sh\nprintf 'ran:'; for a in \"$@\"; do printf ' [%s]' \"$a\"; done; printf ' master=%s\\n' \"${KUBERNETES_MASTER:-unset}\"\n"
	if err := os.WriteFile(filepath.Join(real, "kubectl"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	kc := filepath.Join(dir, "kubeconfig")
	os.WriteFile(kc, []byte(kubeconfig), 0o600)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+real+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := replay.WriteTools(bin, kc, self); err != nil {
		t.Fatal(err)
	}

	run := func(env string, args ...string) (string, int) {
		cmd := exec.Command(filepath.Join(bin, "kubectl"), args...)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+env, "KUBERNETES_MASTER=https://prod.example")
		out, err := cmd.CombinedOutput()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out)), code
	}
	pin := " [--kubeconfig=" + kc + "] [--context=case] [--cluster=frozen] [--server=http://127.0.0.1:5000/kubernetes] [--user=nobody]"

	if out, code := run(kc, "get", "pods", "-A", "-l", "app=a b"); code != 0 || out != "ran: [get] [pods] [-A] [-l] [app=a b]"+pin+" master=unset" {
		t.Errorf("an ordinary read: %q (exit %d)", out, code)
	}
	if out, code := run(kc, "logs", "x", "--", "-s", "y"); code != 0 || out != "ran: [logs] [x]"+pin+" [--] [-s] [y] master=unset" {
		t.Errorf("arguments after --: %q (exit %d)", out, code)
	}
	for _, args := range [][]string{
		{"get", "pods", "--context", "prod"}, {"get", "pods", "-As", "https://prod.example"}, {"get", "pods", "--kubeconfig=/home/me/.kube/config"},
		{"config", "use-context", "prod"}, {"config", "set", "clusters.frozen.server", "https://prod.example"}, {"delete", "pod", "x"},
		{"get", "-f", "/etc/passwd"}, {"", "delete", "pod", "x"}, {"-n", "shop", "", "delete", "pod", "x"},
	} {
		if out, code := run(kc, args...); code != guard.ExitRefused || strings.Contains(out, "ran:") || !strings.HasPrefix(out, "kubectl refused by lapilli-case: ") {
			t.Errorf("kubectl %v reached the real binary: %q (exit %d)", args, out, code)
		}
	}
	if out, code := run("/home/me/.kube/config", "get", "pods"); code != guard.ExitRefused || strings.Contains(out, "ran:") {
		t.Errorf("kubectl ran with another KUBECONFIG: %q (exit %d)", out, code)
	}
	// What a run does not offer its agent, of what is a read: refused by the guard, wherever the
	// namespace stands, and nothing else with it.
	if out, code := run(kc, "-n", "shop", "auth", "can-i", "get", "pods"); code != 0 || !strings.HasPrefix(out, "ran: [-n] [shop] [auth] [can-i] [get] [pods]") {
		t.Errorf("a read, with nothing withheld: %q (exit %d)", out, code)
	}
	t.Setenv(guard.NotOfferedEnv, "auth cluster-info config explain")
	for _, args := range [][]string{{"-n", "shop", "auth", "can-i", "get", "pods"}, {"-nshop", "explain", "get"}, {"cluster-info"}, {"--namespace=shop", "config", "current-context"}} {
		if out, code := run(kc, args...); code != guard.ExitRefused || strings.Contains(out, "ran:") || !strings.Contains(out, "is not offered in this run") {
			t.Errorf("kubectl %v, withheld, reached the real binary: %q (exit %d)", args, out, code)
		}
	}
	if out, code := run(kc, "-n", "shop", "get", "configmap", "config"); code != 0 || !strings.HasPrefix(out, "ran: [-n] [shop] [get] [configmap] [config]") {
		t.Errorf("a ConfigMap called config, with `config` withheld: %q (exit %d)", out, code)
	}
	if out, code := run(kc, "-n", "shop", "delete", "pod", "auth"); code != guard.ExitRefused || !strings.Contains(out, `"delete" is not a read-only verb`) {
		t.Errorf("a write is refused as a write, whatever else is withheld: %q (exit %d)", out, code)
	}
	os.Unsetenv(guard.NotOfferedEnv)
	// The kubeconfig is read at every call: one that has stopped naming a place is refused, not guessed at.
	os.WriteFile(kc, []byte("contexts: []\n"), 0o600)
	if out, code := run(kc, "get", "pods"); code != guard.ExitRefused || strings.Contains(out, "ran:") {
		t.Errorf("kubectl ran with a kubeconfig that names no context: %q (exit %d)", out, code)
	}
	if code := guardKubectl([]string{kc}); code != 2 {
		t.Errorf("the guard run by hand exited %d", code)
	}
}

func TestFlagsMayStandAnywhereAndDoubleDashEndsThem(t *testing.T) {
	type result struct {
		Agent string
		Runs  int
		Pos   []string
	}
	read := func(args ...string) (result, error) {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		var r result
		fs.StringVar(&r.Agent, "agent", "", "")
		fs.IntVar(&r.Runs, "runs", 1, "")
		pos, err := parse(fs, args)
		r.Pos = pos
		return r, err
	}
	for _, tc := range []struct {
		args []string
		want result
	}{
		{[]string{"a", "b", "--agent", "x"}, result{"x", 1, []string{"a", "b"}}},
		{[]string{"--agent", "x", "a", "--runs", "3", "b"}, result{"x", 3, []string{"a", "b"}}},
		{[]string{"a", "--", "-b", "--agent", "c"}, result{"", 1, []string{"a", "-b", "--agent", "c"}}},
		{[]string{"--runs=2", "--", "--runs=9"}, result{"", 2, []string{"--runs=9"}}},
		{nil, result{"", 1, nil}},
	} {
		if got, err := read(tc.args...); err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%v: %+v (%v), want %+v", tc.args, got, err, tc.want)
		}
	}
	if _, err := read("a", "--nope"); err == nil {
		t.Error("an unknown flag after a positional was accepted")
	}
}

// `export-metrics` and then `pack`, as commands: the case says of its metrics what the export
// learned, the file the export left beside its metrics is not in the case, and what an earlier
// export left under the same name is not taken for the later one's.
func TestPackSealsWhatExportMetricsLearned(t *testing.T) {
	blocksEnd := "5000"
	series := func(job string, value float64) *prompb.TimeSeries {
		return &prompb.TimeSeries{Labels: []prompb.Label{{Name: "__name__", Value: "up"}, {Name: "job", Value: job}}, Samples: []prompb.Sample{{Timestamp: 9000, Value: value}}}
	}
	read, _ := (&prompb.ReadResponse{Results: []*prompb.QueryResult{{Timeseries: []*prompb.TimeSeries{series("a", 1), series("z", 1)}}}}).Marshal()
	prometheus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/read":
			w.Write(snappy.Encode(nil, read))
		case "/api/v1/status/buildinfo":
			io.WriteString(w, `{"status":"success","data":{"version":"3.15.0"}}`)
		case "/api/v1/status/flags":
			io.WriteString(w, `{"status":"success","data":{"query.lookback-delta":"2m"}}`)
		case "/api/v1/status/tsdb/blocks":
			io.WriteString(w, `{"status":"success","data":{"blocks":[{"ulid":"01","minTime":0,"maxTime":`+blocksEnd+`}]}}`)
		case "/api/v1/metadata":
			io.WriteString(w, `{"status":"success","data":{"up":[{"type":"gauge","help":"Whether the target answered.","unit":""}]}}`)
		case "/api/v1/series":
			io.WriteString(w, `{"status":"success","data":[{"__name__":"up","job":"z"},{"__name__":"up","job":"a"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer prometheus.Close()
	dir := t.TempDir()
	snapshot, spec, file := filepath.Join(dir, "snapshot"), filepath.Join(dir, "case.yaml"), filepath.Join(dir, "exported.jsonl.gz")
	os.MkdirAll(filepath.Join(snapshot, "namespaces", "shop", "v1", "pod"), 0o755)
	os.WriteFile(filepath.Join(snapshot, "namespaces", "shop", "v1", "pod", "gone.yaml"), []byte("apiVersion: v1\nkind: Pod\nmetadata: {name: gone, namespace: shop}\n"), 0o644)
	os.WriteFile(spec, []byte("id: packed\nprompt: \"why?\"\nexpected: [\"x\"]\nmust_not: [\"y\"]\nspecificity: gone\ndecoys: [NetworkPolicy]\nevidence: ['name: gone']\nmetrics: true\n"), 0o644)
	export := func() {
		t.Helper()
		if code, err := cmdExportMetrics(context.Background(), []string{"--url", prometheus.URL, "-o", file, "--at", "10", "--window", "1m"}); code != 0 || err != nil {
			t.Fatalf("export-metrics: %d %v", code, err)
		}
	}
	pack := func(out, metricsFile string) (int, error, map[string]any) {
		code, err := cmdPack(context.Background(), []string{spec, "--snapshot", snapshot, "--freeze-time", "10", "-o", out, "--metrics", metricsFile})
		var said struct{ Metrics map[string]any }
		raw, _ := os.ReadFile(filepath.Join(out, casefile.FreezeName))
		json.Unmarshal(raw, &said)
		return code, err, said.Metrics
	}
	export()
	beside, err := os.ReadFile(metrics.LearnedPath(file))
	if err != nil || !strings.Contains(string(beside), `"head_from_ms": 5000`) {
		t.Fatalf("export-metrics left beside its file: %s (%v)", beside, err)
	}
	// The pair, sealed: the case says what the export learned, and holds its own two files of metrics and no third.
	out := filepath.Join(dir, "case")
	code, err, said := pack(out, file)
	described, _ := os.ReadFile(filepath.Join(out, casefile.MetadataName))
	left, _ := filepath.Glob(filepath.Join(out, "*learned*"))
	if code != 0 || err != nil || said["from_ms"] != float64(10000-60000-120000) || said["head_from_ms"] != float64(5000) || said["series_order"] != casefile.OrderOfTheHead || said["prometheus_version"] != "3.15.0" ||
		said["lookback_delta_ms"] != float64(120000) || said["metadata_families"] != float64(1) || !strings.Contains(string(described), "Whether the target answered.") || len(left) != 0 {
		t.Errorf("pack of what export-metrics wrote: %d %v, the case says %v of its metrics, describes them with %q, and holds %v", code, err, said, described, left)
	}
	// The metrics file alone, as one moved without the other: sealed, and it says none of it.
	alone := filepath.Join(dir, "alone.jsonl.gz")
	samples, _ := os.ReadFile(file)
	os.WriteFile(alone, samples, 0o644)
	if code, err, said := pack(filepath.Join(dir, "case-alone"), alone); code != 0 || err != nil || said["series"] != float64(2) || said["from_ms"] != nil || said["series_order"] != nil || said["metadata_families"] != nil || said["lookback_delta_ms"] != nil {
		t.Errorf("pack of a metrics file with nothing beside it: %d %v, and the case says %v", code, err, said)
	}
	// Exported again under the same name, at the same instant, from a Prometheus whose blocks now end
	// elsewhere and one of whose targets has gone down: what is beside the file is of the new export.
	// And what the earlier one left, put back, is of a file that is no longer there: it is refused,
	// and no case is written.
	blocksEnd = "8000"
	read, _ = (&prompb.ReadResponse{Results: []*prompb.QueryResult{{Timeseries: []*prompb.TimeSeries{series("a", 0), series("z", 1)}}}}).Marshal()
	export()
	if later, _ := os.ReadFile(metrics.LearnedPath(file)); !strings.Contains(string(later), `"head_from_ms": 8000`) || bytes.Equal(later, beside) {
		t.Errorf("exported again, the file beside the metrics says %s", later)
	}
	os.WriteFile(metrics.LearnedPath(file), beside, 0o644)
	stale := filepath.Join(dir, "case-stale")
	if code, err, _ := pack(stale, file); code == 0 || err == nil || !strings.Contains(err.Error(), "is of another metrics file") {
		t.Errorf("pack of a metrics file beside what an earlier export left: %d %v", code, err)
	}
	if _, err := os.Stat(filepath.Join(stale, casefile.FreezeName)); err == nil {
		t.Error("a case was sealed from a metrics file and what was learned of another")
	}
	// An export that cannot write its metrics leaves nothing an earlier one left beside that name: what
	// was there is of another file, and goes first.
	blocked := filepath.Join(dir, "blocked.jsonl.gz")
	os.Mkdir(blocked, 0o755)
	os.WriteFile(metrics.LearnedPath(blocked), beside, 0o644)
	if code, err := cmdExportMetrics(context.Background(), []string{"--url", prometheus.URL, "-o", blocked, "--at", "10", "--window", "1m"}); code == 0 || err == nil {
		t.Errorf("export-metrics to a name that cannot be written: %d %v", code, err)
	}
	if _, err := os.Stat(metrics.LearnedPath(blocked)); err == nil {
		t.Error("what an earlier export left is still beside a name the export failed to write")
	}
	// And one that writes its metrics and cannot leave the second file says so, and fails: a directory
	// it may not add a file to, with the metrics file already in it.
	if os.Geteuid() != 0 { // nothing is closed to root
		os.Remove(metrics.LearnedPath(file))
		os.Chmod(dir, 0o555)
		code, err := cmdExportMetrics(context.Background(), []string{"--url", prometheus.URL, "-o", file, "--at", "10", "--window", "1m"})
		os.Chmod(dir, 0o755)
		if code == 0 || err == nil || !strings.Contains(err.Error(), "could not be written beside it") {
			t.Errorf("export-metrics where the second file cannot be written: %d %v", code, err)
		}
	}
}
