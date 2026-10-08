package guard

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var pin = Pin{Kubeconfig: "/case/kubeconfig", Context: "case", Cluster: "frozen", User: "nobody", Server: "http://127.0.0.1:5000/kubernetes"}

const pinned = "--kubeconfig=/case/kubeconfig --context=case --cluster=frozen --server=http://127.0.0.1:5000/kubernetes --user=nobody"

func decide(args ...string) (string, error) {
	out, err := Kubectl(args, pin.Kubeconfig, pin)
	return strings.Join(out, " "), err
}

func TestAnInvestigationRunsPointedAtTheCase(t *testing.T) {
	for _, tc := range []struct{ args, want string }{
		{"get pods -A", "get pods -A " + pinned},
		{"get pods -n shop -o wide --sort-by=.metadata.name -l app=x", "get pods -n shop -o wide --sort-by=.metadata.name -l app=x " + pinned},
		{"-n shop get pods -ojson", "-n shop get pods -ojson " + pinned},
		{"--namespace shop describe pod x", "--namespace shop describe pod x " + pinned},
		{"logs -f deploy/cache --since=5m --tail=3 -c main", "logs -f deploy/cache --since=5m --tail=3 -c main " + pinned},
		{"logs x --previous", "logs x --previous " + pinned},
		{"get pods -lapp=shops -L tier", "get pods -lapp=shops -L tier " + pinned}, // an s inside a value is not -s
		{"get events -A -w", "get events -A -w " + pinned},
		{"top pod -A", "top pod -A " + pinned},
		{"auth can-i list pods", "auth can-i list pods " + pinned},
		{"rollout history deploy/x -n shop", "rollout history deploy/x -n shop " + pinned},
		// A read, not a wait: `rollout status` would not return while the rollout is stuck.
		{"rollout status deploy/x -n shop", "rollout status deploy/x -n shop " + pinned + " --watch=false"},
		{"rollout status deploy/x --watch=true --timeout=1h", "rollout status deploy/x --watch=true --timeout=1h " + pinned + " --watch=false"},
		{"rollout status deploy/x -- --watch=true", "rollout status deploy/x " + pinned + " --watch=false -- --watch=true"},
		// kubectl's own flags, in the places and spellings it reads them the same way everywhere
		{"--namespace=shop get pods", "--namespace=shop get pods " + pinned}, {"-nshop -v6 get pods", "-nshop -v6 get pods " + pinned},
		{"--request-timeout 5s -n shop rollout history deploy/x", "--request-timeout 5s -n shop rollout history deploy/x " + pinned},
		// formats that are written on the command line and read nothing
		{"get pods -o jsonpath={.items[*].metadata.name}", "get pods -o jsonpath={.items[*].metadata.name} " + pinned},
		{"get pods -o go-template={{.kind}} -o=custom-columns=N:.metadata.name --output=name", "get pods -o go-template={{.kind}} -o=custom-columns=N:.metadata.name --output=name " + pinned},
		{"get pods -l profile=x -o wide", "get pods -l profile=x -o wide " + pinned},
		{"config current-context", "config current-context " + pinned},
		{"api-resources -o name", "api-resources -o name " + pinned},
		{"--help", "--help " + pinned},
		{"", pinned},
		// What follows -- is not kubectl's, and the pin goes before it.
		{"get pods -- --server https://elsewhere -s x", "get pods " + pinned + " -- --server https://elsewhere -s x"},
	} {
		if got, err := decide(strings.Fields(tc.args)...); err != nil || got != tc.want {
			t.Errorf("kubectl %s\n got %s (%v)\nwant %s", tc.args, got, err, tc.want)
		}
	}
}

func TestWhatIsRefused(t *testing.T) {
	for _, tc := range []struct{ args, why string }{
		// another cluster, in every spelling kubectl accepts
		{"get pods --kubeconfig /home/me/.kube/config", "--kubeconfig"}, {"get pods --kubeconfig=/home/me/.kube/config", "--kubeconfig"},
		{"--context prod get pods", "--context"}, {"get pods --context=prod", "--context"}, {"get pods --cluster=prod", "--cluster"},
		{"get pods --server https://prod", "--server"}, {"get pods --server=https://prod", "--server"},
		{"get pods -s https://prod", "-s"}, {"get pods -shttps://prod", "-s"}, {"get pods -s=https://prod", "-s"},
		{"get pods -As https://prod", "-s in -As"}, {"get pods -As=https://prod", "-s in -As=https://prod"}, {"get pods -ws https://prod", "-s in -ws"},
		// another identity
		{"get pods --user=admin", "--user"}, {"get pods --token=abc", "--token"}, {"get pods --as=system:admin", "--as"},
		{"get pods --as-group=system:masters", "--as-group"}, {"get pods --as-uid=0", "--as-uid"},
		{"get pods --client-certificate=/x", "--client-certificate"}, {"get pods --client-key=/x", "--client-key"},
		{"get pods --certificate-authority=/x", "--certificate-authority"}, {"get pods --insecure-skip-tls-verify", "--insecure-skip-tls-verify"},
		{"get pods --tls-server-name=prod", "--tls-server-name"}, {"get pods --proxy-url=http://x", "--proxy-url"},
		// changing the cluster, or the kubeconfig itself
		{"delete pod x", "not a read-only verb"}, {"apply x.yaml", "not a read-only verb"}, {"exec x -- sh", "not a read-only verb"},
		{"edit deploy/x", "not a read-only verb"}, {"scale deploy/x --replicas=0", "not a read-only verb"}, {"port-forward x 1:1", "not a read-only verb"},
		{"cp x:/a /b", "not a read-only verb"}, {"debug node/x", "not a read-only verb"}, {"run x --image=y", "not a read-only verb"},
		{"config set clusters.hack.server https://10.0.0.1", "only current-context, get-contexts"},
		{"config use-context prod", "only current-context, get-contexts"}, {"config view --raw", "only current-context, get-contexts"}, {"config", "only current-context, get-contexts"},
		{"rollout restart deploy/x", "only status, history"}, {"rollout undo deploy/x", "only status, history"}, {"auth reconcile x", "only can-i, whoami"},
		// a plugin is somebody else's program with its own idea of flags
		{"crust-gather collect", "not a read-only verb"}, {"-o json get pods", "-o stands before the verb"},
		// The command kubectl runs is not the first word when an unknown flag stands before it: kubectl
		// takes the next word for that flag's value. Each of these read as a read and ran as a write.
		{"-l version delete pods -A", "-l stands before the verb"},
		{"--field-manager get annotate pod x a=b", "--field-manager stands before the verb"},
		{"--field-manager get create configmap leak --from-file=/etc/passwd", "--field-manager stands before the verb"},
		{"-A get pods", "-A stands before the verb"}, {"--all-namespaces get pods", "--all-namespaces stands before the verb"},
		{"rollout --field-manager history restart deploy/x", "only status, history"},
		{"rollout -n shop history deploy/x", "only status, history"},
		{"config --field-manager current-context view --raw", "only current-context, get-contexts"},
		{"config -o current-context view", "only current-context, get-contexts"},
		{"auth --field-manager can-i reconcile x", "only can-i, whoami"},
		// a path is not a resource: it can be a proxy to a node, a pod or a service
		{"get --raw /api/v1/nodes/worker/proxy/metrics/cadvisor", "--raw"}, {"get --raw=/api/v1/namespaces/shop/pods/x:8080/proxy/admin/reset", "--raw"},
		// an output format whose template is a file prints that file
		{"get ns -o go-template-file=/home/me/.kube/config", "output format reads a file"}, {"get ns -ogo-template-file=/x", "output format reads a file"},
		{"get ns -o=jsonpath-file=/x", "output format reads a file"}, {"get ns -o custom-columns-file=/x", "output format reads a file"},
		{"get ns --output templatefile --template=/x", "output format reads a file"}, {"get ns --output=jsonpath-file=/x", "output format reads a file"},
		{"get pods -Ao go-template-file=/x", "output format reads a file"}, {"rollout history deploy/x -o go-template-file=/x", "output format reads a file"},
		{"auth whoami -o jsonpath-file=/x", "output format reads a file"}, {"cluster-info dump -o go-template-file=/x", "output format reads a file"},
		// files on this machine
		{"get -f /etc/passwd", "-f in -f reads files"}, {"get --filename=/etc/passwd", "--filename"}, {"describe -k /dir", "-k in -k"},
		{"get pods -Af /etc/passwd", "-f in -Af"}, {"cluster-info dump --output-directory=/tmp/x", "--output-directory"}, {"get pods --cache-dir=/tmp/x", "--cache-dir"},
	} {
		got, err := decide(strings.Fields(tc.args)...)
		if err == nil || !strings.Contains(err.Error(), tc.why) || !strings.HasPrefix(err.Error(), "kubectl refused by lapilli-case: ") {
			t.Errorf("kubectl %s: ran as %q (%v), want a refusal naming %q", tc.args, got, err, tc.why)
		}
	}
	if _, err := Kubectl([]string{"get", "pods"}, "/home/me/.kube/config", pin); err == nil {
		t.Error("kubectl ran with another KUBECONFIG")
	}
	if _, err := Kubectl([]string{"get", "pods"}, "", pin); err == nil {
		t.Error("kubectl ran with no KUBECONFIG, which means the default one")
	}
}

// The second control: whatever the refusals let through, the case's destination comes last, and
// kubectl takes the last value of a flag.
func TestThePinComesAfterEverythingTheAgentWrote(t *testing.T) {
	for _, args := range []string{"get pods -A", "get pods -n x -o json", "logs x", "get pods -- -s y"} {
		out, err := Kubectl(strings.Fields(args), pin.Kubeconfig, pin)
		if err != nil {
			t.Fatal(err)
		}
		end := len(out)
		for i, a := range out {
			if a == "--" {
				end = i
			}
		}
		want := strings.Fields(pinned)
		if end < len(want) || !reflect.DeepEqual(out[end-len(want):end], want) {
			t.Errorf("kubectl %s ran as %v; the pin is not the last thing kubectl reads", args, out)
		}
	}
	// A kubeconfig with no user still pins everything it has.
	anonymous := pin
	anonymous.User = ""
	if out, _ := Kubectl([]string{"get", "pods"}, pin.Kubeconfig, anonymous); strings.Contains(strings.Join(out, " "), "--user") {
		t.Errorf("an empty user was pinned: %v", out)
	}
}

func TestLoadPinReadsTheCurrentContextOnly(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		path := filepath.Join(dir, "kubeconfig")
		os.WriteFile(path, []byte(body), 0o600)
		return path
	}
	// The shape crust-gather writes.
	path := write("clusters:\n- name: kubernetes\n  cluster:\n    server: http://127.0.0.1:50354/kubernetes\nusers:\n- name: kubernetes\ncontexts:\n- name: kubernetes\n  context:\n    cluster: kubernetes\n    user: kubernetes\ncurrent-context: kubernetes\n")
	got, err := LoadPin(path)
	if want := (Pin{Kubeconfig: path, Context: "kubernetes", Cluster: "kubernetes", User: "kubernetes", Server: "http://127.0.0.1:50354/kubernetes"}); err != nil || got != want {
		t.Errorf("%+v (%v)", got, err)
	}
	// Two clusters in one file: only the current one is where kubectl may go.
	path = write("current-context: kind\ncontexts:\n- {name: prod, context: {cluster: prod, user: me}}\n- {name: kind, context: {cluster: kind, user: kind}}\nclusters:\n- {name: prod, cluster: {server: 'https://prod'}}\n- {name: kind, cluster: {server: 'https://127.0.0.1:6443'}}\n")
	if got, err := LoadPin(path); err != nil || got.Server != "https://127.0.0.1:6443" || got.Cluster != "kind" || got.User != "kind" {
		t.Errorf("%+v (%v)", got, err)
	}
	for name, body := range map[string]string{
		"no current context":              "contexts:\n- {name: a, context: {cluster: a}}\nclusters:\n- {name: a, cluster: {server: 'https://a'}}\n",
		"a context that names no cluster": "current-context: a\ncontexts:\n- {name: a, context: {}}\n",
		"a cluster with no server":        "current-context: a\ncontexts:\n- {name: a, context: {cluster: a}}\nclusters:\n- {name: a, cluster: {}}\n",
		"not YAML":                        "{{{",
	} {
		if got, err := LoadPin(write(body)); err == nil {
			t.Errorf("%s gave %+v", name, got)
		}
	}
	if _, err := LoadPin(filepath.Join(dir, "missing")); err == nil {
		t.Error("a kubeconfig that does not exist gave a pin")
	}
}

func TestEnvironDropsWhatRedirectsKubectl(t *testing.T) {
	got := Environ([]string{"PATH=/bin", "KUBECONFIG=/case/kubeconfig", "KUBERNETES_MASTER=https://prod", "KUBERNETES_SERVICE_HOST=10.0.0.1", "KUBECTL_PLUGINS_PATH=/x", "HOME=/h"})
	if want := []string{"PATH=/bin", "KUBECONFIG=/case/kubeconfig", "HOME=/h"}; !reflect.DeepEqual(got, want) {
		t.Errorf("%v", got)
	}
}

// The verb of a command line is found as Kubectl finds it: for what Kubectl refuses as no read, it
// is the word Kubectl names; and a flag before it, in any of kubectl's spellings, does not hide it.
func TestTheVerbIsFoundAsKubectlFindsIt(t *testing.T) {
	for line, want := range map[string]string{
		"get pods": "get", "-n shop get pods": "get", "-nshop get pods": "get", "--namespace=shop get pods": "get", "--namespace shop logs x": "logs",
		"-v6 -n shop auth can-i get pods": "auth", "--request-timeout 5s -n shop rollout history deploy/x": "rollout", "-n=shop config view": "config",
		"-A delete pods": "delete", "": "", "-n": "", "-n shop": "", "-- get pods": "", "logs x -- -s y": "logs", "-n shop -- get": "",
	} {
		if got := Verb(strings.Fields(line)); got != want {
			t.Errorf("the verb of `kubectl %s`: %q, want %q", line, got, want)
		}
	}
	pin := Pin{Kubeconfig: "/case/kubeconfig", Context: "case", Cluster: "frozen", User: "nobody", Server: "http://127.0.0.1:1/kubernetes"}
	// A word that is empty stands where the verb stands and is no read: `kubectl "" delete pod x` was
	// passed on once, the check being for a verb that was not empty, and what kubectl then makes of
	// the words after it was left to kubectl.
	for _, args := range [][]string{{"", "delete", "pod", "x"}, {"-n", "shop", "", "delete", "pods"}, {"", "auth", "can-i", "get", "pods"}, {""}, {"-v6", ""}} {
		if argv, err := Kubectl(args, pin.Kubeconfig, pin); err == nil || !strings.Contains(err.Error(), `"" is not a read-only verb`) {
			t.Errorf("kubectl %q, an empty word for a verb, was passed on as %q (%v)", args, argv, err)
		}
	}
	// A command line with no word at all before its flags end is kubectl saying how it is used.
	for _, args := range [][]string{{}, {"--help"}, {"-v=9"}, {"-n", "shop"}} {
		if _, err := Kubectl(args, pin.Kubeconfig, pin); err != nil {
			t.Errorf("kubectl %q: %v", args, err)
		}
	}
	for _, line := range []string{"delete pod x", "-n shop delete pod get", "-nshop exec p -- get", "--namespace=shop apply get", "-v6 scale deploy/x --replicas=0", "-n shop patch deploy/y get"} {
		_, err := Kubectl(strings.Fields(line), pin.Kubeconfig, pin)
		if verb := Verb(strings.Fields(line)); err == nil || !strings.Contains(err.Error(), `"`+verb+`" is not a read-only verb`) {
			t.Errorf("`kubectl %s`: Verb says %q, Kubectl says %v", line, verb, err)
		}
	}
}
