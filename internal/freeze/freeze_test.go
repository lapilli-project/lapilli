package freeze

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang/snappy"
	"github.com/lapilli-project/lapilli/internal/casefile"
	"github.com/lapilli-project/lapilli/internal/metrics"
	"github.com/prometheus/prometheus/prompb"
)

const spec = `
id: demo
prompt: "why?"
expected: ["the cache is full"]
must_not: ["blames the network policy"]
specificity: report-worker
decoys: [NetworkPolicy]
evidence:
  - 'conn-table active=40/40'
  - 'CACHE_CONN_MODE'
`

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

const secret = `apiVersion: v1
kind: Secret
metadata:
  name: db
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: '{"data":{"password":"aHVudGVyMg=="}}'
    kept: "yes"
data:
  password: aHVudGVyMg==
stringData:
  token: plain-token
`

func snapshot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"namespaces/shop/secret/db.yaml":        secret,
		"namespaces/shop/secret/broken.yaml":    "{ password: aHVudGVyMg==",
		"namespaces/shop/configmap/app.yaml":    "data:\n  CACHE_CONN_MODE: per-task\n",
		"namespaces/shop/pod/cache-0/cache.log": "conn-table active=40/40\n",
	})
	return dir
}

func TestRedactSecretsRemovesEveryValueAndKeepsTheKeys(t *testing.T) {
	dir := snapshot(t)
	n, err := RedactSecrets(dir)
	if err != nil || n != 2 {
		t.Fatalf("touched %d Secrets (%v), want 2", n, err)
	}
	got := read(t, filepath.Join(dir, "namespaces/shop/secret/db.yaml"))
	for _, leaked := range []string{"aHVudGVyMg==", "plain-token", "last-applied-configuration"} {
		if strings.Contains(got, leaked) {
			t.Errorf("%q is still in the Secret:\n%s", leaked, got)
		}
	}
	for _, kept := range []string{"password: " + Redacted, "token: " + Redacted, "kept:", "name: db"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%q is gone from the Secret:\n%s", kept, got)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "namespaces/shop/secret/broken.yaml")); !os.IsNotExist(err) {
		t.Error("a Secret that could not be parsed was left in the snapshot with its values")
	}
	if got := read(t, filepath.Join(dir, "namespaces/shop/configmap/app.yaml")); got != "data:\n  CACHE_CONN_MODE: per-task\n" {
		t.Errorf("a file with no Secret in it was rewritten: %q", got)
	}
}

// A snapshot does not have to come from crust-gather, and a Secret does not have to lie in a
// directory called `secret`.
func TestRedactSecretsGoesByWhatAFileContains(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"dump/everything.yaml": "apiVersion: v1\nkind: ConfigMap\ndata: {a: keep-me}\n---\napiVersion: v1\nkind: Secret\nmetadata: {name: one}\ndata: {password: c2VjcmV0MQ==}\n---\nkind: Secret\nmetadata: {name: two}\nstringData: {token: secret2}\n",
		"dump/list.json":       `{"kind":"List","items":[{"kind":"Pod","metadata":{"name":"p"}},{"kind":"Secret","metadata":{"name":"three"},"data":{"k":"c2VjcmV0Mw=="}}]}`,
		"dump/secrets.yaml":    "kind: SecretList\nitems:\n- metadata: {name: four}\n  data: {k: c2VjcmV0NA==}\n",
		"dump/app.log":         "kind: Secret is only a word in a log line, data: c2VjcmV0NQ==\n",
		"dump/notes.yaml":      "just: {some: yaml}\n",
	})
	n, err := RedactSecrets(dir)
	if err != nil || n != 4 {
		t.Fatalf("touched %d Secrets (%v), want 4", n, err)
	}
	all := read(t, filepath.Join(dir, "dump/everything.yaml")) + read(t, filepath.Join(dir, "dump/list.json")) + read(t, filepath.Join(dir, "dump/secrets.yaml"))
	for _, leaked := range []string{"c2VjcmV0MQ==", "secret2", "c2VjcmV0Mw==", "c2VjcmV0NA=="} {
		if strings.Contains(all, leaked) {
			t.Errorf("%q survived", leaked)
		}
	}
	for _, kept := range []string{"keep-me", "name: one", "name: two", "name: three", "name: four", "name: p", "password: " + Redacted} {
		if !strings.Contains(all, kept) {
			t.Errorf("%q is gone", kept)
		}
	}
	if got := read(t, filepath.Join(dir, "dump/app.log")); !strings.Contains(got, "c2VjcmV0NQ==") {
		t.Error("a log file was rewritten; logs are copied as they are, and the docs say so")
	}
	if got := read(t, filepath.Join(dir, "dump/notes.yaml")); got != "just: {some: yaml}\n" {
		t.Errorf("a file with no Secret in it was rewritten: %q", got)
	}
}

func TestPackSealsACaseThatSaysWhetherItCanBeSolved(t *testing.T) {
	caseYAML := filepath.Join(t.TempDir(), "case.yaml")
	os.WriteFile(caseYAML, []byte(spec), 0o644)
	out := filepath.Join(t.TempDir(), "case")
	info, manifest, err := Pack(caseYAML, snapshot(t), out, 1791228354.43095, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]bool{"conn-table active=40/40": true, "CACHE_CONN_MODE": true}; !reflect.DeepEqual(info.EvidenceInSnapshot, want) {
		t.Errorf("evidence = %v", info.EvidenceInSnapshot)
	}
	if info.SecretsRedacted != 2 || info.FrozenAt != "2026-10-05T19:25:54Z" || !reflect.DeepEqual(info.Stores, []string{"kubernetes"}) || info.Metrics != nil {
		t.Errorf("info = %+v", info)
	}
	if problems, err := casefile.Verify(out); err != nil || len(problems) != 0 {
		t.Errorf("a freshly packed case does not verify: %v %v", problems, err)
	}
	if stored, err := casefile.LoadFreezeInfo(out); err != nil || !reflect.DeepEqual(stored, info) {
		t.Errorf("freeze.json holds %+v (%v), Pack returned %+v", stored, err, info)
	}
	if len(manifest.Files) != 3 {
		t.Errorf("sealed files = %v", manifest.Files)
	}

	// The same tree packs to the same bytes, whenever and wherever this build packs it.
	again := filepath.Join(t.TempDir(), "case")
	if _, second, err := Pack(caseYAML, snapshot(t), again, 1791228354.43095, nil); err != nil || second.Digest != manifest.Digest {
		t.Errorf("a second pack of the same incident sealed to %s (%v), the first to %s", second.Digest, err, manifest.Digest)
	}

	// Evidence that is not in the copy is reported, not hidden.
	bare := t.TempDir()
	write(t, bare, map[string]string{"namespaces/shop/pod/cache-0/cache.log": "conn-table active=40/40\n"})
	info, _, err = Pack(caseYAML, bare, filepath.Join(t.TempDir(), "case"), 1, nil)
	if err != nil || info.EvidenceInSnapshot["CACHE_CONN_MODE"] || !info.EvidenceInSnapshot["conn-table active=40/40"] {
		t.Errorf("evidence = %v (%v)", info.EvidenceInSnapshot, err)
	}
}

func TestPackCarriesAMetricsStoreOnlyWhenTheCaseSaysSo(t *testing.T) {
	dir := t.TempDir()
	without, with := filepath.Join(dir, "without.yaml"), filepath.Join(dir, "with.yaml")
	os.WriteFile(without, []byte(spec), 0o644)
	os.WriteFile(with, []byte(spec+"  - {pattern: 'client=indexer', store: metrics}\nmetrics: true\n"), 0o644)
	store := &metrics.Store{}
	store.Add(map[string]string{"__name__": "up"}, []int64{1000, 2000}, []float64{1, 1})

	if _, _, err := Pack(without, snapshot(t), filepath.Join(dir, "a"), 1, store); err == nil {
		t.Error("a metrics store was packed into a case that declares none")
	}
	if _, _, err := Pack(with, snapshot(t), filepath.Join(dir, "b"), 1, nil); err == nil {
		t.Error("a case that promises metrics was packed without any")
	}
	if _, _, err := Pack(with, snapshot(t), filepath.Join(dir, "c"), 1, &metrics.Store{}); err == nil {
		t.Error("an empty metrics store was packed")
	}
	info, manifest, err := Pack(with, snapshot(t), filepath.Join(dir, "d"), 1, store)
	if err != nil {
		t.Fatal(err)
	}
	if info.Metrics == nil || info.Metrics.PrometheusVersion != "" || info.Metrics.EvaluationIntervalMs != 0 {
		t.Errorf("a store that does not say where it came from was described: %+v", info.Metrics)
	}
	if info.Metrics == nil || info.Metrics.Series != 1 || info.Metrics.Samples != 2 || info.Metrics.Oldest != 1000 || info.Metrics.Newest != 2000 || !reflect.DeepEqual(info.Stores, []string{"kubernetes", "metrics"}) {
		t.Errorf("info = %+v %+v", info, info.Metrics)
	}
	if _, ok := manifest.Files[casefile.MetricsName]; !ok {
		t.Errorf("the metrics file is not sealed: %v", manifest.Files)
	}
	// Evidence in the metrics store is looked for in the metrics, and its absence is reported like any other.
	if found, checked := info.EvidenceInSnapshot["client=indexer"]; !checked || found {
		t.Errorf("metrics evidence that is not in the store: checked=%v found=%v", checked, found)
	}
	indexed := &metrics.Store{}
	indexed.Add(map[string]string{"__name__": "requests_total", "client": "indexer"}, []int64{1000, 2000}, []float64{1, 7})
	if info, _, err := Pack(with, snapshot(t), filepath.Join(dir, "e"), 2, indexed); err != nil || !info.EvidenceInSnapshot["client=indexer"] {
		t.Errorf("metrics evidence that is in the store: %v (%v)", info, err)
	}
	// A witness query narrows where the pattern must show up, and a query that does not evaluate is a broken key.
	witness := filepath.Join(dir, "witness.yaml")
	os.WriteFile(witness, []byte(spec+"  - {pattern: '(?m)^\\{client=indexer\\} 7$', store: metrics, query: 'sum by (client) (requests_total)'}\nmetrics: true\n"), 0o644)
	if info, _, err := Pack(witness, snapshot(t), filepath.Join(dir, "f"), 2, indexed); err != nil || !info.EvidenceInSnapshot[`(?m)^\{client=indexer\} 7$`] {
		t.Errorf("a witness query: %+v (%v)", info, err)
	}
	broken := filepath.Join(dir, "broken.yaml")
	os.WriteFile(broken, []byte(spec+"  - {pattern: 'x', store: metrics, query: 'sum('}\nmetrics: true\n"), 0o644)
	if _, _, err := Pack(broken, snapshot(t), filepath.Join(dir, "g"), 2, indexed); err == nil || !strings.Contains(err.Error(), "query") {
		t.Errorf("a witness query that does not parse gave %v", err)
	}
	back, err := metrics.Load(filepath.Join(dir, "d", casefile.MetricsName))
	var a, b bytes.Buffer
	if store.Write(&a); err != nil || back.Write(&b) != nil || !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Errorf("the packed store differs from the one given (%v)", err)
	}

	// What the Prometheus knew of its metrics is a file of the case, sealed with it, and freeze.json
	// says how many families it describes. A store that knows of none has no such file, and one packed
	// where a case stood that had it does not leave the old one behind.
	if _, sealed := manifest.Files[casefile.MetadataName]; sealed || info.Metrics.MetadataFamilies != 0 {
		t.Errorf("a store that describes no metric was packed with %d described and %v sealed", info.Metrics.MetadataFamilies, manifest.Files)
	}
	// Nor does a case say anything of the order of its series, of where its metrics begin or its
	// Prometheus's blocks ended, or of labels taken off, that its store did not learn.
	if written, _ := os.ReadFile(filepath.Join(dir, "d", casefile.FreezeName)); strings.Contains(string(written), "series_order") || strings.Contains(string(written), "from_ms") || strings.Contains(string(written), "external_labels") {
		t.Errorf("a store that learned nothing of its Prometheus was packed with:\n%s", written)
	}
	described := &metrics.Store{Metadata: map[string][]metrics.Metadata{"requests_total": {{Type: "counter", Help: "Requests."}}, "up": {{Type: "gauge"}}}}
	described.Add(map[string]string{"__name__": "requests_total", "client": "indexer"}, []int64{1000, 2000}, []float64{1, 7})
	h := filepath.Join(dir, "h")
	info, manifest, err = Pack(with, snapshot(t), h, 2, described)
	if err != nil || info.Metrics.MetadataFamilies != 2 || manifest.Files[casefile.MetadataName] == "" {
		t.Fatalf("a store that describes its metrics: %+v, sealed %v (%v)", info, manifest, err)
	}
	if kept, err := metrics.LoadMetadata(filepath.Join(h, casefile.MetadataName)); err != nil || !reflect.DeepEqual(kept, described.Metadata) {
		t.Errorf("the case's metadata file holds %v (%v)", kept, err)
	}
	if left, err := casefile.Verify(h); err != nil || len(left) != 0 {
		t.Errorf("the case with its metadata does not verify: %v (%v)", left, err)
	}
	for _, none := range []map[string][]metrics.Metadata{nil, {}} {
		described.Metadata = none
		info, manifest, err = Pack(with, snapshot(t), h, 2, described)
		_, there := os.Stat(filepath.Join(h, casefile.MetadataName))
		if _, sealed := manifest.Files[casefile.MetadataName]; err != nil || sealed || info.Metrics.MetadataFamilies != 0 || !errors.Is(there, os.ErrNotExist) {
			t.Errorf("packed again with %#v described: %d families, sealed %v, the file: %v (%v)", none, info.Metrics.MetadataFamilies, sealed, there, err)
		}
	}
}

// Measured on a kind cluster with crust-gather v0.17.1: with this flag a collection creates no pod
// and no event; without it, a pod with the host's process namespace on every node, and their events.
func TestFreezeOnlyReadsUnlessNodeLogsAreAskedFor(t *testing.T) {
	if got := strings.Join(collectArgs("/k", "/s", false), " "); got != "collect -k /k -f /s --disable-additional-logs" {
		t.Errorf("by default the collector is run as: %s", got)
	}
	if got := strings.Join(collectArgs("/k", "/s", true), " "); got != "collect -k /k -f /s" {
		t.Errorf("with node logs the collector is run as: %s", got)
	}
}

func TestFreezeRefusesTheDefaultKubeconfig(t *testing.T) {
	if _, _, err := Freeze(t.Context(), Options{CaseYAML: "case.yaml", OutDir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "kubeconfig is required") {
		t.Errorf("Freeze without a kubeconfig gave %v", err)
	}
}

// The collector takes the logs of a pod's containers and of none of its init containers, which is
// where a failed migration says why. They are taken after it, into the place it would have put them.
func TestTheLogsOfInitContainersAreTakenToo(t *testing.T) {
	snapshot := t.TempDir()
	pods := filepath.Join(snapshot, "namespaces", "shop", "v1", "pod")
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A pod stuck in its second init container: the first ran to its end, the second has failed
	// twice and waits, a third has not started; a sidecar runs; the main container has a log already.
	write(filepath.Join(pods, "api-1.yaml"), `apiVersion: v1
kind: Pod
metadata: {name: api-1, namespace: shop}
spec:
  initContainers: [{name: schema}, {name: migrate}, {name: warm}, {name: mesh, restartPolicy: Always}]
  containers: [{name: app}]
status:
  initContainerStatuses:
    - {name: schema, state: {terminated: {exitCode: 0}}}
    - {name: migrate, restartCount: 2, state: {waiting: {reason: CrashLoopBackOff}}, lastState: {terminated: {exitCode: 1}}}
    - {name: warm, state: {waiting: {reason: PodInitializing}}}
    - {name: mesh, state: {running: {}}}
  containerStatuses: [{name: app, state: {waiting: {reason: PodInitializing}}}]
  ephemeralContainerStatuses: [{name: debugger, state: {terminated: {exitCode: 0}}}]
`)
	write(filepath.Join(pods, "api-1", "app", "current.log"), "the collector's own\n")
	write(filepath.Join(pods, "api-1", "mesh", "current.log"), "already there\n")
	write(filepath.Join(pods, "plain.yaml"), "apiVersion: v1\nkind: Pod\nmetadata: {name: plain, namespace: shop}\nspec: {containers: [{name: app}]}\nstatus: {containerStatuses: [{name: app, state: {running: {}}}]}\n")
	write(filepath.Join(pods, "not-a-pod.yaml"), "just: text\n")
	// What a snapshot from a stranger might hold: names that are a path, or a flag.
	for file, pod := range map[string]string{
		"climbs.yaml":  `metadata: {name: "../../../climbs", namespace: shop}` + "\nstatus: {initContainerStatuses: [{name: init, state: {terminated: {}}}]}\n",
		"flag.yaml":    `metadata: {name: flag, namespace: shop}` + "\nstatus: {initContainerStatuses: [{name: --kubeconfig=/elsewhere, state: {terminated: {}}}]}\n",
		"nowhere.yaml": `metadata: {name: nowhere, namespace: "--all-namespaces"}` + "\nstatus: {initContainerStatuses: [{name: init, state: {terminated: {}}}]}\n",
	} {
		write(filepath.Join(pods, file), "apiVersion: v1\nkind: Pod\n"+pod)
	}

	// And a file that is not where its pod would be filed: it names a pod of another namespace.
	write(filepath.Join(pods, "elsewhere.yaml"), "apiVersion: v1\nkind: Pod\nmetadata: {name: other, namespace: kube-system}\nstatus: {initContainerStatuses: [{name: init, state: {terminated: {}}}]}\n")

	var asked []string
	taken, missing, err := takeLogsLeftOut(context.Background(), snapshot, func(ctx context.Context, l leftOut, to io.Writer) error {
		asked = append(asked, fmt.Sprintf("%s/%s/%s previous=%v", l.Namespace, l.Pod, l.Container, l.Previous))
		if _, bounded := ctx.Deadline(); !bounded {
			t.Errorf("%v is fetched with no limit on how long it may take", l)
		}
		if l.Container == "debugger" {
			io.WriteString(to, "half of a log, and then")
			return errors.New("the kubelet has thrown it away")
		}
		_, err := io.WriteString(to, "2026-10-07T09:05:26.000000001Z from "+l.Container+"\n")
		return err
	})
	sort.Strings(asked)
	want := []string{"shop/api-1/debugger previous=false", "shop/api-1/migrate previous=false", "shop/api-1/migrate previous=true", "shop/api-1/schema previous=false"}
	if err != nil || taken != 3 || !reflect.DeepEqual(asked, want) || !reflect.DeepEqual(missing, []string{"shop/api-1/debugger"}) {
		t.Fatalf("took %d and missed %v (%v), having asked for %v; want 3 of %v", taken, missing, err, asked, want)
	}
	for path, content := range map[string]string{
		"api-1/schema/current.log":   "2026-10-07T09:05:26.000000001Z from schema\n",
		"api-1/migrate/current.log":  "2026-10-07T09:05:26.000000001Z from migrate\n",
		"api-1/migrate/previous.log": "2026-10-07T09:05:26.000000001Z from migrate\n",
		"api-1/mesh/current.log":     "already there\n",
		"api-1/app/current.log":      "the collector's own\n",
	} {
		if got, err := os.ReadFile(filepath.Join(pods, filepath.FromSlash(path))); err != nil || string(got) != content {
			t.Errorf("%s: %q (%v)", path, got, err)
		}
	}
	for _, path := range []string{"api-1/warm", "api-1/debugger", "plain/app"} {
		if _, err := os.Stat(filepath.Join(pods, filepath.FromSlash(path))); err == nil {
			t.Errorf("%s was written", path)
		}
	}
	// A second time there is nothing left to take but what could not be taken.
	if left, _ := logsLeftOut(snapshot); len(left) != 1 || left[0].Container != "debugger" {
		t.Errorf("left out after taking: %+v", left)
	}

	// A kubectl that will not run the command at all stops the freeze: no other log would fare better.
	calls := 0
	_, _, err = takeLogsLeftOut(context.Background(), snapshot, func(context.Context, leftOut, io.Writer) error {
		calls++
		return fmt.Errorf("%w: it is a guard", errRefused)
	})
	if !errors.Is(err, errRefused) || calls != 1 {
		t.Errorf("a refusal: %v after %d calls", err, calls)
	}
	// A snapshot with no pods in it, and one in a directory whose name is a pattern.
	odd := filepath.Join(t.TempDir(), "case[1]")
	write(filepath.Join(odd, "namespaces", "shop", "v1", "pod", "p.yaml"), "apiVersion: v1\nkind: Pod\nmetadata: {name: p, namespace: shop}\nstatus: {initContainerStatuses: [{name: init, state: {terminated: {}}}]}\n")
	if left, err := logsLeftOut(odd); err != nil || len(left) != 1 || left[0].String() != "shop/p/init" {
		t.Errorf("a snapshot under a directory named like a pattern: %v %v", left, err)
	}
	if left, err := logsLeftOut(t.TempDir()); err != nil || left != nil {
		t.Errorf("a snapshot with no namespaces: %v %v", left, err)
	}
}

// The command that fetches a log, and what stands behind `kubectl` on the path when it is run: the
// cluster's kubeconfig by flag and by environment, the kubelet's times asked for, and a guard's
// refusal told apart from a log that is not there.
func TestALogIsFetchedWithTheKubectlOnThePath(t *testing.T) {
	if got := logArgs("/k/config", leftOut{Namespace: "shop", Pod: "api-1", Container: "migrate", Previous: true}); !reflect.DeepEqual(got,
		[]string{"--kubeconfig", "/k/config", "logs", "api-1", "-n", "shop", "-c", "migrate", "--timestamps", "--previous"}) {
		t.Errorf("the command for a previous log: %q", got)
	}
	if got := logArgs("/k/config", leftOut{Namespace: "shop", Pod: "api-1", Container: "schema"}); got[len(got)-1] != "--timestamps" {
		t.Errorf("the command for a log: %q", got)
	}

	bin := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in\n  *refused*) echo 'kubectl guard: REFUSED' >&2; exit 97 ;;\n  *gone*) echo 'not found' >&2; exit 1 ;;\nesac\necho \"$KUBECONFIG|$*\"\n"
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	fetch, err := kubectlLogs("/k/config")
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := fetch(context.Background(), leftOut{Namespace: "shop", Pod: "api-1", Container: "schema"}, &got); err != nil || got.String() != "/k/config|--kubeconfig /k/config logs api-1 -n shop -c schema --timestamps\n" {
		t.Errorf("a log fetched: %q (%v)", got.String(), err)
	}
	if err := fetch(context.Background(), leftOut{Namespace: "shop", Pod: "gone", Container: "c"}, &got); err == nil || errors.Is(err, errRefused) {
		t.Errorf("a log that is not there: %v", err)
	}
	if err := fetch(context.Background(), leftOut{Namespace: "shop", Pod: "refused", Container: "c"}, &got); !errors.Is(err, errRefused) || !strings.Contains(err.Error(), "guard") {
		t.Errorf("a kubectl that is a served case's guard: %v", err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := kubectlLogs("/k/config"); err == nil {
		t.Error("no kubectl on the path, and no error")
	}
}

// A freeze from end to end, with a collector and a kubectl that are scripts: the collector leaves
// the init container's log out, the freeze fetches it, the case carries it, and what could not be
// fetched is named in what the freeze says of itself.
func TestAFreezeCarriesTheLogsTheCollectorLeavesOut(t *testing.T) {
	bin, out := t.TempDir(), filepath.Join(t.TempDir(), "case")
	collector := `#!/bin/sh
# collect -k <kubeconfig> -f <snapshot> ...
while [ $# -gt 0 ]; do [ "$1" = "-f" ] && snapshot="$2"; shift; done
pods="$snapshot/namespaces/shop/v1/pod"
mkdir -p "$pods/api-1/app"
printf '2026-10-07T09:05:26.000000001Z conn-table active=40/40 CACHE_CONN_MODE\n' > "$pods/api-1/app/current.log"
cat > "$pods/api-1.yaml" <<POD
apiVersion: v1
kind: Pod
metadata: {name: api-1, namespace: shop}
status:
  initContainerStatuses:
    - {name: migrate, state: {terminated: {exitCode: 1}}}
    - {name: lost, state: {terminated: {exitCode: 0}}}
POD
`
	kubectl := "#!/bin/sh\ncase \"$*\" in *' -c lost '*) exit 1 ;; esac\necho '2026-10-07T09:05:20.000000001Z schema is locked'\n"
	for name, script := range map[string]string{"collector": collector, "kubectl": kubectl} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("LAPILLI_CRUST_GATHER", filepath.Join(bin, "collector"))
	caseYAML := filepath.Join(t.TempDir(), "case.yaml")
	if err := os.WriteFile(caseYAML, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	info, _, err := Freeze(context.Background(), Options{CaseYAML: caseYAML, OutDir: out, Kubeconfig: filepath.Join(t.TempDir(), "kubeconfig")})
	if err != nil {
		t.Fatal(err)
	}
	if info.LogsAdded != 1 || !reflect.DeepEqual(info.LogsMissing, []string{"shop/api-1/lost"}) {
		t.Errorf("the freeze says it added %d logs and missed %v", info.LogsAdded, info.LogsMissing)
	}
	written, err := casefile.LoadFreezeInfo(out)
	if err != nil || written.LogsAdded != 1 || len(written.LogsMissing) != 1 {
		t.Errorf("freeze.json: %+v (%v)", written, err)
	}
	files := untar(t, filepath.Join(out, casefile.KubernetesName))
	if got := files["namespaces/shop/v1/pod/api-1/migrate/current.log"]; got != "2026-10-07T09:05:20.000000001Z schema is locked\n" {
		t.Errorf("the init container's log in the case: %q (the case has %v)", got, keys(files))
	}
	if _, there := files["namespaces/shop/v1/pod/api-1/lost/current.log"]; there {
		t.Error("a log that could not be fetched is in the case")
	}
}

// untar reads a case's snapshot archive into memory: path to content.
func untar(t *testing.T, archive string) map[string]string {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for tr := tar.NewReader(zr); ; {
		h, err := tr.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			body, _ := io.ReadAll(tr)
			files[h.Name] = string(body)
		}
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The metrics are read after the cluster and up to the instant that was named before it: a scrape
// under way at that instant has committed by then. And what the Prometheus says of itself goes into
// freeze.json, since the metrics file does not carry it.
func TestAFreezeReadsTheMetricsLastAndUpToItsInstant(t *testing.T) {
	bin, out, mark := t.TempDir(), filepath.Join(t.TempDir(), "case"), filepath.Join(t.TempDir(), "collected")
	collector := `#!/bin/sh
while [ $# -gt 0 ]; do [ "$1" = "-f" ] && snapshot="$2"; shift; done
pods="$snapshot/namespaces/shop/v1/pod"
mkdir -p "$pods/api-1/app"
printf '2026-10-07T09:05:26.000000001Z conn-table active=40/40 CACHE_CONN_MODE\n' > "$pods/api-1/app/current.log"
printf 'apiVersion: v1\nkind: Pod\nmetadata: {name: api-1, namespace: shop}\n' > "$pods/api-1.yaml"
touch "$LAPILLI_TEST_COLLECTED"
`
	if err := os.WriteFile(filepath.Join(bin, "collector"), []byte(collector), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAPILLI_CRUST_GATHER", filepath.Join(bin, "collector"))
	t.Setenv("LAPILLI_TEST_COLLECTED", mark)
	type read struct {
		collected  bool
		from, upTo int64
	}
	var reads []read
	prometheus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/read":
			_, err := os.Stat(mark)
			body, _ := io.ReadAll(r.Body)
			raw, _ := snappy.Decode(nil, body)
			var req prompb.ReadRequest
			req.Unmarshal(raw)
			reads = append(reads, read{err == nil, req.Queries[0].StartTimestampMs, req.Queries[0].EndTimestampMs})
			answer, _ := (&prompb.ReadResponse{Results: []*prompb.QueryResult{{Timeseries: []*prompb.TimeSeries{{
				Labels: []prompb.Label{{Name: "__name__", Value: "up"}}, Samples: []prompb.Sample{{Timestamp: req.Queries[0].EndTimestampMs - 1000, Value: 1}},
			}}}}}).Marshal()
			w.Write(snappy.Encode(nil, answer))
		case "/api/v1/status/buildinfo":
			io.WriteString(w, `{"status":"success","data":{"version":"3.5.0"}}`)
		case "/api/v1/status/config":
			io.WriteString(w, `{"status":"success","data":{"yaml":"global:\n  evaluation_interval: 15s\n"}}`)
		case "/api/v1/status/flags":
			io.WriteString(w, `{"status":"success","data":{"query.lookback-delta":"2m","web.enable-admin-api":"true"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer prometheus.Close()
	caseYAML := filepath.Join(t.TempDir(), "case.yaml")
	if err := os.WriteFile(caseYAML, []byte(spec+"metrics: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, _, err := Freeze(context.Background(), Options{CaseYAML: caseYAML, OutDir: out, Kubeconfig: filepath.Join(t.TempDir(), "kubeconfig"), MetricsURL: prometheus.URL, MetricsWindow: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	// Asked first whether it can be read at all — a read of nothing, before anything is collected — and
	// read last, the ten minutes up to the instant that was named before the collector ran, and before
	// them the two minutes this Prometheus says an instant looks back.
	at := metrics.FromSeconds(info.FreezeTime).UnixMilli()
	collected, err := os.Stat(mark)
	if err != nil {
		t.Fatal(err)
	}
	if len(reads) != 2 || reads[0] != (read{false, at, at}) || reads[1] != (read{true, at - 720_000, at}) {
		t.Errorf("the Prometheus was read %+v; want nothing at %d before the collection and twelve minutes up to it after", reads, at)
	}
	if info.Metrics == nil || info.Metrics.FromMs != at-720_000 {
		t.Errorf("the case says its metrics begin at %+v; they were read from %d", info.Metrics, at-720_000)
	}
	if at > collected.ModTime().UnixMilli() {
		t.Errorf("the freeze is at %d, after the cluster was collected at %d: the instant is named first", at, collected.ModTime().UnixMilli())
	}
	if m := info.Metrics; m == nil || m.PrometheusVersion != "3.5.0" || m.EvaluationIntervalMs != 15000 || m.LookbackDeltaMs != 120000 {
		t.Errorf("of its Prometheus the freeze recorded %+v", info.Metrics)
	}
	// Under the names a replay, and anyone's script, reads them by.
	written, _ := os.ReadFile(filepath.Join(out, casefile.FreezeName))
	for _, key := range []string{`"prometheus_version": "3.5.0"`, `"evaluation_interval_ms": 15000`, `"lookback_delta_ms": 120000`} {
		if !strings.Contains(string(written), key) {
			t.Errorf("freeze.json does not say %s: %s", key, written)
		}
	}
}

// A Prometheus that cannot be read is found out before the cluster is collected, not after.
func TestAFreezeAsksBeforeItCollectsWhetherTheMetricsCanBeRead(t *testing.T) {
	bin, mark := t.TempDir(), filepath.Join(t.TempDir(), "collected")
	if err := os.WriteFile(filepath.Join(bin, "collector"), []byte("#!/bin/sh\ntouch \"$LAPILLI_TEST_COLLECTED\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAPILLI_CRUST_GATHER", filepath.Join(bin, "collector"))
	t.Setenv("LAPILLI_TEST_COLLECTED", mark)
	nothing := httptest.NewServer(http.NotFoundHandler())
	defer nothing.Close()
	caseYAML := filepath.Join(t.TempDir(), "case.yaml")
	os.WriteFile(caseYAML, []byte(spec+"metrics: true\n"), 0o644)
	for what, opt := range map[string]Options{
		"a Prometheus with no remote read": {MetricsURL: nothing.URL},
		"a selector that does not parse":   {MetricsURL: nothing.URL, MetricsSelectors: []string{"up{"}},
	} {
		opt.CaseYAML, opt.OutDir, opt.Kubeconfig = caseYAML, filepath.Join(t.TempDir(), "case"), filepath.Join(t.TempDir(), "kubeconfig")
		if _, _, err := Freeze(context.Background(), opt); err == nil || !strings.Contains(err.Error(), "freezing metrics") {
			t.Errorf("%s: %v", what, err)
		}
		if _, err := os.Stat(mark); err == nil {
			t.Errorf("%s: the cluster was collected before it was found out", what)
		}
	}
}

// A Prometheus that can be read before the cluster is collected and not after — the port-forward to it
// died, it holds a histogram a case cannot carry — does not cost the freeze what was collected: the
// cluster may not be as it was by the next try. What was collected is kept, its Secrets blanked.
func TestAFreezeKeepsWhatItCollectedWhenTheMetricsFailAfterwards(t *testing.T) {
	bin := t.TempDir()
	collector := `#!/bin/sh
while [ $# -gt 0 ]; do [ "$1" = "-f" ] && snapshot="$2"; shift; done
mkdir -p "$snapshot/namespaces/shop/v1/secret" "$snapshot/namespaces/shop/v1/pod"
printf 'apiVersion: v1\nkind: Secret\nmetadata: {name: db, namespace: shop}\ndata: {password: aHVudGVyMg==}\n' > "$snapshot/namespaces/shop/v1/secret/db.yaml"
printf 'apiVersion: v1\nkind: Pod\nmetadata: {name: api-1, namespace: shop}\n' > "$snapshot/namespaces/shop/v1/pod/api-1.yaml"
`
	if err := os.WriteFile(filepath.Join(bin, "collector"), []byte(collector), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAPILLI_CRUST_GATHER", filepath.Join(bin, "collector"))
	reads := 0
	prometheus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reads++; r.URL.Path != "/api/v1/read" || reads > 1 { // the probe is answered, and nothing after it
			http.Error(w, "gone", http.StatusBadGateway)
			return
		}
		answer, _ := (&prompb.ReadResponse{Results: []*prompb.QueryResult{{}}}).Marshal()
		w.Write(snappy.Encode(nil, answer))
	}))
	defer prometheus.Close()
	caseYAML, out := filepath.Join(t.TempDir(), "case.yaml"), filepath.Join(t.TempDir(), "case")
	os.WriteFile(caseYAML, []byte(spec+"metrics: true\n"), 0o644)
	before := time.Now()
	_, _, err := Freeze(context.Background(), Options{CaseYAML: caseYAML, OutDir: out, Kubeconfig: filepath.Join(t.TempDir(), "kubeconfig"), MetricsURL: prometheus.URL})
	if err == nil || !strings.Contains(err.Error(), "freezing metrics") {
		t.Fatalf("a freeze whose metrics could not be read: %v", err)
	}
	said := regexp.MustCompile(`is kept in (\S+) with its Secrets blanked: .pack --snapshot. seals it with --freeze-time ([0-9.]+),`).FindStringSubmatch(err.Error())
	if said == nil {
		t.Fatalf("the freeze does not say where it kept what it collected: %v", err)
	}
	defer os.RemoveAll(filepath.Dir(said[1]))
	secret, readErr := os.ReadFile(filepath.Join(said[1], "namespaces", "shop", "v1", "secret", "db.yaml"))
	if readErr != nil || strings.Contains(string(secret), "aHVudGVyMg") {
		t.Errorf("what was kept: %v, and its Secret reads %q", readErr, secret)
	}
	if at, _ := strconv.ParseFloat(said[2], 64); at < float64(before.UnixMilli())/1000 || at > float64(time.Now().UnixMilli())/1000 {
		t.Errorf("the freeze says it collected at %s, which is not when it ran", said[2])
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("a case was written though the freeze failed")
	}
}

// A case sealed by `pack` from what `export-metrics` left is the case a freeze of that Prometheus
// makes: the export leaves beside its metrics file what it learned besides the samples, and pack
// reads the two. With the metrics file alone, the case says none of it.
func TestACasePackedFromAnExportIsWhatAFreezeMakes(t *testing.T) {
	series := func(name, job string, ts ...int64) *prompb.TimeSeries {
		out := &prompb.TimeSeries{Labels: []prompb.Label{{Name: "__name__", Value: name}, {Name: "cluster", Value: "prod"}, {Name: "job", Value: job}}}
		for _, at := range ts {
			out.Samples = append(out.Samples, prompb.Sample{Timestamp: at, Value: 1})
		}
		return out
	}
	// A Prometheus with blocks that ended at 5s, external labels, a head that made `z` before `a`, and
	// something to say of its metrics.
	read, _ := (&prompb.ReadResponse{Results: []*prompb.QueryResult{{Timeseries: []*prompb.TimeSeries{series("up", "a", 1000, 9000), series("up", "z", 1000, 9000), series("gone", "a", 1000)}}}}).Marshal()
	prometheus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answer := map[string]string{
			"/api/v1/status/buildinfo":   `{"status":"success","data":{"version":"3.15.0"}}`,
			"/api/v1/status/config":      `{"status":"success","data":{"yaml":"global:\n  evaluation_interval: 15s\n  external_labels:\n    cluster: prod\n"}}`,
			"/api/v1/status/flags":       `{"status":"success","data":{"query.lookback-delta":"2m"}}`,
			"/api/v1/status/tsdb/blocks": `{"status":"success","data":{"blocks":[{"ulid":"01","minTime":0,"maxTime":5000}]}}`,
			"/api/v1/metadata":           `{"status":"success","data":{"up":[{"type":"gauge","help":"Whether the target answered.","unit":""}]}}`,
		}[r.URL.Path]
		switch {
		case r.URL.Path == "/api/v1/read":
			w.Write(snappy.Encode(nil, read))
		case r.URL.Path == "/api/v1/series" && r.FormValue("match[]") == `{cluster="prod"}`:
			io.WriteString(w, `{"status":"success","data":[]}`)
		case r.URL.Path == "/api/v1/series":
			io.WriteString(w, `{"status":"success","data":[{"__name__":"up","job":"z"},{"__name__":"up","job":"a"}]}`)
		case answer != "":
			io.WriteString(w, answer)
		default:
			http.NotFound(w, r)
		}
	}))
	defer prometheus.Close()
	dir := t.TempDir()
	with := filepath.Join(dir, "with.yaml")
	os.WriteFile(with, []byte(spec+"metrics: true\n"), 0o644)
	at := time.UnixMilli(10000)
	export := func() *metrics.Store {
		store, err := metrics.Export(context.Background(), prometheus.Client(), prometheus.URL, at, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	sealed := func(out string, store *metrics.Store) (said *casefile.MetricsInfo, described, samples string) {
		info, _, err := Pack(with, snapshot(t), out, 10, store)
		if err != nil {
			t.Fatal(err)
		}
		d, _ := os.ReadFile(filepath.Join(out, casefile.MetadataName))
		m, _ := os.ReadFile(filepath.Join(out, casefile.MetricsName))
		return info.Metrics, string(d), fmt.Sprintf("%x", m)
	}
	// What a freeze makes: the export in hand, packed as it is.
	frozen, frozenDescribed, frozenSamples := sealed(filepath.Join(dir, "frozen"), export())
	if frozen.FromMs != at.Add(-3*time.Minute).UnixMilli() || frozen.HeadFromMs != 5000 || frozen.SeriesOrder != casefile.OrderOfTheHead || frozen.MetadataFamilies != 1 ||
		frozen.LookbackDeltaMs != 120000 || frozen.EvaluationIntervalMs != 15000 || frozen.PrometheusVersion != "3.15.0" || !reflect.DeepEqual(frozen.ExternalLabels, map[string]string{"cluster": "prod"}) ||
		!strings.Contains(frozenDescribed, "Whether the target answered.") {
		t.Fatalf("the stand-in Prometheus is not frozen as this test takes it to be: %+v", frozen)
	}
	// What `export-metrics` leaves and `pack` picks up: the metrics file, and the file beside it.
	file := filepath.Join(dir, "exported.jsonl.gz")
	exported := export()
	if err := exported.Save(file); err != nil {
		t.Fatal(err)
	}
	if err := exported.SaveLearned(file); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadFile(metrics.LearnedPath(file)); !strings.Contains(string(left), `"series_order": "`+casefile.OrderOfTheHead+`"`) { // the word freeze.json has for it
		t.Errorf("what the export left beside its file does not say its order as freeze.json does: %s", left)
	}
	loaded, err := metrics.Load(file)
	if err != nil {
		t.Fatal(err)
	}
	if found, err := loaded.LoadLearned(file); !found || err != nil {
		t.Fatalf("what the export learned was not found beside its file: %v", err)
	}
	packed, packedDescribed, packedSamples := sealed(filepath.Join(dir, "packed"), loaded)
	if !reflect.DeepEqual(packed, frozen) || packedDescribed != frozenDescribed || packedSamples != frozenSamples {
		t.Errorf("a case packed from an export says of its metrics\n%+v\nand a freeze\n%+v\n(what kind each metric is, the same: %v; the samples: %v)", packed, frozen, packedDescribed == frozenDescribed, packedSamples == frozenSamples)
	}
	// And with the metrics file alone: the samples, in the order they were written, and nothing else.
	alone, err := metrics.Load(file)
	if err != nil {
		t.Fatal(err)
	}
	bare, bareDescribed, bareSamples := sealed(filepath.Join(dir, "bare"), alone)
	if bare.FromMs != 0 || bare.HeadFromMs != 0 || bare.SeriesOrder != "" || len(bare.ExternalLabels) != 0 || bare.MetadataFamilies != 0 || bare.PrometheusVersion != "" || bare.LookbackDeltaMs != 0 ||
		bare.Series != frozen.Series || bare.Samples != frozen.Samples || bareDescribed != "" || bareSamples != frozenSamples {
		t.Errorf("a case packed from a metrics file with nothing beside it says %+v, describes its metrics with %q, and has the same samples: %v", bare, bareDescribed, bareSamples == frozenSamples)
	}
	if _, err := os.Stat(filepath.Join(dir, "bare", casefile.MetadataName)); err == nil {
		t.Error("a case packed from a metrics file with nothing beside it describes its metrics")
	}
	// Neither case holds the file that was beside the metrics: what it said is in freeze.json and metrics-metadata.json.
	if left, _ := filepath.Glob(filepath.Join(dir, "packed", "*learned*")); len(left) != 0 {
		t.Errorf("the case holds %v", left)
	}
	// Nor does a case whose metrics were exported into its own directory, under the name a case has
	// them by: the file beside them is read, and is gone before the case is sealed.
	inPlace := filepath.Join(dir, "in-place")
	os.MkdirAll(inPlace, 0o755)
	own := filepath.Join(inPlace, casefile.MetricsName)
	if err := exported.Save(own); err != nil {
		t.Fatal(err)
	}
	if err := exported.SaveLearned(own); err != nil {
		t.Fatal(err)
	}
	here, err := metrics.Load(own)
	if err != nil {
		t.Fatal(err)
	}
	if found, err := here.LoadLearned(own); !found || err != nil {
		t.Fatal(err)
	}
	inside, _, _ := sealed(inPlace, here)
	manifest, _ := os.ReadFile(filepath.Join(inPlace, casefile.ManifestName))
	if _, err := os.Stat(metrics.LearnedPath(own)); err == nil || strings.Contains(string(manifest), "learned") || !reflect.DeepEqual(inside, frozen) || len(manifest) == 0 {
		t.Errorf("a case packed where its metrics were exported: the file beside them is still there (%v), or sealed with it, or the case says %+v", err == nil, inside)
	}
}
