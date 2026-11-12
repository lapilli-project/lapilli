package serve

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
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

// deploysFile is the deploys file as read: its entries, the histories they
// name, and a digest of all of it, which an unchanged file is known by.
type deploysFile struct {
	path    string
	entries []deployEntry
	// histories are each history's bytes, by the entry's index.
	histories map[int][]byte
	sum       string
}

type deployEntry struct {
	Labels   map[string]string `yaml:"labels"`
	History  string            `yaml:"history"`
	Start    time.Time         `yaml:"start"`
	End      time.Time         `yaml:"end"`
	Revision string            `yaml:"revision"`
}

// readDeploys reads a deploys file and the histories it names, without
// reading the histories' lines:
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
// A history is a regular file in the deploys file's directory, or below
// it, named by a relative path.
func readDeploys(path string) (*deploysFile, error) {
	b, err := readRegular(path)
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
	f := &deploysFile{path: path, histories: map[int][]byte{}}
	if err := raw.Deploys.Decode(&f.entries); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	h := sha256.New()
	h.Write(b)
	var root *os.Root
	defer func() {
		if root != nil {
			root.Close()
		}
	}()
	for i, e := range f.entries {
		switch {
		case e.History != "" && !e.Start.IsZero():
			return nil, fmt.Errorf("%s: deploy %d has a history and a start: give one", path, i+1)
		case e.History == "" && e.Start.IsZero():
			return nil, fmt.Errorf("%s: deploy %d has neither a history nor a start", path, i+1)
		case e.History == "":
			continue
		case !filepath.IsLocal(e.History):
			return nil, fmt.Errorf("%s: deploy %d: a history is a file beside the deploys file, or below it, by a relative path", path, i+1)
		}
		if root == nil {
			if root, err = os.OpenRoot(filepath.Dir(path)); err != nil {
				return nil, err
			}
		}
		hb, err := readInRoot(root, e.History)
		if err != nil {
			return nil, fmt.Errorf("%s: deploy %d: %w", path, i+1, err)
		}
		f.histories[i] = hb
		fmt.Fprintf(h, "\x00%d\x00%d\x00", i, len(hb))
		h.Write(hb)
	}
	f.sum = hex.EncodeToString(h.Sum(nil))
	return f, nil
}

// deploys are what the file holds, each history's lines read.
func (f *deploysFile) deploys() ([]ledger.Deploy, error) {
	var out []ledger.Deploy
	for i, e := range f.entries {
		if e.History == "" {
			out = append(out, ledger.Deploy{Start: e.Start, End: e.End, Labels: e.Labels, Revision: e.Revision})
			continue
		}
		// Every line of a history deploys what its entry names: they share
		// the entry's labels, which the ledger keeps once.
		ds, err := readHistory(f.histories[i], e.History, e.Labels)
		if err != nil {
			return nil, fmt.Errorf("%s: deploy %d: %w", f.path, i+1, err)
		}
		out = append(out, ds...)
		if len(out) > ledger.MaxDeploys {
			return nil, fmt.Errorf("%s: over %d deploys: keep fewer in each history (DEPLOYS_KEEP), or fewer histories", f.path, ledger.MaxDeploys)
		}
	}
	return out, nil
}

// LoadDeploys reads a deploys file (readDeploys), and each history's
// lines; sum is a digest of the file and its histories.
func LoadDeploys(path string) (ds []ledger.Deploy, sum string, err error) {
	f, err := readDeploys(path)
	if err != nil {
		return nil, "", err
	}
	ds, err = f.deploys()
	return ds, f.sum, err
}

// readHistory reads the deploys a recipe wrote, each a deploy of what the
// labels name, at an instant.
func readHistory(b []byte, name string, labels map[string]string) ([]ledger.Deploy, error) {
	var out []ledger.Deploy
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		m := deployLine.FindSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("%s:%d: want REVISION TIME, as recipes/versions/argocd.sh and flux.sh write them", name, n)
		}
		at, err := time.Parse(time.RFC3339, string(m[2]))
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", name, n, err)
		}
		out = append(out, ledger.Deploy{Start: at, End: at, Labels: labels, Revision: string(m[1]), Source: filepath.Base(name)})
		if len(out) > ledger.MaxDeploys {
			break
		}
	}
	return out, sc.Err()
}

// errNotRegular is a deploys file or history that isn't a regular file: a
// FIFO would hold a read up for good.
var errNotRegular = errors.New("not a regular file")

// readRegular reads a regular file, deploysMax at most.
func readRegular(path string) ([]byte, error) {
	if st, err := os.Stat(path); err != nil {
		return nil, err
	} else if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %w", path, errNotRegular)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readFile(f, path)
}

// readInRoot reads a regular file in root, deploysMax at most. A link out
// of root is refused, as is a name climbing out of it.
func readInRoot(root *os.Root, name string) ([]byte, error) {
	if st, err := root.Stat(name); err != nil {
		return nil, unrooted(name, err)
	} else if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %w", name, errNotRegular)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, unrooted(name, err)
	}
	defer f.Close()
	return readFile(f, name)
}

// unrooted says why a history couldn't be read, by the name given alone:
// never where a link led.
func unrooted(name string, err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return fmt.Errorf("%s: %w", name, err)
}

func readFile(f *os.File, name string) ([]byte, error) {
	if st, err := f.Stat(); err != nil {
		return nil, err
	} else if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %w", name, errNotRegular)
	}
	b, err := io.ReadAll(io.LimitReader(f, deploysMax+1))
	if err == nil && len(b) > deploysMax {
		err = fmt.Errorf("%s: over %d MB", name, deploysMax>>20)
	}
	return b, err
}

// syncDeploys reads the deploys file again, and keeps what it holds: when
// it and its histories are as they were, nothing more is read or written.
func (s *Server) syncDeploys(ctx context.Context, o verdict.Options) error {
	f, err := readDeploys(s.cfg.Deploys)
	if err != nil {
		return err
	}
	read := ledger.DeploysRead{At: s.now(), Sum: f.sum}
	unchanged, err := s.l.DeploysUnchanged(ctx, read, o)
	if err != nil {
		return err
	}
	if !unchanged {
		ds, err := f.deploys()
		if err != nil {
			return err
		}
		changed, err := s.l.SyncDeploys(ctx, ds, o, read)
		if err != nil {
			return err
		}
		if changed {
			s.log.Info("deploys file read again", "deploys", len(ds))
		}
	}
	s.deploysRead.Store(read.At.Unix())
	return nil
}
