// Package scenariotest is a stand-in for promtool, for tests: a test binary
// that calls Fake first in its TestMain answers as promtool when it is run
// through a link named promtool (Install), so no test needs a real one.
//
// The stand-in checks what Lapilli must hold it to before anything else,
// and exits 3, saying WRONG, when any doesn't hold: its arguments are
// test rules test.yaml; its environment is TMPDIR alone, a directory in
// the one it runs in; the test file there names rules.yaml alone as its
// rule_files, and rules.yaml is there. Then it answers as the first test's
// name in test.yaml says:
//
//	pass            SUCCESS, exit 0
//	fail            FAILED:, exit 1
//	fail-if TEXT    FAILED: when rules.yaml holds TEXT, else SUCCESS
//	sleep           waits a minute, then SUCCESS
//	crash           killed by a signal
//	garbage         says what promtool doesn't, exit 0
//	exit2           exits 2, saying nothing promtool says of a test
//	flood           8 MiB of nothing, then SUCCESS, exit 0
//	hog             takes 1 GiB of memory, a page at a time, then SUCCESS
//	wait            waits for a file named release beside the link Install
//	                made, then SUCCESS; WRONG after a minute without it
//	see             FAILED:, then what it was given: its arguments, its
//	                environment, its RLIMIT_DATA, and rules.yaml as it
//	                read it
//
// Each run is also written down, a line a run, in the file named by
// count.txt beside the link Install made, when there is one: how many
// times promtool ran.
package scenariotest

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// Name is the name Install links the test binary under.
const Name = "promtool"

// Fake answers as promtool, and exits, when this process was started
// through a link named promtool; else it returns, and the tests run.
func Fake() {
	if filepath.Base(os.Args[0]) != Name {
		return
	}
	os.Exit(fake())
}

func wrong(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "WRONG: "+format+"\n", a...)
	return 3
}

func fake() int {
	if c := filepath.Join(filepath.Dir(os.Args[0]), "count.txt"); fileExists(c) {
		f, err := os.OpenFile(c, os.O_APPEND|os.O_WRONLY, 0)
		if err == nil {
			f.WriteString("ran\n")
			f.Close()
		}
	}
	if !slices.Equal(os.Args[1:], []string{"test", "rules", "test.yaml"}) {
		return wrong("arguments %q", os.Args[1:])
	}
	env := os.Environ()
	wd, _ := os.Getwd()
	if len(env) != 1 || !strings.HasPrefix(env[0], "TMPDIR=") {
		return wrong("environment %q", env)
	}
	if tmp := strings.TrimPrefix(env[0], "TMPDIR="); !fileExists(tmp) || real(filepath.Dir(tmp)) != real(wd) {
		return wrong("TMPDIR %s, not a directory in %s", tmp, wd)
	}
	b, err := os.ReadFile("test.yaml")
	if err != nil {
		return wrong("no test.yaml: %v", err)
	}
	var test struct {
		RuleFiles []string `yaml:"rule_files"`
		Tests     []struct {
			Name string `yaml:"name"`
		} `yaml:"tests"`
	}
	if err := yaml.Unmarshal(b, &test); err != nil {
		return wrong("test.yaml: %v", err)
	}
	if !slices.Equal(test.RuleFiles, []string{"rules.yaml"}) {
		return wrong("rule_files %q", test.RuleFiles)
	}
	group, err := os.ReadFile("rules.yaml")
	if err != nil {
		return wrong("no rules.yaml: %v", err)
	}
	mode := ""
	if len(test.Tests) > 0 {
		mode = test.Tests[0].Name
	}
	fmt.Println("Processing test file: test.yaml")
	failed := func(lines ...string) int {
		fmt.Fprintln(os.Stderr, "  FAILED:")
		for _, l := range lines {
			fmt.Fprintln(os.Stderr, l)
		}
		return 1
	}
	pass := func() int {
		fmt.Println("  SUCCESS")
		return 0
	}
	switch {
	case mode == "pass":
		return pass()
	case mode == "fail":
		return failed("    alertname: CheckoutErrors, time: 10m,", "        exp:[0:", "          Labels:{alertname=\"CheckoutErrors\"}", "        ],", "        got:[]")
	case strings.HasPrefix(mode, "fail-if "):
		if bytes.Contains(group, []byte(strings.TrimPrefix(mode, "fail-if "))) {
			return failed("    alertname: CheckoutErrors, time: 10m,", "        got:[]")
		}
		return pass()
	case mode == "sleep":
		time.Sleep(time.Minute)
		return pass()
	case mode == "crash":
		syscall.Kill(os.Getpid(), syscall.SIGKILL)
		time.Sleep(time.Minute)
		return 0
	case mode == "garbage":
		fmt.Println("all good, probably")
		return 0
	case mode == "exit2":
		fmt.Fprintln(os.Stderr, "promtool: error: something else")
		return 2
	case mode == "flood":
		line := strings.Repeat("x", 1023) + "\n"
		for range 8 << 10 {
			os.Stdout.WriteString(line)
		}
		return pass()
	case mode == "hog":
		var kept [][]byte
		for range 64 {
			b := make([]byte, 16<<20)
			for i := 0; i < len(b); i += 4096 {
				b[i] = 1
			}
			kept = append(kept, b)
		}
		fmt.Fprintln(io.Discard, len(kept))
		return pass()
	case mode == "wait":
		release := filepath.Join(filepath.Dir(os.Args[0]), "release")
		for range 6000 {
			if fileExists(release) {
				return pass()
			}
			time.Sleep(10 * time.Millisecond)
		}
		return wrong("not released")
	case mode == "see":
		var lim syscall.Rlimit
		syscall.Getrlimit(syscall.RLIMIT_DATA, &lim)
		return failed(fmt.Sprintf("args %q", os.Args[1:]), fmt.Sprintf("env %q", env), fmt.Sprintf("rlimit_data %d", lim.Cur), "rules.yaml:", string(group))
	}
	return wrong("no such mode %q", mode)
}

// real is a path with its links followed: macOS's temporary directories
// are under a link (/var to /private/var).
func real(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Install links this test binary as promtool in a new directory whose name
// a shell would split and run (a space, a ;, a $), and returns the link's
// path: started directly, it is the stand-in; started through a shell, it
// is no command at all. The test's TestMain must call Fake first.
func Install(t testing.TB) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "pro mtool;$(x)")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, Name)
	if err := os.Symlink(self, link); err != nil {
		t.Fatal(err)
	}
	return link
}

// Counted makes the stand-in at link write down each run, and returns how
// many times it has run since, as a function.
func Counted(t testing.TB, link string) func() int {
	t.Helper()
	c := filepath.Join(filepath.Dir(link), "count.txt")
	if err := os.WriteFile(c, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return func() int {
		b, _ := os.ReadFile(c)
		return bytes.Count(b, []byte("ran\n"))
	}
}

// Test is a test file of one test, whose name tells the stand-in what to
// answer, naming what rule_files it is given (none when empty).
func Test(mode string, ruleFiles ...string) string {
	var b strings.Builder
	if len(ruleFiles) > 0 {
		b.WriteString("rule_files:\n")
		for _, f := range ruleFiles {
			fmt.Fprintf(&b, "  - %q\n", f)
		}
	}
	fmt.Fprintf(&b, "evaluation_interval: 1m\ngroup_eval_order: [other, shop]\ntests:\n  - name: %q\n    interval: 1m\n    input_series:\n      - series: 'errors{service=\"checkout\"}'\n        values: '0+10x20'\n    alert_rule_test:\n      - eval_time: 10m\n        alertname: CheckoutErrors\n        exp_alerts: []\n", mode)
	return b.String()
}
