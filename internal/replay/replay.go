// Package replay serves a frozen case so that an agent's ordinary tools work against it.
//
// kubectl talks to crust-gather's API server over the snapshot. Metrics queries go to the frozen
// store behind the frozen clock. Nothing here can reach a live cluster, and the guard in front of
// kubectl is there so that the agent cannot either.
package replay

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lapilli-project/lapilli/internal/casefile"
	"github.com/lapilli-project/lapilli/internal/freeze"
	"github.com/lapilli-project/lapilli/internal/metrics"
)

// Extract unpacks a case archive into dest. It writes regular files and directories and refuses
// everything else — links, devices, absolute paths, paths that climb out — because a case is something
// one downloads from a stranger.
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
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", archive, err)
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
			_, err = io.Copy(out, tr)
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

// guard stands in front of the real kubectl on the agent's PATH. It runs kubectl only when it is
// pointed at the one kubeconfig the run was given, and refuses the flags that would point it elsewhere:
// an agent allowed `kubectl get *` must not be able to read another cluster by adding --kubeconfig.
const guard = `#!/bin/sh
# lapilli-case guard: an agent under evaluation may only talk to the case it was given.
if [ "$KUBECONFIG" != %[1]s ]; then
  echo "kubectl refused by lapilli-case: KUBECONFIG is not the case's" >&2; exit 97
fi
for a in "$@"; do
  case "$a" in
    --) break ;;
    --kubeconfig|--kubeconfig=*|--context|--context=*|--cluster|--cluster=*|--server|--server=*|-s|-s?*|--user|--user=*|--token|--token=*|--as|--as=*|--as-group|--as-group=*)
      echo "kubectl refused by lapilli-case: $a would point kubectl away from the case" >&2; exit 97 ;;
  esac
done
exec %[2]s "$@"
`

const promqShim = `#!/bin/sh
exec %s promq "$@"
`

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// WriteTools puts the kubectl guard and the promq helper into binDir, which goes first on the agent's
// PATH. self is the lapilli-case binary that promq is a subcommand of.
func WriteTools(binDir, kubeconfig, self string) error {
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
			real = filepath.Join(dir, "kubectl")
			break
		}
	}
	if real == "" {
		return errors.New("kubectl was not found on PATH")
	}
	if err := os.WriteFile(filepath.Join(binDir, "kubectl"), []byte(fmt.Sprintf(guard, shellQuote(kubeconfig), shellQuote(real))), 0o755); err != nil {
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
	defer func() {
		if err != nil {
			s.Close()
		}
	}()
	if workdir == "" {
		if s.Workdir, err = os.MkdirTemp("", "lapilli-case-"); err != nil {
			return nil, err
		}
		s.cleanup = append(s.cleanup, func() { os.RemoveAll(s.Workdir) })
	}
	if s.Workdir, err = filepath.Abs(s.Workdir); err != nil {
		return nil, err
	}

	snapshot := filepath.Join(s.Workdir, "kubernetes")
	if err = Extract(filepath.Join(caseDir, casefile.KubernetesName), snapshot); err != nil {
		return nil, err
	}
	kubeconfig := filepath.Join(s.Workdir, "kubeconfig")
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	log, err := os.Create(filepath.Join(s.Workdir, "serve.log"))
	if err != nil {
		return nil, err
	}
	// -k is always given: without it the server would write its context into the default kubeconfig.
	cmd := exec.Command(freeze.CrustGather(), "serve", "-a", snapshot, "-s", fmt.Sprintf("127.0.0.1:%d", port), "-k", kubeconfig)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
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
		_, err = os.Stat(kubeconfig)
		return err == nil
	})
	select {
	case <-exited:
		up = false
	default:
	}
	if !up {
		return nil, fmt.Errorf("the snapshot API server did not start; see %s", log.Name())
	}

	bin := filepath.Join(s.Workdir, "bin")
	if err = WriteTools(bin, kubeconfig, self); err != nil {
		return nil, err
	}
	s.Env = map[string]string{"KUBECONFIG": kubeconfig, "PATH": bin + string(os.PathListSeparator) + os.Getenv("PATH")}

	if _, statErr := os.Stat(filepath.Join(caseDir, casefile.MetricsName)); statErr == nil {
		store, err := metrics.Load(filepath.Join(caseDir, casefile.MetricsName))
		if err != nil {
			return nil, err
		}
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		srv := &http.Server{Handler: metrics.NewAPI(store, metrics.FromSeconds(info.FreezeTime), nil).Handler(), ReadHeaderTimeout: 10 * time.Second}
		go srv.Serve(l)
		s.cleanup = append(s.cleanup, func() { srv.Close() })
		s.Env["PROM_URL"] = "http://" + l.Addr().String()
	}
	return s, nil
}
