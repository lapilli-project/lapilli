package main

import (
	"strings"
	"testing"
)

// Changes are refused, before lapilli serve starts, without what makes
// approval a known person's, or with what would make two tokens one.
func TestServeChangesFlags(t *testing.T) {
	hook := write(t, "hook", "https://hooks.example/x")
	tok := write(t, "tok", "changes-token-0123456789\n")
	other := write(t, "other", "changes-token-0123456789\n")
	short := write(t, "short", "x\n")
	signed := []string{"--user-header", "X-Forwarded-Email", "--admins", "ana@example.com"}
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"no sign-in":            {[]string{"--changes"}, "--changes needs --user-header"},
		"no one to approve":     {[]string{"--changes", "--user-header", "X-Forwarded-Email"}, "--changes needs --approvers or --admins"},
		"read-only":             {append([]string{"--changes", "--read-only"}, signed...), "--read-only refuses them"},
		"three approvals":       {append([]string{"--changes", "--approvals", "3"}, signed...), "--approvals 3"},
		"approvers, no changes": {append([]string{"--approvers", "ana@example.com"}, signed...), "go with --changes"},
		"bad limits":            {append([]string{"--changes", "--changes-limits", "5"}, signed...), "--changes-limits"},
		"a short token":         {append([]string{"--changes", "--incidents-listen", "127.0.0.1:0", "--changes-token-file", short}, signed...), "16 characters"},
		"the token twice":       {append([]string{"--changes", "--incidents-listen", "127.0.0.1:0", "--changes-token-file", tok, "--incidents-token-file", other}, signed...), "same token"},
		"no port for it":        {append([]string{"--changes", "--changes-token-file", tok}, signed...), "go with --incidents-listen"},
		"Grafana's, no Grafana": {append([]string{"--changes", "--changes-grafana-token-file", tok}, signed...), "--grafana"},
		"an agent, no roles":    {[]string{"--mcp-propose", "editors"}, "--mcp-propose needs"},
		"an agent, everyone":    {[]string{"--mcp-propose", "everyone"}, "want editors or admins"},
	} {
		var out, errs strings.Builder
		if code := run(append([]string{"serve", "--prometheus", "http://127.0.0.1:1", "--rules", hook}, c.args...), &out, &errs); code != 2 || !strings.Contains(errs.String(), c.want) {
			t.Errorf("%s: %d, want 2 and %q\n%s", name, code, c.want, errs.String())
		}
	}
}
