package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/serve"
)

// depends, dependsLabels and the flags that go with them are passed in both
// modes, only when set, and refused without what they need.
func TestChartDepends(t *testing.T) {
	parallel(t)
	args := func(out string) []string {
		var got []string
		for _, l := range strings.Split(out, "\n") {
			if l = strings.TrimSpace(l); strings.HasPrefix(strings.Trim(strings.TrimPrefix(l, "- "), `"`), "--depends") {
				got = append(got, strings.Trim(strings.TrimPrefix(l, "- "), `"`))
			}
		}
		return got
	}
	out, err := helmTemplate(t, serveValues...)
	if err != nil || len(args(out)) != 0 {
		t.Fatalf("by default: %v %v", err, args(out))
	}
	vals := slices.Concat(serveValues, []string{"--set", "match={service}", "--set-string", `depends=sum by (a\, b) (x)`, "--set", "dependsLabels=a\\,b",
		"--set", "dependsMatch=service", "--set", "dependsDepth=2", "--set", "dependsDirection=both", "--set", "dependsStep=30m"})
	out, err = helmTemplate(t, vals...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := args(out)
	for _, want := range []string{"--depends=sum by (a, b) (x)", "--depends-labels=a,b", "--depends-match=service", "--depends-depth=2", "--depends-direction=both", "--depends-step=30m"} {
		if !slices.Contains(got, want) {
			t.Errorf("no %s in %v", want, got)
		}
	}
	for name, v := range map[string][]string{
		"depends needs dependsLabels": slices.Concat(serveValues, []string{"--set", "match={service}", "--set-string", "depends=x"}),
		"depends needs match":         slices.Concat(serveValues, []string{"--set-string", "depends=x", "--set", "dependsLabels=a\\,b"}),
		"go with depends":             slices.Concat(serveValues, []string{"--set", "dependsDepth=2"}),
	} {
		if out, err := helmTemplate(t, v...); err == nil || !strings.Contains(out, name) {
			t.Errorf("%s: %v\n%s", name, err, out)
		}
	}
}

// lapilli serve takes --depends and what goes with it, as the verdict
// command does, and gives serve what it starts with.
func TestServeDependsFlags(t *testing.T) {
	parallel(t)
	rules := write(t, "rules.yaml", "groups: []\n")
	base := []string{"--prometheus", "http://127.0.0.1:1", "--rules", rules, "--match", "service", "--match", "team"}
	st, err := readServe(append(slices.Clone(base), "--depends", "calls", "--depends-labels", "caller,callee", "--depends-depth", "2",
		"--depends-direction", "both", "--depends-step", "30m", "--depends-match", "team"), false)
	if err != nil {
		t.Fatal(err)
	}
	d := st.cfg.Depends
	if d.Expr != "calls" || d.From != "caller" || d.To != "callee" || d.Depth != 2 || d.Direction != "both" || d.Step != 30*time.Minute || d.Match != "team" {
		t.Errorf("%+v", d)
	}
	// By default the label is the match's first, in effect when it is read.
	st, err = readServe(append(slices.Clone(base), "--depends", "calls", "--depends-labels", "caller,callee"), false)
	if err != nil || st.cfg.Depends.Match != "" || st.cfg.Depends.Depth != 1 || st.cfg.Depends.Direction != "down" {
		t.Errorf("defaults: %+v %v", st.cfg.Depends, err)
	}
	if st, err = readServe(base, false); err != nil || st.cfg.Depends != (serve.DependsConfig{}) {
		t.Errorf("none: %+v %v", st.cfg.Depends, err)
	}
	for name, c := range map[string]struct {
		args []string
		says string
	}{
		"no labels":     {[]string{"--depends", "calls"}, "--depends needs --depends-labels"},
		"labels alone":  {[]string{"--depends-labels", "a,b"}, "goes with --depends"},
		"another match": {[]string{"--depends", "calls", "--depends-labels", "a,b", "--depends-match", "region"}, "want one of the labels --match gives"},
		"too deep":      {[]string{"--depends", "calls", "--depends-labels", "a,b", "--depends-depth", "4"}, "1 to 3"},
	} {
		if _, err := readServe(append(slices.Clone(base), c.args...), false); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v, want %q", name, err, c.says)
		}
	}
}
