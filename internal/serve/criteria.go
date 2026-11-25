package serve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"lapilli.dev/internal/content"
	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
	"lapilli.dev/internal/verdict"
)

// The incident criteria (docs/design/ui.md, "Incident criteria"): besides
// the incidents written down and copied in, the windows where the impact
// signal (--impact, a PromQL expression that returns something while users
// are hurt) returned something for at least --impact-for. Set where serve
// is started, or by an admin on the Settings page, as the other judging
// settings are, and tried on the last four weeks first.
//
// The record the signal keeps follows one rule: it holds only for weeks
// that were read, with data, by the criterion in effect, and, after a
// change, replaced by a successful read of that criterion: from when the
// runs read it, all along, and not before where the data of every
// selector it reads begins (impactData). Saving another reads it again
// over the last four weeks (impactAgain), which puts its windows in place
// of the one before's and keeps the record by it from there; until then,
// the ledger's windows stay, and once a run reads the new one, no record
// is kept by the signal (ledger.ImpactKept.Floor).

// criterion is what makes an impact window: the expression, "" for none,
// and how long a series must return something for.
type criterion struct {
	Expr string
	For  time.Duration
}

// key tells criteria apart, as the ledger keeps the one the runs read
// (ledger.ImpactKept): "" for none.
func (c criterion) key() string {
	if c.Expr == "" {
		return ""
	}
	return verdict.Duration(c.For) + " " + c.Expr
}

// exprOfKey is the expression of a criterion's key.
func exprOfKey(key string) string {
	_, expr, _ := strings.Cut(key, " ")
	return expr
}

// impactOff is the impact setting that turns off an expression the command
// line gives: empty goes back to the command line's, as the other settings
// do. An expression that is the word off is taken for it.
const impactOff = "off"

// The settings the incident criteria are, by name, with the flag each
// stands for.
var criteriaSettable = []struct{ key, flag string }{{"impact", "--impact"}, {"impact-for", "--impact-for"}}

// The bounds of what the UI takes: an expression of 4,096 characters at
// most, as its field counts them, with no range over a day, no subquery
// stepping by less than a minute, and 32 selectors at most; a for from the
// step to a day.
const (
	maxImpactExpr  = 4096
	maxImpactRange = 24 * time.Hour
	maxImpactFor   = 24 * time.Hour
	// minImpactStep is the finest step a subquery may take: a day of a
	// second's steps is 86,400 points a series.
	minImpactStep = time.Minute
	// maxImpactSelectors is how many selectors an expression may read:
	// where each one's data begins is a request of its own.
	maxImpactSelectors = 32
)

// impactAgainWeeks is how far back saving a new criterion reads it again,
// in whole weeks before the cursor's, and a try reads it: what the UI
// calls the last four weeks.
const impactAgainWeeks = 4

// impactTriesAnHour are the reads of a criterion an admin may ask for in an
// hour, a try or a save that reads one: each reads Prometheus over five
// weeks. Kept in the ledger, for every replica, when replicas share it;
// else in this process, and begun again when it starts.
const impactTriesAnHour = 12

// tryWithin is how long a try's reads, and a save's, take at most: within
// the time the page has to answer, whatever --timeout is.
const tryWithin = 90 * time.Second

// triedFor is how long a criterion read in full by a try stands for saving
// it, which then reads it no more.
const triedFor = 30 * time.Minute

// criterionOf is the criterion in effect, of the settings the UI changed
// and the command line.
func (s *Server) criterionOf(set map[string]ledger.Setting) criterion {
	c := criterion{s.cfg.Impact, s.cfg.ImpactFor}
	if v, ok := set["impact"]; ok {
		c.Expr = v.Value
		if v.Value == impactOff {
			c.Expr = ""
		}
	}
	if v, ok := set["impact-for"]; ok {
		if d, err := ParseDuration(v.Value); err == nil && d > 0 {
			c.For = d
		}
	}
	if s.h == nil || s.signalsIn(set).named() {
		// Read from Prometheus, which there is none of; or, with named
		// signals, none has no name: one the UI kept stands aside.
		c.Expr = ""
	}
	return c
}

// criterion is the criterion in effect: the command line's, when the
// settings can't be read.
func (s *Server) criterion(ctx context.Context) (criterion, error) {
	set, err := s.l.Settings(ctx)
	if err != nil {
		return s.criterionOf(nil), err
	}
	return s.criterionOf(set), nil
}

// assumedKey is the criterion a ledger that kept none (a lapilli before
// 0.49's) is taken to have been read with: the command line's.
func (s *Server) assumedKey() string { return s.criterionOf(nil).key() }

// impactFrom is from when the record is kept by the impact signal, for
// verdict.Options.ImpactFrom: what the ledger holds, from when the runs
// read it, all along, with the criterion in effect then, and where the
// data of the selectors it reads begins (ledger.ImpactKept.From). A
// criterion saved and not yet read again keeps the record of the one
// before until a run reads it, and then none until it is read again.
// Turned off, none.
func (s *Server) impactFrom(ctx context.Context, c criterion) (time.Time, error) {
	if c.Expr == "" {
		return time.Time{}, nil
	}
	k, err := s.l.ImpactKept(ctx)
	return k.From(), err
}

// maxQueryOf is the longest query a page takes: a try of the incident
// criteria carries an expression of maxImpactExpr characters, escaped; a
// try of the rule entries, maxEntriesText bytes of them.
func maxQueryOf(page string, r *http.Request) int {
	switch q := r.URL.RawQuery; {
	case page != "settings":
	case strings.Contains(q, "impact-try=") || strings.Contains(q, "signal-try=") || strings.Contains(q, "depends-try="):
		return maxQuery + 8*maxImpactExpr
	case strings.Contains(q, "rules-try="):
		return maxQuery + 3*maxEntriesText
	}
	return maxQuery
}

// impactHeld is the criterion whose windows the ledger holds, as a
// backtest is judged against them (evidence.Impact): the one kept, "" with
// the signal off. While a floor stands, the windows are the one kept's
// before it and those the runs read since after it: a label of its own,
// which no criterion kept is (heldPending), so that a backtest judged
// against them is held once the criterion in effect is read again, the
// one kept among them.
func (s *Server) impactHeld(ctx context.Context) (string, error) {
	n, set, err := s.signalsNowOf(ctx)
	if err != nil {
		return "", err
	}
	if n.named() {
		return s.signalsHeld(ctx, n)
	}
	c := s.criterionOf(set)
	if c.Expr == "" {
		return "", nil
	}
	k, err := s.l.ImpactKept(ctx)
	switch {
	case !k.Floor.IsZero():
		return heldPending(k), err
	case k.Criterion == "" && !k.Since.IsZero():
		return s.assumedKey(), err // a ledger an earlier lapilli kept
	}
	return k.Criterion, err
}

// heldPending is the label of the windows a ledger holds while a floor
// stands: when it began, the criterion kept and the one the runs last
// read. A criterion's key begins with its --impact-for, so none is it.
func heldPending(k ledger.ImpactKept) string {
	return fmt.Sprintf("pending %d %q %q", k.Floor.Unix(), k.Criterion, k.Read)
}

// checkImpact is what the UI refuses of an impact expression before
// anything is read for it: too long, characters that don't show, what
// doesn't read as PromQL as Lapilli reads it (rules.Scope: strings,
// selectors, ranges and brackets closed), or a range or a subquery over a
// day, which would read days of samples at each step. Prometheus's own
// parser has the last word, when it is tried or saved.
func checkImpact(expr string) error {
	switch {
	case utf8.RuneCountInString(expr) > maxImpactExpr:
		return bad("an impact expression is %d characters at most", maxImpactExpr)
	case !utf8.ValidString(expr) || strings.IndexFunc(expr, func(r rune) bool {
		return r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r))
	}) >= 0:
		return bad("an impact expression has characters that don't show")
	}
	if _, err := rules.Scope(expr, nil); err != nil {
		return bad("the impact expression doesn't read as PromQL: %v", err)
	}
	ranges, _ := rules.Ranges(expr)
	for _, r := range ranges {
		window, step, sub := strings.Cut(r, ":")
		d, ok := content.ParseDuration(strings.TrimSpace(window))
		switch {
		case !ok:
			return bad("the impact expression reads a range of %s: Lapilli reads a range as a duration like 5m or 1d", r)
		case d > maxImpactRange:
			return bad("the impact expression reads a range of %s: a range or a subquery is %s at most", r, verdict.Duration(maxImpactRange))
		}
		if step = strings.TrimSpace(step); sub && step != "" {
			if d, ok := content.ParseDuration(step); !ok || d < minImpactStep {
				return bad("the impact expression's subquery %s steps by less than %s, or not by a duration", r, verdict.Duration(minImpactStep))
			}
		}
	}
	if sels, _ := rules.Selectors(expr); len(sels) > maxImpactSelectors {
		return bad("the impact expression reads %d selectors: %d at most, as where each one's data begins is looked for apart", len(sels), maxImpactSelectors)
	}
	return nil
}

// checkImpactFor is what the UI refuses of --impact-for: not a duration,
// or outside from the step to a day.
func (s *Server) checkImpactFor(v string) (time.Duration, error) {
	d, err := ParseDuration(v)
	if err != nil {
		return 0, bad("--impact-for %s: want a duration, like 5m", v)
	}
	if d < s.cfg.Step || d > maxImpactFor {
		return 0, bad("--impact-for %s: want from %s to %s", v, verdict.Duration(s.cfg.Step), verdict.Duration(maxImpactFor))
	}
	return d, nil
}

// impactText is an expression as a form gives it: its line ends \n, its
// ends trimmed.
func impactText(v string) string {
	return strings.TrimSpace(strings.ReplaceAll(v, "\r\n", "\n"))
}

// errNoHistory is an impact signal set with nothing to read it from.
var errNoHistory = bad("the impact signal is read from Prometheus, and lapilli serve was started without --prometheus or --gcp-project")

// errImpactTries is a try, or a save that reads, past impactTriesAnHour.
var errImpactTries = bad("you have tried or saved as many incident criteria in the last hour as anyone may: each reads Prometheus; try again later")

// errNotFromHere is a try of the incident criteria not asked for from the
// Settings page, in a browser, on this site (Sec-Fetch-Site same-origin, or
// none for one typed or bookmarked): it reads Prometheus.
var errNotFromHere = bad("a try of the incident criteria reads Prometheus, so it is asked for from the Settings page, in a browser, on this site")

// fromHere reports whether a browser says a request came from this site or
// from no page at all (typed, a bookmark), as Sec-Fetch-Site does: not
// another site, and not a client that says nothing.
func fromHere(r *http.Request) bool {
	v := r.Header.Get("Sec-Fetch-Site")
	return v == "same-origin" || v == "none"
}

// takeTry counts a read of a criterion for who (impactTriesAnHour),
// reporting whether they may: in the ledger the replicas share, or this
// process's.
func (s *Server) takeTry(ctx context.Context, who string) bool {
	return s.takeOf(ctx, &s.impactTries, "impact-tries/", impactTriesAnHour, who)
}

// takeOf counts one of who's tries in t, most of them an hour, reporting
// whether they may: in the ledger the replicas share, in a bucket named
// by prefix and who, or else in t, this process's.
func (s *Server) takeOf(ctx context.Context, t *tries, prefix string, most int, who string) bool {
	if !s.l.Shared() {
		return t.take(who, false, time.Now())
	}
	sum := sha256.Sum256([]byte(ledger.FoldName(who)))
	ok, err := s.l.Take(ctx, prefix+hex.EncodeToString(sum[:8]), float64(most)/3600, float64(most), 1)
	if err != nil {
		s.log.Warn("a shared limit on tries couldn't be read: refused", "limit", strings.TrimSuffix(prefix, "/"), "err", err)
	}
	return ok && err == nil
}

// triedCache is when each criterion was last read in full by a try, by its
// key: saving it within triedFor reads it no more.
type triedCache struct {
	sync.Mutex
	at map[string]time.Time
}

func (t *triedCache) put(key string, now time.Time) {
	t.Lock()
	defer t.Unlock()
	if t.at == nil {
		t.at = map[string]time.Time{}
	}
	for k, at := range t.at {
		if now.Sub(at) > triedFor {
			delete(t.at, k)
		}
	}
	t.at[key] = now
}

func (t *triedCache) recent(key string, now time.Time) bool {
	t.Lock()
	defer t.Unlock()
	at, ok := t.at[key]
	return ok && now.Sub(at) <= triedFor
}

// promText is a read's error as a page says it: Prometheus's own words
// when it answered, what went wrong otherwise.
func promText(err error) string {
	var e *prom.APIError
	if errors.As(err, &e) {
		return e.Error()
	}
	return err.Error()
}

// firstSamples are, of each selector expr reads, when it first had data in
// [lo, hi], as source.FirstSamples looks for it. Within ctx's time and
// --max-samples.
func (s *Server) firstSamples(ctx context.Context, expr string, lo, hi time.Time, atStart bool) ([]ledger.SelectorData, error) {
	return source.FirstSamples(ctx, s.h, expr, lo, hi, atStart)
}

func (s *Server) firstSample(ctx context.Context, sel string, lo, hi time.Time, atStart bool) (ledger.SelectorData, error) {
	return source.FirstSample(ctx, s.h, sel, lo, hi, atStart)
}

// CheckImpact is what an impact expression is refused for before anything
// is read for it, as the Settings page refuses it (checkImpact): for
// lapilli verdict's named signals, and --impact-file's.
func CheckImpact(expr string) error { return checkImpact(expr) }

// criteriaView is the incident criteria, as the Settings page shows them:
// each setting, the form's values (as typed when tried, else in effect),
// whether there is Prometheus to read the signal from, from when the
// record is kept by it, where the data it reads begins, how its read again
// over the last four weeks goes, and a try's result.
type criteriaView struct {
	Settings  []settingView `json:"settings"`
	Expr, For string        `json:"-"`
	NoHistory bool          `json:"no_history,omitempty"`
	Since     time.Time     `json:"kept_since,omitzero"`
	// Floor is from when the runs read another criterion than the one the
	// record is kept by, which keeps no record until the one in effect is
	// read again (ledger.ImpactKept.Floor).
	Floor time.Time `json:"floor,omitzero"`
	// Data is where the data of the selectors the criterion the ledger
	// holds begins, when it was looked for.
	Data *dataView `json:"data,omitempty"`
	// Again, when the criterion in effect is to be read again over the
	// last four weeks: when it last failed, why, and when it is tried again.
	Again *againView `json:"read_again,omitempty"`
	Tried *impactTry `json:"-"`
	// Depends is the service graph (dependsweb.go), under the criteria.
	Depends *dependsView `json:"-"`
	// Named are the named impact signals in effect (docs/design/signals.md):
	// those serve was started with and those an admin set here, by name;
	// Removed, those serve was started with that an admin took away here;
	// Aside, those set here past the most; Entries, the rule entries.
	Named   []namedView  `json:"named,omitempty"`
	Removed []namedView  `json:"removed,omitempty"`
	Aside   []string     `json:"aside,omitempty"`
	Entries *entriesView `json:"rule_entries,omitempty"`
	// Add is the form a signal is added with, as typed when tried; Renames
	// says the impact signal with no name in effect becomes a named one
	// when one is added (the first).
	Add          signalForm  `json:"-"`
	AddOpen      bool        `json:"-"`
	Renames      bool        `json:"-"`
	SignalsTried *signalsTry `json:"-"`
}

// namedView is a named impact signal as the Settings page lists it: its
// expression, its "for", the labels it is matched on (Follows, the match
// in effect), and whether it keeps the record, and from when; what serve
// was started with of its name (Started, nil for one added here), and,
// changed here (Changed), by whom and when; Off, one serve was started with
// taken away here. Since, Floor, Data and Again are what keeps the record
// by it, as the criterion with no name's are; Form, its form, as typed when
// tried (Open); Lead, its lead of its own, "" to follow the lead in effect
// (docs/design/judging.md, item 1).
type namedView struct {
	Name    string       `json:"name"`
	Expr    string       `json:"expr"`
	For     string       `json:"for"`
	Match   []string     `json:"match"`
	Follows bool         `json:"follows,omitempty"`
	Record  bool         `json:"record"`
	Lead    string       `json:"lead,omitempty"`
	Start   time.Time    `json:"start,omitzero"`
	Started *startedView `json:"-"`
	Off     bool         `json:"off,omitempty"`
	Changed bool         `json:"changed,omitempty"`
	By      string       `json:"changed_by,omitempty"`
	Updated time.Time    `json:"changed_at,omitzero"`
	Floor   time.Time    `json:"floor,omitzero"`
	Data    *dataView    `json:"-"`
	Again   *againView   `json:"read_again,omitempty"`
	Form    signalForm   `json:"-"`
	Open    bool         `json:"-"`
}

// startedView is a named signal as serve was started with it, as the
// Settings page says it.
type startedView struct {
	Expr, For       string
	Match           []string
	Follows, Record bool
}

// entriesView is the rule entries as the Settings page lists them: the
// entries in effect, whether they are the UI's (Changed), by whom and
// when, how many serve was started with (Started), and the form's text, as
// typed when tried (Open).
type entriesView struct {
	List    []entryView `json:"entries"`
	Changed bool        `json:"changed,omitempty"`
	By      string      `json:"changed_by,omitempty"`
	Updated time.Time   `json:"changed_at,omitzero"`
	Started int         `json:"started"`
	Text    string      `json:"-"`
	Open    bool        `json:"-"`
}

type againView struct {
	Failed time.Time `json:"failed,omitzero"`
	Err    string    `json:"error,omitempty"`
	Next   time.Time `json:"next,omitzero"`
}

// dataView is where the data of a criterion's selectors begins, as a page
// says it: each selector it reads (when it first had data, from before
// From, or none since From, and then the stand-in its record is kept by,
// if any: nothing it matches happened), the one that holds the record back
// (Late), and whether one has none at all.
type dataView struct {
	From      time.Time      `json:"from"`
	Selectors []selectorView `json:"selectors"`
	Late      string         `json:"late,omitempty"`
	NoData    bool           `json:"no_data,omitempty"`
	Floor     time.Time      `json:"floor,omitzero"`
}

type selectorView struct {
	ledger.SelectorData
	// StandIn is the stand-in it begins by, from Kept, with no data of its
	// own or before it.
	StandIn string    `json:"stand_in,omitempty"`
	Kept    time.Time `json:"kept_from,omitzero"`
}

// dataOf is where the data of a criterion's selectors begins, as the
// ledger keeps it: of the one kept; nil for another, or before it was
// looked for.
func dataOf(k ledger.ImpactKept, key string) *ledger.ImpactData {
	if d := k.Data; d != nil && d.Criterion == key && key == k.Criterion {
		return d
	}
	return nil
}

func dataViewOf(d *ledger.ImpactData) *dataView {
	if d == nil {
		return nil
	}
	v := &dataView{From: d.From, Selectors: []selectorView{}, Late: d.Late(), NoData: d.NoData, Floor: d.Floor}
	for _, x := range d.Selectors {
		if x.For != "" {
			continue // a stand-in: said of the selector it stands in for
		}
		sv := selectorView{SelectorData: x}
		sv.StandIn, sv.Kept = d.StandInOf(x.Sel)
		v.Selectors = append(v.Selectors, sv)
	}
	return v
}

// showCriterion is a criteria setting as it is in c, as text: off for no
// expression.
func showCriterion(c criterion, key string) string {
	switch key {
	case "impact":
		if c.Expr == "" {
			return impactOff
		}
		return c.Expr
	case "impact-for":
		return verdict.Duration(c.For)
	}
	return ""
}

func (s *Server) criteriaView(ctx context.Context, set map[string]ledger.Setting) (*criteriaView, error) {
	c := s.criterionOf(set)
	started := criterion{s.cfg.Impact, s.cfg.ImpactFor}
	v := &criteriaView{NoHistory: s.h == nil, Expr: c.Expr, For: verdict.Duration(c.For)}
	var err error
	if v.Depends, err = s.dependsViewOf(ctx, set); err != nil {
		return nil, err
	}
	// The match in effect, as a signal that follows it is matched on.
	o := s.cfg.Options
	if x, ok := set["match"]; ok {
		_ = apply(&o, "match", x.Value) // one that doesn't parse leaves the command line's, as options does
	}
	inEffect, above := strings.Join(o.Match, ", "), verdict.Duration(s.leadOf(set))
	v.Add = signalForm{For: verdict.Duration(s.cfg.ImpactFor), Follows: true, Record: true, InEffect: inEffect, LeadAbove: above}
	if n := s.signalsIn(set); n.named() {
		err := s.namedViews(ctx, v, n)
		for i := range v.Named {
			v.Named[i].Form.InEffect, v.Named[i].Form.LeadAbove = inEffect, above
		}
		return v, err
	}
	v.Renames = c.Expr != ""
	for _, st := range criteriaSettable {
		x := settingView{Key: st.key, Flag: st.flag, CommandLine: showCriterion(started, st.key), Value: showCriterion(c, st.key)}
		if st.key == "impact" && started.Expr == "" {
			x.CommandLine = ""
		}
		if ch, ok := set[st.key]; ok {
			x.Changed, x.By, x.Updated = true, ch.By, ch.Updated
		}
		v.Settings = append(v.Settings, x)
	}
	if c.Expr == "" {
		return v, nil
	}
	plan, k, err := s.againPlan(ctx, c)
	if err != nil {
		return nil, err
	}
	v.Since, v.Floor = k.From(), k.Floor
	v.Data = dataViewOf(dataOf(k, c.key()))
	if plan == againRead {
		v.Again = &againView{}
		if a := s.shownSources().Again; a != nil && a.Key == c.key() {
			v.Again.Failed, v.Again.Err, v.Again.Next = a.Failed, a.Err, a.Next
		}
	}
	return v, nil
}

// readOver reads a criterion over the span a try reads, and a read again,
// in the heavy turn: its windows, and where the data of its selectors
// begins. Within tryWithin and --max-samples.
func (s *Server) readOver(ctx context.Context, c criterion, from, to time.Time) ([]incident.Incident, ledger.ImpactData, error) {
	ctx, cancel := context.WithTimeout(ctx, min(s.cfg.Timeout, tryWithin))
	defer cancel()
	series, d, err := s.readSpan(ctx, c, from, to)
	if err != nil {
		return nil, d, err
	}
	ws, err := incident.FromImpact(ctx, series, s.cfg.Step, c.For)
	return ws, d, err
}

// readSpan reads a criterion over the span a try reads, and a save that
// reads first, named or not: its series, and where the data of its
// selectors begins. Within tryWithin and --max-samples, in the heavy turn
// the caller holds.
func (s *Server) readSpan(ctx context.Context, c criterion, from, to time.Time) ([]prom.Series, ledger.ImpactData, error) {
	ctx, cancel := context.WithTimeout(ctx, min(s.cfg.Timeout, tryWithin))
	defer cancel()
	d := ledger.ImpactData{Criterion: c.key(), From: from, CheckedTo: to}
	series, err := s.h.QueryRange(ctx, c.Expr, from, to, s.cfg.Step)
	if err != nil {
		return nil, d, err
	}
	if d.Selectors, err = s.firstSamples(ctx, c.Expr, from, to, true); err != nil {
		return nil, d, err
	}
	return series, d, nil
}

// windowsTried works out, of the windows a try found, which would count, as
// the ledger has it (ledger.ReadAgain, then cover): those no incident
// written down covers, on the window's match or o's, and nothing decided
// before holds for (found, decidedOver); the first triedWindows of them to
// show, and how many more. It gives up when ctx is done.
func windowsTried(ctx context.Context, windows, written []incident.Incident, found []ledger.Incident, from time.Time, o verdict.Options) ([]triedWindow, int, []incident.Incident, error) {
	var shown []triedWindow
	var counting []incident.Incident
	more := 0
	for _, w := range windows {
		if err := ctx.Err(); err != nil {
			return nil, 0, nil, err
		}
		tw := triedWindow{Start: w.Start, End: w.End, Len: w.End.Sub(w.Start), Labels: w.Labels}
		tw.Decided = decidedOver(found, from, w)
		for _, x := range written {
			if len(incident.Merge([]incident.Incident{x}, []incident.Incident{w}, o.UnknownLen, o.Match)) == 1 {
				tw.Written = append(tw.Written, x.ID)
			}
		}
		if tw.Decided == "" && len(tw.Written) == 0 {
			counting = append(counting, w)
		}
		if len(shown) < triedWindows {
			shown = append(shown, tw)
		} else {
			more++
		}
	}
	return shown, more, counting, nil
}

// postImpact saves the incident criteria, an admin's: an expression that
// reads, or empty (or off) for none; a for within bounds. A new one is read
// in full over the last four weeks first, as a try reads it, unless a try
// did within triedFor: what can't be read within the bounds isn't saved.
// Saved, the replica that leads reads it again (impactAgain), which judges
// the weeks again.
func (s *Server) postImpact(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	ctx := r.Context()
	set, err := s.l.Settings(ctx)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if s.signalsIn(set).named() {
		s.oops(w, r, errBesideNamed)
		return
	}
	values := map[string]string{}
	expr := impactText(r.PostFormValue("impact"))
	switch {
	case r.PostFormValue("reset-impact") != "":
		values["impact"] = ""
	case expr == "" || expr == impactOff:
		values["impact"] = impactOff
		if s.cfg.Impact == "" {
			values["impact"] = "" // the command line's: nothing to keep
		}
	default:
		if s.h == nil {
			s.oops(w, r, errNoHistory)
			return
		}
		// The form posts the expression back whatever was changed: the
		// one serve was started with is the command line's, which the UI's
		// checks aren't for, and a change of --impact-for alone is no
		// change of it. Only one an admin changed is checked.
		values["impact"] = ""
		if expr != impactText(s.cfg.Impact) {
			if err := checkImpact(expr); err != nil {
				s.oops(w, r, err)
				return
			}
			values["impact"] = expr
		}
	}
	forText := strings.TrimSpace(r.PostFormValue("impact-for"))
	switch {
	case r.PostFormValue("reset-impact-for") != "":
		values["impact-for"] = ""
	case forText == "":
		values["impact-for"] = ""
	default:
		d, err := s.checkImpactFor(forText)
		if err != nil {
			s.oops(w, r, err)
			return
		}
		values["impact-for"] = verdict.Duration(d)
		if d == s.cfg.ImpactFor {
			values["impact-for"] = ""
		}
	}
	after := map[string]ledger.Setting{}
	for k, v := range set {
		after[k] = v
	}
	for k, v := range values {
		if v == "" {
			delete(after, k)
		} else {
			after[k] = ledger.Setting{Value: v}
		}
	}
	before, next := s.criterionOf(set), s.criterionOf(after)
	if next.Expr != "" && next.key() != before.key() {
		// The replica that leads reads it again over the last four weeks:
		// counted among the saver's reads, whether a try read it lately or
		// not, so that saving back and forth reads no more than trying.
		if !s.takeTry(ctx, s.user(r)) {
			s.oops(w, r, errImpactTries)
			return
		}
		if !s.tried.recent(next.key(), time.Now()) {
			if err := s.readToSave(ctx, s.user(r), next); err != nil {
				s.oops(w, r, err)
				return
			}
		}
	}
	if err := s.l.SetSettings(ctx, values, s.user(r)); err != nil {
		s.oops(w, r, err)
		return
	}
	if next.key() != before.key() {
		// Read again, and judged again, by the replica that leads: here,
		// or wherever it runs.
		s.poke()
	}
	back(w, r, "/settings#incident-criteria")
}

// readToSave reads a criterion about to be saved over the span a try
// reads, as a try does: counted among who's reads, in the heavy turn.
func (s *Server) readToSave(ctx context.Context, who string, c criterion) error {
	if !s.takeTry(ctx, who) {
		return errImpactTries
	}
	from, to, err := s.trySpan(ctx)
	if err != nil {
		return err
	}
	done, err := s.busy(ctx)
	if err != nil {
		return err
	}
	defer done()
	if _, _, err := s.readOver(ctx, c, from, to); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return bad("the impact expression couldn't be read over the last four weeks within --timeout (%s at most) and --max-samples, so nothing was saved: %v",
			verdict.Duration(tryWithin), promText(err))
	}
	s.tried.put(c.key(), time.Now())
	return nil
}

// trySpan is the span a try reads, and a read again: from againFrom to
// where the runs have got to, or to now before the first.
func (s *Server) trySpan(ctx context.Context) (time.Time, time.Time, error) {
	to, err := s.l.Cursor(ctx)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if to.IsZero() {
		to = s.now().Truncate(s.cfg.Step)
	}
	from, err := s.againFrom(ctx, to)
	return from, to, err
}

// againFrom is from when a criterion is read again, or tried, up to to:
// the Monday impactAgainWeeks weeks before to's, or, of a ledger whose runs
// began since, where they began, so that no window is found where nothing
// was replayed to catch it.
func (s *Server) againFrom(ctx context.Context, to time.Time) (time.Time, error) {
	from := s.l.Midnight(ledger.Week(s.l.Day(to)) - 7*impactAgainWeeks)
	began, err := s.l.RunsFrom(ctx)
	if err != nil || began.IsZero() || !began.After(from) {
		return from, err
	}
	return began, nil
}

// againPlan is what the replica that leads does about the criterion in
// effect at its next round (impactAgain).
type againPlan int

const (
	againNone againPlan = iota // read already, or the runs read it from their first
	againMark                  // read all along by the runs: kept so
	againRead                  // to be read again over the last four weeks
)

// againPlan works out what the replica that leads does about c, with what
// the ledger keeps of the impact signal. Read it again, while a floor
// stands (ledger.ImpactKept.Floor): whatever was read since, and whatever
// impact_again says, as an older leader may have left it, only that lifts
// it. Nothing: c is read again already, or there is no run yet, whose
// first reads it from its start. Mark it:
// the runs read c from where a read again would begin, or before, all
// along (a ledger whose runs read it from their first; a lapilli before
// 0.49's, which kept no criterion, when c is the command line's, as that
// one's runs were taken to read it). Else read it again: c saved, or
// started with, since the runs read another, or turned on after they
// read none.
func (s *Server) againPlan(ctx context.Context, c criterion) (againPlan, ledger.ImpactKept, error) {
	return s.againPlanOf(ctx, "", c, s.assumedKey())
}

// againPlanOf is againPlan of a signal by name, "" for the unnamed one;
// assumed is the criterion a ledger that kept none is taken to have been
// read with, "" for a named signal, which no lapilli before 0.49 read.
func (s *Server) againPlanOf(ctx context.Context, name string, c criterion, assumed string) (againPlan, ledger.ImpactKept, error) {
	k, err := s.l.SignalKept(ctx, name)
	switch {
	case err != nil || c.Expr == "":
		return againNone, k, err
	case !k.Floor.IsZero():
		return againRead, k, nil // only a read again lifts it, whatever was read since
	case k.AgainDone(c.key()):
		return againNone, k, nil
	}
	cursor, err := s.l.Cursor(ctx)
	if err != nil || cursor.IsZero() {
		return againNone, k, err
	}
	from, err := s.againFrom(ctx, cursor)
	if err != nil {
		return againNone, k, err
	}
	if k.Again == "" && !k.Since.IsZero() && !k.Since.After(from) &&
		(k.Criterion == c.key() || k.Criterion == "" && assumed != "" && c.key() == assumed) {
		return againMark, k, nil
	}
	return againRead, k, nil
}

// againState is how reading a criterion again over the last four weeks
// went: Key, the criterion; when it last failed, why, and when it is tried
// again, an hour later, then two, four, up to a day.
type againState struct {
	Key    string    `json:"key"`
	Tries  int       `json:"tries"`
	Failed time.Time `json:"failed,omitzero"`
	Err    string    `json:"error,omitempty"`
	Next   time.Time `json:"next,omitzero"`
}

// impactAgain does what againPlan says: reads the criterion in effect
// again over the last four weeks, with where the data of its selectors
// begins there; the windows it finds take the place of those found from
// then on (ledger.ReadAgain), the weeks are judged again, a week a write,
// and the record is kept by it from then on, never before its data. One
// that fails is tried again later, a while after, and the runs read the
// criterion meanwhile from where they are, under a floor: no record by the
// signal until it is read. It reports whether it read it again.
func (s *Server) impactAgain(ctx context.Context, o verdict.Options) (bool, error) {
	n, set, err := s.signalsNowOf(ctx)
	if err != nil {
		return false, err
	}
	if n.named() {
		return s.signalsAgain(ctx, o, n)
	}
	c := s.criterionOf(set)
	if c.Expr == "" {
		return false, nil
	}
	plan, _, err := s.againPlan(ctx, c)
	switch {
	case err != nil || plan == againNone:
		return false, err
	case plan == againMark:
		return false, s.l.MarkAgain(ctx, c.key())
	}
	now := s.now()
	s.sources.Lock()
	a := s.sources.again
	s.sources.Unlock()
	if a.Key == c.key() && now.Before(a.Next) {
		return false, nil
	}
	cursor, err := s.l.Cursor(ctx)
	if err != nil {
		return false, err
	}
	from, err := s.againFrom(ctx, cursor)
	if err != nil {
		return false, err
	}
	s.status.set("reading the impact signal again over the last four weeks")
	rctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	series, err := s.h.QueryRange(rctx, c.Expr, from, cursor, s.cfg.Step)
	var sels []ledger.SelectorData
	if err == nil {
		sels, err = s.firstSamples(rctx, c.Expr, from, cursor, true)
	}
	var windows []incident.Incident
	if err == nil {
		windows, err = incident.FromImpact(rctx, series, s.cfg.Step, c.For)
	}
	if err == nil {
		err = s.l.ReadAgain(ctx, ledger.ImpactRead{From: from, To: cursor, Criterion: c.key(), Windows: windows,
			Data: ledger.ImpactData{From: from, CheckedTo: cursor, Selectors: sels}}, o)
		if errors.Is(err, ledger.ErrRunsMoved) {
			return false, nil // read again at the next round, to where they are
		}
	}
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return false, err
	}
	s.readImpact(err, c.key())
	if err == nil {
		err = s.judgePending(ctx, o)
	}
	s.sources.Lock()
	defer s.sources.Unlock()
	if err != nil {
		if s.sources.again.Key != c.key() {
			s.sources.again = againState{Key: c.key()}
		}
		g := &s.sources.again
		g.Tries++
		g.Failed, g.Err = now, promText(err)
		g.Next = now.Add(min(time.Hour<<min(g.Tries-1, 5), 24*time.Hour))
		return false, fmt.Errorf("--impact read again over the last four weeks: %w", err)
	}
	s.sources.again = againState{}
	return true, nil
}

// impactData looks for where the data of the selectors of the criterion
// the ledger keeps the record by begins, from where the record starts, and
// keeps it (ledger.SetImpactData), which moves the record's start forward
// to the first whole week after it: on a first start, after an upgrade,
// and while a selector has no data yet, a round at a time, each selector
// from where its last look got to, those it could look at kept when one
// fails. Before it is looked for, no record is kept by the signal. Once
// each had data, it isn't looked for again; nor while a floor stands,
// which keeps no record whatever the data, until a read again brings its
// own.
func (s *Server) impactData(ctx context.Context) error {
	n, _, err := s.signalsNowOf(ctx)
	if err != nil {
		return err
	}
	if n.named() {
		return s.signalsData(ctx, n)
	}
	return s.impactDataOf(ctx, "", nil)
}

// impactDataOf is impactData of one signal, by name, "" for the unnamed
// one, within --timeout; of a named one (kept given), within the time ctx
// has, which the signals share.
func (s *Server) impactDataOf(ctx context.Context, name string, kept *ledger.ImpactKept) error {
	var k ledger.ImpactKept
	var err error
	if kept != nil {
		k = *kept
	} else if k, err = s.l.SignalKept(ctx, name); err != nil {
		return err
	}
	if s.h == nil || k.Since.IsZero() || k.Criterion == "" || !k.Floor.IsZero() {
		return nil
	}
	cursor, err := s.l.Cursor(ctx)
	if err != nil {
		return err
	}
	rctx := ctx
	if kept == nil {
		var cancel context.CancelFunc
		rctx, cancel = context.WithTimeout(ctx, s.cfg.Timeout)
		defer cancel()
	}
	// What is looked for is of the expression serve runs, not of the one
	// the record names: a record kept by another, until it is read again,
	// has nothing of it looked for.
	expr, err := s.heldExpr(ctx, name)
	if err != nil {
		return err
	}
	if expr == "" || exprOfKey(k.Criterion) != expr {
		return nil
	}
	d := k.Data
	if d == nil || d.Criterion != k.Criterion {
		d = &ledger.ImpactData{Criterion: k.Criterion, From: k.Since}
	}
	return s.lookFor(rctx, name, d, cursor, expr)
}

// heldExpr is the expression serve runs for a signal, by name, "" for the
// unnamed one: a named one's in effect, as serve was started
// (--impact NAME=PROMQL, --impact-file) or as an admin set it on the
// Settings page, and the unnamed one's, the criterion in effect, which
// the runs read; "" for none. The criterion in effect is the one the
// Settings page keeps in the ledger, when it keeps one (criterionOf), so
// the unnamed signal's can be what whoever can write the ledger wrote
// there: the ledger is guarded as serve's tokens are (SECURITY.md).
func (s *Server) heldExpr(ctx context.Context, name string) (string, error) {
	if name != "" {
		// Of the named signals in effect: serve's start, with what an
		// admin set on the Settings page over it (signalsIn).
		n, _, err := s.signalsNowOf(ctx)
		if err != nil {
			return "", err
		}
		d, _ := n.def(name)
		return d.Expr, nil
	}
	c, err := s.criterion(ctx)
	return c.Expr, err
}

// lookFor looks, of d's selectors, for where the data of those with none
// found yet begins, each from where its last look got to up to cursor, and
// keeps what it found: of each it could look at, a selector that fails
// passing to the next, and returning the last failure. Only the selectors
// expr, the expression serve runs (heldExpr), reads (ledger.SelectorsOf)
// are asked for: a selector kept beside the record, in d, is asked for
// when it is one of them, never one outside the expression serve runs.
// That is all this guards: it doesn't say who wrote expr.
func (s *Server) lookFor(ctx context.Context, name string, d *ledger.ImpactData, cursor time.Time, expr string) error {
	want, err := ledger.SelectorsOf(expr)
	if err != nil {
		return err
	}
	if d.Selectors != nil {
		if err := withStandIns(d); err != nil {
			return err
		}
	}
	if d.Selectors != nil && d.Complete() || !cursor.After(d.From) {
		return nil
	}
	if d.Selectors == nil {
		d.Selectors = append([]ledger.SelectorData{}, want...)
		if len(want) == 0 {
			// Reading no selector (vector(1)), it has data all along.
			d.CheckedTo = cursor
			return s.l.SetSignalData(ctx, name, *d)
		}
	}
	var failed error
	for i, x := range d.Selectors {
		if !x.First.IsZero() || x.For != "" && !d.HasData(x.Of) {
			continue // a stand-in of a selector with no data has none either
		}
		if !slices.ContainsFunc(want, func(w ledger.SelectorData) bool { return w.Sel == x.Sel }) {
			continue // not one expr reads
		}
		lo := x.To
		if lo.IsZero() {
			lo = d.From
		}
		if !cursor.After(lo) {
			continue
		}
		got, err := s.firstSample(ctx, x.Sel, lo, cursor, x.To.IsZero())
		if err != nil {
			failed = err
			continue
		}
		got.For, got.Of, got.To = x.For, x.Of, cursor
		d.Selectors[i] = got
	}
	d.CheckedTo = cursor
	if err := s.l.SetSignalData(context.WithoutCancel(ctx), name, *d); err != nil {
		return err
	}
	return failed
}

// withStandIns gives d, kept by a lapilli before stand-ins
// (ledger.SelectorsOf), those of its criterion, to be looked for: a
// stand-in a lapilli before kept as a selector, its "for" let go, gets it
// back, so that it isn't taken for one the expression reads.
func withStandIns(d *ledger.ImpactData) error {
	expr := exprOfKey(d.Criterion)
	read, err := rules.Selectors(expr)
	if err != nil {
		return err
	}
	want, err := ledger.SelectorsOf(expr)
	if err != nil {
		return err
	}
	for _, w := range want {
		if w.For == "" {
			continue
		}
		if slices.ContainsFunc(d.Selectors, func(x ledger.SelectorData) bool { return x.Sel == w.Sel && x.For == w.For }) {
			continue
		}
		if i := slices.IndexFunc(d.Selectors, func(x ledger.SelectorData) bool {
			return x.Sel == w.Sel && x.For == "" && !slices.Contains(read, x.Sel)
		}); i >= 0 {
			d.Selectors[i].For, d.Selectors[i].Of = w.For, w.Of
			continue
		}
		d.Selectors = append(d.Selectors, w)
	}
	return nil
}

// sourcesSeen is what the replica that leads saw of where incidents come
// from: when the impact signal was last read, with which criterion, and
// the incidents file, and when either last failed, and why; and how
// reading a criterion again went (againState).
type sourcesSeen struct {
	ImpactRead   time.Time   `json:"impact_read,omitzero"`
	ImpactKey    string      `json:"impact_key,omitempty"`
	ImpactFailed time.Time   `json:"impact_failed,omitzero"`
	ImpactErr    string      `json:"impact_error,omitempty"`
	FileRead     time.Time   `json:"file_read,omitzero"`
	FileFailed   time.Time   `json:"file_failed,omitzero"`
	FileErr      string      `json:"file_error,omitempty"`
	Again        *againState `json:"again,omitempty"`
	// Signals are how the last read of each named signal went, and
	// AgainBy how reading each again goes, by name.
	Signals map[string]signalSeen `json:"signals,omitempty"`
	AgainBy map[string]againState `json:"again_by,omitempty"`
}

// sourcesState is sourcesSeen as this replica keeps it, and when it last
// noted each source writing through the incidents port (noteReceived).
type sourcesState struct {
	sync.Mutex
	impactRead, impactFailed time.Time
	impactKey, impactErr     string
	fileRead, fileFailed     time.Time
	fileErr                  string
	again                    againState
	noted                    map[string]time.Time
	signals                  map[string]signalSeen
	againBy                  map[string]againState
}

// sourcesNow is this replica's sourcesSeen.
func (s *Server) sourcesNow() *sourcesSeen {
	s.sources.Lock()
	defer s.sources.Unlock()
	x := &sourcesSeen{ImpactRead: s.sources.impactRead, ImpactKey: s.sources.impactKey, ImpactFailed: s.sources.impactFailed, ImpactErr: s.sources.impactErr,
		FileRead: s.sources.fileRead, FileFailed: s.sources.fileFailed, FileErr: s.sources.fileErr}
	if s.sources.again.Key != "" {
		a := s.sources.again
		x.Again = &a
	}
	if len(s.sources.signals) > 0 {
		x.Signals = maps.Clone(s.sources.signals)
	}
	if len(s.sources.againBy) > 0 {
		x.AgainBy = maps.Clone(s.sources.againBy)
	}
	return x
}

// shownSources is what the pages say was seen of the sources: as the
// status is (shownStatus), the leader's, as it last said it, on a replica
// that follows one; else this replica's own, as with a shared ledger and
// no replica known to lead.
func (s *Server) shownSources() sourcesSeen {
	if !s.follows() {
		return *s.sourcesNow()
	}
	if x := s.shownStatus().Sources; x != nil {
		return *x
	}
	return sourcesSeen{}
}

// readImpact notes how a read of the impact signal with the criterion key
// went.
func (s *Server) readImpact(err error, key string) {
	s.noteFailing("impact", err)
	s.sources.Lock()
	defer s.sources.Unlock()
	if err != nil {
		s.sources.impactFailed, s.sources.impactErr = s.now(), promText(err)
		return
	}
	s.sources.impactRead, s.sources.impactKey, s.sources.impactErr = s.now(), key, ""
}

// readFile notes how a read of the incidents file went.
func (s *Server) readFile(err error) {
	s.noteFailing("incidents-file", err)
	s.sources.Lock()
	defer s.sources.Unlock()
	if err != nil {
		s.sources.fileFailed, s.sources.fileErr = s.now(), err.Error()
		return
	}
	s.sources.fileRead, s.sources.fileErr = s.now(), ""
}

// The try (docs/design/ui.md, "Incident criteria"): what a criterion
// would make incidents of over the last four weeks, and what that would do
// to the verdict, beside what is in effect, written nowhere.

// What a try shows at most: the windows, and the proposals named.
const (
	triedWindows   = 50
	triedProposals = 10
)

// impactTry is a try's result: the period, the criterion as typed, why it
// couldn't be tried (Err, a text in the reader's language); the windows it
// finds (Windows, the first triedWindows of Found, Counted of them would
// count), where the data of its selectors begins (Data), the verdict in
// effect and with it (Rows), and the proposals waiting for approval it
// would hold until they are backtested again. Same says it is the
// criterion the ledger holds: nothing would change.
type impactTry struct {
	From, To    time.Time
	Expr, For   string
	Off         bool
	Same        bool
	Err         string
	Windows     []triedWindow
	Found       int
	Counted     int
	MoreWindows int
	Data        *dataView
	Rows        []triedRow
	Stale       []triedProposal
	StaleMore   int
	StaleN      int
}

// triedWindow is a window a try found: when, how long, its labels (the
// page sums them up: labelsum), and, when it wouldn't count, the incidents
// written down over it, or what was decided about a window found before
// over it.
type triedWindow struct {
	Start, End time.Time
	Len        time.Duration
	Labels     incident.Labels
	Written    []string
	Decided    string
}

// triedRow is a measure of the verdict, in effect and with the criterion
// tried, as the page says them; Changed when they differ.
type triedRow struct {
	Label     string
	Now, With string
	Changed   bool
}

// triedProposal is a proposal a try would hold.
type triedProposal struct {
	ID   int64
	Rule string
}

// tooManyToTry is a try over more firings than a report reads at once.
const tooManyToTry = "the last four weeks have %d firings, more than %d, the most a try of the incident criteria judges at once"

// tryImpact tries the criterion the Settings page's form gives, an
// admin's, from this site: checked as saving it is, then read over the
// last four weeks, within tryWithin and --max-samples, one heavy request
// at a time and impactTriesAnHour an admin; what it couldn't read is said
// in its result, the form kept as typed.
func (s *Server) tryImpact(r *http.Request, v *criteriaView) error {
	ctx := r.Context()
	if s.role(r) != admin {
		return s.forbidden(admin)
	}
	if !fromHere(r) {
		return errNotFromHere
	}
	q := r.URL.Query()
	try := &impactTry{Expr: impactText(q.Get("impact")), For: strings.TrimSpace(q.Get("impact-for"))}
	v.Expr, v.For, v.Tried = try.Expr, try.For, try
	l := lang(r)
	c := criterion{Expr: try.Expr, For: s.cfg.ImpactFor}
	if try.For != "" {
		d, err := s.checkImpactFor(try.For)
		if err != nil {
			try.Err = message(l, err)
			return nil
		}
		c.For = d
	} else {
		try.For = verdict.Duration(c.For)
	}
	if c.Expr == impactOff {
		c.Expr = ""
	}
	try.Off = c.Expr == ""
	if !try.Off {
		if s.h == nil {
			try.Err = message(l, errNoHistory)
			return nil
		}
		if err := checkImpact(c.Expr); err != nil {
			try.Err = message(l, err)
			return nil
		}
		if !s.takeTry(ctx, s.user(r)) {
			return errImpactTries
		}
	}
	var err error
	if try.From, try.To, err = s.trySpan(ctx); err != nil {
		return err
	}
	if n, err := s.l.CountFirings(ctx, try.From, try.To); err != nil {
		return err
	} else if n > reportFirings {
		return bad(tooManyToTry, n, reportFirings)
	}
	done, err := s.busy(ctx)
	if err != nil {
		return err
	}
	defer done()
	var windows []incident.Incident
	var data ledger.ImpactData
	if !try.Off {
		windows, data, err = s.readOver(ctx, c, try.From, try.To)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			try.Err = tr(l, "Prometheus couldn't read it within %s and --max-samples: %s", verdict.Duration(min(s.cfg.Timeout, tryWithin)), promText(err))
			return nil
		}
		s.l.Floor(&data)
		try.Data = dataViewOf(&data)
		s.tried.put(c.key(), time.Now())
	}
	return s.judgeTried(ctx, l, try, c, windows, data)
}

// judgeTried works out, for a try, which windows would count, the verdict
// over its period in effect and with them, the weeks before it that would
// lose their record, and the proposals it would hold. Tried, the criterion
// the ledger holds (read again already) changes nothing: the verdict with
// it is the one in effect.
func (s *Server) judgeTried(ctx context.Context, l string, try *impactTry, c criterion, windows []incident.Incident, data ledger.ImpactData) error {
	o, err := s.options(ctx)
	if err != nil {
		return err
	}
	k, err := s.l.ImpactKept(ctx)
	if err != nil {
		return err
	}
	from, to := try.From, try.To
	o.From, o.To = from, to
	// The criterion the ledger holds the windows of, all along: the runs',
	// with no read again due; or, of a ledger that kept none, the command
	// line's.
	try.Same = !try.Off && k.Again != "-" && (c.key() == k.Criterion || k.Criterion == "" && c.key() == s.assumedKey())
	with := o
	with.ImpactFrom = time.Time{}
	if !try.Off && !data.NoData {
		with.ImpactFrom = later(from, data.Floor)
	}
	// The incidents that keep the record of the period's weeks, and what
	// was decided about the windows found before over it.
	wf, wt := s.l.UnitsOf(o, from, to)
	around, err := s.l.RecordIncidents(ctx, verdict.RecordFrom(wf, wt), wt, o.UnknownLen)
	if err != nil {
		return err
	}
	found, err := s.l.DecidedImpact(ctx, from)
	if err != nil {
		return err
	}
	var written []incident.Incident
	for _, in := range around {
		if in.Origin != "impact" {
			written = append(written, in.Incident)
		}
	}
	// Each window found: whether it would count, as the ledger has it
	// (ledger.ReadAgain, then cover).
	try.Found = len(windows)
	var counting []incident.Incident
	if try.Windows, try.MoreWindows, counting, err = windowsTried(ctx, windows, written, found, from, o); err != nil {
		return err
	}
	try.Counted = len(counting)
	nowIncs := make([]incident.Incident, 0, len(around))
	for _, in := range around {
		nowIncs = append(nowIncs, in.Incident)
	}
	withIncs := triedIncidents(around, from, try.Off, counting)
	if try.Same {
		with, withIncs = o, nowIncs
	}

	fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: from, To: to})
	if err != nil {
		return err
	}
	plain := make([]firing.Firing, 0, len(fs))
	live := make([]ledger.Firing, 0, len(fs))
	for _, f := range fs {
		if !f.Excused {
			plain = append(plain, f.Firing)
			live = append(live, f)
		}
	}
	// With rules noisy by what was done about their firings, the preview
	// asks the same test as every surface, so it is given the responses.
	if rsp, err := s.previewResponses(ctx, live, o); err != nil {
		return err
	} else if rsp != nil {
		o.Responder, with.Responder = rsp, rsp
	}
	// People's verdicts, on both sides (docs/design/judging.md, item 3).
	said, err := s.l.ReplayVerdicts(ctx, from, to)
	if err != nil {
		return err
	}
	o.Verdicts, with.Verdicts = said, said
	o, with = s.graphed(ctx, o, nowIncs), s.graphed(ctx, with, withIncs)
	nowRep := verdict.Judge(nil, plain, nowIncs, o)
	withRep := verdict.Judge(nil, plain, withIncs, with)
	units := s.l.SpanUnits(o, from, to)
	a := triedSumOf(nowRep, verdict.RecordOf(nowIncs, o), o, units, s.l.Midnight, from, to)
	b := triedSumOf(withRep, verdict.RecordOf(withIncs, with), with, units, s.l.Midnight, from, to)
	row := func(label, x, y string) {
		try.Rows = append(try.Rows, triedRow{Label: label, Now: x, With: y, Changed: x != y})
	}
	of := func(n, m int) string { return tr(l, "%d of %d", n, m) }
	// The weeks before the span the record is kept of now, which it would
	// keep by incidents alone: the record by the signal starts within the
	// span.
	if held := k.From(); !try.Same && !held.IsZero() {
		fw, _ := o.RecordUnit(from)
		if held.Before(fw) {
			bw := s.l.SpanUnits(o, held, fw)
			bf, _ := o.RecordUnit(held)
			earlier, err := s.l.RecordIncidents(ctx, verdict.RecordFrom(bf, fw), fw, o.UnknownLen)
			if err != nil {
				return err
			}
			plainE := make([]incident.Incident, 0, len(earlier))
			for _, in := range earlier {
				plainE = append(plainE, in.Incident)
			}
			na, nb := 0, 0
			ra, rb := verdict.RecordOf(plainE, o), verdict.RecordOf(plainE, with)
			n := ledger.UnitDays(o)
			for _, w := range bw {
				na += btoi(ra.Kept(s.l.Midnight(w), s.l.Midnight(w+n)))
				nb += btoi(rb.Kept(s.l.Midnight(w), s.l.Midnight(w+n)))
			}
			row(unitLabel(o, "Days before them with a record", "Weeks before them with a record"), of(na, len(bw)), of(nb, len(bw)))
		}
	}
	row(unitLabel(o, "Days with a record", "Weeks with a record"), of(a.kept, len(units)), of(b.kept, len(units)))
	row("Incidents judged against", fmt.Sprint(a.incidents), fmt.Sprint(b.incidents))
	row("Of them, impact windows", fmt.Sprint(a.impact), fmt.Sprint(b.impact))
	row("Firings judged", of(a.judged, a.firings), of(b.judged, b.firings))
	row("Firings with no record", fmt.Sprint(a.firings-a.judged), fmt.Sprint(b.firings-b.judged))
	row("Precision", pct(a.white, a.judged), pct(b.white, b.judged))
	row("Noisy rules", fmt.Sprint(a.noisy), fmt.Sprint(b.noisy))
	row("Incidents missed", fmt.Sprint(a.missed), fmt.Sprint(b.missed))
	if try.Same {
		return nil
	}
	return s.triedStale(ctx, try, c, with, from, counting)
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// decidedOver is what was decided about a window found before that would
// hold for w, as the ledger has it (ledger's decidedFor), of those a read
// from from on would replace or end there (ledger.DecidedImpact): of one
// that covers half of w or more, sharing a value of each label both have;
// the one that covers most of it, confirmed before dismissed on a tie. ""
// for none.
func decidedOver(found []ledger.Incident, from time.Time, w incident.Incident) string {
	s, e := w.Start.Unix(), w.End.Unix()
	best, most := "", int64(0)
	for _, d := range found {
		if d.State == "" || !d.End.After(from) {
			continue
		}
		ov := max(0, min(e, d.End.Unix())-max(s, d.Start.Unix()))
		if 2*ov < e-s || e <= s || !shareLabels(d.Labels, w.Labels) {
			continue
		}
		if best == "" || ov > most || ov == most && d.State == "confirmed" && best != "confirmed" {
			best, most = d.State, ov
		}
	}
	return best
}

// triedIncidents are the incidents a criterion tried would leave keeping
// the record and judged against, of those around now: from from on, the
// windows found before give way to those it would count; one on at from
// ends there. Turned off, they all stay, as the runs leave them.
func triedIncidents(around []ledger.Incident, from time.Time, off bool, counting []incident.Incident) []incident.Incident {
	out := make([]incident.Incident, 0, len(around)+len(counting))
	for _, in := range around {
		x := in.Incident
		if in.Origin == "impact" && !off {
			if !x.Start.Before(from) {
				continue
			}
			if x.End.After(from) {
				x.End = from
			}
		}
		out = append(out, x)
	}
	return append(out, counting...)
}

// triedSum is a judgement in sum, over a try's period.
type triedSum struct {
	kept, incidents, impact, missed int
	firings, judged, white, noisy   int
}

func triedSumOf(rep *verdict.Report, rec verdict.Record, o verdict.Options, units []int64, mid func(int64) time.Time, from, to time.Time) triedSum {
	var x triedSum
	n := ledger.UnitDays(o)
	for _, w := range units {
		if rec.Kept(mid(w), mid(w+n)) {
			x.kept++
		}
	}
	for _, in := range rep.Incidents {
		if in.Start.Before(from) || !in.Start.Before(to) {
			continue
		}
		x.incidents++
		if in.Candidate {
			x.impact++
		}
		if in.Missed() {
			x.missed++
		}
	}
	for _, r := range rep.Rules {
		x.firings += r.Firings
		x.judged += r.Firings - r.Unjudged
		x.white += r.White
		if r.Noisy {
			x.noisy++
		}
	}
	return x
}

// triedStale names the proposals waiting for approval, not held now, that
// saving the criterion tried would hold until they are backtested again,
// as proposalsNow will once it is read again: backtested against the
// windows of another criterion than it (staleImpact), or their backtest's
// period would have a record where it had none, or none where it had one.
func (s *Server) triedStale(ctx context.Context, try *impactTry, c criterion, with verdict.Options, from time.Time, counting []incident.Incident) error {
	var err error
	try.StaleN, try.Stale, try.StaleMore, err = s.staleBy(ctx, c.key(), with, func(around []ledger.Incident) []incident.Incident {
		return triedIncidents(around, from, try.Off, counting)
	})
	return err
}

// staleBy are the proposals waiting for approval, not held now, that a
// change tried would hold, as triedStale says: how many, the first
// triedProposals of them, and how many more; key is what the windows the
// ledger would hold were read with (impactHeld, as it will be), with the
// options in effect with the change, and withAround, the incidents that
// would keep the record, of those around now.
func (s *Server) staleBy(ctx context.Context, key string, with verdict.Options, withAround func([]ledger.Incident) []incident.Incident) (int, []triedProposal, int, error) {
	ps, err := s.l.Proposals(ctx, ledger.ProposalQuery{States: []string{ledger.StateProposed}})
	if err != nil || len(ps) == 0 {
		return 0, nil, 0, err
	}
	ls, err := s.linesNow(ctx, ps)
	if err != nil {
		return 0, nil, 0, err
	}
	type held struct {
		line proposalLine
		ev   evidence
	}
	var hs []held
	lo, hi := time.Time{}, time.Time{}
	for _, ln := range ls {
		ev := proposalEvidence(ln.Proposal)
		bt := ev.Backtest
		if ln.StaleWhy != nil || ln.Stale || !backtested(ln.Kind) || bt == nil || !bt.From.Before(bt.To) {
			continue
		}
		hs = append(hs, held{ln, ev})
		if lo.IsZero() || bt.From.Before(lo) {
			lo = bt.From
		}
		hi = later(hi, bt.To)
	}
	if len(hs) == 0 {
		return 0, nil, 0, nil
	}
	wf, wt := s.l.UnitsOf(with, lo, hi)
	around, err := s.l.RecordIncidents(ctx, verdict.RecordFrom(wf, wt), wt, with.UnknownLen)
	if err != nil {
		return 0, nil, 0, err
	}
	rec := verdict.RecordOf(withAround(around), with)
	slices.SortFunc(hs, func(a, b held) int { return int(a.line.ID - b.line.ID) })
	n, more := 0, 0
	var out []triedProposal
	for _, h := range hs {
		bt := h.ev.Backtest
		kept := slices.ContainsFunc(s.l.SpanUnits(with, bt.From, bt.To), func(u int64) bool { return s.l.UnitKept(rec, with, u) })
		read := h.ev.Impact != "" && h.ev.Impact != key
		if read || bt.NoRecord != nil && *bt.NoRecord == kept {
			n++
			if len(out) < triedProposals {
				out = append(out, triedProposal{h.line.ID, h.line.Rule})
			} else {
				more++
			}
		}
	}
	return n, out, more, nil
}

// shareLabels reports whether, for every label both have, they have a
// value in common: what was decided about a window holds for one read
// again over it, as the ledger has it.
func shareLabels(a, b incident.Labels) bool {
	for k, va := range a {
		if vb, ok := b[k]; ok && !slices.ContainsFunc(va, func(v string) bool { return slices.Contains(vb, v) }) {
			return false
		}
	}
	return true
}

// unitLabel is the day's label when o judges the record by day, the week's
// when by week.
func unitLabel(o verdict.Options, day, week string) string {
	if o.RecordDay {
		return day
	}
	return week
}
