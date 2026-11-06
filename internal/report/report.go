// Package report sends a verdict on: its metrics to a Pushgateway, its text
// to a Slack incoming webhook.
package report

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"lapilli.dev/internal/verdict"
)

// Push puts the verdict's metrics on a Pushgateway, in place of what
// the last run put in the same group: job lapilli, unless the URL names a
// group itself (http://pushgateway:9091/metrics/job/lapilli/instance/shop), as
// it should when more than one lapilli pushes there.
func Push(ctx context.Context, base string, rep *verdict.Report, now time.Time) error {
	var b bytes.Buffer
	if err := verdict.Prometheus(&b, rep, now); err != nil {
		return err
	}
	u := strings.TrimRight(base, "/")
	if !strings.Contains(u, "/metrics/job/") {
		u += "/metrics/job/lapilli"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, &b)
	if err != nil {
		return fmt.Errorf("--push: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain; version=0.0.4")
	return send(req, "the Pushgateway")
}

// slackMax is how much of a report goes in one Slack message, which takes
// 40,000 characters at most.
const slackMax = 39_000

// Slack posts the text report to a Slack incoming webhook. The webhook's
// URL is a secret, so it is read from a file and never shown, errors
// included.
func Slack(ctx context.Context, file, report string) error {
	return SlackFrom(ctx, file, "--slack-webhook-file", report)
}

// SlackFrom is Slack with a webhook given by another flag, which its errors
// name.
func SlackFrom(ctx context.Context, file, flag, report string) error {
	b, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("%s: %w", flag, err)
	}
	hook := strings.TrimSpace(string(b))
	u, err := url.Parse(hook)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("%s: the file should hold the webhook's URL", flag)
	}
	body, err := json.Marshal(map[string]string{"text": SlackText(report)})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hook, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s: the file should hold the webhook's URL", flag)
	}
	req.Header.Set("Content-Type", "application/json")
	return send(req, "Slack")
}

// SlackText is a report as Slack takes it: in a code block, cut to what one
// message holds, and with what Slack reads as links and mentions escaped,
// which it reads even in a code block: a rule named <!channel> would notify
// everyone.
func SlackText(report string) string {
	text := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "```", "'''").Replace(report)
	if len(text) > slackMax {
		text = strings.ToValidUTF8(text[:slackMax], "") + "\n…"
	}
	return "```\n" + text + "\n```"
}

// SlackMessage posts one line of text, not a report, to the Slack incoming
// webhook whose URL is in file: for what happened, as it happens.
func SlackMessage(ctx context.Context, file, text string) error {
	hook, err := secretURL(file, "--slack-webhook-file")
	if err != nil {
		return err
	}
	text = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hook, bytes.NewReader(body))
	if err != nil {
		return errors.New("--slack-webhook-file: the file should hold the webhook's URL")
	}
	req.Header.Set("Content-Type", "application/json")
	return send(req, "Slack")
}

// Webhook posts body, as JSON, to the URL in file, with key as its
// Idempotency-Key. The URL is a secret, as a Slack webhook's is: read from
// a file, never shown.
func Webhook(ctx context.Context, file string, body []byte, key string) error {
	hook, err := secretURL(file, "--notify-webhook-file")
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hook, bytes.NewReader(body))
	if err != nil {
		return errors.New("--notify-webhook-file: the file should hold a URL")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "lapilli")
	req.Header.Set("Idempotency-Key", key)
	return send(req, "the webhook")
}

// secretURL reads an http or https URL from a file.
func secretURL(file, flag string) (string, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("%s: %w", flag, err)
	}
	hook := strings.TrimSpace(string(b))
	u, err := url.Parse(hook)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", fmt.Errorf("%s: the file should hold a URL", flag)
	}
	return hook, nil
}

// client doesn't follow redirects: Go would turn a POST into a GET, and
// send the webhook's secret URL along as the Referer.
var client = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func send(req *http.Request, to string) error {
	resp, err := client.Do(req)
	if err != nil {
		// The error names the URL, which for a webhook is the secret.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("sending to %s: %w", to, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("sending to %s: HTTP %d: %s", to, resp.StatusCode, verdict.Clean(strings.TrimSpace(string(msg))))
	}
	return nil
}
