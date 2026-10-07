package freeze

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/lapilli-project/lapilli/internal/casefile"
	"github.com/lapilli-project/lapilli/internal/metrics"
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
