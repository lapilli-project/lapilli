package main

import (
	"io"
	"strings"
	"testing"
)

// --webhook-dest names a JSON webhook routes in --config's notify: send
// to, its URL in a file, as --notify-webhook-file's is; --webhook-dest-secret
// its signing key, 32 bytes at least. A name is a destination's name, not
// slack, webhook or team:, nor one --report-dest gives; a key names a
// --webhook-dest; and either needs --config, where the routes are.
func TestServeWebhookDestFlags(t *testing.T) {
	rulesFile := write(t, "rules.yaml", "groups:\n  - name: g\n    rules:\n      - {alert: A, expr: a}\n")
	config := write(t, "config.yaml", "notify:\n  default: [payments-hook]\n")
	hook := write(t, "hook", "https://hooks.example.com/payments")
	key := write(t, "key", "payments-key-of-thirty-two-bytes")
	short := write(t, "short", "short")
	base := []string{"serve", "--rules", rulesFile, "--prometheus", "http://x"}
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"--webhook-dest", "payments-hook=" + hook}, "--webhook-dest takes what a route in --config's notify: sends it: give --config"},
		{[]string{"--config", config, "--webhook-dest", "Payments=" + hook}, "--webhook-dest Payments=" + hook + ": want a name"},
		{[]string{"--config", config, "--webhook-dest", "slack=" + hook}, "want a name (lowercase letters, digits and . _ -, not slack, webhook or team:)"},
		{[]string{"--config", config, "--webhook-dest", "team:payments=" + hook}, "not slack, webhook or team:"},
		{[]string{"--config", config, "--webhook-dest", "payments-hook"}, "want a name"},
		{[]string{"--config", config, "--webhook-dest", "payments-hook=/no/such/hook"}, "no such file"},
		{[]string{"--config", config, "--webhook-dest", "payments-hook=" + hook, "--webhook-dest-secret", "payments-hook=" + short}, "--webhook-dest-secret payments-hook: want a key of 32 bytes at least, not 5"},
		{[]string{"--config", config, "--webhook-dest", "payments-hook=" + hook, "--webhook-dest-secret", "checkout-hook=" + key},
			"--webhook-dest-secret checkout-hook signs the posts to --webhook-dest checkout-hook: give it too"},
		{[]string{"--config", config, "--webhook-dest", "ops=" + hook, "--report-dest", "ops=" + hook}, "--webhook-dest ops: --report-dest gives a destination of that name already"},
	} {
		var errs strings.Builder
		if code := run(append(append([]string{}, base...), c.args...), io.Discard, &errs); code != 2 || !strings.Contains(errs.String(), c.says) {
			t.Errorf("%v: exit %d, want 2 saying %q:\n%s", c.args, code, c.says, errs.String())
		}
	}
	set, err := readServe(append(base[1:], "--config", config, "--webhook-dest", "payments-hook="+hook, "--webhook-dest-secret", "payments-hook="+key,
		"--webhook-dest", "checkout-hook="+hook), false)
	if err != nil {
		t.Fatal(err)
	}
	if set.cfg.WebhookDests["payments-hook"] != hook || set.cfg.WebhookDests["checkout-hook"] != hook || len(set.cfg.WebhookDestSecrets) != 1 ||
		set.cfg.WebhookDestSecrets["payments-hook"] != key {
		t.Errorf("read: %v %v", set.cfg.WebhookDests, set.cfg.WebhookDestSecrets)
	}
}
