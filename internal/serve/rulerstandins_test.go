package serve

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/cloud"
)

// Stand-ins for the managed Prometheus rulers (docs/design/rulers.md), each
// answering as the real one does where it was checked: Mimir's as Mimir
// 3.2.4 answered when run locally (Cortex's at Cortex's paths), Amazon's
// as botocore's amp model has it, Azure's as its REST specification
// (PrometheusRuleGroups, 2023-03-01) has it.

// toStandIn sends every request to a stand-in, whatever its host, and
// notes the host it was for: a ruler's real host is what Lapilli checks.
type toStandIn struct {
	ts    *httptest.Server
	mu    sync.Mutex
	hosts map[string]int
}

func (s *toStandIn) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	if s.hosts == nil {
		s.hosts = map[string]int{}
	}
	s.hosts[r.URL.Host]++
	s.mu.Unlock()
	u, _ := url.Parse(s.ts.URL)
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = u.Scheme, u.Host
	return http.DefaultTransport.RoundTrip(r)
}

// fakeAuth stands in for a cloud's credentials: Amazon's requests are
// signed with SigV4 as they are for real, with keys of the test's, and
// each says which identity sent it: the pod's, or the one given to write
// as.
func fakeAuth(t *testing.T) {
	t.Helper()
	was := requestAuth
	t.Cleanup(func() { requestAuth = was })
	requestAuth = func(ctx context.Context, kind, endpoint, as string) (func(*http.Request) error, error) {
		who := "pod"
		if as != "" {
			who = "as " + as
		}
		creds := aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""))
		sign := cloud.SignRequest(creds, "aps", "us-east-1", time.Now)
		return func(r *http.Request) error {
			r.Header.Set("X-Identity", who)
			if kind == "aws" {
				return sign(r)
			}
			r.Header.Set("Authorization", "Bearer azure "+who)
			return nil
		}, nil
	}
}

// segments are a path's parts after base, each unescaped: a namespace may
// hold slashes, sent as %2F.
func segments(r *http.Request, base string) []string {
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.EscapedPath(), base), "/")
	if rest == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(rest, "/") {
		u, _ := url.PathUnescape(p)
		out = append(out, u)
	}
	return out
}

// mimirStandIn is Mimir's (or, at Cortex's paths, Cortex's) configuration
// API: groups kept as Mimir keeps them, written out again its own way
// (four spaces, comments dropped, durations as 1m30s).
type mimirStandIn struct {
	t    *testing.T
	base string
	mu   sync.Mutex
	// groups by namespace, then by name.
	groups map[string]map[string]*yaml.Node
	writes []string
	// The tokens reads and writes must carry.
	readToken, writeToken string
	// failGets answers every read with a 503; pauseOn is a file made when
	// a group is read, once armed; redirect sends writes on; reject is
	// what a group posted is refused for having, as Mimir refuses an
	// expression that doesn't parse.
	failGets atomic.Bool
	pauseOn  string
	armed    atomic.Bool
	redirect string
	reject   string
	// meanwhile, when set, is run on each write: someone else writing too.
	meanwhile func(*mimirStandIn)
	// stale is how many reads of a group after a write give it as it was,
	// as a ruler that caches its storage does.
	stale    int
	cached   map[string]*yaml.Node
	staleFor map[string]int
}

func newMimir(t *testing.T, base string) *mimirStandIn {
	return &mimirStandIn{t: t, base: base, groups: map[string]map[string]*yaml.Node{},
		readToken: "ruler-reads-0123456789", writeToken: "ruler-writes-0123456789"}
}

// put keeps a group as Mimir would, and says why it wouldn't.
func (m *mimirStandIn) put(ns string, body []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(body, &doc); err != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("invalid rules configuration: not a rule group")
	}
	g := doc.Content[0]
	name, rs := mapValue(g, "name"), mapValue(g, "rules")
	switch {
	case name == nil || name.Value == "":
		return fmt.Errorf("invalid rules configuration: rule group has no name")
	case rs == nil || len(rs.Content) == 0:
		return fmt.Errorf("invalid rules configuration: rule group '%s' has no rules", name.Value)
	case m.reject != "" && bytes.Contains(body, []byte(m.reject)):
		return fmt.Errorf(`group %q, rule 0: could not parse expression: 1:6: parse error: unexpected end of input`, name.Value)
	}
	normalize(g)
	if m.groups[ns] == nil {
		m.groups[ns] = map[string]*yaml.Node{}
	}
	m.groups[ns][name.Value] = g
	return nil
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// normalize writes a group as Mimir keeps it: no comments, durations as
// Prometheus prints them.
func normalize(n *yaml.Node) {
	n.HeadComment, n.LineComment, n.FootComment = "", "", ""
	n.Style &^= yaml.FlowStyle | yaml.DoubleQuotedStyle | yaml.SingleQuotedStyle
	for i, c := range n.Content {
		normalize(c)
		if n.Kind == yaml.MappingNode && i%2 == 1 && slices.Contains([]string{"for", "keep_firing_for", "interval"}, n.Content[i-1].Value) {
			if d, err := ParseDuration(c.Value); err == nil {
				c.Value = promString(d)
			}
		}
	}
}

// promString is a duration as Prometheus prints one: 1m30s, 2h.
func promString(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	var b strings.Builder
	for _, u := range []struct {
		unit time.Duration
		s    string
	}{{24 * time.Hour, "d"}, {time.Hour, "h"}, {time.Minute, "m"}, {time.Second, "s"}} {
		if n := d / u.unit; n > 0 {
			fmt.Fprintf(&b, "%d%s", n, u.s)
			d -= n * u.unit
		}
	}
	return b.String()
}

func encode4(n *yaml.Node) []byte {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(4)
	enc.Encode(n)
	enc.Close()
	return b.Bytes()
}

// list is namespaces, or one, as Mimir lists them: a map of each to its
// groups, by name.
func (m *mimirStandIn) list(only string) []byte {
	root := &yaml.Node{Kind: yaml.MappingNode}
	for _, ns := range slices.Sorted(func(yield func(string) bool) {
		for k := range m.groups {
			if !yield(k) {
				return
			}
		}
	}) {
		if only != "" && ns != only || len(m.groups[ns]) == 0 {
			continue
		}
		seq := &yaml.Node{Kind: yaml.SequenceNode}
		for _, name := range slices.Sorted(func(yield func(string) bool) {
			for k := range m.groups[ns] {
				if !yield(k) {
					return
				}
			}
		}) {
			seq.Content = append(seq.Content, m.groups[ns][name])
		}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: ns}, seq)
	}
	if len(root.Content) == 0 {
		return []byte("{}\n")
	}
	return encode4(root)
}

func (m *mimirStandIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := m.readToken
	if r.Method != http.MethodGet {
		want = m.writeToken
	}
	if r.Header.Get("Authorization") != "Bearer "+want {
		http.Error(w, "no org id", http.StatusUnauthorized)
		return
	}
	if !strings.HasPrefix(r.URL.Path, m.base) {
		http.NotFound(w, r)
		return
	}
	seg := segments(r, m.base)
	if r.Method == http.MethodGet && m.failGets.Load() {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodGet && m.redirect != "" {
		http.Redirect(w, r, m.redirect, http.StatusTemporaryRedirect)
		return
	}
	accepted := func() {
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, `{"status":"success","data":null,"errorType":"","error":""}`)
	}
	w.Header().Set("Content-Type", "application/yaml")
	switch {
	case r.Method == http.MethodGet && len(seg) == 0:
		w.Write(m.list(""))
	case r.Method == http.MethodGet && len(seg) == 1:
		w.Write(m.list(seg[0]))
	case r.Method == http.MethodGet && len(seg) == 2:
		if m.armed.CompareAndSwap(true, false) {
			os.WriteFile(m.pauseOn, []byte("1"), 0o644)
		}
		g, ok := m.groups[seg[0]][seg[1]]
		if k := seg[0] + "/" + seg[1]; m.staleFor[k] > 0 {
			m.staleFor[k]--
			g, ok = m.cached[k], m.cached[k] != nil
		}
		if !ok {
			http.Error(w, "group does not exist", http.StatusNotFound)
			return
		}
		w.Write(encode4(g))
	case r.Method == http.MethodPost && len(seg) == 1:
		if r.Header.Get("Content-Type") != "application/yaml" {
			http.Error(w, "unsupported content type", http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var named struct{ Name string }
		yaml.Unmarshal(body, &named)
		if m.stale > 0 {
			if m.cached == nil {
				m.cached, m.staleFor = map[string]*yaml.Node{}, map[string]int{}
			}
			k := seg[0] + "/" + named.Name
			m.cached[k], m.staleFor[k] = m.groups[seg[0]][named.Name], m.stale
		}
		if err := m.put(seg[0], body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var name struct{ Name string }
		yaml.Unmarshal(body, &name)
		m.writes = append(m.writes, "POST "+seg[0]+"/"+name.Name)
		if m.meanwhile != nil {
			m.meanwhile(m)
		}
		accepted()
	case r.Method == http.MethodDelete && len(seg) == 2:
		delete(m.groups[seg[0]], seg[1])
		m.writes = append(m.writes, "DELETE "+seg[0]+"/"+seg[1])
		accepted()
	default:
		http.NotFound(w, r)
	}
}

// group is one group, as the stand-in keeps it.
func (m *mimirStandIn) group(ns, name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.groups[ns][name]
	if !ok {
		return ""
	}
	return string(encode4(g))
}

func (m *mimirStandIn) written() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.writes)
}

// amazonNS is a rule groups namespace, as Amazon keeps one.
type amazonNS struct {
	data           string
	status, reason string
	tags           map[string]string
	modified       float64
	// updating is how many more times it is described UPDATING after a
	// write, before it is ACTIVE.
	updating int
}

// amazonStandIn is Amazon Managed Service for Prometheus's control plane:
// a workspace's namespaces, listed, described and put whole.
type amazonStandIn struct {
	t      *testing.T
	mu     sync.Mutex
	ns     map[string]*amazonNS
	writes []string
	tokens []string
	// slow is how many times a namespace is described UPDATING after a
	// write; fails leaves it UPDATE_FAILED instead.
	slow  int
	fails bool
	// writer is the identity writes must come from.
	writer string
}

func (a *amazonStandIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") || !strings.Contains(auth, "/us-east-1/aps/aws4_request") {
		a.t.Errorf("%s %s isn't signed for aps: %q", r.Method, r.URL.Path, auth)
	}
	who := "pod"
	if r.Method == http.MethodPut {
		who = a.writer
	}
	if got := r.Header.Get("X-Identity"); got != who {
		a.t.Errorf("%s %s sent as %q, want %q", r.Method, r.URL.Path, got, who)
	}
	const base = "/workspaces/ws-1/rulegroupsnamespaces"
	seg := segments(r, base)
	if !strings.HasPrefix(r.URL.Path, base) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	summary := func(name string, n *amazonNS) map[string]any {
		return map[string]any{"arn": "arn:aws:aps:us-east-1:123456789012:rulegroupsnamespace/ws-1/" + name, "name": name,
			"status": map[string]string{"statusCode": n.status, "statusReason": n.reason}, "createdAt": 1.7625e9, "modifiedAt": n.modified, "tags": n.tags}
	}
	switch {
	case r.Method == http.MethodGet && len(seg) == 0:
		var out []any
		for _, name := range slices.Sorted(func(yield func(string) bool) {
			for k := range a.ns {
				if !yield(k) {
					return
				}
			}
		}) {
			out = append(out, summary(name, a.ns[name]))
		}
		json.NewEncoder(w).Encode(map[string]any{"ruleGroupsNamespaces": out})
	case r.Method == http.MethodGet && len(seg) == 1:
		n, ok := a.ns[seg[0]]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"Rule groups namespace not found"}`)
			return
		}
		if n.status == "UPDATING" {
			if n.updating <= 0 {
				n.status = "ACTIVE"
			} else {
				n.updating--
			}
		}
		d := summary(seg[0], n)
		d["data"] = base64.StdEncoding.EncodeToString([]byte(n.data))
		json.NewEncoder(w).Encode(map[string]any{"ruleGroupsNamespace": d})
	case r.Method == http.MethodPut && len(seg) == 1:
		n, ok := a.ns[seg[0]]
		var in struct {
			Data        []byte `json:"data"`
			ClientToken string `json:"clientToken"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Data) == 0 || len(in.ClientToken) == 0 || len(in.ClientToken) > 64 {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"message":"ValidationException"}`)
			return
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if n.status != "ACTIVE" {
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, `{"message":"ConflictException"}`)
			return
		}
		a.writes = append(a.writes, "PUT "+seg[0])
		a.tokens = append(a.tokens, in.ClientToken)
		n.modified++
		if a.fails {
			n.status, n.reason = "UPDATE_FAILED", "the rules file has an error"
		} else {
			n.data, n.status, n.updating = string(in.Data), "UPDATING", a.slow
		}
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"name": seg[0], "arn": summary(seg[0], n)["arn"], "status": map[string]string{"statusCode": "UPDATING"}})
	default:
		http.NotFound(w, r)
	}
}

func (a *amazonStandIn) data(ns string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ns[ns].data
}

// azureStandIn is Azure Resource Manager's Prometheus rule groups in one
// subscription: listed a page at a time, read, and put whole.
type azureStandIn struct {
	t      *testing.T
	mu     sync.Mutex
	groups map[string]map[string]any // by RG/NAME
	writes []string
	bodies []map[string]any
	writer string
	// otherID answers a rule group with another's id; meanwhile, when set,
	// is run once, just after a write: someone else writing in the portal
	// before Lapilli reads it back.
	otherID   bool
	meanwhile func(*azureStandIn)
}

const azureRG = "Microsoft.AlertsManagement/prometheusRuleGroups"

func (z *azureStandIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	z.mu.Lock()
	defer z.mu.Unlock()
	who := "pod"
	if r.Method == http.MethodPut {
		who = z.writer
	}
	if got := r.Header.Get("Authorization"); got != "Bearer azure "+who {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"code":"AuthenticationFailed"}}`)
		return
	}
	if r.URL.Query().Get("api-version") != "2023-03-01" {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"code":"MissingApiVersionParameter"}}`)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range z.groups {
			if !yield(k) {
				return
			}
		}
	})
	p := r.URL.Path
	switch {
	case r.Method == http.MethodGet && p == "/subscriptions/sub-1/providers/"+azureRG:
		// One a page, through nextLink.
		i := 0
		fmt.Sscanf(r.URL.Query().Get("$skiptoken"), "%d", &i)
		out := map[string]any{"value": []any{}}
		if i < len(keys) {
			out["value"] = []any{z.groups[keys[i]]}
		}
		if i+1 < len(keys) {
			out["nextLink"] = fmt.Sprintf("https://management.azure.com/subscriptions/sub-1/providers/%s?api-version=2023-03-01&$skiptoken=%d", azureRG, i+1)
		}
		json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(p, "/subscriptions/sub-1/resourceGroups/"):
		seg := strings.Split(strings.TrimPrefix(p, "/subscriptions/sub-1/resourceGroups/"), "/")
		if len(seg) != 5 || seg[1]+"/"+seg[2]+"/"+seg[3] != "providers/"+azureRG {
			http.NotFound(w, r)
			return
		}
		key := seg[0] + "/" + seg[4]
		g, ok := z.groups[key]
		switch r.Method {
		case http.MethodGet:
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, `{"error":{"code":"ResourceNotFound"}}`)
				return
			}
			if z.otherID {
				c := map[string]any{}
				for k, v := range g {
					c[k] = v
				}
				c["id"] = strings.Replace(g["id"].(string), "/"+seg[4], "/elsewhere", 1)
				g = c
			}
			json.NewEncoder(w).Encode(g)
		case http.MethodPut:
			var in map[string]any
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			for _, k := range []string{"id", "name", "type", "systemData"} {
				if _, ok := in[k]; ok {
					z.t.Errorf("PUT sent %s, which Azure sets", k)
				}
			}
			props, _ := in["properties"].(map[string]any)
			if props == nil || props["scopes"] == nil || props["rules"] == nil {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"error":{"code":"InvalidRequestContent"}}`)
				return
			}
			z.bodies = append(z.bodies, in)
			z.writes = append(z.writes, "PUT "+key)
			in["id"] = "/subscriptions/sub-1/resourceGroups/" + seg[0] + "/providers/" + azureRG + "/" + seg[4]
			in["name"], in["type"] = seg[4], azureRG
			in["systemData"] = map[string]any{"lastModifiedBy": who, "lastModifiedAt": time.Now().UTC().Format(time.RFC3339)}
			z.groups[key] = in
			if z.meanwhile != nil {
				z.meanwhile(z)
				z.meanwhile = nil
			}
			if ok {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusCreated)
			}
			json.NewEncoder(w).Encode(in)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	default:
		http.NotFound(w, r)
	}
}

// azureGroupOf is a rule group resource, as Azure answers one.
func azureGroupOf(rg, name string, tags map[string]any, rs ...map[string]any) map[string]any {
	var rules []any
	for _, r := range rs {
		rules = append(rules, r)
	}
	return map[string]any{
		"id":       "/subscriptions/sub-1/resourceGroups/" + rg + "/providers/" + azureRG + "/" + name,
		"name":     name,
		"type":     azureRG,
		"location": "koreacentral",
		"tags":     tags,
		"properties": map[string]any{
			"scopes":      []any{"/subscriptions/sub-1/resourceGroups/" + rg + "/providers/microsoft.monitor/accounts/ws"},
			"enabled":     true,
			"clusterName": "",
			"interval":    "PT1M",
			"rules":       rules,
		},
		"systemData": map[string]any{"createdBy": "someone@example.com", "createdAt": "2026-11-01T00:00:00Z"},
	}
}

func (z *azureStandIn) rule(key, alert string) map[string]any {
	z.mu.Lock()
	defer z.mu.Unlock()
	g := z.groups[key]
	if g == nil {
		return nil
	}
	for _, r := range g["properties"].(map[string]any)["rules"].([]any) {
		if rm := r.(map[string]any); rm["alert"] == alert {
			return rm
		}
	}
	return nil
}
