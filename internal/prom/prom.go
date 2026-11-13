// Package prom asks a Prometheus-compatible HTTP API for the history of an
// expression: Prometheus, Thanos, Mimir, VictoriaMetrics, and the managed
// Prometheus of AWS, Azure and Google Cloud; and Loki, for LogQL, whose
// query API answers the same way.
package prom

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"math/rand/v2"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

// Client queries one API.
type Client struct {
	base  string
	http  *http.Client
	token func(context.Context) (string, error)
	sign  func(*http.Request) error
	agent string
	// headers are sent with every request.
	headers [][2]string
	// loki says the API is Loki's, which takes LogQL (Loki).
	loki bool
	// limit shares the API out among the requests of a run.
	limit *Limiter
	// maxSamples is the most samples the answers to one range may hold in
	// all (WithMaxSamples, --max-samples).
	maxSamples int
}

// Option configures a Client.
type Option func(*Client)

// WithToken sends a bearer token, asked for on every request so it can be
// refreshed.
func WithToken(f func(context.Context) (string, error)) Option {
	return func(c *Client) { c.token = f }
}

// WithSigner signs every request just before it is sent, as AWS SigV4 does.
func WithSigner(f func(*http.Request) error) Option {
	return func(c *Client) { c.sign = f }
}

// WithHeader sends a header with every request: X-Scope-OrgID, a Mimir's
// or a Cortex's tenant, for one with tenants and no gateway to set it.
func WithHeader(name, value string) Option {
	return func(c *Client) { c.headers = append(c.headers, [2]string{name, value}) }
}

// WithHTTPClient replaces the HTTP client.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// WithLimiter shares l among this client's requests and those of every
// other client given it: clients of one API, asked with credentials of
// one, push back together. Without it, a client has a Limiter of its own,
// of no rate.
func WithLimiter(l *Limiter) Option {
	return func(c *Client) { c.limit = l }
}

// DefaultMaxSamples is how many samples the answers to one range (a
// rule's query over the period) may hold in all, unless WithMaxSamples
// says otherwise: a week at a minute of 1,000 series firing all through
// it, 28 days of 250. Replaced in tests.
var DefaultMaxSamples = 10000000

// SampleBytes is what a sample read takes at the peak, near enough, as
// measured over 28 days and 7 at a minute: 62 bytes, 37 as kept after
// (32 of the sample, the rest its series' list grown to hold it), the
// collector's leftovers besides. DefaultMaxSamples is some 600 MB so.
const SampleBytes = 64

// WithMaxSamples holds the answers to one range to n samples in all, past
// which it isn't read (--max-samples); n of 0 or less leaves
// DefaultMaxSamples.
func WithMaxSamples(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.maxSamples = n
		}
	}
}

// Loki says the API is Loki's: a range query is asked of
// /loki/api/v1/query_range, by GET, as Grafana's data source proxy allows
// for Loki; its errors are plain text; and a log query, which answers log
// lines, is refused.
func Loki() Option {
	return func(c *Client) { c.loki = true }
}

// IsLoki reports whether the client asks a Loki.
func (c *Client) IsLoki() bool { return c.loki }

// client is what a Client asks with, unless WithHTTPClient replaces it. It
// follows no redirect: a query, a token or a signature goes only to the API
// it was given. Go would send a bearer token on to the same host's other
// ports and schemes, AWS's session token and a 307's body to any host. It
// gives up on a request after five minutes, past the two Prometheus gives a
// query.
var client = &http.Client{
	Timeout:       5 * time.Minute,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// New returns a client for the API under base, such as
// http://prometheus:9090.
func New(base, userAgent string, opts ...Option) *Client {
	c := &Client{base: strings.TrimRight(base, "/"), http: client, agent: userAgent, maxSamples: DefaultMaxSamples}
	for _, o := range opts {
		o(c)
	}
	if c.limit == nil {
		c.limit = NewLimiter(0)
	}
	return c
}

// Sample is one value of a series.
type Sample struct {
	T time.Time
	V float64
}

// Series is one label set and its samples, oldest first.
type Series struct {
	Labels  map[string]string
	Samples []Sample
}

// maxPoints is how many steps one request covers: a day at one minute,
// well under the 11,000 points per series Prometheus allows, and small
// enough that a query over many series stays under answer size limits.
const maxPoints = 1440

// QueryRange evaluates expr at every step from start to end. Long ranges are
// split into several requests and the series joined again.
func (c *Client) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]Series, error) {
	if step <= 0 {
		return nil, errors.New("step must be positive")
	}
	byKey := map[string]*Series{}
	t := &tally{keys: map[string]bool{}, maxSamples: c.maxSamples}
	chunk := step * maxPoints
	if c.loki {
		// Asked at a finer step (lokiStep), a request stays within Loki's
		// 11,000 points.
		if n := int64(lokiPoints * lokiStep(start, step) / step); n >= 1 && n < maxPoints {
			chunk = step * time.Duration(n)
		}
	}
	for from := start; !from.After(end); from = from.Add(chunk + step) {
		to := from.Add(chunk)
		if to.After(end) {
			to = end
		}
		part, err := c.fetch(ctx, expr, from, to, step, t)
		if err != nil {
			return nil, err
		}
		for _, s := range part {
			k := key(s.Labels)
			if have, ok := byKey[k]; ok {
				have.Samples = append(have.Samples, s.Samples...)
			} else {
				byKey[k] = &s
			}
		}
	}
	out := make([]Series, 0, len(byKey))
	for _, k := range slices.Sorted(maps.Keys(byKey)) {
		out = append(out, *byKey[k])
	}
	return out, nil
}

// lokiText reports whether an answer is Loki's own plain text, as it says
// what went wrong: text/plain, with no markup, which a proxy's page has.
func lokiText(resp *http.Response, msg string) bool {
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return mt == "text/plain" && !strings.Contains(msg, "<")
}

// APIError is an error answer from the API.
type APIError struct {
	Status int
	// Msg is the API's text, maxMsg bytes at most, with what the request
	// carried to sign in taken out, as a server may echo it back.
	Msg string
	// retryAfter is how long an answer of 429 or 503 said to wait before
	// asking again, when said is true (Retry-After).
	retryAfter time.Duration
	said       bool
	// big says the text, before it was cut, said the answer was too large
	// to send (tooBig).
	big bool
	// page says the text isn't the API's own words: a page, or JSON of
	// another shape, as a proxy, a gateway or a sign-in before it writes.
	page bool
}

// maxMsg is how much of the API's text an APIError keeps: a few lines, so
// that a long one doesn't push the rest of a report out of a message.
const maxMsg = 300

// apiError is the API's answer resp to req, as an APIError whose text is
// msg as serverText gives it, without the token, the signature or the
// password req carried, cut to maxMsg.
func apiError(req *http.Request, resp *http.Response, msg string) *APIError {
	ra, said := retryAfter(resp, clock())
	return &APIError{Status: resp.StatusCode, Msg: clip(serverText(msg, req), maxMsg), retryAfter: ra, said: said, big: tooBigText(msg)}
}

// pageError is resp, to req, as an APIError whose text, msg, isn't the
// API's own (APIError.page).
func pageError(req *http.Request, resp *http.Response, msg string) *APIError {
	e := apiError(req, resp, msg)
	e.page = true
	return e
}

func (e *APIError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, clip(serverText(e.Msg, nil), maxMsg))
}

// serverText is a server's text as an error says it: without what doesn't
// show or acts on a terminal (control and format characters), which could
// hide a secret in two, and then without what req carried to sign in
// (sent), or any bearer token.
func serverText(msg string, req *http.Request) string {
	msg = strings.Map(func(c rune) rune {
		if unicode.IsControl(c) || unicode.Is(unicode.Cf, c) {
			return -1
		}
		return c
	}, msg)
	return redact(msg, sent(req))
}

// ServerText is a server's text as an error says it, for the errors of
// what isn't a Prometheus-compatible API (a ruler, a Grafana, a cloud):
// without control and format characters, what req carried to sign in, or
// any bearer token. req may be nil.
func ServerText(msg string, req *http.Request) string { return serverText(msg, req) }

// Redact is a server's text without what req carried to sign in, or any
// bearer token, and otherwise as it was: for an answer whose text is shown
// when it is an error, and may still be read.
func Redact(msg string, req *http.Request) string { return redact(msg, sent(req)) }

// quotedText is a server's text as an error quotes it (%q, which shows
// what doesn't show): without what req carried to sign in, or any bearer
// token; and, when one of those is hidden in two by a character that
// doesn't show, as serverText gives it, without those characters.
func quotedText(raw string, req *http.Request) string {
	kept, cleaned := redact(raw, sent(req)), serverText(raw, req)
	if strings.Count(cleaned, "[redacted]") > strings.Count(kept, "[redacted]") {
		return cleaned
	}
	return kept
}

// cutEnd is s, cut short of what followed it, without the start of one of
// secrets at its end: cut, a secret longer than what is kept is whole
// nowhere in it, and redact wouldn't find it; taken out, as it would be.
func cutEnd(s string, secrets []string) string {
	for _, x := range secrets {
		for k := min(len(x)-1, len(s)); k >= 4; k-- {
			if strings.HasSuffix(s, x[:k]) {
				s = s[:len(s)-k] + "[redacted]"
				break
			}
		}
	}
	return s
}

// clip is s cut to n bytes at most, "…" included, at the start of a rune,
// and said to be cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	n = max(n-len("…"), 0)
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// bearer is a bearer token, however it got into a text: what follows
// "bearer", unless it is a word, letters alone and fewer than twenty, so
// that "missing bearer token." is left as it is.
var bearer = regexp.MustCompile(`(?i)\bbearer\s+([^\s"',;]+)`)

// redact is msg without secrets, and without any bearer token.
func redact(msg string, secrets []string) string {
	for _, s := range secrets {
		msg = strings.ReplaceAll(msg, s, "[redacted]")
	}
	return bearer.ReplaceAllStringFunc(msg, func(m string) string {
		word := strings.TrimRight(bearer.FindStringSubmatch(m)[1], ".:!?)")
		if len(word) < 20 && word != "" && strings.Trim(word, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz") == "" {
			return m
		}
		return "Bearer [redacted]"
	})
}

// sent is what req carried to sign in: its Authorization header, and each
// part of it (a token, a signature, a credential), an AWS session token,
// and a password in its URL, which net/http sends as Basic; the longest
// first, so that none is left in part.
func sent(req *http.Request) []string {
	if req == nil {
		return nil
	}
	var out []string
	add := func(s string) {
		if s = strings.Trim(strings.TrimSpace(s), `"`); len(s) >= 8 {
			out = append(out, s)
		}
	}
	if u := req.URL.User; u != nil {
		if pw, ok := u.Password(); ok {
			add(pw)
			add(base64.StdEncoding.EncodeToString([]byte(u.Username() + ":" + pw)))
		}
	}
	for _, h := range []string{"Authorization", "X-Amz-Security-Token"} {
		v := req.Header.Get(h)
		if v == "" {
			continue
		}
		add(v)
		_, rest, _ := strings.Cut(v, " ")
		add(rest)
		for _, f := range strings.FieldsFunc(rest, func(r rune) bool { return r == ',' || r == ' ' }) {
			if _, val, ok := strings.Cut(f, "="); ok {
				add(val)
			}
		}
	}
	slices.SortFunc(out, func(a, b string) int { return len(b) - len(a) })
	return out
}

// tooBig reports an answer too large for the API to send, which a shorter
// range fixes.
func (e *APIError) tooBig() bool { return e.big || tooBigText(e.Msg) }

func tooBigText(msg string) bool {
	m := strings.ToLower(msg)
	// Loki counts a range's series over the whole range, and says to
	// shorten it: "maximum number of series (500) reached for a single
	// query; consider ... reducing the time range" (3.7.9).
	return strings.Contains(m, "exceeds maximum allowed size") || strings.Contains(m, "too many samples") ||
		strings.Contains(m, "exceeded maximum resolution") || strings.Contains(m, "response too large") ||
		strings.Contains(m, "maximum number of series")
}

// transient reports an overloaded or briefly unavailable API, which is
// asked again: not 501, which says it doesn't do what was asked, nor 505.
func (e *APIError) transient() bool {
	return !e.tooBig() && (e.Status == http.StatusTooManyRequests ||
		e.Status >= 500 && e.Status != http.StatusNotImplemented && e.Status != http.StatusHTTPVersionNotSupported)
}

// refused reports an answer of the API's to the query as written: see
// Refused.
func (e *APIError) refused() bool {
	switch {
	case e.page:
		return false
	case e.tooBig():
		return true
	}
	switch e.Status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusNotImplemented:
		return true
	}
	return e.Status/100 == 2
}

// credsError is credentials that couldn't be had for a request: a token
// file gone, a token endpoint that failed.
type credsError struct{ err error }

func (e *credsError) Error() string { return "credentials: " + e.err.Error() }
func (e *credsError) Unwrap() error { return e.err }

// unreadable is an answer of the API's, to a query, that isn't one lapilli
// can read: not a matrix, or a sample not a time and a number.
type unreadable struct {
	msg string
	// sample is a sample that isn't a time and a number, as the answer
	// wrote it, when that is what can't be read.
	sample [2]string
}

func (e *unreadable) Error() string { return e.msg }

// Refused reports whether err is the API's answer to the query as written,
// which another run is given too, whatever its access: the query refused,
// HTTP 400 or 422 (bad_data, an execution error), in the API's own words,
// not a page a proxy wrote; an answer too large however short the range
// asked; HTTP 501, a query the API doesn't do; or an answer that can't be
// read. Every other failure is of the run, the history not read: not
// reached (a connection, a name, TLS), not let in (401, 403, 407, the
// credentials), not found (404), sent elsewhere (3xx), timed out (408),
// turned away (429, 5xx), or answered with a page that isn't the API's.
// Another run, or this one with its access mended, may be answered.
func Refused(err error) bool {
	var ae *APIError
	var ua *unreadable
	var ce *credsError
	switch {
	case errors.As(err, &ce):
		// Whatever the token's source answered.
		return false
	case errors.As(err, &ae):
		return ae.refused()
	case errors.As(err, &ua):
		return true
	}
	return false
}

// dropped reports a connection the API closed before it answered whole,
// as an API overloaded does: asked again, as after a 5xx.
func dropped(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET)
}

// The waits before asking again after an answer that says to: HTTP 429
// (too many requests) and 5xx. Each wait respects the context, which
// --timeout ends.
var (
	// retryWait is the first wait after a 5xx, or a connection dropped: 2,
	// 4, 8 and 16 s, each less up to half, so that requests turned away
	// together don't come back together.
	retryWait = 2 * time.Second
	// limitWait is the first wait after a 429 that doesn't say how long:
	// 4, 8, 16, 32, 60 and 60 s, each less up to half, 90 s to 3 minutes
	// in all, past the window of a quota by the minute.
	limitWait = 4 * time.Second
	// jitter is a number from 0 up to 1; replaced in tests.
	jitter = rand.Float64
)

const (
	// errorRetries is how many times a request is asked again after 5xx
	// (or a connection dropped), limitRetries after 429, each counted
	// apart, of the answers that don't say how long to wait. One that
	// does, a 429 or a 503, is asked again as often as it says, within
	// maxWaited.
	errorRetries = 4
	limitRetries = 6
	// maxBackoff is the longest wait lapilli picks itself.
	maxBackoff = time.Minute
	// maxRetryAfter is the longest one wait is, whatever Retry-After says,
	// before a quarter more at most: longer than any quota by the minute
	// needs, and short enough that a server saying to wait an hour doesn't
	// hold a run.
	maxRetryAfter = 2 * time.Minute
)

// maxWaited is how long, from when a request is first pushed back (by an
// answer that says to ask again, or by a wait for its turn the API asked
// for), it is asked again: its waits, those for its turn (Limiter) and the
// API's answers all count. A request still waiting for its turn then is
// given up; one asked is waited for. Replaced in tests.
var maxWaited = 5 * time.Minute

// backoff is how long to wait before asking again after e, the nth answer
// of its kind (from 0: 429s, and 5xx, each counted apart); again is false
// when it isn't asked again. An answer that says how long (Retry-After, on
// 429 and 503) is taken at its word, a second at least and maxRetryAfter at
// most, plus up to a quarter more, so that requests told the same wait
// don't all come back at once; while the API says when, and holds every
// other request of the client till then (Limiter), it is asked again,
// within maxWaited.
func (e *APIError) backoff(n int) (wait time.Duration, again bool) {
	switch {
	case e.said:
		d := min(max(e.retryAfter, time.Second), maxRetryAfter)
		return d + time.Duration(jitter()*float64(d/4)), true
	case e.Status == http.StatusTooManyRequests:
		if n >= limitRetries {
			return 0, false
		}
		return spread(min(limitWait<<n, maxBackoff)), true
	}
	return errorBackoff(n)
}

// errorBackoff is the wait after the nth 5xx, or connection dropped.
func errorBackoff(n int) (time.Duration, bool) {
	if n >= errorRetries {
		return 0, false
	}
	return spread(retryWait << n), true
}

// spread is d less up to half.
func spread(d time.Duration) time.Duration { return d/2 + time.Duration(jitter()*float64(d/2)) }

// retried asks with ask, and asks again after an answer that says to (HTTP
// 429, 5xx) or a connection dropped, as backoff says, for maxWaited after
// it was first pushed back (budget); each wait ends with ctx, and one that
// would end past ctx's deadline, before the budget's end, isn't waited:
// the request fails then, for ErrPastDeadline, with the answer. An error
// after more than one ask says how many there were, and over how long.
func retried[T any](ctx context.Context, ask func(context.Context) (T, error)) (T, error) {
	start := clock()
	b := &budget{}
	ctx = context.WithValue(ctx, budgetKey{}, b)
	var asked, tooMany, failed int
	var last error
	// told is err, saying how often the request was asked, n, and over how
	// long, when it was asked again.
	told := func(err error, n int) error {
		if n < 2 {
			return err
		}
		return fmt.Errorf("%w (asked %d times over %s)", err, n, clock().Sub(start).Round(time.Second))
	}
	for {
		got, err := ask(ctx)
		asked++
		var pd *pastDeadline
		switch {
		case last == nil:
		case errors.As(err, &pd):
			// The run would end before its turn came: after the last
			// answer, which it is said with.
			return got, &pastDeadline{why: pd.why, last: told(last, asked-1)}
		case errors.Is(err, errNoTurn):
			// Its time ran out waiting for a turn: the last answer is why
			// it failed.
			return got, told(last, asked-1)
		}
		var ae *APIError
		var wait time.Duration
		var again bool
		switch {
		case err == nil || ctx.Err() != nil:
			return got, err
		case errors.As(err, &ae) && ae.transient():
			n := &failed
			if ae.Status == http.StatusTooManyRequests {
				n = &tooMany
			}
			wait, again = ae.backoff(*n)
			if !ae.said {
				*n++
			}
		case !errors.As(err, &ae) && dropped(err):
			wait, again = errorBackoff(failed)
			failed++
		default:
			return got, err
		}
		now := clock()
		if b.from.IsZero() {
			b.from = now
		}
		by, cut := b.by(), false
		if dl, ok := ctx.Deadline(); ok && dl.Before(by) {
			by, cut = dl, true
		}
		switch {
		case !again || now.Add(wait).After(by) && !cut:
			return got, told(err, asked)
		case now.Add(wait).After(by):
			// The run would end before the wait did.
			why := againPast
			if ae != nil && ae.said {
				why = waitPast
			}
			return got, &pastDeadline{why: why, last: told(err, asked)}
		}
		last = err
		if n := countsOf(ctx); n != nil {
			n.Retries.Add(1)
		}
		if sleep(ctx, wait) != nil {
			return got, err
		}
	}
}

// fetch asks for one range, asking again when the API is briefly
// unavailable or asked too much (retried), and halving the range when the
// answer is too large; each answer counted in t, with those to the rest of
// the range.
func (c *Client) fetch(ctx context.Context, expr string, start, end time.Time, step time.Duration, t *tally) ([]Series, error) {
	got, err := retried(ctx, func(ctx context.Context) ([]Series, error) { return c.queryRange(ctx, expr, start, end, step) })
	var ae *APIError
	if errors.As(err, &ae) && ae.tooBig() && end.Sub(start) >= step {
		// Halved on the range's own grid: a range placed off the step's, as
		// a group placed within its interval is, stays on it.
		mid := start.Add(end.Sub(start) / 2 / step * step)
		a, err := c.fetch(ctx, expr, start, mid, step, t)
		if err != nil {
			return nil, err
		}
		b, err := c.fetch(ctx, expr, mid.Add(step), end, step, t)
		if err != nil {
			return nil, err
		}
		return join(a, b), nil
	}
	if err == nil {
		err = t.add(got)
	}
	if err != nil {
		return nil, err
	}
	return got, nil
}

// tally is what the answers to one range have held so far: its series,
// told apart by their labels, their samples and what their labels take;
// maxSamples the most samples they may hold (the Client's).
type tally struct {
	keys                    map[string]bool
	series, samples, labels int
	maxSamples              int
}

// add counts an answer's series with the rest, and says when they come to
// more than one range is read with (maxRange*, maxSamples): not an answer
// too large, as each is within its bounds, but too much for a shorter one
// to mend; said with the bound, and how to read the range all the same.
func (t *tally) add(part []Series) error {
	for _, s := range part {
		if k := key(s.Labels); !t.keys[k] {
			t.keys[k] = true
			t.series++
			t.labels += labelCost(s.Labels)
		}
		t.samples += len(s.Samples)
	}
	var msg string
	switch {
	case t.series > maxRangeSeries:
		msg = fmt.Sprintf("the answers to the range hold more than %d series in all, the most a range is read with: read it over a shorter range (--since)", maxRangeSeries)
	case t.samples > t.maxSamples:
		msg = fmt.Sprintf("the answers to the range hold more than %d samples in all (--max-samples): read it at a longer --step, over a shorter range (--since), or with a larger --max-samples, each 1000000 more taking some %d MB more at the peak",
			t.maxSamples, SampleBytes)
	case t.labels > maxRangeLabels:
		msg = fmt.Sprintf("the answers to the range hold more than %d MiB of labels in all, the most a range is read with: read it over a shorter range (--since)", maxRangeLabels>>20)
	default:
		return nil
	}
	return &unreadable{msg: msg}
}

// asked is what an error says, at its end, of how often its request was
// asked, and over how long: that differs from one run to the next.
var asked = regexp.MustCompile(` \(asked \d+ times over [0-9hmsµn.]+\)$`)

// Steady is a reason a rule couldn't be replayed without what differs from
// one run to the next, how often its request was asked and over how long,
// which retried writes at its end: as a ledger keeps it. One a server
// wrote elsewhere in it is left, as what it is taken out of could be a
// secret cut in two; any bearer token is taken out again.
func Steady(reason string) string { return redact(asked.ReplaceAllString(reason, ""), nil) }

// retryAfter is how long an answer of 429 or 503 says to wait before
// asking again: Retry-After, in seconds or as an HTTP date; said is false
// when it doesn't say, or says it unreadably. A date gone by is no wait.
func retryAfter(resp *http.Response, now time.Time) (wait time.Duration, said bool) {
	if resp == nil || !pushedBack(resp.StatusCode) {
		return 0, false
	}
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	switch {
	case v == "":
		return 0, false
	case strings.Trim(v, "0123456789") == "":
		if len(v) > 7 { // more than a hundred days: as long as the cap
			return maxRetryAfter, true
		}
		n, _ := strconv.Atoi(v)
		return time.Duration(n) * time.Second, true
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return 0, false
	}
	return max(t.Sub(now), 0), true
}

// join puts two consecutive ranges of the same query back together.
func join(a, b []Series) []Series {
	byKey := map[string]int{}
	for i, s := range a {
		byKey[key(s.Labels)] = i
	}
	for _, s := range b {
		if i, ok := byKey[key(s.Labels)]; ok {
			a[i].Samples = append(a[i].Samples, s.Samples...)
		} else {
			a = append(a, s)
		}
	}
	return a
}

// lokiPoints is how many points a request to Loki asks for at most, under
// the 11,000 a series Loki allows.
const lokiPoints = 10000

// lokiStep is the step Loki is asked at for a range from start every step.
// Loki puts a range's points on whole multiples of its step since the Unix
// epoch, whatever the range's start (measured on Loki 3.7.9: from 471 every
// 5s it answers 470, 475, …); Prometheus puts them at the start and every
// step after. A range that starts off the step's grid, as a group placed
// within its interval does, is asked at the greatest step dividing both
// the step and how far off the grid it starts, whose points include the
// range's, and the rest are left out (lokiOnly).
func lokiStep(start time.Time, step time.Duration) time.Duration {
	ms := step.Milliseconds()
	if ms <= 0 {
		return step
	}
	off := ((start.UnixMilli() % ms) + ms) % ms
	g := ms
	for off != 0 {
		g, off = off, g%off
	}
	return time.Duration(g) * time.Millisecond
}

// lokiOnly keeps of Loki's answer the points at start and every step after
// it, up to end, as Prometheus answers.
func lokiOnly(series []Series, start, end time.Time, step time.Duration) []Series {
	out := series[:0]
	for _, s := range series {
		kept := s.Samples[:0]
		for _, x := range s.Samples {
			if !x.T.Before(start) && !x.T.After(end) && x.T.Sub(start)%step == 0 {
				kept = append(kept, x)
			}
		}
		if len(kept) > 0 {
			s.Samples = kept
			out = append(out, s)
		}
	}
	return out
}

func (c *Client) queryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]Series, error) {
	ask := step
	if c.loki {
		ask = lokiStep(start, step)
	}
	q := url.Values{
		"query": {expr},
		"start": {strconv.FormatInt(start.Unix(), 10)},
		"end":   {strconv.FormatInt(end.Unix(), 10)},
		"step":  {strconv.FormatFloat(ask.Seconds(), 'f', -1, 64)},
	}
	var req *http.Request
	var err error
	if c.loki {
		// A range off whole seconds is asked in milliseconds: Loki reads a
		// time with a point in it as seconds.
		if start.UnixMilli()%1000 != 0 || end.UnixMilli()%1000 != 0 || ask%time.Second != 0 {
			q.Set("start", strconv.FormatFloat(float64(start.UnixMilli())/1e3, 'f', 3, 64))
			q.Set("end", strconv.FormatFloat(float64(end.UnixMilli())/1e3, 'f', 3, 64))
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/loki/api/v1/query_range?"+q.Encode(), nil)
	} else {
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v1/query_range", strings.NewReader(q.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	}
	if err != nil {
		return nil, err
	}
	// Read as it comes, keeping only what was asked: a sample out of the
	// range, or more than its steps, a series past maxAnswerSeries.
	var a *answer
	var bad error
	h := &head{}
	resp, err := c.send(ctx, req, true, func(body io.Reader) error {
		h.r = io.LimitReader(body, maxBody+1)
		a, bad = readAnswer(h, start, end, ask)
		if bad != nil && !errors.Is(bad, errNotJSON) {
			return bad // the body's own: a connection dropped
		}
		// What is left of a whole answer, for the connection to be kept;
		// one passed over in part is closed.
		io.CopyN(io.Discard, h, 64<<10)
		return nil
	})
	if err != nil {
		return nil, err
	}
	failed := func(msg string) error { return apiError(req, resp, msg) }
	what := "a Prometheus"
	if c.loki {
		what = "a Loki"
	}
	said := func() string {
		text := h.buf.String()
		if h.n > int64(len(text)) {
			text = cutEnd(text, sent(req))
		}
		return clip(serverText(strings.TrimSpace(text), req), 200)
	}
	notAPI := func() error { return pageError(req, resp, "not "+what+" API response: "+said()) }
	if h.n > maxBody {
		return nil, &APIError{Status: resp.StatusCode, Msg: fmt.Sprintf("the answer is over %d MiB", maxBody>>20), big: true}
	}
	if bad != nil {
		if msg := said(); c.loki && resp.StatusCode/100 != 2 && lokiText(resp, msg) {
			// Loki says what went wrong in plain text; a page, from a
			// proxy before it, isn't Loki's.
			return nil, failed(msg)
		}
		return nil, notAPI()
	}
	switch a.status {
	case "success":
	case "error":
		return nil, failed(a.errMsg)
	default:
		// JSON, but not the API's: a gateway's, or a sign-in's.
		return nil, notAPI()
	}
	// An API that caps the series in an answer may say so only in a warning
	// (CloudWatch: "result truncated to the maximum of 500 series"). A replay
	// of what is left would be wrong without saying so.
	for _, w := range a.warnings {
		if strings.Contains(strings.ToLower(w), "truncated") {
			return nil, failed("the answer left series out: " + w)
		}
	}
	if c.loki && a.resultType == "streams" {
		return nil, &APIError{Status: http.StatusBadRequest, Msg: "a log query, which returns log lines, not numbers: only a metric query, such as count_over_time, is replayed"}
	}
	if a.resultType != "matrix" {
		// The server's words, as every other error has them: without the
		// token, which a server may echo, in what is quoted.
		return nil, &unreadable{msg: fmt.Sprintf("want a matrix, got %q", clip(serverText(a.resultType, req), 64))}
	}
	var u *unreadable
	if errors.As(a.bad, &u) {
		if u.sample != [2]string{} {
			// The server's words, what the request carried taken out of
			// each before it is cut: cut first, a long token's start
			// would stay.
			return nil, &unreadable{msg: fmt.Sprintf("unreadable sample [%q, %q]", clip(quotedText(u.sample[0], req), 64), clip(quotedText(u.sample[1], req), 64))}
		}
		return nil, &unreadable{msg: clip(serverText(u.msg, req), 200)}
	}
	if a.big != "" {
		// As an API that says so: a shorter range is asked.
		return nil, &APIError{Status: resp.StatusCode, Msg: a.big, big: true}
	}
	if c.loki {
		return lokiOnly(a.series, start, end, step), nil
	}
	return a.series, nil
}

// ParseTime reads a time as the APIs write one, seconds since the epoch
// with a fraction (1794424474.31), exactly: read as a float64 and
// multiplied, 1794424474.31 is 128ns off its millisecond, and a point
// compared with the step it is on is lost. A time written another way (an
// exponent) is read as a float64, to the millisecond, as the APIs keep
// them.
func ParseTime(s string) (time.Time, error) {
	whole, frac, _ := strings.Cut(s, ".")
	digits := func(x string) bool { return x != "" && strings.Trim(x, "0123456789") == "" }
	neg := strings.HasPrefix(whole, "-")
	if w := strings.TrimPrefix(whole, "-"); digits(w) && len(w) <= 12 && (frac == "" || digits(frac)) {
		sec, err := strconv.ParseInt(w, 10, 64)
		if err != nil {
			return time.Time{}, err
		}
		if len(frac) > 9 {
			frac = frac[:9]
		}
		var ns int64
		if frac != "" {
			if ns, err = strconv.ParseInt(frac+strings.Repeat("0", 9-len(frac)), 10, 64); err != nil {
				return time.Time{}, err
			}
		}
		if neg {
			sec, ns = -sec, -ns
		}
		return time.Unix(sec, ns).UTC(), nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.Abs(f) > 1e12 {
		return time.Time{}, fmt.Errorf("unreadable time %q", s)
	}
	return time.UnixMilli(int64(math.Round(f * 1000))).UTC(), nil
}

// do sends a request with the client's credentials, in its turn by the
// client's Limiter, and reads the answer, limit bytes at most (more is
// errTooLarge). heed says whether the Limiter heeds the answer: it doesn't
// of a read lapilli does without when it fails (bestEffort).
func (c *Client) do(ctx context.Context, req *http.Request, heed bool, limit int64) ([]byte, *http.Response, error) {
	var body []byte
	resp, err := c.send(ctx, req, heed, func(r io.Reader) error {
		var err error
		body, err = io.ReadAll(io.LimitReader(r, limit+1))
		if err == nil && int64(len(body)) > limit {
			body, err = nil, errTooLarge{limit}
		}
		return err
	})
	return body, resp, err
}

// ErrTooLarge is in the error of an answer longer than lapilli reads of
// it, refused before it is parsed.
var ErrTooLarge = errors.New("an answer longer than is read of it")

// errTooLarge is an answer longer than lapilli reads of it.
type errTooLarge struct{ limit int64 }

func (e errTooLarge) Is(target error) bool { return target == ErrTooLarge }

func (e errTooLarge) Error() string {
	return fmt.Sprintf("the answer is over %d MiB, more than is read of it", e.limit>>20)
}

// send is do, the answer's body given to read, while the request holds its
// turn: what read fails with is the request's error.
func (c *Client) send(ctx context.Context, req *http.Request, heed bool, read func(io.Reader) error) (*http.Response, error) {
	if err := c.limit.take(ctx); err != nil {
		return nil, err
	}
	status, wait := 0, time.Duration(0)
	defer func() {
		if !heed {
			status, wait = 0, 0
		}
		c.limit.done(status, wait)
	}()
	req.Header.Set("User-Agent", c.agent)
	for _, h := range c.headers {
		req.Header.Set(h[0], h[1])
	}
	if c.token != nil {
		t, err := c.token(ctx)
		if err != nil {
			return nil, &credsError{err}
		}
		req.Header.Set("Authorization", "Bearer "+t)
	}
	if c.sign != nil {
		if err := c.sign(req); err != nil {
			return nil, &credsError{err}
		}
	}
	n := countsOf(ctx)
	if n != nil {
		n.Requests.Add(1)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if dropped(err) {
			// As an API shedding load does, by closing connections: as a
			// 503, asked again (retried) and pushing back.
			status = http.StatusServiceUnavailable
		}
		return nil, err
	}
	defer resp.Body.Close()
	status = resp.StatusCode
	if d, said := retryAfter(resp, clock()); said {
		// Every other request waits as this one does: a second at least,
		// so that "Retry-After: 0" can't bring them all straight back.
		wait = min(max(d, time.Second), maxRetryAfter)
	}
	if status == http.StatusTooManyRequests && n != nil && heed {
		// Counted as an answer the Limiter heeded: a run's end says it
		// asked fewer at once after it.
		n.TooMany.Add(1)
	}
	err = read(resp.Body)
	if err != nil && dropped(err) {
		status = http.StatusServiceUnavailable
	}
	return resp, err
}

// get asks for path, asking again as retried does, and gives the answer,
// or an APIError for a status that isn't 2xx.
func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	return retried(ctx, func(ctx context.Context) ([]byte, error) { return c.getOnce(ctx, path, true, maxBody) })
}

// getOnce asks for path once, reading limit bytes of the answer at most.
func (c *Client) getOnce(ctx context.Context, path string, heed bool, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	body, resp, err := c.do(ctx, req, heed, limit)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, apiError(req, resp, clip(serverText(strings.TrimSpace(string(body)), req), 200))
	}
	return body, nil
}

// bestEffortTime is how long a read lapilli does without when it fails is
// waited for; replaced in tests.
var bestEffortTime = 30 * time.Second

// bestEffort asks for path once, within bestEffortTime, not while the API
// said to wait, and the Limiter doesn't heed the answer: what it reads,
// lapilli does without when it fails, so a failure shouldn't cost the run
// a wait, and an answer to a request asked alone shouldn't halve how many
// go at once after it.
//
// What it reads is limit bytes at most: an answer longer is refused before
// it is parsed, as what is read of it is parsed whole.
func (c *Client) bestEffort(ctx context.Context, path string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.WithValue(ctx, noPauseKey{}, true), bestEffortTime)
	defer cancel()
	return c.getOnce(ctx, path, false, limit)
}

// What a read lapilli does without reads at most: a Prometheus's
// configuration, parsed whole as YAML at some 90 times its size, and its
// active targets, as JSON. A large configuration, of hundreds of jobs, is
// a few hundred KB.
const (
	maxConfig  = 2 << 20
	maxTargets = 32 << 20
)

// Rules asks for the alert rules the API has loaded, as GET /api/v1/rules
// answers; for a Loki, its ruler's /prometheus/api/v1/rules, which answers
// the same way.
func (c *Client) Rules(ctx context.Context) ([]byte, error) {
	return c.get(ctx, c.rulesPath())
}

// RulesOnce asks for them as a read lapilli can do without (bestEffort):
// once, within 30 seconds, not while the API said to wait, and without the
// Limiter heeding the answer. lapilli serve reads a Loki's ruler so each
// minute, between its queries.
func (c *Client) RulesOnce(ctx context.Context) ([]byte, error) {
	return c.bestEffort(ctx, c.rulesPath(), maxBody)
}

func (c *Client) rulesPath() string {
	if c.loki {
		return "/prometheus/api/v1/rules?type=alert"
	}
	return "/api/v1/rules?type=alert"
}

// StatusConfig is the configuration Prometheus runs with, as GET
// /api/v1/status/config answers it: YAML. Most managed services don't
// serve it, and it is asked once (bestEffort).
func (c *Client) StatusConfig(ctx context.Context) (string, error) {
	body, err := c.bestEffort(ctx, "/api/v1/status/config", maxConfig)
	if err != nil {
		return "", err
	}
	var a struct {
		Status string `json:"status"`
		Data   struct {
			YAML string `json:"yaml"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &a); err != nil || a.Status != "success" {
		return "", errors.New("not a Prometheus configuration")
	}
	return a.Data.YAML, nil
}

// Targets is the scrape targets Prometheus has active, as GET
// /api/v1/targets?state=active answers: JSON. Most managed services don't
// serve it, and it is asked once (bestEffort).
func (c *Client) Targets(ctx context.Context) ([]byte, error) {
	return c.bestEffort(ctx, "/api/v1/targets?state=active", maxTargets)
}

// key is a label set as one comparable string.
func key(l map[string]string) string {
	names := make([]string, 0, len(l))
	for k := range l {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, k := range names {
		fmt.Fprintf(&b, "%s=%q,", k, l[k])
	}
	return b.String()
}
