package main

import (
	"strings"
	"testing"
)

// A sign-in of lapilli's own is one way to say who people are, given whole,
// or lapilli serve doesn't start.
func TestServeSignInFlags(t *testing.T) {
	hook := write(t, "hook", "https://hooks.example/x")
	secret := write(t, "secret", "s3cret\n")
	empty := write(t, "empty", "\n")
	shortKey := write(t, "short-key", "too-short\n")
	oidc := []string{"--oidc-issuer", "https://idp.example.com", "--oidc-client-id", "lapilli", "--external-url", "https://lapilli.example.com"}
	iap := []string{"--proxy-jwt", "google-iap", "--proxy-jwt-audience", "/projects/123456789012/global/backendServices/42"}
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"two sign-ins":            {append(append([]string{}, oidc...), "--user-header", "X-Forwarded-Email"), "give one"},
		"three sign-ins":          {append(append(append([]string{}, oidc...), iap...), "--user-header", "X-Forwarded-Email"), "give one"},
		"OIDC and a proxy's JWT":  {append(append([]string{}, oidc...), iap...), "give one"},
		"an issuer over http":     {[]string{"--oidc-issuer", "http://idp.example.com", "--oidc-client-id", "x", "--external-url", "https://lapilli.example.com"}, "--oidc-issuer http://idp.example.com"},
		"no client":               {[]string{"--oidc-issuer", "https://idp.example.com", "--external-url", "https://lapilli.example.com"}, "go together"},
		"no issuer":               {[]string{"--oidc-client-id", "x", "--external-url", "https://lapilli.example.com"}, "go together"},
		"no external URL":         {[]string{"--oidc-issuer", "https://idp.example.com", "--oidc-client-id", "x"}, "needs --external-url"},
		"over http, elsewhere":    {[]string{"--oidc-issuer", "https://idp.example.com", "--oidc-client-id", "x", "--external-url", "http://lapilli.example.com"}, "want https, or http to this machine"},
		"an empty secret":         {append(append([]string{}, oidc...), "--oidc-client-secret-file", empty), "the file is empty"},
		"a short session key":     {append(append([]string{}, oidc...), "--session-key-file", shortKey), "32 characters"},
		"sessions too long":       {append(append([]string{}, oidc...), "--session-max-age", "31d"), "5m to 30d"},
		"sessions too short":      {append(append([]string{}, oidc...), "--session-max-age", "1m"), "5m to 30d"},
		"sessions, no OIDC":       {[]string{"--session-max-age", "1h"}, "--oidc-issuer's sessions"},
		"claims, no sign-in":      {[]string{"--user-claim", "sub"}, "read the tokens"},
		"groups header, OIDC":     {append(append([]string{}, oidc...), "--groups-header", "X-Forwarded-Groups"), "--groups-header is --user-header's"},
		"everyone admin, OIDC":    {append(append([]string{}, oidc...), "--everyone-admin"), "--admins '*'"},
		"everyone admin, a JWT":   {append(append([]string{}, iap...), "--everyone-admin"), "--admins '*'"},
		"a proxy kind unknown":    {[]string{"--proxy-jwt", "basic"}, "want google-iap, aws-alb, jwks"},
		"proxy settings, no kind": {[]string{"--proxy-jwt-audience", "x"}, "go with --proxy-jwt"},
		"IAP, no audience":        {[]string{"--proxy-jwt", "google-iap"}, "--proxy-jwt-audience"},
		"ALB, no ARN":             {[]string{"--proxy-jwt", "aws-alb"}, "--proxy-jwt-alb-arn"},
		"JWKS, no header":         {[]string{"--proxy-jwt", "jwks", "--proxy-jwt-jwks", "https://idp.example.com/keys", "--proxy-jwt-issuer", "x", "--proxy-jwt-audience", "y"}, "--proxy-jwt-header"},
		"JWKS over http":          {[]string{"--proxy-jwt", "jwks", "--proxy-jwt-header", "X-Jwt", "--proxy-jwt-jwks", "http://idp.example.com/keys", "--proxy-jwt-issuer", "x", "--proxy-jwt-audience", "y"}, "--proxy-jwt-jwks"},
		"roles, no sign-in":       {[]string{"--admins", "ana@example.com"}, "need a sign-in"},
		"groups header alone":     {[]string{"--groups-header", "X-Forwarded-Groups"}, "--groups-header needs --user-header"},
	} {
		var out, errs strings.Builder
		// Let through, it would fail to listen, not serve.
		args := append([]string{"serve", "--prometheus", "http://127.0.0.1:1", "--rules", hook, "--listen", "256.0.0.1:1", "--ledger", t.TempDir()}, c.args...)
		if code := run(args, &out, &errs); code != 2 || !strings.Contains(errs.String(), c.want) {
			t.Errorf("%s: %d, want 2 and %q\n%s", name, code, c.want, errs.String())
		}
	}
	// Given whole, each is taken, with roles and changes: it then fails
	// only to listen.
	for name, args := range map[string][]string{
		"OIDC":                  append(append([]string{}, oidc...), "--oidc-client-secret-file", secret, "--admins", "group:ops", "--changes"),
		"OIDC on this machine":  {"--oidc-issuer", "http://127.0.0.1:5556/dex", "--oidc-client-id", "x", "--external-url", "http://localhost:8080", "--editors", "*"},
		"OIDC under .localhost": {"--oidc-issuer", "http://dex.dex.svc.cluster.local:5556/dex", "--oidc-client-id", "x", "--external-url", "http://lapilli.localhost", "--user-claim", "sub", "--groups-claim", ""},
		"IAP":                   append(append([]string{}, iap...), "--admins", "ana@example.com", "--changes"),
		"an ALB":                {"--proxy-jwt", "aws-alb", "--proxy-jwt-alb-arn", "arn:aws:elasticloadbalancing:eu-west-1:123456789012:loadbalancer/app/lapilli/0123456789abcdef"},
		"JWKS":                  {"--proxy-jwt", "jwks", "--proxy-jwt-header", "Authorization", "--proxy-jwt-jwks", "https://idp.example.com/keys", "--proxy-jwt-issuer", "https://idp.example.com", "--proxy-jwt-audience", "lapilli"},
	} {
		var out, errs strings.Builder
		run(append([]string{"serve", "--prometheus", "http://127.0.0.1:1", "--rules", hook, "--listen", "256.0.0.1:1", "--ledger", t.TempDir()}, args...), &out, &errs)
		if !strings.Contains(errs.String(), "--listen") {
			t.Errorf("%s: %s", name, errs.String())
		}
	}
}
