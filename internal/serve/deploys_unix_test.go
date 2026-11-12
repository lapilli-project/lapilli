//go:build unix

package serve

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO for a deploys file or a history is refused, not read: a read of
// one would hold the worker up for good.
func TestDeploysFIFO(t *testing.T) {
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
