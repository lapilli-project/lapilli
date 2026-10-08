// Package freeze turns a live incident into a case that can be investigated with no cluster.
//
// Two stores are frozen: the Kubernetes API (objects, events and pod logs, collected by crust-gather)
// and a Prometheus (its raw samples, read over remote read). Freezing reads and changes nothing, unless
// node logs are asked for (Options.NodeLogs).
//
// Secret values do not enter the case: the collector writes them to a temporary directory on the
// machine that freezes, and they are blanked there before anything is packed. Nothing else is redacted
// — logs, environment values and ConfigMaps are copied as they are — so a case frozen from a real
// cluster is that cluster's data (docs/design-case.md, "What a case contains").
package freeze

import (
	"archive/tar"
	"bytes"
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
// many Secrets it touched.
//
// A Secret is recognised by what a file contains, not by where it lies: any YAML or JSON document of
// kind Secret, alone, in a multi-document file, or as an item of a list. crust-gather happens to keep
// each in a directory named `secret`, and `pack` accepts a snapshot from anywhere. One thing does go
// by place: a file in a `secret` directory that cannot be parsed is removed, because unreadable is
// not provably safe.
func RedactSecrets(snapshot string) (int, error) {
	touched := 0
	err := filepath.WalkDir(snapshot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		inSecretDir := filepath.Base(filepath.Dir(path)) == "secret"
		switch strings.ToLower(filepath.Ext(path)) {
		case ".yaml", ".yml", ".json":
		default:
			if !inSecretDir {
				return nil // logs and the like: not objects
			}
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var docs []any
		dec, found, broken := yaml.NewDecoder(bytes.NewReader(raw)), 0, false
		for {
			var doc any
			if err := dec.Decode(&doc); err == io.EOF {
				break
			} else if err != nil {
				broken = true
				break
			}
			found += redact(doc)
			docs = append(docs, doc)
		}
		switch {
		case broken && inSecretDir:
			touched++
			return os.Remove(path)
		case broken || found == 0:
			return nil // not an object file, or one with no Secret in it: left exactly as it was
		}
		var out bytes.Buffer
		enc := yaml.NewEncoder(&out)
		for _, doc := range docs {
			if err := enc.Encode(doc); err != nil {
				return err
			}
		}
		if err := enc.Close(); err != nil {
			return err
		}
		touched += found
		return os.WriteFile(path, out.Bytes(), 0o644)
	})
	return touched, err
}

// redact blanks every Secret in a decoded document — the document itself, or the items of a list —
// and returns how many it found.
func redact(doc any) int {
	obj, ok := doc.(map[string]any)
	if !ok {
		return 0
	}
	kind, _ := obj["kind"].(string)
	if kind == "Secret" {
		for _, field := range []string{"data", "stringData"} {
			if values, ok := obj[field].(map[string]any); ok {
				for k := range values {
					values[k] = Redacted
				}
			}
		}
		if meta, ok := obj["metadata"].(map[string]any); ok {
			if ann, ok := meta["annotations"].(map[string]any); ok {
				delete(ann, "kubectl.kubernetes.io/last-applied-configuration") // carries the values again
			}
		}
		return 1
	}
	n := 0
	if items, ok := obj["items"].([]any); ok {
		for _, item := range items {
			if m, ok := item.(map[string]any); ok && kind == "SecretList" {
				if _, has := m["kind"]; !has {
					m["kind"] = "Secret" // items of a typed list do not repeat their kind
				}
			}
			n += redact(item)
		}
	}
	return n
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

// MetricsEvidencePresent reports, per metrics-store evidence item of a case, whether its pattern
// matches what `promq` prints at the freeze — for the item's own query, or for every series when it
// names none. A query that does not evaluate is an error: the answer key is broken, not the store.
func MetricsEvidencePresent(c *casefile.Case, store *metrics.Store, freezeTime float64) (map[string]bool, error) {
	found := map[string]bool{}
	api := metrics.NewAPI(store, metrics.FromSeconds(freezeTime))
	for _, e := range c.Evidence {
		if e.Store != casefile.StoreMetrics {
			continue
		}
		query := e.Query
		if query == "" {
			query = metrics.AllSeries
		}
		text, err := api.Text(context.Background(), query)
		if err != nil {
			return nil, fmt.Errorf("evidence %q: query %q: %w", e.Pattern, query, err)
		}
		rx, err := regexp.Compile(e.Pattern)
		if err != nil {
			return nil, err
		}
		found[e.Pattern] = rx.MatchString(text)
	}
	return found, nil
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
	return pack(caseYAML, snapshot, outDir, freezeTime, store, nil)
}

// pack is Pack, with what only a freeze knows about the snapshot it collected.
func pack(caseYAML, snapshot, outDir string, freezeTime float64, store *metrics.Store, collected func(*casefile.FreezeInfo)) (*casefile.FreezeInfo, *casefile.Manifest, error) {
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
	if collected != nil {
		collected(info)
	}
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
		m.PrometheusVersion, m.EvaluationIntervalMs, m.LookbackDeltaMs = store.Source.Version, store.Source.EvaluationInterval.Milliseconds(), store.Source.LookbackDelta.Milliseconds()
		info.Stores, info.Metrics = append(info.Stores, casefile.StoreMetrics), m
		inMetrics, err := MetricsEvidencePresent(c, store, freezeTime)
		if err != nil {
			return nil, nil, err
		}
		for pattern, found := range inMetrics {
			info.EvidenceInSnapshot[pattern] = found
		}
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
	// NodeLogs also collects each node's kubelet journal. crust-gather reads it by starting a pod on
	// every node with the host's process namespace and root filesystem, which is neither read-only nor
	// invisible: the pods and their events end up in the cluster and in the case. Off unless asked for.
	NodeLogs bool
}

// collectArgs is the collector's command line.
func collectArgs(kubeconfig, snapshot string, nodeLogs bool) []string {
	args := []string{"collect", "-k", kubeconfig, "-f", snapshot}
	if !nodeLogs {
		args = append(args, "--disable-additional-logs")
	}
	return args
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
	kept := false // what was collected is thrown away with the rest, unless it is all there is to show for the freeze
	defer func() {
		if !kept {
			os.RemoveAll(tmp)
		}
	}()

	at := time.Now()
	freezeTime := float64(at.UnixMilli()) / 1000
	if opt.MetricsURL != "" { // read last, below; asked now whether it can be read at all
		if err := metrics.Probe(ctx, nil, opt.MetricsURL, at, opt.MetricsSelectors...); err != nil {
			return nil, nil, fmt.Errorf("freezing metrics: %w", err)
		}
	}
	snapshot := filepath.Join(tmp, "kubernetes")
	cmd := exec.CommandContext(ctx, CrustGather(), collectArgs(kubeconfig, snapshot, opt.NodeLogs)...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, nil, fmt.Errorf("%s collect: %w: %s", CrustGather(), err, strings.TrimSpace(string(out)))
	}
	// The logs of init containers, which the collector leaves out (logs.go). kubectl is needed only
	// if there are any.
	var added int
	var missing []string
	if left, err := logsLeftOut(snapshot); err != nil {
		return nil, nil, err
	} else if len(left) > 0 {
		fetch, err := kubectlLogs(kubeconfig)
		if err != nil {
			return nil, nil, err
		}
		if added, missing, err = takeLogsLeftOut(ctx, snapshot, fetch); err != nil {
			return nil, nil, err
		}
	}
	// The metrics last, and up to the instant named first. A scrape that was under way at that instant
	// stamps its samples before it and commits them after: read at once, they are not there yet, and
	// the Prometheus, asked a moment later about that same instant, has a sample the case lacks. The
	// collector has taken its seconds by now, and the window ends where it would have.
	var store *metrics.Store
	if opt.MetricsURL != "" {
		window := opt.MetricsWindow
		if window <= 0 {
			window = time.Hour
		}
		if store, err = metrics.Export(ctx, nil, opt.MetricsURL, at, window, opt.MetricsSelectors...); err != nil {
			// The cluster has been collected, and may not be as it was by the time anyone tries again:
			// what was collected is kept — its Secrets blanked, as a case's are — and said where, with
			// the instant it was collected at.
			if _, blanking := RedactSecrets(snapshot); blanking != nil {
				return nil, nil, fmt.Errorf("freezing metrics: %w", err)
			}
			kept = true
			return nil, nil, fmt.Errorf("freezing metrics: %w\nthe cluster was collected, and is kept in %s with its Secrets blanked: `pack --snapshot` seals it with --freeze-time %.3f, without its metrics or with a file written by `export-metrics --at %.3f`",
				err, snapshot, freezeTime, freezeTime)
		}
	}
	return pack(opt.CaseYAML, snapshot, opt.OutDir, freezeTime, store, func(info *casefile.FreezeInfo) { info.LogsAdded, info.LogsMissing = added, missing })
}
