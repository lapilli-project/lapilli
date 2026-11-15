package rules

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// groupNamed is a group named name on n alert rules.
func groupNamed(name string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "groups:\n- name: %q\n  rules:\n", name)
	for i := range n {
		fmt.Fprintf(&b, "  - {alert: A%d, expr: x}\n", i)
	}
	return b.String()
}

// TestGroupNameCharged: a group's name is on each of its rules, in its key,
// on its pages and in what a replay carries alerts over by, and is charged
// so, as its labels are, in a rule file, a Grafana's, a rules API's answer
// and a Google policy's conditions; its name is shown once for the group,
// its rules sharing it. Before: a name of 1 MiB with a direction mark on
// 1,000 rules kept 1.07 GB, as each rule had its own copy to be shown, and
// one pass of their keys made more than 2.2 GB; a Grafana group's name of
// 1 MiB on 300 rules made 964 MB, as each rule's fingerprint printed it.
func TestGroupNameCharged(t *testing.T) {
	big := strings.Repeat("g", 1<<20)
	var grafana strings.Builder
	fmt.Fprintf(&grafana, "apiVersion: 1\ngroups:\n- orgId: 1\n  name: %s\n  folder: f\n  interval: 1m\n  rules:\n", big)
	for i := range 300 {
		fmt.Fprintf(&grafana, "  - {uid: u%d, title: t%d, condition: A, data: [{refId: A, datasourceUid: d, model: {expr: x}}]}\n", i, i)
	}
	var api strings.Builder
	fmt.Fprintf(&api, `{"status":"success","data":{"groups":[{"name":%q,"rules":[`, big)
	for i := range 1000 {
		if i > 0 {
			api.WriteString(",")
		}
		fmt.Fprintf(&api, `{"type":"alerting","name":"A%d","query":"x"}`, i)
	}
	api.WriteString("]}]}}")
	var policy strings.Builder
	fmt.Fprintf(&policy, `{"displayName": %q, "conditions": [`, big[:256<<10])
	for i := range 100 {
		if i > 0 {
			policy.WriteString(",")
		}
		fmt.Fprintf(&policy, `{"displayName": "c%d", "conditionThreshold": {}}`, i)
	}
	policy.WriteString("]}")
	for _, c := range []struct {
		what string
		read func() error
	}{
		{"1 MiB with a direction mark, 1,000 rules", func() error { _, err := Parse([]byte(groupNamed(big+"\u200e", 1000))); return err }},
		{"1 MiB, 1,000 rules", func() error { _, err := Parse([]byte(groupNamed(big, 1000))); return err }},
		{"a Grafana group of 1 MiB, 300 rules", func() error { _, err := Parse([]byte(grafana.String())); return err }},
		{"a rules API's group of 1 MiB, 1,000 rules", func() error { _, err := FromAPI([]byte(api.String())); return err }},
		{"a Google policy's name of 256 KiB, 100 conditions", func() error { _, err := Parse([]byte(policy.String())); return err }},
	} {
		var err error
		bytes, took := allocated(func() { err = c.read() })
		t.Logf("%s: %d MB allocated, %v: %.160v", c.what, bytes>>20, took, err)
		var made *MadeError
		if !errors.As(err, &made) || !strings.Contains(err.Error(), "name") {
			t.Errorf("%s: %v", c.what, err)
		}
		if bytes > 64<<20 {
			t.Errorf("%s: %d MB allocated", c.what, bytes>>20)
		}
	}

	// Within the bound, the rules share their group's name as shown.
	rs, err := Parse([]byte(groupNamed(strings.Repeat("g", 1<<10)+"\u200e", 1000)))
	if err != nil || len(rs) != 1000 || !strings.HasSuffix(rs[0].Group, "\ufffd") {
		t.Fatalf("%d, %v", len(rs), err)
	}
	for i, r := range rs {
		if !sameString(r.Group, rs[0].Group) {
			t.Fatalf("rule %d has a copy of its group's name of its own", i)
		}
	}
	// A Grafana rule's fingerprint tells its group and folder apart still.
	finger := func(group, folder string) string {
		rs, err := Parse(fmt.Appendf(nil, "apiVersion: 1\ngroups:\n- orgId: 1\n  name: %s\n  folder: %s\n  interval: 1m\n  rules:\n"+
			"  - {uid: u, title: t, condition: A, data: [{refId: A, datasourceUid: d, model: {expr: x}}]}\n", group, folder))
		if err != nil || len(rs) != 1 {
			t.Fatalf("%v", err)
		}
		return rs[0].Fingerprint
	}
	if a := finger("g", "f"); a == finger("h", "f") || a == finger("g", "e") || a != finger("g", "f") {
		t.Error("a fingerprint doesn't tell a group and a folder apart")
	}
}
