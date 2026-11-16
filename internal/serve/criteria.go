package serve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// The incident criteria (docs/design/ui.md, "Incident criteria"): besides
// the incidents written down and copied in, the windows where the impact
// signal (--impact, a PromQL expression that returns something while users
// are hurt) returned something for at least --impact-for. Set where serve
// is started, or by an admin on the Settings page, as the other judging
// settings are, and tried on the last four weeks first. The criterion the
// runs read keeps the record only from when they read it: saving another
// reads it again over the last four weeks (impactAgain), and the weeks
// before keep what the one before found, as incidents, and no more.

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

// impactOff is the impact setting that turns off an expression the command
// line gives: empty goes back to the command line's, as the other settings
// do. An expression that is the word off is taken for it.
const impactOff = "off"

// The settings the incident criteria are, by name, with the flag each
// stands for.
var criteriaSettable = []struct{ key, flag string }{{"impact", "--impact"}, {"impact-for", "--impact-for"}}

// The bounds of what the UI takes: an expression of 4 KB at most, a for
// from the step to a day.
const (
	maxImpactExpr = 4096
	maxImpactFor  = 24 * time.Hour
)

// impactAgainWeeks is how far back saving a new criterion reads it again,
// in whole weeks before the cursor's, and a try reads it: what the UI
// calls the last four weeks.
const impactAgainWeeks = 4

// impactTriesAnHour are the tries of the incident criteria an admin may
// ask for in an hour, made or refused once checked: each reads Prometheus
// over five weeks. Saving one that is new checks it once with Prometheus,
// and counts as one too.
const impactTriesAnHour = 12

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
	if s.h == nil {
		c.Expr = "" // read from Prometheus, which there is none of
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

// impactFrom is from when the record is kept by the impact signal, for
// verdict.Options.ImpactFrom: from when the runs read it, all of it, with
// the criterion in effect. Read with another (the UI changed it since, and
// it isn't read again yet), nothing: the weeks before weren't read with
// this one. A ledger a lapilli before 0.47 kept keeps no criterion: its is
// taken to be this.
func (s *Server) impactFrom(ctx context.Context, c criterion) (time.Time, error) {
	if c.Expr == "" {
		return time.Time{}, nil
	}
	k, err := s.l.ImpactKept(ctx)
	if err != nil || k.Criterion != "" && k.Criterion != c.key() {
		return time.Time{}, err
	}
	return k.Since, nil
}

// checkImpact is what the UI refuses of an impact expression before
// anything is read for it: too long, characters that don't show, or what
// doesn't read as PromQL as Lapilli reads it (rules.Scope: strings,
// selectors, ranges and brackets closed). Prometheus's own parser has the
// last word, when it is tried or saved (checkWithPrometheus).
func checkImpact(expr string) error {
	switch {
	case len(expr) > maxImpactExpr:
		return bad("an impact expression is %d bytes at most", maxImpactExpr)
	case !utf8.ValidString(expr) || strings.IndexFunc(expr, func(r rune) bool {
		return r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r))
	}) >= 0:
		return bad("an impact expression has characters that don't show")
	}
	if _, err := rules.Scope(expr, nil); err != nil {
		return bad("the impact expression doesn't read as PromQL: %v", err)
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

// errImpactTries is a try, or a save, of the incident criteria past
// impactTriesAnHour.
var errImpactTries = bad("you have tried or saved as many incident criteria in the last hour as anyone may: each reads Prometheus; try again later")

// errCrossSite is a try of the incident criteria asked for from another
// site: it reads Prometheus, so it is asked from the Settings page alone.
var errCrossSite = bad("a try of the incident criteria reads Prometheus, so it is asked for from the Settings page, on this site")

// crossSite reports whether a browser says a request came from another
// site (Sec-Fetch-Site): a link elsewhere, or a page's image.
func crossSite(r *http.Request) bool {
	v := r.Header.Get("Sec-Fetch-Site")
	return v == "cross-site" || v == "same-site"
}

// checkWithPrometheus asks Prometheus to evaluate an expression once, now:
// its parser, and whether Lapilli may read it. What it says is refused.
func (s *Server) checkWithPrometheus(ctx context.Context, expr string) error {
	ctx, cancel := context.WithTimeout(ctx, min(s.cfg.Timeout, 30*time.Second))
	defer cancel()
	at := s.now().Truncate(s.cfg.Step)
	if _, err := s.h.QueryRange(ctx, expr, at, at, s.cfg.Step); err != nil {
		return bad("Prometheus didn't take the impact expression, so nothing was saved: %v", promText(err))
	}
	return nil
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

// criteriaView is the incident criteria, as the Settings page shows them:
// each setting, the form's values (as typed when tried, else in effect),
// whether there is Prometheus to read the signal from, from when the
// record is kept by it, how its read again over the last four weeks went,
// and a try's result.
type criteriaView struct {
	Settings  []settingView `json:"settings"`
	Expr, For string        `json:"-"`
	NoHistory bool          `json:"no_history,omitempty"`
	Since     time.Time     `json:"kept_since,omitzero"`
	// Again, when the criterion in effect waits to be read again over the
	// last four weeks: when it last failed, why, and when it is tried again.
	Again *againView `json:"read_again,omitempty"`
	Tried *impactTry `json:"-"`
}

type againView struct {
	Failed time.Time `json:"failed,omitzero"`
	Err    string    `json:"error,omitempty"`
	Next   time.Time `json:"next,omitzero"`
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
	var err error
	if v.Since, err = s.impactFrom(ctx, c); err != nil {
		return nil, err
	}
	if c.Expr != "" {
		k, err := s.l.ImpactKept(ctx)
		if err != nil {
			return nil, err
		}
		if !k.AgainDone(c.key()) {
			v.Again = &againView{}
			if a := s.shownSources().Again; a != nil && a.Key == c.key() {
				v.Again.Failed, v.Again.Err, v.Again.Next = a.Failed, a.Err, a.Next
			}
		}
	}
	return v, nil
}

// postImpact saves the incident criteria, an admin's: an expression that
// reads, or empty (or off) for none; a for within bounds. One new is
// checked with Prometheus first, and is then read again over the last
// four weeks by the replica that leads (impactAgain), which judges them
// again.
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
		if err := checkImpact(expr); err != nil {
			s.oops(w, r, err)
			return
		}
		values["impact"] = expr
		if expr == s.cfg.Impact {
			values["impact"] = ""
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
	if next.Expr != "" && next.Expr != before.Expr {
		if !s.impactTries.take(s.user(r), false, time.Now()) {
			s.oops(w, r, errImpactTries)
			return
		}
		if err := s.checkWithPrometheus(ctx, next.Expr); err != nil {
			s.oops(w, r, err)
			return
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

// impactAgain reads the criterion in effect again over the last four
// weeks, when the runs haven't read it from before then: one saved in the
// UI, or a new --impact the process was started with. The windows it finds
// take the place of those found from then on, and the stones there are
// given again (ledger.ReadAgain); the record is kept by it from then on.
// A ledger whose runs read it from their first, or that a lapilli before
// 0.47 kept, needs none. One that fails is tried again later, a while
// after, and the runs read the criterion meanwhile from where they are. It
// reports whether it read it again.
func (s *Server) impactAgain(ctx context.Context, o verdict.Options) (bool, error) {
	c, err := s.criterion(ctx)
	if err != nil || c.Expr == "" {
		return false, err
	}
	k, err := s.l.ImpactKept(ctx)
	if err != nil || k.AgainDone(c.key()) {
		return false, err
	}
	cursor, err := s.l.Cursor(ctx)
	if err != nil || cursor.IsZero() {
		return false, err // the runs read it from their first
	}
	if k.Again == "" && !k.Since.IsZero() && (k.Criterion == "" || k.Criterion == c.key()) {
		// Read all along: by this lapilli's runs from their first, or by
		// an earlier one's, which kept no criterion.
		return false, s.l.MarkAgain(ctx, c.key())
	}
	now := s.now()
	s.sources.Lock()
	a := s.sources.again
	s.sources.Unlock()
	if a.Key == c.key() && now.Before(a.Next) {
		return false, nil
	}
	from, err := s.againFrom(ctx, cursor)
	if err != nil {
		return false, err
	}
	s.status.set("reading the impact signal again over the last four weeks")
	rctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	series, err := s.h.QueryRange(rctx, c.Expr, from, cursor, s.cfg.Step)
	if err == nil {
		err = s.l.ReadAgain(ctx, ledger.ImpactRead{From: from, To: cursor, Criterion: c.key(), Windows: incident.FromImpact(series, s.cfg.Step, c.For)}, o)
		if errors.Is(err, ledger.ErrRunsMoved) {
			return false, nil // read again at the next round, to where they are
		}
	}
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return false, err
	}
	s.readImpact(err)
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

// sourcesSeen is what the replica that leads saw of where incidents come
// from: when the impact signal was last read, and the incidents file, and
// when either last failed, and why; and how reading a criterion again
// went (againState).
type sourcesSeen struct {
	ImpactRead   time.Time   `json:"impact_read,omitzero"`
	ImpactFailed time.Time   `json:"impact_failed,omitzero"`
	ImpactErr    string      `json:"impact_error,omitempty"`
	FileRead     time.Time   `json:"file_read,omitzero"`
	FileFailed   time.Time   `json:"file_failed,omitzero"`
	FileErr      string      `json:"file_error,omitempty"`
	Again        *againState `json:"again,omitempty"`
}

// sourcesState is sourcesSeen as this replica keeps it, and when it last
// noted each source writing through the incidents port (noteReceived).
type sourcesState struct {
	sync.Mutex
	impactRead, impactFailed time.Time
	impactErr                string
	fileRead, fileFailed     time.Time
	fileErr                  string
	again                    againState
	noted                    map[string]time.Time
}

// sourcesNow is this replica's sourcesSeen.
func (s *Server) sourcesNow() *sourcesSeen {
	s.sources.Lock()
	defer s.sources.Unlock()
	x := &sourcesSeen{ImpactRead: s.sources.impactRead, ImpactFailed: s.sources.impactFailed, ImpactErr: s.sources.impactErr,
		FileRead: s.sources.fileRead, FileFailed: s.sources.fileFailed, FileErr: s.sources.fileErr}
	if s.sources.again.Key != "" {
		a := s.sources.again
		x.Again = &a
	}
	return x
}

// shownSources is what the pages say was seen of the sources: the
// leader's, as it last said it, on a replica that doesn't lead.
func (s *Server) shownSources() sourcesSeen {
	if s.l.Shared() && !s.l.Leading() {
		s.leaderSaid.Lock()
		defer s.leaderSaid.Unlock()
		if x := s.leaderSaid.st.Sources; x != nil {
			return *x
		}
		return sourcesSeen{}
	}
	return *s.sourcesNow()
}

// readImpact notes how a run's read of the impact signal went.
func (s *Server) readImpact(err error) {
	s.sources.Lock()
	defer s.sources.Unlock()
	if err != nil {
		s.sources.impactFailed, s.sources.impactErr = s.now(), promText(err)
		return
	}
	s.sources.impactRead, s.sources.impactErr = s.now(), ""
}

// readFile notes how a read of the incidents file went.
func (s *Server) readFile(err error) {
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
// count), the verdict in effect and with it (Rows), and the proposals
// waiting for approval it would hold until they are backtested again.
type impactTry struct {
	From, To    time.Time
	Expr, For   string
	Off         bool
	Err         string
	Windows     []triedWindow
	Found       int
	Counted     int
	MoreWindows int
	Rows        []triedRow
	Stale       []triedProposal
	StaleMore   int
	StaleN      int
}

// triedWindow is a window a try found: when, how long, its labels, and,
// when it wouldn't count, the incidents written down over it, or what was
// decided about a window found before over it.
type triedWindow struct {
	Start, End time.Time
	Len        time.Duration
	Labels     string
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

// tryImpact tries the criterion the Settings page's form gives, an
// admin's, from this site: checked as saving it is, then read over the
// last four weeks, within --timeout and --max-samples, one heavy request
// at a time and impactTriesAnHour an admin; what it couldn't read is said
// in its result, the form kept as typed.
func (s *Server) tryImpact(r *http.Request, v *criteriaView) error {
	ctx := r.Context()
	if s.role(r) != admin {
		return s.forbidden(admin)
	}
	if crossSite(r) {
		return errCrossSite
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
		if !s.impactTries.take(s.user(r), false, time.Now()) {
			return errImpactTries
		}
	}
	to, err := s.l.Cursor(ctx)
	if err != nil {
		return err
	}
	if to.IsZero() {
		to = s.now().Truncate(s.cfg.Step)
	}
	if try.From, err = s.againFrom(ctx, to); err != nil {
		return err
	}
	try.To = to
	done, err := s.heavyTurn(ctx, try.From, to)
	if err != nil {
		return err
	}
	defer done()
	var windows []incident.Incident
	if !try.Off {
		rctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
		series, err := s.h.QueryRange(rctx, c.Expr, try.From, to, s.cfg.Step)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			try.Err = tr(l, "Prometheus couldn't read it: %s", promText(err))
			return nil
		}
		windows = incident.FromImpact(series, s.cfg.Step, c.For)
	}
	return s.judgeTried(ctx, l, try, windows)
}

// judgeTried works out, for a try, which windows would count, the verdict
// over its period in effect and with them, and the proposals it would
// hold.
func (s *Server) judgeTried(ctx context.Context, l string, try *impactTry, windows []incident.Incident) error {
	o, err := s.options(ctx)
	if err != nil {
		return err
	}
	from, to := try.From, try.To
	o.From, o.To = from, to
	with := o
	with.ImpactFrom = time.Time{}
	if !try.Off {
		with.ImpactFrom = from
	}
	// The incidents that keep the record of the period's weeks, and what
	// was decided about the windows found before over it.
	wf, _ := s.l.WeekOf(from)
	_, wt := s.l.WeekOf(to.Add(-time.Nanosecond))
	around, err := s.l.RecordIncidents(ctx, verdict.RecordFrom(wf, wt), wt, o.UnknownLen)
	if err != nil {
		return err
	}
	found, err := s.l.ImpactWindows(ctx, from, wt)
	if err != nil {
		return err
	}
	var decided []ledger.Incident
	for _, in := range found {
		if in.State != "" {
			decided = append(decided, in)
		}
	}
	var written []incident.Incident
	for _, in := range around {
		if in.Origin != "impact" {
			written = append(written, in.Incident)
		}
	}
	// Each window found: whether it would count, as the ledger has it
	// (ledger.ReadAgain, then cover).
	var counting []incident.Incident
	try.Found = len(windows)
	for _, w := range windows {
		tw := triedWindow{Start: w.Start, End: w.End, Len: w.End.Sub(w.Start), Labels: incidentLabelsText(w.Labels)}
		for _, d := range decided {
			if !d.Start.After(w.End) && !d.End.Before(w.Start) && shareLabels(d.Labels, w.Labels) {
				tw.Decided = d.State
			}
		}
		for _, x := range written {
			if len(incident.Merge([]incident.Incident{x}, []incident.Incident{w}, o.UnknownLen, o.Match)) == 1 {
				tw.Written = append(tw.Written, x.ID)
			}
		}
		if tw.Decided == "" && len(tw.Written) == 0 {
			counting = append(counting, w)
			try.Counted++
		}
		if len(try.Windows) < triedWindows {
			try.Windows = append(try.Windows, tw)
		} else {
			try.MoreWindows++
		}
	}
	nowIncs := make([]incident.Incident, 0, len(around))
	for _, in := range around {
		nowIncs = append(nowIncs, in.Incident)
	}
	withIncs := triedIncidents(around, from, try.Off, counting)

	fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: from, To: to})
	if err != nil {
		return err
	}
	plain := make([]firing.Firing, 0, len(fs))
	for _, f := range fs {
		if !f.Excused {
			plain = append(plain, f.Firing)
		}
	}
	nowRep := verdict.Judge(nil, plain, nowIncs, o)
	withRep := verdict.Judge(nil, plain, withIncs, with)
	weeks := s.l.SpanWeeks(from, to)
	a := triedSumOf(nowRep, verdict.RecordOf(nowIncs, o), weeks, s.l.Midnight, from, to)
	b := triedSumOf(withRep, verdict.RecordOf(withIncs, with), weeks, s.l.Midnight, from, to)
	row := func(label, x, y string) {
		try.Rows = append(try.Rows, triedRow{Label: label, Now: x, With: y, Changed: x != y})
	}
	of := func(n, m int) string { return tr(l, "%d of %d", n, m) }
	row("Weeks with a record", of(a.kept, len(weeks)), of(b.kept, len(weeks)))
	row("Incidents judged against", fmt.Sprint(a.incidents), fmt.Sprint(b.incidents))
	row("Of them, impact windows", fmt.Sprint(a.impact), fmt.Sprint(b.impact))
	row("Firings judged", of(a.judged, a.firings), of(b.judged, b.firings))
	row("Firings with no record", fmt.Sprint(a.firings-a.judged), fmt.Sprint(b.firings-b.judged))
	row("Precision", pct(a.white, a.judged), pct(b.white, b.judged))
	row("Noisy rules", fmt.Sprint(a.noisy), fmt.Sprint(b.noisy))
	row("Incidents missed", fmt.Sprint(a.missed), fmt.Sprint(b.missed))
	return s.triedStale(ctx, try, with, from, counting)
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

func triedSumOf(rep *verdict.Report, rec verdict.Record, weeks []int64, mid func(int64) time.Time, from, to time.Time) triedSum {
	var x triedSum
	for _, w := range weeks {
		if rec.Kept(mid(w), mid(w+7)) {
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
// a criterion tried would hold until they are backtested again: their
// backtest's period would have a record where it had none, or none where it
// had one (proposalsNow).
func (s *Server) triedStale(ctx context.Context, try *impactTry, with verdict.Options, from time.Time, counting []incident.Incident) error {
	ps, err := s.l.Proposals(ctx, ledger.ProposalQuery{States: []string{ledger.StateProposed}})
	if err != nil || len(ps) == 0 {
		return err
	}
	ls, err := s.linesNow(ctx, ps)
	if err != nil {
		return err
	}
	type held struct {
		line proposalLine
		bt   *backtest
	}
	var hs []held
	lo, hi := time.Time{}, time.Time{}
	for _, ln := range ls {
		bt := proposalEvidence(ln.Proposal).Backtest
		if ln.StaleWhy != nil || ln.Stale || !backtested(ln.Kind) || bt == nil || bt.NoRecord == nil || !bt.From.Before(bt.To) {
			continue
		}
		hs = append(hs, held{ln, bt})
		if lo.IsZero() || bt.From.Before(lo) {
			lo = bt.From
		}
		hi = later(hi, bt.To)
	}
	if len(hs) == 0 {
		return nil
	}
	wf, _ := s.l.WeekOf(lo)
	_, wt := s.l.WeekOf(hi.Add(-time.Nanosecond))
	around, err := s.l.RecordIncidents(ctx, verdict.RecordFrom(wf, wt), wt, with.UnknownLen)
	if err != nil {
		return err
	}
	rec := verdict.RecordOf(triedIncidents(around, from, try.Off, counting), with)
	slices.SortFunc(hs, func(a, b held) int { return int(a.line.ID - b.line.ID) })
	for _, h := range hs {
		kept := slices.ContainsFunc(s.l.SpanWeeks(h.bt.From, h.bt.To), func(w int64) bool { return s.l.WeekKept(rec, w) })
		if *h.bt.NoRecord == kept {
			try.StaleN++
			if len(try.Stale) < triedProposals {
				try.Stale = append(try.Stale, triedProposal{h.line.ID, h.line.Rule})
			} else {
				try.StaleMore++
			}
		}
	}
	return nil
}

// incidentLabelsText is an incident's labels as a line: name=value, sorted.
func incidentLabelsText(ls incident.Labels) string {
	var out []string
	for k, vs := range ls {
		for _, v := range vs {
			out = append(out, k+"="+v)
		}
	}
	slices.Sort(out)
	return strings.Join(out, " ")
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
