package serve

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// What was deployed, and when (docs/design/noc.md): --deploys names a file
// that owns the ledger's deploys, read again each run, as the incidents
// file is.

// deploysMax is how large the deploys file, and each history it names, may
// be.
const deploysMax = 8 << 20

// A history's line, as recipes/versions/argocd.sh and flux.sh write it: a
// commit's hex and when it was deployed, in UTC to the second.
var deployLine = regexp.MustCompile(`^([0-9a-f]{7,64}) ([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z)$`)

// LoadDeploys reads a deploys file:
//
//	deploys:
//	  # What argocd.sh or flux.sh wrote for an Application: each line a
//	  # deploy, of the service and namespace these labels name.
//	  - labels: {service: checkout, namespace: shop}
//	    history: checkout.txt
//	  # One written down: from start to end (start, when not given).
//	  - labels: {namespace: payments}
//	    start: 2026-11-10T10:00:00Z
//	    end: 2026-11-10T10:06:00Z
//	    revision: 4f2a9c1
//
// A history's path is from the file's directory, unless it is absolute.
func LoadDeploys(path string) ([]ledger.Deploy, error) {
	b, err := readBounded(path)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Deploys yaml.Node `yaml:"deploys"`
	}
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// An empty file isn't taken for no deploys: a job that failed leaves
	// one behind, and the file owns what it holds.
	if raw.Deploys.Kind == 0 {
		return nil, fmt.Errorf("%s: no deploys key: want deploys:, with a list under it, an empty one if there are none", path)
	}
	var entries []struct {
		Labels   map[string]string `yaml:"labels"`
		History  string            `yaml:"history"`
		Start    time.Time         `yaml:"start"`
		End      time.Time         `yaml:"end"`
		Revision string            `yaml:"revision"`
	}
	if err := raw.Deploys.Decode(&entries); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var out []ledger.Deploy
	for i, e := range entries {
		switch {
		case e.History != "" && !e.Start.IsZero():
			return nil, fmt.Errorf("%s: deploy %d has a history and a start: give one", path, i+1)
		case e.History == "" && e.Start.IsZero():
			return nil, fmt.Errorf("%s: deploy %d has neither a history nor a start", path, i+1)
		case e.History == "":
			out = append(out, ledger.Deploy{Start: e.Start, End: e.End, Labels: e.Labels, Revision: e.Revision})
			continue
		}
		h := e.History
		if !filepath.IsAbs(h) {
			h = filepath.Join(filepath.Dir(path), h)
		}
		ds, err := readHistory(h, e.Labels)
		if err != nil {
			return nil, fmt.Errorf("%s: deploy %d: %w", path, i+1, err)
		}
		out = append(out, ds...)
		if len(out) > ledger.MaxDeploys {
			break // refused whole, below
		}
	}
	if len(out) > ledger.MaxDeploys {
		return nil, fmt.Errorf("%s: over %d deploys: keep fewer in each history (DEPLOYS_KEEP), or fewer histories", path, ledger.MaxDeploys)
	}
	return out, nil
}

// readHistory reads the deploys a recipe wrote, each a deploy of what the
// labels name, at an instant.
func readHistory(path string, labels map[string]string) ([]ledger.Deploy, error) {
	b, err := readBounded(path)
	if err != nil {
		return nil, err
	}
	var out []ledger.Deploy
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		m := deployLine.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("%s:%d: want REVISION TIME, as recipes/versions/argocd.sh and flux.sh write them", path, n)
		}
		at, err := time.Parse(time.RFC3339, m[2])
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		out = append(out, ledger.Deploy{Start: at, End: at, Labels: labels, Revision: m[1], Source: filepath.Base(path)})
		if len(out) > ledger.MaxDeploys {
			break
		}
	}
	return out, sc.Err()
}

func readBounded(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, deploysMax+1))
	if err == nil && len(b) > deploysMax {
		err = fmt.Errorf("%s: over %d MB", path, deploysMax>>20)
	}
	return b, err
}

// syncDeploys reads the deploys file again, and keeps what it holds.
func (s *Server) syncDeploys(ctx context.Context, o verdict.Options) error {
	ds, err := LoadDeploys(s.cfg.Deploys)
	if err != nil {
		return err
	}
	changed, err := s.l.SyncDeploys(ctx, ds, o)
	if changed {
		s.log.Info("deploys file read again", "deploys", len(ds))
	}
	return err
}
