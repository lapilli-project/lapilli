package serve

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// A proposal for the rules picked (docs/design/ui.md): the rules list's bar
// proposes, for each rule picked, the change its own page's form would
// propose, made by the reader, through the same doors: an editor's, each
// rule read again and backtested under the heavy turn, as many an hour as
// one by one, one unfinished proposal a rule. Each is approved as one made
// on its rule's page is; nothing changes without a person's approval.

// maxPropose is how many rules one bulk proposal takes: each is read again
// and backtested, as heavy as its page's form, and a person makes
// ledger.ProposalsAnHour an hour at most.
const maxPropose = ledger.ProposalsAnHour

// proposeTime is how long a bulk proposal starts rules for: no rule is
// started after it, and those left are not tried ("time's up, pick them
// again"), so that the answer comes back within the time a proxy in front
// commonly gives a request (30 seconds for some load balancers' backends,
// 60 for some ingresses), which, past it, closes the request and leaves
// the reader an error for what was made. A rule started just before it
// takes what it takes: its wait for the heavy turn ends with it too, its
// backtest within --timeout.
var proposeTime = 20 * time.Second

// proposeWhy are every refusal propose gives, with what it calls, by the
// text it refuses with (TestProposeWhyKept holds this to them, both
// ways), and what a bulk proposal makes of each: of the rule, counted apart
// (already, a proposal waiting for it; absent, not in effect; unreplayed,
// no backtest could check it; same, that for already) or as refused, the
// rest going on; or stop, the server's or the request's, which would be the
// same for every rule left: counted failed, and the rest not tried.
var proposeWhy = map[string]string{
	"this rule has a proposal not yet finished with: one at a time, so that each is seen applied": "already",
	"no rule in effect is called %q":                                                                                           "absent",
	"it isn't replayed (%s): no backtest could check a change to it":                                                           "unreplayed",
	"it isn't replayed and no history is tied to it: what it would catch is unknown, so retiring it isn't proposed":            "unreplayed",
	"Lapilli doesn't replay it, so it turns it off and goes no further: delete it in the console once it has been off a while": "unreplayed",
	"that is its for already": "same",

	"more than one rule in effect is called %q, alike in group and labels: change them where they are kept": "refused",
	"the rule changed since it was read: try again in a minute":                                             "refused",
	"a rule file has no off: retire it by deleting it":                                                      "refused",
	"it is off already": "refused",
	"it has been off since %s: four weeks aren't up":                                                                                                                     "refused",
	"it was turned off by hand, not by a proposal approved here: delete it in Grafana":                                                                                   "refused",
	"it was turned off by hand, not by a proposal approved here: delete it where it is kept":                                                                             "refused",
	"it was turned on again since it was turned off: it isn't deleted":                                                                                                   "refused",
	"a Grafana rule is turned off first, and deleted no sooner than four weeks later: propose turning it off":                                                            "refused",
	"a rule kept where it can be turned off is turned off first, and deleted no sooner than four weeks later: propose turning it off":                                    "refused",
	"it fired at another severity on %s, within twelve weeks, and that history is held apart from its own: retiring it rests on twelve weeks at the severity it has now": "refused",
	"its history is copied in only up to %s: retiring it rests on twelve weeks up to now, so copy its source's in again":                                                 "refused",
	"its history is copied in without a gap only from %s, less than twelve weeks: retiring it rests on twelve, so copy in what is missing":                               "refused",
	"its condition has no such number to change":                                                                                                                         "refused",
	"that is its threshold already":                                                                                                                 "refused",
	"the content check finds no %q for this rule":                                                                                                   "refused",
	"it says nothing there to take out":                                                                                                             "refused",
	"it says that already":                                                                                                                          "refused",
	"a Grafana rule's dashboard and panel are changed in Grafana":                                                                                   "refused",
	"what a rule says is changed in its summary, description, runbook_url or runbook":                                                               "refused",
	"a runbook is an https URL of 2,048 characters at most":                                                                                         "refused",
	"a summary or a description is 4 KB at most":                                                                                                    "refused",
	"give what it is to say, or take it out":                                                                                                        "refused",
	"what it says has a control character in it":                                                                                                    "refused",
	"what it says calls query, which would run PromQL each time the alert fires: Lapilli doesn't write one":                                         "refused",
	"what it says isn't a template that parses, and the rule's file would be refused for it: %s":                                                    "refused",
	"what it says sets a variable: Lapilli writes only labels, values, if, with and functions that don't make it grow":                              "refused",
	"what it says calls %s: Lapilli writes only labels, values, if, with and functions that don't make it grow":                                     "refused",
	"what it says formats with printf other than by a format written in it, with widths of 99 at most: Lapilli doesn't write one":                   "refused",
	"what it says applies %s to what another function gave: Lapilli writes it only on a label, a value or a text, so it can't grow again and again": "refused",

	"this lapilli serve reads no rules of its own: there is nothing to change":            "stop",
	"the rules haven't been read yet: try again in a minute":                              "stop",
	"nothing has been replayed yet to backtest it against: try again after the first run": "stop",
	"another comparison is running: try again in a minute":                                "stop",
	"a for of more than nothing and %s at most":                                           "stop",
	"threshold %q: want a number":                                                         "stop",
	"a proposal's kind is for, threshold, disable, delete or says":                        "stop",
	"a proposal names an incident by its ID, as the ledger has it":                        "stop",
	"the ledger has no incident %q":                                                       "stop",
}

// bulkPropose is the rules list's bulk proposal, for the rules picked:
// what=for, a new for for each, as its proposals tab's form proposes it;
// suggested, the for suggested for each, as its "Propose it"; retire, each
// retired as its proposals tab's form retires it, turned off where it can
// be and deleted where it can't, as propose finds it. Each proposal is the
// one that form makes, one at a time; what is refused for what the rule is
// is counted, and the rest go on; what would be the same for the rest (the
// hour's proposals made, an error that isn't the rule's, the time for the
// request up) stops it there, the rest counted so without being read. It
// goes back to the list it came from, which says once what was done, with
// a link to the proposals made.
func (s *Server) bulkPropose(w http.ResponseWriter, r *http.Request, rules []string) {
	from := r.PostFormValue("back")
	var res bulkResult
	switch {
	case len(rules) == 0:
		back(w, r, bulkBack(from, "/rules", "propose", res)) // nothing picked: nothing read
		return
	case len(rules) > maxPropose:
		s.oops(w, r, bad("at most %d rules at a time for proposals: pick fewer", maxPropose))
		return
	case s.role(r) < editor:
		// Proposing is an editor's, as POST /proposals is: a team's
		// editor's are refused, each, for that.
		res.Editors = len(rules)
		back(w, r, bulkBack(from, "/rules", "propose", res))
		return
	case !s.replaying():
		s.oops(w, r, bad("this lapilli serve reads no rules of its own: there is nothing to change"))
		return
	}
	what := r.PostFormValue("propose")
	var d time.Duration
	switch what {
	case "for":
		v := r.PostFormValue("for")
		x, err := ParseDuration(v)
		switch {
		case v == "" || err != nil:
			s.oops(w, r, bad("for %q: want a duration like 15m", v))
			return
		case x <= 0 || x > compareFor:
			s.oops(w, r, bad("a for of more than nothing and %s at most", verdict.Duration(compareFor)))
			return
		}
		d = x
	case "suggested", "retire":
	default:
		s.oops(w, r, bad("what to propose for the rules picked: a new for, the for suggested for each, or retiring each"))
		return
	}
	ctx := r.Context()
	start := time.Now()
	suggested, spare, err := s.offered(r, what, listWeeks(from))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	by := s.user(r)
	rc := http.NewResponseController(w)
	for i, rule := range rules {
		left := len(rules) - i
		if time.Since(start) >= proposeTime {
			res.Late = left
			break
		}
		// The answer is due after the last rule started: its wait for the
		// heavy turn, ending when the time for the request is up, and its
		// backtest, within --timeout.
		rc.SetWriteDeadline(time.Now().Add(proposeTime + s.cfg.Timeout + time.Minute))
		q := ask{Rule: rule, From: "person"}
		switch what {
		case "for":
			// The proposals tab's form says it comes from the suggestion,
			// when there is one, whatever for is typed.
			q.Kind, q.For = ledger.KindFor, d
			if _, ok := suggested[rule]; ok {
				q.From = "suggestion"
			}
		case "suggested":
			sf, ok := suggested[rule]
			x, err := ParseDuration(sf)
			if !ok || err != nil {
				res.NoSuggest++
				continue
			}
			q.Kind, q.For, q.From = ledger.KindFor, x, "suggestion"
		case "retire":
			q.Kind = kindRetire // turned off or deleted, as propose finds it
			if spare[rule] {
				q.From = "spare"
			}
		}
		id, err := s.proposeUntil(ctx, start.Add(proposeTime), q, by)
		if err == nil {
			res.Made = append(res.Made, id)
		}
		if errors.Is(err, errTimeUp) {
			res.Late = left
			break
		}
		if s.proposeCount(&res, r, err, left) {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		// A proxy's timeout, the reader gone, the server stopping: what
		// was made is kept, and no one is told but here.
		s.log.Warn("a proposal for the rules picked stopped: its request ended", "made", res.Done, "failed", res.Failed,
			"not tried", res.Untried, "not tried, time up", res.Late, "err", err)
	}
	back(w, r, bulkBack(from, "/rules", "propose", res))
}

// listWeeks are the weeks the rules list a bulk form came from shows (back,
// its address): 1, 4 or 12, and the list's own four unless it says. A rule's
// page opened from the list is over them.
func listWeeks(back string) int {
	u, err := url.Parse(back)
	if err != nil {
		return periodOf["rules"]
	}
	switch u.Query().Get("weeks") {
	case "1":
		return 1
	case "12":
		return weeks
	}
	return periodOf["rules"]
}

// offered is what each rule's page offers a bulk proposal of what: the for
// suggested for it, and, to retire it, whether it is spare over the weeks
// the list showed, as its page opened from the list says. Worked out under
// a page's turn, as the page is; given back before any proposal waits for
// the heavy turn.
func (s *Server) offered(r *http.Request, what string, n int) (suggested map[string]string, spare map[string]bool, err error) {
	tn, err := s.renderTurn(r.Context())
	if err != nil {
		return nil, nil, err
	}
	defer tn.give()
	if suggested, err = s.suggestedFors(r.Context()); err != nil {
		return nil, nil, err
	}
	spare = map[string]bool{}
	if what == "retire" {
		names, err := s.spareOver(r.Context(), n)
		if err != nil {
			return nil, nil, err
		}
		for _, x := range names {
			spare[x] = true
		}
	}
	return suggested, spare, nil
}

// errTimeUp is a bulk proposal's rule that waited for the heavy turn until
// the time for its request was up: it, and those after it, not tried.
var errTimeUp = errors.New("the time for one request is up")

// proposeUntil is a proposal as a rule's page's form makes it (proposeOne),
// for a bulk proposal: its wait for the heavy turn ends at until, errTimeUp.
func (s *Server) proposeUntil(ctx context.Context, until time.Time, q ask, by string) (int64, error) {
	if err := s.mayPropose(ctx, q.Rule, by, false); err != nil {
		return 0, err // refused without waiting for the heavy turn
	}
	wait, cancel := context.WithDeadline(ctx, until)
	defer cancel()
	done, err := s.busy(wait)
	switch {
	case err != nil && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		return 0, errTimeUp
	case err != nil:
		return 0, err
	}
	defer done()
	return s.propose(ctx, q, by, false, false)
}

// proposeOne is a proposal as a rule's page's form makes it (POST
// /proposals): under the heavy turn, waited for as a page waits for it,
// holding no turn to be worked out meanwhile, and given back once it is
// made.
func (s *Server) proposeOne(ctx context.Context, q ask, by string) (int64, error) {
	if err := s.mayPropose(ctx, q.Rule, by, false); err != nil {
		return 0, err // refused without waiting for the heavy turn
	}
	done, err := s.busy(ctx)
	if err != nil {
		return 0, err
	}
	defer done()
	return s.propose(ctx, q, by, false, false)
}

// suggestedFors are the fors the digest's suggestions give, by rule, as a
// rule's page offers them: written as its form sends them.
func (s *Server) suggestedFors(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	rs, err := s.l.Results(ctx, "suggestions", 1)
	if err != nil || len(rs) == 0 {
		return out, err
	}
	var ss []verdict.Suggestion
	if err := rs[0].Decode(&ss); err != nil {
		return nil, err
	}
	for _, x := range ss {
		out[x.Rule] = verdict.Duration(x.NewFor)
	}
	return out, nil
}

// proposeCount adds a proposal's outcome to a bulk proposal's result, of
// left rules this one among them: made; refused for what the rule is, by
// why (proposeWhy); refused for the reader, the hour's proposals made or
// tries asked for, which the rest would be too: they are counted so, not
// read, and stop reports true; or failed, the server's or the request's
// (proposeWhy's stop), or an error that isn't the rule's (the heavy turn
// waited for too long, a database that stopped answering), which would be
// the same for the rest: logged once, as an error page logs it, unless it
// is the server's being busy or not ready, or the reader's leaving, and
// stop reports true.
func (s *Server) proposeCount(b *bulkResult, r *http.Request, err error, left int) (stop bool) {
	var br badRequest
	var inv ledger.ErrInvalid
	why := ""
	switch {
	case err == nil:
		b.Done++
		return false
	case errors.Is(err, ledger.ErrProposed):
		why = "already"
	case errors.Is(err, ledger.ErrTooManyProposals):
		b.Limit += left
		return true
	case errors.Is(err, errTooManyTries):
		b.Tries += left
		return true
	case errors.As(err, &br):
		why = cmpOr(proposeWhy[br.format], "refused")
	case errors.As(err, &inv), errors.Is(err, ledger.ErrNotFound):
		why = "refused"
	}
	switch why {
	case "already":
		b.Already++
	case "absent":
		b.Absent++
	case "unreplayed":
		b.Unreplayed++
	case "same":
		b.Same++
	case "refused":
		b.Refused++
	default:
		b.Failed++
		b.Untried += left - 1
		if why == "" && !errors.Is(err, context.Canceled) && !errors.Is(err, errBusy) {
			s.log.Error("serving", "path", r.URL.Path, "err", err)
		}
		return true
	}
	return false
}

// madeBy is a bulk proposal's result as its reader may be told it: of the
// proposals made= names, those that are theirs, made in Lapilli, not by an
// agent; and proposed, how many those are. Any other is left out, as is
// what can't be read: a link names no one else's as the reader's. ok is
// false when nothing is left to say of what the address claimed.
func (s *Server) madeBy(r *http.Request, b *bulkResult) (ok bool) {
	me := ledger.FoldName(s.user(r))
	var own []int64
	for _, id := range b.Made {
		if p, err := s.l.ProposalByID(r.Context(), id); err == nil && ledger.FoldName(p.MadeBy) == me && !p.Agent && !slices.Contains(own, id) {
			own = append(own, id)
		}
	}
	claimed := b.Done > 0 || len(b.Made) > 0
	b.Made, b.Done = own, len(own)
	b.None = b.Done+b.Theirs+b.Refused+b.Already+b.Failed+b.Untried+b.Editors+b.Limit+b.Tries+b.Unreplayed+b.Absent+b.Same+b.NoSuggest+b.Late == 0
	return !(b.None && claimed)
}

// proposalsMade is the proposals list of the proposals a bulk proposal
// made, in every state, for its result's link.
func proposalsMade(ids []int64) string {
	q := url.Values{"state": {"all"}}
	for _, id := range ids {
		q.Add("id", strconv.FormatInt(id, 10))
	}
	return "/proposals?" + q.Encode()
}
