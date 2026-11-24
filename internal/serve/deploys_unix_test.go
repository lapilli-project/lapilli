//go:build unix

package serve

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A FIFO for a deploys file or a history is refused, not read: a read of
// one would hold the worker up for good.
func TestDeploysFIFO(t *testing.T) {
	parallel(t)
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "h.txt"), 0o644); err != nil {
		t.Skip(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo.yaml"), 0o644); err != nil {
		t.Skip(err)
	}
	file := filepath.Join(dir, "deploys.yaml")
	os.WriteFile(file, []byte("deploys:\n  - {labels: {service: a}, history: h.txt}\n"), 0o644)
	done := make(chan error, 2)
	go func() { _, _, err := LoadDeploys(file); done <- err }()
	go func() { _, _, err := LoadDeploys(filepath.Join(dir, "fifo.yaml")); done <- err }()
	for range 2 {
		select {
		case err := <-done:
			if err == nil {
				t.Error("a FIFO taken")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("reading a FIFO held the read up")
		}
	}
}

// A deploys file read, whose directory can't be opened for its histories,
// says so by the file's path, which a reader who isn't an admin is shown
// without (redactPaths), never by its directory's.
func TestDeploysDirectorySaidByTheFile(t *testing.T) {
	parallel(t)
	dir := filepath.Join(t.TempDir(), "private-mount")
	os.Mkdir(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "h.txt"), nil, 0o644)
	file := filepath.Join(dir, "deploys.yaml")
	os.WriteFile(file, []byte("deploys:\n  - {labels: {service: a}, history: h.txt}\n"), 0o644)
	if err := os.Chmod(dir, 0o100); err != nil {
		t.Skip(err)
	}
	defer os.Chmod(dir, 0o755)
	_, _, err := LoadDeploys(file)
	if err == nil {
		t.Skip("a directory that can't be read was opened: run as root?")
	}
	if rest := strings.ReplaceAll(err.Error(), file, ""); strings.Contains(rest, dir) {
		t.Errorf("said by the directory's path: %v", err)
	}
}
