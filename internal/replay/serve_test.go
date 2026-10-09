package replay

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lapilli-project/lapilli/internal/casefile"
	"github.com/lapilli-project/lapilli/internal/freeze"
	"github.com/lapilli-project/lapilli/internal/guard"
	"github.com/lapilli-project/lapilli/internal/metrics"
)

// The snapshot server is someone else's binary, and no test here has it. This stands in for it: the
// test binary, run again as a child in its place, serves one pod's log the way the real one does —
// a line that has no time is given the time the server took its files up, which is some time after
// the case began to be served and is the same at every asking.
func TestMain(m *testing.M) {
	if os.Getenv("LAPILLI_TEST_SNAPSHOT_SERVER") == "1" {
		standInSnapshotServer(os.Args[1:])
		return
	}
	os.Exit(m.Run())
}

func standInSnapshotServer(args []string) { // serve -a <snapshot> -s <address> -k <kubeconfig>
	flags := map[string]string{}
	for i := 1; i+1 < len(args); i += 2 {
		flags[args[i]] = args[i+1]
	}
	tookUp := time.Now().UTC()
	config := fmt.Sprintf("apiVersion: v1\nkind: Config\ncurrent-context: snapshot\ncontexts:\n- name: snapshot\n  context: {cluster: snapshot, user: snapshot}\n"+
		"clusters:\n- name: snapshot\n  cluster: {server: %q}\nusers:\n- name: snapshot\n  user: {}\n", "http://"+flags["-s"]+"/kubernetes")
	if err := os.WriteFile(flags["-k"], []byte(config), 0o600); err != nil {
		os.Exit(1)
	}
	http.ListenAndServe(flags["-s"], http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/kubernetes/version":
			io.WriteString(w, `{"major":"1","minor":"37"}`)
		case strings.HasSuffix(r.URL.Path, "/pods/gone/log") && r.URL.Query().Get("timestamps") == "true":
			io.WriteString(w, tookUp.Format("2006-01-02T15:04:05.000000000Z07:00")+" unable to retrieve container logs for containerd://abc")
		default:
			http.NotFound(w, r)
		}
	}))
}

// A case served from end to end, with that stand-in behind it: unpacked, the snapshot server
// started, the front before it, and a kubeconfig that names the front. Through all of it, the line
// the snapshot holds without a time comes back without the time the snapshot server gave it.
func TestServingACaseTakesOffTheTimesTheSnapshotServerMakesUp(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	standIn := filepath.Join(t.TempDir(), "snapshot-server")
	if err := os.WriteFile(standIn, []byte("#!/bin/sh\nLAPILLI_TEST_SNAPSHOT_SERVER=1 exec \""+self+"\" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAPILLI_CRUST_GATHER", standIn)

	snapshot, caseDir := t.TempDir(), filepath.Join(t.TempDir(), "case")
	pod := filepath.Join(snapshot, "namespaces", "shop", "v1", "pod")
	if err := os.MkdirAll(pod, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(pod, "gone.yaml"), []byte("apiVersion: v1\nkind: Pod\nmetadata: {name: gone, namespace: shop}\n"), 0o644)
	spec := filepath.Join(t.TempDir(), "case.yaml")
	os.WriteFile(spec, []byte("id: served\nprompt: \"why?\"\nexpected: [\"x\"]\nmust_not: [\"y\"]\nspecificity: gone\ndecoys: [NetworkPolicy]\nevidence: ['name: gone']\n"), 0o644)
	if _, _, err := freeze.Pack(spec, snapshot, caseDir, float64(time.Now().Add(-time.Hour).Unix()), nil); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	s, err := Serve(ctx, caseDir, "", self)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pin, err := guard.LoadPin(s.Env["KUBECONFIG"])
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"?container=app", "?container=app&timestamps=true", "?container=app&tailLines=0"} {
		resp, err := http.Get(pin.Server + "/api/v1/namespaces/shop/pods/gone/log" + query)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "unable to retrieve container logs for containerd://abc" {
			t.Errorf("logs%s through a served case: %d %q", query, resp.StatusCode, body)
		}
	}
	// Closed, nothing of it answers any more: what stood in front of the snapshot server is gone with it.
	s.Close()
	if resp, err := http.Get(pin.Server + "/version"); err == nil {
		resp.Body.Close()
		t.Errorf("a served case that was closed still answers at %s", pin.Server)
	}

	// A snapshot server that does not come up is a case that is not served, and why is in the error:
	// what the server wrote before it gave up, since its log goes with the directory the session had.
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	for _, c := range []struct{ what, script, want string }{
		{"says why and gives up", "echo 'it cannot read the archive' >&2\nexit 3\n", "the snapshot API server did not start: it cannot read the archive"},
		{"gives up and says nothing", "exit 3\n", "the snapshot API server did not start: it wrote nothing"},
		{"says a great deal and gives up", "head -c 5000 /dev/zero | tr '\\0' x\necho ' and then the end'\nexit 3\n", "the snapshot API server did not start: … " + strings.Repeat("x", 2000-len(" and then the end")) + " and then the end"},
	} {
		dies := filepath.Join(t.TempDir(), "snapshot-server")
		os.WriteFile(dies, []byte("#!/bin/sh\n"+c.script), 0o755)
		t.Setenv("LAPILLI_CRUST_GATHER", dies)
		broken, err := Serve(ctx, caseDir, "", self)
		if left, _ := filepath.Glob(filepath.Join(scratch, "lapilli-case-*")); err == nil || err.Error() != c.want || broken != nil || len(left) != 0 {
			t.Errorf("a snapshot server that %s: %v; a session handed over: %v; left behind: %v", c.what, err, broken != nil, left)
		}
	}
	// And one that is not waited for to the end — the caller gave up first — is said to be that, and is stopped.
	hangs := filepath.Join(t.TempDir(), "snapshot-server")
	os.WriteFile(hangs, []byte("#!/bin/sh\nexec sleep 60\n"), 0o755)
	t.Setenv("LAPILLI_CRUST_GATHER", hangs)
	soon, stop := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer stop()
	began := time.Now()
	broken, err := Serve(soon, caseDir, "", self)
	if left, _ := filepath.Glob(filepath.Join(scratch, "lapilli-case-*")); err == nil || !strings.Contains(err.Error(), "was not waited for") || broken != nil || len(left) != 0 || time.Since(began) > 20*time.Second {
		t.Errorf("a snapshot server that hangs, and a caller that gives up: %v after %v; a session handed over: %v; left behind: %v", err, time.Since(began), broken != nil, left)
	}
}

// What a freeze learned of the Prometheus besides its samples is in freeze.json, not in the metrics
// file, and a served case has to give it back to the engine: a subquery that names no step is
// evaluated at that Prometheus's interval, not at the default.
func TestAServedCaseEvaluatesAsItsPrometheusWasSetTo(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	standIn := filepath.Join(t.TempDir(), "snapshot-server")
	if err := os.WriteFile(standIn, []byte("#!/bin/sh\nLAPILLI_TEST_SNAPSHOT_SERVER=1 exec \""+self+"\" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAPILLI_CRUST_GATHER", standIn)

	snapshot, caseDir := t.TempDir(), filepath.Join(t.TempDir(), "case")
	pod := filepath.Join(snapshot, "namespaces", "shop", "v1", "pod")
	if err := os.MkdirAll(pod, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(pod, "gone.yaml"), []byte("apiVersion: v1\nkind: Pod\nmetadata: {name: gone, namespace: shop}\n"), 0o644)
	spec := filepath.Join(t.TempDir(), "case.yaml")
	os.WriteFile(spec, []byte("id: served\nprompt: \"why?\"\nexpected: [\"x\"]\nmust_not: [\"y\"]\nspecificity: gone\ndecoys: [NetworkPolicy]\nevidence: ['name: gone']\nmetrics: true\n"), 0o644)
	frozen := time.Now().Add(-time.Hour).Truncate(15 * time.Second) // on a step of the subquery below, and so on a sample
	var ts []int64
	var vs []float64
	for at := frozen.Add(-3 * time.Minute); !at.After(frozen); at = at.Add(5 * time.Second) {
		ts, vs = append(ts, at.UnixMilli()), append(vs, 1)
	}
	// Its metrics begin three minutes before the freeze, its Prometheus's blocks ended a minute before
	// it, and its head held the job z before the job a.
	store := &metrics.Store{Source: metrics.Source{Version: "3.5.0", EvaluationInterval: 15 * time.Second, LookbackDelta: 2 * time.Second},
		Metadata: map[string][]metrics.Metadata{"up": {{Type: "gauge", Help: "Whether the target answered."}}},
		From:     frozen.Add(-3 * time.Minute).UnixMilli(), HeadFrom: frozen.Add(-time.Minute).UnixMilli(), OrderKnown: true, ExternalLabels: map[string]string{"cluster": "prod"}}
	if err := store.Add(map[string]string{"__name__": "up", "job": "z"}, ts, vs); err != nil {
		t.Fatal(err)
	}
	if err := store.Add(map[string]string{"__name__": "up", "job": "a"}, ts, vs); err != nil {
		t.Fatal(err)
	}
	info, _, err := freeze.Pack(spec, snapshot, caseDir, float64(frozen.Unix()), store)
	if err != nil {
		t.Fatal(err)
	}
	if info.Metrics.PrometheusVersion != "3.5.0" || info.Metrics.EvaluationIntervalMs != 15000 || info.Metrics.LookbackDeltaMs != 2000 {
		t.Fatalf("the freeze recorded %+v of its Prometheus", info.Metrics)
	}
	// What the export learned besides the samples is in freeze.json, under the names a replay and anyone's
	// script read it by.
	written, _ := os.ReadFile(filepath.Join(caseDir, casefile.FreezeName))
	for _, key := range []string{fmt.Sprintf(`"from_ms": %d`, store.From), fmt.Sprintf(`"head_from_ms": %d`, store.HeadFrom), `"series_order": "head"`, `"metadata_families": 1`, `"cluster": "prod"`, `"external_labels": {`} {
		if !strings.Contains(string(written), key) {
			t.Errorf("freeze.json does not say %s:\n%s", key, written)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	s, err := Serve(ctx, caseDir, "", self)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	resp, err := http.Get(s.Env["PROM_URL"] + "/api/v1/query?query=" + url.QueryEscape(`count_over_time(up[2m:])`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Count(string(body), `"8"]`) != 2 { // two minutes at fifteen seconds; at the default of one minute, two
		t.Errorf("a subquery without a step, through a served case: %s", body)
	}
	// It hands a query its series as its Prometheus's head held them, and by label where the query looks
	// back to before the blocks ended; and of a query that looks back to before its metrics begin, it says so.
	ask := func(query string) string {
		resp, err := http.Get(s.Env["PROM_URL"] + "/api/v1/query?query=" + url.QueryEscape(query))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}
	if got := ask(`up`); strings.Index(got, `"job":"z"`) > strings.Index(got, `"job":"a"`) || strings.Contains(got, "frozen case") {
		t.Errorf("up, through a served case whose head held z before a: %s", got)
	}
	if got := ask(`up offset 90s`); strings.Index(got, `"job":"a"`) > strings.Index(got, `"job":"z"`) || strings.Contains(got, "frozen case") {
		t.Errorf("up ninety seconds ago, which reaches a block, through a served case: %s", got)
	}
	if got := ask(`up offset 10m`); !strings.Contains(got, `"warnings":["frozen case: it holds no samples before `+time.UnixMilli(store.From).UTC().Format("2006-01-02T15:04:05.000Z")+`, and this query looks 7m2s further back than that`) {
		t.Errorf("up ten minutes ago, which is before the case, through a served case: %s", got)
	}
	// And it says what kind of metric each is, as its Prometheus did.
	if resp, err = http.Get(s.Env["PROM_URL"] + "/api/v1/metadata?metric=up"); err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"up":[{"type":"gauge","help":"Whether the target answered.","unit":""}]`) {
		t.Errorf("what kind of metric up is, through a served case: %s", body)
	}
	// And it looks back as far as that Prometheus did: two seconds before the freeze the newest sample is
	// three seconds old, which is too old for one set to two and would not be for the default of five minutes.
	for query, want := range map[string]string{`up offset 2s`: `"result":[]`, `up offset 4s`: `"result":[{`} {
		resp, err := http.Get(s.Env["PROM_URL"] + "/api/v1/query?query=" + url.QueryEscape(query))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), want) {
			t.Errorf("%s through a served case whose Prometheus looked back two seconds: %s", query, body)
		}
	}

	// A case that says it describes its metrics has to have the file that does: one that is not there, or
	// cannot be read, is a case that cannot be served. And one that says nothing of it is served without.
	s.Close()
	if resp, err := http.Get(s.Env["PROM_URL"] + "/-/ready"); err == nil { // and closed, its metrics answer no more
		resp.Body.Close()
		t.Errorf("a served case that was closed still answers for its metrics at %s", s.Env["PROM_URL"])
	}
	described := filepath.Join(caseDir, casefile.MetadataName)
	for what, spoil := range map[string]func() error{"that is no JSON": func() error { return os.WriteFile(described, []byte("{"), 0o644) }, "that is gone": func() error { return os.Remove(described) }} {
		if err := spoil(); err != nil {
			t.Fatal(err)
		}
		broken, err := Serve(ctx, caseDir, "", self)
		if err == nil {
			broken.Close()
		}
		if err == nil || !strings.Contains(err.Error(), casefile.MetadataName) {
			t.Errorf("a case with a metadata file %s was served, or refused for something else: %v", what, err)
		}
	}
	store.Metadata = nil
	if _, _, err := freeze.Pack(spec, snapshot, caseDir, float64(frozen.Unix()), store); err != nil {
		t.Fatal(err)
	}
	// And a metrics file that cannot be read is said to be that.
	samples, _ := os.ReadFile(filepath.Join(caseDir, casefile.MetricsName))
	// It is refused with no session handed over, and with nothing of one left behind: the directory it
	// worked in is gone, which its error does not name.
	os.WriteFile(filepath.Join(caseDir, casefile.MetricsName), []byte("not gzip"), 0o644)
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	broken, err := Serve(ctx, caseDir, "", self)
	if err == nil || !strings.Contains(err.Error(), casefile.MetricsName) {
		if err == nil {
			broken.Close()
		}
		t.Errorf("a case whose metrics file is no metrics file was served, or refused without naming it: %v", err)
	} else if left, _ := filepath.Glob(filepath.Join(scratch, "lapilli-case-*")); broken != nil || len(left) != 0 {
		t.Errorf("a case that was refused left a session behind (%v), or its working directory: %v", broken != nil, left)
	}
	os.WriteFile(filepath.Join(caseDir, casefile.MetricsName), samples, 0o644)
	os.WriteFile(described, []byte("{"), 0o644) // and a file it does not say it has is not read
	without, err := Serve(ctx, caseDir, "", self)
	if err != nil {
		t.Fatalf("a case that carries no metadata was not served: %v", err)
	}
	defer without.Close()
	resp, err = http.Get(without.Env["PROM_URL"] + "/api/v1/metadata")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"data":{}`) {
		t.Errorf("a case that carries no metadata answered %s", body)
	}
}
