// Package prom asks a Prometheus-compatible HTTP API for the history of an
// expression: Prometheus, Thanos, Mimir, VictoriaMetrics, and the managed
// Prometheus of AWS, Azure and Google Cloud; and Loki, for LogQL, whose
// query API answers the same way.
package prom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
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
	c := &Client{base: strings.TrimRight(base, "/"), http: client, agent: userAgent}
	for _, o := range opts {
		o(c)
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
		part, err := c.fetch(ctx, expr, from, to, step)
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

// APIError is an error answer from the API.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string {
	// The text is the server's: nothing in it should act on a terminal.
	msg := strings.Map(func(c rune) rune {
		if unicode.IsControl(c) || unicode.Is(unicode.Cf, c) {
			return -1
		}
		return c
	}, e.Msg)
	return fmt.Sprintf("HTTP %d: %s", e.Status, msg)
}

// tooBig reports an answer too large for the API to send, which a shorter
// range fixes.
func (e *APIError) tooBig() bool {
	m := strings.ToLower(e.Msg)
	// Loki counts a range's series over the whole range, and says to
	// shorten it: "maximum number of series (500) reached for a single
	// query; consider ... reducing the time range" (3.7.9).
	return strings.Contains(m, "exceeds maximum allowed size") || strings.Contains(m, "too many samples") ||
		strings.Contains(m, "exceeded maximum resolution") || strings.Contains(m, "response too large") ||
		strings.Contains(m, "maximum number of series")
}

// transient reports an overloaded or briefly unavailable API.
func (e *APIError) transient() bool {
	return !e.tooBig() && (e.Status == http.StatusTooManyRequests || e.Status >= 500)
}

// retryWait is the first pause before asking again; it doubles each time.
var retryWait = 2 * time.Second

// fetch asks for one range, asking again when the API is briefly
// unavailable and halving the range when the answer is too large.
func (c *Client) fetch(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]Series, error) {
	wait := retryWait
	for attempt := 0; ; attempt++ {
		got, err := c.queryRange(ctx, expr, start, end, step)
		var ae *APIError
		switch {
		case err == nil:
			return got, nil
		case errors.As(err, &ae) && ae.tooBig() && end.Sub(start) >= step:
			// Halved on the range's own grid: a range placed off the
			// step's, as a group placed within its interval is, stays on it.
			mid := start.Add(end.Sub(start) / 2 / step * step)
			a, err := c.fetch(ctx, expr, start, mid, step)
			if err != nil {
				return nil, err
			}
			b, err := c.fetch(ctx, expr, mid.Add(step), end, step)
			if err != nil {
				return nil, err
			}
			return join(a, b), nil
		case errors.As(err, &ae) && ae.transient() && attempt < 4:
			select {
			case <-ctx.Done():
				return nil, err
			case <-time.After(wait):
			}
			wait *= 2
		default:
			return nil, err
		}
	}
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
	body, resp, err := c.do(ctx, req)
	if err != nil {
		return nil, err
	}
	var r struct {
		Status   string   `json:"status"`
		Error    string   `json:"error"`
		Warnings []string `json:"warnings"`
		Data     struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string    `json:"metric"`
				Values [][2]json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		if c.loki && resp.StatusCode/100 != 2 {
			// Loki says what went wrong in plain text.
			return nil, &APIError{Status: resp.StatusCode, Msg: msg}
		}
		what := "a Prometheus"
		if c.loki {
			what = "a Loki"
		}
		return nil, &APIError{Status: resp.StatusCode, Msg: "not " + what + " API response: " + msg}
	}
	if r.Status != "success" {
		return nil, &APIError{Status: resp.StatusCode, Msg: r.Error}
	}
	// An API that caps the series in an answer may say so only in a warning
	// (CloudWatch: "result truncated to the maximum of 500 series"). A replay
	// of what is left would be wrong without saying so.
	for _, w := range r.Warnings {
		if strings.Contains(strings.ToLower(w), "truncated") {
			return nil, &APIError{Status: resp.StatusCode, Msg: "the answer left series out: " + w}
		}
	}
	if c.loki && r.Data.ResultType == "streams" {
		return nil, &APIError{Status: http.StatusBadRequest, Msg: "a log query, which returns log lines, not numbers: only a metric query, such as count_over_time, is replayed"}
	}
	if r.Data.ResultType != "matrix" {
		return nil, fmt.Errorf("want a matrix, got %q", r.Data.ResultType)
	}
	out := make([]Series, 0, len(r.Data.Result))
	for _, res := range r.Data.Result {
		s := Series{Labels: res.Metric, Samples: make([]Sample, 0, len(res.Values))}
		for _, v := range res.Values {
			// [1730419200.5, "42"]: a number, then the value as a string.
			ts, err1 := ParseTime(string(v[0]))
			f, err2 := strconv.ParseFloat(strings.Trim(string(v[1]), `"`), 64)
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("unreadable sample [%s, %s]", v[0], v[1])
			}
			s.Samples = append(s.Samples, Sample{T: ts, V: f})
		}
		out = append(out, s)
	}
	if c.loki {
		return lokiOnly(out, start, end, step), nil
	}
	return out, nil
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

// do sends a request with the client's credentials and reads the answer.
func (c *Client) do(ctx context.Context, req *http.Request) ([]byte, *http.Response, error) {
	req.Header.Set("User-Agent", c.agent)
	for _, h := range c.headers {
		req.Header.Set(h[0], h[1])
	}
	if c.token != nil {
		t, err := c.token(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("credentials: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+t)
	}
	if c.sign != nil {
		if err := c.sign(req); err != nil {
			return nil, nil, fmt.Errorf("credentials: %w", err)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	return body, resp, err
}

// Rules asks for the alert rules the API has loaded, as GET /api/v1/rules
// answers; for a Loki, its ruler's /prometheus/api/v1/rules, which answers
// the same way.
func (c *Client) Rules(ctx context.Context) ([]byte, error) {
	path := "/api/v1/rules?type=alert"
	if c.loki {
		path = "/prometheus/api/v1/rules?type=alert"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	body, resp, err := c.do(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, &APIError{Status: resp.StatusCode, Msg: msg}
	}
	return body, nil
}

// StatusConfig is the configuration Prometheus runs with, as GET
// /api/v1/status/config answers it: YAML. Most managed services don't
// serve it.
func (c *Client) StatusConfig(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1/status/config", nil)
	if err != nil {
		return "", err
	}
	body, resp, err := c.do(ctx, req)
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return "", &APIError{Status: resp.StatusCode, Msg: msg}
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
// serve it.
func (c *Client) Targets(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1/targets?state=active", nil)
	if err != nil {
		return nil, err
	}
	body, resp, err := c.do(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, &APIError{Status: resp.StatusCode, Msg: msg}
	}
	return body, nil
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
