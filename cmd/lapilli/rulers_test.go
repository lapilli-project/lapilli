package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// lapilli verdict reads a Mimir's rules with --ruler, as written, with its
// token and its tenant; rules of one name in two namespaces are told apart
// by both.
func TestRulerRules(t *testing.T) {
	alone(t)
	newHistory = func(context.Context, *verdictFlags) (history, error) { return fakeHistory{}, nil }
	now = func() time.Time { return t0 }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ruler-token" || r.Header.Get("X-Scope-OrgID") != "team-a" || r.URL.Path != "/prometheus/config/v1/rules" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		io.WriteString(w, `shop:
    - name: shop
      rules:
        - alert: CheckoutErrors
          expr: errors > 5
          for: 5m
payments:
    - name: shop
      rules:
        - alert: CheckoutErrors
          expr: errors > 7
`)
	}))
	defer srv.Close()
	token := write(t, "token", "ruler-token\n")
	incidents := write(t, "incidents.yaml", "incidents:\n  - {id: INC-7, title: Checkout down, start: 2026-11-01T21:00:00Z, end: 2026-11-01T22:00:00Z}\n")
	var stdout, stderr bytes.Buffer
	args := []string{"verdict", "--ruler", srv.URL + "/prometheus/config/v1/rules", "--ruler-token-file", token, "--ruler-tenant", "team-a",
		"--prometheus", "http://p", "--incidents", incidents, "--tz", "UTC"}
	if code := run(args, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "CheckoutErrors [shop/shop] +5m") ||
		!strings.Contains(stdout.String(), "1 rule never fired:") {
		t.Fatalf("exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
}

// --ruler's flags are refused, before anything is read, where they can't
// go: Amazon's query URL, Amazon or Azure without their credentials, a
// token or a tenant for a cloud, or without --ruler.
func TestRulerFlags(t *testing.T) {
	parallel(t)
	incidents := write(t, "incidents.yaml", "incidents: []\n")
	tok := write(t, "tok", "ruler-token-0123456789\n")
	base := []string{"verdict", "--prometheus", "http://p", "--incidents", incidents}
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"Amazon's query URL": {[]string{"--ruler", "https://aps-workspaces.us-east-1.amazonaws.com/workspaces/ws-1"},
			"https://aps.us-east-1.amazonaws.com/workspaces/ws-1"},
		"Amazon, no --auth":     {[]string{"--ruler", "https://aps.us-east-1.amazonaws.com/workspaces/ws-1"}, "add --auth aws"},
		"Azure, no --auth":      {[]string{"--ruler", "https://management.azure.com/subscriptions/s"}, "add --auth azure"},
		"Azure, Amazon's auth":  {[]string{"--ruler", "https://management.azure.com/subscriptions/s", "--auth", "aws"}, "add --auth azure"},
		"a token for Amazon":    {[]string{"--ruler", "https://aps.us-east-1.amazonaws.com/workspaces/ws-1", "--auth", "aws", "--ruler-token-file", tok}, "Mimir's or a Cortex's token"},
		"a token, no ruler":     {[]string{"--rules", incidents, "--ruler-token-file", tok}, "go with --ruler"},
		"a tenant, no ruler":    {[]string{"--rules", incidents, "--ruler-tenant", "a"}, "go with --ruler"},
		"a tenant with a slash": {[]string{"--ruler", "http://mimir/rules", "--ruler-tenant", "a/b"}, "a tenant's ID"},
		"two places":            {[]string{"--ruler", "http://mimir/rules", "--rules", incidents}, "both say where the firings come from"},
		"history":               {[]string{"--ruler", "http://mimir/rules", "--rules-history", t.TempDir()}, "--rules-history"},
	} {
		var out, errs strings.Builder
		if code := run(append(append([]string{}, base...), c.args...), &out, &errs); code != 2 || !strings.Contains(errs.String(), c.want) {
			t.Errorf("%s: %d, want 2 and %q\n%s", name, code, c.want, errs.String())
		}
	}
}

// lapilli serve's changes through a ruler: each credential for its own
// kind of ruler, with --changes, and --changes-in for --ruler.
func TestServeRulerFlags(t *testing.T) {
	parallel(t)
	tok := write(t, "tok", "ruler-writes-0123456789\n")
	same := write(t, "same", "ruler-writes-0123456789\n")
	signed := []string{"--changes", "--user-header", "X-Forwarded-Email", "--admins", "ana@example.com"}
	mimir := []string{"--ruler", "http://mimir:8080/prometheus/config/v1/rules", "--prometheus", "http://mimir:8080/prometheus"}
	amazon := []string{"--ruler", "https://aps.us-east-1.amazonaws.com/workspaces/ws-1", "--auth", "aws",
		"--prometheus", "https://aps-workspaces.us-east-1.amazonaws.com/workspaces/ws-1"}
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"in, no changes":           {append([]string{"--changes-in", "shop"}, mimir...), "go with --changes"},
		"in, no ruler":             {append([]string{"--changes-in", "shop", "--rules", tok, "--prometheus", "http://p"}, signed...), "--ruler reads"},
		"in, a comma only":         {append(append([]string{"--changes-in", ","}, mimir...), signed...), "--changes-in"},
		"in, a star inside":        {append(append([]string{"--changes-in", "te*am"}, mimir...), signed...), "--changes-in"},
		"managed, Mimir":           {append(append([]string{"--changes-in", "shop", "--changes-managed", "team=*"}, mimir...), signed...), "--changes-managed names tags"},
		"managed, no value":        {append(append([]string{"--changes-managed", "team"}, amazon...), signed...), "KEY=VALUE"},
		"Mimir's token, Amazon":    {append(append([]string{"--changes-ruler-token-file", tok}, amazon...), signed...), "--changes-ruler-token-file is for"},
		"Mimir's token, short":     {append(append([]string{"--changes-ruler-token-file", write(t, "short", "x")}, mimir...), signed...), "16 characters"},
		"the read token to write":  {append(append([]string{"--changes-ruler-token-file", tok, "--ruler-token-file", same}, mimir...), signed...), "same token"},
		"a role, Mimir":            {append(append([]string{"--changes-aws-role", "arn:aws:iam::123456789012:role/r"}, mimir...), signed...), "--changes-aws-role is for"},
		"a role that isn't":        {append(append([]string{"--changes-aws-role", "arn:aws:s3:::bucket"}, amazon...), signed...), "a role's ARN"},
		"a client ID, Amazon":      {append(append([]string{"--changes-azure-client-id", "00000000-0000-0000-0000-000000000000"}, amazon...), signed...), "--changes-azure-client-id is for"},
		"a client ID that isn't":   {append(append([]string{"--changes-azure-client-id", "ana"}, amazon...), signed...), "a client ID"},
		"a role, without changes":  {append([]string{"--changes-aws-role", "arn:aws:iam::123456789012:role/r"}, amazon...), "go with --changes"},
		"a token, without changes": {append([]string{"--changes-ruler-token-file", tok}, mimir...), "go with --changes"},
	} {
		var out, errs strings.Builder
		if code := run(append([]string{"serve"}, c.args...), &out, &errs); code != 2 || !strings.Contains(errs.String(), c.want) {
			t.Errorf("%s: %d, want 2 and %q\n%s", name, code, c.want, errs.String())
		}
	}
}

// lapilli serve's changes through Loki's ruler, read with
// --loki-loaded-rules: its namespaces named loki:NAMESPACE in --changes-in,
// a token of their own to write with, and no Loki namespace named without
// Loki's ruler read.
func TestServeLokiChangesFlags(t *testing.T) {
	parallel(t)
	tok := write(t, "tok", "loki-writes-0123456789\n")
	same := write(t, "same", "loki-writes-0123456789\n")
	signed := []string{"--changes", "--user-header", "X-Forwarded-Email", "--admins", "ana@example.com"}
	loki := []string{"--loki", "http://loki:3100", "--loki-loaded-rules", "--prometheus", "http://p"}
	dir := t.TempDir()
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"a token, files alone":    {append([]string{"--changes-loki-token-file", tok, "--loki", "http://loki:3100", "--loki-rules", dir, "--prometheus", "http://p"}, signed...), "--changes-loki-token-file is for"},
		"a token, short":          {append(append([]string{"--changes-loki-token-file", write(t, "short", "x")}, loki...), signed...), "16 characters"},
		"the read token to write": {append(append([]string{"--changes-loki-token-file", tok, "--loki-token-file", same}, loki...), signed...), "same token as --loki-token-file"},
		"a token, no changes":     {append([]string{"--changes-loki-token-file", tok}, loki...), "go with --changes"},
		"loki:, files alone":      {append([]string{"--changes-in", "loki:shop", "--loki", "http://loki:3100", "--loki-rules", dir, "--prometheus", "http://p", "--alarms", "cw=https://monitoring.eu-west-1.amazonaws.com"}, signed...), "names a namespace of Loki's ruler"},
		"alarms named loki":       {append(append([]string{"--changes-in", "loki:shop", "--alarms", "loki=https://monitoring.eu-west-1.amazonaws.com"}, loki...), signed...), "--alarms loki="},
	} {
		var out, errs strings.Builder
		if code := run(append([]string{"serve"}, c.args...), &out, &errs); code != 2 || !strings.Contains(errs.String(), c.want) {
			t.Errorf("%s: %d, want 2 and %q\n%s", name, code, c.want, errs.String())
		}
	}
	// Taken: Loki's namespaces alone, with a token to write; and beside a
	// Mimir's, loki:shop is Loki's, shop the Mimir's.
	for _, args := range [][]string{
		append(append([]string{"--changes-in", "loki:shop,loki:team-*", "--changes-loki-token-file", tok}, loki...), signed...),
		append(append([]string{"--changes-in", "shop,loki:shop", "--ruler", "http://mimir:8080/prometheus/config/v1/rules"}, loki...), signed...),
	} {
		got, err := readServe(args, false)
		if err != nil {
			t.Errorf("%v: %v", args, err)
			continue
		}
		if !got.cfg.LokiLoaded || !slices.Contains(got.cfg.ChangesIn, "loki:shop") {
			t.Errorf("%v: %+v", args, got.cfg.ChangesIn)
		}
		if slices.Contains(args, "--changes-loki-token-file") && got.cfg.ChangesLokiToken != tok {
			t.Errorf("the write token: %q", got.cfg.ChangesLokiToken)
		}
	}
}
