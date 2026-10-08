package main

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lapilli-project/lapilli/internal/guard"
	"github.com/lapilli-project/lapilli/internal/replay"
)

// The test binary stands in for lapilli-case when bin/kubectl hands over to it, so the chain an agent
// goes through — the shim on its PATH, the guard, the real kubectl — is run as it is installed.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "guard-kubectl" {
		os.Exit(guardKubectl(os.Args[2:]))
	}
	os.Exit(m.Run())
}

const kubeconfig = "current-context: case\ncontexts:\n- {name: case, context: {cluster: frozen, user: nobody}}\n- {name: prod, context: {cluster: prod, user: me}}\n" +
	"clusters:\n- {name: frozen, cluster: {server: 'http://127.0.0.1:5000/kubernetes'}}\n- {name: prod, cluster: {server: 'https://prod.example'}}\n"

func TestAnAgentsKubectlGoesThroughTheGuardToTheCase(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A directory with a quote and a space in it, as a path may have; and a stand-in for the real
	// kubectl that says what it was run with.
	dir := filepath.Join(t.TempDir(), "it's here")
	real, bin := filepath.Join(dir, "real"), filepath.Join(dir, "bin")
	os.MkdirAll(real, 0o755)
	fake := "#!/bin/sh\nprintf 'ran:'; for a in \"$@\"; do printf ' [%s]' \"$a\"; done; printf ' master=%s\\n' \"${KUBERNETES_MASTER:-unset}\"\n"
	if err := os.WriteFile(filepath.Join(real, "kubectl"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	kc := filepath.Join(dir, "kubeconfig")
	os.WriteFile(kc, []byte(kubeconfig), 0o600)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+real+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := replay.WriteTools(bin, kc, self); err != nil {
		t.Fatal(err)
	}

	run := func(env string, args ...string) (string, int) {
		cmd := exec.Command(filepath.Join(bin, "kubectl"), args...)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+env, "KUBERNETES_MASTER=https://prod.example")
		out, err := cmd.CombinedOutput()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out)), code
	}
	pin := " [--kubeconfig=" + kc + "] [--context=case] [--cluster=frozen] [--server=http://127.0.0.1:5000/kubernetes] [--user=nobody]"

	if out, code := run(kc, "get", "pods", "-A", "-l", "app=a b"); code != 0 || out != "ran: [get] [pods] [-A] [-l] [app=a b]"+pin+" master=unset" {
		t.Errorf("an ordinary read: %q (exit %d)", out, code)
	}
	if out, code := run(kc, "logs", "x", "--", "-s", "y"); code != 0 || out != "ran: [logs] [x]"+pin+" [--] [-s] [y] master=unset" {
		t.Errorf("arguments after --: %q (exit %d)", out, code)
	}
	for _, args := range [][]string{
		{"get", "pods", "--context", "prod"}, {"get", "pods", "-As", "https://prod.example"}, {"get", "pods", "--kubeconfig=/home/me/.kube/config"},
		{"config", "use-context", "prod"}, {"config", "set", "clusters.frozen.server", "https://prod.example"}, {"delete", "pod", "x"},
		{"get", "-f", "/etc/passwd"}, {"", "delete", "pod", "x"}, {"-n", "shop", "", "delete", "pod", "x"},
	} {
		if out, code := run(kc, args...); code != guard.ExitRefused || strings.Contains(out, "ran:") || !strings.HasPrefix(out, "kubectl refused by lapilli-case: ") {
			t.Errorf("kubectl %v reached the real binary: %q (exit %d)", args, out, code)
		}
	}
	if out, code := run("/home/me/.kube/config", "get", "pods"); code != guard.ExitRefused || strings.Contains(out, "ran:") {
		t.Errorf("kubectl ran with another KUBECONFIG: %q (exit %d)", out, code)
	}
	// What a run does not offer its agent, of what is a read: refused by the guard, wherever the
	// namespace stands, and nothing else with it.
	if out, code := run(kc, "-n", "shop", "auth", "can-i", "get", "pods"); code != 0 || !strings.HasPrefix(out, "ran: [-n] [shop] [auth] [can-i] [get] [pods]") {
		t.Errorf("a read, with nothing withheld: %q (exit %d)", out, code)
	}
	t.Setenv(guard.NotOfferedEnv, "auth cluster-info config explain")
	for _, args := range [][]string{{"-n", "shop", "auth", "can-i", "get", "pods"}, {"-nshop", "explain", "get"}, {"cluster-info"}, {"--namespace=shop", "config", "current-context"}} {
		if out, code := run(kc, args...); code != guard.ExitRefused || strings.Contains(out, "ran:") || !strings.Contains(out, "is not offered in this run") {
			t.Errorf("kubectl %v, withheld, reached the real binary: %q (exit %d)", args, out, code)
		}
	}
	if out, code := run(kc, "-n", "shop", "get", "configmap", "config"); code != 0 || !strings.HasPrefix(out, "ran: [-n] [shop] [get] [configmap] [config]") {
		t.Errorf("a ConfigMap called config, with `config` withheld: %q (exit %d)", out, code)
	}
	if out, code := run(kc, "-n", "shop", "delete", "pod", "auth"); code != guard.ExitRefused || !strings.Contains(out, `"delete" is not a read-only verb`) {
		t.Errorf("a write is refused as a write, whatever else is withheld: %q (exit %d)", out, code)
	}
	os.Unsetenv(guard.NotOfferedEnv)
	// The kubeconfig is read at every call: one that has stopped naming a place is refused, not guessed at.
	os.WriteFile(kc, []byte("contexts: []\n"), 0o600)
	if out, code := run(kc, "get", "pods"); code != guard.ExitRefused || strings.Contains(out, "ran:") {
		t.Errorf("kubectl ran with a kubeconfig that names no context: %q (exit %d)", out, code)
	}
	if code := guardKubectl([]string{kc}); code != 2 {
		t.Errorf("the guard run by hand exited %d", code)
	}
}

func TestFlagsMayStandAnywhereAndDoubleDashEndsThem(t *testing.T) {
	type result struct {
		Agent string
		Runs  int
		Pos   []string
	}
	read := func(args ...string) (result, error) {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		var r result
		fs.StringVar(&r.Agent, "agent", "", "")
		fs.IntVar(&r.Runs, "runs", 1, "")
		pos, err := parse(fs, args)
		r.Pos = pos
		return r, err
	}
	for _, tc := range []struct {
		args []string
		want result
	}{
		{[]string{"a", "b", "--agent", "x"}, result{"x", 1, []string{"a", "b"}}},
		{[]string{"--agent", "x", "a", "--runs", "3", "b"}, result{"x", 3, []string{"a", "b"}}},
		{[]string{"a", "--", "-b", "--agent", "c"}, result{"", 1, []string{"a", "-b", "--agent", "c"}}},
		{[]string{"--runs=2", "--", "--runs=9"}, result{"", 2, []string{"--runs=9"}}},
		{nil, result{"", 1, nil}},
	} {
		if got, err := read(tc.args...); err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%v: %+v (%v), want %+v", tc.args, got, err, tc.want)
		}
	}
	if _, err := read("a", "--nope"); err == nil {
		t.Error("an unknown flag after a positional was accepted")
	}
}
