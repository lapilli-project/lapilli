package preflight

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"lapilli.dev/internal/cloud"
	"lapilli.dev/internal/serve"
	"lapilli.dev/internal/source"
)

// ErrOffList is a request the allowlist refused, before it was sent.
var ErrOffList = errors.New("lapilli preflight doesn't send this request: it isn't one of the reads it makes")

// allow is a request the preflight may send: its method; its URL's scheme
// and host, and its path, or the start of a path for a read under it; and
// what its query, a POST's form and its headers must be.
type allow struct {
	api    string
	method string
	host   string // scheme://host, in lower case
	path   string
	under  bool
	// hostIs and pathIs, when set, stand for host and path: the credential
	// endpoints a cloud has many of (STS's, by region).
	hostIs, pathIs *regexp.Regexp
	query          func(url.Values) bool // nil: no query
	form           func(url.Values) bool // a POST's form; nil: none
	header         func(http.Header) bool
}

// allowlist is every request the preflight may send. Anything else is
// refused before it is sent, and counted.
type allowlist struct {
	mu      sync.Mutex
	allows  []allow
	by      map[string]int
	refused int
	now     func() time.Time
}

func newAllowlist(now func() time.Time) *allowlist {
	return &allowlist{by: map[string]int{}, now: now}
}

func (a *allowlist) add(x allow) {
	a.mu.Lock()
	a.allows = append(a.allows, x)
	a.mu.Unlock()
}

// check lets a request go when an allow holds it, counted by its API;
// refuses it otherwise.
func (a *allowlist) check(req *http.Request) error {
	host := strings.ToLower(req.URL.Scheme + "://" + req.URL.Host)
	var form url.Values
	if req.Method == http.MethodPost && req.GetBody != nil && strings.HasPrefix(req.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		if b, err := req.GetBody(); err == nil {
			raw, _ := io.ReadAll(io.LimitReader(b, 1<<20))
			b.Close()
			form, _ = url.ParseQuery(string(raw))
		}
	}
	q := req.URL.Query()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, x := range a.allows {
		switch {
		case x.method != req.Method:
		case x.hostIs == nil && x.host != host, x.hostIs != nil && !x.hostIs.MatchString(host):
		case x.pathIs != nil && !x.pathIs.MatchString(req.URL.Path):
		case x.pathIs == nil && !x.under && req.URL.Path != x.path:
		case x.pathIs == nil && x.under && req.URL.Path != x.path && !strings.HasPrefix(req.URL.Path, strings.TrimSuffix(x.path, "/")+"/"):
		case x.query == nil && req.URL.RawQuery != "":
		case x.query != nil && !x.query(q):
		case x.form != nil && (form == nil || !x.form(form)):
		case x.form == nil && form != nil:
		case x.header != nil && !x.header(req.Header):
		default:
			a.by[x.api]++
			return nil
		}
	}
	a.refused++
	return ErrOffList
}

func (a *allowlist) asked() Asked {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := Asked{By: map[string]int{}, Refused: a.refused}
	for k, n := range a.by {
		out.By[k] = n
		out.Requests += n
	}
	return out
}

// at splits a URL into the allow's host and path, the path without a
// trailing slash; ok false for one that isn't http(s).
func at(raw string) (host, path string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", "", false
	}
	return strings.ToLower(u.Scheme + "://" + u.Host), strings.TrimRight(u.Path, "/"), true
}

// exactly is a query of these keys, each with this one value, and no other.
func exactly(kv ...string) func(url.Values) bool {
	return func(q url.Values) bool {
		if len(q) != len(kv)/2 {
			return false
		}
		for i := 0; i+1 < len(kv); i += 2 {
			if vs := q[kv[i]]; len(vs) != 1 || vs[0] != kv[i+1] {
				return false
			}
		}
		return true
	}
}

// anyQuery is any query: a read under a ruler's or a cloud's API, which
// pages.
func anyQuery(url.Values) bool { return true }

// recent reports whether a time, as a Prometheus or Loki API takes one
// (seconds, or Loki's nanoseconds), is within the last ten minutes.
func (a *allowlist) recent(s string, nanos bool) bool {
	now := a.now()
	var t time.Time
	if nanos {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return false
		}
		t = time.Unix(0, n)
	} else {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return false
		}
		t = time.UnixMilli(int64(f * 1000))
	}
	return !t.Before(now.Add(-10*time.Minute)) && !t.After(now.Add(time.Minute))
}

// promQuery is a range query of exactly expr, over the last ten minutes:
// the form prom's client sends.
func (a *allowlist) promQuery(expr string) func(url.Values) bool {
	return func(f url.Values) bool {
		if len(f) != 4 || f.Get("query") != expr || len(f["query"]) != 1 || f.Get("step") == "" {
			return false
		}
		return a.recent(f.Get("start"), false) && a.recent(f.Get("end"), false)
	}
}

// lokiRecent is a query of the last ten minutes by Loki's keys alone.
func (a *allowlist) lokiRecent(q url.Values) bool {
	return len(q) == 2 && len(q["start"]) == 1 && len(q["end"]) == 1 && a.recent(q.Get("start"), true) && a.recent(q.Get("end"), true)
}

// The PromQL the preflight sends: the cheapest query every
// Prometheus-compatible API answers, and the day's firings.
const vectorOne = "vector(1)"

// alertsCount is how many samples each alert name fired over the last
// day, of a series of what really fired.
func alertsCount(series string) string {
	return "sum by (alertname) (count_over_time(" + series + "[1d]))"
}

// allowAll allows what the preflight will read, as far as the flags say:
// the reads of what it learns as it goes (Grafana's data sources, the
// sign-in's keys, the on-call feeds) are allowed when it learns them.
func (p *run) allowAll() {
	c, a := p.c, p.al
	if base := p.historyBase(); base != "" {
		if host, path, ok := at(base); ok {
			for _, expr := range []string{vectorOne, alertsCount(serve.AlertsSeries), alertsCount(serve.GrafanaAlertsSeries)} {
				a.add(allow{api: "prometheus", method: http.MethodPost, host: host, path: path + "/api/v1/query_range", form: a.promQuery(expr)})
			}
			a.add(allow{api: "prometheus", method: http.MethodGet, host: host, path: path + "/api/v1/rules", query: exactly("type", "alert")})
			a.add(allow{api: "prometheus", method: http.MethodGet, host: host, path: path + "/api/v1/status/config"})
			a.add(allow{api: "prometheus", method: http.MethodGet, host: host, path: path + "/api/v1/targets", query: exactly("state", "active")})
		}
	}
	if host, path, ok := at(c.Serve.Loki); ok {
		a.add(allow{api: "loki", method: http.MethodGet, host: host, path: path + "/loki/api/v1/labels", query: a.lokiRecent})
		a.add(allow{api: "loki", method: http.MethodGet, host: host, path: path + "/prometheus/api/v1/rules", query: exactly("type", "alert")})
	}
	if host, path, ok := at(c.Serve.Grafana); ok {
		a.add(allow{api: "grafana", method: http.MethodGet, host: host, path: path + "/api/v1/provisioning/alert-rules/export", query: exactly("format", "yaml")})
		for _, x := range []string{"templates", "contact-points", "policies"} {
			a.add(allow{api: "grafana", method: http.MethodGet, host: host, path: path + "/api/v1/provisioning/" + x})
		}
	}
	if host, path, ok := at(c.Serve.Ruler); ok {
		a.add(allow{api: "ruler", method: http.MethodGet, host: host, path: path, under: true, query: anyQuery})
	}
	for _, x := range c.Serve.Alarms {
		host, path, ok := at(x.URL)
		if !ok {
			continue
		}
		api := "alarms:" + x.Source
		if kind, _ := source.AlarmsKind(x.URL); kind == source.AlarmsAWS {
			// CloudWatch's API takes its action in a header: the read alone.
			a.add(allow{api: api, method: http.MethodPost, host: host, path: path + "/", header: func(h http.Header) bool {
				return h.Get("X-Amz-Target") == "GraniteServiceVersion20100801.DescribeAlarms"
			}})
			continue
		}
		if kind, _ := source.AlarmsKind(x.URL); kind == source.AlarmsOCI || kind == source.AlarmsAzure {
			// OCI's alarms are listed at their path's root; Azure's under
			// the subscription the URL names.
			path = ociOrAzureRoot(kind, path)
		}
		a.add(allow{api: api, method: http.MethodGet, host: host, path: path, under: true, query: anyQuery})
	}
	if o := c.Serve.OIDC; o != nil {
		if host, path, ok := at(o.Issuer); ok {
			a.add(allow{api: "signin", method: http.MethodGet, host: host, path: path + "/.well-known/openid-configuration"})
		}
	}
	if px := c.Serve.Proxy; px != nil {
		keys := px.JWKS
		if px.Kind == "google-iap" {
			keys = "https://www.gstatic.com/iap/verify/public_key-jwk"
		}
		p.allowURL("signin", keys)
	}
	p.allowCredentials()
}

// ociOrAzureRoot is where a cloud's alarms are read under: OCI's, its
// alarms' path; Azure's, the subscription's.
func ociOrAzureRoot(kind, path string) string {
	if kind == source.AlarmsOCI {
		return "/20180401/alarms"
	}
	if m := regexp.MustCompile(`^(/subscriptions/[^/]+)`).FindString(path); m != "" {
		return m
	}
	return path
}

// allowURL allows a GET of exactly this URL, its query too.
func (p *run) allowURL(api, raw string) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return
	}
	q := u.Query()
	p.al.add(allow{api: api, method: http.MethodGet, host: strings.ToLower(u.Scheme + "://" + u.Host), path: u.Path,
		query: func(got url.Values) bool {
			if len(q) == 0 {
				return len(got) == 0
			}
			if len(got) != len(q) {
				return false
			}
			for k, vs := range q {
				if !slices.Equal(got[k], vs) {
					return false
				}
			}
			return true
		}})
}

// historyBase is the history's API, as serve asks it: --prometheus, or
// Google Cloud's for --gcp-project; "" without either.
func (p *run) historyBase() string {
	if p.c.History.GCPProject != "" {
		return "https://monitoring.googleapis.com/v1/projects/" + url.PathEscape(p.c.History.GCPProject) + "/location/global/prometheus"
	}
	return p.c.History.URL
}

// The credential endpoints of the clouds serve signs for, matched by host
// and action: what makes a credential, never what writes. Each cloud's
// when its credentials are used: --auth, --ruler, --alarms or
// --changes-*.
var (
	stsHost   = regexp.MustCompile(`^https://sts(-fips)?(\.[a-z0-9-]+)?\.amazonaws\.com(\.cn)?$`)
	stsAction = []string{"AssumeRole", "AssumeRoleWithWebIdentity", "GetCallerIdentity"}
	entraHost = []string{"https://login.microsoftonline.com", "https://login.microsoftonline.us", "https://login.chinacloudapi.cn"}
	entraPath = regexp.MustCompile(`^/[A-Za-z0-9.-]+/(oauth2/(v2\.0/)?token|v2\.0/\.well-known/openid-configuration)$`)
)

// clouds are the clouds whose credentials the flags use.
func (p *run) clouds() map[string]bool {
	s := p.c.Serve
	out := map[string]bool{}
	if k := p.c.History.Auth; k != "" {
		out[k] = true
	}
	if p.c.History.GCPProject != "" {
		out["google"] = true
	}
	if s.Ruler != "" {
		switch k, _ := source.RulerKind(s.Ruler); k {
		case source.RulerAmazon:
			out["aws"] = true
		case source.RulerAzure:
			out["azure"] = true
		}
	}
	for _, x := range s.Alarms {
		switch k, _ := source.AlarmsKind(x.URL); k {
		case source.AlarmsAWS:
			out["aws"] = true
		case source.AlarmsAzure:
			out["azure"] = true
		case source.AlarmsGoogle:
			out["google"] = true
		case source.AlarmsOCI:
			out["oci"] = true
		}
	}
	for k, on := range map[string]bool{"aws": s.ChangesAWSRole != "", "azure": s.ChangesAzureClientID != "",
		"google": s.ChangesGoogleServiceAccount != "", "oci": s.ChangesOCIConfig != ""} {
		if on {
			out[k] = true
		}
	}
	return out
}

func (p *run) allowCredentials() {
	a := p.al
	clouds := p.clouds()
	imds := []string{"http://169.254.169.254", "http://[fd00:ec2::254]"}
	if clouds["aws"] {
		sts := func(f url.Values) bool { return slices.Contains(stsAction, f.Get("Action")) && len(f["Action"]) == 1 }
		a.add(allow{api: "credentials", method: http.MethodPost, hostIs: stsHost, path: "/", form: sts})
		for _, env := range []string{"AWS_ENDPOINT_URL_STS", "AWS_ENDPOINT_URL"} {
			if host, path, ok := at(os.Getenv(env)); ok {
				a.add(allow{api: "credentials", method: http.MethodPost, host: host, path: path + "/", form: sts})
			}
		}
		for _, h := range imds {
			a.add(allow{api: "credentials", method: http.MethodPut, host: h, path: "/latest/api/token"})
			a.add(allow{api: "credentials", method: http.MethodGet, host: h, path: "/latest/meta-data/iam/security-credentials", under: true})
		}
		// EKS Pod Identity's and ECS's endpoints, where the environment
		// names them.
		if raw := os.Getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI"); raw != "" {
			p.allowURL("credentials", raw)
		}
		if rel := os.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"); rel != "" {
			p.allowURL("credentials", "http://169.254.170.2"+rel)
		}
	}
	if clouds["azure"] {
		hosts := slices.Clone(entraHost)
		if h, _, ok := at(os.Getenv("AZURE_AUTHORITY_HOST")); ok {
			hosts = append(hosts, h)
		}
		for _, h := range hosts {
			a.add(allow{api: "credentials", method: http.MethodPost, host: h, pathIs: entraPath, form: anyQuery})
			a.add(allow{api: "credentials", method: http.MethodGet, host: h, pathIs: entraPath})
			a.add(allow{api: "credentials", method: http.MethodGet, host: h, path: "/common/discovery/instance", query: anyQuery})
		}
		for _, h := range imds {
			a.add(allow{api: "credentials", method: http.MethodGet, host: h, path: "/metadata/identity/oauth2/token", query: anyQuery})
		}
		if raw := os.Getenv("IDENTITY_ENDPOINT"); raw != "" {
			if host, path, ok := at(raw); ok {
				a.add(allow{api: "credentials", method: http.MethodGet, host: host, path: path, query: anyQuery})
			}
		}
	}
	if clouds["google"] {
		a.add(allow{api: "credentials", method: http.MethodPost, host: "https://oauth2.googleapis.com", path: "/token", form: anyQuery})
		a.add(allow{api: "credentials", method: http.MethodPost, host: "https://sts.googleapis.com", path: "/v1/token", form: anyQuery})
		if host, path, ok := at(cloud.IAMCredentials); ok {
			a.add(allow{api: "credentials", method: http.MethodPost, host: host, path: path + "/v1/projects/-/serviceAccounts", under: true,
				pathIs: regexp.MustCompile(`^` + regexp.QuoteMeta(path) + `/v1/projects/-/serviceAccounts/[^/]+:generateAccessToken$`)})
		}
		for _, h := range []string{"http://metadata.google.internal", "http://169.254.169.254"} {
			a.add(allow{api: "credentials", method: http.MethodGet, host: h, path: "/computeMetadata/v1", under: true, query: anyQuery})
		}
	}
}
