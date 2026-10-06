package replay

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name, body string
	kind       byte
	link       string
}

func tarball(t *testing.T, entries ...entry) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.kind, Mode: 0o644, Size: int64(len(e.body)), Linkname: e.link}
		if e.kind == tar.TypeDir {
			h.Mode = 0o755
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(e.body))
	}
	tw.Close()
	zw.Close()
	path := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractWritesFilesAndRefusesEverythingElse(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out")
	good := tarball(t, entry{name: "cluster/", kind: tar.TypeDir}, entry{name: "cluster/node.yaml", body: "kind: Node\n", kind: tar.TypeReg}, entry{name: "deep/er/file", body: "x", kind: tar.TypeReg})
	if err := Extract(good, dest); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(dest, "cluster", "node.yaml")); err != nil || string(raw) != "kind: Node\n" {
		t.Errorf("extracted %q (%v)", raw, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "deep", "er", "file")); err != nil {
		t.Error("a file whose directory had no entry of its own was not written")
	}

	outside := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(outside, []byte("mine"), 0o644)
	for name, archive := range map[string]string{
		"a path that climbs out": tarball(t, entry{name: "../escaped", body: "x", kind: tar.TypeReg}),
		"an absolute path":       tarball(t, entry{name: "/etc/escaped", body: "x", kind: tar.TypeReg}),
		"a symbolic link":        tarball(t, entry{name: "link", kind: tar.TypeSymlink, link: outside}),
		"a hard link":            tarball(t, entry{name: "link", kind: tar.TypeLink, link: outside}),
		"a device":               tarball(t, entry{name: "dev", kind: tar.TypeChar}),
		"a file written twice":   tarball(t, entry{name: "a", body: "1", kind: tar.TypeReg}, entry{name: "a", body: "2", kind: tar.TypeReg}),
	} {
		into := filepath.Join(t.TempDir(), "out")
		if err := Extract(archive, into); err == nil {
			t.Errorf("%s was extracted", name)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(into), "escaped")); err == nil {
			t.Errorf("%s wrote outside the destination", name)
		}
	}
	if raw, _ := os.ReadFile(outside); string(raw) != "mine" {
		t.Error("a file outside the destination was changed")
	}
}

// The guard is what stands between an agent that was allowed `kubectl get *` and every other cluster
// the machine can reach.
func TestTheGuardLetsKubectlTalkToTheCaseAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	realDir, bin := filepath.Join(dir, "real"), filepath.Join(dir, "bin")
	os.MkdirAll(realDir, 0o755)
	// A stand-in for kubectl that says what it was asked. Its directory carries a quote, as a path may.
	odd := filepath.Join(dir, "it's here")
	os.MkdirAll(odd, 0o755)
	fake := "#!/bin/sh\necho \"ran: $*\"\n"
	if err := os.WriteFile(filepath.Join(odd, "kubectl"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	kubeconfig := filepath.Join(odd, "kubeconfig")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+odd+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := WriteTools(bin, kubeconfig, "/opt/lapilli-case"); err != nil {
		t.Fatal(err)
	}

	run := func(env string, args ...string) (string, int) {
		cmd := exec.Command(filepath.Join(bin, "kubectl"), args...)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+env)
		out, err := cmd.CombinedOutput()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out)), code
	}

	if out, code := run(kubeconfig, "get", "pods", "-A", "--sort-by=.metadata.name", "--since=5m", "-l", "app=x"); code != 0 || out != "ran: get pods -A --sort-by=.metadata.name --since=5m -l app=x" {
		t.Errorf("an ordinary read was refused or changed: %q (exit %d)", out, code)
	}
	if out, code := run(kubeconfig, "exec", "pod", "--", "curl", "--server", "x", "-s"); code != 0 || !strings.HasPrefix(out, "ran: exec pod --") {
		t.Errorf("flags that belong to the command after -- were refused: %q (exit %d)", out, code)
	}
	if out, code := run("/home/me/.kube/config", "get", "pods"); code != 97 || strings.Contains(out, "ran:") {
		t.Errorf("kubectl ran with another KUBECONFIG: %q (exit %d)", out, code)
	}
	if out, code := run("", "get", "pods"); code != 97 || strings.Contains(out, "ran:") {
		t.Errorf("kubectl ran with no KUBECONFIG, which means the default one: %q (exit %d)", out, code)
	}
	for _, flag := range [][]string{
		{"--kubeconfig", "/home/me/.kube/config"}, {"--kubeconfig=/home/me/.kube/config"}, {"--context", "prod"}, {"--context=prod"},
		{"--cluster=prod"}, {"--server", "https://prod"}, {"--server=https://prod"}, {"-s", "https://prod"}, {"-shttps://prod"}, {"-s=https://prod"},
		{"--user=admin"}, {"--token=abc"}, {"--as=system:admin"}, {"--as-group=system:masters"},
	} {
		for _, args := range [][]string{append([]string{"get", "pods"}, flag...), append(append([]string{}, flag...), "get", "pods")} {
			if out, code := run(kubeconfig, args...); code != 97 || strings.Contains(out, "ran:") {
				t.Errorf("kubectl %v reached the real binary: %q (exit %d)", args, out, code)
			}
		}
	}

	shim, _ := os.ReadFile(filepath.Join(bin, "promq"))
	if !strings.Contains(string(shim), `exec '/opt/lapilli-case' promq "$@"`) {
		t.Errorf("promq shim = %s", shim)
	}
	t.Setenv("PATH", bin)
	if err := WriteTools(bin, kubeconfig, "/opt/lapilli-case"); err == nil {
		t.Error("the guard was written to wrap itself: no real kubectl is on PATH")
	}
}
