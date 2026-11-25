package main

import (
	"testing"

	"lapilli.dev/internal/content"
	"lapilli.dev/internal/rules"
)

// --changed-only takes in the rules left alone that a side's own routing
// needs: the before side's, with --alertmanager-config-before, not the
// after's.
func TestLeftAlonePerSide(t *testing.T) {
	const inhibits = `route: {receiver: r, group_by: [alertname]}
receivers: [{name: r, slack_configs: [{channel: '#x'}]}]
inhibit_rules:
  - source_matchers: ['alertname="NodeDown"']
    target_matchers: ['severity="page"']
    equal: [node]
`
	const plain = `route: {receiver: r, group_by: [alertname]}
receivers: [{name: r, slack_configs: [{channel: '#x'}]}]
`
	read := func(text string) *notifier {
		c, err := content.ReadAlertmanagerRouting([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		return &notifier{cfg: c}
	}
	after, before := read(plain), read(inhibits)
	alike := []rules.Rule{{Name: "NodeDown"}, {Name: "Other"}}
	if got := before.leftAlone(alike); len(got) != 1 || got[0].Name != "NodeDown" {
		t.Errorf("the before side, whose config has the inhibition: %+v", got)
	}
	if got := after.leftAlone(alike); len(got) != 0 {
		t.Errorf("the after side, whose config has none: %+v", got)
	}
	both := unionRules(before.leftAlone(alike), after.leftAlone(alike))
	if len(both) != 1 {
		t.Fatalf("union: %+v", both)
	}
	fired, ran := ofRules(nil, both, after.leftAlone(alike))
	if len(fired) != 0 || len(ran) != 0 {
		t.Errorf("the after side is given what only the before side needs: %+v", ran)
	}
}
