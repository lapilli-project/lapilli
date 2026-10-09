package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/golang/snappy"
	"github.com/lapilli-project/lapilli/internal/casefile"
	"github.com/lapilli-project/lapilli/internal/freeze"
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
	if len(os.Args) > 1 && os.Args[1] == "promq" { // bin/promq hands over to it too
		code, err := cmdPromq(context.Background(), os.Args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "lapilli-case promq:", err)
			code = max(code, 1)
		}
		os.Exit(code)
	}
	if os.Getenv("LAPILLI_TEST_SNAPSHOT_SERVER") == "1" {
		standInSnapshotServer(os.Args[1:])
		return
	}
	os.Exit(m.Run())
}

// The snapshot server is someone else's binary, and no test here has it. This stands in for it, as
// the one in internal/replay does: it writes the kubeconfig that names it and listens where it is
// told. What a kubectl would ask it, the stand-in for kubectl below answers itself.
func standInSnapshotServer(args []string) { // serve -a <snapshot> -s <address> -k <kubeconfig>
	flags := map[string]string{}
	for i := 1; i+1 < len(args); i += 2 {
		flags[args[i]] = args[i+1]
	}
	config := fmt.Sprintf("apiVersion: v1\nkind: Config\ncurrent-context: snapshot\ncontexts:\n- name: snapshot\n  context: {cluster: snapshot, user: snapshot}\n"+
		"clusters:\n- name: snapshot\n  cluster: {server: %q}\nusers:\n- name: snapshot\n  user: {}\n", "http://"+flags["-s"]+"/kubernetes")
	if err := os.WriteFile(flags["-k"], []byte(config), 0o600); err != nil {
		os.Exit(1)
	}
	http.ListenAndServe(flags["-s"], http.NotFoundHandler())
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

// `reach` serves a case and runs what each evidence item names as its witness — a kubectl command, a
// query — through what an agent of that case is handed: the kubectl on its PATH, which is the guard,
// and promq. It says of each whether the witness printed the item, and fails when one did not.
func TestReachRunsEachWitnessThroughWhatAnAgentIsHanded(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	standIn, real := filepath.Join(dir, "snapshot-server"), filepath.Join(dir, "real")
	os.WriteFile(standIn, []byte("#!/bin/sh\nLAPILLI_TEST_SNAPSHOT_SERVER=1 exec \""+self+"\" \"$@\"\n"), 0o755)
	t.Setenv("LAPILLI_CRUST_GATHER", standIn)
	// A kubectl that prints what the frozen cluster would, for the three things it is asked here, and
	// says what it was run with: the pin the guard puts on every command is among it.
	os.MkdirAll(real, 0o755)
	os.WriteFile(filepath.Join(real, "kubectl"), []byte(`#!/bin/sh
case "$*" in
  "-n shop logs deploy/cache --kubeconfig="*" --context=case "*) echo "2026-10-05T18:57:12Z conn-table active=40/40 queued=7" ;;
  "-n shop get configmap app -o yaml --kubeconfig="*) echo "data:"; echo "  CACHE_CONN_MODE: per-task" ;;
  *"get pods"*) echo "NAME   READY"; echo "nothing of the kind"; echo "a complaint" >&2; exit 3 ;;
  *"get events"*) echo "conn-table active=40/40, and only where a complaint goes" >&2 ;;
  *) echo "asked: $*" ;;
esac
`), 0o755)
	t.Setenv("PATH", real+string(os.PathListSeparator)+os.Getenv("PATH"))
	snapshot := filepath.Join(dir, "snapshot")
	os.MkdirAll(filepath.Join(snapshot, "namespaces", "shop", "v1", "pod"), 0o755)
	os.WriteFile(filepath.Join(snapshot, "namespaces", "shop", "v1", "pod", "cache.yaml"), []byte("apiVersion: v1\nkind: Pod\nmetadata: {name: cache, namespace: shop}\n# conn-table active=40/40\n# CACHE_CONN_MODE\n"), 0o644)
	store := &metrics.Store{}
	now := time.Now().Add(-time.Hour)
	store.Add(map[string]string{"__name__": "up", "client": "indexer"}, []int64{now.Add(-time.Minute).UnixMilli(), now.UnixMilli()}, []float64{1, 1})
	sealed := func(name, evidence string) string {
		t.Helper()
		spec, out := filepath.Join(dir, name+".yaml"), filepath.Join(dir, name)
		os.WriteFile(spec, []byte("id: "+name+"\nprompt: \"why?\"\nexpected: [\"x\"]\nmust_not: [\"y\"]\nspecificity: cache\ndecoys: [NetworkPolicy]\nmetrics: true\nevidence:\n"+evidence), 0o644)
		if _, _, err := freeze.Pack(spec, snapshot, out, float64(now.UnixMilli())/1000, store); err != nil {
			t.Fatal(err)
		}
		return out
	}
	reach := func(args ...string) (string, int, error) {
		t.Helper()
		was := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		code, err := cmdReach(ctx, args)
		cancel()
		w.Close()
		os.Stdout = was
		printed, _ := io.ReadAll(r)
		return string(printed), code, err
	}
	const logs, configmap, metric = "  - {pattern: 'conn-table active=40/40', command: 'kubectl -n shop logs deploy/cache'}\n", "  - {pattern: 'CACHE_CONN_MODE', command: \"kubectl -n shop get configmap app -o yaml\"}\n",
		"  - {pattern: 'client=indexer\\} 1', store: metrics, query: 'up'}\n"

	// Every item names its witness, and each prints its item.
	whole := sealed("whole", logs+configmap+metric)
	printed, code, err := reach("--every", whole)
	if code != 0 || err != nil || strings.Count(printed, "  reached  ") != 3 || strings.Contains(printed, "NOT") ||
		!strings.Contains(printed, "'conn-table active=40/40'  by  kubectl -n shop logs deploy/cache\n") || !strings.Contains(printed, "by  promq 'up'\n") || !strings.HasPrefix(printed, whole+":\n") {
		t.Errorf("a case whose every item is reached: %d %v\n%s", code, err, printed)
	}
	// A witness that prints something else does not reach its item, and what it printed is shown: what
	// it printed, what it complained of, and how it ended.
	missed := sealed("missed", logs+"  - {pattern: 'CACHE_CONN_MODE', command: 'kubectl -n shop get pods'}\n"+metric)
	printed, code, err = reach(missed)
	if code != 2 || err != nil || strings.Count(printed, "  reached  ") != 2 || !strings.Contains(printed, "  NOT REACHED       'CACHE_CONN_MODE'  by  kubectl -n shop get pods\n") ||
		!strings.Contains(printed, "      | nothing of the kind\n") || !strings.Contains(printed, "      | a complaint\n") || !strings.Contains(printed, "      | (exit status 3)\n") {
		t.Errorf("a case one of whose witnesses prints something else: %d %v\n%s", code, err, printed)
	}
	// What a witness complains of is not what it printed: an item that is only there is not reached.
	printed, code, _ = reach(sealed("complained", "  - {pattern: 'conn-table active=40/40', command: 'kubectl -n shop get events'}\n"+metric))
	if code != 2 || !strings.Contains(printed, "  NOT REACHED       'conn-table active=40/40'  by  kubectl -n shop get events\n") || !strings.Contains(printed, "only where a complaint goes") {
		t.Errorf("a case whose witness prints its item only where a complaint goes: %d\n%s", code, printed)
	}
	// And a query that prints something else, likewise.
	printed, code, _ = reach(sealed("no-such", logs+configmap+"  - {pattern: 'client=indexer\\} 1', store: metrics, query: 'up offset 30m'}\n"))
	if code != 2 || !strings.Contains(printed, "  NOT REACHED       'client=indexer\\} 1'  by  promq 'up offset 30m'\n") || !strings.Contains(printed, "      | (empty result)\n") {
		t.Errorf("a case whose query prints nothing of its item: %d\n%s", code, printed)
	}
	// A witness goes through the guard, as an agent's command does: one that is no read is refused
	// there, reaches nothing, and the real kubectl is never run.
	printed, code, _ = reach(sealed("a-write", "  - {pattern: 'conn-table active=40/40', command: 'kubectl -n shop delete pod cache'}\n"+metric))
	if code != 2 || !strings.Contains(printed, "NOT REACHED") || !strings.Contains(printed, "kubectl refused by lapilli-case") || strings.Contains(printed, "asked:") {
		t.Errorf("a case whose witness is a write: %d\n%s", code, printed)
	}
	// An item that names no witness is said to name none. It fails only where every item is asked to.
	bare := sealed("bare", "  - 'conn-table active=40/40'\n"+configmap+"  - {pattern: 'client=indexer', store: metrics}\n")
	printed, code, _ = reach(bare)
	if code != 0 || !strings.Contains(printed, "  names no command  'conn-table active=40/40', in the kubernetes store\n") || !strings.Contains(printed, "  names no query    'client=indexer', in the metrics store\n") || strings.Count(printed, "  reached  ") != 1 {
		t.Errorf("a case two of whose items name no witness: %d\n%s", code, printed)
	}
	if printed, code, _ = reach("--every", bare); code != 2 || strings.Contains(printed, "NOT REACHED") {
		t.Errorf("the same, where every item has to name one: %d\n%s", code, printed)
	}
	// Several cases are each served and each said; one that fails fails the whole.
	if printed, code, _ = reach(whole, missed); code != 2 || !strings.Contains(printed, whole+":\n") || !strings.Contains(printed, missed+":\n") {
		t.Errorf("two cases, one of them with an item not reached: %d\n%s", code, printed)
	}
	if _, code, err = reach(); code != 2 || err == nil {
		t.Errorf("no case named: %d %v", code, err)
	}
	if _, code, err = reach(filepath.Join(dir, "no-such-case")); code != 1 || err == nil {
		t.Errorf("a case that is not there: %d %v", code, err)
	}
}
