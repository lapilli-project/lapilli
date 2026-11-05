package serve

import (
	"context"
	"encoding/json"
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
	for _, bad := range []map[string]any{{"rules": "groups: [{"}, {}, {"remove": []string{"Nope"}}, {"rules": longer, "days": 30}} {
		if failed, text, _ := compare(bad); !failed {
			t.Errorf("%v should be refused: %s", bad, text)
		}
	}

	// A browser can't be made to ask, from a page elsewhere or a name
	// pointed here.
	for _, origin := range []string{"https://evil.example", ts.URL} {
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

// A rule given is put in place of the rule of its name in effect, told
// apart by group where several share it.
func TestProposed(t *testing.T) {
	current := []rules.Rule{{Name: "A [g1]", Group: "g1", Expr: "a"}, {Name: "A [g2]", Group: "g2", Expr: "a"}, {Name: "B", Group: "g1", Expr: "b"}}
	got, err := proposed(current, []rules.Rule{{Name: "A", Group: "g2", Expr: "a2"}, {Name: "B", Group: "other", Expr: "b2"}, {Name: "C", Expr: "c"}}, []string{"A [g1]"})
	if err != nil {
		t.Fatal(err)
	}
	var have []string
	for _, r := range got {
		have = append(have, r.Name+"="+r.Expr)
	}
	if strings.Join(have, " ") != "A [g2]=a2 B=b2 C=c" {
		t.Errorf("proposed: %v", have)
	}
	// An A in another group is another rule; one of two in the same
	// group, told apart by labels, can't be picked.
	if got, err := proposed(current, []rules.Rule{{Name: "A", Group: "g3"}}, nil); err != nil || len(got) != 4 {
		t.Errorf("an A in another group: %v %v", got, err)
	}
	twins := []rules.Rule{{Name: "A [severity=page]", Group: "g1"}, {Name: "A [severity=info]", Group: "g1"}}
	if _, err := proposed(twins, []rules.Rule{{Name: "A", Group: "g1"}}, nil); err == nil || !strings.Contains(err.Error(), "several rules") {
		t.Errorf("an A of two in g1: %v", err)
	}
	if _, err := proposed(current, nil, []string{"Z"}); err == nil {
		t.Error("taking away a rule that isn't there should be refused")
	}
}
