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
	// MaxSamples, when set, is the most samples the answers to one range
	// may hold in all (--max-samples; prom.DefaultMaxSamples).
	MaxSamples int
	// Limit, when set, is the Limiter every client of the Loki shares, its
	// queries' and its ruler's: one is made of MaxRate otherwise.
	Limit *prom.Limiter
}

// noRedirects is a client that follows no redirect, so a token goes
// nowhere it wasn't sent; and that gives up on a request after five
// minutes, as prom's does.
var noRedirects = &http.Client{
	Timeout:       5 * time.Minute,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// limiter is the Limiter of the Loki's clients.
func (l Loki) limiter() *prom.Limiter {
	if l.Limit != nil {
		return l.Limit
	}
	return prom.NewLimiter(l.MaxRate)
}

// History is a client for the Loki's query API.
func (l Loki) History(agent string) *prom.Client {
	opts := []prom.Option{prom.Loki(), prom.WithHTTPClient(noRedirects), prom.WithLimiter(l.limiter()), prom.WithMaxSamples(l.MaxSamples)}
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
// when not nil, is the data source's own Limiter: Grafana passes on what
// each data source answers, Retry-After included, so each pushes back of
// its own; the data sources of a Grafana share a Pace (--max-rate). more
// are options besides (prom.WithMaxSamples).
func GrafanaLoki(base, tokenFile, agent, uid string, limit *prom.Limiter, more ...prom.Option) *prom.Client {
	opts := append([]prom.Option{prom.Loki(), prom.WithHTTPClient(noRedirects)}, more...)
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
	rs, docs, err := LokiRules(ctx, l, agent, warn)
	if err != nil {
		return nil, nil, nil, err
	}
	places, err := LokiPlaces(ctx, l, agent)
	if err != nil {
		return nil, nil, nil, err
	}
	Placed(rs, places)
	return rs, docs, places, nil
}

// LokiRules reads the rules a Loki's ruler holds, as LokiLoaded does, but
// not where it runs each group.
func LokiRules(ctx context.Context, l Loki, agent string, warn func(string)) ([]rules.Rule, []rules.Doc, error) {
	r := &Ruler{URL: strings.TrimRight(l.URL, "/") + "/loki/api/v1/rules", Kind: RulerConfig, Tenant: l.Tenant, Agent: agent,
		Reads: Creds{TokenFile: l.TokenFile}, Warn: warn, Flag: "--loki-loaded-rules"}
	rs, docs, err := r.Read(ctx)
	if err != nil {
		return nil, nil, err
	}
	// Read again as a Loki's, as the ledger reads them again: what the API
	// gives is as Loki holds it.
	for i := range docs {
		docs[i].Logs = rules.LokiRuler
	}
	if rs, err = rules.FromDocs(docs); err != nil {
		return nil, nil, fmt.Errorf("--loki-loaded-rules: %w", err)
	}
	return rs, docs, nil
}

// Placed sets each rule's Phase, where in its interval its group is run,
// from places.
func Placed(rs []rules.Rule, places map[rules.Place]time.Duration) {
	for i := range rs {
		if d, ok := places[rules.Place{Namespace: rs[i].Namespace, Group: rs[i].Group}]; ok {
			rs[i].Phase = d
		}
	}
}

// LokiPlaces asks a Loki's ruler where it runs each group within its
// interval: by l.Limit, as its queries are asked, and asked again as they
// are.
func LokiPlaces(ctx context.Context, l Loki, agent string) (map[rules.Place]time.Duration, error) {
	return lokiPlaces(l.History(agent).Rules(ctx))
}

// LokiPlacesOnce asks it as a read lapilli can do without: once, within
// 30 seconds, not while the Loki said to wait, and without its answer
// changing how the Loki is asked (prom's RulesOnce). lapilli serve reads
// them so each minute, between its replays' queries, and keeps those it
// read before when it can't.
func LokiPlacesOnce(ctx context.Context, l Loki, agent string) (map[rules.Place]time.Duration, error) {
	return lokiPlaces(l.History(agent).RulesOnce(ctx))
}

func lokiPlaces(b []byte, err error) (map[rules.Place]time.Duration, error) {
	if err != nil {
		return nil, fmt.Errorf("--loki-loaded-rules: %w", err)
	}
	places, err := rules.Places(b)
	if err != nil {
		return nil, fmt.Errorf("--loki-loaded-rules: %w", err)
	}
	return places, nil
}
