package freeze

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
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
