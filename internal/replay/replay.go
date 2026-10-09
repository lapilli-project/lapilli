// Package replay serves a frozen case so that an agent's ordinary tools work against it.
//
// kubectl talks to crust-gather's API server over the snapshot. Metrics queries go to the frozen
// store behind the frozen clock. Nothing here reaches a live cluster, and the guard in front of
// kubectl (internal/guard) is there to keep an agent that runs `kubectl` from reaching one either.
package replay

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lapilli-project/lapilli/internal/casefile"
	"github.com/lapilli-project/lapilli/internal/freeze"
	"github.com/lapilli-project/lapilli/internal/guard"
	"github.com/lapilli-project/lapilli/internal/metrics"
)

// What an archive may unpack to. The largest of the three cases here is 676 files and 5 MB; these are
// there so that a hostile one fills neither the disk nor a directory.
var (
	MaxEntries = 500_000
	MaxBytes   = int64(8) << 30
)

// Extract unpacks a case archive into dest. It writes regular files and directories and refuses
// everything else — links, devices, absolute paths, paths that climb out, more than MaxEntries entries
// or MaxBytes of content — because a case is something one downloads from a stranger.
func Extract(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", archive, err)
	}
	root, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	entries, room := 0, MaxBytes
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", archive, err)
		}
		if entries++; entries > MaxEntries {
			return fmt.Errorf("%s: more than %d entries", archive, MaxEntries)
		}
		if !filepath.IsLocal(filepath.FromSlash(h.Name)) {
			return fmt.Errorf("%s: entry %q would be written outside the case", archive, h.Name)
		}
		target := filepath.Join(root, filepath.FromSlash(h.Name))
		switch h.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(target, 0o755)
		case tar.TypeReg:
			if err = os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				break
			}
			var out *os.File
			if out, err = os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644); err != nil {
				break
			}
			var n int64
			n, err = io.Copy(out, io.LimitReader(tr, room+1)) // the header's size is a claim; this is what is written
			if room -= n; err == nil && room < 0 {
				err = fmt.Errorf("unpacks to more than %d bytes", MaxBytes)
			}
			if cerr := out.Close(); err == nil {
				err = cerr
			}
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
		default:
			err = fmt.Errorf("entry %q is neither a file nor a directory", h.Name)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", archive, err)
		}
	}
}

// The two tools that go first on the agent's PATH are one line each: they hand over to this binary.
// `kubectl` goes to the guard (internal/guard), which decides what the real kubectl is run with;
// `promq` is the metrics helper.
const (
	kubectlShim = "#!/bin/sh\nexec %s guard-kubectl %s %s \"$@\"\n"
	promqShim   = "#!/bin/sh\nexec %s promq \"$@\"\n"
)

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// WriteTools puts the kubectl guard and the promq helper into binDir, which goes first on the agent's
// PATH. kubeconfig is the one file kubectl may use; self is the lapilli-case binary both hand over to.
func WriteTools(binDir, kubeconfig, self string) error {
	if _, err := guard.LoadPin(kubeconfig); err != nil { // now, rather than at the agent's first command
		return err
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	bin, _ := filepath.Abs(binDir)
	var real string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if abs, _ := filepath.Abs(dir); abs == bin {
			continue
		}
		if st, err := os.Stat(filepath.Join(dir, "kubectl")); err == nil && !st.IsDir() {
			real, _ = filepath.Abs(filepath.Join(dir, "kubectl"))
			break
		}
	}
	if real == "" {
		return errors.New("kubectl was not found on PATH")
	}
	shim := fmt.Sprintf(kubectlShim, shellQuote(self), shellQuote(kubeconfig), shellQuote(real))
	if err := os.WriteFile(filepath.Join(binDir, "kubectl"), []byte(shim), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(binDir, "promq"), []byte(fmt.Sprintf(promqShim, shellQuote(self))), 0o755)
}

// Session is a case being served.
type Session struct {
	Case    *casefile.Case
	Info    *casefile.FreezeInfo
	Workdir string
	// Env is what an agent should run with: KUBECONFIG, PATH and, when the case has metrics, PROM_URL.
	Env map[string]string

	cleanup []func()
}

// Close stops the servers and removes the working directory if Serve created it.
func (s *Session) Close() {
	for i := len(s.cleanup) - 1; i >= 0; i-- {
		s.cleanup[i]()
	}
	s.cleanup = nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func waitFor(ctx context.Context, d time.Duration, ready func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if ready() {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// Serve starts everything a case needs and returns once it answers. workdir may be empty, in which
// case a temporary directory is created and removed again on Close. self is the lapilli-case binary.
func Serve(ctx context.Context, caseDir, workdir, self string) (s *Session, err error) {
	c, err := casefile.Load(caseDir)
	if err != nil {
		return nil, err
	}
	info, err := casefile.LoadFreezeInfo(caseDir)
	if err != nil {
		return nil, err
	}
	s = &Session{Case: c, Info: info, Workdir: workdir}
	began := time.Now() // before the snapshot is unpacked: see lineTime
	// What was started is stopped if the rest fails. A failure returns no session, and by then the
	// result's name holds none: closing through it, and the cleanups reading the working directory
	// through it, were a nil pointer where an error should have been — on every failure from here on, a
	// metrics file that cannot be read among them. So the session is put back under that name while it
	// is closed.
	started := s
	defer func() {
		if err != nil {
			s = started
			s.Close()
			s = nil
		}
	}()
	if workdir == "" {
		if s.Workdir, err = os.MkdirTemp("", "lapilli-case-"); err != nil {
			return nil, err
		}
		s.cleanup = append(s.cleanup, func() { os.RemoveAll(s.Workdir) }) // after a failure too: what an error has to say of a file here, it says itself
	}
	if s.Workdir, err = filepath.Abs(s.Workdir); err != nil {
		return nil, err
	}

	snapshot := filepath.Join(s.Workdir, "kubernetes")
	if err = Extract(filepath.Join(caseDir, casefile.KubernetesName), snapshot); err != nil {
		return nil, err
	}
	// Two kubeconfigs: the one the snapshot server writes for itself, and the one the agent is given,
	// which points at what stands in front of it (fields.go).
	upstreamConfig, kubeconfig := filepath.Join(s.Workdir, "snapshot.kubeconfig"), filepath.Join(s.Workdir, "kubeconfig")
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	log, err := os.Create(filepath.Join(s.Workdir, "serve.log"))
	if err != nil {
		return nil, err
	}
	// -k is always given: without it the server would write its context into the default kubeconfig.
	cmd := exec.Command(freeze.CrustGather(), "serve", "-a", snapshot, "-s", fmt.Sprintf("127.0.0.1:%d", port), "-k", upstreamConfig)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+upstreamConfig)
	cmd.Stdout, cmd.Stderr = log, log
	if err = cmd.Start(); err != nil {
		log.Close()
		return nil, fmt.Errorf("starting %s: %w", freeze.CrustGather(), err)
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	s.cleanup = append(s.cleanup, func() {
		cmd.Process.Signal(os.Interrupt)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
			<-exited
		}
		log.Close()
	})
	up := waitFor(ctx, 60*time.Second, func() bool {
		select {
		case <-exited:
			return true // it died; the checks below report it
		default:
		}
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
		_, err = os.Stat(upstreamConfig)
		return err == nil
	})
	select {
	case <-exited:
		up = false
	default:
	}
	if !up {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("the snapshot API server was not waited for: %w", ctx.Err())
		}
		// What it wrote before it gave up, in the error itself: its log is in a directory that goes with the session.
		said, _ := os.ReadFile(log.Name())
		if said = bytes.TrimSpace(said); len(said) > 2000 {
			said = append([]byte("… "), said[len(said)-2000:]...)
		} else if len(said) == 0 {
			said = []byte("it wrote nothing")
		}
		return nil, fmt.Errorf("the snapshot API server did not start: %s", said)
	}

	upstream, err := guard.LoadPin(upstreamConfig)
	if err != nil {
		return nil, err
	}
	server, err := url.Parse(upstream.Server)
	if err != nil {
		return nil, err
	}
	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	answers := newFront(server, time.Unix(0, int64(info.FreezeTime*1e9)))
	answers.served = began
	filter := &http.Server{Handler: answers, ReadHeaderTimeout: 10 * time.Second}
	go filter.Serve(front)
	s.cleanup = append(s.cleanup, func() { filter.Close() })
	config := fmt.Sprintf("apiVersion: v1\nkind: Config\ncurrent-context: case\ncontexts:\n- name: case\n  context: {cluster: case, user: case}\n"+
		"clusters:\n- name: case\n  cluster: {server: %q}\nusers:\n- name: case\n  user: {}\n", "http://"+front.Addr().String()+server.Path)
	if err = os.WriteFile(kubeconfig, []byte(config), 0o400); err != nil { // nothing has a reason to write it again
		return nil, err
	}
	bin := filepath.Join(s.Workdir, "bin")
	if err = WriteTools(bin, kubeconfig, self); err != nil {
		return nil, err
	}
	s.Env = map[string]string{"KUBECONFIG": kubeconfig, "PATH": bin + string(os.PathListSeparator) + os.Getenv("PATH")}

	if _, statErr := os.Stat(filepath.Join(caseDir, casefile.MetricsName)); statErr == nil {
		store, err := metrics.Load(filepath.Join(caseDir, casefile.MetricsName))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", casefile.MetricsName, err)
		}
		if m := info.Metrics; m != nil { // what the freeze learned of the Prometheus, which the metrics file does not carry
			store.Source = metrics.Source{Version: m.PrometheusVersion, EvaluationInterval: time.Duration(m.EvaluationIntervalMs) * time.Millisecond,
				LookbackDelta: time.Duration(m.LookbackDeltaMs) * time.Millisecond}
			// Where the metrics begin and where the blocks ended are what a replay goes by. Whether the
			// order is the head's and which labels were taken off are said in freeze.json for whoever
			// reads it: the file is in the order it is in, and a replay hands it over as it is.
			store.From, store.HeadFrom = m.FromMs, m.HeadFromMs
		}
		if described := filepath.Join(caseDir, casefile.MetadataName); info.Metrics != nil && info.Metrics.MetadataFamilies > 0 {
			if store.Metadata, err = metrics.LoadMetadata(described); err != nil {
				return nil, fmt.Errorf("%s: %w", casefile.MetadataName, err)
			}
		}
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		srv := &http.Server{Handler: metrics.NewAPI(store, metrics.FromSeconds(info.FreezeTime)).Handler(), ReadHeaderTimeout: 10 * time.Second}
		go srv.Serve(l)
		s.cleanup = append(s.cleanup, func() { srv.Close() })
		s.Env["PROM_URL"] = "http://" + l.Addr().String()
	}
	return s, nil
}
