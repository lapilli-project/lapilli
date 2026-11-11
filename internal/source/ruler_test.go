package source

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"

	"lapilli.dev/internal/cloud"
	"lapilli.dev/internal/rules"
)

// Reading a managed Prometheus ruler (docs/design/rulers.md): Mimir's
// answers as Mimir 3.2.4 gave them, run locally; Amazon's and Azure's as
// their API documents show them. None of these ran against an AWS or an
// Azure account.

func TestRulerKind(t *testing.T) {
	for raw, want := range map[string]string{
		"http://mimir:8080/prometheus/config/v1/rules":                       RulerConfig,
		"http://cortex:9009/api/v1/rules":                                    RulerConfig,
		"https://aps.us-east-1.amazonaws.com/workspaces/ws-1":                RulerAmazon,
		"https://vpce-0abc.aps.eu-west-1.vpce.amazonaws.com/workspaces/ws-1": RulerAmazon,
		"https://management.azure.com/subscriptions/sub-1":                   RulerAzure,
		"https://management.azure.com/subscriptions/sub-1/resourceGroups/rg": RulerAzure,
		"https://management.usgovcloudapi.net/subscriptions/sub-1":           RulerAzure,
	} {
		if got, err := RulerKind(raw); got != want || err != nil {
			t.Errorf("%s: %q %v, want %q", raw, got, err, want)
		}
	}
	for raw, says := range map[string]string{
		"https://aps-workspaces.us-east-1.amazonaws.com/workspaces/ws-1": "https://aps.us-east-1.amazonaws.com/workspaces/ws-1",
		"http://aps.us-east-1.amazonaws.com/workspaces/ws-1":             "control plane",
		"https://aps.us-east-1.amazonaws.com/workspaces":                 "control plane",
		"https://monitoring.us-east-1.amazonaws.com":                     "CloudWatch",
		"http://management.azure.com/subscriptions/sub-1":                "subscription",
		"https://management.azure.com/subscriptions/sub-1/x":             "subscription",
		"https://user:pw@mimir/prometheus/config/v1/rules":               "ruler's URL",
		"mimir:8080":             "ruler's URL",
		"http://mimir/rules?x=1": "ruler's URL",
	} {
		if _, err := RulerKind(raw); err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("%s: %v, want one saying %q", raw, err, says)
		}
	}
}

func testdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Mimir's list, as Mimir 3.2.4 answered it: one document a namespace, a
// namespace with slashes in it among them, each rule knowing its
// namespace, and rules of one name in two namespaces told apart by both.
func TestConfigDocs(t *testing.T) {
	base := "http://mimir:8080/prometheus/config/v1/rules"
	docs, passed, err := ConfigDocs(base, testdata(t, "mimir-3.2.4-list.yaml"))
	if len(passed) != 0 {
		t.Errorf("passed over %v", passed)
	}
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range docs {
		got = append(got, d.Namespace+" "+d.Source)
	}
	want := []string{"alloy/payments/rules/0b1c " + base + "/alloy%2Fpayments%2Frules%2F0b1c", "shop " + base + "/shop", "solo " + base + "/solo"}
	if !slices.Equal(got, want) {
		t.Fatalf("documents %q, want %q", got, want)
	}
	rs, err := rules.FromDocs(docs)
	if err != nil {
		t.Fatal(err)
	}
	// The expression as written, not printed again.
	if i := slices.IndexFunc(rs, func(r rules.Rule) bool { return r.Namespace == "shop" && r.Name == "ScrapeSlow" }); i < 0 ||
		rs[i].Expr != `scrape_duration_seconds{job="mimir"}>0.000001` || rs[i].Group != "shop" {
		t.Errorf("ScrapeSlow in shop: %+v", rs)
	}
	TellApart(rs)
	var names []string
	for _, r := range rs {
		names = append(names, r.Name)
	}
	if !slices.Contains(names, "ScrapeSlow [shop/shop, severity=page]") || !slices.Contains(names, "ScrapeSlow [alloy%2Fpayments%2Frules%2F0b1c/shop]") {
		t.Errorf("told apart as %q", names)
	}
	if docs, _, err := ConfigDocs(base, []byte("{}\n")); err != nil || len(docs) != 0 {
		t.Errorf("none: %v %v", docs, err)
	}
	if _, _, err := ConfigDocs(base, []byte("- a\n- b\n")); err == nil {
		t.Error("a list read as namespaces")
	}
}

// to is a transport that sends every request to a stand-in, whatever its
// host, and notes the host asked for.
type to struct {
	ts    *httptest.Server
	mu    sync.Mutex
	hosts []string
}

func (s *to) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.hosts = append(s.hosts, r.URL.Host)
	s.mu.Unlock()
	u, _ := url.Parse(s.ts.URL)
	r = r.Clone(r.Context())
	r.Header.Set("X-Asked-Host", r.URL.Host)
	r.URL.Scheme, r.URL.Host = u.Scheme, u.Host
	return http.DefaultTransport.RoundTrip(r)
}

// A Mimir's or a Cortex's configuration API, read with its token and its
// tenant; a 404 is none.
func TestReadConfig(t *testing.T) {
	ctx := context.Background()
	token := filepath.Join(t.TempDir(), "token")
	os.WriteFile(token, []byte("read-token-0123456789\n"), 0o600)
	list := testdata(t, "mimir-3.2.4-list.yaml")
	var missing atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer read-token-0123456789" || r.Header.Get("X-Scope-OrgID") != "team-a" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		switch {
		case missing.Load():
			http.NotFound(w, r)
		case r.URL.Path == "/api/v1/rules":
			w.Write(list)
		case r.URL.Path == "/api/v1/rules/shop/shop":
			w.Write(testdata(t, "mimir-3.2.4-group.yaml"))
		default:
			http.Error(w, "group does not exist", http.StatusNotFound)
		}
	}))
	defer ts.Close()
	r, err := NewRuler(ts.URL+"/api/v1/rules", "test")
	if err != nil {
		t.Fatal(err)
	}
	r.Tenant, r.Reads.TokenFile = "team-a", token
	rs, docs, err := r.Read(ctx)
	if err != nil || len(docs) != 3 || len(rs) != 5 {
		t.Fatalf("read %d rules, %d documents: %v", len(rs), len(docs), err)
	}
	u, err := r.Unit(ctx, "shop", "shop")
	if err != nil || u.Gone || u.Source != ts.URL+"/api/v1/rules/shop" || u.At != ts.URL+"/api/v1/rules/shop/shop" {
		t.Fatalf("the group: %+v %v", u, err)
	}
	if !strings.HasPrefix(string(u.Raw), "groups:\n  - name: shop\n    interval: 30s\n") {
		t.Errorf("the group as a rule file:\n%s", u.Raw)
	}
	if u, err := r.Unit(ctx, "shop", "edge"); err != nil || !u.Gone || string(u.Raw) != "groups: []\n" {
		t.Errorf("a group that isn't there: %+v %v", u, err)
	}
	missing.Store(true)
	if rs, docs, err := r.Read(ctx); err != nil || len(rs)+len(docs) != 0 {
		t.Errorf("a 404: %d rules, %v", len(rs), err)
	}
}

// What Mimir gave for a group, as a rule file of that one group and back,
// reads alike.
func TestWrapGroup(t *testing.T) {
	g := testdata(t, "mimir-3.2.4-group.yaml")
	w, err := WrapGroup(g)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rules.Parse(w); err != nil {
		t.Fatal(err)
	}
	back, ok, err := GroupOf(w)
	if err != nil || !ok {
		t.Fatal(err)
	}
	a, _ := rules.TreeHash(g)
	b, _ := rules.TreeHash(back)
	if a != b {
		t.Errorf("read back otherwise:\n%s", back)
	}
	if _, ok, err := GroupOf([]byte("groups: []\n")); ok || err != nil {
		t.Errorf("no group: %v %v", ok, err)
	}
	if _, _, err := GroupOf([]byte("groups:\n  - name: a\n  - name: b\n")); err == nil {
		t.Error("two groups taken as one")
	}
	if _, err := WrapGroup([]byte("- not\n- a group\n")); err == nil {
		t.Error("a list taken as a group")
	}
}

// amazon is Amazon Managed Service for Prometheus's control plane, as
// botocore's amp model has it: namespaces listed a page at a time, each
// described with its rule file in base64.
type amazon struct {
	t         *testing.T
	mu        sync.Mutex
	ns        map[string]string // name to its rule file
	modified  map[string]float64
	described map[string]int
}

func (a *amazon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") ||
		!strings.Contains(r.Header.Get("Authorization"), "/us-east-1/aps/aws4_request") {
		a.t.Errorf("%s %s signed %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
	}
	const base = "/workspaces/ws-1/rulegroupsnamespaces"
	names := slices.Sorted(func(yield func(string) bool) {
		for n := range a.ns {
			if !yield(n) {
				return
			}
		}
	})
	summary := func(n string) map[string]any {
		return map[string]any{"arn": "arn:aws:aps:us-east-1:123456789012:rulegroupsnamespace/ws-1/" + n, "name": n,
			"status": map[string]string{"statusCode": "ACTIVE"}, "createdAt": 1.7625e9, "modifiedAt": a.modified[n]}
	}
	switch {
	case r.URL.Path == base:
		// One a page, to page through.
		i := 0
		if tok := r.URL.Query().Get("nextToken"); tok != "" {
			fmt.Sscanf(tok, "page-%d", &i)
		}
		out := map[string]any{"ruleGroupsNamespaces": []any{}}
		if i < len(names) {
			out["ruleGroupsNamespaces"] = []any{summary(names[i])}
		}
		if i+1 < len(names) {
			out["nextToken"] = fmt.Sprintf("page-%d", i+1)
		}
		json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(r.URL.Path, base+"/"):
		n := strings.TrimPrefix(r.URL.Path, base+"/")
		data, ok := a.ns[n]
		if !ok {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		a.described[n]++
		d := summary(n)
		d["data"] = base64.StdEncoding.EncodeToString([]byte(data))
		json.NewEncoder(w).Encode(map[string]any{"ruleGroupsNamespace": d})
	default:
		http.NotFound(w, r)
	}
}

func signed() func(*http.Request) error {
	creds := aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""))
	return cloud.SignRequest(creds, "aps", "us-east-1", time.Now)
}

// Amazon's namespaces, page by page, each signed for aps in its region; a
// namespace whose modifiedAt hasn't moved isn't described again.
func TestReadAmazon(t *testing.T) {
	ctx := context.Background()
	shop := "groups:\n  - name: shop\n    rules:\n      - alert: Errors\n        expr: errors > 5\n"
	a := &amazon{t: t, ns: map[string]string{"shop": shop, "edge": "groups:\n  - name: edge\n    rules:\n      - alert: Errors\n        expr: up == 0\n"},
		modified: map[string]float64{"shop": 1, "edge": 1}, described: map[string]int{}}
	ts := httptest.NewServer(a)
	defer ts.Close()
	tr := &to{ts: ts}
	r, err := NewRuler("https://aps.us-east-1.amazonaws.com/workspaces/ws-1", "test")
	if err != nil {
		t.Fatal(err)
	}
	r.Reads.Sign, r.Transport = signed(), tr
	rs, docs, err := r.Read(ctx)
	if err != nil || len(rs) != 2 || len(docs) != 2 {
		t.Fatalf("%d rules: %v", len(rs), err)
	}
	if docs[1].Source != "arn:aws:aps:us-east-1:123456789012:rulegroupsnamespace/ws-1/shop" || docs[1].Namespace != "shop" || string(docs[1].Data) != shop {
		t.Errorf("the namespace as a document: %+v", docs[1])
	}
	if _, _, err := r.Read(ctx); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.modified["shop"] = 2
	a.ns["shop"] = strings.Replace(shop, "> 5", "> 6", 1)
	a.mu.Unlock()
	rs, _, err = r.Read(ctx)
	if err != nil || a.described["shop"] != 2 || a.described["edge"] != 1 {
		t.Errorf("described %v: %v", a.described, err)
	}
	if i := slices.IndexFunc(rs, func(r rules.Rule) bool { return r.Namespace == "shop" }); i < 0 || rs[i].Expr != "errors > 6" {
		t.Errorf("the namespace changed: %+v", rs)
	}
	for _, h := range tr.hosts {
		if h != "aps.us-east-1.amazonaws.com" {
			t.Errorf("asked %s", h)
		}
	}
	u, err := r.Unit(ctx, "shop", "shop")
	if err != nil || u.Status != "ACTIVE" || u.Source != docs[1].Source || u.Hash != rules.Hash(u.Raw) {
		t.Errorf("the namespace as a unit: %+v %v", u, err)
	}
	// The answers as the API documents show them.
	l, err := AmazonList(testdata(t, "amazon-list.json"))
	if err != nil || len(l.RuleGroupsNamespaces) != 1 || l.NextToken != "page-2" || l.RuleGroupsNamespaces[0].Tags["team"] != "shop" {
		t.Errorf("the list: %+v %v", l, err)
	}
	d, data, err := AmazonDescribed(testdata(t, "amazon-describe.json"))
	if err != nil || d.Status.Code != "ACTIVE" || !strings.Contains(string(data), "# The shop's own.") {
		t.Errorf("described: %+v %s %v", d, data, err)
	}
}

// Azure's rule groups, page by page through nextLink on Resource
// Manager's host only, each as a document of its resource group; one whose
// id is outside --ruler's scope isn't read.
func TestReadAzure(t *testing.T) {
	ctx := context.Background()
	page1 := testdata(t, "azure-list.json")
	elsewhere := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api-version") != "2023-03-01" || r.Header.Get("Authorization") != "Bearer azure-token" {
			t.Errorf("%s asked as %q", r.URL, r.Header.Get("Authorization"))
		}
		switch {
		case r.URL.Query().Get("$skiptoken") == "page-2":
			out := map[string]any{"value": []any{
				json.RawMessage(strings.ReplaceAll(string(azureItem(t, page1)), "rg-shop", "rg-edge")),
				json.RawMessage(strings.ReplaceAll(string(azureItem(t, page1)), "sub-1", "sub-2")), // not --ruler's
			}}
			if elsewhere {
				out["nextLink"] = "https://evil.example.com/next"
			}
			json.NewEncoder(w).Encode(out)
		case r.URL.Path == "/subscriptions/sub-1/providers/Microsoft.AlertsManagement/prometheusRuleGroups":
			w.Write(page1)
		case strings.HasSuffix(r.URL.Path, "/prometheusRuleGroups/shop"):
			w.Write(azureItem(t, page1))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	r, err := NewRuler("https://management.azure.com/subscriptions/sub-1", "test")
	if err != nil {
		t.Fatal(err)
	}
	r.Transport = &to{ts: ts}
	r.Reads.Sign = func(r *http.Request) error { r.Header.Set("Authorization", "Bearer azure-token"); return nil }
	rs, docs, err := r.Read(ctx)
	if err != nil || len(docs) != 2 || len(rs) != 2 {
		t.Fatalf("%d documents: %v", len(docs), err)
	}
	if docs[0].Namespace != "rg-shop" || docs[1].Namespace != "rg-edge" || rs[0].Group != "shop" || rs[0].Namespace != "rg-shop" {
		t.Errorf("documents %+v, rules %+v", docs, rs)
	}
	u, err := r.Unit(ctx, "rg-shop", "shop")
	if err != nil || u.Tags["team"] != "shop" || !rules.IsAzureGroup(u.Raw) {
		t.Fatalf("the rule group: %+v %v", u, err)
	}
	// What Azure sets itself isn't hashed.
	h2, _ := AzureHash([]byte(strings.Replace(string(u.Raw), "someone@example.com", "another@example.com", 1)))
	if h2 != u.Hash {
		t.Error("systemData is in the hash")
	}
	elsewhere = true
	if _, _, err := r.Read(ctx); err == nil || !strings.Contains(err.Error(), "isn't followed") {
		t.Errorf("a nextLink elsewhere: %v", err)
	}
	// A rule group that answers with another's id isn't the one asked for.
	if _, err := r.Unit(ctx, "rg-edge", "shop"); err == nil {
		t.Error("another rule group's answer taken")
	}
}

// azureItem is the first rule group of a list.
func azureItem(t *testing.T, list []byte) []byte {
	items, _, err := AzureList(list)
	if err != nil || len(items) == 0 {
		t.Fatal(err)
	}
	return items[0]
}

// A ruler is asked only on --ruler's host: a redirect isn't followed, a URL
// elsewhere isn't asked, and an answer too large is an error.
func TestCallStaysOnHost(t *testing.T) {
	ctx := context.Background()
	var reached atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
	defer elsewhere.Close()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/big" {
			w.Write(make([]byte, 2<<10))
			return
		}
		http.Redirect(w, r, elsewhere.URL+"/rules", http.StatusTemporaryRedirect)
	}))
	defer ts.Close()
	r, _ := NewRuler(ts.URL+"/rules", "test")
	if _, _, err := r.Call(ctx, http.MethodPost, ts.URL+"/rules/shop", []byte("x"), "application/yaml", Creds{}, UnitLimit); err == nil ||
		!strings.Contains(err.Error(), "redirect") {
		t.Errorf("a redirect: %v", err)
	}
	if _, _, err := r.Call(ctx, http.MethodGet, elsewhere.URL+"/rules", nil, "", Creds{}, UnitLimit); err == nil {
		t.Error("asked elsewhere")
	}
	if _, _, err := r.Call(ctx, http.MethodGet, ts.URL+"/big", nil, "", Creds{}, 1<<10); err == nil {
		t.Error("an answer too large read")
	}
	if n := reached.Load(); n != 0 {
		t.Errorf("%d requests elsewhere", n)
	}
	for _, name := range []string{"", ".", "..", "a\x00b", "a\nb", strings.Repeat("a", 1025)} {
		if r.NameOK(name, true) == nil {
			t.Errorf("the name %q taken", name)
		}
	}
	amazon := &Ruler{Kind: RulerAmazon}
	if amazon.NameOK(strings.Repeat("a", 129), true) == nil || amazon.NameOK(strings.Repeat("a", 129), false) != nil {
		t.Error("an Amazon namespace's name longer than 128 bytes")
	}
}

// A proposal is written where it was read: the source is --ruler's, for
// the namespace and the group.
func TestOwns(t *testing.T) {
	m, _ := NewRuler("http://mimir/prometheus/config/v1/rules/", "test")
	a, _ := NewRuler("https://aps.us-east-1.amazonaws.com/workspaces/ws-1", "test")
	z, _ := NewRuler("https://management.azure.com/subscriptions/sub-1/resourceGroups/rg-shop", "test")
	for _, c := range []struct {
		r                 *Ruler
		source, ns, group string
		ok                bool
	}{
		{m, "http://mimir/prometheus/config/v1/rules/shop", "shop", "shop", true},
		{m, "http://mimir/prometheus/config/v1/rules/alloy%2Fx", "alloy/x", "g", true},
		{m, "http://other/prometheus/config/v1/rules/shop", "shop", "shop", false},
		{m, "http://mimir/prometheus/config/v1/rules/shop", "edge", "shop", false},
		{a, "arn:aws:aps:us-east-1:123456789012:rulegroupsnamespace/ws-1/shop", "shop", "g", true},
		{a, "arn:aws:aps:us-east-1:123456789012:rulegroupsnamespace/ws-2/shop", "shop", "g", false},
		{a, "arn:aws:aps:eu-west-1:123456789012:rulegroupsnamespace/ws-1/shop", "shop", "g", false},
		{z, "/subscriptions/SUB-1/resourceGroups/RG-SHOP/providers/Microsoft.AlertsManagement/prometheusRuleGroups/shop", "rg-shop", "shop", true},
		{z, "/subscriptions/sub-1/resourceGroups/rg-edge/providers/Microsoft.AlertsManagement/prometheusRuleGroups/shop", "rg-edge", "shop", false},
		{z, "/subscriptions/sub-1/resourceGroups/rg-shop/providers/Microsoft.Other/things/shop", "rg-shop", "shop", false},
	} {
		if err := c.r.Owns(c.source, c.ns, c.group); (err == nil) != c.ok {
			t.Errorf("%s %s/%s: %v", c.source, c.ns, c.group, err)
		}
	}
}

// Rules of one name in namespace a/b's group api and in namespace a's group
// b/api aren't given one name: each part is written with its / escaped.
func TestTellApartSlashes(t *testing.T) {
	rs := []rules.Rule{
		{Name: "Errors", Group: "api", Namespace: "a/b", Expr: "errors > 5"},
		{Name: "Errors", Group: "b/api", Namespace: "a", Expr: "vector(1)"},
		{Name: "Errors", Group: "x]", Namespace: "a, b", Expr: "vector(2)"},
	}
	TellApart(rs)
	got := []string{rs[0].Name, rs[1].Name, rs[2].Name}
	if want := []string{"Errors [a%2Fb/api]", "Errors [a/b%2Fapi]", "Errors [a%2C b/x%5D]"}; !slices.Equal(got, want) {
		t.Errorf("names %q, want %q", got, want)
	}
	// The two alone: counted as two pairs, so told apart at all.
	two := []rules.Rule{{Name: "Errors", Group: "api", Namespace: "a/b"}, {Name: "Errors", Group: "b/api", Namespace: "a"}}
	TellApart(two)
	if two[0].Name == two[1].Name {
		t.Errorf("both named %q", two[0].Name)
	}
}

// A name with a path segment . or .. or an empty one isn't written to: a
// proxy that decodes the path could take shop-x/../payments to payments.
func TestNameSegments(t *testing.T) {
	r, _ := NewRuler("http://mimir:8080/prometheus/config/v1/rules", "test")
	for _, name := range []string{"shop-x/../payments", "a/./b", "a//b", "/a", "a/", `a\..\b`, "..", "."} {
		if r.NameOK(name, true) == nil {
			t.Errorf("%q taken", name)
		}
	}
	for _, name := range []string{"alloy/payments/rules/0b1c", "a..b", ".hidden", "v1.2"} {
		if err := r.NameOK(name, true); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
}

// Azure may give a resource group's name in another letter case from one
// read to the next: a rule keeps its key.
func TestReadAzureCase(t *testing.T) {
	ctx := context.Background()
	item := string(azureItem(t, testdata(t, "azure-list.json")))
	var upper atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		it := item
		if upper.Load() {
			it = strings.ReplaceAll(it, "rg-shop", "RG-Shop")
		}
		io.WriteString(w, `{"value": [`+it+`]}`)
	}))
	defer ts.Close()
	r, _ := NewRuler("https://management.azure.com/subscriptions/sub-1", "test")
	r.Transport = &to{ts: ts}
	a, _, err := r.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	upper.Store(true)
	b, _, err := r.Read(ctx)
	if err != nil || len(a) != 1 || len(b) != 1 || a[0].Key() != b[0].Key() || a[0].Namespace != "rg-shop" {
		t.Errorf("the key changed with the letter case: %v\n%s\n%s", err, a[0].Key(), b[0].Key())
	}
}

// Mimir 3.2.4 writes an expression whose first line starts with spaces as
// a block YAML can't read back (|4, its lines two short): as it answered,
// recorded. The group is passed over, with why; the rest of the list, and
// of the group's own namespace, is read.
func TestConfigDocsPassesOver(t *testing.T) {
	base := "http://mimir/prometheus/config/v1/rules"
	list := testdata(t, "mimir-3.2.4-indicator-list.yaml")
	docs, passed, err := ConfigDocs(base, list)
	if err != nil || len(docs) != 1 || docs[0].Namespace != "payments" || len(passed) != 1 || !strings.Contains(passed[0].Error(), `namespace "shop", group "lead"`) {
		t.Fatalf("%d documents, passed over %v: %v", len(docs), passed, err)
	}
	more := string(list) + "    - name: ok\n      rules:\n        - alert: Fine\n          expr: up == 1\n"
	docs, passed, err = ConfigDocs(base, []byte(more))
	if err != nil || len(docs) != 2 || len(passed) != 1 {
		t.Fatalf("a good group beside it: %d documents, %v, %v", len(docs), passed, err)
	}
	rs, err := rules.FromDocs(docs)
	if err != nil || len(rs) != 2 || rs[1].Namespace != "shop" || rs[1].Expr != "up == 1" {
		t.Errorf("read %+v: %v", rs, err)
	}
	if _, err := WrapGroup(testdata(t, "mimir-3.2.4-indicator-group.yaml")); err == nil {
		t.Error("the group read back")
	}
}

// A ruler's read passes over what can't be read, tells Warn once while it
// lasts, and reads the rest: Mimir's group as above; Amazon's namespace
// that holds no rule file, and one that held one before keeps it.
func TestReadPassesOver(t *testing.T) {
	ctx := context.Background()
	group := testdata(t, "mimir-3.2.4-indicator-group.yaml")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/shop/lead") {
			w.Write(group)
			return
		}
		w.Write(testdata(t, "mimir-3.2.4-indicator-list.yaml"))
	}))
	defer ts.Close()
	r, _ := NewRuler(ts.URL+"/prometheus/config/v1/rules", "test")
	var told []string
	r.Warn = func(s string) { told = append(told, s) }
	for range 2 {
		rs, _, err := r.Read(ctx)
		if err != nil || len(rs) != 1 || rs[0].Name != "Fine" {
			t.Fatalf("read %+v: %v", rs, err)
		}
	}
	if len(told) != 1 || !strings.Contains(told[0], "lead") {
		t.Errorf("told %q", told)
	}
	if _, err := r.Unit(ctx, "shop", "lead"); !errors.Is(err, ErrUnreadable) {
		t.Errorf("the group: %v", err)
	}

	shop := "groups:\n  - name: shop\n    rules:\n      - alert: Errors\n        expr: errors > 5\n"
	a := &amazon{t: t, ns: map[string]string{"shop": shop, "draft": "# to be filled in\n"},
		modified: map[string]float64{"shop": 1, "draft": 1}, described: map[string]int{}}
	as := httptest.NewServer(a)
	defer as.Close()
	z, _ := NewRuler("https://aps.us-east-1.amazonaws.com/workspaces/ws-1", "test")
	z.Reads.Sign, z.Transport = signed(), &to{ts: as}
	told = nil
	z.Warn = func(s string) { told = append(told, s) }
	rs, docs, err := z.Read(ctx)
	if err != nil || len(rs) != 1 || len(docs) != 1 || len(told) != 1 || !strings.Contains(told[0], `"draft"`) {
		t.Fatalf("a namespace of no rules: %d rules, told %q: %v", len(rs), told, err)
	}
	a.mu.Lock()
	a.ns["shop"], a.modified["shop"] = "groups:\n  - name: shop\n    rules:\n      - alert: Errors\n        expr: errors > 5\n        for: 5 minutes\n", 2
	a.mu.Unlock()
	rs, _, err = z.Read(ctx)
	if err != nil || len(rs) != 1 || rs[0].Expr != "errors > 5" || !strings.Contains(told[len(told)-1], "last rules read are kept") {
		t.Errorf("a namespace that read before: %+v, told %q: %v", rs, told, err)
	}
}

// A namespace whose groups are YAML but not rules (a for that isn't a
// duration) is passed over at the read, the rest read.
func TestReadPassesOverRules(t *testing.T) {
	ctx := context.Background()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "bad:\n    - name: g\n      rules:\n        - alert: B\n          expr: up\n          for: 5 minutes\n"+
			"good:\n    - name: g\n      rules:\n        - alert: A\n          expr: up\n")
	}))
	defer ts.Close()
	r, _ := NewRuler(ts.URL+"/rules", "test")
	var told []string
	r.Warn = func(s string) { told = append(told, s) }
	rs, docs, err := r.Read(ctx)
	if err != nil || len(rs) != 1 || rs[0].Name != "A" || len(docs) != 1 || len(told) != 1 || !strings.Contains(told[0], "/rules/bad") {
		t.Errorf("read %+v, told %q: %v", rs, told, err)
	}
}
