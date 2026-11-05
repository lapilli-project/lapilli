package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"lapilli.dev/internal/rules"
)

// signedIn adds the sign-in proxy's header to each request.
type signedIn string

func (a signedIn) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Forwarded-Email", string(a))
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, url, who string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: url + "/mcp",
		HTTPClient: &http.Client{Transport: signedIn(who)}, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// call calls a tool and reads what it gives back into out; it fails the
// test on a protocol error, and returns whether the tool said it failed.
func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any, out any) (bool, string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var text string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	if res.IsError {
		return true, text
	}
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("%s: %v\n%s", tool, err, b)
	}
	return false, text
}

// An agent reads what the pages show, and an editor's agent replays a
// change it proposes; a page elsewhere can't reach it.
func TestMCP(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, func(c *Config) {
		c.UserHeader = "X-Forwarded-Email"
		c.Editors = []string{"ed@example.com"}
		c.MCPCompare = "editors"
	})
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	look, edit := connect(t, ts.URL, "viewer@example.com"), connect(t, ts.URL, "ed@example.com")
	tools := func(cs *mcp.ClientSession) []string {
		res, err := cs.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, x := range res.Tools {
			names = append(names, x.Name)
			if x.Annotations == nil || !x.Annotations.ReadOnlyHint {
				t.Errorf("%s should say it only reads", x.Name)
			}
		}
		slices.Sort(names)
		return names
	}
	if got := strings.Join(tools(look), " "); got != "firings incidents outcomes overview rule rules" {
		t.Errorf("a viewer's tools: %s", got)
	}
	if got := strings.Join(tools(edit), " "); got != "compare firings incidents outcomes overview rule rules" {
		t.Errorf("an editor's tools: %s", got)
	}
	if ir := edit.InitializeResult(); ir.ServerInfo.Name != "lapilli" || !strings.Contains(ir.Instructions, "white when it caught an incident") {
		t.Errorf("initialize: %+v", ir)
	}

	var ov overviewOut
	if failed, text := call(t, look, "overview", nil, &ov); failed || ov.Empty || len(ov.Weeks) == 0 || ov.Weeks[0].Firings == 0 {
		t.Errorf("overview: %s %+v", text, ov)
	}
	var rs rulesOut
	if failed, text := call(t, look, "rules", nil, &rs); failed || len(rs.Rules) != 1 || rs.Rules[0].Name != "Errors" || rs.Rules[0].Firings != 21 || rs.Weeks != 4 {
		t.Errorf("rules: %s %+v", text, rs)
	}
	if failed, text := call(t, look, "rules", map[string]any{"weeks": 2}, &rs); !failed || !strings.Contains(text, "1, 4 or 12") {
		t.Errorf("rules over two weeks should be refused: %s", text)
	}
	if failed, text := call(t, look, "rules", map[string]any{"team": "nobody"}, &rs); !failed || !strings.Contains(text, `no team called "nobody"`) {
		t.Errorf("an unknown team should be refused: %s", text)
	}
	var r ruleDetailOut
	if failed, text := call(t, look, "rule", map[string]any{"name": "Errors"}, &r); failed || r.Definition == nil ||
		r.Definition.Expr != "errors > 5" || r.Definition.For != "2m" || r.Definition.Labels["severity"] != "page" ||
		len(r.Weeks) != weeks || len(r.Caught) != 1 || len(r.Latest) == 0 {
		t.Errorf("rule: %s %+v", text, r)
	}
	if failed, text := call(t, look, "rule", map[string]any{"name": "Nope"}, &r); !failed || !strings.Contains(text, `no rule called "Nope"`) {
		t.Errorf("an unknown rule: %s", text)
	}
	var is incidentsOut
	if failed, text := call(t, look, "incidents", nil, &is); failed || !slices.ContainsFunc(is.Incidents, func(x incidentOut) bool {
		return x.ID == "INC-1" && x.Status == "caught" && len(x.CaughtBy) == 1 && x.CaughtBy[0].Rule == "Errors"
	}) || !slices.Equal(is.Match, []string{"service"}) {
		t.Errorf("incidents: %s %+v", text, is)
	}
	var fs firingsOut
	if failed, text := call(t, look, "firings", map[string]any{"days": 3, "rule": "Errors", "black_only": true, "limit": 5}, &fs); failed ||
		len(fs.Firings) != 5 || !fs.More || fs.Firings[0].Stone != "black" || fs.Firings[0].Labels["service"] != "search" {
		t.Errorf("firings: %s %+v", text, fs)
	}
	var out outcomes
	if failed, text := call(t, look, "outcomes", nil, &out); failed || len(out.Record.Weeks) != weeks {
		t.Errorf("outcomes: %s %+v", text, out)
	}
	if _, err := look.CallTool(ctx, &mcp.CallToolParams{Name: "compare", Arguments: map[string]any{"rules": "x"}}); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("a viewer has no compare: %v", err)
	}

	// The rule with a longer "for", replayed over the week before: as the
	// review of the same change says, 21 firings become 1, and the
	// incident is still caught.
	longer := `groups:
  - name: shop
    rules:
      - alert: Errors
        expr: errors > 5
        for: 15m
        labels: {severity: page}
`
	compare := func(args map[string]any) (bool, string, compareOut) {
		var cmp compareOut
		failed, text := call(t, edit, "compare", args, &cmp)
		return failed, text, cmp
	}
	// INC-1 is still caught, a few minutes later; the impact window is
	// judged against too.
	if failed, text, cmp := compare(map[string]any{"rules": longer}); failed || len(cmp.Rules) != 1 ||
		cmp.Rules[0].Before.Firings != 21 || cmp.Rules[0].After.Firings != 1 || cmp.Rules[0].After.White != 1 ||
		len(cmp.Incidents) != 1 || cmp.Incidents[0].ID != "INC-1" || len(cmp.Incidents[0].After) != 1 ||
		cmp.Incidents[0].After[0].Delay <= cmp.Incidents[0].Before[0].Delay || cmp.Judged != 2 || len(cmp.Unfinished) != 0 {
		t.Errorf("compare: %s %+v", text, cmp)
	}
	if failed, text, cmp := compare(map[string]any{"rules": strings.Replace(longer, "15m", "2m", 1)}); failed ||
		len(cmp.Rules) != 0 || cmp.Same != 1 || !strings.Contains(cmp.Note, "nothing to replay") {
		t.Errorf("a proposal like the rules in effect: %s %+v", text, cmp)
	}
	if failed, text, cmp := compare(map[string]any{"remove": []string{"Errors"}}); failed || len(cmp.Rules) != 1 ||
		cmp.Rules[0].After != nil || len(cmp.Incidents) != 1 || len(cmp.Incidents[0].After) != 0 {
		t.Errorf("taking Errors away: %s %+v", text, cmp)
	}
	added := strings.Replace(longer, "alert: Errors", "alert: MoreErrors", 1)
	if failed, text, cmp := compare(map[string]any{"rules": added}); failed || len(cmp.Rules) != 1 ||
		cmp.Rules[0].Rule != "MoreErrors" || cmp.Rules[0].Before != nil || cmp.Rules[0].After == nil || cmp.Note != "" {
		t.Errorf("adding a rule: %s %+v", text, cmp)
	}
	// What can't be compared, or would cost too much, is refused: among
	// them a Grafana expression that refers back to itself, which once
	// recursed until the stack ran out.
	loop := `apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    rules:
      - uid: c1
        title: Loop
        condition: B
        data:
          - {refId: A, datasourceUid: prom, model: {expr: 'vector(1)', instant: true}}
          - {refId: B, datasourceUid: __expr__, model: {type: math, expression: '$A + $B'}}
`
	var many strings.Builder
	many.WriteString("groups:\n  - name: shop\n    rules:\n")
	for i := range compareGiven + 1 {
		fmt.Fprintf(&many, "      - alert: R%d\n        expr: errors > 5\n", i)
	}
	for _, bad := range []map[string]any{{"rules": "groups: [{"}, {}, {"remove": []string{"Nope"}}, {"rules": longer, "days": 30},
		{"rules": strings.Replace(longer, "15m", "3d", 1)}, {"rules": loop}, {"rules": many.String()}} {
		if failed, text, _ := compare(bad); !failed {
			t.Errorf("%v should be refused: %s", bad, text)
		}
	}
	// Without --mcp-compare, no one gets it.
	off, _ := server(t, func(c *Config) { c.UserHeader = "X-Forwarded-Email"; c.Editors = []string{"ed@example.com"} })
	ts2 := httptest.NewServer(off.Handler())
	defer ts2.Close()
	if got := strings.Join(tools(connect(t, ts2.URL, "ed@example.com")), " "); got != "firings incidents outcomes overview rule rules" {
		t.Errorf("without --mcp-compare, an editor's tools: %s", got)
	}

	// A browser can't be made to ask, from a page elsewhere or a name
	// pointed here.
	for _, origin := range []string{"https://evil.example", ts.URL, "null"} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Origin", origin)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("Origin %s: %d", origin, res.StatusCode)
		}
	}
}

// A rule given takes the place of the rule in effect with its name and
// group, picked by its labels among rules told apart by them; any other is
// added, told apart from those in effect.
func TestProposed(t *testing.T) {
	show := func(rs []rules.Rule) string {
		var have []string
		for _, r := range rs {
			have = append(have, r.Name+"="+r.Expr)
		}
		return strings.Join(have, " ")
	}
	current := []rules.Rule{{Name: "A [g1]", Group: "g1", Expr: "a"}, {Name: "A [g2]", Group: "g2", Expr: "a"}, {Name: "B", Group: "g1", Expr: "b"}}
	got, err := proposed(current, []rules.Rule{{Name: "A", Group: "g2", Expr: "a2"}, {Name: "B", Group: "other", Expr: "b2"}, {Name: "C", Expr: "c"}}, []string{"A [g1]"})
	if err != nil || show(got) != "A [g2]=a2 B=b B [other]=b2 C=c" {
		t.Errorf("proposed: %s %v", show(got), err)
	}
	// Twins told apart by a label: the label given picks one; taking one
	// away and giving it again adds it, and leaves the other be.
	twins := []rules.Rule{{Name: "A [severity=page]", Group: "g1", Labels: map[string]string{"severity": "page"}, Expr: "p"},
		{Name: "A [severity=info]", Group: "g1", Labels: map[string]string{"severity": "info"}, Expr: "i"}}
	page := rules.Rule{Name: "A", Group: "g1", Labels: map[string]string{"severity": "page"}, Expr: "p2"}
	if got, err := proposed(twins, []rules.Rule{page}, nil); err != nil || show(got) != "A [severity=page]=p2 A [severity=info]=i" {
		t.Errorf("one twin: %s %v", show(got), err)
	}
	if got, err := proposed(twins, []rules.Rule{page}, []string{"A [severity=page]"}); err != nil || show(got) != "A [severity=info]=i A [g1]=p2" {
		t.Errorf("a twin taken away and given again: %s %v", show(got), err)
	}
	// One rule changed and one of its name added in another group.
	one := []rules.Rule{{Name: "A", Group: "g1", Expr: "a"}}
	if got, err := proposed(one, []rules.Rule{{Name: "A", Group: "g1", Expr: "a-new"}, {Name: "A", Group: "g2", Expr: "a2"}}, nil); err != nil ||
		show(got) != "A=a-new A [g2]=a2" {
		t.Errorf("a change and an addition of one name: %s %v", show(got), err)
	}
	// A twin with a label its sibling lacks, and a label value with a
	// comma in it, are picked by their labels, not by parsing the names.
	odd := []rules.Rule{{Name: "X [severity=page]", Group: "g1", Labels: map[string]string{"severity": "page"}, Expr: "p"},
		{Name: "X", Group: "g1", Expr: "x"},
		{Name: "Y [team=a, b]", Group: "g1", Labels: map[string]string{"team": "a, b"}, Expr: "ab"},
		{Name: "Y [team=c]", Group: "g1", Labels: map[string]string{"team": "c"}, Expr: "c"}}
	if got, err := proposed(odd, []rules.Rule{{Name: "X", Group: "g1", Labels: map[string]string{"severity": "page"}, Expr: "p2"},
		{Name: "Y", Group: "g1", Labels: map[string]string{"team": "a, b"}, Expr: "ab2"}}, nil); err != nil ||
		show(got) != "X [severity=page]=p2 X=x Y [team=a, b]=ab2 Y [team=c]=c" {
		t.Errorf("twins picked by their labels: %s %v", show(got), err)
	}
	if _, err := proposed(one, []rules.Rule{{Name: "A", Group: "g1"}, {Name: "A", Group: "g1"}}, nil); err == nil {
		t.Error("the same rule given twice should be refused")
	}
	if _, err := proposed(current, nil, []string{"Z"}); err == nil {
		t.Error("taking away a rule that isn't there should be refused")
	}
}
