package probe

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestArgs(t *testing.T) {
	args := Args(Options{Dest: "alice@lab", Timeout: 10 * time.Second})
	for _, want := range []string{"BatchMode=yes", "CheckHostIP=no", "RemoteCommand=none", "StrictHostKeyChecking=yes", "-N"} {
		if !slices.Contains(args, want) {
			t.Errorf("args lack %s: %v", want, args)
		}
	}
	if got := args[len(args)-2:]; got[0] != "--" || got[1] != "alice@lab" {
		t.Errorf("args end with %v", got)
	}
	args = Args(Options{Dest: "alice@lab", Unknown: []string{"remotecommand"}})
	if slices.Contains(args, "RemoteCommand=none") {
		t.Errorf("an unknown option was kept: %v", args)
	}
}

// fakeSSH puts an ssh on PATH that writes its arguments, one run per line, to
// calls, and runs body.
func fakeSSH(t *testing.T, body string) (calls string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell script")
	}
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$*\" >> " + calls + "\n" + body
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return calls
}

func runs(t *testing.T, calls string) []string {
	t.Helper()
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// OpenSSH before 7.6 has no RemoteCommand and stops at it.
func TestRunLeavesOutWhatSSHDoesNotKnow(t *testing.T) {
	calls := fakeSSH(t, `case "$*" in *RemoteCommand=*)
  echo "command-line: line 0: Bad configuration option: remotecommand" >&2; exit 255;; esac
echo "OpenSSH_7.4p1, OpenSSL 1.0.2k-fips  26 Jan 2017" >&2
echo "Authenticated to lab ([172.21.0.2]:2322)." >&2
`)
	res, err := Run(context.Background(), Options{Dest: "alice@lab", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(runs(t, calls)); n != 2 {
		t.Errorf("ssh ran %d times, want 2", n)
	}
	if !strings.Contains(string(res.Log), "Authenticated to lab") {
		t.Errorf("log is from the failed run:\n%s", res.Log)
	}
	if slices.Contains(res.Args, "RemoteCommand=none") || !slices.Contains(res.Args, "CheckHostIP=no") {
		t.Errorf("args: %v", res.Args)
	}
}

// An option the user gave is theirs to fix; the probe reports it as it is.
func TestRunKeepsTheUsersOptions(t *testing.T) {
	calls := fakeSSH(t, `echo "command-line: line 0: Bad configuration option: usekeychain" >&2; exit 255
`)
	res, err := Run(context.Background(), Options{SSHArgs: []string{"-o", "UseKeychain=yes"}, Dest: "alice@lab", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(runs(t, calls)); n != 1 {
		t.Errorf("ssh ran %d times, want 1", n)
	}
	if res.ExitCode != 255 {
		t.Errorf("exit %d", res.ExitCode)
	}
}
