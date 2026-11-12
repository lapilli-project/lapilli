package source

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// Loki is a Loki whose API rules written in LogQL are replayed against
// (docs/design/loki.md): --loki, with its token and tenant.
type Loki struct {
	URL string
	// TokenFile holds a bearer token, read again for each request.
	TokenFile string
	// Tenant, when set, is sent as X-Scope-OrgID.
	Tenant string
	// MaxRate, when set, is how many requests a second it is asked at most
	// (--max-rate).
	MaxRate float64
}

// noRedirects is a client that follows no redirect, so a token goes
// nowhere it wasn't sent.
var noRedirects = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// History is a client for the Loki's query API.
func (l Loki) History(agent string) *prom.Client {
	opts := []prom.Option{prom.Loki(), prom.WithHTTPClient(noRedirects), prom.WithLimiter(prom.NewLimiter(l.MaxRate))}
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
// rules. Grafana takes only GET there, which is how Loki is asked. limit,
// when not nil, is shared by every data source of the Grafana, which all
// go through it.
func GrafanaLoki(base, tokenFile, agent, uid string, limit *prom.Limiter) *prom.Client {
	opts := []prom.Option{prom.Loki(), prom.WithHTTPClient(noRedirects)}
	if limit != nil {
		opts = append(opts, prom.WithLimiter(limit))
	}
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

// grafanaUID is a Grafana data source's uid, as Grafana makes and takes
// one: what goes into the proxy's path is no more than that.
var grafanaUID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

// For is the history a rule written in LogQL is replayed against.
func (l *Logs) For(r rules.Rule) (replay.History, error) {
	if r.Graph != nil && l.Grafana != nil {
		if !grafanaUID.MatchString(r.Logs) {
			return nil, errors.New("its Loki data source's uid isn't one Grafana makes, so it isn't asked")
		}
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
// --loki. Loki 3.7.9 drops a group's query_offset and a rule's
// keep_firing_for when it reads a file, and runs the rule without them:
// so is it replayed, and notes say which rules had them.
func LokiFiles(path string) (rs []rules.Rule, docs []rules.Doc, notes []string, err error) {
	written, docs, err := rules.Read(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("--loki-rules %w", err)
	}
	for _, r := range written {
		if r.KeepFiringFor != 0 || r.QueryOffset != 0 {
			notes = append(notes, fmt.Sprintf("--loki-rules: %s has keep_firing_for or its group query_offset, which Loki 3.7.9 drops and doesn't run: it is replayed without them", verdict.Clean(r.Name)))
		}
	}
	for i := range docs {
		docs[i].Logs = rules.LokiFile
	}
	// Read again as a Loki's, as the ledger reads them again.
	if rs, err = rules.FromDocs(docs); err != nil {
		return nil, nil, nil, fmt.Errorf("--loki-rules %w", err)
	}
	for _, r := range rs {
		if r.Graph != nil || r.Cloud != "" {
			return nil, nil, nil, fmt.Errorf("--loki-rules %s: %s isn't a Loki ruler's rule: give it with --rules, --grafana or --alarms", path, r.Name)
		}
	}
	return rs, docs, notes, nil
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
	// Read again as a Loki's, as the ledger reads them again: what the API
	// gives is as Loki holds it.
	for i := range docs {
		docs[i].Logs = rules.LokiRuler
	}
	if rs, err = rules.FromDocs(docs); err != nil {
		return nil, nil, nil, fmt.Errorf("--loki-loaded-rules: %w", err)
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
