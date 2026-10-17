//go:build unix

package probe

import (
	"os/exec"
	"syscall"
)

// detach starts ssh in a session of its own, without a controlling terminal,
// so nothing it runs (a jump host's ssh, a ProxyCommand) can ask for a password
// on yours; and stops the whole group when the probe ends.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
