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
	if got := read(t, filepath.Join(dir, "namespaces/shop/configmap/app.yaml")); !strings.Contains(got, "per-task") {
		t.Error("a ConfigMap was redacted; only Secrets are")
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
	if _, found := info.EvidenceInSnapshot["client=indexer"]; found {
		t.Error("evidence that lives in the metrics store was looked for in the Kubernetes snapshot")
	}
	back, err := metrics.Load(filepath.Join(dir, "d", casefile.MetricsName))
	var a, b bytes.Buffer
	if store.Write(&a); err != nil || back.Write(&b) != nil || !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Errorf("the packed store differs from the one given (%v)", err)
	}
}

func TestFreezeRefusesTheDefaultKubeconfig(t *testing.T) {
	if _, _, err := Freeze(t.Context(), Options{CaseYAML: "case.yaml", OutDir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "kubeconfig is required") {
		t.Errorf("Freeze without a kubeconfig gave %v", err)
	}
}
