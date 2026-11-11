package source

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
)

// Loki is a Loki whose API rules written in LogQL are replayed against
// (docs/design/loki.md): --loki, with its token and tenant.
type Loki struct {
	URL string
	// TokenFile holds a bearer token, read again for each request.
	TokenFile string
	// Tenant, when set, is sent as X-Scope-OrgID.
	Tenant string
}

// noRedirects is a client that follows no redirect, so a token goes
// nowhere it wasn't sent.
var noRedirects = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// History is a client for the Loki's query API.
func (l Loki) History(agent string) *prom.Client {
	opts := []prom.Option{prom.Loki(), prom.WithHTTPClient(noRedirects)}
	if l.Tenant != "" {
		opts = append(opts, prom.WithHeader("X-Scope-OrgID", l.Tenant))
	}
	if l.TokenFile != "" {
		opts = append(opts, prom.WithToken(tokenFrom(l.TokenFile)))
	}
	return prom.New(l.URL, agent, opts...)
}

// tokenFrom reads a token from a file for each request, so a rotated one
// is picked up.
func tokenFrom(file string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		b, err := os.ReadFile(file)
		return strings.TrimSpace(string(b)), err
	}
}

// GrafanaLoki is a client for a Grafana's Loki data source, through
// Grafana's data source proxy: the Loki, the tenant and the credentials
// Grafana asks when it evaluates a rule, with the token that reads the
// rules. Grafana takes only GET there, which is how Loki is asked.
func GrafanaLoki(base, tokenFile, agent, uid string) *prom.Client {
	opts := []prom.Option{prom.Loki(), prom.WithHTTPClient(noRedirects)}
	if tokenFile != "" {
		opts = append(opts, prom.WithToken(tokenFrom(tokenFile)))
	}
	return prom.New(strings.TrimRight(base, "/")+"/api/datasources/proxy/uid/"+url.PathEscape(uid), agent, opts...)
}

// Logs is where rules written in LogQL are asked: a Loki ruler's of
// --loki; a Grafana rule's through --grafana's data source proxy, or, with
// no --grafana, of --loki.
type Logs struct {
	// Loki is --loki's history, nil without it.
	Loki replay.History
	// Grafana is a Grafana Loki data source's history, by its uid, nil
	// without --grafana: GrafanaLoki, but for tests.
	Grafana func(uid string) replay.History

	mu    sync.Mutex
	proxy map[string]replay.History
}

// For is the history a rule written in LogQL is replayed against.
func (l *Logs) For(r rules.Rule) (replay.History, error) {
	if r.Graph != nil && l.Grafana != nil {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.proxy == nil {
			l.proxy = map[string]replay.History{}
		}
		h := l.proxy[r.Logs]
		if h == nil {
			h = l.Grafana(r.Logs)
			l.proxy[r.Logs] = h
		}
		return h, nil
	}
	if l.Loki == nil {
		return nil, errors.New("it is LogQL: give --loki URL to replay it")
	}
	return l.Loki, nil
}

// NoLoki and NoGrafanaLoki say which rules written in LogQL there is no
// Loki to ask for, as rules.Replaying takes them.
func (l *Logs) NoLoki() bool        { return l == nil || l.Loki == nil }
func (l *Logs) NoGrafanaLoki() bool { return l == nil || l.Loki == nil && l.Grafana == nil }

// LokiFiles reads Loki ruler rule files, or the files of a directory, as
// rules.Read reads Prometheus's: each rule's queries are LogQL, asked of
// --loki.
func LokiFiles(path string) ([]rules.Rule, []rules.Doc, error) {
	_, docs, err := rules.Read(path)
	if err != nil {
		return nil, nil, fmt.Errorf("--loki-rules %w", err)
	}
	for i := range docs {
		docs[i].Logs = rules.LokiRuler
	}
	// Read again as a Loki's, as the ledger reads them again.
	rs, err := rules.FromDocs(docs)
	if err != nil {
		return nil, nil, fmt.Errorf("--loki-rules %w", err)
	}
	for _, r := range rs {
		if r.Graph != nil || r.Cloud != "" {
			return nil, nil, fmt.Errorf("--loki-rules %s: %s isn't a Loki ruler's rule: give it with --rules, --grafana or --alarms", path, r.Name)
		}
	}
	return rs, docs, nil
}

// LokiLoaded reads the rules a Loki's ruler holds: as written, a document
// a namespace, from its configuration API (/loki/api/v1/rules), which
// says each rule's keep_firing_for and query_offset; and where it runs each
// group, from its Prometheus-shaped API (/prometheus/api/v1/rules), which
// doesn't say those but says when each group last ran.
func LokiLoaded(ctx context.Context, l Loki, agent string, warn func(string)) ([]rules.Rule, []rules.Doc, map[rules.Place]time.Duration, error) {
	r := &Ruler{URL: strings.TrimRight(l.URL, "/") + "/loki/api/v1/rules", Kind: RulerConfig, Tenant: l.Tenant, Agent: agent,
		Reads: Creds{TokenFile: l.TokenFile}, Warn: warn, Flag: "--loki-loaded-rules"}
	rs, docs, err := r.Read(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	for i := range docs {
		docs[i].Logs = rules.LokiRuler
	}
	for i := range rs {
		rs[i].Logs = rules.LokiRuler
	}
	places, err := LokiPlaces(ctx, l, agent)
	if err != nil {
		return nil, nil, nil, err
	}
	for i := range rs {
		if d, ok := places[rules.Place{Namespace: rs[i].Namespace, Group: rs[i].Group}]; ok {
			rs[i].Phase = d
		}
	}
	return rs, docs, places, nil
}

// LokiPlaces asks a Loki's ruler where it runs each group within its
// interval.
func LokiPlaces(ctx context.Context, l Loki, agent string) (map[rules.Place]time.Duration, error) {
	b, err := l.History(agent).Rules(ctx)
	if err != nil {
		return nil, fmt.Errorf("--loki-loaded-rules: %w", err)
	}
	places, err := rules.Places(b)
	if err != nil {
		return nil, fmt.Errorf("--loki-loaded-rules: %w", err)
	}
	return places, nil
}
