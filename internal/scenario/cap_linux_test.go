package scenario

import (
	"strconv"
	"syscall"
	"testing"
)

// Guard: the wrapper runs promtool only as Run starts it: another argument,
// anything else in its environment, a cap below MinMemory, a path that
// isn't absolute, and it refuses, setting nothing. Lapilli started under
// its name by anyone else is no command to run something else.
func TestCapWrapperRefuses(t *testing.T) {
	good := []string{"TMPDIR=/tmp/x", capPath + "=/bin/true", capMemory + "=" + strconv.Itoa(MinMemory)}
	for name, c := range map[string]struct{ args, env []string }{
		"args":     {[]string{"test", "rules", "other.yaml"}, good},
		"more env": {[]string{"test", "rules", "test.yaml"}, append(good[:3:3], "LD_PRELOAD=/x.so")},
		"low cap":  {[]string{"test", "rules", "test.yaml"}, []string{good[0], good[1], capMemory + "=1024"}},
		"relative": {[]string{"test", "rules", "test.yaml"}, []string{good[0], capPath + "=bin/true", good[2]}},
		"no cap":   {[]string{"test", "rules", "test.yaml"}, good[:2]},
	} {
		if runCapped(c.args, c.env) != capFailed {
			t.Errorf("%s: not refused", name)
		}
	}
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_DATA, &lim); err != nil || lim.Cur == MinMemory {
		t.Errorf("a refusal set the cap: %+v %v", lim, err)
	}
}
