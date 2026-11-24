package main

import (
	"io"
	"strings"
	"testing"
)

// The notifications port wants what lets a notification in, and refuses
// what isn't one.
func TestServeNotifyFlags(t *testing.T) {
	parallel(t)
	rulesFile := write(t, "rules.yaml", "groups:\n  - name: g\n    rules:\n      - {alert: A, expr: a}\n")
	short := write(t, "short", "too short")
	good := write(t, "good", "a-token-long-enough")
	for _, bad := range [][]string{
		{"--notify-listen", ":8083"},
		{"--notify-token-file", good},
		{"--notify-sns", "arn:aws:sns:us-east-1:123456789012:alerts"},
		{"--notify-listen", ":8083", "--notify-token-file", short},
		{"--notify-listen", ":8083", "--notify-grafana-secret-file", short},
		{"--notify-listen", ":8083", "--notify-token-file", "/no/such/file"},
		{"--notify-listen", ":8083", "--notify-token-file", good, "--read-only"},
		{"--notify-listen", ":8083", "--notify-sns", "alerts"},
		{"--notify-listen", ":8083", "--notify-sns", "arn:aws:sns:us-east-1:123:alerts"},
		{"--notify-listen", ":8083", "--notify-sns", "arn:aws:sqs:us-east-1:123456789012:alerts"},
	} {
		args := append([]string{"serve", "--rules", rulesFile, "--prometheus", "http://x"}, bad...)
		if code := run(args, io.Discard, io.Discard); code != 2 {
			t.Errorf("%v: exit %d", bad, code)
		}
	}
}

// The webhook's signing key is refused at start, saying why and never
// what it is: unreadable, shorter than 32 bytes once a line break at its
// end is let go, or with no webhook to sign the posts to.
func TestServeNotifyWebhookSecretFlags(t *testing.T) {
	parallel(t)
	rulesFile := write(t, "rules.yaml", "groups:\n  - name: g\n    rules:\n      - {alert: A, expr: a}\n")
	hook := write(t, "hook", "https://hooks.example/lapilli")
	short := write(t, "short", "0123456789abcdef0123456789abcde\n") // 31 bytes and a line break
	good := write(t, "good", "0123456789abcdef0123456789abcdef\r\n")
	base := []string{"--rules", rulesFile, "--prometheus", "http://x"}
	for _, bad := range [][]string{
		{"--notify-webhook-secret-file", good},
		{"--notify-webhook-file", hook, "--notify-webhook-secret-file", short},
		{"--notify-webhook-file", hook, "--notify-webhook-secret-file", "/no/such/file"},
	} {
		var stderr strings.Builder
		if code := run(append(append([]string{"serve"}, base...), bad...), io.Discard, &stderr); code != 2 ||
			!strings.Contains(stderr.String(), "--notify-webhook-secret-file") || strings.Contains(stderr.String(), "0123456789") {
			t.Errorf("%v: exit %d, %s", bad, code, stderr.String())
		}
	}
	st, err := readServe(append(base, "--notify-webhook-file", hook, "--notify-webhook-secret-file", good), false)
	if err != nil || st.cfg.NotifyWebhookSecret != good || st.cfg.NotifyWebhook != hook {
		t.Errorf("%+v %v", st.cfg, err)
	}
}
