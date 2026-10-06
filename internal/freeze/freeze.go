// Package freeze turns a live incident into a case that can be investigated with no cluster.
//
// Two stores are frozen: the Kubernetes API (objects, events and pod logs, collected by crust-gather)
// and a Prometheus (its raw samples, read over remote read). Secret values never leave the cluster.
// Nothing else is redacted: logs, environment values and ConfigMaps are copied as they are, so a case
// frozen from a real cluster is that cluster's data (docs/design-case.md, "What a case contains").
package freeze

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/lapilli-project/lapilli/internal/casefile"
	"github.com/lapilli-project/lapilli/internal/metrics"
)

// Redacted replaces every Secret value in a snapshot.
const Redacted = "REDACTED-BY-LAPILLI"

// CrustGather is the collector binary. LAPILLI_CRUST_GATHER overrides it.
func CrustGather() string {
	if p := os.Getenv("LAPILLI_CRUST_GATHER"); p != "" {
		return p
	}
	return "kubectl-crust-gather"
}

// RedactSecrets blanks the values of every Secret in a snapshot, keeping the keys, and returns how
// many Secrets it touched. A Secret file that does not parse is removed: unreadable is not provably safe.
func RedactSecrets(snapshot string) (int, error) {
	touched := 0
	err := filepath.WalkDir(snapshot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(filepath.Dir(path)) != "secret" {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var doc map[string]any
		if yaml.Unmarshal(raw, &doc) != nil || doc == nil {
			touched++
			return os.Remove(path)
		}
		for _, field := range []string{"data", "stringData"} {
			if values, ok := doc[field].(map[string]any); ok {
				for k := range values {
					values[k] = Redacted
				}
			}
		}
		if meta, ok := doc["metadata"].(map[string]any); ok {
			if ann, ok := meta["annotations"].(map[string]any); ok {
				delete(ann, "kubectl.kubernetes.io/last-applied-configuration") // carries the values again
			}
		}
		out, err := yaml.Marshal(doc)
		if err != nil {
			return err
		}
		touched++
		return os.WriteFile(path, out, 0o644)
	})
	return touched, err
}

// EvidencePresent reports, per pattern, whether any file of a snapshot contains it. A case whose
// decisive evidence is not in the frozen copy cannot be solved from it, whatever the agent does.
func EvidencePresent(snapshot string, patterns []string) (map[string]bool, error) {
	found, compiled := map[string]bool{}, map[string]*regexp.Regexp{}
	for _, p := range patterns {
		rx, err := regexp.Compile(p)
		if err != nil {
			return nil, err
		}
		found[p], compiled[p] = false, rx
	}
	missing := len(patterns)
	err := filepath.WalkDir(snapshot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if missing == 0 {
			return fs.SkipAll
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil // a file that cannot be read cannot hold evidence an agent could reach either
		}
		for p, rx := range compiled {
			if !found[p] && rx.Match(raw) {
				found[p] = true
				missing--
			}
		}
		return nil
	})
	return found, err
}

// archive writes a directory as a gzip-compressed tar. Entries are sorted and carry no times, owners
// or modes beyond 0644/0755, so one build packs the same tree to the same bytes and the same seal
// wherever and whenever it runs. A different Go release may compress differently; a sealed case is
// identified by the bytes that were sealed, not by being reproducible from its source.
func archive(src, dest string) error {
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	zw, _ := gzip.NewWriterLevel(out, gzip.BestCompression)
	tw := tar.NewWriter(zw)
	var paths []string
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err == nil && path != src {
			paths = append(paths, path)
		}
		return err
	})
	if err != nil {
		return err
	}
	sort.Strings(paths)
	for _, path := range paths {
		st, err := os.Lstat(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		name := filepath.ToSlash(rel)
		switch {
		case st.IsDir():
			err = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o755, Format: tar.FormatPAX})
		case st.Mode().IsRegular():
			if err = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: st.Size(), Format: tar.FormatPAX}); err == nil {
				var f *os.File
				if f, err = os.Open(path); err == nil {
					_, err = io.Copy(tw, f)
					f.Close()
				}
			}
		default:
			err = fmt.Errorf("%s is neither a file nor a directory; a case holds nothing else", path)
		}
		if err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return out.Close()
}

// Pack assembles and seals a case directory from a snapshot that was already collected and, when
// given, a metrics store. The snapshot is redacted in place.
func Pack(caseYAML, snapshot, outDir string, freezeTime float64, store *metrics.Store) (*casefile.FreezeInfo, *casefile.Manifest, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, nil, err
	}
	spec, err := os.ReadFile(caseYAML)
	if err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(filepath.Join(outDir, casefile.SpecName), spec, 0o644); err != nil {
		return nil, nil, err
	}
	c, err := casefile.Load(outDir)
	if err != nil {
		return nil, nil, err
	}
	if c.Metrics != (store != nil) {
		return nil, nil, fmt.Errorf("case.yaml says metrics: %v, but a metrics store was %s", c.Metrics, map[bool]string{true: "given", false: "not given"}[store != nil])
	}
	info := &casefile.FreezeInfo{FreezeTime: freezeTime, FrozenAt: metrics.FromSeconds(freezeTime).UTC().Format(time.RFC3339), Stores: []string{casefile.StoreKubernetes}}
	if info.SecretsRedacted, err = RedactSecrets(snapshot); err != nil {
		return nil, nil, err
	}
	if info.EvidenceInSnapshot, err = EvidencePresent(snapshot, c.Patterns(casefile.StoreKubernetes)); err != nil {
		return nil, nil, err
	}
	if err := archive(snapshot, filepath.Join(outDir, casefile.KubernetesName)); err != nil {
		return nil, nil, err
	}
	if store != nil {
		m := &casefile.MetricsInfo{}
		m.Series, m.Samples = store.Size()
		if m.Series == 0 {
			return nil, nil, errors.New("the metrics store is empty; a case that promises metrics must carry some")
		}
		m.Oldest, m.Newest, _ = store.Bounds()
		info.Stores, info.Metrics = append(info.Stores, casefile.StoreMetrics), m
		if err := store.Save(filepath.Join(outDir, casefile.MetricsName)); err != nil {
			return nil, nil, err
		}
	}
	if err := info.Save(outDir); err != nil {
		return nil, nil, err
	}
	manifest, err := casefile.Seal(outDir)
	return info, manifest, err
}

// Options are what Freeze needs to know about the live incident.
type Options struct {
	CaseYAML   string
	OutDir     string
	Kubeconfig string // the cluster to freeze; always passed explicitly, never taken from the environment
	// MetricsURL is a Prometheus to freeze beside the cluster. Empty means the case has no metrics.
	MetricsURL       string
	MetricsWindow    time.Duration
	MetricsSelectors []string
}

// Freeze collects the cluster behind a kubeconfig and, when asked, the Prometheus beside it, and
// seals the result as a case.
func Freeze(ctx context.Context, opt Options) (*casefile.FreezeInfo, *casefile.Manifest, error) {
	if opt.Kubeconfig == "" {
		return nil, nil, errors.New("a kubeconfig is required: the default one may point anywhere")
	}
	kubeconfig, err := filepath.Abs(opt.Kubeconfig)
	if err != nil {
		return nil, nil, err
	}
	tmp, err := os.MkdirTemp("", "lapilli-freeze-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(tmp)

	at := time.Now()
	freezeTime := float64(at.UnixMilli()) / 1000
	var store *metrics.Store
	if opt.MetricsURL != "" { // metrics first: the collector takes seconds, and the window should end at the freeze
		window := opt.MetricsWindow
		if window <= 0 {
			window = time.Hour
		}
		if store, err = metrics.Export(ctx, nil, opt.MetricsURL, at, window, opt.MetricsSelectors...); err != nil {
			return nil, nil, fmt.Errorf("freezing metrics: %w", err)
		}
	}
	snapshot := filepath.Join(tmp, "kubernetes")
	cmd := exec.CommandContext(ctx, CrustGather(), "collect", "-k", kubeconfig, "-f", snapshot)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, nil, fmt.Errorf("%s collect: %w: %s", CrustGather(), err, strings.TrimSpace(string(out)))
	}
	return Pack(opt.CaseYAML, snapshot, opt.OutDir, freezeTime, store)
}
