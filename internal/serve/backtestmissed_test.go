package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// Held to the incidents (fail_missed), a backtest through serve says which
// incidents the change leaves caught by no rule, as lapilli verdict
// --fail-missed does: the rules that differ judged by the backtest, those
// left alone by serve's record of them. INC-1 is caught in the ledger by
// Errors, as serve runs it, and by MoreErrors where serve runs that too.

// rulesOf is a rule file of the shop group holding each rule given.
func rulesOf(rs ...string) string {
	return "groups:\n  - name: shop\n    rules:\n" + strings.Join(rs, "")
}

const (
	errorsRule     = "      - alert: Errors\n        expr: errors > 5\n        for: 2m\n        labels: {severity: page}\n"
	errorsLate     = "      - alert: Errors\n        expr: errors > 5\n        for: 2h\n        labels: {severity: page}\n"
	errorsBroken   = "      - alert: Errors\n        expr: nope(up\n        for: 2m\n        labels: {severity: page}\n"
	moreErrorsRule = "      - alert: MoreErrors\n        expr: errors > 1\n        for: 5m\n"
	elsewhereRule  = "      - alert: Elsewhere\n        expr: errors > 1\n        for: 5m\n        labels: {team: other}\n"
)

// btServing is btServer running the rules of file.
func btServing(t *testing.T, file string) *btSet {
	t.Helper()
	return btServer(t, nil, "", func(c *Config) {
		dir := filepath.Join(t.TempDir(), "rules")
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(file), 0o644)
		c.RulesPath = dir
	})
}

// missedOf is a held backtest's summary and the incidents it says missed.
func missedOf(t *testing.T, rep map[string]any) (map[string]any, []string) {
	t.Helper()
	sum, _ := rep["summary"].(map[string]any)
	var ids []string
	list, _ := rep["missed"].([]any)
	for _, x := range list {
		m, _ := x.(map[string]any)
		for k := range m {
			if k != "id" && k != "start" && k != "before" && k != "after" {
				t.Errorf("a missed incident's %s, which the report leaves out of incidents", k)
			}
		}
		ids = append(ids, fmt.Sprint(m["id"]))
	}
	return sum, ids
}

// A change that leaves INC-1 caught by no rule, its only catch made
// later than it lasts, is said so: in the summary, the report's missed
// and the comment, which says the run fails. Not held, it says nothing of
// it, as before.
func TestBacktestFailMissed(t *testing.T) {
	parallel(t)
	b := btServing(t, rulesOf(errorsRule))
	held := map[string]any{"days": 3, "fail_missed": true}
	md, rep := b.done(t, b.plain, btBody(rulesOf(errorsRule), rulesOf(errorsLate), held))
	sum, missed := missedOf(t, rep)
	if sum["missed_checked"] != true || sum["incidents_missed"] != float64(1) || sum["missed_not_known"] != float64(0) ||
		len(missed) != 1 || missed[0] != "INC-1" {
		t.Fatalf("summary %v, missed %v\n%s", sum, missed, md)
	}
	if !strings.Contains(md, "**This run fails** (`--fail-missed`): 1 incidents a rule caught before the change are caught by no rule after it, "+
		"the rules left alone among them, as lapilli serve's record has them.") || !strings.Contains(md, "| `INC-1` | 2026-11-02 06:01 | `Errors` +1m |") {
		t.Errorf("the comment:\n%s", md)
	}
	if strings.Contains(md, "Checkout down") || strings.Contains(fmt.Sprint(rep["missed"]), "Checkout down") {
		t.Errorf("a title the entry didn't ask for:\n%s", md)
	}
	md, rep = b.done(t, b.plain, btBody(rulesOf(errorsRule), rulesOf(errorsLate), map[string]any{"days": 3}))
	if sum, missed := missedOf(t, rep); sum["missed_checked"] != false || len(missed) != 0 || strings.Contains(md, "--fail-missed") {
		t.Errorf("not held: summary %v, missed %v\n%s", sum, missed, md)
	}
	// Nothing that differs: held, nothing missed, as lapilli verdict says.
	_, rep = b.done(t, b.plain, btBody(rulesOf(errorsRule), rulesOf(errorsRule), held))
	if sum, missed := missedOf(t, rep); sum["nothing_changed"] != true || sum["missed_checked"] != true || len(missed) != 0 {
		t.Errorf("nothing changed: summary %v, missed %v", sum, missed)
	}
	if code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", b.plain, btBody(ruleFile, changedRules, map[string]any{"fail_missed": "yes"})); code != 400 {
		t.Errorf("fail_missed a string: %d %v", code, m)
	}
}

// A rule the change leaves alone that serve's record says caught INC-1
// keeps it caught: one the pull request holds, alike on both sides, or one
// of serve's the pull request doesn't hold at all. Nothing of them is
// asked of the history.
func TestBacktestFailMissedLeftAlone(t *testing.T) {
	parallel(t)
	b := btServing(t, rulesOf(errorsRule, moreErrorsRule))
	held := map[string]any{"days": 3, "fail_missed": true}
	for name, sides := range map[string][2]string{
		"held alike":       {rulesOf(errorsRule, moreErrorsRule), rulesOf(errorsLate, moreErrorsRule)},
		"serve's own only": {rulesOf(errorsRule), rulesOf(errorsLate)},
	} {
		md, rep := b.done(t, b.plain, btBody(sides[0], sides[1], held))
		sum, missed := missedOf(t, rep)
		if sum["missed_checked"] != true || sum["incidents_missed"] != float64(0) || sum["missed_not_known"] != float64(0) || len(missed) != 0 {
			t.Errorf("%s: summary %v, missed %v\n%s", name, sum, missed, md)
		}
		if !strings.Contains(md, "Every incident a rule caught before the change is caught after it too (`--fail-missed`).") {
			t.Errorf("%s: the comment:\n%s", name, md)
		}
	}
	// Taken away too, MoreErrors catches it no more: missed.
	_, rep := b.done(t, b.plain, btBody(rulesOf(errorsRule, moreErrorsRule), rulesOf(errorsLate), held))
	if sum, missed := missedOf(t, rep); sum["incidents_missed"] != float64(1) || len(missed) != 1 {
		t.Errorf("both taken away: summary %v, missed %v", sum, missed)
	}
}

// What can't be told stays not known, never missed: a rule after the
// change that couldn't be replayed, or a rule the pull request leaves alone
// that serve couldn't replay, and the replay of it fails too.
func TestBacktestFailMissedNotKnown(t *testing.T) {
	parallel(t)
	held := map[string]any{"days": 3, "fail_missed": true}
	for _, c := range []struct {
		name, serving, before, after string
	}{
		{"after unreplayed", rulesOf(errorsRule), rulesOf(errorsRule), rulesOf(errorsBroken)},
		{"left alone, unreplayable in serve", rulesOf(errorsRule, strings.Replace(moreErrorsRule, "errors > 1", "nope(up)", 1)),
			rulesOf(errorsRule, strings.Replace(moreErrorsRule, "errors > 1", "nope(up)", 1)), rulesOf(errorsLate, strings.Replace(moreErrorsRule, "errors > 1", "nope(up)", 1))},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := btServing(t, c.serving)
			md, rep := b.done(t, b.plain, btBody(c.before, c.after, held))
			sum, missed := missedOf(t, rep)
			if sum["missed_checked"] != true || sum["incidents_missed"] != float64(0) || sum["missed_not_known"] != float64(1) || len(missed) != 0 {
				t.Fatalf("summary %v, missed %v\n%s", sum, missed, md)
			}
			if !strings.Contains(md, "1 incidents a rule caught before the change are caught by no rule after it that lapilli serve can tell") || strings.Contains(md, "This run fails") {
				t.Errorf("the comment:\n%s", md)
			}
		})
	}
}

// A catch in serve's record was made by the rule's text then; after the
// change, a rule left alone runs as it is now. MoreErrors caught INC-1
// while it held 5m; a later version holds it 2h, and catches it no more.
// A pull request that leaves MoreErrors as it is now, at 2h, and makes
// Errors miss INC-1 too, isn't kept caught by MoreErrors' old catch: serve's
// record can't settle it, so MoreErrors is replayed as it is after the
// change, at 2h, and catching nothing, INC-1 is missed. So too of a rule
// serve runs that the pull request doesn't hold.
func TestBacktestFailMissedAnotherText(t *testing.T) {
	parallel(t)
	moreLate := strings.Replace(moreErrorsRule, "for: 5m", "for: 2h", 1)
	held := map[string]any{"days": 3, "fail_missed": true}
	for name, sides := range map[string][2]string{
		"held alike":       {rulesOf(errorsRule, moreLate), rulesOf(errorsLate, moreLate)},
		"serve's own only": {rulesOf(errorsRule), rulesOf(errorsLate)},
	} {
		t.Run(name, func(t *testing.T) {
			b := btServing(t, rulesOf(errorsRule, moreErrorsRule))
			ctx := context.Background()
			if incs, _ := b.s.l.Incidents(ctx, now.Add(-3*24*time.Hour), now, false); len(incs) != 1 || !strings.Contains(fmt.Sprint(incs[0].CaughtBy), "MoreErrors") {
				t.Fatalf("the record before: %+v", incs)
			}
			// MoreErrors made later than INC-1 lasts, from now on.
			dir := filepath.Dir(filepath.Join(b.s.cfg.RulesPath, "shop.yaml"))
			os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(rulesOf(errorsRule, moreLate)), 0o644)
			raw := ""
			if err := b.s.look(ctx, &raw); err != nil {
				t.Fatal(err)
			}
			if vs, _ := b.s.l.Versions(ctx, now.Add(-3*24*time.Hour), now, false); len(vs) != 2 {
				t.Fatalf("%d versions", len(vs))
			}
			md, rep := b.done(t, b.plain, btBody(sides[0], sides[1], held))
			sum, missed := missedOf(t, rep)
			if sum["missed_checked"] != true || sum["incidents_missed"] != float64(1) || sum["missed_not_known"] != float64(0) || len(missed) != 1 {
				t.Errorf("summary %v, missed %v\n%s", sum, missed, md)
			}
		})
	}
}

// Of serve's record, what keeps an incident caught, and what leaves it not
// known: a catch counts only by the text the rule has after the change, in
// every version over the incident's time; a catch by a rule whose name is
// told apart from a changed rule's may be that rule; a rule whose firings
// are copied in has no text to hold; a rule the pull request leaves alone
// needs the runs to cover the incident's time, and to be in effect as the
// pull request has it, and replayed, over it.
func TestLeftAloneCatches(t *testing.T) {
	parallel(t)
	t0 := time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)
	in := incident.Incident{ID: "INC-1", Start: t0, End: t0.Add(30 * time.Minute)}
	at := caughtAt{in.ID, in.Start.Unix()}
	o := verdict.Options{Lead: 15 * time.Minute, UnknownLen: time.Hour}
	now, then := aloneText{sum: [32]byte{1}, reach: 6 * time.Minute}, aloneText{sum: [32]byte{2}, reach: 6 * time.Minute}
	far := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	// One version, from before the ledger began, holding Latency as it is.
	base := func(caught ...string) *leftAlone {
		a := &leftAlone{o: o, by: map[caughtAt][]verdict.Catch{}, changed: map[string]bool{"Errors": true}, same: map[string]bool{},
			bases: map[string]bool{"Errors": true}, copied: map[string]bool{"Copied": true}, want: map[string]aloneText{"Latency": now},
			versions: []aloneVersion{{until: far, texts: map[string]aloneText{"Latency": now}, sameAll: true}}, read: true,
			runsFrom: t0.Add(-24 * time.Hour), cursor: t0.Add(24 * time.Hour)}
		for _, r := range caught {
			a.by[at] = append(a.by[at], verdict.Catch{Rule: r})
		}
		return a
	}
	// A version over the incident with Latency written otherwise, one after
	// it as it is now.
	changedSince := func(a *leftAlone, all bool) {
		a.versions = []aloneVersion{{until: t0.Add(2 * time.Hour), texts: map[string]aloneText{"Latency": then}, sameAll: all},
			{since: t0.Add(2 * time.Hour), until: far, texts: map[string]aloneText{"Latency": now}, sameAll: true}}
	}
	// Latency left alike by the pull request.
	alone := func(change func(*leftAlone), caught ...string) *leftAlone {
		a := base(append([]string{"Errors"}, caught...)...)
		a.same["Latency"], a.bases["Latency"] = true, true
		a.sameReach = now.reach
		change(a)
		return a
	}
	for _, c := range []struct {
		name          string
		a             *leftAlone
		caught, known bool
	}{
		{"the changed rule's own catch", base("Errors"), false, true},
		{"a rule serve runs, as it is now", base("Errors", "Latency"), true, true},
		{"a rule serve runs, written otherwise then", func() *leftAlone { a := base("Errors", "Latency"); changedSince(a, true); return a }(), false, false},
		{"a rule serve runs, its versions not read", func() *leftAlone { a := base("Errors", "Latency"); a.read = false; return a }(), false, false},
		{"a rule serve runs, its versions begun after it", func() *leftAlone { a := base("Errors", "Latency"); a.versions[0].since = t0; return a }(), false, false},
		{"a rule serve runs, two of its name", func() *leftAlone {
			a := base("Errors", "Latency")
			a.want["Latency"] = aloneText{ambiguous: true}
			return a
		}(), false, false},
		{"a rule serve runs no longer", base("Errors", "Gone"), false, true},
		{"a rule whose firings are copied in, none of its versions now", base("Errors", "Copied"), false, false},
		{"told apart from the changed rule", base("Errors [shop]"), false, false},
		{"told apart, and another as it is now", base("Errors [shop]", "Latency"), true, true},
		{"left alone, its record whole", alone(func(*leftAlone) {}), false, true},
		{"left alone, caught as the pull request has it", alone(func(*leftAlone) {}, "Latency"), true, true},
		{"left alone, caught as it was written then", alone(func(a *leftAlone) { changedSince(a, false); a.want["Latency"] = now }, "Latency"), false, false},
		{"left alone, written otherwise over it, uncaught", alone(func(a *leftAlone) { changedSince(a, false) }), false, false},
		{"left alone, written otherwise after it", alone(func(a *leftAlone) {
			a.versions = []aloneVersion{{until: t0.Add(31 * time.Minute), texts: map[string]aloneText{}, sameAll: true},
				{since: t0.Add(31 * time.Minute), until: far, texts: map[string]aloneText{}, sameAll: false}}
		}), false, true},
		{"one name changed and left alone", alone(func(a *leftAlone) { a.same["Errors"] = true }), false, false},
		{"left alone, the runs begun after the lead", alone(func(a *leftAlone) { a.runsFrom = t0.Add(-time.Minute) }), false, false},
		{"left alone, the cursor before the end", alone(func(a *leftAlone) { a.cursor = t0.Add(20 * time.Minute) }), false, false},
		{"left alone, no version read", alone(func(a *leftAlone) { a.read = false }), false, false},
		{"left alone, unreplayable", alone(func(a *leftAlone) { a.unreplayable = true }), false, false},
		{"left alone, a run failed over it", alone(func(a *leftAlone) { a.failed = [][2]time.Time{{t0.Add(-time.Hour), t0.Add(-14 * time.Minute)}} }), false, false},
		{"left alone, a run failed before it", alone(func(a *leftAlone) { a.failed = [][2]time.Time{{t0.Add(-time.Hour), t0.Add(-15 * time.Minute)}} }), false, true},
	} {
		if caught, known := c.a.catches(in); caught != c.caught || known != c.known {
			t.Errorf("%s: caught %t, known %t", c.name, caught, known)
		}
	}
	if nameBase("Errors [shop]") != "Errors" || nameBase("Errors [a] [b]") != "Errors" || nameBase("[x]") != "[x]" || nameBase("Errors [shop") != "Errors [shop" {
		t.Error(nameBase("Errors [shop]"), nameBase("Errors [a] [b]"))
	}
	// Two rules of a name written otherwise are one ambiguous text; the
	// same rule twice, written otherwise, isn't.
	m, texts := map[string]aloneText{}, aloneTexts{}
	addAloneText(m, "A", texts.of(rulesRule("A", "x > 1")))
	addAloneText(m, "A", texts.of(rulesRule("A", "(x) >  1.0")))
	if m["A"].ambiguous {
		t.Error("the same rule twice is ambiguous")
	}
	addAloneText(m, "A", texts.of(rulesRule("A", "x > 2")))
	if !m["A"].ambiguous {
		t.Error("two rules of a name written otherwise aren't ambiguous")
	}
}

// A rule left alone is the same rule in serve's record when it is as
// parsed (rules.Rule.Parsed): as its file writes it, and as Prometheus's
// rules API prints it again. One whose expression isn't read so is the
// same only by its text (rules.Rule.Whole): the same text is one, and
// another, or the same rule read so on one side alone, is another.
func TestAloneTextParsed(t *testing.T) {
	file := rules.Rule{Name: "Errors", Group: "shop", Expr: "sum(errors) by (service) > 5", For: 2 * time.Minute, Interval: time.Minute,
		Labels: map[string]string{"severity": "page"}}
	api := file
	api.Expr, api.Source = "sum by (service) (errors) > 5", "api"
	texts := aloneTexts{}
	if file.Whole() == api.Whole() || aloneTextOf(file) != aloneTextOf(api) || texts.of(file) != texts.of(api) {
		t.Errorf("a rule and its API's printing aren't one")
	}
	tighter := api
	tighter.Expr = "sum by (service) (errors) >= 5"
	if aloneTextOf(file) == aloneTextOf(tighter) {
		t.Errorf("> and >= are one")
	}
	// Not read as PromQL: MetricsQL's limit after by.
	metricsQL := file
	metricsQL.Expr = "sum(errors) by (service) limit 3 > 5"
	if _, ok := metricsQL.Parsed(); ok {
		t.Fatal("read as PromQL")
	}
	again := metricsQL
	again.Source = "api"
	spaced := metricsQL
	spaced.Expr = "sum(errors) by (service)  limit 3 > 5"
	readable := metricsQL
	readable.Expr = "sum(errors) by (service) > 5"
	switch {
	case aloneTextOf(metricsQL) != aloneTextOf(again):
		t.Error("the same text isn't one")
	case aloneTextOf(metricsQL) == aloneTextOf(spaced):
		t.Error("a text read otherwise, not parsed, is one")
	case aloneTextOf(metricsQL) == aloneTextOf(readable):
		t.Error("a text not parsed and a parsed one are one")
	}
	// A Loki ruler's rule is parsed as LogQL: as its API prints it again,
	// it is the same; a rule of another matcher, not.
	loki := rules.Rule{Name: "L", Group: "g", Expr: `sum(count_over_time({app="a"}[1m])) > 1`, Logs: rules.LokiRuler}
	lokiAPI := loki
	lokiAPI.Expr = `(sum(count_over_time({app="a"}[1m])) > 1)`
	lokiOther := loki
	lokiOther.Expr = `sum(count_over_time({app="b"}[1m])) > 1`
	if aloneTextOf(loki) != aloneTextOf(lokiAPI) || aloneTextOf(loki) == aloneTextOf(lokiOther) {
		t.Error("a Loki ruler's rule isn't parsed as LogQL")
	}
}

func rulesRule(name, expr string) rules.Rule { return rules.Rule{Name: name, Group: "g", Expr: expr} }

// reprinting is the tests' history behind a Prometheus whose rules API
// (--loaded-rules) answers with answer: each rule's expression printed
// again, as Prometheus 3.5.0's promql/parser prints it. Asked an
// expression as printed, it answers as for the one as written (as).
type reprinting struct {
	*history
	answer string
	as     map[string]string
	// raw is how often each expression was asked, as it was given.
	raw map[string]int
}

// askedRaw is how often expr was asked, as it was given.
func (h *reprinting) askedRaw(expr string) int {
	h.Lock()
	defer h.Unlock()
	return h.raw[expr]
}

func (h *reprinting) Rules(context.Context) ([]byte, error) { return []byte(h.answer), nil }

func (h *reprinting) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	h.Lock()
	if h.raw == nil {
		h.raw = map[string]int{}
	}
	h.raw[expr]++
	h.Unlock()
	if written, ok := h.as[expr]; ok {
		expr = written
	}
	return h.history.QueryRange(ctx, expr, start, end, step)
}

// reprinted is a Prometheus's rules API answering with Errors and
// MoreErrors, the shop group's interval 60 seconds, MoreErrors's
// expression printed again: sum(errors) by (service) > 1 as sum by
// (service) (errors) > 1.
func reprinted() *reprinting {
	return &reprinting{history: &history{}, as: map[string]string{"sum by (service) (errors) > 1": "errors > 1", "sum(errors) by (service) > 1": "errors > 1"},
		answer: `{"status":"success","data":{"groups":[{"name":"shop","file":"/etc/prometheus/rules/shop.yaml","interval":60,"limit":0,
		  "evaluationTime":0.001,"lastEvaluation":"0001-01-01T00:00:00Z","rules":[
		  {"state":"inactive","name":"Errors","query":"errors > 5","duration":120,"keepFiringFor":0,"labels":{"severity":"page"},
		   "annotations":{},"alerts":[],"health":"ok","type":"alerting"},
		  {"state":"inactive","name":"MoreErrors","query":"sum by (service) (errors) > 1","duration":300,"keepFiringFor":0,"labels":{},
		   "annotations":{},"alerts":[],"health":"ok","type":"alerting"}]}]}}`}
}

// configured is a reprinting Prometheus that serves its configuration
// (/api/v1/status/config) as config, or fails with err.
type configured struct {
	*reprinting
	config string
	err    error
}

func (h *configured) StatusConfig(context.Context) (string, error) { return h.config, h.err }

// prometheus350Config is the configuration a real Prometheus 3.5.0 served,
// which writes every default: its evaluation_interval 1m.
func prometheus350Config(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../content/testdata/prometheus-3.5.0/status-config.json")
	if err != nil {
		t.Fatal(err)
	}
	var a struct {
		Data struct{ YAML string } `json:"data"`
	}
	if err := json.Unmarshal(b, &a); err != nil || !strings.Contains(a.Data.YAML, "evaluation_interval: 1m") {
		t.Fatalf("%v: %.200s", err, a.Data.YAML)
	}
	return a.Data.YAML
}

// rulesEvery is a rule file of the shop group, a minute apart, holding
// each rule given: as a Prometheus whose rules API says interval 60 runs
// it.
func rulesEvery(rs ...string) string {
	return "groups:\n  - name: shop\n    interval: 1m\n    rules:\n" + strings.Join(rs, "")
}

// Serve reads its rules from Prometheus's rules API (--loaded-rules),
// which prints each expression again: MoreErrors, written sum(errors) by
// (service) > 1, is sum by (service) (errors) > 1 there, its for 300
// seconds. A pull request that leaves it as its file has it and makes
// Errors miss INC-1 keeps INC-1 caught by MoreErrors' catch in serve's
// record: the same rule, as parsed, where its text is another, which left
// INC-1 not known before; nothing of it is asked of the history. Left alone
// as >= in the pull request, it is another rule than serve's record is of:
// it is replayed as the pull request has it, and catching nothing, INC-1 is
// missed.
func TestBacktestFailMissedReprinted(t *testing.T) {
	const moreWritten = "      - alert: MoreErrors\n        expr: sum(errors) by (service) > 1 # any error\n        for: 5m\n"
	h := reprinted()
	b := btServer(t, h, "", func(c *Config) { c.RulesPath, c.Loaded = "", true })
	ctx := context.Background()
	incs, err := b.s.l.Incidents(ctx, now.Add(-3*24*time.Hour), now, false)
	if err != nil || len(incs) != 1 || !strings.Contains(fmt.Sprint(incs[0].CaughtBy), "MoreErrors") {
		t.Fatalf("the record: %+v %v", incs, err)
	}
	// The rule as the file has it and as serve read it: two texts.
	file, _ := rules.Parse([]byte(rulesEvery(moreWritten)))
	latest, _, _ := b.s.l.Latest(ctx)
	served, _ := b.s.parse(ctx, latest)
	var api *rules.Rule
	for i := range served {
		if served[i].Name == "MoreErrors" {
			api = &served[i]
		}
	}
	if len(file) != 1 || api == nil || file[0].Whole() == api.Whole() {
		t.Fatalf("the file's %+v, serve's %+v", file, api)
	}
	held := map[string]any{"days": 3, "fail_missed": true}
	md, rep := b.done(t, b.plain, btBody(rulesEvery(errorsRule, moreWritten), rulesEvery(errorsLate, moreWritten), held))
	sum, missed := missedOf(t, rep)
	if sum["missed_checked"] != true || sum["incidents_missed"] != float64(0) || sum["missed_not_known"] != float64(0) || len(missed) != 0 {
		t.Errorf("summary %v, missed %v\n%s", sum, missed, md)
	}
	if !strings.Contains(md, "Every incident a rule caught before the change is caught after it too (`--fail-missed`).") {
		t.Errorf("the comment:\n%s", md)
	}
	if n := h.askedRaw("sum(errors) by (service) > 1"); n != 0 {
		t.Errorf("the record settled it, and the history was asked %d times for it", n)
	}
	// Left alone as another rule: serve's record isn't of it, so it is
	// replayed as the pull request has it, which catches nothing.
	other := strings.Replace(moreWritten, "> 1", ">= 1", 1)
	_, rep = b.done(t, b.plain, btBody(rulesEvery(errorsRule, other), rulesEvery(errorsLate, other), held))
	if sum, missed := missedOf(t, rep); sum["incidents_missed"] != float64(1) || sum["missed_not_known"] != float64(0) || len(missed) != 1 {
		t.Errorf("held as >=: summary %v, missed %v", sum, missed)
	}
	if n := h.askedRaw("sum(errors) by (service) >= 1"); n == 0 {
		t.Error("the rule the record isn't of wasn't replayed")
	}
}

// A rule file's group with no interval of its own runs at Prometheus's
// evaluation_interval, which its rules API says of the group (60) and the
// file doesn't: a pull request leaving MoreErrors alone in such a file is
// taken at it where serve read the configuration (once a day, with the
// scrape intervals): as a real Prometheus 3.5.0 writes it, 1m. INC-1 stays
// caught. Where the configuration can't be read, says another interval,
// or says none, as no Prometheus's does, MoreErrors in the file is another
// rule than serve's record is of: it is replayed as the file has it, which
// catches INC-1, so INC-1 is caught, not known no longer. So too
// where vmalert answers (its groups say their concurrency), whose groups
// run at its own -evaluationInterval, whatever a configuration says. Only
// where the record is of it is the history not asked.
func TestBacktestFailMissedEvaluationInterval(t *testing.T) {
	const moreWritten = "      - alert: MoreErrors\n        expr: sum(errors) by (service) > 1\n        for: 5m\n"
	held := map[string]any{"days": 3, "fail_missed": true}
	for _, c := range []struct {
		name           string
		config         string
		err            error
		vmalert, known bool
		scrape         time.Duration
	}{
		{"Prometheus 3.5.0's", prometheus350Config(t), nil, false, true, 0},
		{"none said", "global:\n  scrape_interval: 15s\n", nil, false, false, 0},
		{"not served", "", errors.New("HTTP 404"), false, false, 0},
		{"another", "global:\n  evaluation_interval: 30s\n", nil, false, false, 0},
		{"vmalert's", "global:\n  evaluation_interval: 1m\n", nil, true, false, 0},
		// --scrape-interval given, the configuration is read for this alone.
		{"--scrape-interval too", prometheus350Config(t), nil, false, true, 30 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := &configured{reprinting: reprinted(), config: c.config, err: c.err}
			if c.vmalert {
				h.answer = strings.Replace(h.answer, `"interval":60,`, `"interval":60,"concurrency":1,`, 1)
			}
			b := btServer(t, h, "", func(cfg *Config) { cfg.RulesPath, cfg.Loaded, cfg.ScrapeInterval = "", true, c.scrape })
			if incs, err := b.s.l.Incidents(context.Background(), now.Add(-3*24*time.Hour), now, false); err != nil || len(incs) != 1 ||
				!strings.Contains(fmt.Sprint(incs[0].CaughtBy), "MoreErrors") {
				t.Fatalf("the record: %+v %v", incs, err)
			}
			md, rep := b.done(t, b.plain, btBody(rulesOf(errorsRule, moreWritten), rulesOf(errorsLate, moreWritten), held))
			sum, missed := missedOf(t, rep)
			if sum["missed_checked"] != true || sum["incidents_missed"] != float64(0) || sum["missed_not_known"] != float64(0) || len(missed) != 0 {
				t.Errorf("summary %v, missed %v\n%s", sum, missed, md)
			}
			// Replayed only where the record isn't of the rule.
			if asked := h.askedRaw("sum(errors) by (service) > 1") > 0; asked == c.known {
				t.Errorf("the record is of the rule: %t, and the history was asked for it: %t", c.known, asked)
			}
		})
	}
	// Without --loaded-rules, serve's rules files: no interval is taken
	// from the configuration, whatever it says.
	s := &Server{cfg: Config{}}
	s.saidRules.evaluation = time.Minute
	if s.evaluationInterval() != 0 {
		t.Error("an evaluation interval without --loaded-rules")
	}
}

// Serve reads Prometheus's configuration once a day, and its rules every
// look. Its evaluation_interval was 30s, as the shop group ran, when it
// was read; since, it is 1m, and the group runs at 60s in serve's latest
// version of the rules, the configuration not read again. A pull
// request's MoreErrors with no interval runs at 1m after the merge: not
// the 30s rule serve's record over INC-1 is of. It isn't taken at the 30s
// still kept: it is replayed as the pull request has it, and catches INC-1.
func TestBacktestFailMissedEvaluationIntervalSince(t *testing.T) {
	const moreWritten = "      - alert: MoreErrors\n        expr: sum(errors) by (service) > 1\n        for: 5m\n"
	h := &configured{reprinting: reprinted(), config: "global:\n  evaluation_interval: 30s\n"}
	h.answer = strings.Replace(h.answer, `"interval":60,`, `"interval":30,`, 1)
	b := btServer(t, h, "", func(c *Config) { c.RulesPath, c.Loaded = "", true })
	ctx := context.Background()
	if incs, err := b.s.l.Incidents(ctx, now.Add(-3*24*time.Hour), now, false); err != nil || len(incs) != 1 ||
		!strings.Contains(fmt.Sprint(incs[0].CaughtBy), "MoreErrors") {
		t.Fatalf("the record: %+v %v", incs, err)
	}
	h.answer, h.config = reprinted().answer, "global:\n  evaluation_interval: 1m\n"
	raw := ""
	if err := b.s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	if vs, _ := b.s.l.Versions(ctx, now.Add(-3*24*time.Hour), now, false); len(vs) != 2 || b.s.evaluationInterval() != 30*time.Second {
		t.Fatalf("%d versions, the evaluation interval %s", len(vs), b.s.evaluationInterval())
	}
	held := map[string]any{"days": 3, "fail_missed": true}
	md, rep := b.done(t, b.plain, btBody(rulesOf(errorsRule, moreWritten), rulesOf(errorsLate, moreWritten), held))
	if sum, missed := missedOf(t, rep); sum["incidents_missed"] != float64(0) || sum["missed_not_known"] != float64(0) || len(missed) != 0 {
		t.Errorf("summary %v, missed %v\n%s", sum, missed, md)
	}
	if h.askedRaw("sum(errors) by (service) > 1") == 0 {
		t.Error("the rule the record isn't of wasn't replayed")
	}
}

const (
	quietRule  = "      - alert: Quiet\n        expr: quiet > 1\n        for: 5m\n"
	brokenRule = "      - alert: Broken\n        expr: nope(up)\n        for: 5m\n"
	diskRule   = "      - alert: Disk\n        expr: disk > 1\n        for: 5m\n"
)

// btServingOn is btServing asking h of the history.
func btServingOn(t *testing.T, h replay.History, file string) *btSet {
	t.Helper()
	return btServer(t, h, "", func(c *Config) {
		dir := filepath.Join(t.TempDir(), "rules")
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(file), 0o644)
		c.RulesPath = dir
	})
}

// askedOf is how often h was asked for expr.
func (h *history) askedOf(expr string) int {
	h.Lock()
	defer h.Unlock()
	return h.exprs[expr]
}

// A rule the pull request leaves alone that serve doesn't run has no record:
// it is replayed, over the incident's catch window alone. Catching INC-1,
// INC-1 is caught, not known no longer; catching nothing, it is missed and
// the run fails. A rule of it that can't be replayed leaves INC-1 not
// known, as before.
func TestBacktestFailMissedReplaysLeftAlone(t *testing.T) {
	parallel(t)
	held := map[string]any{"days": 3, "fail_missed": true}
	for _, c := range []struct {
		name          string
		alone         string
		missed, known int
		asked         string
	}{
		{"catches it", elsewhereRule, 0, 0, "errors > 1"},
		{"catches nothing", quietRule, 1, 0, "quiet > 1"},
		{"can't be replayed", brokenRule, 0, 1, "nope(up)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := &history{}
			b := btServingOn(t, h, rulesOf(errorsRule))
			was := h.askedOf(c.asked)
			md, rep := b.done(t, b.plain, btBody(rulesOf(errorsRule, c.alone), rulesOf(errorsLate, c.alone), held))
			sum, missed := missedOf(t, rep)
			if sum["missed_checked"] != true || sum["incidents_missed"] != float64(c.missed) || sum["missed_not_known"] != float64(c.known) || len(missed) != c.missed {
				t.Fatalf("summary %v, missed %v\n%s", sum, missed, md)
			}
			if sum["missed_limited"] != float64(0) {
				t.Errorf("a limit said, none reached: %v", sum)
			}
			if h.askedOf(c.asked) == was {
				t.Errorf("%s wasn't replayed", c.asked)
			}
			switch {
			case c.missed > 0:
				if !strings.Contains(md, "**This run fails** (`--fail-missed`): 1 incidents") {
					t.Errorf("the run doesn't fail:\n%s", md)
				}
			case c.known > 0:
				if strings.Contains(md, "This run fails") || !strings.Contains(md, "1 incidents a rule caught before the change are caught by no rule after it that lapilli serve can tell") {
					t.Errorf("the comment:\n%s", md)
				}
			}
		})
	}
}

// What serve's record settles isn't replayed: a rule left alone it holds
// as the pull request has it stays out of the replay of the rules it
// doesn't, and with nothing to replay the lane asks nothing of the rules
// left alone, so that the same change not held to the incidents asks as
// much.
func TestBacktestFailMissedReplaysOnlyWhatTheRecordCant(t *testing.T) {
	parallel(t)
	held := map[string]any{"days": 3, "fail_missed": true}
	// Serve runs Errors and Quiet; Elsewhere isn't run, so the record can't
	// settle INC-1, and it is replayed: Quiet, whose record is held, isn't.
	h := &history{}
	b := btServingOn(t, h, rulesOf(errorsRule, quietRule))
	more, quiet := h.askedOf("errors > 1"), h.askedOf("quiet > 1")
	_, rep := b.done(t, b.plain, btBody(rulesOf(errorsRule, quietRule, elsewhereRule), rulesOf(errorsLate, quietRule, elsewhereRule), held))
	if sum, _ := missedOf(t, rep); sum["incidents_missed"] != float64(0) || sum["missed_not_known"] != float64(0) {
		t.Errorf("summary %v", sum)
	}
	if h.askedOf("errors > 1") == more || h.askedOf("quiet > 1") != quiet {
		t.Errorf("Elsewhere asked %d more times, Quiet, held by the record, %d", h.askedOf("errors > 1")-more, h.askedOf("quiet > 1")-quiet)
	}
	// The record settles everything (Errors lost, MoreErrors held by it
	// caught): nothing of the rules left alone is asked, and the lane asks as
	// much as without the hold.
	h = &history{}
	b = btServingOn(t, h, rulesOf(errorsRule, moreErrorsRule))
	more = h.askedOf("errors > 1")
	total := func() int { h.Lock(); defer h.Unlock(); return h.asked }
	asked := func(more map[string]any) int {
		before := total()
		b.done(t, b.plain, btBody(rulesOf(errorsRule, moreErrorsRule), rulesOf(errorsLate, moreErrorsRule), more))
		return total() - before
	}
	plain := asked(map[string]any{"days": 3})
	if got := asked(held); got != plain || got == 0 {
		t.Errorf("held to the incidents the lane asked %d times, %d without", got, plain)
	}
	if n := h.askedOf("errors > 1") - more; n != 0 {
		t.Errorf("a rule the record settles was replayed %d times", n)
	}
}

// The replay is bounded by what the lane may ask. Planned past it, the
// incidents it doesn't reach stay not known, said so in the summary
// (missed_limited) and the comment.
func TestBacktestFailMissedReplayLimited(t *testing.T) {
	parallel(t)
	held := map[string]any{"days": 3, "fail_missed": true}
	h := &history{}
	b := btServingOn(t, h, rulesOf(errorsRule))
	b.s.aloneRequests = 1 // Elsewhere and Disk are two requests
	more, disk := h.askedOf("errors > 1"), h.askedOf("disk > 1")
	md, rep := b.done(t, b.plain, btBody(rulesOf(errorsRule, elsewhereRule, diskRule), rulesOf(errorsLate, elsewhereRule, diskRule), held))
	sum, missed := missedOf(t, rep)
	if sum["incidents_missed"] != float64(0) || sum["missed_not_known"] != float64(1) || sum["missed_limited"] != float64(1) || len(missed) != 0 {
		t.Fatalf("summary %v, missed %v\n%s", sum, missed, md)
	}
	if !strings.Contains(md, "1 of them were left so because the replay of the rules left alone reached the limit lapilli serve puts on it") || strings.Contains(md, "This run fails") {
		t.Errorf("the comment:\n%s", md)
	}
	if h.askedOf("errors > 1") != more || h.askedOf("disk > 1") != disk {
		t.Error("the replay asked past its limit")
	}
	// Within it, the same change is settled.
	b.s.aloneRequests = 10
	_, rep = b.done(t, b.plain, btBody(rulesOf(errorsRule, elsewhereRule, diskRule), rulesOf(errorsLate, elsewhereRule, diskRule), held))
	sum, _ = missedOf(t, rep)
	if sum["missed_not_known"] != float64(0) || sum["incidents_missed"] != float64(0) {
		t.Errorf("within the limit: %v", sum)
	}
	if sum["missed_limited"] != float64(0) {
		t.Errorf("a limit said, none reached: %v", sum)
	}
}

// startSeen is the tests' history that keeps the earliest start of the ranges
// each expression was asked over.
type startSeen struct {
	*history
	mu    sync.Mutex
	first map[string]time.Time
	on    atomic.Bool // set once serve's own runs are over
}

func (h *startSeen) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if !h.on.Load() {
		return h.history.QueryRange(ctx, expr, start, end, step)
	}
	h.mu.Lock()
	if h.first == nil {
		h.first = map[string]time.Time{}
	}
	if at, ok := h.first[expr]; !ok || start.Before(at) {
		h.first[expr] = start
	}
	h.mu.Unlock()
	return h.history.QueryRange(ctx, expr, start, end, step)
}

func (h *startSeen) firstOf(expr string) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.first[expr]
}

// A group's query_offset, which Prometheus's rules API doesn't say: a rule
// of the pull request's with one isn't the rule serve's record is of, so it
// isn't held by it, and it is replayed as the pull request has it, its
// queries read back by the offset. Without one it is the rule the record is
// of, and nothing of it is asked.
func TestBacktestFailMissedReplaysWithQueryOffset(t *testing.T) {
	parallel(t)
	held := map[string]any{"days": 3, "fail_missed": true}
	// The rule left alone in a group with query_offset, or in one with an
	// interval of its own, which is another rule than serve's too and has no
	// offset.
	group := func(attr string, rs ...string) string {
		return "groups:\n  - name: shop\n    " + attr + "\n    rules:\n" + strings.Join(rs, "")
	}
	first := map[string]time.Time{}
	for name, attr := range map[string]string{"offset": "query_offset: 5m", "interval": "interval: 1m"} {
		h := &startSeen{history: &history{}}
		b := btServingOn(t, h, rulesOf(errorsRule, moreErrorsRule))
		h.on.Store(true)
		_, rep := b.done(t, b.plain, btBody(group(attr, errorsRule, moreErrorsRule), group(attr, errorsLate, moreErrorsRule), held))
		if sum, missed := missedOf(t, rep); sum["incidents_missed"] != float64(0) || sum["missed_not_known"] != float64(0) || len(missed) != 0 {
			t.Errorf("%s: summary %v", name, sum)
		}
		first[name] = h.firstOf("errors > 1")
		if first[name].IsZero() {
			t.Errorf("%s: the rule serve's record isn't of wasn't replayed", name)
		}
	}
	if d := first["interval"].Sub(first["offset"]); d < 5*time.Minute {
		t.Errorf("replayed with the group's query_offset, its reads earlier by %s than without it", d)
	}
	// Alike to serve's own, with none: held by the record, nothing asked.
	h := &history{}
	b := btServingOn(t, h, rulesOf(errorsRule, moreErrorsRule))
	more := h.askedOf("errors > 1")
	b.done(t, b.plain, btBody(rulesOf(errorsRule, moreErrorsRule), rulesOf(errorsLate, moreErrorsRule), held))
	if n := h.askedOf("errors > 1") - more; n != 0 {
		t.Errorf("the record is of it, and it was asked %d times", n)
	}
}

// A replay merges the windows of one rule that overlap or meet, so that
// nothing is asked twice, and keeps apart those that don't.
func TestMergeAloneWindows(t *testing.T) {
	parallel(t)
	at := func(m int) time.Time { return time.Date(2026, 11, 2, 6, m, 0, 0, time.UTC) }
	got := mergeWindows([]aloneWindow{
		{from: at(40), to: at(50), cases: []int{2}}, {from: at(0), to: at(20), cases: []int{0}},
		{from: at(10), to: at(30), cases: []int{1}}, {from: at(30), to: at(35), cases: []int{3}},
	})
	if len(got) != 2 || !got[0].from.Equal(at(0)) || !got[0].to.Equal(at(35)) || !got[1].from.Equal(at(40)) || !got[1].to.Equal(at(50)) {
		t.Fatalf("%+v", got)
	}
	if fmt.Sprint(got[0].cases) != "[0 1 3]" || fmt.Sprint(got[1].cases) != "[2]" {
		t.Errorf("the cases: %v %v", got[0].cases, got[1].cases)
	}
}

// Of an incident the record leaves not known, a replay may settle it
// unless a catch in the record is by a rule it can't stand for.
func TestLeftAloneJudgeNeed(t *testing.T) {
	parallel(t)
	t0 := time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)
	in := incident.Incident{ID: "INC-1", Start: t0, End: t0.Add(30 * time.Minute)}
	o := verdict.Options{Lead: 15 * time.Minute, UnknownLen: time.Hour}
	now := aloneText{sum: [32]byte{1}, reach: 6 * time.Minute}
	then := aloneText{sum: [32]byte{2}, reach: 6 * time.Minute}
	far := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name   string
		caught []string
		hard   bool
		names  string
	}{
		{"a rule serve runs, written otherwise then", []string{"Latency"}, false, "[Latency]"},
		{"told apart from the changed rule", []string{"Errors [shop]"}, true, "[]"},
		{"copied in", []string{"Copied"}, true, "[]"},
	} {
		a := &leftAlone{o: o, by: map[caughtAt][]verdict.Catch{}, changed: map[string]bool{"Errors": true}, same: map[string]bool{},
			bases: map[string]bool{"Errors": true}, copied: map[string]bool{"Copied": true}, want: map[string]aloneText{"Latency": now}, read: true,
			versions: []aloneVersion{{until: far, texts: map[string]aloneText{"Latency": then}, sameAll: true}}}
		for _, r := range c.caught {
			a.by[caughtAt{in.ID, in.Start.Unix()}] = append(a.by[caughtAt{in.ID, in.Start.Unix()}], verdict.Catch{Rule: r})
		}
		caught, known, need := a.judge(in)
		var names []string
		for n := range need.names {
			names = append(names, n)
		}
		if caught || known || need.hard != c.hard || fmt.Sprint(names) != c.names && !(len(names) == 0 && c.names == "[]") {
			t.Errorf("%s: caught %t, known %t, need %+v", c.name, caught, known, need)
		}
	}
}

// The replay of the rules left alone plans within what is left of
// --backtest-timeout: nothing once none is, and by the lane's rate over
// what is.
func TestAloneBudgetLeft(t *testing.T) {
	s := &Server{btPort: &backtestPort{rate: 2}}
	for _, c := range []struct {
		left time.Duration
		want int
	}{{0, 0}, {-time.Second, 0}, {10 * time.Second, 20}, {time.Second / 4, 1}} {
		if got := s.aloneBudget(c.left); got != c.want {
			t.Errorf("left %s: %d, want %d", c.left, got, c.want)
		}
	}
	s.btPort.rate = 0
	if got := s.aloneBudget(time.Minute); got != leftAloneRequests {
		t.Errorf("no rate: %d, want %d", got, leftAloneRequests)
	}
	s.aloneRequests = 3
	if got := s.aloneBudget(0); got != 0 {
		t.Errorf("none left, a cap set: %d", got)
	}
}
