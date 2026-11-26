package main

import (
	"slices"
	"strings"
	"testing"
)

// serve.notify.webhookDests are the JSON webhooks routes send to by name:
// each Secret mounted apart, its URL as --webhook-dest and, with
// signingKey, its key as --webhook-dest-secret-file. Refused without
// serve.config, where the routes are; with a name reports.destinations
// has, or one no destination may have; and with the URL's key as its
// signing key.
func TestChartWebhookDests(t *testing.T) {
	parallel(t)
	routed := append(slices.Clone(serveValues), "--set", "serve.config=notify: {default: [payments-hook]}")
	dest := []string{"--set", "serve.notify.webhookDests[0].name=payments-hook", "--set", "serve.notify.webhookDests[0].secretName=lapilli-payments-hook"}
	out, err := helmTemplate(t, slices.Concat(routed, dest, []string{"--set", "serve.notify.webhookDests[0].signingKey=key"})...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{`- "--webhook-dest=payments-hook=/var/run/lapilli/webhook-dests/0/url"`,
		`- "--webhook-dest-secret-file=payments-hook=/var/run/lapilli/webhook-dests/0/key"`, "mountPath: /var/run/lapilli/webhook-dests/0",
		"secretName: lapilli-payments-hook", "- key: url\n                path: url\n              - key: key\n                path: key"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	out, err = helmTemplate(t, slices.Concat(routed, dest)...)
	if err != nil || !strings.Contains(out, "--webhook-dest=payments-hook=") || strings.Contains(out, "--webhook-dest-secret-file") {
		t.Errorf("without a signing key: %v\n%s", err, out)
	}
	for _, c := range []struct {
		args []string
		says string
	}{
		{slices.Concat(serveValues, dest), "give serve.config"},
		{slices.Concat(routed, dest, []string{"--set", "serve.reports.destinations[0].name=payments-hook", "--set", "serve.reports.destinations[0].secretName=s"}),
			"a name serve.reports.destinations has"},
		{slices.Concat(routed, []string{"--set", "serve.notify.webhookDests[0].name=Payments", "--set", "serve.notify.webhookDests[0].secretName=s"}),
			"a name is lowercase letters"},
		{slices.Concat(routed, []string{"--set", "serve.notify.webhookDests[0].name=slack", "--set", "serve.notify.webhookDests[0].secretName=s"}),
			"slack is the name of"},
		{slices.Concat(routed, []string{"--set", "serve.notify.webhookDests[0].name=payments-hook"}), "needs a secretName"},
		{slices.Concat(routed, dest, []string{"--set", "serve.notify.webhookDests[0].signingKey=url"}), "a key of its own"},
	} {
		if out, err := helmTemplate(t, c.args...); err == nil || !strings.Contains(out, c.says) {
			t.Errorf("%v: %v\n%s", c.args, err, out)
		}
	}
}
