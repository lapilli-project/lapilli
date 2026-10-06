package replay

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
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

	// An archive that unpacks to too much, or to too many, is refused part-way rather than obeyed.
	defer func(e int, b int64) { MaxEntries, MaxBytes = e, b }(MaxEntries, MaxBytes)
	MaxEntries, MaxBytes = 3, 10
	if err := Extract(tarball(t, entry{name: "a", body: "12345", kind: tar.TypeReg}, entry{name: "b", body: "12345", kind: tar.TypeReg}), filepath.Join(t.TempDir(), "out")); err != nil {
		t.Errorf("an archive exactly at the limit was refused: %v", err)
	}
	if err := Extract(tarball(t, entry{name: "a", body: "12345", kind: tar.TypeReg}, entry{name: "b", body: "123456", kind: tar.TypeReg}), filepath.Join(t.TempDir(), "out")); err == nil || !strings.Contains(err.Error(), "more than 10 bytes") {
		t.Errorf("an archive one byte over the limit: %v", err)
	}
	if err := Extract(tarball(t, entry{name: "a", kind: tar.TypeReg}, entry{name: "b", kind: tar.TypeReg}, entry{name: "c", kind: tar.TypeReg}, entry{name: "d", kind: tar.TypeReg}), filepath.Join(t.TempDir(), "out")); err == nil || !strings.Contains(err.Error(), "more than 3 entries") {
		t.Errorf("an archive with too many entries: %v", err)
	}
}

// WriteTools writes two one-line hand-overs. What the guard then does with kubectl's arguments is
// tested in internal/guard, and the whole chain in cmd/lapilli-case.
func TestWriteToolsHandsKubectlAndPromqToThisBinary(t *testing.T) {
	dir := t.TempDir()
	odd := filepath.Join(dir, "it's here") // a path may carry a quote
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(odd, 0o755)
	if err := os.WriteFile(filepath.Join(odd, "kubectl"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	kubeconfig := filepath.Join(odd, "kubeconfig")
	os.WriteFile(kubeconfig, []byte("current-context: c\ncontexts:\n- {name: c, context: {cluster: c}}\nclusters:\n- {name: c, cluster: {server: 'http://127.0.0.1:1'}}\n"), 0o600)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+odd+string(os.PathListSeparator)+"/usr/bin")
	if err := WriteTools(bin, kubeconfig, "/opt/lapilli-case"); err != nil {
		t.Fatal(err)
	}
	quoted := strings.ReplaceAll(odd, "'", `'\''`)
	shim, _ := os.ReadFile(filepath.Join(bin, "kubectl"))
	if want := "#!/bin/sh\nexec '/opt/lapilli-case' guard-kubectl '" + quoted + "/kubeconfig' '" + quoted + "/kubectl' \"$@\"\n"; string(shim) != want {
		t.Errorf("kubectl shim\n got %s\nwant %s", shim, want)
	}
	shim, _ = os.ReadFile(filepath.Join(bin, "promq"))
	if want := "#!/bin/sh\nexec '/opt/lapilli-case' promq \"$@\"\n"; string(shim) != want {
		t.Errorf("promq shim = %s", shim)
	}
	for _, name := range []string{"kubectl", "promq"} {
		if st, err := os.Stat(filepath.Join(bin, name)); err != nil || st.Mode()&0o111 == 0 {
			t.Errorf("%s is not executable: %v", name, err)
		}
	}

	// Written twice, the guard must not come to wrap itself.
	t.Setenv("PATH", bin)
	if err := WriteTools(bin, kubeconfig, "/opt/lapilli-case"); err == nil {
		t.Error("the guard was written to wrap itself: no real kubectl is on PATH")
	}
	// A kubeconfig that names no place is found now, not at the agent's first command.
	t.Setenv("PATH", odd)
	os.WriteFile(kubeconfig, []byte("contexts: []\n"), 0o600)
	if err := WriteTools(bin, kubeconfig, "/opt/lapilli-case"); err == nil {
		t.Error("tools were written for a kubeconfig with no current context")
	}
}
