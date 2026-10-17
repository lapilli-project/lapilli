//go:build !unix

package probe

import "os/exec"

func detach(*exec.Cmd) {}
