package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// Where drafts of expectations show and are decided (docs/design/
// expectations.md, step 3): on a rule's expectations tab, each with what
// checking it found, accepted by an approver who isn't its proposer, in
// the UI alone, as a proposal is approved; dropped by whoever may drop a
// proposal; the history's draft of the rule beside them, and of a team's
// rules or every rule as a file to take; the open ones on the proposals
// page; and the API.

func (s *Server) draftRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /expectations/drafts", s.teamWrite(s.postHistoryDraft))
	mux.HandleFunc("POST /expectations/drafts/{id}/accept", s.person(s.postAcceptDraft))
	mux.HandleFunc("POST /expectations/drafts/{id}/drop", s.need(viewer, s.postDropDraft))
	mux.HandleFunc("GET /expectations/draft.yaml", s.draftFile)
	mux.HandleFunc("GET /api/v1/expectations/draft", s.apiHistoryDraft)
	mux.HandleFunc("GET /api/v1/expectation-drafts", s.cached(s.apiDrafts))
	mux.HandleFunc("GET /api/v1/expectation-drafts/{id}", s.cached(s.apiDraft))
}

// draftLine is a draft as a rule's tab shows it: what it found, what the
// reader may do with it, and, accepted, where it went.
type draftLine struct {
	ledger.ExpectDraft
	Items  []draftItem
	Tested []draftTest
	// Own says the reader proposed it, or their agent did.
	Own bool
	// CanAccept, with Key, the reader's form to accept it; CanDrop, to
	// drop it; GitPath, accepting it opens a pull request.
	CanAccept, CanDrop, GitPath bool
	Key                         string
	// Taken is, accepted with no pull request, what a person puts where
	// the expectations are declared; Whole says it is the whole file;
	// TakenWhy, why it can't be given.
	Taken    string
	Whole    bool
	TakenWhy string
}

// draftTest is a draft's test file, as its tab shows it.
type draftTest struct{ Path, Text string }

// draftsView are a rule's drafts, and the history's draft of it.
type draftsView struct {
	Lines []draftLine
	// History is the history's draft of the rule, as an expectations file;
	// HistoryNone why there is none.
	History, HistoryNone string
	// Keep says the reader may keep it as a draft; TeamFile and AllFile
	// are the history's drafts of the rule's team's rules and of every
	// rule, as files to take.
	Keep              bool
	TeamFile, AllFile string
	// Approver, On: the reader may accept, and changes are on.
	Approver, On bool
	Target       string
}

// draftsViewOf are a rule's drafts, the latest first, as the reader may
// act on them, and the history's draft of it.
func (s *Server) draftsViewOf(r *http.Request, rule string, team int64, teamName string) (*draftsView, error) {
	ctx := r.Context()
	ds, err := s.l.Drafts(ctx, ledger.DraftQuery{Rule: rule, HasRule: true, Limit: maxDraftsShown})
	if err != nil {
		return nil, err
	}
	user := s.user(r)
	bearer := false
	if id, ok := whoOf(r); ok && id.by == byBearer {
		bearer = true
	}
	target, _ := s.draftTarget()
	v := &draftsView{Approver: s.approver(r) && !bearer, On: s.cfg.Changes, Target: target}
	mayDrop := !s.cfg.ReadOnly && (s.role(r) >= editor || s.approver(r))
	for _, d := range ds {
		l := draftLine{ExpectDraft: d, Items: draftItemsOf(d.Found), Own: ledger.FoldName(d.MadeBy) == ledger.FoldName(user)}
		for _, p := range sortedKeys(d.Tests) {
			l.Tested = append(l.Tested, draftTest{p, d.Tests[p]})
		}
		if d.State == ledger.DraftOpen {
			l.GitPath = s.gitPath(d)
			l.CanDrop = mayDrop
			if v.Approver && !l.Own {
				l.CanAccept, l.Key = true, s.draftFormKey(d, user)
			}
		}
		if d.State == ledger.DraftAccepted && d.PR == "" {
			// Read when shown: the file as it is now, the draft in it.
			var terr error
			if l.Taken, _, l.Whole, terr = s.takenIn(d); terr != nil {
				l.Taken, l.Whole, l.TakenWhy = d.Text, false, terr.Error()
			}
		}
		v.Lines = append(v.Lines, l)
	}
	// The history's, of this rule.
	h, about, err := s.historyDraft(ctx, func(name string, _ int64) bool { return name == rule })
	if err != nil {
		return nil, err
	}
	if len(h.Expectations) > 0 {
		b, err := h.YAML(about)
		if err != nil {
			return nil, err
		}
		v.History = string(b)
	} else {
		v.HistoryNone = "Nothing is counted of it yet: it has no whole week in the ledger, or Lapilli doesn't replay it."
	}
	v.Keep = len(h.Expectations) > 0 && !s.cfg.ReadOnly && (s.role(r) >= editor || team != 0 && slices.Contains(s.editsTeams(r), team))
	if teamName != "" {
		v.TeamFile = "/expectations/draft.yaml?" + url.Values{"team": {teamName}}.Encode()
	}
	v.AllFile = "/expectations/draft.yaml"
	return v, nil
}

// draftBadge is a draft's state as its badge says it, as a proposal's
// badge says its: one waiting for a person, accepted or dropped.
func draftBadge(state string) stateBadge {
	b := stateBadge{}
	switch state {
	case ledger.DraftOpen:
		b.Class, b.Sign, b.Word = "st-wait", "◷", "awaiting a person"
	case ledger.DraftAccepted:
		b.Class, b.Sign, b.Word = "st-ok", "✓", "accepted"
	default:
		b.Class, b.Sign, b.Word = "st-closed", "⊘", "dropped"
	}
	b.Class += " " + statusClass("draft", state)
	return b
}

// postHistoryDraft keeps the history's draft of a rule, for another to
// accept: a team's editor, of their own team's rule.
func (s *Server) postHistoryDraft(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	ctx := r.Context()
	rule := r.PostFormValue("rule")
	if rule == "" {
		s.oops(w, r, bad("say which rule's draft to keep"))
		return
	}
	o, err := s.owners(ctx)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if teams, limited := scoped(r); limited && (o.rule[rule] == 0 || !slices.Contains(teams, o.rule[rule])) {
		s.oops(w, r, ledger.ErrNotYourTeam)
		return
	}
	id, err := s.keepHistoryDraft(ctx, rule, s.user(r))
	s.written(w, r, err, "/rules/"+url.PathEscape(rule)+"?tab=expectations#draft-"+strconv.FormatInt(id, 10))
}

// draftForm is the draft a form names.
func (s *Server) draftForm(r *http.Request) (ledger.ExpectDraft, error) {
	if err := form(r); err != nil {
		return ledger.ExpectDraft{}, err
	}
	return s.l.DraftByID(r.Context(), pathID(r))
}

// postAcceptDraft is an approver accepting a draft, on its rule's tab, in
// a browser (person): never its proposer, nor the person whose agent made
// it, which the ledger refuses in the write. By the git path where it
// can go (gitPath), with the expectations file it makes, read now: ready
// for a change recipe; otherwise the decision is kept, and its tab gives
// the YAML for a person to put where the expectations are declared.
func (s *Server) postAcceptDraft(w http.ResponseWriter, r *http.Request) {
	d, err := s.draftForm(r)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if !s.draftFormKeyOK(d, s.user(r), r.PostFormValue("key")) {
		s.oops(w, r, errNotAForm)
		return
	}
	doc, hash := "", ""
	if s.gitPath(d) {
		text, h, whole, err := s.takenIn(d)
		if err != nil || !whole {
			s.oops(w, r, bad("the draft can't be taken into the expectations file: %v", err))
			return
		}
		doc, hash = text, h
	}
	if _, err := s.l.AcceptDraft(r.Context(), d.ID, s.user(r), doc, hash); err != nil {
		s.oops(w, r, err)
		return
	}
	back(w, r, "/rules/"+url.PathEscape(d.Rule)+"?tab=expectations#draft-"+strconv.FormatInt(d.ID, 10))
}

// postDropDraft drops an open draft: an editor's, or an approver's, its
// proposer's among them.
func (s *Server) postDropDraft(w http.ResponseWriter, r *http.Request) {
	if s.cfg.ReadOnly || s.role(r) < editor && !s.approver(r) {
		s.oops(w, r, s.forbidden(editor))
		return
	}
	d, err := s.draftForm(r)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if _, err := s.l.DropDraft(r.Context(), d.ID, s.user(r)); err != nil {
		s.oops(w, r, err)
		return
	}
	back(w, r, "/rules/"+url.PathEscape(d.Rule)+"?tab=expectations#draft-"+strconv.FormatInt(d.ID, 10))
}

// historyPick is which rules the history's draft is of, as asked: a
// rule's (rule=), a team's (team=), or every rule's.
func (s *Server) historyPick(ctx context.Context, q url.Values) (func(string, int64) bool, string, error) {
	switch {
	case q.Has("rule"):
		name := q.Get("rule")
		return func(n string, _ int64) bool { return n == name }, "", nil
	case q.Get("team") != "":
		o, err := s.owners(ctx)
		if err != nil {
			return nil, "", err
		}
		t, ok := o.team(q.Get("team"))
		if !ok {
			return nil, "", bad("no team called %q", q.Get("team"))
		}
		return func(_ string, team int64) bool { return team == t.ID }, t.Name, nil
	}
	return func(string, int64) bool { return true }, "", nil
}

// draftFile is the history's draft, as an expectations file to take: of
// a rule, a team's rules, or every rule.
func (s *Server) draftFile(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.RawQuery) > maxQuery {
		s.oops(w, r, errLongQuery)
		return
	}
	pick, _, err := s.historyPick(r.Context(), r.URL.Query())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	d, about, err := s.historyDraft(r.Context(), pick)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	b, err := d.YAML(about)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="expectations.yaml"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}

// historyDraftOut is the history's draft as the API gives it.
type historyDraftOut struct {
	From  time.Time              `json:"from"`
	To    time.Time              `json:"to"`
	Team  string                 `json:"team,omitempty"`
	Rules int                    `json:"rules"`
	Left  []verdict.DraftLeftOut `json:"left_out"`
	YAML  string                 `json:"yaml"`
}

func (s *Server) apiHistoryDraft(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.RawQuery) > maxQuery {
		s.oops(w, r, errLongQuery)
		return
	}
	pick, team, err := s.historyPick(r.Context(), r.URL.Query())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	d, about, err := s.historyDraft(r.Context(), pick)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	b, err := d.YAML(about)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	left := d.Left
	if left == nil {
		left = []verdict.DraftLeftOut{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, historyDraftOut{From: about.From, To: about.To, Team: team, Rules: len(d.Expectations), Left: left, YAML: string(b)})
}

// draftOut is a draft as the API gives it: what it found, item by item.
type draftOut struct {
	ledger.ExpectDraft
	Items []draftItem `json:"items"`
}

func draftOutOf(d ledger.ExpectDraft) draftOut {
	items := draftItemsOf(d.Found)
	if items == nil {
		items = []draftItem{}
	}
	return draftOut{ExpectDraft: d, Items: items}
}

// apiDrafts are the drafts, of a rule (rule=) or of some states
// (state=open,accepted): a viewer's, as proposals are.
func (s *Server) apiDrafts(w http.ResponseWriter, r *http.Request) {
	q := ledger.DraftQuery{Limit: 200}
	if v, ok := r.URL.Query()["rule"]; ok {
		q.Rule, q.HasRule = v[0], true
	}
	if st := r.URL.Query().Get("state"); st != "" {
		q.States = strings.Split(st, ",")
	}
	ds, err := s.l.Drafts(r.Context(), q)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	out := []draftOut{}
	for _, d := range ds {
		out = append(out, draftOutOf(d))
	}
	writeJSON(w, map[string]any{"drafts": out})
}

func (s *Server) apiDraft(w http.ResponseWriter, r *http.Request) {
	d, err := s.l.DraftByID(r.Context(), pathID(r))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	writeJSON(w, draftOutOf(d))
}

// openDrafts are the drafts waiting to be accepted or dropped, of a
// team's rules when rules are given: the proposals page's.
func (s *Server) openDrafts(ctx context.Context, rules []string, team bool) ([]ledger.ExpectDraft, error) {
	ds, err := s.l.Drafts(ctx, ledger.DraftQuery{States: []string{ledger.DraftOpen}, Limit: 50})
	if err != nil || !team {
		return ds, err
	}
	return slices.DeleteFunc(ds, func(d ledger.ExpectDraft) bool { return !slices.Contains(rules, d.Rule) }), nil
}

// The git path: an accepted draft handed to the change recipes, beside
// the proposals, as a change to the expectations file (docs/design/
// changes.md, "A pull request, first"). A recipe finds the file by the
// hash of what serve last read of it, and writes nothing but an
// expectations file there.

// handedDraft is an accepted draft as a recipe reads it.
type handedDraft struct {
	ID       int64  `json:"id"`
	Ledger   string `json:"ledger"`
	Rule     string `json:"rule"`
	State    string `json:"state"`
	DocHash  string `json:"doc_hash"`
	Document string `json:"document,omitempty"`
	Branch   string `json:"branch"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	Link     string `json:"link,omitempty"`
}

// handedDrafts are the drafts ready for a pull request, and those with one
// open: none ready without --changes, or while changes are paused.
func (s *Server) handedDrafts(ctx context.Context) ([]handedDraft, error) {
	prs := []string{ledger.DraftOpened}
	if s.cfg.Changes && !s.changesPaused() {
		prs = append(prs, ledger.DraftReady)
	}
	ds, err := s.l.Drafts(ctx, ledger.DraftQuery{PRs: prs, Limit: 50})
	if err != nil {
		return nil, err
	}
	out := []handedDraft{}
	for _, d := range ds {
		// DocHash is the file the draft was taken into, as serve read it
		// when it was accepted: what the recipe finds it by.
		h := handedDraft{ID: d.ID, Ledger: s.ledgerID, Rule: d.Rule, State: d.PR, DocHash: d.DocHash, Link: d.Link,
			Branch: fmt.Sprintf("lapilli/%s/expectations-%d", s.ledgerID, d.ID), Title: draftTitle(d), Body: s.draftBody(d)}
		if d.PR == ledger.DraftReady {
			h.Document = d.Document
		}
		out = append(out, h)
	}
	return out, nil
}

// draftTitle is a draft's pull request's title: one line, of printable
// text.
func draftTitle(d ledger.ExpectDraft) string {
	t := strings.Map(func(c rune) rune {
		if unicode.IsControl(c) || unicode.Is(unicode.Cf, c) {
			return ' '
		}
		return c
	}, "Lapilli: expectations of "+d.Rule)
	return clip(t, 200)
}

// draftBody is a draft's pull request's description: what it holds the
// rule to, what checking it found, who proposed and accepted it; what
// comes from the rule or an agent in code, so it formats nothing.
func (s *Server) draftBody(d ledger.ExpectDraft) string {
	var b strings.Builder
	origin := "the history's: the most the rule fired in a week, rounded up, and the incidents it caught"
	if d.Agent {
		origin = "an agent's, for " + mdText(d.MadeBy) + ": its words are its own"
	}
	fmt.Fprintf(&b, "A draft of what %s is held to, %s, accepted by %s in Lapilli (#%d).\n\n", mdText(d.Rule), origin, mdText(d.DecidedBy), d.ID)
	b.WriteString("| Held to | As it is | |\n|---|--:|---|\n")
	for _, it := range draftItemsOf(d.Found) {
		fmt.Fprintf(&b, "| %s %s | %s | %s |\n", mdCell(it.What), mdCell(it.Held), mdCell(it.Value), mdCell(it.State))
	}
	if s.cfg.ExternalURL != "" {
		fmt.Fprintf(&b, "\nIts page: %s\n", draftPage(s.cfg.ExternalURL, d.Rule, d.ID))
	}
	return b.String()
}

// mdCell is text from a rule or an agent as a code span in a table, as a
// pull request comment's table has it (verdict.CodeSpan): what doesn't
// show said as \u{...}, each | escaped and a backslash right before one
// said as \u{5c}, so that no | of it ends the cell, however a host reads
// a run of backslashes (0.60.0's fix). mdText is one in a sentence.
func mdCell(s string) string { return verdict.CodeSpan(s, mdMost, true) }

func mdText(s string) string { return verdict.CodeSpan(s, mdMost, false) }

// apiPatchDraft is a recipe saying what became of a draft's pull request:
// opened, merged, closed, or failed with why.
func (s *Server) apiPatchDraft(w http.ResponseWriter, r *http.Request) {
	var in struct {
		State  string `json:"state"`
		Link   string `json:"link"`
		Reason string `json:"reason"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		s.oops(w, r, bad("want JSON: %v", err))
		return
	}
	reason := strings.TrimSpace(strings.Map(func(c rune) rune {
		if unicode.IsControl(c) || unicode.Is(unicode.Cf, c) {
			return ' '
		}
		return c
	}, in.Reason))
	var from []string
	var to, link, why string
	switch in.State {
	case "opened":
		u, err := url.Parse(in.Link)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || len(in.Link) > 2048 {
			s.oops(w, r, bad("an opened change says its pull request's link: an http or https URL of 2,048 characters at most"))
			return
		}
		from, to, link = []string{ledger.DraftReady}, ledger.DraftOpened, in.Link
	case "merged":
		from, to = []string{ledger.DraftOpened}, ledger.DraftMerged
	case "closed":
		from, to, why = []string{ledger.DraftOpened}, ledger.DraftClosed, "the pull request was closed without merging"
	case "failed":
		if reason == "" || len(reason) > 500 {
			s.oops(w, r, bad("a failed change says why, in 500 characters at most"))
			return
		}
		from, to, why = []string{ledger.DraftReady, ledger.DraftOpened}, ledger.DraftFailed, reason
	default:
		s.oops(w, r, bad("a change's state is opened, merged, closed or failed"))
		return
	}
	d, err := s.l.MoveDraftPR(r.Context(), pathID(r), from, to, link, why)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	writeJSON(w, draftOutOf(d))
}
