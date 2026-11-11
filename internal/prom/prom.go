// Package prom asks a Prometheus-compatible HTTP API for the history of an
// expression: Prometheus, Thanos, Mimir, VictoriaMetrics, and the managed
// Prometheus of AWS, Azure and Google Cloud.
package prom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
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

// WithHTTPClient replaces the HTTP client.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// New returns a client for the API under base, such as
// http://prometheus:9090.
func New(base, userAgent string, opts ...Option) *Client {
	c := &Client{base: strings.TrimRight(base, "/"), http: http.DefaultClient, agent: userAgent}
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
	return strings.Contains(m, "exceeds maximum allowed size") || strings.Contains(m, "too many samples") ||
		strings.Contains(m, "exceeded maximum resolution") || strings.Contains(m, "response too large")
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
		case errors.As(err, &ae) && ae.tooBig() && end.Sub(start) >= 2*step:
			mid := start.Add(end.Sub(start) / 2).Truncate(step)
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

func (c *Client) queryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]Series, error) {
	q := url.Values{
		"query": {expr},
		"start": {strconv.FormatInt(start.Unix(), 10)},
		"end":   {strconv.FormatInt(end.Unix(), 10)},
		"step":  {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v1/query_range", strings.NewReader(q.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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
		return nil, &APIError{Status: resp.StatusCode, Msg: "not a Prometheus API response: " + msg}
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
	if r.Data.ResultType != "matrix" {
		return nil, fmt.Errorf("want a matrix, got %q", r.Data.ResultType)
	}
	out := make([]Series, 0, len(r.Data.Result))
	for _, res := range r.Data.Result {
		s := Series{Labels: res.Metric, Samples: make([]Sample, 0, len(res.Values))}
		for _, v := range res.Values {
			// [1730419200.5, "42"]: a number, then the value as a string.
			ts, err1 := strconv.ParseFloat(string(v[0]), 64)
			f, err2 := strconv.ParseFloat(strings.Trim(string(v[1]), `"`), 64)
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("unreadable sample [%s, %s]", v[0], v[1])
			}
			s.Samples = append(s.Samples, Sample{T: time.Unix(0, int64(ts*1e9)).UTC(), V: f})
		}
		out = append(out, s)
	}
	return out, nil
}

// do sends a request with the client's credentials and reads the answer.
func (c *Client) do(ctx context.Context, req *http.Request) ([]byte, *http.Response, error) {
	req.Header.Set("User-Agent", c.agent)
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
// answers.
func (c *Client) Rules(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1/rules?type=alert", nil)
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
