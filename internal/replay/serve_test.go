package replay

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lapilli-project/lapilli/internal/freeze"
	"github.com/lapilli-project/lapilli/internal/guard"
)

// The snapshot server is someone else's binary, and no test here has it. This stands in for it: the
// test binary, run again as a child in its place, serves one pod's log the way the real one does —
// a line that has no time is given the time the server took its files up, which is some time after
// the case began to be served and is the same at every asking.
func TestMain(m *testing.M) {
	if os.Getenv("LAPILLI_TEST_SNAPSHOT_SERVER") == "1" {
		standInSnapshotServer(os.Args[1:])
		return
	}
	os.Exit(m.Run())
}

func standInSnapshotServer(args []string) { // serve -a <snapshot> -s <address> -k <kubeconfig>
	flags := map[string]string{}
	for i := 1; i+1 < len(args); i += 2 {
		flags[args[i]] = args[i+1]
	}
	tookUp := time.Now().UTC()
	config := fmt.Sprintf("apiVersion: v1\nkind: Config\ncurrent-context: snapshot\ncontexts:\n- name: snapshot\n  context: {cluster: snapshot, user: snapshot}\n"+
		"clusters:\n- name: snapshot\n  cluster: {server: %q}\nusers:\n- name: snapshot\n  user: {}\n", "http://"+flags["-s"]+"/kubernetes")
	if err := os.WriteFile(flags["-k"], []byte(config), 0o600); err != nil {
		os.Exit(1)
	}
	http.ListenAndServe(flags["-s"], http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/kubernetes/version":
			io.WriteString(w, `{"major":"1","minor":"37"}`)
		case strings.HasSuffix(r.URL.Path, "/pods/gone/log") && r.URL.Query().Get("timestamps") == "true":
			io.WriteString(w, tookUp.Format("2006-01-02T15:04:05.000000000Z07:00")+" unable to retrieve container logs for containerd://abc")
		default:
			http.NotFound(w, r)
		}
	}))
}

// A case served from end to end, with that stand-in behind it: unpacked, the snapshot server
// started, the front before it, and a kubeconfig that names the front. Through all of it, the line
// the snapshot holds without a time comes back without the time the snapshot server gave it.
func TestServingACaseTakesOffTheTimesTheSnapshotServerMakesUp(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	standIn := filepath.Join(t.TempDir(), "snapshot-server")
	if err := os.WriteFile(standIn, []byte("#!/bin/sh\nLAPILLI_TEST_SNAPSHOT_SERVER=1 exec \""+self+"\" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAPILLI_CRUST_GATHER", standIn)

	snapshot, caseDir := t.TempDir(), filepath.Join(t.TempDir(), "case")
	pod := filepath.Join(snapshot, "namespaces", "shop", "v1", "pod")
	if err := os.MkdirAll(pod, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(pod, "gone.yaml"), []byte("apiVersion: v1\nkind: Pod\nmetadata: {name: gone, namespace: shop}\n"), 0o644)
	spec := filepath.Join(t.TempDir(), "case.yaml")
	os.WriteFile(spec, []byte("id: served\nprompt: \"why?\"\nexpected: [\"x\"]\nmust_not: [\"y\"]\nspecificity: gone\ndecoys: [NetworkPolicy]\nevidence: ['name: gone']\n"), 0o644)
	if _, _, err := freeze.Pack(spec, snapshot, caseDir, float64(time.Now().Add(-time.Hour).Unix()), nil); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	s, err := Serve(ctx, caseDir, "", self)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pin, err := guard.LoadPin(s.Env["KUBECONFIG"])
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"?container=app", "?container=app&timestamps=true", "?container=app&tailLines=0"} {
		resp, err := http.Get(pin.Server + "/api/v1/namespaces/shop/pods/gone/log" + query)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "unable to retrieve container logs for containerd://abc" {
			t.Errorf("logs%s through a served case: %d %q", query, resp.StatusCode, body)
		}
	}
}
