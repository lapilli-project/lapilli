package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var backtestValues = []string{"--set", "serve.backtest.enabled=true", "--set", "serve.backtest.trust.configMap.name=lapilli-backtest-trust"}

// serve.backtest opens the port that takes pull request backtests from CI
// jobs (docs/design/backtests.md): its flags, the trust file and the plain
// tokens mounted, a port of the pod's, of FULLNAME and of
// FULLNAME-backtest, a ClusterIP of its own, and a NetworkPolicy rule
// letting in serve.backtest.from to it alone. Off, nothing of it is
// rendered.
func TestChartBacktest(t *testing.T) {
	for _, args := range [][]string{serveValues, {"--set", "prometheus.url=http://prometheus:9090", "--set", "rules.configMaps={rules}", "--set", "impact=up"}} {
		if out, err := helmTemplate(t, args...); err != nil || strings.Contains(out, "backtest") {
			t.Errorf("by default (%v): %v\n%s", args, err, out)
		}
	}
	on := append(append(append([]string{}, serveValues...), backtestValues...),
		"--set", "serve.backtest.tokens[0].secretName=lapilli-backtest-azure", "--set", "serve.backtest.tokens[1].secretName=lapilli-backtest-azure",
		"--set", "serve.backtest.tokens[1].key=second", "--set", "serve.backtest.tokens[2].secretName=lapilli-backtest-gitea", "--set", "serve.backtest.tokens[2].key=token",
		"--set", "serve.backtest.share=0.3", "--set", "serve.backtest.queue=50", "--set", "serve.backtest.timeout=30m",
		"--set", "serve.networkPolicy.enabled=true", "--set", "serve.networkPolicy.from[0].podSelector.matchLabels.app=proxy",
		"--set", "serve.backtest.from[0].namespaceSelector.matchLabels.kubernetes\\.io/metadata\\.name=ci-runners")
	m, _ := render(t, on...)
	args := m.args(t)
	for _, want := range []string{"--backtest-listen=:8084", "--backtest-trust=/etc/lapilli/backtest/trust.yaml", "--backtest-share=0.3",
		"--backtest-queue=50", "--backtest-timeout=30m"} {
		if !slices.Contains(args, want) {
			t.Errorf("no %s in %v", want, args)
		}
	}
	pod := dig(m["Deployment/ops-lapilli"], "spec", "template", "spec")
	c := dig(pod, "containers", 0)
	if !slices.ContainsFunc(dig(c, "ports").([]any), func(p any) bool { return dig(p, "name") == "backtest" && dig(p, "containerPort") == 8084 }) {
		t.Errorf("the container's ports: %v", dig(c, "ports"))
	}
	mounts := map[string]string{}
	for _, v := range dig(c, "volumeMounts").([]any) {
		if dig(v, "readOnly") == true {
			mounts[dig(v, "name").(string)] = dig(v, "mountPath").(string)
		}
	}
	vols := map[string]any{}
	for _, v := range dig(pod, "volumes").([]any) {
		vols[dig(v, "name").(string)] = v
	}
	// Each Secret once, its keys together, at a path a trust entry's
	// token_file names: /var/run/lapilli/backtest/tokens/SECRET/KEY.
	switch {
	case mounts["backtest-trust"] != "/etc/lapilli/backtest", dig(vols["backtest-trust"], "configMap", "name") != "lapilli-backtest-trust":
		t.Errorf("the trust file: %v %v", mounts, vols["backtest-trust"])
	case mounts["backtest-token-0"] != "/var/run/lapilli/backtest/tokens/lapilli-backtest-azure",
		dig(vols["backtest-token-0"], "secret", "secretName") != "lapilli-backtest-azure",
		dig(vols["backtest-token-0"], "secret", "items", 0, "key") != "token", dig(vols["backtest-token-0"], "secret", "items", 0, "path") != "token",
		dig(vols["backtest-token-0"], "secret", "items", 1, "key") != "second", dig(vols["backtest-token-0"], "secret", "items", 2) != nil:
		t.Errorf("the first Secret's tokens: %v %v", mounts, vols["backtest-token-0"])
	case mounts["backtest-token-1"] != "/var/run/lapilli/backtest/tokens/lapilli-backtest-gitea",
		dig(vols["backtest-token-1"], "secret", "secretName") != "lapilli-backtest-gitea", vols["backtest-token-2"] != nil:
		t.Errorf("the second Secret's: %v %v", mounts, vols["backtest-token-1"])
	}
	if svc := m["Service/ops-lapilli"]; !slices.Equal(ports(svc), []string{"http", "metrics", "backtest"}) || dig(svc, "spec", "ports", 2, "port") != 8084 {
		t.Errorf("FULLNAME, with every port: %v", svc)
	}
	bt := m["Service/ops-lapilli-backtest"]
	if dig(bt, "spec", "type") != "ClusterIP" || !slices.Equal(ports(bt), []string{"backtest"}) || dig(bt, "spec", "ports", 0, "port") != 8084 ||
		dig(bt, "spec", "ports", 0, "targetPort") != "backtest" || dig(bt, "spec", "externalTrafficPolicy") != nil {
		t.Errorf("FULLNAME-backtest: %v", bt)
	}
	if m["Ingress/ops-lapilli-backtest"] != nil {
		t.Error("an Ingress no one asked for")
	}
	rules := dig(m["NetworkPolicy/ops-lapilli"], "spec", "ingress").([]any)
	if r := rules[len(rules)-1]; dig(r, "ports", 0, "port") != "backtest" || len(dig(r, "ports").([]any)) != 1 ||
		dig(r, "from", 0, "namespaceSelector", "matchLabels", "kubernetes.io/metadata.name") != "ci-runners" {
		t.Errorf("the NetworkPolicy's rule: %v", rules)
	}
	if dig(rules[0], "ports", 0, "port") != "http" || len(dig(rules[0], "ports").([]any)) != 1 {
		t.Errorf("the UI's rule: %v", rules[0])
	}

	// lapilli serve takes the flags the chart gives it, the trust file at
	// the path it mounts.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "trust.yaml"), []byte("trust:\n  - {name: azure, token_file: /var/run/lapilli/backtest/tokens/lapilli-backtest-azure/token}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "rules.yaml"), []byte("groups: []\n"), 0o644)
	var flags []string
	for _, a := range args[1:] {
		a = strings.Replace(a, "/etc/lapilli/backtest/", dir+"/", 1)
		a = strings.Replace(a, "--rules=/etc/lapilli/rules/0", "--rules="+filepath.Join(dir, "rules.yaml"), 1)
		flags = append(flags, a)
	}
	st, err := readServe(flags, true)
	if err != nil {
		t.Fatalf("serve refuses the chart's flags: %v\n%v", err, flags)
	}
	if c := st.cfg; c.BacktestListen != ":8084" || c.BacktestShare != 0.3 || c.BacktestQueue != 50 || c.BacktestTimeout.Minutes() != 30 || len(c.Trust) != 1 {
		t.Errorf("as serve reads them: %+v", c)
	}

	// Outside the cluster: a load balancer that ends TLS, said so, and an
	// Ingress of its own with tls, the API's paths alone. The UI's host
	// check and URL are the UI's.
	m, _ = render(t, append(append(append([]string{}, serveValues...), backtestValues...),
		"--set", "serve.backtest.service.type=LoadBalancer", "--set", "serve.backtest.service.loadBalancerEndsTLS=true", "--set", "serve.backtest.service.port=443",
		"--set", "serve.backtest.service.loadBalancerClass=service.k8s.aws/nlb", "--set", "serve.backtest.service.loadBalancerSourceRanges={203.0.113.0/24}",
		"--set", "serve.backtest.service.externalTrafficPolicy=Local", "--set", "serve.backtest.service.annotations.example\\.com/tls=yes",
		"--set", "serve.backtest.ingress.enabled=true", "--set", "serve.backtest.ingress.className=nginx",
		"--set", "serve.backtest.ingress.hosts={lapilli-ci.example.com}", "--set", "serve.backtest.ingress.tls[0].secretName=lapilli-ci-tls",
		"--set", "serve.backtest.ingress.tls[0].hosts={lapilli-ci.example.com}")...)
	bt = m["Service/ops-lapilli-backtest"]
	switch {
	case dig(bt, "spec", "type") != "LoadBalancer", !slices.Equal(ports(bt), []string{"backtest"}), dig(bt, "spec", "ports", 0, "port") != 443:
		t.Errorf("the LoadBalancer: %v", bt)
	case dig(bt, "spec", "loadBalancerClass") != "service.k8s.aws/nlb", dig(bt, "spec", "loadBalancerSourceRanges", 0) != "203.0.113.0/24",
		dig(bt, "spec", "externalTrafficPolicy") != "Local", dig(bt, "metadata", "annotations", "example.com/tls") != "yes":
		t.Errorf("its settings: %v", bt)
	}
	if svc := m["Service/ops-lapilli"]; dig(svc, "spec", "type") != "ClusterIP" || m["Service/ops-lapilli-ui"] != nil {
		t.Errorf("the UI's Services: %v", svc)
	}
	ing := m["Ingress/ops-lapilli-backtest"]
	switch {
	case ing == nil:
		t.Fatal("no Ingress")
	case dig(ing, "spec", "ingressClassName") != "nginx", dig(ing, "spec", "tls", 0, "secretName") != "lapilli-ci-tls",
		dig(ing, "spec", "rules", 0, "host") != "lapilli-ci.example.com":
		t.Errorf("the Ingress: %v", ing)
	case dig(ing, "spec", "rules", 0, "http", "paths", 0, "path") != "/v1/backtests", dig(ing, "spec", "rules", 0, "http", "paths", 0, "pathType") != "Prefix",
		dig(ing, "spec", "rules", 0, "http", "paths", 0, "backend", "service", "name") != "ops-lapilli-backtest",
		dig(ing, "spec", "rules", 0, "http", "paths", 0, "backend", "service", "port", "name") != "backtest", dig(ing, "spec", "rules", 0, "http", "paths", 1) != nil:
		t.Errorf("its paths: %v", dig(ing, "spec", "rules"))
	}
	if m["Ingress/ops-lapilli"] != nil || slices.ContainsFunc(m.args(t), func(a string) bool {
		return strings.HasPrefix(a, "--external-url") || strings.HasPrefix(a, "--allowed-hosts")
	}) {
		t.Errorf("the backtests' host taken as the UI's: %v", m.args(t))
	}
	// TLS ended in front of the Ingress, said so.
	m, _ = render(t, append(append(append([]string{}, serveValues...), backtestValues...), "--set", "serve.backtest.ingress.enabled=true",
		"--set", "serve.backtest.ingress.loadBalancerEndsTLS=true", "--set", "serve.backtest.ingress.hosts={lapilli-ci.example.com}")...)
	if ing := m["Ingress/ops-lapilli-backtest"]; ing == nil || dig(ing, "spec", "tls") != nil {
		t.Errorf("TLS at the load balancer in front: %v", ing)
	}

	// Each refused, with what to do.
	with := func(more ...string) []string {
		return append(append(append([]string{}, serveValues...), backtestValues...), more...)
	}
	firings := []string{"--set", "mode=serve", "--set", "serve.firings.tokenSecret.name=firings", "--set", "serve.backtest.enabled=true",
		"--set", "serve.backtest.trust.configMap.name=t"}
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"no trust file":      {append(append([]string{}, serveValues...), "--set", "serve.backtest.enabled=true"), "serve.backtest.trust.configMap.name"},
		"read-only":          {with("--set", "serve.readOnly=true"), "serve.readOnly refuses writes"},
		"the CronJob":        {[]string{"--set", "prometheus.url=http://p:9090", "--set", "rules.configMaps={r}", "--set", "impact=up", "--set", "serve.backtest.enabled=true"}, "set mode: serve"},
		"no history":         {firings, "give prometheus.url or gcpProject"},
		"no rules":           {append(firings, "--set", "prometheus.url=http://p:9090"), "firings copied in alone"},
		"--alerts":           {with("--set", "extraArgs={--alerts}"), "ALERTS series"},
		"no from":            {with("--set", "serve.networkPolicy.enabled=true", "--set", "serve.networkPolicy.from[0].podSelector.matchLabels.app=proxy"), "serve.backtest.from"},
		"an Ingress, no tls": {with("--set", "serve.backtest.ingress.enabled=true", "--set", "serve.backtest.ingress.hosts={lapilli-ci.example.com}"), "only with TLS"},
		"a LoadBalancer":     {with("--set", "serve.backtest.service.type=LoadBalancer"), "loadBalancerEndsTLS"},
		"a NodePort":         {with("--set", "serve.backtest.service.type=NodePort"), "loadBalancerEndsTLS"},
		"an ExternalName":    {with("--set", "serve.backtest.service.type=ExternalName"), "ClusterIP, LoadBalancer or NodePort"},
		"the UI's port":      {with("--set", "serve.backtest.port=8080"), "none of the pod's others"},
		"the metrics port":   {with("--set", "serve.backtest.port=9091"), "none of the pod's others"},
		"a share past":       {with("--set", "serve.backtest.share=0.9"), "0.05 to 0.75"},
		"a share of words":   {with("--set", "serve.backtest.share=half"), "0.05 to 0.75"},
		"a queue past":       {with("--set", "serve.backtest.queue=5000"), "1 to 1000"},
		"a token, no name":   {with("--set", "serve.backtest.tokens[0].key=token"), "secretName"},
		"a token twice":      {with("--set", "serve.backtest.tokens[0].secretName=a", "--set", "serve.backtest.tokens[1].secretName=a", "--set", "serve.backtest.tokens[1].key=token"), "a/token twice"},
		"another port's":     {with("--set", "serve.incidentsAPI.tokenSecret.name=inc", "--set", "serve.backtest.tokens[0].secretName=inc"), "another port's token"},
		"off, but given":     {append(append([]string{}, serveValues...), "--set", "serve.backtest.trust.configMap.name=t"), "go with serve.backtest.enabled"},
		"off, but exposed":   {append(append([]string{}, serveValues...), "--set", "serve.backtest.ingress.enabled=true"), "go with serve.backtest.enabled"},
	} {
		if out, err := helmTemplate(t, c.args...); err == nil || !strings.Contains(out, c.want) {
			t.Errorf("%s: %v\n%s", name, err, out)
		}
	}
}
