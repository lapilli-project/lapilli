package source

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/cloud"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// Reading a managed Prometheus ruler (docs/design/rulers.md): the rules as
// written, from the API that writes them, one document a namespace or rule
// group, each rule knowing its namespace. --ruler's host says which API it
// is.

// The kinds of ruler --ruler reads.
const (
	// RulerConfig is Mimir's or Cortex's configuration API: a YAML map of
	// every namespace to its groups.
	RulerConfig = "config"
	// RulerAmazon is Amazon Managed Service for Prometheus's control
	// plane: a workspace's rule groups namespaces.
	RulerAmazon = "amazon"
	// RulerAzure is Azure Resource Manager's Prometheus rule groups, in a
	// subscription or a resource group.
	RulerAzure = "azure"
)

// azureAPI is the api-version Azure's rule groups are read and written
// with.
const azureAPI = "2023-03-01"

// azureType is the path a rule group is under in Azure Resource Manager.
const azureType = "providers/Microsoft.AlertsManagement/prometheusRuleGroups"

var (
	amazonPath = regexp.MustCompile(`^/workspaces/([0-9A-Za-z][-.0-9A-Z_a-z]{0,63})/?$`)
	azurePath  = regexp.MustCompile(`(?i)^/subscriptions/([0-9A-Za-z-]{1,64})(?:/resourceGroups/([-\w.()]{1,90}))?/?$`)
	azureID    = regexp.MustCompile(`(?i)^/subscriptions/([0-9A-Za-z-]{1,64})/resourceGroups/([-\w.()]{1,90})/providers/Microsoft\.AlertsManagement/prometheusRuleGroups/([^/]{1,260})$`)
	amazonARN  = regexp.MustCompile(`^arn:aws(?:-cn|-us-gov)?:aps:([a-z0-9-]+):[0-9]{12}:rulegroupsnamespace/([^/]+)/(.+)$`)
)

// RulerKind says which API a --ruler URL is: Mimir's or Cortex's
// configuration API (any URL but the two below), Amazon's control plane
// (aps in a region, a workspace's path), or Azure Resource Manager (a
// subscription, or a resource group in one). A workspace's query URL is
// refused, with the URL to give instead.
func RulerKind(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("--ruler %s: want the ruler's URL, such as http://mimir:8080/prometheus/config/v1/rules", raw)
	}
	if service, region, ok := cloud.AWSHost(u.Hostname()); ok {
		switch {
		case service == "aps-workspaces":
			ws := "WORKSPACE_ID"
			if m := regexp.MustCompile(`/workspaces/([^/]+)`).FindStringSubmatch(u.Path); m != nil {
				ws = m[1]
			}
			return "", fmt.Errorf("--ruler %s is a workspace's query URL: rules are read and written through the control plane, https://aps.%s.amazonaws.com/workspaces/%s", raw, region, ws)
		case service != "aps":
			return "", fmt.Errorf("--ruler %s: CloudWatch holds no Prometheus rule groups", raw)
		case u.Scheme != "https" || !amazonPath.MatchString(u.Path):
			return "", fmt.Errorf("--ruler %s: want a workspace on the control plane, https://aps.%s.amazonaws.com/workspaces/WORKSPACE_ID", raw, region)
		}
		return RulerAmazon, nil
	}
	if slices.ContainsFunc(cloud.AzureHosts, func(h string) bool { return strings.EqualFold(u.Hostname(), h) }) {
		if u.Scheme != "https" || u.Port() != "" || !azurePath.MatchString(u.Path) {
			return "", fmt.Errorf("--ruler %s: want a subscription, https://management.azure.com/subscriptions/SUB, or a resource group in one, .../subscriptions/SUB/resourceGroups/RG", raw)
		}
		return RulerAzure, nil
	}
	return RulerConfig, nil
}

// Creds authenticate a ruler's requests: a bearer token read from a file
// for each, so a rotated one is picked up, or a cloud's signature; neither,
// for a ruler reached only inside the cluster.
type Creds struct {
	TokenFile string
	Sign      func(*http.Request) error
}

// Ruler reads a ruler's rules, and is where they are written through.
type Ruler struct {
	URL  string
	Kind string
	// Tenant, when set, is sent as X-Scope-OrgID: Mimir's or Cortex's
	// tenant, for one with tenants and no gateway to set it.
	Tenant string
	Agent  string
	// Reads are the credentials the rules are read with.
	Reads Creds
	// Transport carries the requests; nil for the default.
	Transport http.RoundTripper

	mu sync.Mutex
	// seen are Amazon's namespaces as last described, by name: one whose
	// modifiedAt hasn't moved isn't described again.
	seen map[string]amazonSeen
}

type amazonSeen struct {
	modified string
	arn      string
	data     []byte
}

// NewRuler is a reader for the ruler at a --ruler URL.
func NewRuler(raw, agent string) (*Ruler, error) {
	kind, err := RulerKind(raw)
	if err != nil {
		return nil, err
	}
	return &Ruler{URL: strings.TrimRight(raw, "/"), Kind: kind, Agent: agent}, nil
}

// The most a ruler's answer may be: a list of every rule, and one unit.
const (
	ListLimit = 64 << 20
	UnitLimit = 4 << 20
)

// rulerTimeout is how long a request to a ruler may take.
const rulerTimeout = 30 * time.Second

// Call asks the ruler, and nowhere else: the URL must be on --ruler's host,
// as it was given, and a redirect isn't followed. The answer is read up to
// limit bytes; more is an error.
func (r *Ruler) Call(ctx context.Context, method, target string, body []byte, contentType string, c Creds, limit int64) (int, []byte, error) {
	base, _ := url.Parse(r.URL)
	u, err := url.Parse(target)
	if err != nil || base == nil || u.Scheme != base.Scheme || !strings.EqualFold(u.Host, base.Host) || u.User != nil {
		return 0, nil, fmt.Errorf("the ruler: %s is not on --ruler's host, so it isn't asked", clipURL(target))
	}
	var in io.Reader
	if body != nil {
		in = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, in)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", r.Agent)
	req.Header.Set("Accept", "application/json, application/yaml")
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	if r.Tenant != "" && r.Kind == RulerConfig {
		req.Header.Set("X-Scope-OrgID", r.Tenant)
	}
	if c.TokenFile != "" {
		b, err := os.ReadFile(c.TokenFile)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(b)))
	}
	if c.Sign != nil {
		if err := c.Sign(req); err != nil {
			return 0, nil, err
		}
	}
	client := &http.Client{Timeout: rulerTimeout, Transport: r.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("the ruler: %w", err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return 0, nil, fmt.Errorf("the ruler: %w", err)
	}
	if int64(len(out)) > limit {
		return 0, nil, fmt.Errorf("the ruler answered %s %s with more than %d MB", method, clipURL(target), limit>>20)
	}
	if resp.StatusCode/100 == 3 {
		return resp.StatusCode, nil, fmt.Errorf("the ruler answered %s %s with a redirect, which isn't followed", method, clipURL(target))
	}
	return resp.StatusCode, out, nil
}

func clipURL(s string) string {
	if len(s) > 200 {
		s = s[:200]
	}
	return verdict.Clean(s)
}

// Failed is an answer from a ruler that isn't what was asked for.
func Failed(code int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return fmt.Errorf("the ruler: HTTP %d: %s", code, verdict.Clean(msg))
}

// NameOK checks a name read from a ruler, which goes into a path as one
// escaped segment: not empty, . or .., without control characters, 1,024
// bytes at most, and an Amazon namespace's 128.
func (r *Ruler) NameOK(name string, namespace bool) error {
	switch {
	case name == "" || name == "." || name == "..":
		return fmt.Errorf("the ruler names a namespace or group %q, which isn't written to", name)
	case len(name) > 1024 || (r.Kind == RulerAmazon && namespace && len(name) > 128):
		return errors.New("the ruler names a namespace or group too long to be written to")
	case strings.IndexFunc(name, func(c rune) bool { return unicode.IsControl(c) || c == unicode.ReplacementChar }) >= 0:
		return errors.New("the ruler names a namespace or group with a control character in it, which isn't written to")
	}
	return nil
}

// Read reads every rule the ruler holds, and one document a namespace (a
// rule group, for Azure).
func (r *Ruler) Read(ctx context.Context) ([]rules.Rule, []rules.Doc, error) {
	var docs []rules.Doc
	var err error
	switch r.Kind {
	case RulerConfig:
		docs, err = r.readConfig(ctx)
	case RulerAmazon:
		docs, err = r.readAmazon(ctx)
	case RulerAzure:
		docs, err = r.readAzure(ctx)
	default:
		err = fmt.Errorf("a ruler of kind %q", r.Kind)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("--ruler: %w", err)
	}
	rs, err := rules.FromDocs(docs)
	if err != nil {
		return nil, nil, fmt.Errorf("--ruler: %w", err)
	}
	return rs, docs, nil
}

// readConfig reads Mimir's or Cortex's configuration API: every
// namespace's groups, as written.
func (r *Ruler) readConfig(ctx context.Context) ([]rules.Doc, error) {
	code, body, err := r.Call(ctx, http.MethodGet, r.URL, nil, "", r.Reads, ListLimit)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound {
		return nil, nil // none: Mimir answers {} instead
	}
	if code != http.StatusOK {
		return nil, Failed(code, body)
	}
	return ConfigDocs(r.URL, body)
}

// ConfigDocs are the documents of a configuration API's list: one a
// namespace, a rule file of its groups, sourced at its URL under base.
func ConfigDocs(base string, body []byte) ([]rules.Doc, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("not the rules a ruler's configuration API lists: %w", err)
	}
	if len(root.Content) == 0 {
		return nil, nil
	}
	m := root.Content[0]
	if m.Kind != yaml.MappingNode {
		return nil, errors.New("not the rules a ruler's configuration API lists: want a map of namespaces")
	}
	var docs []rules.Doc
	for i := 0; i+1 < len(m.Content); i += 2 {
		ns, groups := m.Content[i].Value, m.Content[i+1]
		if groups.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("namespace %q: want a list of groups", ns)
		}
		doc := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Value: "groups"}, groups}}
		b, err := encodeYAML(doc)
		if err != nil {
			return nil, err
		}
		docs = append(docs, rules.Doc{Source: base + "/" + url.PathEscape(ns), Namespace: ns, Data: b})
	}
	return docs, nil
}

func encodeYAML(n *yaml.Node) ([]byte, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// amazonNamespaces is ListRuleGroupsNamespaces's answer.
type amazonNamespaces struct {
	RuleGroupsNamespaces []amazonNamespace `json:"ruleGroupsNamespaces"`
	NextToken            string            `json:"nextToken"`
}

// amazonNamespace is a rule groups namespace, as listed or described.
type amazonNamespace struct {
	ARN        string            `json:"arn"`
	Name       string            `json:"name"`
	Data       string            `json:"data"`
	ModifiedAt json.RawMessage   `json:"modifiedAt"`
	Status     amazonStatus      `json:"status"`
	Tags       map[string]string `json:"tags"`
}

type amazonStatus struct {
	Code   string `json:"statusCode"`
	Reason string `json:"statusReason"`
}

// AmazonList reads a ListRuleGroupsNamespaces answer.
func AmazonList(body []byte) (amazonNamespaces, error) {
	var a amazonNamespaces
	if err := json.Unmarshal(body, &a); err != nil {
		return a, fmt.Errorf("not ListRuleGroupsNamespaces's answer: %w", err)
	}
	return a, nil
}

// AmazonDescribed reads a DescribeRuleGroupsNamespace answer: the
// namespace, and its rule file as it was uploaded.
func AmazonDescribed(body []byte) (amazonNamespace, []byte, error) {
	var a struct {
		NS amazonNamespace `json:"ruleGroupsNamespace"`
	}
	if err := json.Unmarshal(body, &a); err != nil {
		return a.NS, nil, fmt.Errorf("not DescribeRuleGroupsNamespace's answer: %w", err)
	}
	data, err := base64.StdEncoding.DecodeString(a.NS.Data)
	if err != nil {
		return a.NS, nil, fmt.Errorf("rule groups namespace %q: its data: %w", a.NS.Name, err)
	}
	return a.NS, data, nil
}

func (r *Ruler) amazonURL(ns string) string {
	return r.URL + "/rulegroupsnamespaces/" + url.PathEscape(ns)
}

// readAmazon lists a workspace's namespaces, and describes those whose
// modifiedAt moved since they were read.
func (r *Ruler) readAmazon(ctx context.Context) ([]rules.Doc, error) {
	var all []amazonNamespace
	token := ""
	for pages := 0; ; pages++ {
		if pages >= 1000 {
			return nil, errors.New("ListRuleGroupsNamespaces gave more than a thousand pages")
		}
		q := url.Values{"maxResults": {"100"}}
		if token != "" {
			q.Set("nextToken", token)
		}
		code, body, err := r.Call(ctx, http.MethodGet, r.URL+"/rulegroupsnamespaces?"+q.Encode(), nil, "", r.Reads, ListLimit)
		if err != nil {
			return nil, err
		}
		if code != http.StatusOK {
			return nil, Failed(code, body)
		}
		a, err := AmazonList(body)
		if err != nil {
			return nil, err
		}
		all = append(all, a.RuleGroupsNamespaces...)
		if a.NextToken == "" || a.NextToken == token {
			break
		}
		token = a.NextToken
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = map[string]amazonSeen{}
	}
	listed := map[string]bool{}
	var docs []rules.Doc
	for _, ns := range all {
		if err := r.NameOK(ns.Name, true); err != nil || listed[ns.Name] {
			continue // nothing written to can be named so
		}
		listed[ns.Name] = true
		was, ok := r.seen[ns.Name]
		if !ok || was.modified != string(ns.ModifiedAt) || was.arn != ns.ARN || len(ns.ModifiedAt) == 0 {
			code, body, err := r.Call(ctx, http.MethodGet, r.amazonURL(ns.Name), nil, "", r.Reads, UnitLimit)
			if err != nil {
				return nil, err
			}
			if code == http.StatusNotFound {
				continue // deleted since it was listed
			}
			if code != http.StatusOK {
				return nil, Failed(code, body)
			}
			d, data, err := AmazonDescribed(body)
			if err != nil {
				return nil, err
			}
			if d.Name != ns.Name {
				return nil, fmt.Errorf("DescribeRuleGroupsNamespace answered for %q with %q", ns.Name, d.Name)
			}
			was = amazonSeen{modified: string(ns.ModifiedAt), arn: d.ARN, data: data}
			r.seen[ns.Name] = was
		}
		docs = append(docs, rules.Doc{Source: was.arn, Namespace: ns.Name, Data: was.data})
	}
	for name := range r.seen {
		if !listed[name] {
			delete(r.seen, name)
		}
	}
	return docs, nil
}

// azureScope is --ruler's subscription, and its resource group when it
// names one.
func (r *Ruler) azureScope() (sub, rg string) {
	u, _ := url.Parse(r.URL)
	if u == nil {
		return "", ""
	}
	m := azurePath.FindStringSubmatch(u.Path)
	if m == nil {
		return "", ""
	}
	return m[1], m[2]
}

func (r *Ruler) azureBase() string {
	u, _ := url.Parse(r.URL)
	return u.Scheme + "://" + u.Host
}

// AzureList reads a list of rule groups, as Resource Manager answers one:
// each resource as it is, and the next page's link.
func AzureList(body []byte) ([]json.RawMessage, string, error) {
	var a struct {
		Value    []json.RawMessage `json:"value"`
		NextLink string            `json:"nextLink"`
	}
	if err := json.Unmarshal(body, &a); err != nil {
		return nil, "", fmt.Errorf("not a list of Azure rule groups: %w", err)
	}
	return a.Value, a.NextLink, nil
}

// readAzure lists the rule groups in --ruler's subscription or resource
// group, following nextLink on the same host, over https.
func (r *Ruler) readAzure(ctx context.Context) ([]rules.Doc, error) {
	sub, rg := r.azureScope()
	next := r.azureBase() + "/subscriptions/" + url.PathEscape(sub)
	if rg != "" {
		next += "/resourceGroups/" + url.PathEscape(rg)
	}
	next += "/" + azureType + "?api-version=" + azureAPI
	var docs []rules.Doc
	seen := map[string]bool{}
	for pages := 0; next != ""; pages++ {
		if pages >= 1000 {
			return nil, errors.New("the list of rule groups gave more than a thousand pages")
		}
		code, body, err := r.Call(ctx, http.MethodGet, next, nil, "", r.Reads, ListLimit)
		if err != nil {
			return nil, err
		}
		if code != http.StatusOK {
			return nil, Failed(code, body)
		}
		items, link, err := AzureList(body)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			d, ok, err := r.azureDoc(it)
			if err != nil {
				return nil, err
			}
			if ok && !seen[strings.ToLower(d.Source)] {
				seen[strings.ToLower(d.Source)] = true
				docs = append(docs, d)
			}
		}
		if link != "" {
			u, err := url.Parse(link)
			if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, mustHost(r.URL)) || u.User != nil {
				return nil, fmt.Errorf("the list's next page is at %s, not on --ruler's host: it isn't followed", clipURL(link))
			}
		}
		next = link
	}
	return docs, nil
}

func mustHost(raw string) string {
	u, _ := url.Parse(raw)
	if u == nil {
		return ""
	}
	return u.Host
}

// azureDoc is a rule group as a document: under --ruler's scope, by its id,
// or none.
func (r *Ruler) azureDoc(item json.RawMessage) (rules.Doc, bool, error) {
	var head struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(item, &head); err != nil {
		return rules.Doc{}, false, fmt.Errorf("an Azure rule group: %w", err)
	}
	ns, name, ok := r.azureOwn(head.ID)
	if !ok || name != head.Name {
		return rules.Doc{}, false, nil // not one of --ruler's: not read
	}
	b, err := rules.CanonJSON(item)
	if err != nil {
		return rules.Doc{}, false, fmt.Errorf("the Azure rule group %s: %w", name, err)
	}
	return rules.Doc{Source: head.ID, Namespace: ns, Data: b}, true, nil
}

// azureOwn reports whether a rule group's id is under --ruler's scope, and
// of the form a rule group's is, in any letter case, and gives its
// resource group and name.
func (r *Ruler) azureOwn(id string) (rg, name string, ok bool) {
	m := azureID.FindStringSubmatch(id)
	sub, scope := r.azureScope()
	if m == nil || !strings.EqualFold(m[1], sub) || (scope != "" && !strings.EqualFold(m[2], scope)) {
		return "", "", false
	}
	return m[2], m[3], true
}

func (r *Ruler) azureURL(rg, name string) string {
	sub, _ := r.azureScope()
	return r.azureBase() + "/subscriptions/" + url.PathEscape(sub) + "/resourceGroups/" + url.PathEscape(rg) + "/" + azureType + "/" +
		url.PathEscape(name) + "?api-version=" + azureAPI
}

// Unit is what one write replaces: a Mimir or Cortex group, an Amazon
// namespace, an Azure rule group; read from the ruler as it is now.
type Unit struct {
	Namespace, Group string
	// Source is the document the unit is in, as Read names it: a
	// namespace's URL, its ARN, a rule group's id.
	Source string
	// Raw is the unit as Lapilli keeps it: a Mimir or Cortex group as a
	// rule file of that one group ("groups: []" when it is gone), an Amazon
	// namespace's rule file as it was uploaded, an Azure rule group's
	// resource as JSON with its keys in order.
	Raw  []byte
	Hash string
	Gone bool
	// Status and StatusReason are an Amazon namespace's; Tags an Amazon
	// namespace's or an Azure rule group's.
	Status, StatusReason string
	Tags                 map[string]string
	// At is where it is read and written.
	At string
}

// Gone is a Mimir or Cortex group that isn't there, as a rule file.
const goneGroup = "groups: []\n"

// Unit reads one unit again from the ruler: the group (Mimir, Cortex), the
// namespace (Amazon) or the rule group (Azure) a rule of a namespace and a
// group is in.
func (r *Ruler) Unit(ctx context.Context, ns, group string) (Unit, error) {
	u := Unit{Namespace: ns, Group: group}
	if err := r.NameOK(ns, true); err != nil {
		return u, err
	}
	if err := r.NameOK(group, false); err != nil {
		return u, err
	}
	switch r.Kind {
	case RulerConfig:
		u.Source = r.URL + "/" + url.PathEscape(ns)
		u.At = u.Source + "/" + url.PathEscape(group)
		code, body, err := r.Call(ctx, http.MethodGet, u.At, nil, "", r.Reads, UnitLimit)
		if err != nil {
			return u, err
		}
		switch code {
		case http.StatusOK:
			if u.Raw, err = WrapGroup(body); err != nil {
				return u, err
			}
		case http.StatusNotFound:
			u.Raw, u.Gone = []byte(goneGroup), true
		default:
			return u, Failed(code, body)
		}
		u.Hash, err = rules.TreeHash(u.Raw)
		return u, err
	case RulerAmazon:
		u.At = r.amazonURL(ns)
		code, body, err := r.Call(ctx, http.MethodGet, u.At, nil, "", r.Reads, UnitLimit)
		if err != nil {
			return u, err
		}
		if code == http.StatusNotFound {
			u.Gone = true
			return u, nil
		}
		if code != http.StatusOK {
			return u, Failed(code, body)
		}
		d, data, err := AmazonDescribed(body)
		if err != nil {
			return u, err
		}
		if d.Name != ns {
			return u, fmt.Errorf("DescribeRuleGroupsNamespace answered for %q with %q", ns, d.Name)
		}
		if err := r.Owns(d.ARN, ns, group); err != nil {
			return u, err
		}
		u.Source, u.Raw, u.Hash = d.ARN, data, rules.Hash(data)
		u.Status, u.StatusReason, u.Tags = d.Status.Code, d.Status.Reason, d.Tags
		return u, nil
	case RulerAzure:
		u.At = r.azureURL(ns, group)
		if _, rg := r.azureScope(); rg != "" && !strings.EqualFold(rg, ns) {
			return u, fmt.Errorf("the rule group is in the resource group %s, not --ruler's", ns)
		}
		code, body, err := r.Call(ctx, http.MethodGet, u.At, nil, "", r.Reads, UnitLimit)
		if err != nil {
			return u, err
		}
		if code == http.StatusNotFound {
			u.Gone = true
			return u, nil
		}
		if code != http.StatusOK {
			return u, Failed(code, body)
		}
		var head struct {
			ID   string            `json:"id"`
			Tags map[string]string `json:"tags"`
		}
		if err := json.Unmarshal(body, &head); err != nil {
			return u, fmt.Errorf("the Azure rule group %s: %w", group, err)
		}
		if err := r.Owns(head.ID, ns, group); err != nil {
			return u, err
		}
		if u.Raw, err = rules.CanonJSON(body); err != nil {
			return u, fmt.Errorf("the Azure rule group %s: %w", group, err)
		}
		u.Source, u.Tags = head.ID, head.Tags
		u.Hash, err = AzureHash(u.Raw)
		return u, err
	}
	return u, fmt.Errorf("a ruler of kind %q", r.Kind)
}

// AzureSets are what Azure sets on a rule group itself: left out of what
// is hashed, and of what is written.
var AzureSets = []string{"id", "name", "type", "systemData"}

// AzureHash is what an Azure rule group hashes to, but for what Azure sets.
func AzureHash(raw []byte) (string, error) {
	b, err := AzureBody(raw)
	if err != nil {
		return "", err
	}
	return rules.TreeHash(b)
}

// AzureBody is an Azure rule group as it is written: whole, but for what
// Azure sets.
func AzureBody(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, errors.New("an Azure rule group that isn't a JSON object")
	}
	for _, k := range AzureSets {
		delete(m, k)
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Owns checks that a unit's source is this ruler's, for its namespace and
// group: a proposal is written where it was read, and a unit read is the
// one asked for.
func (r *Ruler) Owns(source, ns, group string) error {
	switch r.Kind {
	case RulerConfig:
		if source == r.URL+"/"+url.PathEscape(ns) {
			return nil
		}
	case RulerAmazon:
		m := amazonARN.FindStringSubmatch(source)
		u, _ := url.Parse(r.URL)
		var ws string
		if w := amazonPath.FindStringSubmatch(u.Path); w != nil {
			ws = w[1]
		}
		_, region, _ := cloud.AWSHost(u.Hostname())
		if m != nil && m[1] == region && m[2] == ws && m[3] == ns {
			return nil
		}
	case RulerAzure:
		if rg, name, ok := r.azureOwn(source); ok && strings.EqualFold(rg, ns) && name == group {
			return nil
		}
	}
	return ErrElsewhere
}

// ErrElsewhere is a unit, or a proposal's source, that isn't --ruler's for
// its namespace and group.
var ErrElsewhere = errors.New("the rules were read from another ruler, or another namespace or group, than --ruler's: propose it again")

// WriteAt is where a unit is written: Mimir's or Cortex's namespace, which
// a group is posted to, or the unit itself.
func (r *Ruler) WriteAt(u Unit) string {
	if r.Kind == RulerConfig {
		return u.Source
	}
	return u.At
}

// WrapGroup is a Mimir or Cortex group, as GET .../NAMESPACE/GROUP answers
// it, as a rule file of that one group, read back to check it reads alike.
func WrapGroup(group []byte) ([]byte, error) {
	var b strings.Builder
	b.WriteString("groups:\n")
	first := true
	for _, l := range strings.SplitAfter(string(group), "\n") {
		switch {
		case l == "":
		case strings.TrimSpace(l) == "":
			b.WriteString(l)
		case first:
			b.WriteString("  - " + l)
			first = false
		default:
			b.WriteString("    " + l)
		}
	}
	out := b.String()
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	var g, w any
	if err := yaml.Unmarshal(group, &g); err != nil {
		return nil, errors.New("the ruler's group isn't YAML")
	}
	if _, ok := g.(map[string]any); !ok {
		return nil, errors.New("the ruler's answer isn't a group")
	}
	if err := yaml.Unmarshal([]byte(out), &w); err != nil {
		return nil, errors.New("the ruler's group can't be read as a rule file")
	}
	gs, _ := w.(map[string]any)["groups"].([]any)
	if len(gs) != 1 || !reflect.DeepEqual(gs[0], g) {
		return nil, errors.New("the ruler's group can't be read as a rule file")
	}
	return []byte(out), nil
}

// GroupOf is the one group of a rule file WrapGroup made, as it is posted
// to Mimir or Cortex; ok is false when the file holds none, as when the
// group went with its last rule.
func GroupOf(file []byte) ([]byte, bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(file, &doc); err != nil || len(doc.Content) != 1 {
		return nil, false, errors.New("not a rule file of one group")
	}
	var groups *yaml.Node
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "groups" {
			groups = root.Content[i+1]
		}
	}
	switch {
	case groups == nil || groups.Kind != yaml.SequenceNode || len(root.Content) != 2:
		return nil, false, errors.New("not a rule file of one group")
	case len(groups.Content) == 0:
		return nil, false, nil
	case len(groups.Content) > 1:
		return nil, false, errors.New("not a rule file of one group")
	}
	b, err := encodeYAML(groups.Content[0])
	if err != nil {
		return nil, false, err
	}
	// Read back: the group as the file has it.
	var a, w any
	if yaml.Unmarshal(b, &a) != nil {
		return nil, false, errors.New("the group can't be written out")
	}
	yaml.Unmarshal(file, &w)
	gs, _ := w.(map[string]any)["groups"].([]any)
	if len(gs) != 1 || !reflect.DeepEqual(gs[0], a) {
		return nil, false, errors.New("the group can't be written out as it reads")
	}
	return b, true, nil
}

// ResourceGroup is the resource group --ruler names on Azure, "" when it
// names a subscription.
func (r *Ruler) ResourceGroup() string {
	if r.Kind != RulerAzure {
		return ""
	}
	_, rg := r.azureScope()
	return rg
}
