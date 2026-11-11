package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"lapilli.dev/internal/content"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// The proposals' pages, the forms that approve them, and what the change
// recipes read and say on the incidents port.

func (s *Server) changesRoutes(mux *http.ServeMux, page func(string, func(*http.Request) (any, error)) http.HandlerFunc) {
	mux.HandleFunc("GET /proposals", page("proposals", s.proposalsPage))
	mux.HandleFunc("GET /proposals/{id}", page("proposal", s.proposalPage))
	mux.HandleFunc("POST /proposals", s.write(s.postProposal))
	mux.HandleFunc("POST /proposals/{id}/approve", s.person(s.postApprove))
	mux.HandleFunc("POST /proposals/{id}/undo", s.person(s.postUndo))
	mux.HandleFunc("POST /proposals/{id}/drop", s.write(s.postDrop))
	mux.HandleFunc("POST /proposals/{id}/task", s.write(s.postProposalTask))
	mux.HandleFunc("POST /settings/changes", s.admin(s.postChangesPaused))
	// To read, as every page can be; none of the API approves or applies.
	mux.HandleFunc("GET /api/v1/proposals", s.cached(s.apiProposals))
	mux.HandleFunc("GET /api/v1/proposals/{id}", s.cached(s.apiProposal))
}

// errNotApprover is an approval, or an undo, by someone who may not.
var errNotApprover = errors.New("only an approver can approve or undo a change: one named in --approvers, or an admin when it isn't given")

// errNotAForm is an approval that didn't come from the proposal's page, in
// a browser, on this site.
var errNotAForm = errors.New("a change is approved on its page, in a browser: this request didn't come from the page's form")

// person is a handler for what only a person, in the UI, may do: approve a
// change, or undo one. The request must come from a browser on this site
// (Sec-Fetch-Site: same-origin, which a browser sets and a page can't),
// carry the form the proposal's page gave this person, and be an
// approver's; nothing else, no token, MCP or API, reaches it.
func (s *Server) person(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.ReadOnly {
			http.Error(w, "lapilli serve is read-only (--read-only)", http.StatusForbidden)
			return
		}
		if !s.cfg.Changes {
			s.oops(w, r, bad("changes are off: lapilli serve was started without --changes"))
			return
		}
		if r.Header.Get("Sec-Fetch-Site") != "same-origin" {
			s.oops(w, r, errNotAForm)
			return
		}
		if !s.approver(r) {
			s.oops(w, r, errNotApprover)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		h(w, r)
	}
}

type proposalLine struct {
	ledger.Proposal
	// Stale says the rule in effect isn't the one it was made against.
	Stale bool
	Need  int
	Loc   *time.Location
}

type proposalsView struct {
	Lines    []proposalLine
	On       bool // --changes
	Paused   bool
	Limits   Limits
	Need     int
	Approver bool
	State    string
}

// inEffect are the hashes of the rules in effect, by the names the ledger
// judges them under: of when they fire, from the latest version, and with
// what they say, from the rules read last, which a change to what a rule
// says doesn't make a version of.
func (s *Server) inEffect(r *http.Request) (fires, says map[string]string, err error) {
	ctx := r.Context()
	fires, says = map[string]string{}, map[string]string{}
	latest, ok, err := s.l.Latest(ctx)
	if err != nil || !ok {
		return fires, says, err
	}
	all, names, err := s.named(ctx, latest.ID)
	if err != nil {
		return nil, nil, err
	}
	name := map[string]string{}
	for i, x := range all {
		if names[i] != "" {
			fires[names[i]] = ruleHash(x)
			name[identity(x)] = names[i]
		}
	}
	s.saidRules.Lock()
	rs := s.saidRules.rules
	s.saidRules.Unlock()
	for _, x := range rs {
		if n, ok := name[identity(x)]; ok {
			says[n] = saysHash(x)
		}
	}
	return fires, says, nil
}

func (s *Server) proposalLines(r *http.Request, ps []ledger.Proposal) ([]proposalLine, error) {
	fires, says, err := s.inEffect(r)
	if err != nil {
		return nil, err
	}
	var out []proposalLine
	for _, p := range ps {
		l := proposalLine{Proposal: p, Need: s.approvals(), Loc: s.cfg.Loc}
		if p.Open() && p.BeforeHash != "" {
			now := fires
			if saysChange(p) {
				now = says
			}
			l.Stale = now[p.Rule] != p.BeforeHash
		}
		out = append(out, l)
	}
	return out, nil
}

func (s *Server) proposalsPage(r *http.Request) (any, error) {
	q := ledger.ProposalQuery{Limit: 200}
	state := r.URL.Query().Get("state")
	switch state {
	case "", "open":
		state, q.States = "open", ledger.Unfinished()
	case "all":
	default:
		q.States = []string{state}
	}
	ps, err := s.l.Proposals(r.Context(), q)
	if err != nil {
		return nil, err
	}
	// Those waiting for approval first.
	slices.SortStableFunc(ps, func(a, b ledger.Proposal) int {
		x, y := a.State == ledger.StateProposed, b.State == ledger.StateProposed
		switch {
		case x && !y:
			return -1
		case y && !x:
			return 1
		}
		return 0
	})
	lines, err := s.proposalLines(r, ps)
	if err != nil {
		return nil, err
	}
	return proposalsView{Lines: lines, On: s.cfg.Changes, Paused: s.changesPaused(), Limits: s.limits(), Need: s.approvals(),
		Approver: s.approver(r), State: state}, nil
}

type proposalView struct {
	proposalLine
	Evidence evidence
	On       bool
	Paused   bool
	// What the reader may do with it, and the forms' keys for it.
	CanApprove, CanUndo, CanDrop bool
	Approve, Undo                string
	// Why the reader can't approve it, when they are an approver; Next
	// which approval theirs would be.
	Own, Done bool
	Next      int
	// Body is the change as a pull request's description: for a person to
	// make, when it has no path.
	Body string
	Task *ledger.Task
}

func (s *Server) proposalPage(r *http.Request) (any, error) {
	ctx := r.Context()
	p, err := s.l.ProposalByID(ctx, pathID(r))
	if err != nil {
		return nil, err
	}
	lines, err := s.proposalLines(r, []ledger.Proposal{p})
	if err != nil {
		return nil, err
	}
	v := proposalView{proposalLine: lines[0], Evidence: proposalEvidence(p), On: s.cfg.Changes, Paused: s.changesPaused(), Next: len(p.Approvals) + 1}
	user := s.user(r)
	if s.approver(r) {
		v.Own = strings.EqualFold(p.MadeBy, user)
		v.Done = slices.ContainsFunc(p.Approvals, func(a ledger.Approval) bool { return a.By == user })
		if p.State == ledger.StateProposed && p.Path != ledger.PathNone && !v.Own && !v.Done {
			v.CanApprove, v.Approve = true, s.formKey(p, user, "approve")
		}
		if p.State == ledger.StateApplied && p.Kind != ledger.KindUndo && p.Path != ledger.PathNone {
			v.CanUndo, v.Undo = true, s.formKey(p, user, "undo")
		}
	}
	v.CanDrop = s.role(r) >= editor && !s.cfg.ReadOnly &&
		slices.Contains([]string{ledger.StateProposed, ledger.StateApproved}, p.State)
	v.Body = s.prBody(p)
	if p.TaskID != 0 {
		if t, err := s.l.TaskByID(ctx, p.TaskID); err == nil {
			v.Task = &t
		}
	}
	return v, nil
}

// prBody is a proposal as a pull request's description, in English, as
// what is written in a repository is.
func (s *Server) prBody(p ledger.Proposal) string {
	ev := proposalEvidence(p)
	var b strings.Builder
	fmt.Fprintf(&b, "Lapilli proposal #%d: %s, for the rule %s.\n\n", p.ID, kindText(p.Kind), p.Rule)
	if p.Agent {
		b.WriteString("Proposed by an agent, for a person to check.\n\n")
	}
	if bt := ev.Backtest; bt != nil {
		fmt.Fprintf(&b, "Backtest from %s to %s: fired %d times, %d catching an incident, as it is; %d and %d as proposed.",
			bt.From.Format("2006-01-02"), bt.To.Format("2006-01-02"), bt.Before.Firings, bt.Before.White, bt.After.Firings, bt.After.White)
		if len(bt.Lost) > 0 {
			fmt.Fprintf(&b, " Missed as proposed: %s.", strings.Join(bt.Lost, ", "))
		}
		if len(bt.Later) > 0 {
			fmt.Fprintf(&b, " Caught later: %s.", strings.Join(bt.Later, ", "))
		}
		b.WriteString("\n\n")
	}
	if rec := ev.Record; rec != nil {
		fmt.Fprintf(&b, "Over %d weeks it fired %d times, %d catching an incident; it caught %d incidents, %d first, %d that no other rule caught.\n\n",
			rec.Weeks, rec.Firings, rec.White, rec.Incidents, rec.First, rec.Alone)
		b.WriteString("A rule may wait for a rare incident, and incidents nobody wrote down count against it.\n\n")
	}
	if ev.Incident != "" {
		fmt.Fprintf(&b, "Made for the incident %s, which nothing caught.\n\n", ev.Incident)
	}
	if f := ev.Finding; f != nil {
		fmt.Fprintf(&b, "It answers the content check's %s: %s", f.Kind, f.Key)
		if f.Detail != "" {
			fmt.Fprintf(&b, " (%s)", strings.ReplaceAll(f.Detail, "`", "'"))
		}
		b.WriteString(".\n\n")
	}
	if p.Kind == ledger.KindSays {
		b.WriteString("It changes what the alert says, not when it fires.\n\n")
	}
	fmt.Fprintf(&b, "```diff\n%s```\n", strings.ReplaceAll(p.Diff, "```", "'''"))
	if s.cfg.ExternalURL != "" {
		fmt.Fprintf(&b, "\n%s/proposals/%d\n", strings.TrimRight(s.cfg.ExternalURL, "/"), p.ID)
	}
	return b.String()
}

// kindText is what a kind of proposal does, in English.
func kindText(kind string) string {
	switch kind {
	case ledger.KindFor:
		return "a new for"
	case ledger.KindThreshold:
		return "a new threshold"
	case ledger.KindDisable:
		return "turning it off"
	case ledger.KindDelete:
		return "deleting it"
	case ledger.KindSays:
		return "a fix to what it says"
	case ledger.KindUndo:
		return "undoing a change"
	}
	return kind
}

// postProposal makes a proposal from a rule's page.
func (s *Server) postProposal(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	q := ask{Rule: r.PostFormValue("rule"), Kind: r.PostFormValue("kind"), From: "person"}
	if v := r.PostFormValue("for"); v != "" {
		d, err := ParseDuration(v)
		if err != nil {
			s.oops(w, r, bad("for %q: want a duration like 15m", v))
			return
		}
		q.For = d
	}
	if v := r.PostFormValue("value"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			s.oops(w, r, bad("threshold %q: want a number", v))
			return
		}
		q.Bound, q.Value = int(formInt(r, "bound")), n
	}
	if from := r.PostFormValue("from"); slices.Contains([]string{"suggestion", "near miss", "spare", "silent"}, from) {
		q.From = from
	}
	q.Incident = strings.TrimSpace(r.PostFormValue("incident"))
	if q.Kind == ledger.KindSays {
		// A browser sends a text area's lines ending in CR LF.
		q.Annotation, q.Text = r.PostFormValue("annotation"), strings.ReplaceAll(r.PostFormValue("text"), "\r\n", "\n")
		q.Unset, q.Check = r.PostFormValue("unset") == "1", r.PostFormValue("check")
		if q.Check != "" {
			q.From = "content check"
		}
	}
	done, err := s.busy(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	defer done()
	id, err := s.propose(r.Context(), q, s.user(r), false, false)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	back(w, r, fmt.Sprintf("/proposals/%d", id))
}

// proposalForm is the proposal a form names, as the form expects it.
func (s *Server) proposalForm(r *http.Request, what string) (ledger.Proposal, error) {
	if err := form(r); err != nil {
		return ledger.Proposal{}, err
	}
	p, err := s.l.ProposalByID(r.Context(), pathID(r))
	if err != nil {
		return p, err
	}
	if !s.formKeyOK(p, s.user(r), what, r.PostFormValue("key")) {
		return p, errNotAForm
	}
	return p, nil
}

func (s *Server) postApprove(w http.ResponseWriter, r *http.Request) {
	p, err := s.proposalForm(r, "approve")
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if p.Path == ledger.PathNone {
		s.oops(w, r, bad("this change is for a person to make where the rule is written: its diff is on its page"))
		return
	}
	if _, err := s.l.Approve(r.Context(), p.ID, s.user(r), s.approvals()); err != nil {
		s.oops(w, r, err)
		return
	}
	s.poke() // to carry it out
	back(w, r, fmt.Sprintf("/proposals/%d", p.ID))
}

// postUndo asks for an applied change to be undone: the rule written back
// as it was kept, checked as any change is, by the same path.
func (s *Server) postUndo(w http.ResponseWriter, r *http.Request) {
	p, err := s.proposalForm(r, "undo")
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if p.State != ledger.StateApplied || p.Kind == ledger.KindUndo || p.Path == ledger.PathNone {
		s.oops(w, r, ledger.ErrMoved)
		return
	}
	u := ledger.Proposal{Rule: p.Rule, Kind: ledger.KindUndo, Path: p.Path, Follows: p.ID, Source: p.Source,
		Before: p.After, BeforeHash: p.AfterHash, After: p.Before, Backup: p.Backup, Evidence: p.Evidence}
	// Undone, the rule is as it was; a deletion undone, it is back.
	u.AfterHash = p.BeforeHash
	if p.Kind == ledger.KindDelete {
		u.BeforeHash = ""
	}
	switch p.Path {
	case ledger.PathGit:
		// The file as the change left it, back as it was.
		u.DocHash, u.Document, u.Diff = rules.Hash([]byte(p.Document)), p.Backup, rules.Diff(s.docName(p.Source), []byte(p.Document), []byte(p.Backup))
		if p.Backup == "" {
			s.oops(w, r, bad("the file as it was isn't kept, so the change can't be undone here: revert it in the repository"))
			return
		}
	default:
		u.Diff = rules.Diff(p.Rule, []byte(p.After), []byte(p.Before))
	}
	id, err := s.l.Undo(r.Context(), u, s.user(r))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	s.poke()
	back(w, r, fmt.Sprintf("/proposals/%d", id))
}

func (s *Server) postDrop(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	id := pathID(r)
	if _, err := s.l.Move(r.Context(), id, []string{ledger.StateProposed, ledger.StateApproved}, ledger.StateDropped, s.user(r), ledger.Step{}); err != nil {
		s.oops(w, r, err)
		return
	}
	back(w, r, fmt.Sprintf("/proposals/%d", id))
}

// postProposalTask makes a proposal into a task, which carries it.
func (s *Server) postProposalTask(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	ctx := r.Context()
	p, err := s.l.ProposalByID(ctx, pathID(r))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if p.TaskID != 0 {
		back(w, r, fmt.Sprintf("/tasks/%d", p.TaskID))
		return
	}
	kind := "tune"
	if p.Kind == ledger.KindDisable || p.Kind == ledger.KindDelete {
		kind = "retire"
	}
	t := ledger.Task{Title: clip(fmt.Sprintf("Proposal #%d: %s, %s", p.ID, kindText(p.Kind), p.Rule), 200), Kind: kind, Rule: p.Rule,
		Body: clip(s.prBody(p), 10000), Link: p.Link}
	id, err := s.l.PutTask(ctx, t, s.user(r))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if err := s.l.SetProposalTask(ctx, p.ID, id); err != nil {
		s.oops(w, r, err)
		return
	}
	back(w, r, fmt.Sprintf("/tasks/%d", id))
}

// postChangesPaused is the admin's switch: on, nothing is carried out or
// handed to a recipe, at once.
func (s *Server) postChangesPaused(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	on := r.PostFormValue("paused") == "1"
	v := "0"
	if on {
		v = "1"
	}
	if err := s.l.SetMeta(r.Context(), pausedKey, v); err != nil {
		s.oops(w, r, err)
		return
	}
	s.changes.paused.Store(on)
	s.log.Info("changes to the rules", "paused", on, "by", s.user(r))
	back(w, r, "/proposals")
}

func (s *Server) apiProposals(w http.ResponseWriter, r *http.Request) {
	q := ledger.ProposalQuery{Rule: r.URL.Query().Get("rule"), Limit: 200}
	if st := r.URL.Query().Get("state"); st != "" {
		q.States = strings.Split(st, ",")
	}
	ps, err := s.l.Proposals(r.Context(), q)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if ps == nil {
		ps = []ledger.Proposal{}
	}
	for i := range ps {
		ps[i].Document = "" // on its own page
	}
	writeJSON(w, map[string]any{"proposals": ps, "changes": s.cfg.Changes, "paused": s.changesPaused()})
}

func (s *Server) apiProposal(w http.ResponseWriter, r *http.Request) {
	p, err := s.l.ProposalByID(r.Context(), pathID(r))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	writeJSON(w, p)
}

// The change recipes' token, on the incidents port: it reads the changes
// approved and checked for a pull request, and says what became of them,
// and nothing else. It can't approve, or apply.

func (s *Server) changesTokenRoutes(mux *http.ServeMux) {
	if s.cfg.ChangesToken == "" {
		return
	}
	tk := token{s.cfg.ChangesToken, ledger.ChangesCopier, "--changes-token-file",
		[]string{s.cfg.IncidentsToken, s.cfg.FiringsToken, s.cfg.NotifyToken, s.cfg.TasksToken}, &s.changes.warned, 64 << 10, s.changes.rate, s.changes.busy}
	mux.HandleFunc("GET /api/v1/changes", s.withToken(tk, s.apiChanges))
	mux.HandleFunc("PATCH /api/v1/changes/{id}", s.withToken(tk, s.apiPatchChange))
}

// handedOut is a change as a recipe reads it.
type handedOut struct {
	ID       int64  `json:"id"`
	Rule     string `json:"rule"`
	Kind     string `json:"kind"`
	State    string `json:"state"`
	Source   string `json:"source"`
	DocHash  string `json:"doc_hash"`
	Document string `json:"document,omitempty"`
	Branch   string `json:"branch"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	Link     string `json:"link,omitempty"`
}

// apiChanges are the changes ready for a pull request, and those with one
// open, to see what became of it. Without --changes, or while changes are
// paused, none is ready.
func (s *Server) apiChanges(w http.ResponseWriter, r *http.Request) {
	states := []string{ledger.StateOpened}
	if s.cfg.Changes && !s.changesPaused() {
		states = append(states, ledger.StateReady)
	}
	ps, err := s.l.Proposals(r.Context(), ledger.ProposalQuery{States: states, Limit: 50})
	if err != nil {
		s.oops(w, r, err)
		return
	}
	out := []handedOut{}
	for _, p := range ps {
		if p.Path != ledger.PathGit {
			continue
		}
		h := handedOut{ID: p.ID, Rule: p.Rule, Kind: p.Kind, State: p.State, Source: p.Source, DocHash: p.DocHash, Link: p.Link,
			Branch: fmt.Sprintf("lapilli/proposal-%d", p.ID), Title: prTitle(p), Body: s.prBody(p)}
		if p.State == ledger.StateReady {
			h.Document = p.Document
		}
		out = append(out, h)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{"changes": out})
}

// prTitle is a proposal's pull request's title: one line, of printable
// text, as a rule's name may not be.
func prTitle(p ledger.Proposal) string {
	t := fmt.Sprintf("Lapilli: %s, %s", kindText(p.Kind), p.Rule)
	t = strings.Map(func(c rune) rune {
		if unicode.IsControl(c) || unicode.Is(unicode.Cf, c) {
			return ' '
		}
		return c
	}, t)
	return clip(t, 200)
}

// apiPatchChange is a recipe saying what became of a change: a pull request
// opened, merged or closed, or the change failed.
func (s *Server) apiPatchChange(w http.ResponseWriter, r *http.Request) {
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
	ctx := r.Context()
	id := pathID(r)
	p, err := s.l.ProposalByID(ctx, id)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if p.Path != ledger.PathGit {
		s.oops(w, r, ledger.ErrNotFound)
		return
	}
	reason := strings.TrimSpace(strings.Map(func(c rune) rune {
		if unicode.IsControl(c) || unicode.Is(unicode.Cf, c) {
			return ' '
		}
		return c
	}, in.Reason))
	var from []string
	var to string
	step := ledger.Step{}
	switch in.State {
	case "opened":
		u, err := url.Parse(in.Link)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || len(in.Link) > 2048 {
			s.oops(w, r, bad("an opened change says its pull request's link: an http or https URL of 2,048 characters at most"))
			return
		}
		from, to, step.Link = []string{ledger.StateReady}, ledger.StateOpened, in.Link
	case "merged":
		from, to = []string{ledger.StateOpened}, ledger.StateMerged
	case "closed":
		from, to, step.Reason = []string{ledger.StateOpened}, ledger.StateDropped, "the pull request was closed without merging"
	case "failed":
		if reason == "" || len(reason) > 500 {
			s.oops(w, r, bad("a failed change says why, in 500 characters at most"))
			return
		}
		from, to, step.Reason = []string{ledger.StateReady, ledger.StateOpened}, ledger.StateFailed, reason
	default:
		s.oops(w, r, bad("a change's state is opened, merged, closed or failed"))
		return
	}
	got, err := s.l.Move(ctx, id, from, to, ledger.ChangesCopier, step)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	// Its task carries the pull request's link.
	if to == ledger.StateOpened && got.TaskID != 0 {
		patch, _ := json.Marshal(map[string]string{"link": got.Link})
		if _, err := s.l.PatchTask(ctx, got.TaskID, patch, ledger.ChangesCopier); err != nil && !errors.Is(err, ledger.ErrNotFound) {
			s.log.Warn("a proposal's task didn't take its pull request's link", "task", got.TaskID, "err", err)
		}
	}
	got.Document, got.Backup = "", ""
	writeJSON(w, got)
}

// ruleChanges are a rule page's proposals, and what its forms propose.
type ruleChanges struct {
	Proposals []proposalLine
	// SuggestedFor is the digest's suggested for, when there is one.
	SuggestedFor string
	// Thresholds are the numbers its condition compares with, and a near
	// miss's when one was found.
	Thresholds []thresholdForm
	// RetireKind is how it is retired: turned off, where it can be, or
	// deleted.
	RetireKind string
	// SaysForms propose a fix for what the content check found that the
	// text can fix: a summary, a runbook link. Lapilli doesn't write the
	// text; a person, or an agent, does.
	SaysForms []saysForm
	// SaysKeys are the annotations a fix to what it says may change.
	SaysKeys []string
}

type saysForm struct {
	Annotation, Check, Label string
	Long                     bool // a text area
}

// saysFix is the annotation that fixes a finding, and the form's label.
var saysFix = map[string]saysForm{
	content.NoText:        {Annotation: "summary", Label: "Propose a summary"},
	content.NoRunbook:     {Annotation: "runbook_url", Label: "Propose a runbook link"},
	content.RunbookNotURL: {Annotation: "runbook_url", Label: "Propose a runbook link"},
}

type thresholdForm struct {
	At                           int
	Op, Now, Suggested, Incident string
}

func (s *Server) ruleChangesOf(r *http.Request, v *ruleView) error {
	ctx := r.Context()
	ps, err := s.l.Proposals(ctx, ledger.ProposalQuery{Rule: v.Name, Limit: 20})
	if err != nil {
		return err
	}
	if v.Proposals, err = s.proposalLines(r, ps); err != nil {
		return err
	}
	if !s.replaying() {
		return nil
	}
	f, err := s.current(ctx, v.Name, false)
	if err != nil {
		var b badRequest
		if errors.As(err, &b) {
			return nil // not in effect: nothing to propose
		}
		return err
	}
	v.RetireKind = ledger.KindDelete
	if f.rule.UID != "" {
		v.RetireKind = ledger.KindDisable
	}
	v.SaysKeys = saysKeys
	fs, err := s.contentFindings(ctx)
	if err != nil {
		return err
	}
	for _, x := range fs[v.Name] {
		if fix, ok := saysFix[x.Kind]; ok && x.Class == content.Fact {
			fix.Check = x.Kind
			v.SaysForms = append(v.SaysForms, fix)
		}
	}
	if rs, err := s.l.Results(ctx, "suggestions", 1); err != nil {
		return err
	} else if len(rs) > 0 {
		var ss []verdict.Suggestion
		if err := rs[0].Decode(&ss); err != nil {
			return err
		}
		for _, x := range ss {
			if x.Rule == v.Name {
				v.SuggestedFor = verdict.Duration(x.NewFor)
			}
		}
	}
	if f.rule.Graph != nil || f.rule.Unsupported != "" {
		return nil
	}
	near, err := s.nearMisses(ctx)
	if err != nil {
		return err
	}
	for _, b := range f.rule.Bounds() {
		t := thresholdForm{At: b.At, Op: b.Op, Now: strconv.FormatFloat(b.Value, 'g', -1, 64)}
		for _, id := range slices.Sorted(maps.Keys(near)) {
			for _, nm := range near[id] {
				if nm.Rule == v.Name && nm.Value != nil && nm.Value.Op == b.Op && nm.Value.Bound == b.Value && nm.Value.Of == b.Of {
					t.Suggested, t.Incident = strconv.FormatFloat(nm.Value.New, 'g', -1, 64), nm.Incident
				}
			}
		}
		v.Thresholds = append(v.Thresholds, t)
	}
	return nil
}

// proposedRules are the rules with a proposal not yet finished with.
func (s *Server) proposedRules(ctx context.Context) (map[string]bool, error) {
	ps, err := s.l.Proposals(ctx, ledger.ProposalQuery{States: ledger.Unfinished(), Limit: 1000})
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, p := range ps {
		out[p.Rule] = true
	}
	return out, nil
}
