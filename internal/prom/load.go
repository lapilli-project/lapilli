package prom

import (
	"context"
	"net/http"
	"slices"
	"sync/atomic"
	"time"
)

// APIs are the kinds of history API a request is counted under (Counts.Of):
// a Prometheus-compatible API, a Loki, and a Grafana's data source proxy,
// which Grafana's rules on Loki are asked through. They are fixed, so a
// label of them is never a URL, a host, a tenant or a data source's name.
var APIs = []string{"prometheus", "loki", "grafana"}

// Answers are the classes of answer a request is counted under: the
// hundreds of its HTTP status, or none, for a request that had no answer
// (not reached, a connection refused or dropped, its time up).
var Answers = []string{"2xx", "3xx", "4xx", "5xx", "none"}

const nAPIs, nAnswers = 3, 5

// apiCounts are what one kind of API was asked, kept in Counts.
type apiCounts struct {
	answers                  [nAnswers]atomic.Int64
	tooMany, retries, yields atomic.Int64
	// waited and paced are nanoseconds (Load).
	waited, paced atomic.Int64
}

// Load is what one kind of API was asked, as Counts.Of gives it.
type Load struct {
	// Answers are the requests sent, by the class of their answer
	// (Answers).
	Answers map[string]int64
	// TooMany are the answers of HTTP 429, heeded or not; Retries the
	// requests asked again after an answer that said to (429, 5xx) or a
	// connection dropped; Yields how often a run yielded to it (Yield).
	TooMany, Retries, Yields int64
	// Waited is how long its requests waited for the API, summed over
	// them: out a wait it asked for (Retry-After), and before being asked
	// again. Paced is how long they waited for their turn by a rate
	// (--max-rate, a catch-up's).
	Waited, Paced time.Duration
}

// Of is what the kind of API api, one of APIs, was asked; nothing for
// another.
func (c *Counts) Of(api string) Load {
	l := Load{Answers: map[string]int64{}}
	i := slices.Index(APIs, api)
	if c == nil || i < 0 {
		return l
	}
	a := &c.apis[i]
	for k, name := range Answers {
		l.Answers[name] = a.answers[k].Load()
	}
	l.TooMany, l.Retries, l.Yields = a.tooMany.Load(), a.retries.Load(), a.yields.Load()
	l.Waited, l.Paced = time.Duration(a.waited.Load()), time.Duration(a.paced.Load())
	return l
}

// CountsOf is the Counts ctx counts into (Counting), nil for none.
func CountsOf(ctx context.Context) *Counts { return countsOf(ctx) }

// CountedAs says which of APIs a client's requests are counted under:
// without it, loki for a Loki (Loki), prometheus otherwise.
func CountedAs(api string) Option {
	return func(c *Client) { c.api = api }
}

// kind is which of APIs the client's requests are counted under.
func (c *Client) kind() string {
	switch {
	case c.api != "":
		return c.api
	case c.loki:
		return "loki"
	}
	return "prometheus"
}

type apiKey struct{}

// asking is ctx of a request to the client's kind of API: what it waits
// for, and how often it is asked again, are counted under it.
func (c *Client) asking(ctx context.Context) context.Context {
	return context.WithValue(ctx, apiKey{}, c.kind())
}

// apiCountsOf are the counts, in ctx's Counts, of the kind of API ctx asks
// (asking); nil for none.
func apiCountsOf(ctx context.Context) *apiCounts {
	n := countsOf(ctx)
	api, _ := ctx.Value(apiKey{}).(string)
	if n == nil {
		return nil
	}
	return n.of(api)
}

// of is c's counts of api, nil for none of APIs.
func (c *Counts) of(api string) *apiCounts {
	i := slices.Index(APIs, api)
	if i < 0 {
		return nil
	}
	return &c.apis[i]
}

// countAnswer counts a request sent to api: its answer, resp, or none.
func countAnswer(ctx context.Context, api string, resp *http.Response) {
	n := countsOf(ctx)
	if n == nil {
		return
	}
	a := n.of(api)
	if a == nil {
		return
	}
	class := nAnswers - 1 // none, as for a status of no class
	if resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 600 {
		class = resp.StatusCode/100 - 2
	}
	a.answers[class].Add(1)
	if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
		a.tooMany.Add(1)
	}
}

// countRetry counts a request of ctx's API asked again.
func countRetry(ctx context.Context) {
	if a := apiCountsOf(ctx); a != nil {
		a.retries.Add(1)
	}
}

// countWait counts d waited by a request of ctx's API: for its turn by a
// rate, when paced, or else for the API.
func countWait(ctx context.Context, d time.Duration, paced bool) {
	a := apiCountsOf(ctx)
	if a == nil || d <= 0 {
		return
	}
	if paced {
		a.paced.Add(int64(d))
	} else {
		a.waited.Add(int64(d))
	}
}
