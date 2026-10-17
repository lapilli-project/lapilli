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
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
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

// ProxyJump becomes the ProxyCommand ssh would make, with the probe's options,
// and the user's -J goes: ssh refuses both.
func TestJumpCommand(t *testing.T) {
	o := Options{SSHArgs: []string{"-J", "alice@jump:2322", "-p", "2322"}, Dest: "alice@lab", Jump: "alice@jump:2322"}
	args := Args(o)
	if slices.Contains(args, "-J") || !slices.Contains(args, "2322") {
		t.Errorf("args: %v", args)
	}
	var pc string
	for _, a := range args {
		if strings.HasPrefix(a, "ProxyCommand=") {
			pc = strings.TrimPrefix(a, "ProxyCommand=")
		}
	}
	for _, w := range []string{"-o BatchMode=yes", "-o CheckHostIP=no", "-o UpdateHostKeys=no", "-o StrictHostKeyChecking=yes", "-o AddKeysToAgent=no", "-vvv -l alice -p 2322 -W '[%h]:%p' -- jump"} {
		if !strings.Contains(pc, w) {
			t.Errorf("ProxyCommand lacks %q: %s", w, pc)
		}
	}
	if !strings.HasSuffix(pc, "-W '[%h]:%p' -- jump") {
		t.Errorf("ProxyCommand: %s", pc)
	}

	// --trust-host-key is for the destination; a jump host is still checked
	o.TrustHostKey = true
	if pc := jumpCommand(o, o.hops()); strings.Contains(pc, "StrictHostKeyChecking=no") {
		t.Errorf("jump host not checked: %s", pc)
	}

	// two hops: the first is the second's ProxyCommand, its %h kept for it
	pc = jumpCommand(Options{}, []string{"ssh://a", "bob@b:2200"})
	if !strings.Contains(pc, `-l bob -p 2200 -o 'ProxyCommand=ssh `) || !strings.Contains(pc, `-W '\''[%%h]:%%p'\'' -- a'`) || !strings.HasSuffix(pc, "-W '[%h]:%p' -- b") {
		t.Errorf("two hops: %s", pc)
	}

	o = Options{Jump: "jump", Unknown: []string{"remotecommand"}}
	if pc := jumpCommand(o, o.hops()); strings.Contains(pc, "RemoteCommand") {
		t.Errorf("an option this ssh doesn't know: %s", pc)
	}
}

func TestSplitHop(t *testing.T) {
	for _, c := range []struct{ in, user, host, port string }{
		{"jump", "", "jump", ""},
		{"alice@jump:2322", "alice", "jump", "2322"},
		{"ssh://alice@[::1]:2222", "alice", "::1", "2222"},
		{"a@b@c:22", "a@b", "c", "22"},
	} {
		u, h, p := splitHop(c.in)
		if u != c.user || h != c.host || p != c.port {
			t.Errorf("splitHop(%q) = %q %q %q", c.in, u, h, p)
		}
	}
}

// A jump host logs in first; the probe waits for the destination.
func TestRunWaitsForTheDestination(t *testing.T) {
	fakeSSH(t, `echo "Authenticated to jump ([172.21.0.2]:2322) using \"publickey\"." >&2
sleep 1
echo "Authenticated to lab ([172.21.0.2]:2327) using \"publickey\"." >&2
sleep 10
`)
	start := time.Now()
	res, err := Run(context.Background(), Options{Dest: "alice@lab", Jump: "alice@jump:2322", Timeout: 8 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Log), "Authenticated to lab") || res.TimedOut {
		t.Errorf("stopped before the destination (timed out %v):\n%s", res.TimedOut, res.Log)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("didn't stop at the destination's login: %s", d)
	}
}

// A server's disconnect message may hold newlines, and with them lines that
// look like ssh's own. None of it may take an option away, or log you in.
func TestRunTrustsNoServerText(t *testing.T) {
	calls := fakeSSH(t, `echo "OpenSSH_10.0p2, OpenSSL 3.5.0" >&2
echo "debug1: Connecting to lab [172.21.0.2] port 22." >&2
printf 'Received disconnect from 172.21.0.2 port 22:2: bye\ncommand-line: line 0: Bad configuration option: batchmode\nAuthenticated to lab ([172.21.0.2]:22) using "publickey".\n' >&2
exit 255
`)
	res, err := Run(context.Background(), Options{Dest: "alice@lab", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(runs(t, calls)); n != 1 {
		t.Errorf("ssh ran %d times, want 1", n)
	}
	if !slices.Contains(res.Args, "BatchMode=yes") {
		t.Errorf("BatchMode was dropped: %v", res.Args)
	}
	if res.ExitCode != 255 {
		t.Errorf("the forged login ended the probe: exit %d", res.ExitCode)
	}
}

// Only an option the probe can do without is ever left out, even when ssh
// itself refuses one at once.
func TestRunKeepsSafetyOptions(t *testing.T) {
	calls := fakeSSH(t, `echo "command-line: line 0: Bad configuration option: stricthostkeychecking" >&2; exit 255
`)
	if _, err := Run(context.Background(), Options{Dest: "alice@lab", Timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if n := len(runs(t, calls)); n != 1 {
		t.Errorf("ssh ran %d times, want 1", n)
	}
}
