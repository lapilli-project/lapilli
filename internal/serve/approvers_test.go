package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// proposeAs proposes a new for for Errors, as who.
func proposeAs(t *testing.T, h http.Handler, who, d string) int64 {
	t.Helper()
	code, body, hd := ui(t, h, http.MethodPost, "/proposals", who, "same-origin", url.Values{"rule": {"Errors"}, "kind": {"for"}, "for": {d}})
	if code != http.StatusSeeOther {
		t.Fatalf("proposing as %s: %d\n%s", who, code, body)
	}
	var id int64
	json.Unmarshal([]byte(strings.TrimPrefix(hd.Get("Location"), "/proposals/")), &id)
	if id == 0 {
		t.Fatalf("proposing went to %q", hd.Get("Location"))
	}
	return id
}

// approval is a proposal's approval panel, as who reads it.
func approval(t *testing.T, h http.Handler, id int64, who string) string {
	t.Helper()
	code, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), who, "", nil)
	if code != http.StatusOK {
		t.Fatalf("/proposals/%d as %s: %d", id, who, code)
	}
	i := strings.Index(body, `<section class="card" id="approval">`)
	if i < 0 {
		t.Fatalf("/proposals/%d has no approval panel", id)
	}
	j := strings.Index(body[i:], "</section>")
	return body[i : i+j]
}

// TestApproversAsTheyAre: the Roles page says who may approve a change in
// effect, and a proposal's page who may approve it: named to an admin, who
// reads them on the Roles page, and in words to anyone else.
func TestApproversAsTheyAre(t *testing.T) {
	s, _ := changesServer(t, nil)
	h := s.Handler()
	if _, body, _ := ui(t, h, http.MethodGet, "/roles", ana, "", nil); !strings.Contains(body, "<td>Those --approvers names: <bdi>ana@example.com, cy@example.com</bdi>.</td>") {
		t.Errorf("/roles doesn't say who --approvers names:\n%s", body)
	}
	id := proposeFor(t, h, "15m")
	named := "Who may approve it: those --approvers names, but the one it was made by or for: <bdi>ana@example.com, cy@example.com</bdi>."
	if p := approval(t, h, id, ana); !strings.Contains(p, named) {
		t.Errorf("an admin isn't told who may approve it, by name:\n%s", p)
	}
	p := approval(t, h, id, bo)
	if !strings.Contains(p, "Who may approve it: those --approvers names, but the one it was made by or for.</p>") || strings.Contains(p, "cy@example.com") {
		t.Errorf("an editor isn't told who may approve it in words alone:\n%s", p)
	}
	// Approved, nothing is said of who may.
	if code, body := approve(t, h, id, cy); code != http.StatusSeeOther {
		t.Fatalf("approving: %d\n%s", code, body)
	}
	if p := approval(t, h, id, ana); strings.Contains(p, "may approve it") {
		t.Errorf("an approved proposal says who may approve it:\n%s", p)
	}

	// Without --approvers, the admins, named on the Roles page.
	s, _ = changesServer(t, func(c *Config) { c.Approvers = nil; c.Admins = []string{ana, cy} })
	h = s.Handler()
	if _, body, _ := ui(t, h, http.MethodGet, "/roles", ana, "", nil); !strings.Contains(body, "<td>The admins, as --approvers isn&#39;t given: <bdi>ana@example.com, cy@example.com</bdi>.</td>") {
		t.Errorf("/roles doesn't say the admins approve:\n%s", body)
	}
	id = proposeAs(t, h, ana, "15m")
	if p := approval(t, h, id, cy); !strings.Contains(p, "Who may approve it: the admins, as --approvers isn&#39;t given, but the one it was made by or for: <bdi>cy@example.com</bdi>.") {
		t.Errorf("an admin isn't told the other admin may approve it:\n%s", p)
	}

	// Changes off: no one, said on the Roles page; the proposal's button
	// says it already.
	s, _ = server(t, func(c *Config) { c.UserHeader, c.Admins, c.Editors = "X-Forwarded-Email", []string{ana}, []string{"*"} })
	if _, body, _ := ui(t, s.Handler(), http.MethodGet, "/roles", ana, "", nil); !strings.Contains(body, "<td>No one: changes are off, as lapilli serve was started without --changes.</td>") {
		t.Errorf("/roles doesn't say no one approves with changes off:\n%s", body)
	}
}

// TestApproversNoOneLeft: when the only one who may approve a proposal is
// the one it was made by or for, or fewer are left than it needs, its page
// says so, and how to change it, to whoever reads it; a group among those
// who may is anyone, and nothing is said to be stuck.
func TestApproversNoOneLeft(t *testing.T) {
	// The only admin approves, and made it.
	s, _ := changesServer(t, func(c *Config) { c.Approvers = nil })
	h := s.Handler()
	id := proposeAs(t, h, ana, "15m")
	stuck := "No one may approve it: only the admins approve, as --approvers isn&#39;t given, and the only admin is the one it was made by or for. Give --approvers to name who else may."
	for _, who := range []string{ana, bo} {
		if p := approval(t, h, id, who); !strings.Contains(p, stuck) {
			t.Errorf("%s isn't told no one may approve it:\n%s", who, p)
		}
	}
	// Made by an agent for the only admin, the same.
	s, _ = changesServer(t, func(c *Config) { c.Approvers = nil })
	agent := proposeAgent(t, s, ana)
	if p := approval(t, s.Handler(), agent, bo); !strings.Contains(p, stuck) {
		t.Errorf("an agent's proposal for the only admin isn't said to be stuck:\n%s", p)
	}

	// --approvers names only the one who made it.
	s, _ = changesServer(t, func(c *Config) { c.Approvers = []string{"user:" + cy} })
	h = s.Handler()
	id = proposeAs(t, h, strings.ToUpper(cy), "15m")
	if p := approval(t, h, id, bo); !strings.Contains(p, "No one may approve it: --approvers names only the one it was made by or for. Name others in --approvers.") {
		t.Errorf("a proposal only its maker may approve isn't said to be stuck:\n%s", p)
	}

	// Two approvals, one approver left besides the one who made it.
	s, _ = changesServer(t, func(c *Config) { c.Approvals = 2 })
	h = s.Handler()
	id = proposeAs(t, h, cy, "15m")
	if p := approval(t, h, id, bo); !strings.Contains(p, "It needs 2 more approvals, and only 1 of those who may approve haven&#39;t made or approved it: name more in --approvers, or give --approvals 1.") {
		t.Errorf("a proposal that can get one approval of two isn't said to be stuck:\n%s", p)
	}
	// With a group among them, anyone may be in it: nothing is stuck.
	s, _ = changesServer(t, func(c *Config) { c.Approvals, c.Approvers = 2, []string{cy, "group:sre"} })
	h = s.Handler()
	id = proposeAs(t, h, cy, "15m")
	if p := approval(t, h, id, ana); strings.Contains(p, "No one may") || strings.Contains(p, "It needs") ||
		!strings.Contains(p, "but the one it was made by or for: <bdi>group:sre</bdi>.") {
		t.Errorf("a group of approvers is taken for none:\n%s", p)
	}
}

// TestApproversInEachLanguage: what the Roles page and a proposal's page
// say of who may approve is in every language.
func TestApproversInEachLanguage(t *testing.T) {
	for _, c := range []struct {
		change        func(*Config)
		by, as, roles string
	}{
		{func(c *Config) { c.Approvers = nil }, ana, bo, ana},
		{func(c *Config) { c.Approvers = nil }, bo, ana, ""},
		{func(c *Config) { c.Approvers = nil }, bo, bo, ""},
		{nil, bo, ana, ana},
		{nil, bo, bo, ""},
		{func(c *Config) { c.Approvals = 2 }, cy, bo, ""},
		{func(c *Config) { c.Approvers = []string{ana} }, ana, bo, ""},
	} {
		s, _ := changesServer(t, c.change)
		h := s.Handler()
		inEachLanguage(t, h, c.as, "/proposals/"+itoa(proposeAs(t, h, c.by, "15m")))
		if c.roles != "" {
			inEachLanguage(t, h, c.roles, "/roles")
		}
	}
	s, _ := server(t, func(c *Config) { c.UserHeader, c.Admins, c.Editors = "X-Forwarded-Email", []string{ana}, []string{"*"} })
	inEachLanguage(t, s.Handler(), ana, "/roles")
}

// proposeAgent proposes a new for for Errors as an agent acting for who.
func proposeAgent(t *testing.T, s *Server, who string) int64 {
	t.Helper()
	_, out, err := s.toolPropose(context.WithValue(context.Background(), agentFor{}, who), &mcp.CallToolRequest{}, proposeIn{Rule: "Errors", Kind: "for", For: "25m"})
	if err != nil || out.ID == 0 {
		t.Fatalf("an agent proposing for %s: %v", who, err)
	}
	return out.ID
}
