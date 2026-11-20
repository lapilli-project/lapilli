//go:build unix

package scenario

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// ownGroup starts a run in a process group of its own, killed whole when
// the run is stopped: a --promtool that is a wrapper (a shell script that
// starts promtool, a runner of containers) leaves no child running past
// its time limit. The group's id is its leader's pid: promtool, or the
// memory cap's wrapper, which promtool replaces.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
