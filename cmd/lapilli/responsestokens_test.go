package main

import (
	"io"
	"strings"
	"testing"
)

// --responses-token-file is SOURCE=FILE, once for each source: a token writes
// its source's pages; a source named twice, the file's own, a bad name, one
// token for two sources, and a token another port's are refused. A token
// alone is a responses input, so --noisy-by useful and --routine go with it,
// and rules are found noisy by useful by default.
func TestServeResponsesTokensFlags(t *testing.T) {
	parallel(t)
	rules := write(t, "rules.yaml", "groups: []\n")
	pd := write(t, "pd", "a-pagerduty-token-long-enough")
	ops := write(t, "ops", "an-ops-token-long-enough-too")
	base := []string{"--prometheus", "http://127.0.0.1:1", "--rules", rules, "--jobs-listen", ":8082"}
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"--responses-token-file", pd}, "SOURCE=FILE"},
		{[]string{"--responses-token-file", "=" + pd}, "lowercase letters"},
		{[]string{"--responses-token-file", "PD=" + pd}, "lowercase letters"},
		{[]string{"--responses-token-file", "file=" + pd}, "owns its pages"},
		{[]string{"--responses-token-file", "pd=" + pd, "--responses-token-file", "pd=" + ops}, "given twice"},
		{[]string{"--responses-token-file", "pd=" + pd, "--responses-token-file", "ops=" + write(t, "same", "a-pagerduty-token-long-enough\n")}, "hold the same token"},
		{[]string{"--responses-token-file", "pd=" + pd, "--incidents-token-file", write(t, "inc", "a-pagerduty-token-long-enough\n")}, "the same token"},
		{[]string{"--responses-token-file", "pd=" + write(t, "short", "short")}, "16 characters"},
	} {
		var errs strings.Builder
		if code := run(append([]string{"serve"}, append(slicesClone(base), c.args...)...), io.Discard, &errs); code != 2 || !strings.Contains(errs.String(), c.says) {
			t.Errorf("%v: exit %d, want 2 saying %q:\n%s", c.args, code, c.says, errs.String())
		}
	}
	args := append(slicesClone(base), "--responses-token-file", "pd="+pd, "--responses-token-file", "ops="+ops)
	st, err := readServe(args, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.cfg.ResponsesTokens; len(got) != 2 || got["pd"] != pd || got["ops"] != ops {
		t.Errorf("tokens: %v", got)
	}
	// Pages by PUT alone are a responses input.
	if got := st.cfg.Options.NoisyBy; got != "useful" {
		t.Errorf("noisy by %q with a responses token, want useful", got)
	}
	for _, extra := range [][]string{{"--noisy-by", "useful"}, {"--routine", "0.7"}} {
		if _, err := readServe(append(slicesClone(args), extra...), false); err != nil {
			t.Errorf("%v with a responses token: %v", extra, err)
		}
	}
}

func slicesClone(s []string) []string { return append([]string(nil), s...) }
