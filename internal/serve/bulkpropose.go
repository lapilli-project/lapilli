package serve

import (
	"context"
	"errors"
	"net/http"
	"net/url"
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

// proposeWhy are the refusals a bulk proposal counts apart, by the text
// propose refuses with (TestProposeWhyKept holds each to it): one waiting
// for the rule already, the hour's proposals made, a rule not in effect, one
// not replayed, so that no backtest could check it, a for it has already.
// Any other refusal for what the rule is counts as refused. Two are the
// server's, not the rule's, and would be the same for the rest: failed.
var proposeWhy = map[string]string{
	"this rule has a proposal not yet finished with: one at a time, so that each is seen applied":         "already",
	"you have made %d proposals in the last hour, with your agents, the most anyone may: try again later": "limit",
	"no rule in effect is called %q":                                                                                           "absent",
	"it isn't replayed (%s): no backtest could check a change to it":                                                           "unreplayed",
	"it isn't replayed and no history is tied to it: what it would catch is unknown, so retiring it isn't proposed":            "unreplayed",
	"Lapilli doesn't replay it, so it turns it off and goes no further: delete it in the console once it has been off a while": "unreplayed",
	"that is its for already":                                "same",
	"another comparison is running: try again in a minute":   "failed",
	"the rules haven't been read yet: try again in a minute": "failed",
}

// bulkPropose is the rules list's bulk proposal, for the rules picked:
// what=for, a new for for each, as its proposals tab's form proposes it;
// suggested, the for suggested for each, as its "Propose it"; retire, each
// retired as its proposals tab's form retires it, turned off where it can
// be and deleted where it can't. Each proposal is the one that form makes,
// one at a time; what is refused for what the rule is is counted, and the
// rest go on; an error of another kind stops it there. It goes back to the
// list it came from, which says once what was done, with a link to the
// proposals made.
func (s *Server) bulkPropose(w http.ResponseWriter, r *http.Request, rules []string) {
	from := r.PostFormValue("back")
	if len(rules) > maxPropose {
		s.oops(w, r, bad("at most %d rules at a time for proposals: pick fewer", maxPropose))
		return
	}
	var res bulkResult
	if s.role(r) < editor {
		// Proposing is an editor's, as POST /proposals is: a team's
		// editor's are refused, each, for that.
		res.Editors = len(rules)
		back(w, r, bulkBack(from, "/rules", "propose", res))
		return
	}
	if !s.replaying() {
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
	// What each rule's page offers, read once: the for suggested for it,
	// and, to retire it, whether it is spare.
	suggested, err := s.suggestedFors(ctx)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	spare := map[string]bool{}
	if what == "retire" {
		names, err := s.spareRules(ctx)
		if err != nil {
			s.oops(w, r, err)
			return
		}
		for _, n := range names {
			spare[n] = true
		}
	}
	by := s.user(r)
	rc := http.NewResponseController(w)
	for i, rule := range rules {
		// Each has its wait for the heavy turn and its backtest's time
		// before the answer is due: twenty may take longer than the
		// server's two minutes to write it, and the reader would see an
		// error for what was made.
		rc.SetWriteDeadline(time.Now().Add(busyWait + s.cfg.Timeout + time.Minute))
		q := ask{Rule: rule, From: "person"}
		var err error
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
			if !ok {
				res.NoSuggest++
				continue
			}
			q.Kind, q.From = ledger.KindFor, "suggestion"
			q.For, err = ParseDuration(sf)
		case "retire":
			var f found
			if f, err = s.current(ctx, rule, false); err == nil {
				q.Kind = ledger.KindDelete
				if s.offable(f) && !isOff(f.rule) {
					q.Kind = ledger.KindDisable // off first; deleted once off four weeks
				}
				if spare[rule] {
					q.From = "spare"
				}
			}
		}
		if err == nil {
			var id int64
			if id, err = s.proposeOne(ctx, q, by); err == nil {
				res.Made = append(res.Made, id)
			}
		}
		if s.proposeCount(&res, r, err) {
			res.Untried = len(rules) - i - 1
			break
		}
	}
	back(w, r, bulkBack(from, "/rules", "propose", res))
}

// proposeOne is a proposal as a rule's page's form makes it (POST
// /proposals): under the heavy turn, waited for as a page waits for it,
// holding no turn to be worked out meanwhile, and given back once it is
// made.
func (s *Server) proposeOne(ctx context.Context, q ask, by string) (int64, error) {
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

// proposeCount adds a proposal's outcome to a bulk proposal's result: made;
// refused for what the rule is, by why (proposeWhy); or failed, an error
// that isn't the rule's (the heavy turn waited for too long, a comparison
// running, a database that stopped answering), which would be the same for
// the rest: logged once, as an error page logs it, unless it is the
// server's being busy or the reader's leaving, and stop reports true.
func (s *Server) proposeCount(b *bulkResult, r *http.Request, err error) (stop bool) {
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
		why = "limit"
	case errors.As(err, &br):
		why = cmpOr(proposeWhy[br.format], "refused")
	case errors.As(err, &inv), errors.Is(err, ledger.ErrNotFound):
		why = "refused"
	}
	switch why {
	case "already":
		b.Already++
	case "limit":
		b.Limit++
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
		if why == "" && !errors.Is(err, context.Canceled) && !errors.Is(err, errBusy) {
			s.log.Error("serving", "path", r.URL.Path, "err", err)
		}
		return true
	}
	return false
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
