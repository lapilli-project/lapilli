package preflight

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/source"
)

// With --queries, each replayable rule's expression is asked once, an
// instant query at now, of where it is replayed (docs/design/preflight.md):
// the Prometheus-compatible history (--prometheus, --gcp-project, a
// cloud's PromQL, the queries of Grafana's rules), --loki, and Grafana's
// Loki data sources through its proxy. Off by default: it is the one
// place a preflight sends an expression a person wrote. It writes
// nothing, and is held to the limits the replay keeps: the client's rate
// (--max-rate) and sample bound (--max-samples), --parallel at once, a
// time for each query (eachTime), and the whole preflight's
// (--preflight-timeout).

// The reasons a query fails, as a report says them.
const (
	reasonParse    = "parse error"
	reasonFunction = "unknown function"
	reasonSamples  = "too many samples"
	reasonTimeout  = "timeout"
	reasonLetIn    = "not let in"
	reasonOther    = "failed"
)

// queryGroup is what is asked of one place: an item of its own.
type queryGroup struct {
	id, what string
	// asks, by the expression and, of Grafana's Loki, the data source's
	// uid; order is the order they are asked in.
	asks  map[string]*queryAsk
	order []string
}

type queryAsk struct {
	expr, uid string
	// rules are the rules that ask it, in the order read.
	rules []string
}

func (g *queryGroup) add(rule, expr, uid string) {
	k := uid + "\x00" + expr
	a := g.asks[k]
	if a == nil {
		a = &queryAsk{expr: expr, uid: uid}
		g.asks[k] = a
		g.order = append(g.order, k)
	}
	if !slices.Contains(a.rules, rule) {
		a.rules = append(a.rules, rule)
	}
}

// queryGroups sorts the expressions of the rules a run replays by where
// they are asked: the history, --loki, Grafana's Loki data sources. A
// cloud's alarms have no expression to ask.
func (p *run) queryGroups(rd *read) (hist, loki, graf *queryGroup, noLoki, noHistory int) {
	asks := func() map[string]*queryAsk { return map[string]*queryAsk{} }
	hist = &queryGroup{id: "queries.history", what: "[--prometheus]", asks: asks()}
	loki = &queryGroup{id: "queries.loki", what: "[--loki]", asks: asks()}
	graf = &queryGroup{id: "queries.grafana", what: "Grafana's Loki data sources", asks: asks()}
	s := p.c.Serve
	for _, r := range replayable(rd.all) {
		if r.Cloud != "" {
			continue
		}
		for _, q := range replay.Queries(r) {
			switch {
			case q.Logs == "":
				if p.historyBase() == "" {
					noHistory++
					continue
				}
				hist.add(r.Name, q.Expr, "")
			case r.Graph != nil && s.Grafana != "":
				// Through Grafana's data source proxy, as the replay asks it
				// (source.Logs.For), by a uid Grafana makes; one that isn't
				// is not asked.
				if source.ValidUID(q.Logs) {
					graf.add(r.Name, q.Expr, q.Logs)
				}
			case s.Loki != "":
				loki.add(r.Name, q.Expr, "")
			default:
				noLoki++
			}
		}
	}
	return hist, loki, graf, noLoki, noHistory
}

// queries asks each rule's expression once, with --queries.
func (p *run) queries(ctx context.Context, rd *read) {
	if !p.c.Queries {
		return
	}
	if rd == nil || len(replayable(rd.all)) == 0 {
		p.add(Item{ID: "queries.history", State: Unknown, What: "no rule's expression asked: there are no rules to replay"})
		return
	}
	hist, loki, graf, noLoki, noHistory := p.queryGroups(rd)
	if noHistory > 0 {
		p.add(Item{ID: "queries.history", State: Unknown, What: fmt.Sprintf("%s of rules written in PromQL not asked: there is no --prometheus or --gcp-project", plural(noHistory, "query"))})
	}
	if noLoki > 0 {
		p.add(Item{ID: "queries.loki", State: Unknown, What: fmt.Sprintf("%s of rules written in LogQL not asked: there is no --loki", plural(noLoki, "query"))})
	}
	for _, g := range []*queryGroup{hist, loki, graf} {
		if len(g.asks) == 0 {
			continue
		}
		if p.c.Offline {
			p.offline(g.id, plural(len(g.asks), "expression")+" not asked of "+g.what)
			continue
		}
		p.ask(ctx, g)
	}
}

// ask asks each expression of a group once, as an instant query at now,
// --parallel at once, and says which rules' failed and why.
func (p *run) ask(ctx context.Context, g *queryGroup) {
	s := p.c.Serve
	ask, err := p.asker(ctx, g)
	if err != nil {
		why, _ := unreached(err, "--prometheus", "")
		p.add(Item{ID: g.id, State: Unknown, What: g.what + "'s client couldn't be made: no expression asked", Why: why})
		return
	}
	t := p.now.Truncate(time.Second)
	type result struct {
		err  error
		done bool
	}
	results := make([]result, len(g.order))
	// Allowed one by one, as exactly what is sent: the allowlist holds the
	// preflight to these expressions and no others.
	for _, k := range g.order {
		p.allowQuery(g, g.asks[k])
	}
	workers := max(1, s.Parallel)
	next := make(chan int)
	var wg sync.WaitGroup
	for range min(workers, len(g.order)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				a := g.asks[g.order[i]]
				each, cancel := context.WithTimeout(ctx, eachTime)
				_, err := ask(each, a, t)
				cancel()
				results[i] = result{err, true}
			}
		}()
	}
feed:
	for i := range g.order {
		select {
		case next <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(next)
	wg.Wait()
	var failed []Failed
	seen := map[[2]string]bool{}
	asked, unasked := 0, 0
	for i, k := range g.order {
		r := results[i]
		if !r.done {
			unasked++
			continue
		}
		asked++
		if r.err == nil {
			continue
		}
		reason := queryReason(r.err)
		for _, rule := range g.asks[k].rules {
			if key := [2]string{rule, reason}; !seen[key] {
				seen[key] = true
				failed = append(failed, Failed{Rule: rule, Reason: reason, Why: said(r.err)})
			}
		}
	}
	slices.SortStableFunc(failed, func(a, b Failed) int {
		if c := strings.Compare(a.Reason, b.Reason); c != 0 {
			return c
		}
		return strings.Compare(a.Rule, b.Rule)
	})
	it := Item{ID: g.id, State: OK, What: fmt.Sprintf("%s asked of %s, each once, as an instant query at now: all answered", plural(asked, "expression"), g.what)}
	switch {
	case len(failed) > 0:
		it.State = Problem
		it.What = fmt.Sprintf("%s of %s asked of %s failed", fmt.Sprintf("%d rule queries", len(failed)), plural(asked, "expression"), g.what)
		it.Failed = failed
		it.Fix = "a run asks each of these over its range and fails the rule the same way: change the expression, or what limits it (--max-samples, the server's own timeout and limits)"
	case unasked > 0:
		it.State = Unknown
	}
	if unasked > 0 {
		it.What += fmt.Sprintf("; %d more not asked, the time ran out (--preflight-timeout)", unasked)
		if it.State == OK {
			it.State = Unknown
		}
	}
	p.add(it)
}

// asker is how an expression of a group is asked, as the replay asks it.
func (p *run) asker(ctx context.Context, g *queryGroup) (func(context.Context, *queryAsk, time.Time) ([]prom.Series, error), error) {
	s := p.c.Serve
	switch g.id {
	case "queries.history":
		h, err := p.historyClient(ctx)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, a *queryAsk, t time.Time) ([]prom.Series, error) {
			return h.QueryRange(ctx, a.expr, t, t, s.Step)
		}, nil
	case "queries.loki":
		h := p.c.Loki.History(p.agent())
		return func(ctx context.Context, a *queryAsk, t time.Time) ([]prom.Series, error) {
			return h.QueryRange(ctx, a.expr, t, t, s.Step)
		}, nil
	}
	// One Limiter for each data source, within the Grafana's pace and cap,
	// as the replay asks them (cmd/lapilli verdictFlags.logs).
	pace, most := prom.NewPace(p.c.MaxRate), prom.NewCap(s.Parallel)
	var mu sync.Mutex
	hs := map[string]replay.History{}
	return func(ctx context.Context, a *queryAsk, t time.Time) ([]prom.Series, error) {
		mu.Lock()
		h := hs[a.uid]
		if h == nil {
			h = source.GrafanaLoki(s.Grafana, s.GrafanaToken, p.agent(), a.uid, prom.NewLimiterPaced(pace).Within(most), prom.WithMaxSamples(p.c.MaxSamples))
			hs[a.uid] = h
		}
		mu.Unlock()
		return h.QueryRange(ctx, a.expr, t, t, s.Step)
	}, nil
}

// allowQuery lets the one request that asks a, of g's place, go.
func (p *run) allowQuery(g *queryGroup, a *queryAsk) {
	s := p.c.Serve
	switch g.id {
	case "queries.history":
		if host, path, ok := at(p.historyBase()); ok {
			p.al.add(allow{api: "prometheus", method: http.MethodPost, host: host, path: path + "/api/v1/query_range", form: p.al.promQuery(a.expr)})
		}
	case "queries.loki":
		if host, path, ok := at(s.Loki); ok {
			p.al.add(allow{api: "loki", method: http.MethodGet, host: host, path: path + "/loki/api/v1/query_range", query: p.al.lokiInstant(a.expr)})
		}
	default:
		if host, path, ok := at(s.Grafana); ok {
			p.al.add(allow{api: "grafana", method: http.MethodGet, host: host, path: path + "/api/datasources/proxy/uid/" + url.PathEscape(a.uid) + "/loki/api/v1/query_range",
				query: p.al.lokiInstant(a.expr)})
		}
	}
}

// queryReason says why a query failed, as a kind, by the error's type first:
// a time that ran out, the client's own bound, a status that says not let in
// or timed out; then, of what the server said of the expression, its words
// (Prometheus's, Mimir's and Loki's). A transport's error is none of those.
func queryReason(err error) string {
	var ae *prom.APIError
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return reasonTimeout
	case prom.OverBound(err):
		return reasonSamples
	case !errors.As(err, &ae):
		return reasonOther
	}
	m := strings.ToLower(ae.Msg)
	switch {
	case ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden:
		return reasonLetIn
	case ae.Status == http.StatusRequestTimeout || ae.Status == http.StatusGatewayTimeout:
		return reasonTimeout
	case strings.Contains(m, "unknown function"), strings.Contains(m, "unknown fn"):
		return reasonFunction
	case strings.Contains(m, "too many samples"), strings.Contains(m, "exceeds maximum allowed size"), strings.Contains(m, "maximum number of series"),
		strings.Contains(m, "response too large"), strings.Contains(m, "exceeded maximum resolution"):
		return reasonSamples
	case strings.Contains(m, "parse error"), strings.Contains(m, "syntax error"), strings.Contains(m, "bad_data"):
		return reasonParse
	case strings.Contains(m, "timed out"), strings.Contains(m, "timeout"), strings.Contains(m, "deadline exceeded"):
		return reasonTimeout
	}
	return reasonOther
}
