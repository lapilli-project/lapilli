package casefile

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
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
	plain := map[string][]string{
		`kubectl -n shop logs deploy/cache`:                        {"-n", "shop", "logs", "deploy/cache"},
		"  kubectl   get\tpods  -A  ":                              {"get", "pods", "-A"},
		"kubectl get pods -A\n":                                    {"get", "pods", "-A"}, // as a block of YAML ends
		`kubectl get pods -l 'app=a b' -o "jsonpath={.items}"`:     {"get", "pods", "-l", "app=a b", "-o", "jsonpath={.items}"},
		`kubectl get pods -o jsonpath='{.items[*].metadata.name}'`: {"get", "pods", "-o", "jsonpath={.items[*].metadata.name}"},
		`kubectl get pods -l 'app in (a,b)' -- x`:                  {"get", "pods", "-l", "app in (a,b)", "--", "x"},
		`kubectl get pods --field-selector=status.phase!=Running`:  {"get", "pods", "--field-selector=status.phase!=Running"},
		`kubectl get pods -l app=a,tier!=b`:                        {"get", "pods", "-l", "app=a,tier!=b"},
		`kubectl get cm a\ b -o yaml`:                              {"get", "cm", "a b", "-o", "yaml"},
		`kubectl get cm \a a#b a~b`:                                {"get", "cm", "a", "a#b", "a~b"},
		`kubectl get cm 'a\b' "c\\d" '$x' 'a|b'`:                   {"get", "cm", `a\b`, `c\d`, "$x", "a|b"},
		`kubectl get cm "say \"hi\"" ''`:                           {"get", "cm", `say "hi"`, ""},
		`kubectl logs x --since-time='2026-10-05T18:57:12Z'`:       {"logs", "x", "--since-time=2026-10-05T18:57:12Z"},
		// In double quotes a backslash is one, but before a quote, a backslash, a dollar or a backquote.
		`kubectl get pods -o "custom-columns=N:.metadata.labels.app\.kubernetes\.io/name"`: {"get", "pods", "-o", `custom-columns=N:.metadata.labels.app\.kubernetes\.io/name`},
		`kubectl get cm "a\$b" "18\:57"`: {"get", "cm", "a$b", `18\:57`},
		// A backslash at the end of a line joins the next line to it, in quotes and out of them.
		"kubectl get pods \\\n  -n shop": {"get", "pods", "-n", "shop"},
		"kubectl get cm \"a\\\nb\" 온도":   {"get", "cm", "ab", "온도"},
	}
	for command, want := range plain {
		got, err := (Evidence{Command: command}).KubectlArgs()
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%q: read as %q (%v), want %q", command, got, err, want)
		}
		// And it is what a shell makes of it: the words `set --` is given, which runs nothing.
		read, shErr := exec.Command("/bin/sh", "-c", "set -- "+command+"\n"+`for a in "$@"; do printf '%s\000' "$a"; done`).Output()
		if words := strings.Split(strings.TrimSuffix(string(read), "\x00"), "\x00"); shErr != nil || !reflect.DeepEqual(words, append([]string{"kubectl"}, want...)) {
			t.Errorf("%q: /bin/sh reads it as %q (%v), and this as %q", command, words, shErr, want)
		}
	}
	refused := map[string]string{
		`kubectl logs x; kubectl get po`: `holds ";"`,
		`kubectl logs "$POD"`:            `holds "$"`,
		"kubectl logs \"`pod`\"":         "holds \"`\"",
		"kubectl logs x\nkubectl get po": `holds "\n"`,
		`kubectl get pods #all`:          `holds "#" outside quotes`,
		`kubectl get pods ~`:             `holds "~" outside quotes`,
		"kubectl get pods a\x00b":        "holds a control character",
		"kubectl get pods a\x01b":        "holds a control character",
		"kubectl get pods a\x7fb":        "holds a control character",
		"kubectl get\rpods":              "holds a control character",
		`kubectl logs 'x`:                "ends inside a quote",
		`kubectl logs "x`:                "ends inside a quote",
		`kubectl logs x\`:                "ends inside a quote",
		`kubectl`:                        "nothing asked of it",
		``:                               "is not a kubectl command",
		"  \t ":                          "is not a kubectl command",
		`promq up`:                       "is not a kubectl command",
		`sh -c 'kubectl get pods'`:       "is not a kubectl command",
		`'kubectl get' pods`:             "is not a kubectl command",
	}
	for _, r := range "|&;<>()$`" { // each of what only a shell reads, in a word of its own and inside one
		refused["kubectl logs x "+string(r)+" y"], refused["kubectl logs x"+string(r)+"y"] = `holds "`+string(r)+`", which only a shell reads`, `holds "`+string(r)+`", which only a shell reads`
	}
	for _, r := range "*?[]{}" { // and of what a shell may read as something else
		refused["kubectl get pods a"+string(r)+"b"] = `holds "` + string(r) + `" outside quotes`
	}
	for command, want := range refused {
		if got, err := (Evidence{Command: command}).KubectlArgs(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: read as %q (%v), want it refused as one that %s", command, got, err, want)
		}
	}
	// A case whose command is none is a broken case; and a command is of the Kubernetes store, as a query is of the metrics.
	for name, tc := range map[string]struct{ body, want string }{
		"a command with a pipe":        {with(`'kubectl -n shop logs deploy/cache | grep conn'`), "which only a shell reads"},
		"a command that is no kubectl": {with(`'cat /etc/passwd'`), "is not a kubectl command"},
		"a command on a metrics item":  {strings.Replace(spec, "store: metrics}", "store: metrics, command: 'kubectl get pods'}", 1), "names a command, which only evidence in the kubernetes store has"},
		// What an item says of itself has to be something: a pattern, and one that the empty text does not satisfy.
		"an item with no pattern":            {strings.Replace(spec, "  - 'conn-table active=40/40'\n", "  - {command: 'kubectl -n shop logs deploy/cache'}\n", 1), `evidence pattern "" is found in the empty text`},
		"a pattern the empty text satisfies": {with(`'kubectl get pods'`)[:0] + strings.Replace(spec, "'conn-table active=40/40'", "'(conn-table)?'", 1), "is found in the empty text, and so in anything"},
		"a name misspelt":                    {strings.Replace(spec, "{pattern: 'client", "{patern: 'client", 1), `an evidence item has no "patern"`},
		"a witness's name misspelt":          {with(`'kubectl get pods'`)[:0] + strings.Replace(spec, "  - 'conn-table active=40/40'\n", "  - {pattern: 'conn-table', comand: 'kubectl get pods'}\n", 1), `an evidence item has no "comand"`},
		"an item that is a list":             {strings.Replace(spec, "  - 'conn-table active=40/40'\n", "  - ['conn-table']\n", 1), "an evidence item is a pattern, or a mapping with one"},
		// A witness that holds its own pattern prints it whatever the case holds.
		"a query that writes what it looks for": {strings.Replace(spec, "store: metrics}", `store: metrics, query: 'label_replace(vector(7), "client", "catalog-indexer", "", "")'}`, 1), "is found in its own witness"},
		"a command whose template is the text":  {strings.Replace(spec, "  - 'conn-table active=40/40'\n", "  - {pattern: 'CACHE_CONN_MODE', command: \"kubectl get ns -o jsonpath='CACHE_CONN_MODE'\"}\n", 1), "is found in its own witness"},
	} {
		if _, err := Load(writeCase(t, tc.body)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error containing %q", name, err, tc.want)
		}
	}
}
