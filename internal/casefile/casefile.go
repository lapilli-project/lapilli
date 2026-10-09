// Package casefile is the case: a frozen incident together with the answer key that was written
// before any agent saw it. The format is described in docs/case-format.md.
package casefile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Stores a piece of decisive evidence can live in.
const (
	StoreKubernetes = "kubernetes"
	StoreMetrics    = "metrics"
)

// OrderOfTheHead is what freeze.json says of a metrics file whose lines are in the order the
// Prometheus's head had its series.
const OrderOfTheHead = "head"

// File names inside a case directory.
const (
	SpecName       = "case.yaml"
	KubernetesName = "kubernetes.tar.gz"
	MetricsName    = "metrics.jsonl.gz"
	MetadataName   = "metrics-metadata.json"
	FreezeName     = "freeze.json"
)

// Evidence is one decisive item an investigation has to have retrieved. In YAML it is either a bare
// regular expression (the Kubernetes store is assumed) or a mapping that names its store.
type Evidence struct {
	Pattern string `yaml:"pattern"`
	Store   string `yaml:"store"`
	// Query is for evidence in the metrics store: a PromQL query whose `promq` output the pattern
	// must match at the freeze. It is the witness that the evidence can be reached at all. Without
	// one the pattern is looked for in the output of every series.
	Query string `yaml:"query"`
	// Command is for evidence in the Kubernetes store: one `kubectl` command, as an agent types it,
	// whose output the pattern must match in the served case (`lapilli-case reach`). It is the witness
	// that a tool reaches the evidence: finding the pattern in the snapshot's files says that it is
	// there, and not that anything an agent can run prints it.
	Command string `yaml:"command"`
}

// KubectlArgs is what a command passes to kubectl: the words after `kubectl`, read as a shell reads
// one simple command — spaces part words, quotes keep them together, a backslash takes the next
// character as it is — and nothing a shell would do besides. A pipe, a redirection, a variable, a
// second command are refused: what reaches the evidence has to be the one command, since a `grep`
// after it would find the pattern in anything.
func (e Evidence) KubectlArgs() ([]string, error) {
	var words []string
	var word strings.Builder
	in, quote, escaped := false, rune(0), false
	for _, r := range e.Command {
		switch {
		case escaped:
			word.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped, in = true, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else if quote == '"' && (r == '$' || r == '`') {
				return nil, fmt.Errorf("the command %q holds %q, which only a shell reads: one kubectl command is expected", e.Command, string(r))
			} else {
				word.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t':
			if in {
				words = append(words, word.String())
				word.Reset()
				in = false
			}
		case strings.ContainsRune("|&;<>()$`\n\r#*?~{}[]!", r):
			return nil, fmt.Errorf("the command %q holds %q, which only a shell reads: one kubectl command is expected", e.Command, string(r))
		default:
			word.WriteRune(r)
			in = true
		}
	}
	if quote != 0 || escaped {
		return nil, fmt.Errorf("the command %q ends inside a quote", e.Command)
	}
	if in {
		words = append(words, word.String())
	}
	if len(words) == 0 || words[0] != "kubectl" {
		return nil, fmt.Errorf("the command %q is not a kubectl command: it has to begin with `kubectl`", e.Command)
	}
	if len(words) == 1 {
		return nil, fmt.Errorf("the command %q is kubectl with nothing asked of it", e.Command)
	}
	return words[1:], nil
}

func (e *Evidence) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		e.Pattern, e.Store = n.Value, StoreKubernetes
		return nil
	}
	type plain Evidence
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	if p.Store == "" {
		p.Store = StoreKubernetes
	}
	*e = Evidence(p)
	return nil
}

// Case is the answer key. Every field but Title, Difficulty and Metrics is required: a case with no
// narrowing fact and no decoy is a lookup, and a lookup separates nobody.
type Case struct {
	ID          string     `yaml:"id"`
	Title       string     `yaml:"title"`
	Difficulty  string     `yaml:"difficulty"`
	Prompt      string     `yaml:"prompt"`
	Expected    []string   `yaml:"expected"`    // statements a correct answer conveys
	MustNot     []string   `yaml:"must_not"`    // what makes an answer wrong: a decoy named as the cause
	Specificity string     `yaml:"specificity"` // the one population the problem is confined to
	Decoys      []string   `yaml:"decoys"`      // plausible wrong causes planted in the scene
	Evidence    []Evidence `yaml:"evidence"`
	Metrics     bool       `yaml:"metrics"` // the case carries a metrics store

	Dir string `yaml:"-"` // the directory the case was loaded from
}

// Load reads a case from a directory or from the path of its case.yaml.
func Load(path string) (*Case, error) {
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		path = filepath.Join(path, SpecName)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Case
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.Dir, err = filepath.Abs(filepath.Dir(path))
	return &c, err
}

func (c *Case) validate() error {
	var missing []string
	for name, empty := range map[string]bool{
		"id": c.ID == "", "prompt": c.Prompt == "", "expected": len(c.Expected) == 0, "must_not": len(c.MustNot) == 0,
		"specificity": c.Specificity == "", "decoys": len(c.Decoys) == 0, "evidence": len(c.Evidence) == 0,
	} {
		if empty {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("missing required field(s): %s", strings.Join(missing, ", "))
	}
	for _, e := range c.Evidence {
		// An invalid pattern or an unknown store is a broken case, not a failed run.
		if _, err := regexp.Compile(e.Pattern); err != nil {
			return fmt.Errorf("evidence pattern %q: %w", e.Pattern, err)
		}
		if e.Store != StoreKubernetes && e.Store != StoreMetrics {
			return fmt.Errorf("unknown evidence store %q; known: %s, %s", e.Store, StoreKubernetes, StoreMetrics)
		}
		if e.Query != "" && e.Store != StoreMetrics {
			return fmt.Errorf("evidence %q names a query, which only evidence in the %s store has", e.Pattern, StoreMetrics)
		}
		if e.Store == StoreMetrics && !c.Metrics {
			return fmt.Errorf("evidence %q lives in the %s store, and the case says metrics: false", e.Pattern, StoreMetrics)
		}
		if e.Command != "" {
			if e.Store != StoreKubernetes {
				return fmt.Errorf("evidence %q names a command, which only evidence in the %s store has", e.Pattern, StoreKubernetes)
			}
			if _, err := e.KubectlArgs(); err != nil {
				return fmt.Errorf("evidence %q: %w", e.Pattern, err)
			}
		}
	}
	return nil
}

// Patterns returns the evidence patterns that live in one store, or all of them for "".
func (c *Case) Patterns(store string) []string {
	var out []string
	for _, e := range c.Evidence {
		if store == "" || e.Store == store {
			out = append(out, e.Pattern)
		}
	}
	return out
}

// ErrNotSealed is what Verify reports first for a directory that carries no manifest.
var ErrNotSealed = errors.New("no " + ManifestName + ": this case was never sealed")
