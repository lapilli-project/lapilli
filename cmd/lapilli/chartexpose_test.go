package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// manifest is a rendered chart's documents, by kind and name.
type manifest map[string]map[string]any

func render(t *testing.T, args ...string) (manifest, string) {
	t.Helper()
	out, err := helmTemplate(t, args...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	m := manifest{}
	for _, doc := range strings.Split(out, "\n---\n") {
		var d map[string]any
		if yaml.Unmarshal([]byte(doc), &d) != nil || d == nil {
			continue
		}
		kind, _ := d["kind"].(string)
		meta, _ := d["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		m[kind+"/"+name] = d
	}
	return m, out
}

// get walks into a document: keys, and indexes into lists.
func dig(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[k]
		case int:
			l, _ := v.([]any)
			if k >= len(l) {
				return nil
			}
			v = l[k]
		}
	}
	return v
}

// args are lapilli's flags in the Deployment.
func (m manifest) args(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, a := range dig(m["Deployment/ops-lapilli"], "spec", "template", "spec", "containers", 0, "args").([]any) {
		out = append(out, a.(string))
	}
	return out
}

func ports(svc map[string]any) []string {
	var out []string
	for _, p := range dig(svc, "spec", "ports").([]any) {
		out = append(out, p.(map[string]any)["name"].(string))
	}
	return out
}

var trackersValues = []string{"--set", "serve.trackers.tokenSecret.name=tasks", "--set", "serve.trackers.tracker=github",
	"--set", "serve.trackers.image=example/tools@sha256:" + strings.Repeat("ab", 32), "--set", "serve.trackers.trackerTokenSecret.name=gh"}

// The release's Service stays a ClusterIP with every port, as jobs and
// Prometheus reach it. serve.service.type puts the UI alone on
// FULLNAME-ui, a LoadBalancer or a NodePort, with the cloud's settings.
// Serving the UI outside the cluster with no sign-in is refused unless
// asked for.
func TestChartService(t *testing.T) {
	m, _ := render(t, append(serveValues, trackersValues...)...)
	svc := m["Service/ops-lapilli"]
	if dig(svc, "spec", "type") != "ClusterIP" || !slices.Equal(ports(svc), []string{"http", "metrics", "incidents"}) || m["Service/ops-lapilli-ui"] != nil {
		t.Errorf("by default: %v", svc)
	}
	probe := func(m manifest, which string) any {
		return dig(m["Deployment/ops-lapilli"], "spec", "template", "spec", "containers", 0, which, "httpGet", "path")
	}
	if probe(m, "readinessProbe") != "/readyz" || probe(m, "livenessProbe") != "/healthz" {
		t.Errorf("probes: %v %v", probe(m, "readinessProbe"), probe(m, "livenessProbe"))
	}
	if m, _ := render(t, append(serveValues, "--set", "serve.readinessPath=/healthz")...); probe(m, "readinessProbe") != "/healthz" {
		t.Error("serve.readinessPath")
	}
	for _, typ := range []string{"LoadBalancer", "NodePort"} {
		if out, err := helmTemplate(t, append(serveValues, "--set", "serve.service.type="+typ)...); err == nil || !strings.Contains(out, "serve.exposeWithoutSignIn") {
			t.Errorf("%s with no sign-in: %v", typ, err)
		}
	}
	lb := append(append([]string{}, serveValues...), append(trackersValues, "--set", "serve.service.type=LoadBalancer", "--set", "serve.exposeWithoutSignIn=true",
		"--set", "serve.service.annotations.service\\.beta\\.kubernetes\\.io/aws-load-balancer-scheme=internal",
		"--set", "serve.service.loadBalancerSourceRanges={203.0.113.0/24}", "--set", "serve.service.loadBalancerClass=example.com/lb",
		"--set", "serve.service.externalTrafficPolicy=Local", "--set", "serve.service.nodePort=30080", "--set", "serve.service.port=8443")...)
	m, _ = render(t, lb...)
	ui := m["Service/ops-lapilli-ui"]
	switch {
	case dig(ui, "spec", "type") != "LoadBalancer", !slices.Equal(ports(ui), []string{"http"}), dig(ui, "spec", "ports", 0, "port") != 8443:
		t.Errorf("the UI's LoadBalancer: %v", ui)
	case dig(ui, "spec", "loadBalancerSourceRanges", 0) != "203.0.113.0/24", dig(ui, "spec", "loadBalancerClass") != "example.com/lb",
		dig(ui, "spec", "externalTrafficPolicy") != "Local", dig(ui, "spec", "ports", 0, "nodePort") != 30080,
		dig(ui, "metadata", "annotations", "service.beta.kubernetes.io/aws-load-balancer-scheme") != "internal":
		t.Errorf("the load balancer's settings: %v", ui)
	}
	if svc := m["Service/ops-lapilli"]; dig(svc, "spec", "type") != "ClusterIP" || !slices.Equal(ports(svc), []string{"http", "metrics", "incidents"}) ||
		dig(svc, "metadata", "annotations") != nil {
		t.Errorf("the release's own Service: %v", svc)
	}
	env := dig(m["CronJob/ops-lapilli-trackers"], "spec", "jobTemplate", "spec", "template", "spec", "containers", 0, "env").([]any)
	if !slices.ContainsFunc(env, func(e any) bool {
		return dig(e, "name") == "LAPILLI_URL" && dig(e, "value") == "http://ops-lapilli:8082"
	}) {
		t.Errorf("the trackers job reaches %v", env)
	}
	if out, err := helmTemplate(t, append(serveValues, "--set", "serve.service.type=ExternalName")...); err == nil || !strings.Contains(out, "ClusterIP, LoadBalancer or NodePort") {
		t.Errorf("ExternalName: %v", err)
	}
	// A plain header on a load balancer of TCP: anyone who reaches it names
	// themselves anyone. Refused, unless only the proxy's addresses reach it
	// and that is said.
	header := append(append([]string{}, serveValues...), "--set", "serve.userHeader=X-Forwarded-Email", "--set", "serve.networkPolicy.enabled=true",
		"--set", "serve.networkPolicy.from[0].ipBlock.cidr=0.0.0.0/0", "--set", "serve.roles.admins={boss@example.com}")
	for name, more := range map[string][]string{
		"a LoadBalancer":            {"--set", "serve.service.type=LoadBalancer", "--set", "serve.service.externalTrafficPolicy=Local"},
		"a NodePort":                {"--set", "serve.service.type=NodePort"},
		"a NodePort, said so":       {"--set", "serve.service.type=NodePort", "--set", "serve.service.sourceRangesAreTheProxy=true"},
		"no ranges, said so":        {"--set", "serve.service.type=LoadBalancer", "--set", "serve.service.sourceRangesAreTheProxy=true"},
		"ranges, not said":          {"--set", "serve.service.type=LoadBalancer", "--set", "serve.service.loadBalancerSourceRanges={10.1.0.0/24}"},
		"othersKeptOut, a NodePort": {"--set", "serve.service.type=NodePort", "--set", "serve.networkPolicy.othersKeptOut=true"},
	} {
		if out, err := helmTemplate(t, append(append([]string{}, header...), more...)...); err == nil || !strings.Contains(out, "plain header") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if out, err := helmTemplate(t, append(header, "--set", "serve.service.type=LoadBalancer", "--set", "serve.service.loadBalancerSourceRanges={10.1.0.0/24}",
		"--set", "serve.service.sourceRangesAreTheProxy=true")...); err != nil {
		t.Errorf("the proxy's own ranges, said so: %v\n%s", err, out)
	}
}

// An Ingress or an HTTPRoute serves the UI at a host: --external-url and
// --allowed-hosts are worked out from it, so the host check and the
// sign-in's redirect hold, unless serve.externalURL is given.
func TestChartIngressAndRoute(t *testing.T) {
	signedIn := append(append([]string{}, serveValues...), "--set", "serve.userHeader=X-Forwarded-Email", "--set", "serve.networkPolicy.othersKeptOut=true")
	m, _ := render(t, append(signedIn, "--set", "serve.ingress.enabled=true", "--set", "serve.ingress.className=nginx",
		"--set", "serve.ingress.hosts={*.example.com,lapilli.example.com,ui.example.org}", "--set", "serve.ingress.tls[0].secretName=lapilli-tls",
		"--set", "serve.ingress.tls[0].hosts={lapilli.example.com}", "--set", "serve.ingress.annotations.example\\.com/x=y",
		"--set", "serve.allowedHosts={proxy.example.net}")...)
	ing := m["Ingress/ops-lapilli"]
	switch {
	case ing == nil:
		t.Fatal("no Ingress")
	case dig(ing, "spec", "ingressClassName") != "nginx", dig(ing, "spec", "tls", 0, "secretName") != "lapilli-tls",
		dig(ing, "metadata", "annotations", "example.com/x") != "y":
		t.Errorf("the Ingress: %v", ing)
	case dig(ing, "spec", "rules", 1, "host") != "lapilli.example.com",
		dig(ing, "spec", "rules", 1, "http", "paths", 0, "backend", "service", "name") != "ops-lapilli",
		dig(ing, "spec", "rules", 1, "http", "paths", 0, "backend", "service", "port", "name") != "http",
		dig(ing, "spec", "rules", 1, "http", "paths", 0, "pathType") != "Prefix":
		t.Errorf("its rules: %v", dig(ing, "spec", "rules"))
	}
	args := m.args(t)
	if !slices.Contains(args, "--external-url=https://lapilli.example.com") || !slices.Contains(args, "--allowed-hosts=proxy.example.net,lapilli.example.com,ui.example.org") {
		t.Errorf("from the Ingress: %v", args)
	}
	// https unless serve.ingress.https says not (TLS ended at a load
	// balancer in front leaves the Ingress no tls); serve.externalURL wins.
	m, _ = render(t, append(signedIn, "--set", "serve.ingress.enabled=true", "--set", "serve.ingress.hosts={lapilli.example.com}")...)
	if !slices.Contains(m.args(t), "--external-url=https://lapilli.example.com") {
		t.Errorf("no tls: %v", m.args(t))
	}
	m, _ = render(t, append(signedIn, "--set", "serve.ingress.enabled=true", "--set", "serve.ingress.hosts={lapilli.example.com}", "--set", "serve.ingress.https=false")...)
	if !slices.Contains(m.args(t), "--external-url=http://lapilli.example.com") {
		t.Errorf("serve.ingress.https=false: %v", m.args(t))
	}
	for name, more := range map[string][]string{
		"a path":            {"--set", "serve.ingress.enabled=true", "--set", "serve.ingress.hosts={lapilli.example.com}", "--set", "serve.ingress.path=/lapilli"},
		"a path in the URL": {"--set", "serve.externalURL=https://example.com/lapilli"},
	} {
		if out, err := helmTemplate(t, append(append([]string{}, signedIn...), more...)...); err == nil || !strings.Contains(out, "root of its host") {
			t.Errorf("%s: %v", name, err)
		}
	}
	m, _ = render(t, append(signedIn, "--set", "serve.ingress.enabled=true", "--set", "serve.ingress.hosts={lapilli.example.com}", "--set", "serve.externalURL=https://ui.example.com")...)
	if !slices.Contains(m.args(t), "--external-url=https://ui.example.com") {
		t.Errorf("serve.externalURL: %v", m.args(t))
	}
	// A Gateway's route.
	if out, err := helmTemplate(t, append(signedIn, "--set", "serve.httpRoute.enabled=true")...); err == nil || !strings.Contains(out, "parentRefs") {
		t.Errorf("a route with no Gateway: %v", err)
	}
	m, _ = render(t, append(signedIn, "--set", "serve.httpRoute.enabled=true", "--set", "serve.httpRoute.parentRefs[0].name=gw",
		"--set", "serve.httpRoute.parentRefs[0].namespace=gateways", "--set", "serve.httpRoute.hostnames={lapilli.example.com}")...)
	route := m["HTTPRoute/ops-lapilli"]
	if dig(route, "apiVersion") != "gateway.networking.k8s.io/v1" || dig(route, "spec", "parentRefs", 0, "namespace") != "gateways" ||
		dig(route, "spec", "hostnames", 0) != "lapilli.example.com" || dig(route, "spec", "rules", 0, "backendRefs", 0, "name") != "ops-lapilli" ||
		dig(route, "spec", "rules", 0, "backendRefs", 0, "port") != 8080 {
		t.Errorf("the HTTPRoute: %v", route)
	}
	if args := m.args(t); !slices.Contains(args, "--external-url=https://lapilli.example.com") || !slices.Contains(args, "--allowed-hosts=lapilli.example.com") {
		t.Errorf("from the route: %v", args)
	}
	m, _ = render(t, append(signedIn, "--set", "serve.httpRoute.enabled=true", "--set", "serve.httpRoute.parentRefs[0].name=gw",
		"--set", "serve.httpRoute.hostnames={lapilli.localhost}", "--set", "serve.httpRoute.https=false")...)
	if !slices.Contains(m.args(t), "--external-url=http://lapilli.localhost") {
		t.Errorf("an http listener: %v", m.args(t))
	}
	// Exposed with no sign-in: refused.
	if out, err := helmTemplate(t, append(serveValues, "--set", "serve.ingress.enabled=true")...); err == nil || !strings.Contains(out, "exposeWithoutSignIn") {
		t.Errorf("an Ingress with no sign-in: %v", err)
	}
}

// The sign-in of lapilli's own: its flags, its Secrets mounted, and the
// rules it is given whole by. Roles and changes take it as they take
// userHeader, without the NetworkPolicy plain headers need.
func TestChartSignIn(t *testing.T) {
	oidc := append(append([]string{}, serveValues...), "--set", "serve.signIn.oidc.issuer=https://idp.example.com", "--set", "serve.signIn.oidc.clientId=lapilli",
		"--set", "serve.externalURL=https://lapilli.example.com", "--set", "serve.roles.viewers={*@example.com}")
	m, _ := render(t, append(oidc, "--set", "serve.signIn.oidc.clientSecret.name=lapilli-oidc", "--set", "serve.signIn.oidc.scopes={email,groups}",
		"--set", "serve.signIn.sessionMaxAge=8h", "--set", "serve.signIn.sessionKeySecret.name=lapilli-session", "--set", "serve.signIn.userClaim=preferred_username",
		"--set", "serve.roles.admins={group:ops}", "--set", "serve.changes.enabled=true", "--set", "serve.signIn.oidc.hostedDomains={example.com}",
		"--set", "serve.signIn.oidc.trustedClients={cli}", "--set", "serve.signIn.oidc.bearer=false")...)
	args := m.args(t)
	for _, want := range []string{"--oidc-issuer=https://idp.example.com", "--oidc-client-id=lapilli", "--oidc-client-secret-file=/var/run/lapilli/oidc/client-secret",
		"--oidc-scopes=email,groups", "--session-max-age=8h", "--session-key-file=/var/run/lapilli/session-key/session-key",
		"--user-claim=preferred_username", "--groups-claim=groups", "--admins=group:ops", "--changes", "--viewers=*@example.com",
		"--oidc-hosted-domain=example.com", "--oidc-trusted-clients=cli", "--oidc-bearer=false"} {
		if !slices.Contains(args, want) {
			t.Errorf("no %s in %v", want, args)
		}
	}
	vols := dig(m["Deployment/ops-lapilli"], "spec", "template", "spec", "volumes").([]any)
	for _, want := range []string{"lapilli-oidc", "lapilli-session"} {
		if !slices.ContainsFunc(vols, func(v any) bool { return dig(v, "secret", "secretName") == want }) {
			t.Errorf("%s isn't mounted: %v", want, vols)
		}
	}
	m, _ = render(t, append(append([]string{}, serveValues...), "--set", "serve.signIn.proxyJWT.kind=aws-alb",
		"--set", "serve.signIn.proxyJWT.albArn=arn:aws:elasticloadbalancing:eu-west-1:123456789012:loadbalancer/app/lapilli/0123456789abcdef",
		"--set", "serve.signIn.groupsClaim=", "--set", "serve.roles.editors={*}", "--set", "serve.roles.viewers={group:ops}")...)
	args = m.args(t)
	for _, want := range []string{"--proxy-jwt=aws-alb", "--proxy-jwt-alb-arn=arn:aws:elasticloadbalancing:eu-west-1:123456789012:loadbalancer/app/lapilli/0123456789abcdef",
		"--user-claim=email", "--groups-claim=", "--editors=*", "--viewers=group:ops"} {
		if !slices.Contains(args, want) {
			t.Errorf("no %s in %v", want, args)
		}
	}
	if slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--oidc") || strings.HasPrefix(a, "--session") }) {
		t.Errorf("OIDC's flags with a proxy's JWT: %v", args)
	}
	// Each refused, with what to do.
	iap := []string{"--set", "serve.signIn.proxyJWT.kind=google-iap", "--set", "serve.signIn.proxyJWT.audience=/projects/1/global/backendServices/2"}
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"two sign-ins":         {append(append([]string{}, oidc...), iap...), "give one"},
		"OIDC and a header":    {append(append([]string{}, oidc...), "--set", "serve.userHeader=X-Forwarded-Email", "--set", "serve.networkPolicy.othersKeptOut=true"), "give one"},
		"no client":            {append(append([]string{}, serveValues...), "--set", "serve.signIn.oidc.issuer=https://idp.example.com", "--set", "serve.externalURL=https://l.example.com"), "clientId"},
		"no URL":               {append(append([]string{}, serveValues...), "--set", "serve.signIn.oidc.issuer=https://idp.example.com", "--set", "serve.signIn.oidc.clientId=x"), "externalURL"},
		"sessions, no OIDC":    {append(append([]string{}, serveValues...), append(iap, "--set", "serve.signIn.sessionMaxAge=1h")...), "sessions"},
		"a kind unknown":       {append(append([]string{}, serveValues...), "--set", "serve.signIn.proxyJWT.kind=basic"), "google-iap, aws-alb or jwks"},
		"everyone an admin":    {append(append([]string{}, oidc...), "--set", "serve.everyoneAdmin=true"), "serve.roles.admins"},
		"a groups header":      {append(append([]string{}, oidc...), "--set", "serve.roles.groupsHeader=X-Forwarded-Groups"), "groupsClaim"},
		"roles, no sign-in":    {append(append([]string{}, serveValues...), "--set", "serve.roles.admins={a@example.com}"), "needs a sign-in"},
		"changes, no sign-in":  {append(append([]string{}, serveValues...), "--set", "serve.changes.enabled=true"), "needs a sign-in"},
		"a route, no sign-in":  {append(append([]string{}, serveValues...), "--set", "serve.httpRoute.enabled=true", "--set", "serve.httpRoute.parentRefs[0].name=gw"), "exposeWithoutSignIn"},
		"networkPolicy, empty": {append(append([]string{}, oidc...), "--set", "serve.networkPolicy.enabled=true"), "ingress controller"},
		"no one let in":        {append(append([]string{}, serveValues...), iap...), "lets in only those a role names"},
		"'*', no viewers":      {append(append([]string{}, serveValues...), append(iap, "--set", "serve.roles.admins={*}")...), "'*'"},
		"'*', viewers '*'":     {append(append([]string{}, oidc...), "--set", "serve.roles.viewers={*}", "--set", "serve.roles.editors={*}"), "'*'"},
		"OIDC over http":       {append(append([]string{}, oidc...), "--set", "serve.externalURL=http://lapilli.example.com"), "in the clear"},
	} {
		if out, err := helmTemplate(t, c.args...); err == nil || !strings.Contains(out, c.want) {
			t.Errorf("%s: %v\n%s", name, err, out)
		}
	}
	// Exposed and signed in, with a NetworkPolicy letting the ingress
	// controller in to the UI's port alone.
	m, _ = render(t, append(oidc, "--set", "serve.ingress.enabled=true", "--set", "serve.ingress.hosts={lapilli.example.com}", "--set", "serve.networkPolicy.enabled=true",
		"--set", "serve.networkPolicy.from[0].namespaceSelector.matchLabels.kubernetes\\.io/metadata\\.name=ingress-nginx")...)
	np := m["NetworkPolicy/ops-lapilli"]
	if dig(np, "spec", "ingress", 0, "from", 0, "namespaceSelector", "matchLabels", "kubernetes.io/metadata.name") != "ingress-nginx" ||
		dig(np, "spec", "ingress", 0, "ports", 0, "port") != "http" || len(dig(np, "spec", "ingress").([]any)) != 1 {
		t.Errorf("the NetworkPolicy: %v", np)
	}
}

// An upgrade with --reuse-values keeps the old release's values, without
// the keys this chart added: it renders as with their defaults.
func TestChartReuseValues(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("no helm")
	}
	src := filepath.Join("..", "..", "charts", "lapilli")
	dst := t.TempDir()
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(src, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := yaml.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	s := v["serve"].(map[string]any)
	for _, k := range []string{"service", "ingress", "httpRoute", "signIn", "exposeWithoutSignIn", "readinessPath"} {
		delete(s, k)
	}
	delete(s["roles"].(map[string]any), "viewers")
	old, _ := yaml.Marshal(v)
	if err := os.WriteFile(filepath.Join(dst, "values.yaml"), old, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, more := range [][]string{nil, {"--set", "serve.userHeader=X-Forwarded-Email", "--set", "serve.networkPolicy.othersKeptOut=true", "--set", "serve.roles.admins={a@example.com}"}} {
		cmd := exec.Command("helm", append([]string{"template", "ops", dst, "--set", "mode=serve", "--set", "prometheus.url=http://p:9090", "--set", "rules.configMaps={r}"}, more...)...)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+filepath.Join(t.TempDir(), "none"))
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "path: /readyz") || strings.Contains(string(out), "kind: Ingress") {
			t.Errorf("0.28.1's values %v: %v\n%s", more, err, out)
		}
	}
}

// With serve.signIn.oidc, the chart makes the key sessions are signed with,
// a Secret kept across upgrades, unless one of the user's is named.
func TestChartSessionKey(t *testing.T) {
	oidc := append(append([]string{}, serveValues...), "--set", "serve.signIn.oidc.issuer=https://idp.example.com", "--set", "serve.signIn.oidc.clientId=lapilli",
		"--set", "serve.externalURL=https://lapilli.example.com", "--set", "serve.roles.viewers={*@example.com}")
	m, _ := render(t, oidc...)
	sec := m["Secret/ops-lapilli-session-key"]
	if sec == nil || dig(sec, "metadata", "annotations", "helm.sh/resource-policy") != "keep" || len(dig(sec, "data", "session-key").(string)) < 60 {
		t.Fatalf("the session key's Secret: %v", sec)
	}
	if !slices.Contains(m.args(t), "--session-key-file=/var/run/lapilli/session-key/session-key") {
		t.Errorf("not used: %v", m.args(t))
	}
	m, _ = render(t, append(oidc, "--set", "serve.signIn.sessionKeySecret.name=mine", "--set", "serve.signIn.sessionKeySecret.key=k")...)
	if m["Secret/ops-lapilli-session-key"] != nil || !slices.Contains(m.args(t), "--session-key-file=/var/run/lapilli/session-key/k") {
		t.Errorf("the user's own: %v", m.args(t))
	}
	m, _ = render(t, append(oidc, "--set", "serve.signIn.sessionKeySecret.create=false")...)
	if m["Secret/ops-lapilli-session-key"] != nil || slices.ContainsFunc(m.args(t), func(a string) bool { return strings.HasPrefix(a, "--session-key-file") }) {
		t.Errorf("none made: %v", m.args(t))
	}
	if m, _ := render(t, serveValues...); m["Secret/ops-lapilli-session-key"] != nil {
		t.Error("a session key with no sign-in")
	}
}
