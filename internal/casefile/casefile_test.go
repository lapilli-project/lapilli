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
