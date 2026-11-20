// Package report sends a verdict on: its metrics to a Pushgateway, its text
// to a Slack incoming webhook; and what lapilli serve tells of, to a
// webhook, signed when it is given a key.
package report

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/egress"
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
	return SlackMessageFrom(ctx, file, "--slack-webhook-file", text)
}

// SlackMessageFrom is SlackMessage to the webhook in file, which flag
// named: its errors say the flag, never the file's URL.
func SlackMessageFrom(ctx context.Context, file, flag, text string) error {
	text = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
	// Shown as written: a name in it from outside, a rule's or a task's,
	// makes no bold text and no preview of a link.
	return slackPost(ctx, file, flag, text, false)
}

// SlackMarkup posts text as Slack's markup (mrkdwn) to the webhook in file,
// which flag named: a template an admin wrote, its links and mentions read
// as such, with everything in it from outside escaped already, by the
// caller. No link is previewed.
func SlackMarkup(ctx context.Context, file, flag, text string) error {
	return slackPost(ctx, file, flag, text, true)
}

func slackPost(ctx context.Context, file, flag, text string, mrkdwn bool) error {
	hook, err := secretURL(file, flag)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"text": text, "mrkdwn": mrkdwn, "unfurl_links": false, "unfurl_media": false})
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

// Webhook posts body, as JSON, to the URL in file, with key as its
// Idempotency-Key. The URL is a secret, as a Slack webhook's is: read from
// a file, never shown. With keyFile, the post is signed with the key in it,
// at the time at (Sign), read for each post so that it can be rotated.
func Webhook(ctx context.Context, file string, body []byte, key, keyFile string, at time.Time) error {
	return WebhookFrom(ctx, file, "--notify-webhook-file", body, key, keyFile, "--notify-webhook-secret-file", at)
}

// WebhookFrom is Webhook to the URL in file, which flag named, signed with
// the key in keyFile, which keyFlag named: its errors say the flag, never
// the URL or the key.
func WebhookFrom(ctx context.Context, file, flag string, body []byte, key, keyFile, keyFlag string, at time.Time) error {
	hook, err := secretURL(file, flag)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hook, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s: the file should hold a URL", flag)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "lapilli")
	req.Header.Set("Idempotency-Key", key)
	if keyFile != "" {
		// Never sent unsigned: a key that can't be read fails the post,
		// which is tried again.
		k, err := SigningKey(keyFile, keyFlag)
		if err != nil {
			return err
		}
		Sign(req.Header, k, body, at)
	}
	return send(req, "the webhook")
}

// SigningKeyLeast is how long a webhook's signing key is, in bytes, at
// least: as long as the HMAC-SHA256 it keys.
const SigningKeyLeast = 32

// SigningKey is the key in a file that a webhook's posts are signed with:
// what it holds, less one line break at its end (a file written with echo
// has one), 32 bytes at least. Its errors say flag, the flag that named the
// file (--notify-webhook-secret-file, or --webhook-dest-secret NAME), and
// never hold the key.
func SigningKey(file, flag string) ([]byte, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", flag, err)
	}
	k := TrimKey(b)
	if len(k) < SigningKeyLeast {
		return nil, fmt.Errorf("%s: want a key of %d bytes at least, not %d", flag, SigningKeyLeast, len(k))
	}
	return k, nil
}

// TrimKey is a key file's bytes less one line break at their end, \n or
// \r\n: the rest, spaces and all, is the key.
func TrimKey(b []byte) []byte {
	if k, ok := bytes.CutSuffix(b, []byte("\n")); ok {
		k, _ = bytes.CutSuffix(k, []byte("\r"))
		return k
	}
	return b
}

// Sign signs a post to a webhook, so that whoever takes it can tell it came
// from whoever holds key, as it was sent: X-Lapilli-Timestamp is at, in
// Unix seconds, and X-Lapilli-Signature is sha256= and the HMAC-SHA256,
// with key, of the timestamp, a dot and body, the bytes sent, in lowercase
// hex. Each try is signed again, at its own time.
func Sign(h http.Header, key, body []byte, at time.Time) {
	ts := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	h.Set("X-Lapilli-Timestamp", ts)
	h.Set("X-Lapilli-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
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
	Transport:     egress.Wrap(nil),
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
