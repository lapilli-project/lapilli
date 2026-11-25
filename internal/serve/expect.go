package serve

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/scenario"
	"lapilli.dev/internal/verdict"
)

// Expectations after deploy (docs/design/expectations.md, steps 2 and 4):
// what a rule is held to, declared in --expect's file, under
// expectations: in --config (for rules kept only in Grafana or a ruler), or
// in the rule's own annotation lapilli.dev/budget, is checked by the leader
// after each round over the week so far and the twelve whole weeks before:
// each budget against the ledger's weeks (Monday, in --tz), each catch
// against the incidents written down, each scenario by running promtool
// (--promtool) on its test file against the rule's group as the latest
// version has it, again when either changes. What it finds is kept
// (expectation_checks); a broken one is told once a rule, an expectation
// and a week, and, with --expect-tasks, made a task for the rule's team.
//
// How the sources combine: --expect's file and --config's expectations are
// one list, read as one file is (verdict.NewExpectations): a rule's, a
// group's or a selector's entries in both are made one, and the same
// budget or incident given in both is refused, the two kept as they last
// were, and said. A rule's annotation is its author's, not the operator's:
// it holds the rule (by its name) to what it says unless the file or
// --config give that rule, by the same rule:, the same budget, when it is
// passed over, and said on the rule's tab as not known. One that can't be
// read is said so too, and holds it to nothing.

// The sources an expectation is declared in.
const (
	expectFromFile       = "file"
	expectFromConfig     = "config"
	expectFromAnnotation = "annotation"
)

// annotationSelector is what holds a rule to its annotation's budget, as
// its check is kept: its own annotation.
const annotationSelector = "annotation " + verdict.BudgetAnnotation

// Why a check is not known, or open.
const (
	whyNoHours = "no_hours" // a budget while people slept, by hours with no sleep
	// whyPagesNotYet is a budget of pages in a lapilli serve that isn't
	// given the alert manager's configuration to work them out from
	// (docs/design/routing.md); the code is as it was.
	whyPagesNotYet = "pages_not_yet"
	// whyPagesNotCovered is a week the notifications weren't all worked out
	// for: before the first hour worked out, or past the last.
	whyPagesNotCovered = "pages_not_covered"
	// whyNotHeld is a budget's week before the ledger's first firing: no
	// firing held isn't none fired, so it has no number (bundle 67).
	whyNotHeld       = "not_held"
	whySoFar         = "so_far"           // the week under way, under budget so far
	whyUnreplayed    = "unreplayed"       // a rule Lapilli can't replay, nor copies in
	whyNoIncident    = "no_incident"      // a catch of an incident not written down
	whyNotJudged     = "not_judged_again" // a catch of a rule changed since, not yet judged on it
	whyTooOld        = "older"            // a catch of a rule changed since, of an incident older than the weeks checked
	whyReplayFailed  = "replay_failed"    // judging it again failed
	whyAnnotationBad = "annotation_unread"
	whyAnnotationTwo = "annotation_twice"
	// whyAnnotationPast is a rule's annotation passed over: the check
	// holds maxExpectHeld at most, the operator's first.
	whyAnnotationPast = "annotation_past"
	// whyNotCopied is a week of a rule only copied in that its source's
	// copies don't cover: no firing copied isn't none fired.
	whyNotCopied = "not_copied"
	// A scenario's (step 4).
	whyNoPromtool = "no_promtool"    // serve has no --promtool
	whyNotProm    = "not_prometheus" // the rule isn't in Prometheus's format, or its definition isn't known
	whyNotRunYet  = "not_run_yet"    // its group or test file changed since: to be run
	whyTestUnread = "test_unread"    // its test file can't be read
	whyRunFailed  = "run_failed"     // promtool couldn't run it: a time out, a crash, an answer it doesn't give
)

// scenarioBegun is what a scenario's check says while its run hasn't
// ended (KeepScenarioRun): kept so by the next leader when the run never
// ended, as when lapilli serve was stopped as promtool took its memory.
const scenarioBegun = "promtool didn't finish: lapilli serve stopped while it ran (as when the container runs out of memory); " +
	"it isn't run again until the rule's group, the test file or promtool changes"

// scenarioStopped is what a scenario's check says when lapilli serve
// stopped its run itself (shut down, or another leader took over): kept
// with no version, so that the next leader runs it again.
const scenarioStopped = "promtool didn't finish: lapilli serve stopped it as it stopped itself; it is run again"

// expectWeeks is how many weeks a check reads: the week so far and the
// twelve whole weeks before it.
const expectWeeks = 13

// maxExpectHeld is the most rules and expectations a check holds, each
// rule to each budget and catch that holds it: each is kept for each of
// thirteen weeks.
var maxExpectHeld = 10000

// expectReplaysARun is how many catches of rules changed since are judged
// again by a replay in a round, within one --timeout; the rest in the
// rounds after. expectToldARun and expectTasksARun bound what is told and
// made a task of in a round, as the goals' are.
var (
	expectReplaysARun = 5
	expectToldARun    = 50
	expectTasksARun   = 20
	// expectScenariosARun is how many scenarios are run in a round, each
	// within promtool's own time limit; the rest in the rounds after.
	expectScenariosARun = 5
)

// expectState is what the leader made of the expectations declared, and
// of checking them.
type expectState struct {
	sync.Mutex
	// --expect: the file kept to (applied, its hash; file, what it
	// declares), read once (pending, at pendingAt), as --config is: kept
	// to once read the same twice in a row, configEvery apart. fileErr is
	// what is wrong with the file last read, logged once (fileSaid).
	applied, pending string
	pendingAt        time.Time
	file             *verdict.Expectations
	fileErr          error
	fileSaid         string
	// config are --config's, as the file the ledger was last kept to
	// declares them; configKept says it was kept to in this term.
	config     []verdict.Expectation
	configKept bool
	// good is the last combination of the two that could be used, of
	// goodKey, and from where each is declared (sources, by selector and
	// what); bad, why the latest can't, "" for nothing.
	good    *verdict.Expectations
	sources map[string]string
	goodKey string
	bad     string
	// key is what the last check was of: the ledger's generation, the
	// week and the expectations; "" to check again.
	key string
	// groups are, of the version of the rules groupsOf, each rule's group
	// as promtool reads it, by the name it is judged under (scenarioGroups).
	groupsOf int64
	groups   map[string][]byte
}

// newTerm forgets what an earlier term read: the file is read twice again,
// and --config's are known once the ledger is kept to it again.
func (st *expectState) newTerm() {
	st.Lock()
	defer st.Unlock()
	st.applied, st.pending, st.file, st.fileErr, st.fileSaid = "", "", nil, nil, ""
	st.config, st.configKept, st.key = nil, false, ""
}

// keptConfig notes --config's expectations, as the file the ledger was
// just kept to declares them.
func (s *Server) keptConfig(xs []verdict.Expectation) {
	st := &s.expect
	st.Lock()
	changed := !st.configKept || expectListKey(st.config) != expectListKey(xs)
	st.config, st.configKept = xs, true
	if changed {
		st.key = ""
	}
	st.Unlock()
	if changed && len(xs) > 0 {
		s.poke()
	}
}

// readExpect reads --expect again: kept to once it is read the same twice
// in a row, configEvery apart at least; a file that can't be read, or
// held, changes nothing, and is said once.
func (s *Server) readExpect() {
	if s.cfg.Expect == "" {
		return
	}
	st := &s.expect
	b, err := readBounded(s.cfg.Expect, verdict.MaxExpectations)
	sum := sha256.Sum256(b)
	hash := hex.EncodeToString(sum[:])
	var e *verdict.Expectations
	if err == nil {
		e, err = verdict.ReadExpectations(bytes.NewReader(b))
	}
	now := s.wall()
	st.Lock()
	defer st.Unlock()
	if err != nil {
		st.fileErr, st.pending = err, ""
		if text := err.Error(); text != st.fileSaid {
			st.fileSaid = text
			s.log.Error("--expect can't be read; the expectations stay as they last were", "file", s.cfg.Expect, "err", text)
		}
		return
	}
	switch {
	case hash == st.applied:
		st.pending, st.fileErr, st.fileSaid = "", nil, ""
		return
	case hash != st.pending:
		st.pending, st.pendingAt = hash, now
		return
	case now.Before(st.pendingAt.Add(configEvery)):
		return
	}
	st.applied, st.file, st.pending, st.fileErr, st.fileSaid, st.key = hash, e, "", nil, "", ""
	s.log.Info("--expect read", "file", s.cfg.Expect, "expectations", len(e.Expectations))
	go s.poke()
}

// readBounded reads a file, n bytes at most.
func readBounded(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(n)+1))
	if err == nil && len(b) > n {
		err = fmt.Errorf("more than %d bytes", n)
	}
	return b, err
}

// expectListKey tells one list of expectations from another.
func expectListKey(xs []verdict.Expectation) string {
	h := sha256.New()
	for _, x := range xs {
		fmt.Fprintf(h, "%s\n", x.Selector())
		for _, it := range x.Items() {
			fmt.Fprintf(h, "%s %s\n", it.What, it.Held)
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// expectations are the file's and --config's made one, and from where each
// is declared, once both are known in this term: ok false until then. A
// combination that can't be used gives the last that could, and bad says
// why.
func (s *Server) expectations() (e *verdict.Expectations, sources map[string]string, key string, ok bool) {
	st := &s.expect
	st.Lock()
	defer st.Unlock()
	if s.cfg.Expect != "" && st.applied == "" || s.cfg.Config != "" && !st.configKept {
		return nil, nil, "", false
	}
	k := st.applied + "/" + expectListKey(st.config)
	if k != st.goodKey {
		var list []verdict.Expectation
		sources := map[string]string{}
		add := func(xs []verdict.Expectation, from string) {
			for _, x := range xs {
				list = append(list, x)
				for _, it := range x.Items() {
					sources[x.Selector()+"\n"+it.What] = from
				}
			}
		}
		if st.file != nil {
			add(st.file.Expectations, expectFromFile)
		}
		add(st.config, expectFromConfig)
		e, err := verdict.NewExpectations(list)
		if err != nil {
			bad := "--expect and --config's expectations: " + err.Error()
			if bad != st.bad {
				s.log.Error("the expectations of --expect and --config can't be held together; they stay as they last were", "err", err)
			}
			st.bad = bad
		} else {
			st.good, st.sources, st.goodKey, st.bad = e, sources, k, ""
		}
	}
	if st.good == nil && st.goodKey == "" {
		return nil, nil, "", st.bad == ""
	}
	return st.good, st.sources, st.goodKey, true
}

// expectRule is a rule as a check holds it: what it is named, grouped and
// labelled by, its team, and how it stood over the weeks checked.
type expectRule struct {
	name, group string
	labels      map[string]string // nil when not known
	id, team    int64
	unreplayed  bool
	// budget is its annotation's, budgetBad that one it has can't be read.
	budget    *verdict.Budget
	budgetBad bool
	// from is when it came into effect, zero for before the weeks
	// checked; since, when it came to be as it is now; key, what it is
	// now (zero values for a rule only copied in, which isn't replayed).
	from, since time.Time
	key         string
	def         *rules.Rule
	// copied says it is only copied in (its source's, as rules lists it);
	// spans, what its source's copies cover of it, each span without a gap
	// or a period it was left out of them, the earliest first: a week or
	// an incident not inside one is not known.
	copied bool
	spans  [][2]time.Time
}

// copiedOver says a rule only copied in is known from from to to: its
// source's copies cover them within one span, with no gap between. Of the
// week under way (soFar), to is where it ends, and the copies are known so
// far as they reach into it, from its start: from the latest span, which
// no gap follows.
func (x *expectRule) copiedOver(from, to time.Time, soFar bool) bool {
	for i, s := range x.spans {
		if s[0].After(from) {
			continue
		}
		if !s[1].Before(to) || soFar && i == len(x.spans)-1 && !s[1].Before(from) {
			return true
		}
	}
	return false
}

// expectRules are the rules in effect now: those of the latest version of
// the rules, over the weeks from start, and those copied in; with no
// version, every rule the ledger has.
func (s *Server) expectRules(ctx context.Context, start time.Time, table map[int64]ledger.RuleRow, own *owners) ([]*expectRule, error) {
	ids := make(map[string]int64, len(table))
	for id, r := range table {
		ids[r.Name] = id
	}
	var vs []version
	if s.replaying() {
		var err error
		if vs, err = s.versions(ctx, start, s.now()); err != nil {
			return nil, err
		}
	}
	by := map[string]*expectRule{}
	var out []*expectRule
	add := func(name string) *expectRule {
		x := by[name]
		if x == nil {
			x = &expectRule{name: name, id: ids[name], team: own.rule[name]}
			if r, ok := table[x.id]; ok && x.id != 0 {
				x.group, x.unreplayed = r.Group, r.Unsupported != "" && r.Source == ""
			}
			if l, ok := own.ruleLabels[name]; ok {
				x.labels = l
			}
			by[name] = x
			out = append(out, x)
		}
		return x
	}
	for i, v := range vs {
		for j := range v.Rules {
			r := &v.Rules[j]
			x := by[r.Name]
			if x == nil {
				x = add(r.Name)
				if i > 0 {
					x.from = v.Since
				}
			}
			if k := ruleKey(*r); k != x.key {
				x.key, x.since = k, v.Since
			}
			if i == len(vs)-1 {
				x.def, x.group, x.unreplayed = r, r.Group, r.Unsupported != ""
				x.labels = cmpOrLabels(r.AlertLabels())
				if a, ok := r.Says.Annotations[verdict.BudgetAnnotation]; ok {
					b, err := verdict.ParseBudget(a)
					x.budget, x.budgetBad = b, err != nil
				}
			}
		}
	}
	if len(vs) > 0 {
		// Only those in the latest version, and those copied in.
		latest := map[string]bool{}
		for _, r := range vs[len(vs)-1].Rules {
			latest[r.Name] = true
		}
		out = slices.DeleteFunc(out, func(x *expectRule) bool { return !latest[x.name] })
		for _, r := range table {
			if r.Source != "" && !latest[r.Name] {
				add(r.Name)
			}
		}
	} else {
		for _, r := range table {
			add(r.Name)
		}
	}
	slices.SortFunc(out, func(a, b *expectRule) int { return strings.Compare(a.name, b.name) })
	return out, nil
}

// ruleKey is what tells one version of a rule from another, as its page
// tells them (rules.Rule.Whole), hashed.
func ruleKey(r rules.Rule) string {
	h := sha256.Sum256([]byte(r.Whole()))
	return hex.EncodeToString(h[:8])
}

func cmpOrLabels(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// heldItem is one thing a rule is held to: by what, from where.
type heldItem struct {
	verdict.ExpectItem
	selector, source string
	why              string // whyAnnotationBad or whyAnnotationTwo: said, and held to nothing
}

// heldBy are what a rule is held to: by each expectation that holds it,
// and its annotation's budget, but where the file or --config give it, by
// the same rule:, the same budget.
func heldBy(x *expectRule, e *verdict.Expectations, sources map[string]string) []heldItem {
	var out []heldItem
	given := map[string]bool{}
	if e != nil {
		for i := range e.Expectations {
			ex := &e.Expectations[i]
			if !ex.Holds(x.name, x.group, x.labels) {
				continue
			}
			for _, it := range ex.Items() {
				out = append(out, heldItem{ExpectItem: it, selector: ex.Selector(), source: sources[ex.Selector()+"\n"+it.What]})
				if ex.Rule == x.name {
					given[it.What] = true
				}
			}
		}
	}
	switch {
	case x.budgetBad:
		out = append(out, heldItem{ExpectItem: verdict.ExpectItem{What: "budget", Held: "–"}, selector: annotationSelector, source: expectFromAnnotation, why: whyAnnotationBad})
	case x.budget != nil:
		for _, it := range verdict.BudgetItems(x.budget) {
			h := heldItem{ExpectItem: it, selector: annotationSelector, source: expectFromAnnotation}
			if given[it.What] {
				h.why = whyAnnotationTwo
			}
			out = append(out, h)
		}
	}
	return out
}

// errHeldTooMuch is what a check says past maxExpectHeld, of the
// operator's own: the expectations file's and serve's configuration's.
func errExpectTooMuch() error {
	return fmt.Errorf("the rules are held to more than %d expectations by the expectations file and serve's configuration, each rule to each budget and catch that holds it: "+
		"give a catch to the rule it is for (rule:, not group: or labels:), or hold fewer rules", maxExpectHeld)
}

// expectPair is a rule and one thing it is held to.
type expectPair struct {
	r  *expectRule
	it heldItem
}

// expectPairs are what each rule is held to, bounded: the operator's own
// (the expectations file's and serve's configuration's) first, maxExpectHeld
// at most, past which the check fails, saying what to do; then the rule
// authors' annotations, a rule's whole, in the rules' order, while they
// fit in what is left, the rest each one not known (whyAnnotationPast). So
// no rule's annotation can stop the operator's checks.
func expectPairs(rs []*expectRule, e *verdict.Expectations, sources map[string]string) ([]expectPair, error) {
	type held struct {
		r        *expectRule
		own, its []heldItem
	}
	var all []held
	n := 0
	for _, r := range rs {
		h := held{r: r}
		for _, it := range heldBy(r, e, sources) {
			if it.source == expectFromAnnotation {
				h.its = append(h.its, it)
			} else {
				h.own = append(h.own, it)
			}
		}
		if n += len(h.own); n > maxExpectHeld {
			return nil, errExpectTooMuch()
		}
		all = append(all, h)
	}
	var out []expectPair
	for _, h := range all {
		for _, it := range h.own {
			out = append(out, expectPair{h.r, it})
		}
		if len(h.its) == 0 {
			continue
		}
		if n+len(h.its) > maxExpectHeld {
			out = append(out, expectPair{h.r, heldItem{ExpectItem: verdict.ExpectItem{What: "budget", Held: "–"}, selector: annotationSelector,
				source: expectFromAnnotation, why: whyAnnotationPast}})
			continue
		}
		n += len(h.its)
		for _, it := range h.its {
			out = append(out, expectPair{h.r, it})
		}
	}
	return out, nil
}

// expectAfter checks each rule's expectations after a round, the
// leader's: over the week so far and the twelve whole weeks before, from
// one read of the weeks' tallies, the hours of the rules held to a budget
// while people slept, and the incidents the catches name; a catch of a
// rule changed since its incident is judged again on the rule as it is,
// by a replay, expectReplaysARun a round. What it finds is kept; what is
// broken is told, and made a task with --expect-tasks. Nothing is checked
// until --expect, and --config's expectations, have been read in this term
// (expectations).
func (s *Server) expectAfter(ctx context.Context) error {
	s.readExpect()
	e, sources, ekey, ok := s.expectations()
	s.expect.Lock()
	fileErr, bad := s.expect.fileErr, s.expect.bad
	s.expect.Unlock()
	// Said on the status line, as each round finds it; logged once, as
	// it was read (readExpect, expectations).
	s.status.Lock()
	switch {
	case fileErr != nil:
		s.status.errWhat, s.status.err = "reading the expectations", fmt.Sprintf("--expect %s: %s", s.cfg.Expect, fileErr)
	case bad != "":
		s.status.errWhat, s.status.err = "reading the expectations", bad
	case s.status.errWhat == "reading the expectations" || s.status.errWhat == "checking the expectations":
		s.status.err, s.status.errWhat = "", ""
	}
	s.status.Unlock()
	if !ok {
		return nil
	}
	this := ledger.Week(s.l.Day(s.now()))
	latest, err := s.l.LatestID(ctx)
	if err != nil {
		return err
	}
	// The scenarios' test files, read each round: one changed is checked
	// again, whatever else did.
	tests, tkey := s.scenarioTests(e, sources)
	key := fmt.Sprint(s.l.Gen(), "/", this, "/", ekey, "/", latest, "/", tkey)
	s.expect.Lock()
	same := s.expect.key == key
	s.expect.Unlock()
	if same {
		return nil
	}
	again, err := s.checkExpectations(ctx, e, sources, tests, this)
	if err != nil {
		return err
	}
	if !again {
		s.expect.Lock()
		s.expect.key = key
		s.expect.Unlock()
	}
	return nil
}

// scenarioTest is a scenario's test file as a check reads it: rewritten as
// promtool runs it (scenario.Rewrite), or why it can't be.
type scenarioTest struct {
	test []byte
	err  error
}

// scenarioTests are, with --promtool, the test files of the scenarios the
// expectations name, by where each is declared and its path: each read in
// the directory of the file that declares it, --expect's or --config's,
// and nowhere else (scenario.ReadTest); and a key of what they hold. None
// without --promtool: nothing is read.
func (s *Server) scenarioTests(e *verdict.Expectations, sources map[string]string) (map[string]scenarioTest, string) {
	if s.cfg.Promtool == "" || e == nil {
		return nil, ""
	}
	roots := map[string]*os.Root{}
	defer func() {
		for _, r := range roots {
			if r != nil {
				r.Close()
			}
		}
	}()
	out := map[string]scenarioTest{}
	h := sha256.New()
	for _, x := range e.Expectations {
		for _, p := range x.Scenarios {
			src := sources[x.Selector()+"\n"+verdict.ScenarioWhat+p]
			k := src + "\n" + p
			if _, ok := out[k]; ok {
				continue
			}
			root, ok := roots[src]
			if !ok {
				file := s.cfg.Expect
				if src == expectFromConfig {
					file = s.cfg.Config
				}
				root, _ = os.OpenRoot(filepath.Dir(file)) // nil: ReadTest says so
				roots[src] = root
			}
			b, err := scenario.ReadTest(root, p)
			var t []byte
			if err == nil {
				t, err = scenario.Rewrite(b)
			}
			out[k] = scenarioTest{t, err}
			fmt.Fprintf(h, "%q %q %x %v\n", src, p, sha256.Sum256(t), err)
		}
	}
	return out, hex.EncodeToString(h.Sum(nil)[:8])
}

// scenarioGroups are, of the rules in effect now (the latest version), the
// group of each rule promtool runs, as scenario.Group writes it, by the
// name the rule is judged under: its alert rules as the version keeps
// them, and its recording rules. Read once a version.
func (s *Server) scenarioGroups(ctx context.Context) (map[string][]byte, error) {
	id, err := s.l.LatestID(ctx)
	if err != nil || id == 0 {
		return map[string][]byte{}, err
	}
	st := &s.expect
	st.Lock()
	if st.groupsOf == id && st.groups != nil {
		g := st.groups
		st.Unlock()
		return g, nil
	}
	st.Unlock()
	var recs []rules.Recording
	all, names, err := s.namedRec(ctx, id, &recs)
	if err != nil {
		return nil, err
	}
	alerts := map[string][]rules.Rule{}
	for _, r := range all {
		k := scenario.GroupKey(r.Source, r.Namespace, r.Group)
		alerts[k] = append(alerts[k], r)
	}
	recorded := map[[2]string][]rules.Recording{}
	for _, r := range recs {
		k := [2]string{r.Source, r.Group}
		recorded[k] = append(recorded[k], r)
	}
	written := map[string][]byte{}
	out := map[string][]byte{}
	for j, r := range all {
		if names[j] == "" || !scenario.Runs(r) {
			continue
		}
		k := scenario.GroupKey(r.Source, r.Namespace, r.Group)
		g, ok := written[k]
		if !ok {
			if g, err = scenario.Group(alerts[k], recorded[[2]string{r.Source, r.Group}]); err != nil {
				continue
			}
			written[k] = g
		}
		out[names[j]] = g
	}
	st.Lock()
	st.groupsOf, st.groups = id, out
	st.Unlock()
	return out, nil
}

// promtool is --promtool as a scenario runs it: each run within its time
// limit, the output it keeps and the memory it may take
// (--promtool-memory), one at a time across serve.
func (s *Server) promtool() servePromtool {
	return servePromtool{scenario.Promtool{Path: s.cfg.Promtool, Memory: s.cfg.PromtoolMemory}, &s.promtoolTurn}
}

// servePromtool is promtool as serve runs it: one run at a time, a
// round's scenario and an agent's alike (test_expectation,
// propose_expectation, a new rule's check), so that no two take
// --promtool-memory each beside serve's own under the container's one
// limit. Each waits its turn within its own context (an agent's check,
// its deadline), never holding the ledger's writer, and one whose turn
// didn't come is not run, saying so.
type servePromtool struct {
	scenario.Promtool
	turn *oneTurn
}

// whyPromtoolBusy is a run whose turn didn't come within its time.
const whyPromtoolBusy = "not run: another scenario was running, and serve runs promtool one at a time; its turn didn't come within this check's time"

// Run runs promtool as scenario.Promtool.Run does, once its turn comes.
func (p servePromtool) Run(ctx context.Context, group, test []byte) scenario.Result {
	if !p.turn.take(ctx) {
		return scenario.Result{State: scenario.Unrun, Why: whyPromtoolBusy}
	}
	defer p.turn.give()
	return p.Promtool.Run(ctx, group, test)
}

// oneTurn is one turn, taken within a context: a channel of one, made as
// it is first taken.
type oneTurn struct {
	once sync.Once
	c    chan struct{}
}

// take waits for the turn, as long as ctx lasts: false when it ended
// first.
func (t *oneTurn) take(ctx context.Context) bool {
	t.once.Do(func() { t.c = make(chan struct{}, 1) })
	if ctx.Err() != nil {
		return false
	}
	select {
	case t.c <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// give gives back the turn taken.
func (t *oneTurn) give() { <-t.c }

// scenarioKey is what a scenario is run on: promtool, the group and the
// test file, as its row's version keeps it.
func scenarioKey(promtool string, group, test []byte) string {
	h := sha256.New()
	fmt.Fprintf(h, "%q\x00%d\x00%s\x00%s", promtool, len(group), group, test)
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// sameFound says two checks of a scenario found the same, whatever week.
func sameFound(a, b ledger.ExpectCheck) bool {
	return a.Result == b.Result && a.Why == b.Why && a.Value == b.Value && a.Version == b.Version && a.Detail == b.Detail
}

// maxDetail is the most bytes of what promtool said, or why it couldn't
// run a scenario, a check keeps.
const maxDetail = 2000

// checkExpectations is a round's check; again says some of it was left for
// the next round (rows past MaxExpectWrites, catches to judge again,
// scenarios to run).
func (s *Server) checkExpectations(ctx context.Context, e *verdict.Expectations, sources map[string]string, tests map[string]scenarioTest, this int64) (again bool, err error) {
	first := this - (expectWeeks-1)*7
	start := s.l.Midnight(first)
	table, err := s.l.RuleTable(ctx)
	if err != nil {
		return false, err
	}
	own, err := s.owners(ctx)
	if err != nil {
		return false, err
	}
	rs, err := s.expectRules(ctx, start, table, own)
	if err != nil {
		return false, err
	}
	pairs, err := expectPairs(rs, e, sources)
	if err != nil {
		return false, err
	}
	asleep, wantPages, catches := false, false, map[string]bool{}
	for _, p := range pairs {
		if p.it.why != "" {
			continue
		}
		asleep = asleep || p.it.Asleep()
		wantPages = wantPages || p.it.Pages()
		if p.it.Catch != nil {
			catches[p.it.Catch.Incident] = true
		}
	}
	// A rule only copied in is known where its source's copies cover it,
	// in any span the ledger keeps of them: elsewhere, a week no firing was
	// copied in for isn't a week none fired. Nor is one in a period the
	// rule alone was left out of them (ledger.LeaveOut).
	covered := map[string][][2]time.Time{}
	leftOut := map[string]map[string][][2]time.Time{}
	for _, r := range rs {
		row, ok := table[r.id]
		if r.def != nil || !ok || r.id == 0 || row.Source == "" {
			continue
		}
		spans, seen := covered[row.Source]
		if !seen {
			if spans, err = s.l.Spans(ctx, row.Source); err != nil {
				return false, err
			}
			covered[row.Source] = spans
			if leftOut[row.Source], err = s.l.LeftOut(ctx, row.Source); err != nil {
				return false, err
			}
		}
		r.copied, r.spans = true, ledger.Without(spans, leftOut[row.Source][row.Sent])
	}
	old, err := s.l.ExpectChecks(ctx, ledger.ExpectQuery{})
	if err != nil {
		return false, err
	}
	o, err := s.options(ctx)
	if err != nil {
		return false, err
	}
	// The weeks' tallies, by rule: one read of the weeks table.
	t := &tallies{by: map[int64]map[int64]ledger.Tally{}, o: o, rules: table}
	for w := first; w <= this; w += 7 {
		t.weeks = append(t.weeks, w)
		t.by[w] = map[int64]ledger.Tally{}
	}
	ws, err := s.l.Weeks(ctx, first, this)
	if err != nil {
		return false, err
	}
	for _, x := range ws {
		if t.by[x.At] != nil {
			t.by[x.At][x.Rule] = x
		}
	}
	firstFiring, _, err := s.l.Bounds(ctx)
	if err != nil {
		return false, err
	}
	// Where the ledger's firings are up to: a week past it isn't all in
	// the ledger yet, and is so far, as the week under way is. With no
	// runs, what was copied in is as far as it goes.
	upTo := s.now()
	if s.running(ctx) {
		if upTo, err = s.l.Cursor(ctx); err != nil {
			return false, err
		}
	}
	// The pages worked out of the firings that started in each week, by
	// rule, and the span they are worked out over, for a budget of pages.
	var pages map[int64]map[string]int
	var pagesUnknown map[int64]bool
	var pagesCover ledger.RoutedCover
	pagesKnown := false
	if s.notifies() && wantPages {
		if pagesCover, err = s.l.RoutedCover(ctx); err != nil {
			return false, err
		}
		if pagesKnown = pagesCover.Any(); pagesKnown {
			if pages, pagesUnknown, err = s.l.RoutedPagesByWeek(ctx, s.l.Midnight(first), s.l.Midnight(this+7)); err != nil {
				return false, err
			}
		}
	}
	var hours map[int64]map[int64]ledger.HourTally
	sc := own.schedules(o.Hours)
	if asleep {
		if hours, err = s.byHours(ctx, t); err != nil {
			return false, err
		}
	}
	var keys []string
	for k := range catches {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	incs, err := s.l.IncidentsByKey(ctx, keys)
	if err != nil {
		return false, err
	}
	was := map[ledger.ExpectKey]ledger.ExpectCheck{}
	for _, c := range old {
		was[c.Key()] = c
	}
	now := s.now()
	var found []ledger.ExpectCheck
	type rejudge struct {
		at  int // in found
		r   *expectRule
		inc ledger.Incident
		c   verdict.ExpectCatch
	}
	var pending []rejudge
	// Scenarios: each rule's latest row, whatever its week; the groups
	// of the rules in effect, read once a round there are any to run; those
	// to run, expectScenariosARun a round.
	lastOf := map[[2]string]ledger.ExpectCheck{}
	for _, c := range old {
		if k := [2]string{c.Rule, c.Expect}; c.Scenario() {
			// The current one, and of two (a run begun, kept apart, beside
			// the row it was to replace) the later week's.
			if p, ok := lastOf[k]; !ok || !p.Current && c.Current || p.Current == c.Current && c.Week > p.Week {
				lastOf[k] = c
			}
		}
	}
	var groups map[string][]byte
	type scenarioRun struct {
		at          int // in found
		group, test []byte
		key         string
	}
	var runs []scenarioRun
	for _, p := range pairs {
		r, it := p.r, p.it
		base := ledger.ExpectCheck{Rule: r.name, Expect: ledger.ExpectHash(it.selector, it.What, it.Held), What: it.What, Held: it.Held,
			Selector: it.selector, Source: it.source, Value: "–"}
		if it.why != "" {
			// An annotation said, holding the rule to nothing.
			base.Week, base.Result, base.Why = this, ledger.ExpectUnknown, it.why
			found = append(found, base)
			continue
		}
		if it.Scenario != "" {
			// Of the week it is found in; run again only when promtool,
			// the rule's group or its test file changed.
			c := base
			c.Week, c.Result = this, ledger.ExpectUnknown
			prev, had := lastOf[[2]string{r.name, c.Expect}]
			tf, read := tests[it.source+"\n"+it.Scenario]
			switch {
			case s.cfg.Promtool == "":
				c.Why = whyNoPromtool
			case r.def == nil || !scenario.Runs(*r.def):
				c.Why = whyNotProm
			case !read || tf.err != nil:
				c.Why = whyTestUnread
				if tf.err != nil {
					c.Detail = cutText(rules.Shown(tf.err.Error()), maxDetail)
				}
			default:
				if groups == nil {
					if groups, err = s.scenarioGroups(ctx); err != nil {
						return false, err
					}
				}
				g, ok := groups[r.name]
				if !ok {
					c.Why = whyNotProm
					break
				}
				key := scenarioKey(s.cfg.Promtool, g, tf.test)
				if had && prev.Version == key {
					c.Result, c.Value, c.Why, c.Detail, c.Version = prev.Result, prev.Value, prev.Why, prev.Detail, prev.Version
					break
				}
				c.Why = whyNotRunYet
				runs = append(runs, scenarioRun{len(found), g, tf.test, key})
			}
			if had && sameFound(prev, c) {
				c.Week = prev.Week
			}
			found = append(found, c)
			continue
		}
		if it.Catch != nil {
			c := base
			in, ok := incs[it.Catch.Incident]
			switch {
			case !ok:
				c.Result, c.Why = ledger.ExpectUnknown, whyNoIncident
			case r.unreplayed:
				c.Week, c.At, c.Result, c.Why = ledger.Week(s.l.Day(in.Start)), in.Start, ledger.ExpectUnknown, whyUnreplayed
			case r.copied && !slices.ContainsFunc(in.CaughtBy, func(k verdict.Catch) bool { return k.Rule == r.name }) &&
				!r.copiedOver(in.Start, in.Start.Add(time.Duration(it.Catch.Within)), false):
				// No span of its copies covers the incident and its delay
				// (one ending before, or beginning after, it began; a gap
				// within it): not caught in them isn't missed.
				c.Week, c.At, c.Result, c.Why = ledger.Week(s.l.Day(in.Start)), in.Start, ledger.ExpectUnknown, whyNotCopied
			default:
				c.Week, c.At = ledger.Week(s.l.Day(in.Start)), in.Start
				value, state := verdict.CaughtWithin(in.CaughtBy, r.name, *it.Catch)
				c.Value, c.Result = value, state
				if r.since.After(in.Start) && r.def != nil {
					// The rule changed since: judged again on it as it is.
					prev, had := was[c.Key()]
					switch {
					case had && prev.Version == r.key:
						c.Result, c.Value, c.Why, c.Version = prev.Result, prev.Value, prev.Why, prev.Version
					case in.Start.Before(start):
						c.Result, c.Why, c.Value = ledger.ExpectUnknown, whyTooOld, value
					default:
						c.Result, c.Why, c.Value = ledger.ExpectUnknown, whyNotJudged, value
						pending = append(pending, rejudge{len(found), r, in, *it.Catch})
					}
				}
			}
			found = append(found, c)
			continue
		}
		h := sc.of(r.name)
		for w := first; w <= this; w += 7 {
			wFrom, wTo := s.l.Midnight(w), s.l.Midnight(w+7)
			if !r.from.IsZero() && !r.from.Before(wTo) {
				continue // not in effect yet
			}
			c := base
			c.Week = w
			x := t.by[w][r.id]
			n := x.Firings
			switch {
			case it.Asleep():
				n = hours[w][r.id].Asleep
			case it.Refires():
				n = x.Refired
			case it.Pages():
				n = pages[w][r.name]
			}
			c.N, c.Value = &n, fmt.Sprint(n)
			whole := !firstFiring.IsZero() && firstFiring.Before(wFrom) && (r.from.IsZero() || !r.from.After(wFrom))
			switch {
			case r.unreplayed:
				c.Result, c.Why, c.N, c.Value = ledger.ExpectUnknown, whyUnreplayed, nil, "–"
			case it.Pages() && !s.notifies():
				c.Result, c.Why, c.N, c.Value = ledger.ExpectUnknown, whyPagesNotYet, nil, "–"
			case it.Pages() && (!pagesKnown || !pagesCover.Covers(wFrom, minTime(wTo, upTo)) || pagesUnknown[w]):
				// Not worked out for all of the week, or for the part of it
				// that has gone, or the work of an hour of it stopped or
				// failed, or of one a firing that began in it was on in: a
				// gap isn't none paged.
				c.Result, c.Why, c.N, c.Value = ledger.ExpectUnknown, whyPagesNotCovered, nil, "–"
			case it.Asleep() && (h == nil || !h.Sleeps()):
				c.Result, c.Why, c.N, c.Value = ledger.ExpectUnknown, whyNoHours, nil, "–"
			case n > *it.Limit:
				// Over, in the week so far or a week held in part too.
				c.Result = ledger.ExpectBroken
			case r.copied && !r.copiedOver(wFrom, wTo, w == this):
				// Not inside a span of its source's copies: a week before
				// the first, one touching a gap, a whole week past the
				// last, or the week so far the last didn't reach from its
				// start.
				c.Result, c.Why = ledger.ExpectUnknown, whyNotCopied
			case w == this || upTo.Before(wTo):
				c.Result, c.Why = ledger.ExpectOpen, whySoFar
			case firstFiring.IsZero() || !firstFiring.Before(wTo):
				c.Result, c.Why, c.N, c.Value = ledger.ExpectUnknown, whyNotHeld, nil, "–"
			case !whole:
				c.Result, c.Why = ledger.ExpectUnknown, whyNotWhole
			default:
				c.Result = ledger.ExpectMet
			}
			found = append(found, c)
		}
	}
	// Catches of rules changed since, judged again: a few a round, within
	// one --timeout, while the runs don't wait.
	if len(pending) > 0 && s.replaying() && !s.runsWait() {
		rctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
		for i, p := range pending {
			if i == expectReplaysARun || rctx.Err() != nil {
				again = true
				break
			}
			value, state, why, err := s.judgeCatchAgain(rctx, p.r, p.inc, p.c, o)
			if err != nil {
				s.log.Warn("judging a catch again on the rule as it is", "rule", p.r.name, "incident", p.inc.ID, "err", err)
			}
			c := &found[p.at]
			c.Value, c.Result, c.Why, c.Version = value, state, why, p.r.key
		}
		cancel()
	}
	// Scenarios to run: a few a round, each within promtool's own time
	// limit and memory cap, never while the ledger's writer is held, and
	// one at a time across serve: each waits its turn (an agent's check
	// may be running), and is then kept as begun before promtool starts,
	// and as found once it ends: a run that never ends (lapilli serve
	// killed as promtool took its memory) is kept so, and not run again by
	// the next leader until what it is run on changes; one that waited and
	// never began isn't.
	p := s.promtool()
	for i, x := range runs {
		if i == expectScenariosARun || ctx.Err() != nil {
			again = true
			break
		}
		if !p.turn.take(ctx) {
			again = true
			break
		}
		err := func() error {
			defer p.turn.give()
			c := &found[x.at]
			c.Version, c.Week = x.key, this
			c.Result, c.Value, c.Why, c.Detail = ledger.ExpectUnknown, "–", whyRunFailed, scenarioBegun
			if err := s.l.KeepScenarioRun(ctx, *c, this, now); err != nil {
				return err
			}
			res := scenario.Judged(p.Promtool.Run(ctx, x.group, x.test))
			switch res.State {
			case verdict.ExpectMet:
				c.Result, c.Value, c.Why, c.Detail = ledger.ExpectMet, "passes", "", ""
			case verdict.ExpectBroken:
				c.Result, c.Value, c.Why, c.Detail = ledger.ExpectBroken, "fails", "", cutText(res.Said, maxDetail)
			default:
				c.Result, c.Value, c.Why, c.Detail = ledger.ExpectUnknown, "–", whyRunFailed, res.Why
			}
			wctx, cancel := ctx, context.CancelFunc(func() {})
			if ctx.Err() != nil {
				// lapilli serve stopped the run itself, as it stops: not a
				// run that died. Kept with no version, so that it is run
				// again; what it found, if it ended first, kept as found.
				wctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), s.cfg.Timeout)
				if c.Result == ledger.ExpectUnknown {
					c.Detail, c.Version = scenarioStopped, ""
				}
			}
			defer cancel()
			return s.l.KeepScenarioRun(wctx, *c, this, now)
		}()
		if err != nil {
			return false, err
		}
	}
	left, err := s.l.KeepExpectChecks(ctx, found, old, first, this, now)
	if err != nil {
		return false, err
	}
	again = again || left > 0
	if !s.still(ctx) {
		return again, nil
	}
	// What is told and made a task of, as the ledger now keeps it.
	cur, err := s.l.ExpectChecks(ctx, ledger.ExpectQuery{Broken: true, Current: true, From: this - 7})
	if err != nil {
		return again, err
	}
	return again, errors.Join(s.tellExpect(ctx, cur, this), s.expectTasks(ctx, cur, this, own))
}

// judgeCatchAgain replays a rule as it is now over an incident, and says
// whether it catches it within the catch's delay: its delay, the state,
// and why when not known.
func (s *Server) judgeCatchAgain(ctx context.Context, r *expectRule, in ledger.Incident, k verdict.ExpectCatch, o verdict.Options) (string, string, string, error) {
	end := in.End
	if end.IsZero() || end.Before(in.Start) {
		end = in.Start.Add(o.UnknownLen)
	}
	from := in.Start.Add(-o.Lead - r.def.For - time.Hour).Truncate(s.cfg.Step)
	to := end.Add(time.Duration(k.Within) + time.Hour)
	if now := s.now(); now.Before(to) {
		to = now
	}
	to = to.Truncate(s.cfg.Step)
	cmp, err := s.replayCompared(ctx, s.h, s.cfg.Parallel, nil, []rules.Rule{*r.def}, 0, from, to, s.cfg.Timeout, o)
	if err != nil {
		return "–", ledger.ExpectUnknown, whyReplayFailed, err
	}
	for _, rc := range cmp.Rules {
		if rc.After != nil && rc.After.Unsupported != "" {
			return "–", ledger.ExpectUnknown, whyReplayFailed, nil
		}
	}
	for _, ir := range cmp.After.Incidents {
		if ir.ID == in.ID {
			value, state := verdict.CaughtWithin(ir.CaughtBy, r.name, k)
			return value, state, "", nil
		}
	}
	return "–", ledger.ExpectUnknown, whyReplayFailed, nil
}

// expectNotice is what expectation.broken tells of an expectation broken:
// of a scenario, its path (Scenario) and the week it was found failing in.
type expectNotice struct {
	Rule     string    `json:"rule"`
	Team     string    `json:"team,omitempty"`
	What     string    `json:"what"`
	Held     string    `json:"held"`
	Selector string    `json:"selector"`
	Source   string    `json:"source"`
	N        *int      `json:"n,omitempty"`
	Value    string    `json:"value"`
	SoFar    bool      `json:"so_far,omitempty"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Incident string    `json:"incident,omitempty"`
	Scenario string    `json:"scenario,omitempty"`
}

// check is the check a notice tells of, as expectSentence says it.
func (n *expectNotice) check(s *Server) ledger.ExpectCheck {
	c := ledger.ExpectCheck{Rule: n.Rule, What: n.What, Held: n.Held, N: n.N, Value: n.Value}
	if n.Incident == "" {
		c.Week = ledger.Week(s.l.Day(n.From))
	}
	return c
}

// expectThis is, for expectSentence, the week a notice's was under way in:
// its own when it was told so far.
func (s *Server) expectThis(n *expectNotice) int64 {
	if n.SoFar {
		return ledger.Week(s.l.Day(n.From))
	}
	return -1
}

// expectShown are the checks of the expectations broken, in the order
// they are said: the week so far first, then the last whole week, then
// catches and scenarios, broken whatever their week, each by rule; of a
// rule's budget broken in both weeks, the latest alone.
func expectShown(cs []ledger.ExpectCheck, this int64) []ledger.ExpectCheck {
	var out []ledger.ExpectCheck
	seen := map[[2]string]bool{}
	slices.SortStableFunc(cs, func(a, b ledger.ExpectCheck) int {
		rank := func(c ledger.ExpectCheck) int {
			if c.Weekless() {
				return 2
			}
			if c.Week == this {
				return 0
			}
			return 1
		}
		return cmp.Or(cmp.Compare(rank(a), rank(b)), strings.Compare(a.Rule, b.Rule), strings.Compare(a.What, b.What))
	})
	for _, c := range cs {
		if c.Result != ledger.ExpectBroken || !c.Current || !c.Weekless() && c.Week < this-7 {
			continue
		}
		k := [2]string{c.Rule, c.Expect}
		if !seen[k] {
			seen[k] = true
			out = append(out, c)
		}
	}
	return out
}

// tellExpect tells expectation.broken of each expectation broken in the
// week so far or the last whole week, or a catch lost, not yet told:
// once a rule, an expectation and a week, expectToldARun a round, the rest
// in the rounds after. To where what is told once goes (toldTo), each
// notice where the routes send it, by its rule's team and labels.
func (s *Server) tellExpect(ctx context.Context, cs []ledger.ExpectCheck, this int64) error {
	dests := s.toldTo()
	if len(dests) == 0 {
		return nil
	}
	own, err := s.owners(ctx)
	if err != nil {
		return err
	}
	var keys []ledger.ExpectKey
	var told [][]ledger.Told
	for _, c := range expectShown(cs, this) {
		if c.Told != 0 {
			continue
		}
		if len(keys) == expectToldARun {
			break
		}
		n := expectNotice{Rule: c.Rule, What: c.What, Held: c.Held, Selector: c.Selector, Source: c.Source, N: c.N, Value: c.Value, SoFar: !c.Weekless() && c.Week == this}
		if t, ok := own.byID[own.rule[c.Rule]]; ok {
			n.Team = t.Name
		}
		switch {
		case c.Catch():
			n.Incident, n.From = strings.TrimPrefix(c.What, "catches "), c.At.UTC()
			n.To = n.From
		case c.Scenario():
			n.Scenario = strings.TrimPrefix(c.What, verdict.ScenarioWhat)
			n.From, n.To = s.l.Midnight(c.Week).UTC(), s.l.Midnight(c.Week+7).UTC()
		default:
			n.From, n.To = s.l.Midnight(c.Week).UTC(), s.l.Midnight(c.Week+7).UTC()
		}
		var ts []ledger.Told
		for _, d := range dests {
			ts = append(ts, ledger.Told{Dest: d, About: map[string]any{"expectation": n}})
		}
		keys, told = append(keys, c.Key()), append(told, ts)
	}
	if len(keys) == 0 {
		return nil
	}
	return s.l.TellExpectBroken(ctx, keys, this, told)
}

// expectTaskIn is the task an expectation broken becomes, in a language:
// of kind tune, about the rule, for its team, titled with what was broken,
// its text where it is held and the rule's tab of its expectations.
func (s *Server) expectTaskIn(l string, c ledger.ExpectCheck, team int64, this int64) ledger.Task {
	link := "/rules/" + url.PathEscape(c.Rule) + "?tab=expectations"
	if s.cfg.ExternalURL != "" {
		link = strings.TrimRight(s.cfg.ExternalURL, "/") + link
	}
	body := s.expectSentence(l, c, this) + "\n\n" + tr(l, "Held by %s (%s). Tune the rule, or change what it is held to where it is declared. Its expectations: %s",
		c.Selector, tr(l, expectSourceText[c.Source]), link)
	return ledger.Task{Title: cutText(s.expectTitle(l, c, this), 200), Kind: "tune", Rule: c.Rule, TeamID: team, Body: body}
}

// expectTasks makes, with --expect-tasks, a task of each expectation
// broken (expectShown) for the rule's team: none while one made of the
// rule's expectation is open, none twice in a week for a rule and an
// expectation; expectTasksARun a round.
func (s *Server) expectTasks(ctx context.Context, cs []ledger.ExpectCheck, this int64, own *owners) error {
	if !s.cfg.ExpectTasks {
		return nil
	}
	all, err := s.l.ExpectChecks(ctx, ledger.ExpectQuery{})
	if err != nil {
		return err
	}
	tasked := map[[2]string]int64{}
	for _, c := range all {
		k := [2]string{c.Rule, c.Expect}
		tasked[k] = max(tasked[k], c.Tasked)
	}
	open, err := s.l.OpenExpectTasks(ctx)
	if err != nil {
		return err
	}
	isOpen := map[[2]string]bool{}
	for k := range open {
		isOpen[[2]string{k.Rule, k.Expect}] = true
	}
	made := 0
	var errs []error
	for _, c := range expectShown(cs, this) {
		k := [2]string{c.Rule, c.Expect}
		if isOpen[k] || tasked[k] >= this {
			continue
		}
		if made == expectTasksARun {
			break
		}
		made++
		if _, err := s.l.MakeExpectTask(ctx, s.expectTaskIn(s.cfg.Lang, c, own.rule[c.Rule], this), c.Key(), this, ledger.ExpectTasksBy); err != nil &&
			!errors.Is(err, ledger.ErrExpectTaskOpen) && !errors.Is(err, ledger.ErrExpectNotBroken) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// expectSourceText names where an expectation is declared.
var expectSourceText = map[string]string{
	expectFromFile:       "the expectations file",
	expectFromConfig:     "serve's configuration",
	expectFromAnnotation: "the rule's annotation",
}

// mondayOf is a week's Monday, as a person reads it.
func (s *Server) mondayOf(w int64) string { return s.l.Midnight(w).In(s.cfg.Loc).Format("2006-01-02") }

// expectSentence says what an expectation broken came to, in a language.
func (s *Server) expectSentence(l string, c ledger.ExpectCheck, this int64) string {
	name := verdict.Clean(c.Rule)
	if c.Catch() {
		inc := verdict.Clean(strings.TrimPrefix(c.What, "catches "))
		if c.Value == "✗" || c.Value == "–" {
			return tr(l, "%s didn't catch %s, which it is expected to catch", name, inc)
		}
		return tr(l, "%s caught %s %s after it began, expected within %s", name, inc, strings.TrimPrefix(c.Value, "+"), strings.TrimPrefix(c.Held, "within "))
	}
	if c.Scenario() {
		return tr(l, "%s fails the scenario %s, which it is expected to pass", name, verdict.Clean(strings.TrimPrefix(c.What, verdict.ScenarioWhat)))
	}
	n := 0
	if c.N != nil {
		n = *c.N
	}
	week := s.mondayOf(c.Week)
	switch {
	case c.What == "budget asleep" && c.Week == this:
		return tr(l, "%s has fired %d times while people slept so far in the week of %s, held to at most %s", name, n, week, c.Held)
	case c.What == "budget asleep":
		return tr(l, "%s fired %d times while people slept in the week of %s, held to at most %s", name, n, week, c.Held)
	case c.What == "budget refires" && c.Week == this:
		return tr(l, "%s has re-fired %d times so far in the week of %s, held to at most %s", name, n, week, c.Held)
	case c.What == "budget refires":
		return tr(l, "%s re-fired %d times in the week of %s, held to at most %s", name, n, week, c.Held)
	case c.Week == this:
		return tr(l, "%s has fired %d times so far in the week of %s, held to at most %s", name, n, week, c.Held)
	}
	return tr(l, "%s fired %d times in the week of %s, held to at most %s", name, n, week, c.Held)
}

// expectTitle is the title of a task made of an expectation broken.
func (s *Server) expectTitle(l string, c ledger.ExpectCheck, this int64) string {
	name := verdict.Clean(c.Rule)
	if c.Catch() {
		return tr(l, "%s no longer catches %s as expected", name, verdict.Clean(strings.TrimPrefix(c.What, "catches ")))
	}
	if c.Scenario() {
		return tr(l, "%s fails its scenario %s", name, verdict.Clean(strings.TrimPrefix(c.What, verdict.ScenarioWhat)))
	}
	return tr(l, "%s over its budget, week of %s", name, s.mondayOf(c.Week))
}
