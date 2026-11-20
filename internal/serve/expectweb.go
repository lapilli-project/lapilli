package serve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
)

// Where the expectations' checks show (docs/design/expectations.md, "After
// it is deployed"): a rule's tab of them, each with its weeks; a line of
// the overview's What to look at for each broken, made a task with one
// press; the rules list's facet; the weekly digest's count; and
// GET /api/v1/expectations.

// expectWhyText says why a check is not known, or so far.
var expectWhyText = map[string]string{
	whyNoHours:        "no one's hours say when people sleep",
	whyNotWhole:       "a week the ledger holds part of",
	whyNotHeld:        "a week before the ledger's first firing",
	whySoFar:          "so far",
	whyUnreplayed:     "the rule isn't replayed, nor copied in",
	whyNoIncident:     "the incident isn't written down",
	whyNotJudged:      "the rule changed since: to be judged again on it as it is",
	whyTooOld:         "the rule changed since, and the incident is older than the weeks checked",
	whyReplayFailed:   "judging it again on the rule as it is failed",
	whyAnnotationBad:  "its annotation lapilli.dev/budget can't be read: want a budget such as \"10/week, 2 asleep, 3 refires\"",
	whyAnnotationTwo:  "passed over: the expectations file or serve's configuration give this rule this budget too",
	whyAnnotationPast: "passed over: a check holds so many expectations at most, the expectations file's and serve's configuration's first",
	whyNotCopied:      "a week its firings' copies don't reach",
	whyNoPromtool:     "not run: serve has no --promtool",
	whyNotProm:        "not a rule in Prometheus's format, which promtool runs",
	whyNotRunYet:      "the rule's group or its test file changed since: to be run",
	whyTestUnread:     "its test file can't be read",
	whyRunFailed:      "promtool couldn't run it",
}

// expectHeldText is what an expectation holds a rule to, for a person: an
// incident's id, which comes from outside, in a <bdi>, as a name in a
// sentence is (trBDI), so that one written right to left doesn't turn
// the words around it.
func expectHeldText(l string, c ledger.ExpectCheck) template.HTML {
	esc := template.HTMLEscapeString
	plain := func(s string) template.HTML { return template.HTML(esc(s)) }
	switch c.What {
	case "budget week":
		return plain(tr(l, "at most %s a week", c.Held))
	case "budget asleep":
		return plain(tr(l, "at most %s a week while people slept", c.Held))
	case "budget refires":
		return plain(tr(l, "at most %s re-fires a week", c.Held))
	case "budget":
		return plain(tr(l, "a budget"))
	}
	if c.Scenario() {
		return plain(tr(l, "passes the scenario %s", strings.TrimPrefix(c.What, "scenario ")))
	}
	inc := "<bdi>" + esc(strings.TrimPrefix(c.What, "catches ")) + "</bdi>"
	if c.Held == "caught" {
		return template.HTML(fmt.Sprintf(esc(tr(l, "catches %s")), inc))
	}
	return template.HTML(fmt.Sprintf(esc(tr(l, "catches %s %s")), inc, esc(tr(l, "within %s", strings.TrimPrefix(c.Held, "within ")))))
}

// expectCell is a check as a cell: its colour, sign, value and what it
// says in full.
type expectCell struct {
	Week                     string
	Class, Sign, Value, Text string
}

func expectCellOf(l string, c ledger.ExpectCheck, week string) expectCell {
	x := expectCell{Week: week, Value: c.Value}
	switch c.Result {
	case ledger.ExpectMet:
		x.Class, x.Sign = statusClass("expect", "met"), "✓"
	case ledger.ExpectBroken:
		x.Class, x.Sign = statusClass("expect", "broken"), "✕"
	case ledger.ExpectOpen:
		x.Class, x.Sign = statusClass("expect", "open"), "…"
	default:
		x.Class, x.Sign = statusClass("expect", "unknown"), "?"
	}
	text := tr(l, expectResultText[c.Result])
	if why := expectWhyText[c.Why]; why != "" && c.Result != ledger.ExpectOpen {
		text += ": " + tr(l, why)
	}
	if week != "" {
		x.Text = tr(l, "week of %s: %s, %s", week, text, x.Value)
	} else {
		x.Text = text + ", " + x.Value
	}
	return x
}

// expectResultText names a check's result.
var expectResultText = map[string]string{
	ledger.ExpectMet: "met", ledger.ExpectBroken: "broken", ledger.ExpectUnknown: "not known", ledger.ExpectOpen: "so far",
}

// expectLine is one expectation of a rule on its tab: what it holds the
// rule to, by what and from where; a budget's weeks, the earliest first,
// or a catch's or a scenario's one result (of a scenario, when it was
// found, and what promtool said); its task open, and, broken, the row a
// task is made of.
type expectLine struct {
	Held                     template.HTML
	Expect, Selector, Source string
	Catch, Scenario          bool
	Weeks                    []expectCell
	Last                     expectCell
	Incident, IncidentAt     string
	Found, Detail            string
	Broken                   bool
	Task                     int64
	Week                     string // the row a task is made of: its week, as days
}

// expectView is a rule's expectations: each, how many are broken, and
// whether any is held at all.
type expectView struct {
	Lines  []expectLine
	Broken int
	Weeks  []expectHead
}

// expectHead is a week's column: its Monday, MM-DD, or the week under way.
type expectHead struct {
	Label string
	SoFar bool
}

// expectViewOf is a rule's expectations as its tab shows them, from the
// checks the ledger keeps.
func (s *Server) expectViewOf(ctx context.Context, l, rule string) (*expectView, error) {
	cs, err := s.l.ExpectChecks(ctx, ledger.ExpectQuery{Rule: rule, HasRule: true, Current: true})
	if err != nil || len(cs) == 0 {
		return nil, err
	}
	open, err := s.l.OpenExpectTasks(ctx)
	if err != nil {
		return nil, err
	}
	this := ledger.Week(s.l.Day(s.now()))
	v := &expectView{}
	for w := this - (expectWeeks-1)*7; w <= this; w += 7 {
		v.Weeks = append(v.Weeks, expectHead{Label: s.l.Midnight(w).In(s.cfg.Loc).Format("01-02"), SoFar: w == this})
	}
	by := map[string]*expectLine{}
	var order []string
	for _, c := range cs {
		x := by[c.Expect]
		if x == nil {
			x = &expectLine{Expect: c.Expect, Held: expectHeldText(l, c), Selector: c.Selector, Source: tr(l, expectSourceText[c.Source]), Catch: c.Catch(),
				Scenario: c.Scenario()}
			by[c.Expect] = x
			order = append(order, c.Expect)
		}
		if id := open[c.Key()]; id != 0 {
			x.Task = id
		}
		if c.Catch() {
			x.Last = expectCellOf(l, c, "")
			x.Incident = strings.TrimPrefix(c.What, "catches ")
			if !c.At.IsZero() {
				x.IncidentAt = c.At.In(s.cfg.Loc).Format("2006-01-02 15:04")
			}
			if c.Result == ledger.ExpectBroken {
				x.Broken, x.Week = true, strconv.FormatInt(c.Week, 10)
			}
			continue
		}
		if c.Scenario() {
			x.Last, x.Detail = expectCellOf(l, c, ""), c.Detail
			if !c.Checked.IsZero() {
				x.Found = c.Checked.In(s.cfg.Loc).Format("2006-01-02 15:04")
			}
			if c.Result == ledger.ExpectBroken {
				x.Broken, x.Week = true, strconv.FormatInt(c.Week, 10)
			}
			continue
		}
		x.Weeks = append(x.Weeks, expectCellOf(l, c, s.mondayOf(c.Week)))
		// Broken in the last whole week or the week so far: the latest is
		// what a task is made of.
		if c.Week >= this-7 && c.Result == ledger.ExpectBroken {
			x.Broken, x.Week = true, strconv.FormatInt(c.Week, 10)
		}
	}
	for _, k := range order {
		x := by[k]
		// A budget's weeks before the rule was in effect have no check:
		// their cells are left empty, so each column is one week.
		if !x.Catch && !x.Scenario && len(x.Weeks) < len(v.Weeks) {
			pad := make([]expectCell, len(v.Weeks)-len(x.Weeks))
			x.Weeks = append(pad, x.Weeks...)
		}
		if x.Broken {
			v.Broken++
		}
		v.Lines = append(v.Lines, *x)
	}
	// What is broken first.
	slices.SortStableFunc(v.Lines, func(a, b expectLine) int {
		if a.Broken != b.Broken {
			if a.Broken {
				return -1
			}
			return 1
		}
		return 0
	})
	return v, nil
}

// expectActions are the overview's lines of what is broken: each a rule's
// expectation broken in the week so far or the last whole week, or a
// catch lost, of the team's rules when one is picked, made a task with one
// press, or its task open.
func (s *Server) expectActions(ctx context.Context, l string, team *ledger.Team, o *owners) ([]action, error) {
	this := ledger.Week(s.l.Day(s.now()))
	cs, err := s.l.ExpectChecks(ctx, ledger.ExpectQuery{Broken: true, Current: true, From: this - 7})
	if err != nil || len(cs) == 0 {
		return nil, err
	}
	open, err := s.l.OpenExpectTasks(ctx)
	if err != nil {
		return nil, err
	}
	openOf := map[[2]string]int64{}
	for k, id := range open {
		openOf[[2]string{k.Rule, k.Expect}] = id
	}
	var out []action
	for _, c := range expectShown(cs, this) {
		if team != nil && o.rule[c.Rule] != team.ID {
			continue
		}
		detail := tr(l, "held by %s, in %s", c.Selector, tr(l, expectSourceText[c.Source]))
		out = append(out, action{Text: s.expectSentence(l, c, this), Detail: detail,
			Href: "/rules/" + url.PathEscape(c.Rule) + "?tab=expectations", Kind: "tune", Rule: c.Rule, Title: s.expectTitle(l, c, this),
			Expect: c.Expect, ExpectWeek: strconv.FormatInt(c.Week, 10), Task: openOf[[2]string{c.Rule, c.Expect}]})
	}
	return out, nil
}

// postExpectTask makes the task an expectation broken becomes, with one
// press: the row by its rule, expectation and week, as the ledger last
// checked it, refused when it isn't broken; a task open about it, of any
// week, is gone to. A team's editor makes their own team's rule's alone.
func (s *Server) postExpectTask(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	ctx := r.Context()
	rule, expect := r.PostFormValue("rule"), r.PostFormValue("expect")
	week, err := strconv.ParseInt(r.PostFormValue("week"), 10, 64)
	if err != nil || rule == "" || len(expect) != 32 {
		s.oops(w, r, bad("say which expectation to make a task of: its rule, expectation and week"))
		return
	}
	o, err := s.owners(ctx)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	team := o.rule[rule]
	if teams, limited := scoped(r); limited && (team == 0 || !slices.Contains(teams, team)) {
		s.oops(w, r, ledger.ErrNotYourTeam)
		return
	}
	cs, err := s.l.ExpectChecks(ctx, ledger.ExpectQuery{Rule: rule, HasRule: true, Current: true})
	if err != nil {
		s.oops(w, r, err)
		return
	}
	i := slices.IndexFunc(cs, func(c ledger.ExpectCheck) bool { return c.Expect == expect && c.Week == week })
	if i < 0 || cs[i].Result != ledger.ExpectBroken {
		s.oops(w, r, bad("that expectation isn't broken, as it was last checked"))
		return
	}
	this := ledger.Week(s.l.Day(s.now()))
	id, err := s.l.MakeExpectTask(ctx, s.expectTaskIn(lang(r), cs[i], team, this), cs[i].Key(), this, s.user(r))
	switch {
	case errors.Is(err, ledger.ErrExpectTaskOpen):
		back(w, r, urlContext(r.URL.Query()).Link(fmt.Sprintf("/tasks/%d", id)))
		return
	case errors.Is(err, ledger.ErrExpectNotBroken):
		err = bad("that expectation isn't broken, as it was last checked")
	}
	to := r.PostFormValue("back")
	if to == "" {
		to = urlContext(r.URL.Query()).Link(fmt.Sprintf("/tasks/%d", id))
	}
	s.written(w, r, err, to)
}

// apiExpectations are the checks of the expectations held now, of one
// rule (rule=) or every rule, with the weeks: a viewer's, as the rules are.
func (s *Server) apiExpectations(w http.ResponseWriter, r *http.Request) {
	q := ledger.ExpectQuery{Current: true}
	if v, ok := r.URL.Query()["rule"]; ok {
		q.Rule, q.HasRule = v[0], true
	}
	cs, err := s.l.ExpectChecks(r.Context(), q)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	writeJSON(w, s.expectOut(cs))
}

// expectCheckOut is a check as the API and MCP give it: its week as its
// Monday's date, "" for a catch of an incident not written down.
type expectCheckOut struct {
	ledger.ExpectCheck
	WeekOf string `json:"week_of"`
}

func (s *Server) expectOut(cs []ledger.ExpectCheck) map[string]any {
	out := []expectCheckOut{}
	for _, c := range cs {
		x := expectCheckOut{ExpectCheck: c}
		if c.Week != 0 {
			x.WeekOf = s.mondayOf(c.Week)
		}
		out = append(out, x)
	}
	return map[string]any{"checks": out, "this_week": s.mondayOf(ledger.Week(s.l.Day(s.now())))}
}

// expectHeld are, of the expectations held now, the rules held to one,
// and of them those with one broken in the week so far or the last whole
// week, or a catch lost: the rules list's facet.
func (s *Server) expectHeld(ctx context.Context) (held, broken map[string]bool, err error) {
	cs, err := s.l.ExpectChecks(ctx, ledger.ExpectQuery{Current: true})
	if err != nil {
		return nil, nil, err
	}
	this := ledger.Week(s.l.Day(s.now()))
	held, broken = map[string]bool{}, map[string]bool{}
	for _, c := range cs {
		held[c.Rule] = true
	}
	for _, c := range expectShown(cs, this) {
		broken[c.Rule] = true
	}
	return held, broken, nil
}

// expectDigest is what the weekly digest says of the expectations in its
// week, of a team's rules or everyone's: how many were broken, met and not
// known, and each broken, 20 named.
type expectDigest struct {
	Broken   int      `json:"broken"`
	Met      int      `json:"met"`
	NotKnown int      `json:"not_known"`
	Said     []string `json:"broken_said,omitempty"`
}

func (s *Server) expectDigestOf(ctx context.Context, week int64, team *ledger.Team) (*expectDigest, error) {
	cs, err := s.l.ExpectChecks(ctx, ledger.ExpectQuery{Current: true, From: week})
	if err != nil || len(cs) == 0 {
		return nil, err
	}
	o, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	d := &expectDigest{}
	l := s.cfg.Lang
	for _, c := range cs {
		// Of a catch, the week its incident began in; of a scenario, the
		// week it was found in.
		if team != nil && o.rule[c.Rule] != team.ID || c.Week != week {
			continue
		}
		switch c.Result {
		case ledger.ExpectMet:
			d.Met++
		case ledger.ExpectBroken:
			d.Broken++
			if len(d.Said) < 20 {
				d.Said = append(d.Said, s.expectSentence(l, c, week+7))
			}
		case ledger.ExpectUnknown:
			d.NotKnown++
		}
	}
	if d.Broken+d.Met+d.NotKnown == 0 {
		return nil, nil
	}
	return d, nil
}

// expectLine is the digest's line of the expectations.
func (d *expectDigest) text(l string) string {
	if d == nil {
		return ""
	}
	line := tr(l, "Expectations: %d broken, %d met, %d not known.", d.Broken, d.Met, d.NotKnown)
	if len(d.Said) > 0 {
		line += " " + strings.Join(d.Said, "; ") + "."
		if n := d.Broken - len(d.Said); n > 0 {
			line += " " + tr(l, "And %d more.", n)
		}
	}
	return line
}

// expectMCP is what MCP's rule tool says of a rule's expectations.
func (s *Server) expectMCP(ctx context.Context, rule string) ([]expectCheckOut, error) {
	cs, err := s.l.ExpectChecks(ctx, ledger.ExpectQuery{Rule: rule, HasRule: true, Current: true})
	if err != nil {
		return nil, err
	}
	out := s.expectOut(cs)["checks"].([]expectCheckOut)
	return out, nil
}

// aboutExpect adds to what is told of a digest its expectations, as the
// digest's text counts them: of a team's rules, or everyone's.
func (s *Server) aboutExpect(ctx context.Context, about map[string]any, from time.Time, team *ledger.Team) error {
	d, err := s.expectDigestOf(ctx, ledger.Week(s.l.Day(from)), team)
	if err == nil && d != nil {
		about["expectations"] = d
	}
	return err
}

// maxExpectSeries is the most rules lapilli_rule_expectations_broken has
// series of.
const maxExpectSeries = 500

// expectMetrics are the expectations, as last checked, for /metrics: how
// many rules and expectations are held, and of them broken, in the week so
// far or the last whole week, or a catch; and, by rule, those broken.
// Nothing with none held.
func (s *Server) expectMetrics(ctx context.Context, b *bytes.Buffer) error {
	cs, err := s.l.ExpectChecks(ctx, ledger.ExpectQuery{Current: true})
	if err != nil || len(cs) == 0 {
		return err
	}
	held := map[[2]string]bool{}
	for _, c := range cs {
		held[[2]string{c.Rule, c.Expect}] = true
	}
	broken := expectShown(cs, ledger.Week(s.l.Day(s.now())))
	byRule := map[string]int{}
	for _, c := range broken {
		byRule[c.Rule]++
	}
	fmt.Fprintf(b, "# HELP lapilli_expectations_held The expectations rules are held to, each rule to each budget, catch and scenario that holds it, as last checked (docs/design/expectations.md).\n# TYPE lapilli_expectations_held gauge\nlapilli_expectations_held %d\n", len(held))
	fmt.Fprintf(b, "# HELP lapilli_expectations_broken Of lapilli_expectations_held, those broken: a budget in the week so far or the last whole week, a catch, or a scenario.\n# TYPE lapilli_expectations_broken gauge\nlapilli_expectations_broken %d\n", len(broken))
	if len(byRule) > 0 {
		b.WriteString("# HELP lapilli_rule_expectations_broken Each rule's expectations broken, as lapilli_expectations_broken counts them; 500 rules at most.\n# TYPE lapilli_rule_expectations_broken gauge\n")
		for i, name := range slices.Sorted(maps.Keys(byRule)) {
			if i == maxExpectSeries {
				break
			}
			fmt.Fprintf(b, "lapilli_rule_expectations_broken{rule=\"%s\"} %d\n", labelValue(name), byRule[name])
		}
	}
	return nil
}
