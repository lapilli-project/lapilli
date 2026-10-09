package casefile

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
  - {pattern: 'client\W{1,4}catalog-indexer', store: metrics}
metrics: true
`

func writeCase(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, SpecName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadReadsEvidenceWithAndWithoutAStore(t *testing.T) {
	c, err := Load(writeCase(t, spec))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Patterns(""); len(got) != 2 {
		t.Fatalf("all patterns = %v", got)
	}
	if got := c.Patterns(StoreKubernetes); !reflect.DeepEqual(got, []string{"conn-table active=40/40"}) {
		t.Fatalf("kubernetes patterns = %v", got)
	}
	if got := c.Patterns(StoreMetrics); !reflect.DeepEqual(got, []string{`client\W{1,4}catalog-indexer`}) {
		t.Fatalf("metrics patterns = %v", got)
	}
}

func TestLoadRejectsABrokenCase(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"missing fields":               {"id: x\nprompt: y\n", "missing required field(s): decoys, evidence, expected, must_not, specificity"},
		"bad pattern":                  {strings.Replace(spec, "'conn-table active=40/40'", "'(unclosed'", 1), "evidence pattern"},
		"unknown store":                {strings.Replace(spec, "store: metrics", "store: tape", 1), `unknown evidence store "tape"`},
		"query on a kubernetes item":   {strings.Replace(spec, "store: metrics", "store: kubernetes, query: up", 1), "names a query"},
		"metrics evidence, no metrics": {strings.Replace(spec, "metrics: true", "metrics: false", 1), "metrics: false"},
	} {
		if _, err := Load(writeCase(t, tc.body)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error containing %q", name, err, tc.want)
		}
	}
}

func TestSealDetectsAlterationAdditionAndRemoval(t *testing.T) {
	dir := writeCase(t, spec)
	if problems, _ := Verify(dir); !reflect.DeepEqual(problems, []string{ErrNotSealed.Error()}) {
		t.Fatalf("unsealed case reported %v", problems)
	}
	if _, err := Seal(dir); err != nil {
		t.Fatal(err)
	}
	if problems, err := Verify(dir); err != nil || len(problems) != 0 {
		t.Fatalf("freshly sealed case: %v %v", problems, err)
	}

	specPath := filepath.Join(dir, SpecName)
	original, _ := os.ReadFile(specPath)
	os.WriteFile(specPath, append(original, []byte("\n# edited\n")...), 0o644)
	if problems, _ := Verify(dir); !reflect.DeepEqual(problems, []string{"altered: case.yaml"}) {
		t.Fatalf("after an edit: %v", problems)
	}
	os.WriteFile(specPath, original, 0o644)

	os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("x"), 0o644)
	if problems, _ := Verify(dir); !reflect.DeepEqual(problems, []string{"not in manifest: extra.txt"}) {
		t.Fatalf("after an addition: %v", problems)
	}
	os.Remove(filepath.Join(dir, "extra.txt"))

	os.Remove(specPath)
	if problems, _ := Verify(dir); !reflect.DeepEqual(problems, []string{"missing: case.yaml"}) {
		t.Fatalf("after a removal: %v", problems)
	}
}

func TestManifestDigestIsTheSha256sumOfTheFileList(t *testing.T) {
	// The digest must be recomputable with a shell: `sha256sum` lines, sorted by path.
	want := sha256.Sum256([]byte("11  a\n22  b\n"))
	if got := listDigest(map[string]string{"b": "22", "a": "11"}); got != hex.EncodeToString(want[:]) {
		t.Fatalf("digest = %s", got)
	}
}

// An evidence item of the Kubernetes store can name the kubectl command that prints it: one command,
// read as a shell reads a simple one, and nothing a shell would do besides.
func TestAnEvidenceItemNamesTheCommandThatReachesIt(t *testing.T) {
	with := func(command string) string {
		return strings.Replace(spec, "  - 'conn-table active=40/40'\n", "  - {pattern: 'conn-table active=40/40', command: "+command+"}\n", 1)
	}
	c, err := Load(writeCase(t, with(`'kubectl -n shop logs deploy/cache'`)))
	if err != nil {
		t.Fatal(err)
	}
	if e := c.Evidence[0]; e.Store != StoreKubernetes || e.Command != "kubectl -n shop logs deploy/cache" || c.Evidence[1].Command != "" {
		t.Errorf("the item was read as %+v", e)
	}
	for command, want := range map[string][]string{
		`kubectl -n shop logs deploy/cache`:                        {"-n", "shop", "logs", "deploy/cache"},
		`  kubectl   get	pods  -A  `:                               {"get", "pods", "-A"},
		`kubectl get pods -l 'app=a b' -o "jsonpath={.items}"`:     {"get", "pods", "-l", "app=a b", "-o", "jsonpath={.items}"},
		`kubectl get pods -o jsonpath='{.items[*].metadata.name}'`: {"get", "pods", "-o", "jsonpath={.items[*].metadata.name}"},
		`kubectl get cm a\ b -o yaml`:                              {"get", "cm", "a b", "-o", "yaml"},
		`kubectl get cm 'a\b' "c\\d"`:                              {"get", "cm", `a\b`, `c\d`},
		`kubectl get cm "say \"hi\"" ''`:                           {"get", "cm", `say "hi"`, ""},
		`kubectl logs x --since-time='2026-10-05T18:57:12Z'`:       {"logs", "x", "--since-time=2026-10-05T18:57:12Z"},
	} {
		if got, err := (Evidence{Command: command}).KubectlArgs(); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: read as %q (%v), want %q", command, got, err, want)
		}
	}
	for command, want := range map[string]string{
		`kubectl logs x | grep conn`:      `holds "|"`,
		`kubectl logs x > out`:            `holds ">"`,
		`kubectl logs x; kubectl get po`:  `holds ";"`,
		`kubectl logs x && true`:          `holds "&"`,
		`kubectl logs $POD`:               `holds "$"`,
		`kubectl logs "$POD"`:             `holds "$"`,
		"kubectl logs `pod`":              "holds \"`\"",
		`kubectl logs $(pod)`:             `holds "$"`,
		`kubectl get pods -o jsonpath={}`: `holds "{"`,
		`kubectl get pods *`:              `holds "*"`,
		"kubectl logs x\nkubectl get po":  `holds "\n"`,
		`kubectl logs 'x`:                 "ends inside a quote",
		`kubectl logs x\`:                 "ends inside a quote",
		`kubectl`:                         "nothing asked of it",
		`promq up`:                        "is not a kubectl command",
		`sh -c 'kubectl get pods'`:        "is not a kubectl command",
		`'kubectl get' pods`:              "is not a kubectl command",
	} {
		if got, err := (Evidence{Command: command}).KubectlArgs(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: read as %q (%v), want it refused as one that %s", command, got, err, want)
		}
	}
	// A case whose command is none is a broken case; and a command is of the Kubernetes store, as a query is of the metrics.
	for name, tc := range map[string]struct{ body, want string }{
		"a command with a pipe":        {with(`'kubectl -n shop logs deploy/cache | grep conn'`), "which only a shell reads"},
		"a command that is no kubectl": {with(`'cat /etc/passwd'`), "is not a kubectl command"},
		"a command on a metrics item":  {strings.Replace(spec, "store: metrics}", "store: metrics, command: 'kubectl get pods'}", 1), "names a command, which only evidence in the kubernetes store has"},
	} {
		if _, err := Load(writeCase(t, tc.body)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error containing %q", name, err, tc.want)
		}
	}
}
