package serve

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/scenario"
	"lapilli.dev/internal/source"
	"lapilli.dev/internal/verdict"
)

// Drafts of expectations (docs/design/expectations.md, step 3): from the
// history, each rule's most in a week and its catches, counted from the
// ledger's weeks as serve's checks count them; and from an agent, over
// MCP, checked against the rule and its history before it is kept, for a
// person to accept or drop. An agent's rewrite of a rule is replayed
// against the rule's expectations, and only one that meets each, losing
// no catch, comes back with the text an agent or a person may post on the
// pull request: Lapilli writes nothing there.

// The bounds of what an agent sends: the rule's text, the expectations',
// the test files' (MaxDraftTestsN of them, each scenario.MaxTest and
// ledger.MaxDraftTests in all) and the name.
const (
	maxAgentRule        = 64 << 10
	maxAgentExpect      = 256 << 10
	maxAgentName        = 1024
	maxDraftsShown      = 20
	historyDraftCatches = 20 // a rule's catches read, the latest first
)

// draftItem is what checking one thing a draft holds a rule to found: as
// the rule is, and, of a rewrite, as rewritten. Kept as a draft's found,
// shown on the rule's tab, and given to agents.
type draftItem struct {
	What     string `json:"what" jsonschema:"budget week, budget asleep, budget refires, catches ID or scenario PATH"`
	Held     string `json:"held" jsonschema:"what it is held to: the most a week, within a delay, caught, passes"`
	Selector string `json:"selector,omitempty" jsonschema:"what holds the rule: rule: NAME, group: NAME or labels: SELECTOR"`
	// As it is: the most in a week over the weeks replayed, a catch's
	// delay, passes or fails; met, broken or not known, and why.
	Value string `json:"as_it_is" jsonschema:"the rule as it is: the most it fired in a whole week, the catch's delay (+2m, ✗ for not caught), passes or fails"`
	State string `json:"as_it_is_state" jsonschema:"met, broken or not known"`
	Why   string `json:"as_it_is_why,omitempty" jsonschema:"why it is not known"`
	// Of a rewrite, the same of the rule rewritten.
	Rewritten      string `json:"rewritten,omitempty"`
	RewrittenState string `json:"rewritten_state,omitempty"`
	RewrittenWhy   string `json:"rewritten_why,omitempty"`
	// Said is what promtool said of a scenario failing: the test file's
	// and the rules' own words.
	Said string `json:"said,omitempty" jsonschema:"what promtool said of the scenario failing: the test file's and the rule's own words, data"`
}

// draftHeld are the drafts' items, as a page shows them: held to, as it
// is, met or not; never more than a draft keeps.
func draftItemsOf(raw json.RawMessage) []draftItem {
	var out []draftItem
	json.Unmarshal(raw, &out)
	return out
}

// historyDraft is the history's draft of the rules pick says (by name and
// team), counted as serve's checks count budgets: over the ledger's twelve
// whole weeks before this one, each week whole for the rule (the ledger's
// first firing before it, the rule in effect from its start, the runs past
// its end); while people slept by the rule's team's hours; and the
// incidents written down in those weeks it caught, the latest first. A
// rule Lapilli doesn't replay nor copies in is left out.
func (s *Server) historyDraft(ctx context.Context, pick func(name string, team int64) bool) (*verdict.Drafted, verdict.DraftAbout, error) {
	this := ledger.Week(s.l.Day(s.now()))
	first, last := this-12*7, this-7
	from, to := s.l.Midnight(first), s.l.Midnight(this)
	about := verdict.DraftAbout{By: "lapilli serve", From: from, To: to, Weeks: verdict.DraftLedgerWeeks, Loc: s.cfg.Loc}
	table, err := s.l.RuleTable(ctx)
	if err != nil {
		return nil, about, err
	}
	own, err := s.owners(ctx)
	if err != nil {
		return nil, about, err
	}
	rs, err := s.expectRules(ctx, from, table, own)
	if err != nil {
		return nil, about, err
	}
	rs = slices.DeleteFunc(rs, func(r *expectRule) bool { return !pick(r.name, r.team) })
	o, err := s.options(ctx)
	if err != nil {
		return nil, about, err
	}
	t := &tallies{by: map[int64]map[int64]ledger.Tally{}, o: o, rules: table}
	for w := first; w <= last; w += 7 {
		t.weeks = append(t.weeks, w)
		t.by[w] = map[int64]ledger.Tally{}
	}
	ws, err := s.l.Weeks(ctx, first, last)
	if err != nil {
		return nil, about, err
	}
	for _, x := range ws {
		if t.by[x.At] != nil {
			t.by[x.At][x.Rule] = x
		}
	}
	hours, err := s.byHours(ctx, t)
	if err != nil {
		return nil, about, err
	}
	sc := own.schedules(o.Hours)
	firstFiring, _, err := s.l.Bounds(ctx)
	if err != nil {
		return nil, about, err
	}
	upTo := s.now()
	if s.running(ctx) {
		if upTo, err = s.l.Cursor(ctx); err != nil {
			return nil, about, err
		}
	}
	incs, err := s.l.Incidents(ctx, from, to, false)
	if err != nil {
		return nil, about, err
	}
	slices.SortStableFunc(incs, func(a, b ledger.Incident) int { return b.Start.Compare(a.Start) })
	caught := map[string][]verdict.DraftCatch{}
	for _, in := range incs {
		seen := map[string]bool{}
		for _, c := range in.CaughtBy {
			if !seen[c.Rule] && len(caught[c.Rule]) < historyDraftCatches {
				seen[c.Rule] = true
				caught[c.Rule] = append(caught[c.Rule], verdict.DraftCatch{Incident: in.ID, Delay: c.Delay})
			}
		}
	}
	var drs []verdict.DraftRule
	var unreplayed []verdict.DraftLeftOut
	for _, r := range rs {
		if r.unreplayed {
			unreplayed = append(unreplayed, verdict.DraftLeftOut{Rule: r.name, Why: verdict.DraftUnreplayed})
			continue
		}
		h := sc.of(r.name)
		d := verdict.DraftRule{Rule: r.name, Sleeps: h != nil && h.Sleeps(), Caught: caught[r.name]}
		for _, w := range t.weeks {
			wFrom, wTo := s.l.Midnight(w), s.l.Midnight(w+7)
			if firstFiring.IsZero() || !firstFiring.Before(wFrom) || !r.from.IsZero() && r.from.After(wFrom) || upTo.Before(wTo) {
				continue // not whole for it
			}
			x := t.by[w][r.id]
			d.Weeks++
			d.Most, d.Refires = max(d.Most, x.Firings), max(d.Refires, x.Refired)
			d.Asleep = max(d.Asleep, hours[w][r.id].Asleep)
		}
		drs = append(drs, d)
	}
	out := verdict.Draft(drs)
	out.Left = append(out.Left, unreplayed...)
	return out, about, nil
}

// historyItems are what a history's draft found, as a draft keeps it:
// each budget met by the most it is the rounding up of, each catch by its
// delay.
func historyItems(x verdict.Expectation, r verdict.DraftRule) []draftItem {
	var out []draftItem
	for _, it := range x.Items() {
		d := draftItem{What: it.What, Held: it.Held, Selector: x.Selector(), State: verdict.ExpectMet}
		switch {
		case it.Catch != nil:
			for _, c := range r.Caught {
				if c.Incident == it.Catch.Incident {
					d.Value = verdict.Delay(c.Delay)
				}
			}
		case it.Asleep():
			d.Value = fmt.Sprint(r.Asleep)
		case it.Refires():
			d.Value = fmt.Sprint(r.Refires)
		default:
			d.Value = fmt.Sprint(r.Most)
		}
		out = append(out, d)
	}
	return out
}

// draftTarget is where accepting a draft goes, as serve is started: the
// expectations file (--expect), with the hash of what was last read of it,
// which the git path finds it by; serve's configuration (--config); or
// nowhere serve knows.
func (s *Server) draftTarget() (string, string) {
	switch {
	case s.cfg.Expect != "":
		s.expect.Lock()
		hash := s.expect.applied
		s.expect.Unlock()
		return ledger.DraftToFile, hash
	case s.cfg.Config != "":
		return ledger.DraftToConfig, ""
	}
	return ledger.DraftToNone, ""
}

// takenIn is a draft accepted, as a person puts it where the expectations
// are declared: the expectations file with it taken in (verdict.TakeIn),
// read now, with the hash of the file as read, or why it can't be; for
// serve's configuration or none, its own entries, to add under
// expectations:.
func (s *Server) takenIn(d ledger.ExpectDraft) (text, hash string, whole bool, err error) {
	entries, err := verdict.DraftEntries(d.Text, d.Rule)
	if err != nil {
		return "", "", false, err
	}
	if d.Target != ledger.DraftToFile || s.cfg.Expect == "" {
		b, err := verdict.ExpectationsYAML(entries)
		return string(b), "", false, err
	}
	file, err := readBounded(s.cfg.Expect, verdict.MaxExpectations)
	if err != nil {
		return "", "", false, fmt.Errorf("the expectations file can't be read: %w", err)
	}
	b, err := verdict.TakeIn(file, entries)
	return string(b), docHashOf(file), true, err
}

// gitPath says whether a draft accepted goes by the git path: kept to the
// expectations file, with changes on and the recipes' token given, and no
// scenario, whose test file a pull request to one file can't add.
func (s *Server) gitPath(d ledger.ExpectDraft) bool {
	return d.Target == ledger.DraftToFile && s.cfg.Expect != "" && s.cfg.Changes && s.cfg.ChangesToken != "" && len(d.Tests) == 0 &&
		!strings.Contains(d.Text, "scenarios:")
}

// draftFormKey is what a draft's accept form carries: good for one draft
// as it stood when the page was shown, for one person.
func (s *Server) draftFormKey(d ledger.ExpectDraft, user string) string {
	m := hmac.New(sha256.New, s.changes.key)
	fmt.Fprintf(m, "draft\x00%d\x00%s\x00%s", d.ID, d.State, user)
	return hex.EncodeToString(m.Sum(nil))
}

func (s *Server) draftFormKeyOK(d ledger.ExpectDraft, user, got string) bool {
	return got != "" && hmac.Equal([]byte(got), []byte(s.draftFormKey(d, user)))
}

// MCP: test_expectation and propose_expectation.

type testExpectIn struct {
	Rule         string            `json:"rule" jsonschema:"the rule's name, as rules gives it"`
	Expectations string            `json:"expectations,omitempty" jsonschema:"an expectations file (expectations.yaml, as lapilli verdict --expect reads it), such as a pull request's: its entries that hold the rule are checked; serve's own for the rule when not given"`
	Rewrite      string            `json:"rewrite,omitempty" jsonschema:"the rule rewritten, as a Prometheus rule file holding it alone: its name and group as rule gives them, any other expression, for, threshold or labels; replayed beside it as it is"`
	Tests        map[string]string `json:"tests,omitempty" jsonschema:"promtool's test files the given expectations' scenarios name, by their path: 20 at most, 256 KiB in all"`
}

type proposeExpectIn struct {
	Rule         string            `json:"rule" jsonschema:"the rule's name, as rules gives it"`
	Expectations string            `json:"expectations" jsonschema:"what the rule is to be held to, as an expectations file holding it by rule: NAME alone: budget (week, asleep, refires), catches (incident, within), scenarios"`
	Tests        map[string]string `json:"tests,omitempty" jsonschema:"promtool's test files its scenarios name, by their path: 20 at most, 256 KiB in all"`
	Reason       string            `json:"reason,omitempty" jsonschema:"why, in a few sentences: shown as your words"`
}

type testExpectOut struct {
	Rule string    `json:"rule"`
	From time.Time `json:"from" jsonschema:"the period replayed: the twelve weeks to now, or since the runs began"`
	To   time.Time `json:"to"`
	// Weeks are the whole weeks budgets are held over, counted back from To.
	Weeks int `json:"weeks" jsonschema:"the whole weeks budgets are held over, counted back from to, the most in one given"`
	// HeldBy says whose expectations: given (the text given) or serve's.
	HeldBy string      `json:"held_by" jsonschema:"given: the expectations text given; serve: serve's own, its expectations file, its configuration and the rule's annotation"`
	Items  []draftItem `json:"items" jsonschema:"each thing the rule is held to, met, broken or not known (never met), as it is and, of a rewrite, rewritten"`
	// Firings and caught, as it is and rewritten.
	Fired  [2]int `json:"fired" jsonschema:"its firings over the period, as it is and rewritten (as it is twice without a rewrite)"`
	Caught [2]int `json:"caught" jsonschema:"its firings that caught an incident, as it is and rewritten"`
	// Lost are incidents it catches as it is that the rewrite doesn't.
	Lost       []string `json:"lost" jsonschema:"incidents the rule catches as it is that the rewrite misses: a rewrite that loses one is never given a post"`
	Unfinished bool     `json:"not_finished,omitempty" jsonschema:"the replay ran out of time: its numbers are partial, and not known"`
	// Meets says the rewrite meets each, losing no catch; Post, then, is
	// the text to post on the pull request.
	Meets bool   `json:"meets" jsonschema:"of a rewrite: the rule as it is and rewritten each replayed to the end, held to one expectation at least, it meets each (not known is never met) and loses no incident the rule catches"`
	Post  string `json:"post,omitempty" jsonschema:"of a rewrite that meets each: Markdown you or a person may post on the pull request, marked as an agent's rewrite backtested by Lapilli, with the numbers before and after. Lapilli posts nothing itself. Not given otherwise"`
	Note  string `json:"note,omitempty"`
}

type proposeExpectOut struct {
	ID    int64       `json:"id"`
	Page  string      `json:"page,omitempty" jsonschema:"the rule's expectations tab, where a person accepts or drops it"`
	Items []draftItem `json:"items" jsonschema:"what checking it against the rule as it is found, item by item: met, broken or not known"`
	Note  string      `json:"note,omitempty"`
}

// checked is what checking expectations against a rule found.
type checked struct {
	out     testExpectOut
	entries []verdict.Expectation // the rule's, as given
	tests   map[string]string
	// sides are the rule replayed as it is and, of a rewrite, rewritten
	// (as it is twice otherwise); nil where it wasn't.
	sides [2]*verdict.RuleResult
}

// cleanTests are the test files an agent sent, each by a path an
// expectations file may name, bounded, and read as promtool would expand
// it (scenario.Bound) before it is run: what it would make promtool hold
// in serve's memory is refused past the bounds.
func cleanTests(tests map[string]string) (map[string][]byte, error) {
	if len(tests) > ledger.MaxDraftTestsN {
		return nil, fmt.Errorf("%d test files at most", ledger.MaxDraftTestsN)
	}
	out := map[string][]byte{}
	n := 0
	for _, p := range slices.Sorted(maps.Keys(tests)) {
		t := tests[p]
		if err := verdict.ScenarioPath(p); err != nil {
			return nil, err
		}
		if n += len(p) + len(t); n > ledger.MaxDraftTests || len(t) > scenario.MaxTest {
			return nil, fmt.Errorf("the test files are %d bytes in all at most", ledger.MaxDraftTests)
		}
		if err := scenario.Bound([]byte(t)); err != nil {
			return nil, fmt.Errorf("test file %s: %w", verdict.Clean(p), err)
		}
		b, err := scenario.Rewrite([]byte(t))
		if err != nil {
			return nil, fmt.Errorf("test file %s: %w", verdict.Clean(p), err)
		}
		out[p] = b
	}
	return out, nil
}

// agentText is text an agent wrote, as it is read: valid UTF-8, n bytes at
// most.
func agentText(what, v string, n int) error {
	if len(v) > n || !utf8.ValidString(v) {
		return fmt.Errorf("%s: %d bytes of UTF-8 at most", what, n)
	}
	return nil
}

// checkHeld checks what a rule is held to against it and its history, as
// test_expectation and propose_expectation do: the expectations given (or
// serve's own for the rule), against the rule replayed over the twelve
// weeks to now, beside its rewrite when one is given; each scenario run by
// --promtool, an agent's test file bounded first and run one at a time.
// One comparison runs at a time, as compare's: it replays history with
// Lapilli's access.
func (s *Server) checkHeld(ctx context.Context, rule, text, rewrite string, tests map[string]string, ownOnly bool) (*checked, error) {
	switch {
	case s.cfg.Alerts:
		return nil, errors.New("this lapilli serve judges the ALERTS series, not rules: there are no rules to replay")
	case !s.replaying():
		return nil, errors.New("this lapilli serve judges firings copied in from elsewhere, with no rules of its own: there are no rules to replay")
	case rule == "" || len(rule) > maxAgentName || !utf8.ValidString(rule):
		return nil, fmt.Errorf("give the rule's name, as rules gives it, %d bytes at most", maxAgentName)
	}
	if err := errors.Join(agentText("expectations", text, maxAgentExpect), agentText("rewrite", rewrite, maxAgentRule)); err != nil {
		return nil, err
	}
	given, err := cleanTests(tests)
	if err != nil {
		return nil, err
	}
	if len(given) > 0 && strings.TrimSpace(text) == "" {
		return nil, errors.New("tests are the files the expectations given name: give the expectations too")
	}
	latest, ok, err := s.l.Latest(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("the rules haven't been read yet: try again in a minute")
	}
	current, err := s.parse(ctx, latest)
	if err != nil {
		return nil, err
	}
	at := slices.IndexFunc(current, func(r rules.Rule) bool { return r.Name == rule })
	if at < 0 {
		return nil, fmt.Errorf("no rule in effect is called %q; rules lists them", rule)
	}
	r := current[at]
	if r.Unsupported != "" {
		return nil, fmt.Errorf("%s can't be replayed: %s", verdict.Clean(rule), r.Unsupported)
	}
	c := &checked{out: testExpectOut{Rule: rule, Items: []draftItem{}, Lost: []string{}}, tests: tests}
	// What it is held to: the entries given that hold it, or serve's.
	var held *verdict.Expectations
	labels := r.AlertLabels()
	if strings.TrimSpace(text) != "" {
		e, err := verdict.ReadExpectations(strings.NewReader(text))
		if err != nil {
			return nil, fmt.Errorf("the expectations given: %w", err)
		}
		var list []verdict.Expectation
		for _, x := range e.Expectations {
			if ownOnly && x.Rule != rule {
				return nil, fmt.Errorf("a draft holds %s by rule: %s alone, not %s", verdict.Clean(rule), verdict.Clean(rule), verdict.Clean(x.Selector()))
			}
			if x.Holds(rule, r.Group, labels) {
				list = append(list, x)
			}
		}
		if len(list) == 0 {
			return nil, fmt.Errorf("the expectations given hold no rule called %q", rule)
		}
		named := map[string]bool{}
		for _, x := range list {
			for _, p := range x.Scenarios {
				named[p] = true
				if _, ok := given[p]; !ok && s.cfg.Promtool != "" {
					return nil, fmt.Errorf("scenario %s: give its test file in tests", verdict.Clean(p))
				}
			}
		}
		for p := range given {
			if !named[p] {
				return nil, fmt.Errorf("test file %s: no scenario the expectations given hold the rule to names it", verdict.Clean(p))
			}
		}
		if held, err = verdict.NewExpectations(list); err != nil {
			return nil, err
		}
		c.entries, c.out.HeldBy = held.Expectations, "given"
	} else {
		if held, given, err = s.servesHeld(r); err != nil {
			return nil, err
		}
		c.out.HeldBy = "serve"
	}
	var gone, came []rules.Rule
	if strings.TrimSpace(rewrite) != "" {
		w, err := rewritten(current, r, rewrite)
		if err != nil {
			return nil, err
		}
		gone, came = []rules.Rule{r}, []rules.Rule{w}
	} else {
		came = []rules.Rule{r}
	}
	runner := func(ctx context.Context) (verdict.ScenarioRunner, error) {
		return s.draftRunner(ctx, r, came[0], given, len(gone) > 0, c.out.HeldBy == "given")
	}
	results, err := s.replayHeld(ctx, c, held, gone, came, len(current)-1, runner, false)
	if err != nil {
		return nil, err
	}
	if len(gone) == 0 {
		return c, nil
	}
	c.out.Meets = verdict.RewriteMeets(results, c.out.Lost, c.sides[0], c.sides[1])
	if c.out.Meets {
		c.out.Post = verdict.RewriteMarkdown(verdict.Rewritten{Rule: rule, From: c.out.From, To: c.out.To, Fired: c.out.Fired, Caught: c.out.Caught,
			Results: results}, rules.Diff(rule, []byte(ruleText(r)), []byte(ruleText(came[0]))))
	}
	switch why := unreplayed(c.sides); {
	case len(results) == 0:
	case why != "":
		// A side cut short, or that couldn't be replayed, catches nothing
		// it would have: a catch the rewrite loses would go unseen.
		c.out.Note = strings.TrimSpace(c.out.Note + " There is nothing to post: " + why +
			", so whether the rewrite meets each expectation and loses no catch the rule makes is not known.")
	case !c.out.Meets:
		c.out.Note = "The rewrite doesn't meet each expectation, or loses a catch: there is nothing to post."
	}
	return c, nil
}

// unreplayed says which side of a rewrite's check wasn't replayed to the
// end, the rule as it is or rewritten, and why; "" when both were.
func unreplayed(sides [2]*verdict.RuleResult) string {
	for i, x := range sides {
		which := [2]string{"the rule as it is", "the rewrite"}[i]
		switch {
		case x == nil:
			return which + " wasn't replayed"
		case x.Unsupported == replay.Unfinished:
			return which + " wasn't replayed to the end (out of time)"
		case x.Unsupported != "":
			return which + " couldn't be replayed (" + x.Unsupported + ")"
		}
	}
	return ""
}

// replayHeld replays the rules came (beside gone, the rule as it is, of a
// rewrite; nothing, of a rule added) over the twelve weeks to now, or since
// the runs began, its firings kept, and holds them to held with
// CheckExpectationsBy, each scenario run by the runner made for the
// replay: what test_expectation, propose_expectation and a proposal adding
// a rule are each checked by. It fills c's period, weeks, items, firings
// and catches (of gone and came), the incidents lost, and its note when
// the replay didn't finish or nothing holds the rule. One comparison runs
// at a time, as compare's: it replays history with Lapilli's access, the
// whole check within the time a comparison has. wait says to wait for a
// comparison running, as a job does (an approved add carried out, as a
// change's backtest is); else one asked for meanwhile is refused
// (comparingBusy), to be asked again.
func (s *Server) replayHeld(ctx context.Context, c *checked, held *verdict.Expectations, gone, came []rules.Rule, same int,
	runner func(context.Context) (verdict.ScenarioRunner, error), wait bool) ([]verdict.ExpectResult, error) {
	rule := c.out.Rule
	if wait {
		s.comparing.Lock()
	} else if !s.comparing.TryLock() {
		return nil, bad(comparingBusy)
	}
	defer s.comparing.Unlock()
	// The whole check, its replay and its scenarios, within the time a
	// comparison has: a scenario past it is stopped, and not known.
	within := min(s.cfg.Timeout, compareTime)
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	c.out.To = s.now().Add(-s.cfg.Settle).Truncate(s.cfg.Step).UTC()
	c.out.From = c.out.To.Add(-weeks * weekLen)
	if began, err := s.l.RunsFrom(ctx); err != nil {
		return nil, err
	} else if began.After(c.out.From) {
		c.out.From = began.UTC()
	}
	if !c.out.From.Before(c.out.To) {
		return nil, errors.New("nothing has been replayed yet to check it against: try again after the first run")
	}
	o, err := s.options(ctx)
	if err != nil {
		return nil, err
	}
	// Its firings kept, as the backtest port's are: a budget counts them
	// by week.
	cmp, err := s.replayCompared(ctx, s.h, s.cfg.Parallel, gone, came, same, c.out.From, c.out.To, within, o)
	if err != nil {
		return nil, err
	}
	hours := func(string) *verdict.Hours { return o.Hours }
	if own, err := s.owners(ctx); err == nil {
		hours = own.schedules(o.Hours).of
	}
	run, err := runner(ctx)
	if err != nil {
		return nil, err
	}
	cmp.Scenarios = run
	k, err := verdict.CheckExpectationsBy(cmp, held, nil, hours)
	if err != nil {
		return nil, err
	}
	c.out.Weeks = k.Weeks
	selector := map[string]string{}
	for _, x := range held.Expectations {
		for _, it := range x.Items() {
			if _, ok := selector[it.What]; !ok {
				selector[it.What] = x.Selector()
			}
		}
	}
	var before, after *verdict.RuleResult
	for _, rc := range cmp.Rules {
		if rc.Rule == rule {
			before, after = rc.Before, rc.After
		}
	}
	if len(gone) == 0 {
		before = after
	}
	c.sides = [2]*verdict.RuleResult{before, after}
	for _, x := range []*verdict.RuleResult{before, after} {
		if x != nil && x.Unsupported == replay.Unfinished {
			c.out.Unfinished = true
		}
	}
	for i, x := range []*verdict.RuleResult{before, after} {
		if x != nil {
			c.out.Fired[i], c.out.Caught[i] = x.Firings, x.White
		}
	}
	inPeriod := map[string]bool{}
	for _, in := range cmp.After.Incidents {
		inPeriod[in.ID] = true
	}
	sleeps := hours(rule) != nil && hours(rule).Sleeps()
	why := func(res verdict.ExpectResult, x *verdict.RuleResult, scenarioWhy string) string {
		switch {
		case scenarioWhy != "":
			return scenarioWhy
		case x != nil && x.Unsupported == replay.Unfinished:
			return "the replay didn't finish in time"
		case x != nil && x.Unsupported != "":
			return "it couldn't be replayed: " + x.Unsupported
		case res.What == "budget asleep" && !sleeps:
			return "no one's hours say when people sleep"
		case strings.HasPrefix(res.What, "catches ") && !inPeriod[strings.TrimPrefix(res.What, "catches ")]:
			return "the incident isn't written down in the period replayed"
		}
		return ""
	}
	results := k.Results
	for _, res := range results {
		it := draftItem{What: res.What, Held: res.Limit, Selector: selector[res.What], Said: res.Said}
		if len(gone) == 0 {
			it.Value, it.State = res.After, res.AfterState
			if it.State == verdict.ExpectUnknown {
				it.Why = why(res, after, res.AfterWhy)
			}
		} else {
			it.Value, it.State = res.Before, res.BeforeState
			if it.State == verdict.ExpectUnknown {
				it.Why = why(res, before, res.BeforeWhy)
			}
			it.Rewritten, it.RewrittenState = res.After, res.AfterState
			if it.RewrittenState == verdict.ExpectUnknown {
				it.RewrittenWhy = why(res, after, res.AfterWhy)
			}
		}
		c.out.Items = append(c.out.Items, it)
	}
	if len(gone) > 0 {
		for _, ic := range cmp.Incidents {
			was := slices.ContainsFunc(ic.Before, func(x verdict.Catch) bool { return x.Rule == rule })
			is := slices.ContainsFunc(ic.After, func(x verdict.Catch) bool { return x.Rule == rule })
			if was && !is {
				c.out.Lost = append(c.out.Lost, ic.ID)
			}
		}
	}
	switch {
	case c.out.Unfinished:
		c.out.Note = fmt.Sprintf("The replay didn't finish within %s: what it decides is not known.", within)
	case len(results) == 0:
		c.out.Note = "Nothing holds the rule: there is nothing to check."
	}
	return results, nil
}

// rewritten is an agent's rewrite of a rule, as a Prometheus rule file
// holding it alone, read as compare reads the rules it is given (replayed
// as the rules in effect are; durations bounded) and taking the rule's
// place by its name and group, as compare's rules do: the same rule,
// changed.
func rewritten(current []rules.Rule, r rules.Rule, text string) (rules.Rule, error) {
	given, err := rules.Parse([]byte(text))
	if err != nil {
		return rules.Rule{}, fmt.Errorf("the rewrite: %w", err)
	}
	if len(given) != 1 {
		return rules.Rule{}, fmt.Errorf("the rewrite holds %d rules: give the rule alone, rewritten", len(given))
	}
	g := given[0]
	base, _, _ := strings.Cut(r.Name, " [")
	switch {
	case g.Graph != nil:
		return rules.Rule{}, errors.New("the rewrite is a Grafana rule: give a Prometheus rule")
	case g.Unsupported != "":
		return rules.Rule{}, fmt.Errorf("the rewrite can't be replayed: %s", g.Unsupported)
	case g.For < 0 || g.KeepFiringFor < 0 || g.Interval < 0:
		return rules.Rule{}, errors.New("the rewrite: a duration is negative")
	case g.For > compareFor || g.KeepFiringFor > compareFor:
		return rules.Rule{}, fmt.Errorf("the rewrite: for and keep_firing_for are %s at most here", verdict.Duration(compareFor))
	case g.Interval > compareEvery:
		return rules.Rule{}, fmt.Errorf("the rewrite: its group's interval is %s at most here", verdict.Duration(compareEvery))
	case g.Name != base || g.Group != r.Group:
		return rules.Rule{}, fmt.Errorf("a rewrite keeps the rule's name and group: alert %s in group %s", verdict.Clean(base), verdict.Clean(r.Group))
	}
	given[0].Labels = g.Labels
	// Where several share its name and group, the labels given pick it.
	for k, v := range r.Labels {
		if _, ok := g.Labels[k]; !ok && strings.HasSuffix(r.Name, "]") {
			if given[0].Labels == nil {
				given[0].Labels = map[string]string{}
			}
			given[0].Labels[k] = v
		}
	}
	is, err := proposed(current, given, nil)
	if err != nil {
		return rules.Rule{}, err
	}
	_, _, came := source.Split(current, is)
	for _, x := range came {
		if x.Name == r.Name {
			return x, nil
		}
	}
	return rules.Rule{}, errors.New("the rewrite changes nothing that decides when the rule fires")
}

// servesHeld are serve's own expectations of a rule, as its checks hold
// it: --expect's and --config's that hold it, and its annotation's budget
// where neither gives it by rule:; and, with --promtool, their scenarios'
// test files, as serve reads them.
func (s *Server) servesHeld(r rules.Rule) (*verdict.Expectations, map[string][]byte, error) {
	e, sources, _, ok := s.expectations()
	if !ok {
		return nil, nil, errors.New("serve's expectations haven't been read yet: try again in a minute, or give them")
	}
	var list []verdict.Expectation
	given := map[string]bool{}
	labels := r.AlertLabels()
	if e != nil {
		for _, x := range e.Expectations {
			if x.Holds(r.Name, r.Group, labels) {
				list = append(list, x)
				if x.Rule == r.Name {
					for _, it := range x.Items() {
						given[it.What] = true
					}
				}
			}
		}
	}
	if a, ok := r.Says.Annotations[verdict.BudgetAnnotation]; ok {
		if b, err := verdict.ParseBudget(a); err == nil {
			own := &verdict.Budget{}
			if !given["budget week"] {
				own.Week = b.Week
			}
			if !given["budget asleep"] {
				own.Asleep = b.Asleep
			}
			if !given["budget refires"] {
				own.Refires = b.Refires
			}
			if own.Week != nil || own.Asleep != nil || own.Refires != nil {
				list = append(list, verdict.Expectation{Rule: r.Name, Budget: own})
			}
		}
	}
	if len(list) == 0 {
		return nil, nil, fmt.Errorf("serve holds %s to nothing: give the expectations to check", verdict.Clean(r.Name))
	}
	held, err := verdict.NewExpectations(list)
	if err != nil {
		return nil, nil, err
	}
	tests := map[string][]byte{}
	read, _ := s.scenarioTests(held, sources)
	for k, t := range read {
		if t.err == nil {
			_, p, _ := strings.Cut(k, "\n")
			tests[p] = t.test
		}
	}
	return held, tests, nil
}

// draftRun runs a check's scenarios (verdict.ScenarioRunner): each test
// file against the rule's group as it is ("before", or "after" with no
// rewrite) or rewritten ("after"), with --promtool; an agent's one at a
// time across serve.
type draftRun struct {
	ctx    context.Context
	s      *Server
	groups [2][]byte
	why    [2]string
	tests  map[string][]byte
	agent  bool
}

func (d *draftRun) RunScenario(side string, _ bool, _ string, path string) verdict.ScenarioResult {
	i := 0
	if side == "after" {
		i = 1
	}
	switch {
	case d.s.cfg.Promtool == "":
		return verdict.ScenarioResult{State: verdict.ExpectUnknown, Why: "not run: serve has no --promtool"}
	case d.why[i] != "":
		return verdict.ScenarioResult{State: verdict.ExpectUnknown, Why: d.why[i]}
	}
	t, ok := d.tests[path]
	if !ok {
		return verdict.ScenarioResult{State: verdict.ExpectUnknown, Why: "its test file can't be read"}
	}
	if d.agent {
		// One at a time across serve: another waiting would hold its
		// request, and promtool's memory, as long.
		if !d.s.agentScenarios.TryLock() {
			return verdict.ScenarioResult{State: verdict.ExpectUnknown, Why: "not run: another agent's scenario is running; try again in a minute"}
		}
		defer d.s.agentScenarios.Unlock()
	}
	return scenario.Judged(d.s.promtool().Run(d.ctx, d.groups[i], t))
}

// draftRunner is a check's scenario runner: the rule's group as the latest
// version has it, and with the rule rewritten (after) when there is a
// rewrite.
func (s *Server) draftRunner(ctx context.Context, r, after rules.Rule, tests map[string][]byte, rewrite, agent bool) (*draftRun, error) {
	d := &draftRun{ctx: ctx, s: s, tests: tests, agent: agent}
	if s.cfg.Promtool == "" || len(tests) == 0 {
		return d, nil
	}
	sides := []rules.Rule{r, r}
	if rewrite {
		sides[1] = after
	}
	for i, x := range sides {
		if !scenario.Runs(x) {
			d.why[i] = "not a rule in Prometheus's format, which promtool runs"
			continue
		}
		g, err := s.scenarioGroupWith(ctx, r.Name, x)
		if err != nil {
			d.why[i] = "its group couldn't be written"
			continue
		}
		d.groups[i] = g
	}
	return d, nil
}

// scenarioGroupWith is the group of a rule in effect, as scenarioGroups
// writes it, with the rule as with has it.
func (s *Server) scenarioGroupWith(ctx context.Context, name string, with rules.Rule) ([]byte, error) {
	id, err := s.l.LatestID(ctx)
	if err != nil {
		return nil, err
	}
	var recs []rules.Recording
	all, names, err := s.namedRec(ctx, id, &recs)
	if err != nil {
		return nil, err
	}
	at := slices.Index(names, name)
	if at < 0 {
		return nil, errors.New("not in the rules in effect")
	}
	k := scenario.GroupKey(all[at].Source, all[at].Namespace, all[at].Group)
	var alerts []rules.Rule
	for j, x := range all {
		if scenario.GroupKey(x.Source, x.Namespace, x.Group) != k {
			continue
		}
		if j == at {
			w := with
			w.Source, w.Namespace, w.Group, w.Prometheus = x.Source, x.Namespace, x.Group, x.Prometheus
			if w.Says.Annotations == nil && w.Says.Summary == "" && w.Says.Description == "" {
				w.Says = x.Says
			}
			x = w
		}
		alerts = append(alerts, x)
	}
	var recorded []rules.Recording
	for _, x := range recs {
		if x.Source == all[at].Source && x.Group == all[at].Group {
			recorded = append(recorded, x)
		}
	}
	return scenario.Group(alerts, recorded)
}

// toolTestExpectation checks an expectation, or a rewrite of a rule,
// against the rule and its history, and keeps nothing.
func (s *Server) toolTestExpectation(ctx context.Context, _ *mcp.CallToolRequest, in testExpectIn) (*mcp.CallToolResult, testExpectOut, error) {
	c, err := s.checkHeld(ctx, in.Rule, in.Expectations, in.Rewrite, in.Tests, false)
	if err != nil {
		return nil, testExpectOut{Items: []draftItem{}, Lost: []string{}}, err
	}
	return nil, c.out, nil
}

// toolProposeExpectation is an agent's draft of what a rule is held to,
// checked as test_expectation checks it, kept for a person to accept or
// drop: an agent's, under the name of the person it acts for, who can't
// accept it; counted with their proposals an hour.
func (s *Server) toolProposeExpectation(ctx context.Context, _ *mcp.CallToolRequest, in proposeExpectIn) (*mcp.CallToolResult, proposeExpectOut, error) {
	out := proposeExpectOut{Items: []draftItem{}}
	by, ok := actsFor(ctx)
	switch {
	case !ok, by == "anonymous" && s.in != nil:
		return nil, out, errors.New("who this agent acts for isn't known: nothing is drafted")
	case strings.TrimSpace(in.Expectations) == "":
		return nil, out, errors.New("give what the rule is to be held to, as an expectations file holding it by rule: NAME")
	}
	reason := strings.TrimSpace(strings.Map(func(c rune) rune {
		if c != '\n' && (unicode.IsControl(c) || unicode.Is(unicode.Cf, c)) {
			return -1
		}
		return c
	}, in.Reason))
	if len(reason) > ledger.MaxDraftReason {
		return nil, out, fmt.Errorf("reason is %d bytes at most", ledger.MaxDraftReason)
	}
	// Refused before anything is replayed: the hour's drafts and
	// proposals, and its tries.
	if err := s.l.MayDraft(ctx, by); err != nil {
		return nil, out, err
	}
	if !s.tries.take(by, true, time.Now()) {
		return nil, out, errTooManyTries
	}
	c, err := s.checkHeld(ctx, in.Rule, in.Expectations, "", in.Tests, true)
	if err != nil {
		return nil, out, err
	}
	text, err := verdict.ExpectationsYAML(c.entries)
	if err != nil {
		return nil, out, err
	}
	found, err := json.Marshal(c.out.Items)
	if err != nil {
		return nil, out, err
	}
	target, hash := s.draftTarget()
	tests := map[string]string{}
	for _, x := range c.entries {
		for _, p := range x.Scenarios {
			if t, ok := in.Tests[p]; ok {
				tests[p] = t
			}
		}
	}
	id, err := s.l.KeepDraft(ctx, ledger.ExpectDraft{Rule: in.Rule, Origin: ledger.DraftAgent, MadeBy: by, Agent: true, Text: string(text), Tests: tests,
		Reason: reason, Found: found, Target: target, DocHash: hash})
	if err != nil {
		return nil, out, err
	}
	out.ID, out.Items = id, c.out.Items
	if s.cfg.ExternalURL != "" {
		out.Page = draftPage(s.cfg.ExternalURL, in.Rule, id)
	}
	out.Note = "Kept for a person to accept or drop on the rule's expectations tab; the person you act for can't accept it."
	return nil, out, nil
}

// draftPage is a draft's place on its rule's tab.
func draftPage(base, rule string, id int64) string {
	return strings.TrimRight(base, "/") + "/rules/" + url.PathEscape(rule) + "?tab=expectations#draft-" + fmt.Sprint(id)
}

// keepHistoryDraft keeps the history's draft of one rule, by whoever asked,
// for another to accept: its expectations, and what they were counted
// from, met by what is.
func (s *Server) keepHistoryDraft(ctx context.Context, rule, by string) (int64, error) {
	if err := s.l.MayDraft(ctx, by); err != nil {
		return 0, err
	}
	d, _, err := s.historyDraft(ctx, func(name string, _ int64) bool { return name == rule })
	if err != nil {
		return 0, err
	}
	if len(d.Expectations) == 0 {
		return 0, bad("nothing is counted of this rule yet: it has no whole week in the ledger, or isn't replayed")
	}
	text, err := verdict.ExpectationsYAML(d.Expectations)
	if err != nil {
		return 0, err
	}
	found, err := json.Marshal(historyItems(d.Expectations[0], d.Counted(rule)))
	if err != nil {
		return 0, err
	}
	target, hash := s.draftTarget()
	return s.l.KeepDraft(ctx, ledger.ExpectDraft{Rule: rule, Origin: ledger.DraftHistory, MadeBy: by, Text: string(text), Found: found,
		Target: target, DocHash: hash})
}

// docHashOf is the hash of a file's bytes as the git path finds it.
func docHashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
