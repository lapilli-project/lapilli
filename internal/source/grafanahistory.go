package source

import (
	"context"
	"crypto/sha256"
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
	"unicode/utf8"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/prom"
)

// What a Grafana's alert rules really fired, read from its own alert state
// history (docs/design/grafanahistory.md): what it keeps by default,
// annotations, through its annotations API; or, kept in Loki, through its
// state history API. Both only read, with a Viewer's token.

// Where a Grafana keeps its alert state history and serves it from.
const (
	HistoryAnnotations = "annotations"
	HistoryLoki        = "loki"
)

// What a read of a Grafana's state history takes at most. It is what
// another system sends, and lapilli serve reads it again after a restart:
// none of it may grow past what is set here. An answer is historyBody at
// most, a page read again halved when it is more. A read asks
// historyRequests requests, reads historyBytes of answers, keeps
// MaxChanges changes of historyAlerts alerts, their labels historyLabels
// bytes all told, and tells historySeen changes apart, at most: past any,
// it fails as too many (ErrTooManyChanges). A change whose alert has more
// than maxHistoryLabels labels, a label's name past maxHistoryName bytes
// or its value past maxHistoryValue, a rule's name past maxHistoryRule or
// none, a state past maxHistoryState or Grafana has no name for, or a time
// outside what was asked, is passed over, and counted: so is every other
// change of its alert, and its rule is unsure (HistoryRead).
const (
	minHistoryPage   = 10
	maxHistoryLabels = 128
	maxHistoryName   = 256
	maxHistoryValue  = 4096
	maxHistoryRule   = 1024
	maxHistoryState  = 200
	maxSettingsBody  = 8 << 20
	// historySpan is how long a span one request asks over is at most:
	// Grafana's Loki is asked 721h at most by default.
	historySpan = 7 * 24 * time.Hour
	// historyReach is how far back Reaches looks in Loki, within that.
	historyReach = 30 * 24 * time.Hour
	// maxUnsure is how many rules a read says are unsure, by name.
	maxUnsure = 1000
)

// The bounds of a read; variables for tests.
var (
	MaxChanges      = 200000
	historyAlerts   = 50000
	historyLabels   = int64(64 << 20)
	historySeen     = 400000
	historyBody     = int64(16 << 20)
	historyBytes    = int64(256 << 20)
	historyRequests = 5000
	historyPage     = 1000
	// historyGrow is the most changes a page asks for, grown from
	// historyPage while a page holds changes of one time alone: in Loki,
	// 5,000, its max_entries_limit_per_query by default.
	historyGrow = map[string]int{HistoryAnnotations: 16000, HistoryLoki: 5000}
)

// ErrUnusable is in the error of a read of a Grafana's state history whose
// answers can't be used, and won't be on asking again: refused (HTTP 4xx
// but 401 and 429), more than is read, not what was asked for, or
// annotations written before Grafana 12.4, which hold no labels. What
// isn't is worth asking again: no answer, HTTP 429 or 5xx once retried,
// or 401, a token expired or not yet rotated in, which a later ask may
// have right: a day not checked for it would be lost for good.
var ErrUnusable = errors.New("Grafana's alert state history can't be used")

// ErrTooManyChanges is in the error of a read past its bounds; it is
// ErrUnusable too.
var ErrTooManyChanges = fmt.Errorf("%w: more than is read at once", ErrUnusable)

// unusable is err, an answer's, as ErrUnusable when asking again won't
// change it.
func unusable(what string, err error) error {
	var ae *prom.APIError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrUnusable):
		return err
	case errors.Is(err, prom.ErrTooLarge),
		errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 && ae.Status != http.StatusUnauthorized && ae.Status != http.StatusTooManyRequests:
		return fmt.Errorf("%w: reading %s: %v", ErrUnusable, what, err)
	}
	return fmt.Errorf("reading %s: %w", what, err)
}

// GrafanaHistory reads a Grafana's alert state history: at URL, with the
// token in TokenFile, read again for each request, and redirects not
// followed, so it goes nowhere else. Limit is the Limiter its requests are
// asked by, each try of each: --max-rate's, shared with the Grafana's data
// sources when it has some.
type GrafanaHistory struct {
	URL, TokenFile, Agent string
	Limit                 *prom.Limiter
}

func (g GrafanaHistory) client() *prom.Client {
	opts := []prom.Option{prom.WithHTTPClient(noRedirects)}
	if g.Limit != nil {
		opts = append(opts, prom.WithLimiter(g.Limit))
	}
	if g.TokenFile != "" {
		opts = append(opts, prom.WithToken(tokenFrom(g.TokenFile)))
	}
	return prom.New(strings.TrimRight(g.URL, "/"), g.Agent, opts...)
}

// HistoryWhere is where a Grafana keeps its alert state history, as its
// frontend settings say.
type HistoryWhere struct {
	// Serves is the history it serves, as Lapilli reads it:
	// HistoryAnnotations, from Grafana 12.4 on, which writes an alert's
	// labels in its annotations' tags; HistoryLoki; or "".
	Serves string
	// Series says it keeps it in Prometheus as GRAFANA_ALERTS too, or may:
	// kept there alone, or in several places (multiple), which the
	// settings don't name. While GRAFANA_ALERTS holds anything, it is read.
	Series bool
}

// Where asks the Grafana where it keeps its alert state history, as its
// frontend settings say, which anyone signed in reads.
func (g GrafanaHistory) Where(ctx context.Context) (HistoryWhere, error) {
	body, err := g.client().Get(ctx, "/api/frontend/settings", maxSettingsBody)
	if err != nil {
		return HistoryWhere{}, unusable("where Grafana keeps its alert state history", err)
	}
	var s struct {
		BuildInfo struct {
			Version string `json:"version"`
		} `json:"buildInfo"`
		UnifiedAlerting struct {
			Backend string `json:"alertStateHistoryBackend"`
			Primary string `json:"alertStateHistoryPrimary"`
		} `json:"unifiedAlerting"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return HistoryWhere{}, fmt.Errorf("%w: reading where Grafana keeps its alert state history: %v", ErrUnusable, err)
	}
	var w HistoryWhere
	serves := s.UnifiedAlerting.Backend
	switch serves {
	case "multiple":
		serves, w.Series = s.UnifiedAlerting.Primary, true
	case "prometheus":
		w.Series = true
	}
	if serves == HistoryLoki || serves == HistoryAnnotations && tagged(s.BuildInfo.Version) {
		w.Serves = serves
	}
	return w, nil
}

// tagged reports whether a Grafana of version writes an alert's labels in
// its annotations' tags: from 12.4 on. One that doesn't say is taken to,
// and its annotations are checked as they are read.
func tagged(version string) bool {
	major, rest, _ := strings.Cut(strings.TrimPrefix(version, "v"), ".")
	minor, _, _ := strings.Cut(rest, ".")
	x, err1 := strconv.Atoi(major)
	y, err2 := strconv.Atoi(strings.TrimRightFunc(minor, func(r rune) bool { return r < '0' || r > '9' }))
	if err1 != nil || err2 != nil {
		return true
	}
	return x > 12 || x == 12 && y >= 4
}

// HistoryRead is what a read of the history couldn't take: how many
// changes it passed over, as the bounds above say; and the rules some of
// whose changes it passed over, maxUnsure at most, UnsureMore past them,
// as what is left of them would say they fired when they didn't: by name
// (Unsure); or, read to tell rules apart (Judge), a change that says its
// rule by that rule, beside the name it gives it (unsureOf), and Unsure
// the names of those that say none.
type HistoryRead struct {
	Passed     int
	Unsure     []string
	UnsureMore bool
	unsureOf   []passedOf
}

// passedOf is a rule some change of which was passed over: the name the
// change gives it, and, read to tell rules apart, the rule the change says
// it is of, zero where it says none.
type passedOf struct {
	name string
	of   firing.RuleOf
}

// Read reads the changes of state of every alert over [from, to] from
// where the Grafana serves them (HistoryWhere.Serves), oldest first. A
// read past its bounds fails, as ErrTooManyChanges; one whose answers
// can't be used, as ErrUnusable.
func (g GrafanaHistory) Read(ctx context.Context, where string, from, to time.Time) ([]firing.Transition, HistoryRead, error) {
	return g.read(ctx, where, from, to, false)
}

// read is Read; with apart, the alerts of two rules of one alert name and
// the same labels are two, told apart by what the history says of each
// change's rule (firing.RuleOf), which each change then carries.
func (g GrafanaHistory) read(ctx context.Context, where string, from, to time.Time, apart bool) ([]firing.Transition, HistoryRead, error) {
	var read func(context.Context, *prom.Client, time.Time, time.Time, *historyPages) error
	switch where {
	case HistoryAnnotations:
		read = readAnnotations
	case HistoryLoki:
		read = readLokiHistory
	default:
		return nil, HistoryRead{}, fmt.Errorf("%w: it keeps none that can be read (%q)", ErrUnusable, where)
	}
	c := g.client()
	p := newHistoryPages(where, from, to)
	p.apart = apart
	for a := from; a.Before(to) || a.Equal(from); a = a.Add(historySpan) {
		b := a.Add(historySpan)
		if b.After(to) {
			b = to
		}
		if err := read(ctx, c, a, b, p); err != nil {
			return nil, HistoryRead{}, err
		}
		if !b.Before(to) {
			break
		}
	}
	return p.result()
}

// Reaches reports whether the Grafana's history holds a change at or
// before at, as one it keeps from before then does: a history that begins
// after it, kept so far back no more, or not yet, says nothing of what
// fired then. In Loki, it looks back historyReach.
func (g GrafanaHistory) Reaches(ctx context.Context, where string, at time.Time) (bool, error) {
	var path, what string
	switch where {
	case HistoryAnnotations:
		// From the epoch's first millisecond: Grafana filters by time only
		// with both ends given.
		path, what = "/api/annotations?"+url.Values{"type": {"alert"}, "from": {"1"}, "to": {strconv.FormatInt(at.UnixMilli(), 10)}, "limit": {"1"}}.Encode(),
			"Grafana's alert annotations"
	case HistoryLoki:
		path, what = "/api/v1/rules/history?"+url.Values{"from": {strconv.FormatInt(at.Add(-historyReach).Unix(), 10)},
			"to": {strconv.FormatInt(at.Unix()+1, 10)}, "limit": {"1"}}.Encode(), "Grafana's alert state history"
	default:
		return false, fmt.Errorf("%w: it keeps none that can be read (%q)", ErrUnusable, where)
	}
	body, err := g.client().Get(ctx, path, historyBody)
	if err != nil {
		return false, unusable(what, err)
	}
	if where == HistoryAnnotations {
		var items []json.RawMessage
		if err := json.Unmarshal(body, &items); err != nil {
			return false, fmt.Errorf("%w: reading %s: %v", ErrUnusable, what, err)
		}
		return len(items) > 0, nil
	}
	times, _, err := lokiFrame(body)
	if err != nil {
		return false, fmt.Errorf("%w: reading %s: %v", ErrUnusable, what, err)
	}
	return len(times) > 0, nil
}

// historyPages is a read under way: the period; each alert kept, once, by
// a hash of its rule and labels, and each change kept of it, its time and
// states alone; the changes seen, by id or by a hash, as a page asked to a
// time a change is at asks for it again; the alerts with a change passed
// over, and the rules of them; and how much was asked and read.
type historyPages struct {
	where      string
	from, to   time.Time
	alerts     map[[32]byte]*keptAlert
	changes    []keptChange
	states     map[string]string
	seenIDs    map[int64]struct{}
	seenKeys   map[[16]byte]struct{}
	bad        map[[32]byte]bool
	unsure     map[passedOf]bool
	unsureMore bool
	passed     int
	// apart says an alert is told apart by its rule too, as the history
	// says it (firing.RuleOf).
	apart bool
	// labels is what the labels of the alerts kept take, near enough;
	// read the answers' bytes, and asked the requests.
	labels int64
	read   int64
	asked  int
}

type keptAlert struct {
	rule   string
	labels map[string]string
	key    string
	of     firing.RuleOf
}

type keptChange struct {
	alert    *keptAlert
	ms       int64
	from, to string
}

func newHistoryPages(where string, from, to time.Time) *historyPages {
	return &historyPages{where: where, from: from, to: to, alerts: map[[32]byte]*keptAlert{}, states: map[string]string{},
		seenIDs: map[int64]struct{}{}, seenKeys: map[[16]byte]struct{}{}, bad: map[[32]byte]bool{}, unsure: map[passedOf]bool{}}
}

// tooMuch is the error of a read past its bounds.
func (p *historyPages) tooMuch(what string) error {
	return fmt.Errorf("Grafana's state history from %s to %s: %w (%s)", p.from.UTC().Format(time.RFC3339), p.to.UTC().Format(time.RFC3339),
		ErrTooManyChanges, what)
}

// seen reports whether a change was read before, by its id or its key,
// and notes it.
func (p *historyPages) seen(id int64, key [16]byte, byID bool) (bool, error) {
	if byID {
		if _, ok := p.seenIDs[id]; ok {
			return true, nil
		}
		p.seenIDs[id] = struct{}{}
	} else {
		if _, ok := p.seenKeys[key]; ok {
			return true, nil
		}
		p.seenKeys[key] = struct{}{}
	}
	if len(p.seenIDs)+len(p.seenKeys) > historySeen {
		return false, p.tooMuch(fmt.Sprintf("more than %d changes told apart", historySeen))
	}
	return false, nil
}

// markUnsure notes a rule some change of which was passed over, by the
// name the change gives it; read to tell rules apart, by the rule it says
// it is of too (of), where it says one: by its name alone, a rule of the
// name in a folder Lapilli doesn't read would leave the judged rule out. A
// name past maxHistoryRule, or none, is no rule's; a uid past
// maxHistoryName says none.
func (p *historyPages) markUnsure(rule string, of firing.RuleOf) {
	if !p.apart || len(of.UID) > maxHistoryName {
		of = firing.RuleOf{}
	}
	x := passedOf{rule, of}
	switch {
	case rule == "" || len(rule) > maxHistoryRule || p.unsure[x]:
	case len(p.unsure) >= maxUnsure:
		p.unsureMore = true
	default:
		p.unsure[x] = true
	}
}

// take keeps one change not seen before: at, of the alert rule and labels
// (alertname among them or not), from and to states, and what the history
// says of its rule besides (of). One past the bounds is counted, its alert
// marked, and its rule unsure.
func (p *historyPages) take(at time.Time, rule string, labels map[string]string, from, to string, of firing.RuleOf) error {
	sum, ok := historyAlert(rule, labels)
	if ok && p.apart {
		// The rule's uid or id with its alert.
		if len(of.UID) > maxHistoryName {
			ok = false
		} else {
			sum = sha256.Sum256(fmt.Appendf(sum[:], "\x00%d:%s\x00%d", len(of.UID), of.UID, of.ID))
		}
	}
	switch {
	case !ok:
		p.passed++
		p.markUnsure(rule, of)
		return nil
	case len(from) > maxHistoryState || len(to) > maxHistoryState || !firing.GrafanaState(from) || !firing.GrafanaState(to) ||
		at.Before(p.from.Add(-time.Minute)) || at.After(p.to.Add(time.Minute)):
		p.bad[sum] = true
		p.passed++
		p.markUnsure(rule, of)
		return nil
	}
	a := p.alerts[sum]
	if a == nil {
		cost := int64(len(rule)) + 128
		for k, v := range labels {
			cost += int64(len(k)+len(v)) + 64
		}
		if p.apart {
			cost += int64(len(of.UID))
		}
		if len(p.alerts) >= historyAlerts || p.labels+cost > historyLabels {
			return p.tooMuch(fmt.Sprintf("more than %d alerts, or %d MiB of their labels", historyAlerts, historyLabels>>20))
		}
		l := make(map[string]string, len(labels))
		for k, v := range labels {
			if k != "alertname" {
				l[k] = v
			}
		}
		a = &keptAlert{rule: rule, labels: l, key: string(sum[:16])}
		if p.apart {
			a.of = of
		}
		p.alerts[sum] = a
		p.labels += cost
	}
	if len(p.changes) >= MaxChanges {
		return p.tooMuch(fmt.Sprintf("more than %d changes", MaxChanges))
	}
	p.changes = append(p.changes, keptChange{a, at.UnixMilli(), p.state(from), p.state(to)})
	return nil
}

// state is a state, kept once however many changes have it.
func (p *historyPages) state(s string) string {
	if k, ok := p.states[s]; ok {
		return k
	}
	p.states[s] = s
	return s
}

// result is the changes kept, oldest first: of each alert one of whose
// changes was passed over, none, as what was left of it would say it
// fired when it didn't.
func (p *historyPages) result() ([]firing.Transition, HistoryRead, error) {
	bad := map[*keptAlert]bool{}
	for sum := range p.bad {
		if a := p.alerts[sum]; a != nil {
			bad[a] = true
		}
	}
	out := make([]firing.Transition, 0, len(p.changes))
	for _, c := range p.changes {
		if bad[c.alert] {
			p.passed++
			continue
		}
		out = append(out, firing.Transition{Rule: c.alert.rule, Labels: c.alert.labels, Key: c.alert.key, At: time.UnixMilli(c.ms).UTC(), From: c.from, To: c.to, Of: c.alert.of})
	}
	slices.SortStableFunc(out, func(a, b firing.Transition) int { return a.At.Compare(b.At) })
	read := HistoryRead{Passed: p.passed, UnsureMore: p.unsureMore}
	names := map[string]bool{}
	for x := range p.unsure {
		if x.of == (firing.RuleOf{}) {
			names[x.name] = true
		} else {
			read.unsureOf = append(read.unsureOf, x)
		}
	}
	read.Unsure = slices.Sorted(maps.Keys(names))
	return out, read, nil
}

// historyAlert is the hash of an alert, its rule and labels; ok is false
// past the bounds, or with no rule.
func historyAlert(rule string, labels map[string]string) ([32]byte, bool) {
	if rule == "" || len(rule) > maxHistoryRule || len(labels) > maxHistoryLabels {
		return [32]byte{}, false
	}
	h := sha256.New()
	h.Write([]byte(rule))
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		v := labels[k]
		if k == "" || len(k) > maxHistoryName || len(v) > maxHistoryValue {
			return [32]byte{}, false
		}
		if k == "alertname" {
			continue
		}
		fmt.Fprintf(h, "\x00%d:%s=%s", len(k), k, v)
	}
	var sum [32]byte
	h.Sum(sum[:0])
	return sum, true
}

// page asks for a page of changes, limit at most, and the page smaller,
// down to minHistoryPage, while its answer is more than historyBody. It
// gives the answer and the limit asked with.
func (p *historyPages) page(ctx context.Context, c *prom.Client, path func(limit int) string, limit int, what string) ([]byte, int, error) {
	for {
		if p.asked++; p.asked > historyRequests {
			return nil, 0, p.tooMuch(fmt.Sprintf("more than %d requests", historyRequests))
		}
		body, err := c.Get(ctx, path(limit), historyBody)
		if errors.Is(err, prom.ErrTooLarge) && limit > minHistoryPage {
			limit = max(limit/2, minHistoryPage)
			continue
		}
		if err != nil {
			return nil, 0, unusable(what, err)
		}
		// Pages that each hold one change not seen, or are asked again
		// and again, can't go on for ever.
		if p.read += int64(len(body)); p.read > historyBytes {
			return nil, 0, p.tooMuch(fmt.Sprintf("more than %d MiB of answers", historyBytes>>20))
		}
		return body, limit, nil
	}
}

// pages reads a history a page at a time, newest first: each page the
// newest changes to until, and each next to the oldest of the page before,
// keyed by the time each change is at (milliseconds), at reaches it. Those
// at that time that the page before held are read again, and told apart
// (historyPages.seen). A page that holds changes of one time alone, as
// many as were asked, is asked again for twice as many, historyGrow at
// most: no change is lost where a page ends among changes at one time.
func (p *historyPages) pages(ctx context.Context, c *prom.Client, until int64, path func(until int64, n int) string, read func([]byte) ([]int64, error),
	at func(ms int64) int64, what string) error {
	limit, most := historyPage, historyGrow[p.where]
	for {
		body, n, err := p.page(ctx, c, func(n int) string { return path(until, n) }, limit, what)
		if err != nil {
			return err
		}
		times, err := read(body)
		if err != nil {
			return err
		}
		if len(times) < n {
			return nil
		}
		next := at(slices.Min(times))
		if next >= until {
			// All of one time: asked again for more, unless the answer was
			// too long to.
			if n < limit || n >= most {
				return fmt.Errorf("%w: reading %s: more than %d changes of state at one time, more than a page", ErrUnusable, what, n)
			}
			limit = min(2*n, most)
			continue
		}
		until, limit = next, n
	}
}

// annotation is an alert annotation, as GET /api/annotations answers one:
// from Grafana 12.4 on, its labels are in its tags, NAME:VALUE each, as
// AnnotationLabels writes them. alertId is its rule's id in Grafana's
// database, and alertName that rule's title now, "" once it is deleted.
type annotation struct {
	ID        int64    `json:"id"`
	AlertID   int64    `json:"alertId"`
	AlertName string   `json:"alertName"`
	PrevState string   `json:"prevState"`
	NewState  string   `json:"newState"`
	Time      int64    `json:"time"`
	Tags      []string `json:"tags"`
}

// readAnnotations reads the alert annotations over [from, to], newest
// first, a page at a time (pages). The rule each is of is its tags'
// alertname, as AnnotationName writes it; Grafana's alertName is the
// rule's title now, which may not be what it was. One with no tags at all,
// as Grafana before 12.4 writes them, says nothing of which alert it is of:
// none of them can be used.
func readAnnotations(ctx context.Context, c *prom.Client, from, to time.Time, p *historyPages) error {
	const what = "Grafana's alert annotations"
	return p.pages(ctx, c, to.UnixMilli(), func(until int64, n int) string {
		q := url.Values{"type": {"alert"}, "from": {strconv.FormatInt(from.UnixMilli(), 10)},
			"to": {strconv.FormatInt(until, 10)}, "limit": {strconv.Itoa(n)}}
		return "/api/annotations?" + q.Encode()
	}, func(body []byte) ([]int64, error) {
		var items []annotation
		if err := json.Unmarshal(body, &items); err != nil {
			return nil, fmt.Errorf("%w: reading %s: %v", ErrUnusable, what, err)
		}
		times := make([]int64, 0, len(items))
		for _, a := range items {
			times = append(times, a.Time)
			if seen, err := p.seen(a.ID, [16]byte{}, true); seen || err != nil {
				if err != nil {
					return nil, err
				}
				continue
			}
			if len(a.Tags) == 0 {
				return nil, fmt.Errorf("%w: %s hold no labels, as Grafana before 12.4 writes them", ErrUnusable, what)
			}
			labels, ok := tagLabels(a.Tags)
			rule := labels["alertname"]
			if rule == "" && len(a.AlertName) <= maxHistoryRule {
				// A title too long for a tag: as it would be written.
				rule = AnnotationName(a.AlertName)
			}
			if !ok {
				// Its labels can't be told, nor its alert: its rule is unsure,
				// by its id where it says one.
				p.passed++
				p.markUnsure(AnnotationName(a.AlertName), firing.RuleOf{ID: a.AlertID})
				continue
			}
			if err := p.take(time.UnixMilli(a.Time), rule, labels, a.PrevState, a.NewState, firing.RuleOf{ID: a.AlertID}); err != nil {
				return nil, err
			}
		}
		return times, nil
	}, func(until int64) int64 { return until }, what)
}

// tagLabels reads an annotation's tags as labels: NAME:VALUE each, a tag
// with no colon a label with no value, as Grafana keeps one; ok is false
// for a name given twice, or past the bounds.
func tagLabels(tags []string) (map[string]string, bool) {
	if len(tags) > maxHistoryLabels {
		return nil, false
	}
	l := make(map[string]string, len(tags))
	for _, t := range tags {
		k, v, _ := strings.Cut(t, ":")
		if _, twice := l[k]; twice || k == "" || len(k) > maxHistoryName || len(v) > maxHistoryValue {
			return nil, false
		}
		l[k] = v
	}
	return l, true
}

// The bounds of what Grafana keeps of a label in an annotation's tags.
const tagMost = 100

// AnnotationName is a label's name or value as Grafana 12.4 and later
// keep it in an alert annotation's tags (convertLabelsToTags, and the tag
// store's ParseTagPairs): each colon an underscore, and spaces either end
// trimmed. One past 100 characters with its colons underscores, Grafana
// leaves out.
func AnnotationName(s string) string {
	return strings.Trim(strings.ReplaceAll(s, ":", "_"), " ")
}

// AnnotationLabels are labels as an alert annotation's tags hold them
// (AnnotationName), and those Grafana leaves out left out: a name or a
// value past 100 characters. Grafana leaves out more, those past its
// annotations' tags_length, the last by name; a check passes over a label
// either side doesn't have. Nil for none.
func AnnotationLabels(l map[string]string) map[string]string {
	if len(l) == 0 {
		return l
	}
	out := make(map[string]string, len(l))
	for k, v := range l {
		sk, sv := strings.ReplaceAll(k, ":", "_"), strings.ReplaceAll(v, ":", "_")
		if utf8.RuneCountInString(sk) > tagMost || utf8.RuneCountInString(sv) > tagMost {
			continue
		}
		if sk = strings.Trim(sk, " "); sk != "" {
			out[sk] = strings.Trim(sv, " ")
		}
	}
	return out
}

// lokiLine is one change, as Grafana writes it to Loki and its state
// history API answers it.
type lokiLine struct {
	Previous    string            `json:"previous"`
	Current     string            `json:"current"`
	RuleTitle   string            `json:"ruleTitle"`
	RuleUID     string            `json:"ruleUID"`
	Fingerprint string            `json:"fingerprint"`
	Labels      map[string]string `json:"labels"`
}

// readLokiHistory reads the changes over [from, to] from Grafana's state
// history API, kept in Loki: newest first across Loki's streams, a page at
// a time (pages), to a whole second, as the API takes its times; the next
// page to the second after the oldest change of the page before, which
// holds it, Loki's end being before it. The changes seen are told apart by
// a hash of when, which alert and which states.
func readLokiHistory(ctx context.Context, c *prom.Client, from, to time.Time, p *historyPages) error {
	const what = "Grafana's alert state history"
	until := to.Unix()
	if !to.Truncate(time.Second).Equal(to) {
		until++
	}
	return p.pages(ctx, c, until, func(until int64, n int) string {
		q := url.Values{"from": {strconv.FormatInt(from.Unix(), 10)}, "to": {strconv.FormatInt(until, 10)}, "limit": {strconv.Itoa(n)}}
		return "/api/v1/rules/history?" + q.Encode()
	}, func(body []byte) ([]int64, error) {
		times, lines, err := lokiFrame(body)
		if err != nil {
			return nil, fmt.Errorf("%w: reading %s: %v", ErrUnusable, what, err)
		}
		for i, ms := range times {
			var l lokiLine
			bad := json.Unmarshal(lines[i], &l) != nil
			// When, which alert and which states; the line whole, when it
			// can't be read or Grafana gives no fingerprint of the alert.
			of := []byte(l.RuleUID + "\x00" + l.Fingerprint + "\x00" + l.Previous + "\x00" + l.Current)
			if bad || l.Fingerprint == "" {
				of = lines[i]
			}
			if seen, err := p.seen(0, lokiKey(ms, of), false); seen || err != nil {
				if err != nil {
					return nil, err
				}
				continue
			}
			if bad {
				p.passed++
				continue
			}
			rule := l.Labels["alertname"]
			if rule == "" {
				rule = l.RuleTitle
			}
			if err := p.take(time.UnixMilli(ms), rule, l.Labels, l.Previous, l.Current, firing.RuleOf{UID: l.RuleUID}); err != nil {
				return nil, err
			}
		}
		return times, nil
	}, func(ms int64) int64 { return ms/1000 + 1 }, what)
}

// lokiKey is a change's key, kept to tell it apart from those read again:
// a hash, so that what is kept of each is small, however long what it is
// of.
func lokiKey(ms int64, of []byte) [16]byte {
	h := sha256.New()
	fmt.Fprintf(h, "%d\x00", ms)
	h.Write(of)
	var k [16]byte
	copy(k[:], h.Sum(nil))
	return k
}

// lokiFrame reads the data frame Grafana's state history API answers
// with, kept in Loki: the time of each change, in milliseconds, and its
// line.
func lokiFrame(body []byte) ([]int64, []json.RawMessage, error) {
	var f struct {
		Schema struct {
			Fields []struct {
				Name string `json:"name"`
			} `json:"fields"`
		} `json:"schema"`
		Data struct {
			Values []json.RawMessage `json:"values"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, nil, err
	}
	ti, li := -1, -1
	for i, fl := range f.Schema.Fields {
		switch fl.Name {
		case "time":
			ti = i
		case "line":
			li = i
		}
	}
	if ti < 0 || li < 0 || ti >= len(f.Data.Values) || li >= len(f.Data.Values) {
		return nil, nil, errors.New("an answer that isn't changes of state, with a time and a line each")
	}
	var times []int64
	var lines []json.RawMessage
	if err := json.Unmarshal(f.Data.Values[ti], &times); err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal(f.Data.Values[li], &lines); err != nil {
		return nil, nil, err
	}
	if len(times) != len(lines) {
		return nil, nil, errors.New("an answer with as many times as lines")
	}
	return times, lines, nil
}
