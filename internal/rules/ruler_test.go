package rules

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/sse"
)

// A rule's key takes its namespace only when it has one: every rule read
// as Lapilli read it before keeps its key, byte for byte, so no ledger sees
// a new version of its rules on upgrade.
func TestKeyNamespace(t *testing.T) {
	r := Rule{Name: "Errors", Group: "api", Expr: "errors > 5", For: 2 * time.Minute, Labels: map[string]string{"severity": "page"}}
	was := fmt.Sprintf("%q %q %q %d %d %d %q %q %q %q", r.Name, r.Group, r.Expr, r.For, r.KeepFiringFor, r.Interval,
		sse.LabelKey(r.Labels), r.UID, r.Digest, r.Unsupported)
	if r.Key() != was {
		t.Errorf("a rule without a namespace: key %s, want %s", r.Key(), was)
	}
	a, b := r, r
	a.Namespace, b.Namespace = "payments", "shop"
	if a.Key() == r.Key() || a.Key() == b.Key() {
		t.Errorf("rules of one name in two namespaces have one key: %s", a.Key())
	}
	rs, err := FromDocs([]Doc{{Source: "http://mimir/rules/shop", Namespace: "shop", Data: []byte(fileA)}, {Source: "f.yaml", Data: []byte(fileA)}})
	if err != nil {
		t.Fatal(err)
	}
	if rs[0].Namespace != "shop" || rs[len(rs)-1].Namespace != "" {
		t.Errorf("namespaces: %q, %q", rs[0].Namespace, rs[len(rs)-1].Namespace)
	}
}

const fileA = `groups:
  - name: api
    rules:
      - alert: Errors
        expr: errors > 5
        for: 2m
`

// A ruler that writes a group out again in its own way, other indentation
// or quoting, gives the same tree hash.
func TestTreeHash(t *testing.T) {
	a := "name: shop\nrules:\n  - alert: A\n    expr: up == 0\n    labels: {severity: \"page\"}\n"
	b := "name: shop\nrules:\n    - alert: A\n      expr: 'up == 0'\n      labels:\n        severity: page\n"
	c := `{"name": "shop", "rules": [{"alert": "A", "expr": "up == 0", "labels": {"severity": "page"}}]}`
	ha, _ := TreeHash([]byte(a))
	hb, _ := TreeHash([]byte(b))
	hc, _ := TreeHash([]byte(c))
	if ha != hb || ha != hc {
		t.Errorf("one tree, three hashes: %s %s %s", ha, hb, hc)
	}
	hd, _ := TreeHash([]byte(strings.Replace(a, "up == 0", "up == 1", 1)))
	if hd == ha {
		t.Error("another tree, the same hash")
	}
}

func TestISODurationWritten(t *testing.T) {
	for d, want := range map[time.Duration]string{
		10 * time.Minute: "PT10M", 90 * time.Minute: "PT1H30M", 24 * time.Hour: "P1D", 26 * time.Hour: "P1DT2H",
		90 * time.Second: "PT1M30S", 1500 * time.Millisecond: "PT1.5S", 0: "PT0S",
	} {
		if got := ISODuration(d); got != want {
			t.Errorf("%s: %s, want %s", d, got, want)
		}
		if back, err := isoDuration(ISODuration(d)); err != nil || back != d {
			t.Errorf("%s reads back as %s, %v", ISODuration(d), back, err)
		}
	}
}

// azureGroup is an Azure rule group as its API answers one, written as
// Lapilli keeps it.
const azureGroup = `{
  "id": "/subscriptions/sub-1/resourceGroups/rg-shop/providers/Microsoft.AlertsManagement/prometheusRuleGroups/shop",
  "location": "koreacentral",
  "name": "shop",
  "properties": {
    "clusterName": "",
    "enabled": true,
    "interval": "PT1M",
    "rules": [
      {"alert": "Errors", "expression": "errors > 5", "for": "PT2M", "severity": 3, "labels": {"team": "shop"}, "annotations": {"summary": "errors"}},
      {"record": "job:errors:sum", "expression": "sum(errors)"},
      {"alert": "Other", "expression": "errors > 9", "for": "PT5M", "severity": 4}
    ],
    "scopes": ["/subscriptions/sub-1/resourceGroups/rg-shop/providers/microsoft.monitor/accounts/ws"]
  },
  "tags": {"team": "shop"},
  "type": "Microsoft.AlertsManagement/prometheusRuleGroups"
}`

// An Azure rule group is changed in its tree: a for as Azure writes one,
// a number in its expression, an annotation, enabled: false to turn a rule
// off; the rest as it was, its other rules and its tags included.
func TestEditTree(t *testing.T) {
	d := 10 * time.Minute
	e := Edit{Name: "Errors", Group: "shop"}
	for name, c := range map[string]struct {
		e    Edit
		want func(Rule) bool
	}{
		"for":      {Edit{For: &d}, func(r Rule) bool { return r.For == d }},
		"off":      {Edit{Pause: true}, func(r Rule) bool { return r.Unsupported == Disabled }},
		"says":     {Edit{Annotation: "runbook_url", Value: "https://wiki/x"}, func(r Rule) bool { return r.Says.Runbook == "https://wiki/x" }},
		"says out": {Edit{Annotation: "summary", Unset: true}, func(r Rule) bool { return r.Says.Summary == "" }},
		"delete":   {Edit{Delete: true}, nil},
	} {
		x := c.e
		x.Name, x.Group = e.Name, e.Group
		out, err := EditTree([]byte(azureGroup), x)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !IsAzureGroup(out) {
			t.Fatalf("%s: no longer a rule group:\n%s", name, out)
		}
		rs, err := Parse(out)
		if err != nil {
			t.Fatal(err)
		}
		var got *Rule
		for i := range rs {
			if rs[i].Name == "Errors" {
				got = &rs[i]
			}
		}
		switch {
		case c.want == nil && got != nil:
			t.Errorf("%s: still there", name)
		case c.want != nil && (got == nil || !c.want(*got)):
			t.Errorf("%s: %+v", name, got)
		}
		if !strings.Contains(string(out), `"team": "shop"`) || !strings.Contains(string(out), `"sum(errors)"`) || !strings.Contains(string(out), `"Other"`) {
			t.Errorf("%s: the rest of the group changed:\n%s", name, out)
		}
	}
	if out, _ := EditTree([]byte(azureGroup), Edit{Name: "Errors", Group: "shop", For: &d}); !strings.Contains(string(out), `"for": "PT10M"`) {
		t.Errorf("a for written so:\n%s", out)
	}
	// A threshold, as WithBound writes it.
	rs, _ := Parse([]byte(azureGroup))
	b := rs[0].Bounds()[0]
	out, err := EditTreeThreshold([]byte(azureGroup), rs[0], e, b, 7)
	if err != nil || !strings.Contains(string(out), `"errors > 7"`) || !strings.Contains(string(out), `"errors > 9"`) {
		t.Errorf("threshold: %v\n%s", err, out)
	}
	// A rule group's last rule isn't deleted: a person deletes the group.
	one := `{"name": "solo", "type": "Microsoft.AlertsManagement/prometheusRuleGroups", "properties": {"scopes": ["x"], "rules": [{"alert": "Solo", "expression": "up == 0"}]}}`
	if _, err := EditTree([]byte(one), Edit{Name: "Solo", Group: "solo", Delete: true}); !errors.Is(err, ErrNotEditable) {
		t.Errorf("the last rule deleted: %v", err)
	}
	if _, err := EditTree([]byte(azureGroup), Edit{Name: "Errors", Group: "other", For: &d}); !errors.Is(err, ErrNotEditable) {
		t.Errorf("another group's rule: %v", err)
	}
}

// Google Cloud's Rules resources go by the git path: the edit finds
// spec.groups in one, and writes a new for and a new number there, its
// comment and the rest of the file as they were.
func TestEditGoogleRules(t *testing.T) {
	file := `apiVersion: monitoring.googleapis.com/v1
kind: Rules
metadata:
  name: shop
  namespace: shop
spec:
  groups:
    - name: shop
      interval: 30s
      rules:
        # Pages the shop's on-call.
        - alert: Errors
          expr: errors{job="shop"} > 5
          for: 2m
`
	d := 10 * time.Minute
	out, err := EditDoc([]byte(file), Edit{Name: "Errors", Group: "shop", For: &d})
	if err != nil || string(out) != strings.Replace(file, "for: 2m", "for: 10m", 1) {
		t.Fatalf("a for: %v\n%s", err, out)
	}
	rs, err := Parse([]byte(file))
	if err != nil {
		t.Fatal(err)
	}
	out, err = EditThreshold([]byte(file), rs[0], Edit{Name: "Errors", Group: "shop"}, rs[0].Bounds()[0], 7)
	if err != nil || string(out) != strings.Replace(file, "> 5", "> 7", 1) {
		t.Fatalf("a threshold: %v\n%s", err, out)
	}
}

// A rule is put back as it was kept: in place of the rule as it is now,
// or back where it was among the rules as they are now; every other rule,
// changed since or not, as it is now.
func TestPutBack(t *testing.T) {
	was := `groups:
  - name: shop
    rules:
      - alert: A
        expr: a > 1
        for: 1m
      - alert: B
        expr: b > 1
        for: 2m
      - record: r
        expr: sum(c)
  - name: edge
    rules:
      - alert: E
        expr: e > 1
`
	// B was changed; A changed since by someone else.
	now := strings.Replace(strings.Replace(was, "for: 2m", "for: 10m", 1), "a > 1", "a > 2", 1)
	out, err := PutBack([]byte(now), []byte(was), Edit{Name: "B", Group: "shop"})
	if err != nil || string(out) != strings.Replace(was, "a > 1", "a > 2", 1) {
		t.Fatalf("a change undone: %v\n%s", err, out)
	}
	// B was deleted, and A changed since: back between A and the recording
	// rule.
	gone := strings.Replace(strings.Replace(was, "      - alert: B\n        expr: b > 1\n        for: 2m\n", "", 1), "a > 1", "a > 2", 1)
	out, err = PutBack([]byte(gone), []byte(was), Edit{Name: "B", Group: "shop"})
	if err != nil || string(out) != strings.Replace(was, "a > 1", "a > 2", 1) {
		t.Fatalf("a deletion undone: %v\n%s", err, out)
	}
	// The first rule deleted goes back before the others.
	noA := strings.Replace(was, "      - alert: A\n        expr: a > 1\n        for: 1m\n", "", 1)
	if out, err = PutBack([]byte(noA), []byte(was), Edit{Name: "A", Group: "shop"}); err != nil || string(out) != was {
		t.Fatalf("the first rule: %v\n%s", err, out)
	}
	// A group that went with its last rule is put back, after the group
	// before it.
	noEdge := strings.SplitAfter(was, "  - name: edge\n")[0]
	noEdge = strings.TrimSuffix(noEdge, "  - name: edge\n")
	if out, err = PutBack([]byte(noEdge), []byte(was), Edit{Name: "E", Group: "edge"}); err != nil || string(out) != was {
		t.Fatalf("a group: %v\n%s", err, out)
	}
	// A ruler's group that went: the file of that one group, as it was.
	solo := "groups:\n  - name: solo\n    rules:\n        - alert: S\n          expr: up == 0\n"
	if out, err = PutBack([]byte("groups: []\n"), []byte(solo), Edit{Name: "S", Group: "solo"}); err != nil || string(out) != solo {
		t.Fatalf("a ruler's group: %v\n%s", err, out)
	}
	// Indented otherwise now, as a ruler writes it out: indented as now.
	now4 := "groups:\n    - name: shop\n      rules:\n          - alert: A\n            expr: a > 1\n            for: 1m\n"
	was2 := "groups:\n  - name: shop\n    rules:\n      - alert: A\n        expr: a > 1\n        for: 1m\n      - alert: B\n        expr: b > 1\n"
	if out, err = PutBack([]byte(now4), []byte(was2), Edit{Name: "B", Group: "shop"}); err != nil ||
		string(out) != now4+"          - alert: B\n            expr: b > 1\n" {
		t.Fatalf("indented otherwise: %v\n%s", err, out)
	}
	// Not there in what was kept: nothing is put back.
	if _, err := PutBack([]byte(now), []byte(was), Edit{Name: "Z", Group: "shop"}); !errors.Is(err, ErrNotEditable) {
		t.Errorf("a rule never kept: %v", err)
	}
}

// An Azure rule group's rule is put back in its tree, the other rules as
// they are now.
func TestPutBackTree(t *testing.T) {
	d := 10 * time.Minute
	changed, _ := EditTree([]byte(azureGroup), Edit{Name: "Errors", Group: "shop", For: &d})
	other := strings.Replace(string(changed), `"errors > 9"`, `"errors > 11"`, 1)
	out, err := PutBack([]byte(other), []byte(azureGroup), Edit{Name: "Errors", Group: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	rs, _ := Parse(out)
	if rs[0].For != 2*time.Minute || rs[1].Expr != "errors > 11" {
		t.Errorf("put back: %+v", rs)
	}
	deleted, _ := EditTree([]byte(azureGroup), Edit{Name: "Errors", Group: "shop", Delete: true})
	out, err = PutBack(deleted, []byte(azureGroup), Edit{Name: "Errors", Group: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	canon, _ := CanonJSON([]byte(azureGroup))
	if string(out) != string(canon) {
		t.Errorf("a deletion undone:\n%s\nwant\n%s", out, canon)
	}
}

// Two units of one shape hold the same rules, however the ruler writes
// them out: 90s and 1m30s are one. A recording rule changed is another
// shape.
func TestShape(t *testing.T) {
	a := "groups:\n  - name: g\n    rules:\n      - alert: A\n        expr: a > 1\n        for: 90s\n      - record: r\n        expr: sum(c)\n"
	b := "groups:\n    - name: g\n      rules:\n        - alert: A\n          expr: a > 1\n          for: 1m30s\n        - record: r\n          expr: sum(c)\n"
	sa, err1 := Shape([]byte(a), nil)
	sb, err2 := Shape([]byte(b), nil)
	if err1 != nil || err2 != nil || sa != sb {
		t.Errorf("one shape, written two ways: %v %v\n%s\n%s", err1, err2, sa, sb)
	}
	sc, _ := Shape([]byte(strings.Replace(a, "sum(c)", "sum(d)", 1)), nil)
	sd, _ := Shape([]byte(strings.Replace(a, "for: 90s", "for: 2m", 1)), nil)
	if sc == sa || sd == sa {
		t.Error("another rule, the same shape")
	}
	// What Azure sets itself isn't the shape.
	x, _ := Shape([]byte(azureGroup), []string{"id", "name", "type", "systemData"})
	y, _ := Shape([]byte(strings.Replace(azureGroup, `"location"`, `"systemData": {"createdBy": "x"}, "location"`, 1)), []string{"id", "name", "type", "systemData"})
	if x != y {
		t.Error("what Azure sets changed the shape")
	}
}

// A deletion put back in a group that went since: the group comes back
// holding that one rule, its other rules and their comments left out; and
// before the first group still there, above that group's own comment.
func TestPutBackOneRule(t *testing.T) {
	was := `groups:
  - name: shop
    interval: 1m
    rules:
      # A's.
      - alert: A
        expr: a > 1
      - record: r
        expr: sum(c)
      - alert: B
        expr: b > 1
  # The edge's.
  - name: edge
    rules:
      - alert: E
        expr: e > 1
`
	now := "groups:\n  # The edge's.\n  - name: edge\n    rules:\n      - alert: E\n        expr: e > 1\n"
	out, err := PutBack([]byte(now), []byte(was), Edit{Name: "B", Group: "shop"})
	want := "groups:\n  - name: shop\n    interval: 1m\n    rules:\n      - alert: B\n        expr: b > 1\n" + strings.TrimPrefix(now, "groups:\n")
	if err != nil || string(out) != want {
		t.Fatalf("put back: %v\n%s", err, out)
	}
	if !Adds([]byte(now), out, Edit{Name: "B", Group: "shop"}) {
		t.Error("not the one rule added")
	}
	// A ruler's group that went: a file of that group, holding that rule.
	shop, _, _ := strings.Cut(was, "  # The edge's.\n")
	out, err = PutBack([]byte("groups: []\n"), []byte(shop), Edit{Name: "A", Group: "shop"})
	if err != nil || string(out) != "groups:\n  - name: shop\n    interval: 1m\n    rules:\n      # A's.\n      - alert: A\n        expr: a > 1\n" {
		t.Fatalf("a ruler's group: %v\n%s", err, out)
	}
}

// Adds: a unit put back with the one rule and nothing else; one with
// another rule changed, or another added, isn't.
func TestAdds(t *testing.T) {
	now := "groups:\n  - name: g\n    rules:\n      - alert: A\n        expr: a > 1\n"
	e := Edit{Name: "B", Group: "g"}
	for out, ok := range map[string]bool{
		now + "      - alert: B\n        expr: b > 1\n":                                        true,
		strings.Replace(now, "a > 1", "a > 2", 1) + "      - alert: B\n        expr: b > 1\n":  false,
		now + "      - alert: B\n        expr: b > 1\n      - alert: C\n        expr: c > 1\n": false,
		now + "  - name: h\n    rules:\n      - alert: B\n        expr: b > 1\n":               false,
	} {
		if Adds([]byte(now), []byte(out), e) != ok {
			t.Errorf("%s: want %v", out, ok)
		}
	}
	// In place of the rule as it is: alike but for it.
	if !Adds([]byte(now+"      - alert: B\n        expr: b > 2\n"), []byte(now+"      - alert: B\n        expr: b > 1\n"), e) {
		t.Error("a rule put back in place of itself")
	}
}

// A rule's whole entry: its durations as lengths of time, every other key
// as it is; one a unit holds twice, or not at all, is none.
func TestEntry(t *testing.T) {
	e := Edit{Name: "A", Group: "g"}
	a, ok1 := Entry([]byte("groups:\n  - name: g\n    rules:\n      - alert: A\n        expr: a > 1\n        for: 90s\n"), e)
	b, ok2 := Entry([]byte("groups:\n    - name: g\n      rules:\n        - alert: A\n          expr: 'a > 1'\n          for: 1m30s\n"), e)
	if !ok1 || !ok2 || !reflect.DeepEqual(a, b) {
		t.Errorf("one rule written two ways: %v %v", a, b)
	}
	c, _ := Entry([]byte("groups:\n  - name: g\n    rules:\n      - alert: A\n        expr: a > 1\n        for: 90s\n        annotations: {team: x}\n"), e)
	if reflect.DeepEqual(a, c) {
		t.Error("an annotation no check reads isn't in the entry")
	}
	if _, ok := Entry([]byte("groups:\n  - name: g\n    rules:\n      - alert: A\n        expr: a\n      - alert: A\n        expr: b\n"), e); ok {
		t.Error("one of two alike taken")
	}
	x, ok3 := Entry([]byte(azureGroup), Edit{Name: "Errors", Group: "shop"})
	if !ok3 || x["for"] != int64(2*time.Minute) {
		t.Errorf("an Azure rule: %v", x)
	}
}

// What a unit holds is every key of each alert rule: an Azure rule given
// an action group, or a rule an annotation no check reads, is another
// shape.
func TestShapeWholeEntries(t *testing.T) {
	sets := []string{"id", "name", "type", "systemData"}
	a, _ := Shape([]byte(azureGroup), sets)
	b, _ := Shape([]byte(strings.Replace(azureGroup, `"severity": 3,`, `"severity": 3, "actions": [{"actionGroupId": "oncall"}],`, 1)), sets)
	if a == b {
		t.Error("an action group is the same shape")
	}
	f := "groups:\n  - name: g\n    rules:\n      - alert: A\n        expr: a > 1\n"
	c, _ := Shape([]byte(f), nil)
	d, _ := Shape([]byte(f+"        annotations: {team: x}\n"), nil)
	if c == d {
		t.Error("an annotation is the same shape")
	}
}

// Adds takes one rule of its name in, never two.
func TestAddsOne(t *testing.T) {
	now := "groups:\n  - name: g\n    rules:\n      - alert: A\n        expr: a > 1\n"
	two := now + "      - alert: B\n        expr: b > 1\n      - alert: B\n        expr: b > 2\n"
	if Adds([]byte(now), []byte(two), Edit{Name: "B", Group: "g"}) {
		t.Error("two rules of one name put back for one")
	}
}

// slow stretches the time budgets under the race detector (race_test.go)
// and on CI runners.
var slow = 1.0

func init() {
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		slow = max(slow, 2)
	}
}

// A deletion put back in a group of thousands of rules that went since is
// worked out at once, not once a rule: a unit of up to 4 MB is read once.
func TestPutBackOneRuleOfMany(t *testing.T) {
	for _, c := range []struct {
		n   int
		pad string
	}{{10000, ""}, {2000, strings.Repeat("x", 400)}} {
		var b strings.Builder
		b.WriteString("groups:\n  - name: g\n    rules:\n")
		for i := range c.n {
			fmt.Fprintf(&b, "      # A%d's.\n      - alert: A%d\n        expr: up == %d\n", i, i, i)
			if c.pad != "" {
				fmt.Fprintf(&b, "        annotations:\n          description: %s\n", c.pad)
			}
		}
		b.WriteString("      - alert: Target\n        expr: up == 0\n")
		now := "groups:\n  - name: other\n    rules:\n      - alert: O\n        expr: up == 1\n"
		start := time.Now()
		out, err := PutBack([]byte(now), []byte(b.String()), Edit{Name: "Target", Group: "g"})
		took := time.Since(start)
		want := "groups:\n  - name: g\n    rules:\n      - alert: Target\n        expr: up == 0\n  - name: other\n    rules:\n      - alert: O\n        expr: up == 1\n"
		if err != nil || string(out) != want {
			t.Fatalf("%d rules: %v\n%.300s", c.n, err, out)
		}
		if budget := time.Duration(float64(3*time.Second) * slow); took > budget {
			t.Errorf("%d rules, %d KB: put back in %v; budget %v", c.n, b.Len()>>10, took, budget)
		}
	}
}
