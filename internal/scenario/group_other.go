//go:build !unix

package scenario

import "os/exec"

// ownGroup does nothing here: the run's process alone is killed when it
// is stopped.
func ownGroup(*exec.Cmd) {}
