package main

import (
	"io"
	"testing"
)

// The notifications port wants what lets a notification in, and refuses
// what isn't one.
func TestServeNotifyFlags(t *testing.T) {
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
