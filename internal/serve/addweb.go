package serve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
)

// The new rule's form, its proposal's page, and MCP's places and propose
// of kind add (docs/design/expectations.md, step 5).

// newRuleView is the new rule's form: the places a rule may be added to,
// and an example of each thing it takes.
type newRuleView struct {
	Places placesView
	// Off says why the form isn't offered: no place, or rules not read.
	Off    string
	On     bool // --changes
	Paused bool
}

// newRulePage is the new rule's form, an editor's, as proposing is.
func (s *Server) newRulePage(r *http.Request) (any, error) {
	v := newRuleView{On: s.cfg.Changes, Paused: s.changesPaused()}
	offers, err := s.addPlaces(r.Context())
	if err != nil {
		return nil, err
	}
	v.Places = placesViewOf(offers)
	switch {
	case !s.replaying():
		v.Off = "this lapilli serve reads no rules of its own: there is nothing to add to"
	case len(offers) == 0:
		v.Off = "No place serve reads takes a rule added: a group of a rule file under --rules or --loki-rules, a Grafana folder's group, a ruler's namespace --changes-in names (Loki's as loki:NAMESPACE), or CloudWatch's or Google Cloud's alarms read with --alarms."
	}
	return v, nil
}

// postAddProposal is the new rule's form, posted: a proposal adding it,
// made under the heavy turn, as a rule's page's proposals are.
func (s *Server) postAddProposal(w http.ResponseWriter, r *http.Request) {
	offers, err := s.addPlaces(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	place, ok := offerByKey(offers, r.PostFormValue("place"), strings.TrimSpace(r.PostFormValue("group")))
	if !ok {
		s.oops(w, r, errNoPlace)
		return
	}
	if place.Interval, err = durationOrZero(r.PostFormValue("interval")); err != nil {
		s.oops(w, r, err)
		return
	}
	place.DataSource = r.PostFormValue("data_source")
	for _, t := range r.PostForm["tells"] {
		if t != "" {
			place.Tells = append(place.Tells, t)
		}
	}
	reason, err := agentReason(r.PostFormValue("reason"))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	q := addAsk{Rule: strings.ReplaceAll(r.PostFormValue("rule"), "\r\n", "\n"), Place: place,
		Expect: strings.ReplaceAll(r.PostFormValue("expectations"), "\r\n", "\n"), From: "person", Reason: reason}
	by := s.user(r)
	// Refused without waiting for the heavy turn: a rule that doesn't
	// read, the hour's proposals, the reader's tries.
	a, err := rules.ParseAdded([]byte(q.Rule))
	if err != nil {
		s.oops(w, r, ruleRefused(err))
		return
	}
	if err := s.mayProposeAdd(r.Context(), a, by, false); err != nil {
		s.oops(w, r, err)
		return
	}
	done, err := s.busy(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	defer done()
	id, err := s.proposeAdd(r.Context(), q, by, false)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	back(w, r, fmt.Sprintf("/proposals/%d", id))
}

// agentReason is why, in whoever proposes it's words: what doesn't show
// taken out but line breaks, 2,000 bytes at most.
func agentReason(v string) (string, error) {
	reason := strings.TrimSpace(strings.Map(func(c rune) rune {
		if c != '\n' && !shows(string(c)) {
			return -1
		}
		return c
	}, strings.ReplaceAll(v, "\r\n", "\n")))
	if len(reason) > 2000 {
		return "", bad("reason is 2,000 characters at most")
	}
	return reason, nil
}

// shows reports whether a text holds nothing that doesn't show.
func shows(s string) bool {
	for _, c := range s {
		switch {
		case c < 0x20, c == 0x7f, c >= 0x80 && c < 0xa0, c == 0x200b, c >= 0x202a && c <= 0x202e, c >= 0x2066 && c <= 0x2069, c == 0xfeff, c == 0xfffd:
			return false
		}
	}
	return true
}

// addView is what a proposal adding a rule's page shows beside its
// backtest: where it goes, the rule as Lapilli wrote it, each expectation
// it is held to with what its backtest found, why it isn't approved when
// it isn't, and, once added, where its expectations went.
type addView struct {
	Place addPlace
	Rule  string
	Items []draftItem
	Weeks int
	Note  string
	// Held is why it isn't approved, "" when it may be.
	Held string
	// Expect is what it is held to, as Lapilli wrote it; Draft, once it is
	// added, the draft its expectations were kept as.
	Expect string
	Draft  *ledger.ExpectDraft
	// YAML is, once added, its expectations to put where they are declared
	// by hand, when they didn't go by the git path.
	YAML string
}

// addViewOf is a proposal adding a rule's addView.
func (s *Server) addViewOf(ctx context.Context, l proposalLine, ev evidence) (*addView, error) {
	if l.Kind != ledger.KindAdd || ev.Add == nil {
		return nil, nil
	}
	v := &addView{Place: ev.Add.Place, Rule: ev.Add.Rule, Items: ev.Add.Items, Weeks: ev.Add.Weeks, Note: ev.Add.Note, Held: l.AddHeld}
	x, err := s.l.ProposalExpect(ctx, l.ID)
	if err != nil {
		return nil, err
	}
	v.Expect = x.Text
	if d, err := s.l.AddedDraft(ctx, l.ID); err == nil {
		v.Draft = &d
		if d.PR == "" {
			if v.YAML, err = s.addedYAML(ctx, l.Proposal); err != nil {
				return nil, err
			}
		}
	} else if !errors.Is(err, ledger.ErrNotFound) {
		return nil, err
	}
	return v, nil
}

// MCP: places, and propose's kind add.

type placesIn struct{}

type placesOut struct {
	Places []placeOut `json:"places"`
}

type placeOut struct {
	Path        string   `json:"path" jsonschema:"git, grafana, ruler, loki or alarm: the way a change goes there"`
	File        string   `json:"file,omitempty" jsonschema:"git: the rule file, as the rules are named by it"`
	Loki        bool     `json:"loki,omitempty" jsonschema:"git: the file is a Loki ruler's rule file (--loki-rules), whose rule is LogQL; loki, a Loki ruler's API: its rule is LogQL too"`
	Folder      string   `json:"folder,omitempty" jsonschema:"grafana: the folder's title"`
	Namespace   string   `json:"namespace,omitempty" jsonschema:"ruler and loki: the namespace"`
	Alarms      string   `json:"alarms,omitempty" jsonschema:"alarm: the --alarms source"`
	Group       string   `json:"group,omitempty" jsonschema:"the group; none for a new group, whose name you give"`
	NewGroup    bool     `json:"new_group,omitempty" jsonschema:"a new group, made for the rule, in that file or namespace"`
	DataSources []string `json:"data_sources,omitempty" jsonschema:"grafana: the data sources its folder's rules read, one of which the rule's query reads"`
	Tells       []string `json:"tells,omitempty" jsonschema:"grafana and alarm: whom the rules there tell (a contact point, an action that only tells, a notification channel): a rule added tells only some of these"`
	MustTell    bool     `json:"must_tell,omitempty" jsonschema:"grafana: every rule of the group names a contact point, so a rule added names one of tells; otherwise one naming none is routed by Grafana's notification policies, by its labels, as those naming none are"`
}

// toolPlaces are the places a rule may be added to, for propose's kind
// add.
func (s *Server) toolPlaces(ctx context.Context, _ *mcp.CallToolRequest, _ placesIn) (*mcp.CallToolResult, placesOut, error) {
	out := placesOut{Places: []placeOut{}}
	offers, err := s.addPlaces(ctx)
	if err != nil {
		return nil, out, err
	}
	for _, o := range offers {
		out.Places = append(out.Places, placeOut{Path: o.Path, File: o.File, Loki: o.Loki, Folder: o.Folder, Namespace: o.Namespace, Alarms: o.Alarms, Group: o.Group,
			NewGroup: o.NewGroup, DataSources: o.DataSources, Tells: o.Tells, MustTell: grafanaRouted(o.addPlace, o.Tells, o.routed) != nil})
	}
	return nil, out, nil
}

// addOf is an agent's ask to add a rule: its place, as places gives one.
func (in proposeIn) addOf(reason string) (addAsk, error) {
	q := addAsk{Rule: in.Added, Expect: in.Expectations, Tests: in.Tests, From: "agent", Reason: reason,
		Place: addPlace{Path: in.Path, File: in.File, Loki: in.Loki, Folder: in.Folder, Namespace: in.Namespace, Alarms: in.Alarms, Group: in.Group,
			NewGroup: in.NewGroup, DataSource: in.DataSource, Tells: in.Tells}}
	d, err := durationOrZero(in.Interval)
	if err != nil {
		return q, errors.New("interval: a duration such as 1m")
	}
	q.Place.Interval = d
	if len(in.Added) > maxAgentRule || len(in.Expectations) > maxAgentExpect {
		return q, fmt.Errorf("added is %d bytes at most, expectations %d", maxAgentRule, maxAgentExpect)
	}
	if a, err := rules.ParseAdded([]byte(in.Added)); err == nil && a.Alert != in.Rule {
		return q, errors.New("kind add: rule names the new rule, as its alert does")
	}
	return q, nil
}
