package oncall

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Client is how feeds are fetched: thirty seconds, no redirect followed.
// PagerDuty answers a bad key with a redirect to a page, and Grafana's
// http-to-https redirect drops its path: neither is the feed.
func Client() *http.Client {
	return &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Read reads a schedule's feed, from its file or its URL, and only from
// there: the URL is read from its file each time, so it can be rotated,
// and is never said in an error, since its secret is in it.
func (s *Schedule) Read(ctx context.Context, client *http.Client, userAgent string) ([]byte, error) {
	if s.File != "" {
		f, err := os.Open(s.File)
		if err != nil {
			return nil, fmt.Errorf("reading the feed's file: %w", err)
		}
		defer f.Close()
		return readAll(f)
	}
	raw := s.URL
	if s.URLFile != "" {
		b, err := os.ReadFile(s.URLFile)
		if err != nil {
			return nil, fmt.Errorf("reading the file of the feed's URL: %w", err)
		}
		raw = strings.TrimSpace(string(b))
	}
	u, err := FeedURL(raw)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("the feed's URL isn't one")
	}
	req.Header.Set("Accept", "text/calendar")
	req.Header.Set("User-Agent", userAgent)
	if s.TokenFile != "" {
		b, err := os.ReadFile(s.TokenFile)
		token := strings.TrimSpace(string(b))
		if err != nil || token == "" || strings.ContainsAny(token, "\r\n") {
			return nil, errors.New("the token's file can't be read, or holds no token")
		}
		switch s.Auth {
		case "genie":
			req.Header.Set("Authorization", "GenieKey "+token)
		case "basic":
			req.SetBasicAuth(s.User, token)
		default:
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	if client == nil {
		client = Client()
	}
	resp, err := client.Do(req)
	if err != nil {
		// A url.Error says the URL, and its secret: only what went wrong.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("fetching the feed from %s: %v", u.Host, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		to := "elsewhere"
		if l, err := resp.Location(); err == nil {
			to = l.Host
		}
		return nil, fmt.Errorf("%s answered %d, a redirect to %s, which isn't followed: is the URL, or its key, right?", u.Host, resp.StatusCode, to)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%s answered %d %s", u.Host, resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return readAll(resp.Body)
}

func readAll(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxFeed+1))
	if err != nil {
		return nil, fmt.Errorf("reading the feed: %v", err)
	}
	if len(b) > MaxFeed {
		return nil, fmt.Errorf("the feed is over %d MB", MaxFeed>>20)
	}
	return b, nil
}

// Fetch reads a schedule's feed and its turns.
func (c *Config) Fetch(ctx context.Context, s *Schedule, client *http.Client, userAgent string, now time.Time) (*Feed, error) {
	b, err := s.Read(ctx, client, userAgent)
	if err != nil {
		return nil, err
	}
	return Parse(b, s, c, now)
}
