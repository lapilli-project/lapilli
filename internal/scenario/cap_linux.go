package scenario

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// CanCap says a run's memory can be capped here (Promtool.Memory): on
// Linux, by RLIMIT_DATA, which since Linux 4.7 holds every private
// writable mapping a process makes, Go's heap among them. RLIMIT_AS would
// hold the address space Go's runtime reserves as it starts, which is far
// more than it uses, and stop promtool before it reads a test.
const CanCap = true

// The memory cap's wrapper: Lapilli started again under capName runs
// nothing of its own, whichever binary it is (lapilli, or a test binary
// that links this package), and is never a command a person gives.
func init() {
	if filepath.Base(os.Args[0]) == capName {
		os.Exit(runCapped(os.Args[1:], os.Environ()))
	}
}

// runCapped sets RLIMIT_DATA, hard and soft, to the cap capMemory gives,
// and executes promtool at capPath in this process's place, with test
// rules test.yaml and TMPDIR alone, as Run starts it uncapped. It refuses
// to when it was started otherwise than Run starts it (other arguments, or
// anything else in its environment), and returns only when it couldn't.
func runCapped(args, env []string) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "lapilli: "+format+"\n", a...)
		return capFailed
	}
	var path, tmp, memory string
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "TMPDIR":
			tmp = v
		case capPath:
			path = v
		case capMemory:
			memory = v
		default:
			return fail("promtool's memory cap was started otherwise than lapilli starts it")
		}
	}
	n, err := strconv.ParseUint(memory, 10, 64)
	if len(env) != 3 || err != nil || n < MinMemory || !filepath.IsAbs(path) || tmp == "" || !slices.Equal(args, []string{"test", "rules", testName}) {
		return fail("promtool's memory cap was started otherwise than lapilli starts it")
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_DATA, &syscall.Rlimit{Cur: n, Max: n}); err != nil {
		return fail("promtool's memory cap couldn't be set: %v", err)
	}
	err = syscall.Exec(path, []string{path, "test", "rules", testName}, []string{"TMPDIR=" + tmp})
	return fail("promtool couldn't be started: %v", err)
}
