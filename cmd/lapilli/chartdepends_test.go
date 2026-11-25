package main

import (
	"slices"
	"strings"
	"testing"
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
