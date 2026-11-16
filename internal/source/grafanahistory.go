package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
// another system sends: an answer is 32 MiB at most, a page of changes
// read again halved when it is more; a read keeps MaxChanges at most, and
// reads 256 MiB of answers at most, and fails past either
// (ErrTooManyChanges). A change whose alert has more than
// maxHistoryLabels labels, a name past maxHistoryName bytes or a value past
// maxHistoryValue, an alert name past maxHistoryRule, a state Grafana has
// no name for, or a time outside what was asked is passed over, and
// counted: so are all the changes of that alert, which has those labels in
// each.
const (
	minHistoryPage   = 10
	maxHistoryLabels = 128
	maxHistoryName   = 256
	maxHistoryValue  = 4096
	maxHistoryRule   = 1024
	maxSettingsBody  = 8 << 20
	// historySpan is how long a span one request asks over is at most:
	// Grafana's Loki is asked 721h at most by default.
	historySpan = 7 * 24 * time.Hour
)

// MaxChanges is how many changes of state a read keeps at most;
// historyBody how long an answer is read, and historyPage how many changes
// a page asks for. Variables for tests.
var (
	MaxChanges   = 200000
	historyBody  = int64(32 << 20)
	historyPage  = 1000
	historyBytes = int64(256 << 20)
)

// ErrTooManyChanges is in the error of a read past MaxChanges.
var ErrTooManyChanges = errors.New("more changes of state than are read at once")

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

// Where asks the Grafana where it keeps its alert state history, as its
// frontend settings say, which anyone signed in reads: HistoryAnnotations
// or HistoryLoki, those it serves; "" for none it serves, kept in
// Prometheus alone (GRAFANA_ALERTS) or not kept, or a Grafana that doesn't
// say (before 10).
func (g GrafanaHistory) Where(ctx context.Context) (string, error) {
	body, err := g.client().Get(ctx, "/api/frontend/settings", maxSettingsBody)
	if err != nil {
		return "", fmt.Errorf("reading where Grafana keeps its alert state history: %w", err)
	}
	var s struct {
		UnifiedAlerting struct {
			Backend string `json:"alertStateHistoryBackend"`
			Primary string `json:"alertStateHistoryPrimary"`
		} `json:"unifiedAlerting"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return "", fmt.Errorf("reading where Grafana keeps its alert state history: %w", err)
	}
	where := s.UnifiedAlerting.Backend
	if where == "multiple" {
		where = s.UnifiedAlerting.Primary
	}
	if where == HistoryAnnotations || where == HistoryLoki {
		return where, nil
	}
	return "", nil
}

// HistoryRead is what a read of the history passed over: changes it
// couldn't take, as the bounds above say.
type HistoryRead struct {
	Passed int
}

// Read reads the changes of state of every alert over [from, to] from
// where the Grafana keeps them (Where), oldest first.
func (g GrafanaHistory) Read(ctx context.Context, where string, from, to time.Time) ([]firing.Transition, HistoryRead, error) {
	var read func(context.Context, *prom.Client, time.Time, time.Time, *historyPages) error
	switch where {
	case HistoryAnnotations:
		read = readAnnotations
	case HistoryLoki:
		read = readLokiHistory
	default:
		return nil, HistoryRead{}, fmt.Errorf("Grafana keeps no alert state history that can be read (%q)", where)
	}
	c := g.client()
	p := &historyPages{from: from, to: to, seen: map[string]bool{}, bad: map[string]bool{}}
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
	// Each change of an alert one of whose changes was passed over goes
	// too: what is left of it would say it fired when it didn't.
	out := make([]firing.Transition, 0, len(p.kept))
	for _, k := range p.kept {
		if p.bad[k.alert] {
			p.passed++
			continue
		}
		out = append(out, k.t)
	}
	slices.SortStableFunc(out, func(a, b firing.Transition) int { return a.At.Compare(b.At) })
	return out, HistoryRead{Passed: p.passed}, nil
}

// historyPages is a read under way: the period, the changes kept and the
// keys of those seen, as a page asked to a time a change is at asks for it
// again; alerts with a change passed over, by their key, and how many
// changes were.
type historyPages struct {
	from, to time.Time
	kept     []keptChange
	seen     map[string]bool
	bad      map[string]bool
	passed   int
	// read is how much of the answers was read, all told.
	read int64
}

// tooMuch is the error of a read past MaxChanges or historyBytes.
func (p *historyPages) tooMuch() error {
	return fmt.Errorf("Grafana's state history: %w (%d, or %d MiB of answers) from %s to %s", ErrTooManyChanges, MaxChanges, historyBytes>>20,
		p.from.UTC().Format(time.RFC3339), p.to.UTC().Format(time.RFC3339))
}

type keptChange struct {
	alert string
	t     firing.Transition
}

// take keeps one change, with key, unique to it, unless seen. A change
// past the bounds is counted, and its alert marked.
func (p *historyPages) take(key string, at time.Time, rule string, labels map[string]string, from, to string) error {
	if p.seen[key] {
		return nil
	}
	p.seen[key] = true
	alert, ok := historyAlert(rule, labels)
	switch {
	case !ok:
		p.passed++
		return nil
	case !firing.GrafanaState(from) || !firing.GrafanaState(to) || len(from) > 200 || len(to) > 200 ||
		at.Before(p.from.Add(-time.Minute)) || at.After(p.to.Add(time.Minute)):
		p.bad[alert] = true
		p.passed++
		return nil
	}
	if len(p.kept) >= MaxChanges {
		return p.tooMuch()
	}
	l := maps.Clone(labels)
	delete(l, "alertname")
	p.kept = append(p.kept, keptChange{alert, firing.Transition{Rule: rule, Labels: l, At: at.UTC(), From: from, To: to}})
	return nil
}

// historyAlert is the key of an alert, its rule and its labels; ok is false
// past the bounds, or with no rule.
func historyAlert(rule string, labels map[string]string) (string, bool) {
	if rule == "" || len(rule) > maxHistoryRule || !utf8.ValidString(rule) || len(labels) > maxHistoryLabels {
		return "", false
	}
	var b strings.Builder
	b.WriteString(rule)
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		v := labels[k]
		if k == "" || len(k) > maxHistoryName || len(v) > maxHistoryValue || !utf8.ValidString(k) || !utf8.ValidString(v) {
			return "", false
		}
		b.WriteString("\x00" + k + "=" + v)
	}
	return b.String(), true
}

// page asks for a page of changes, limit at most, and the page smaller,
// down to minHistoryPage, while its answer is more than historyBody. It
// gives the answer and the limit asked with.
func page(ctx context.Context, c *prom.Client, path func(limit int) string, limit int) ([]byte, int, error) {
	for {
		body, err := c.Get(ctx, path(limit), historyBody)
		if errors.Is(err, prom.ErrTooLarge) && limit > minHistoryPage {
			limit = max(limit/2, minHistoryPage)
			continue
		}
		return body, limit, err
	}
}

// annotation is an alert annotation, as GET /api/annotations answers one:
// its labels are in its tags, NAME:VALUE each.
type annotation struct {
	ID        int64    `json:"id"`
	AlertName string   `json:"alertName"`
	PrevState string   `json:"prevState"`
	NewState  string   `json:"newState"`
	Time      int64    `json:"time"`
	Tags      []string `json:"tags"`
}

// readAnnotations reads the alert annotations over [from, to], newest
// first, a page at a time (pages).
func readAnnotations(ctx context.Context, c *prom.Client, from, to time.Time, p *historyPages) error {
	return pages(ctx, c, p, to.UnixMilli(), func(until int64, n int) string {
		q := url.Values{"type": {"alert"}, "from": {strconv.FormatInt(from.UnixMilli(), 10)},
			"to": {strconv.FormatInt(until, 10)}, "limit": {strconv.Itoa(n)}}
		return "/api/annotations?" + q.Encode()
	}, func(body []byte) ([]int64, error) {
		var items []annotation
		if err := json.Unmarshal(body, &items); err != nil {
			return nil, fmt.Errorf("reading Grafana's alert annotations: %w", err)
		}
		times := make([]int64, 0, len(items))
		for _, a := range items {
			times = append(times, a.Time)
			labels, ok := tagLabels(a.Tags)
			rule := labels["alertname"]
			if rule == "" {
				rule = a.AlertName
			}
			if !ok {
				labels, rule = nil, "" // passed over, as its labels can't be told
			}
			if err := p.take("a"+strconv.FormatInt(a.ID, 10), time.UnixMilli(a.Time), rule, labels, a.PrevState, a.NewState); err != nil {
				return nil, err
			}
		}
		return times, nil
	}, func(until int64) int64 { return until }, "Grafana's alert annotations")
}

// maxHistoryGrow is the most changes a page asks for, grown from
// historyPage while a page holds changes of one time alone; a variable for
// tests.
var maxHistoryGrow = 16000

// pages reads a history a page at a time, newest first: each page the
// newest changes to until, and each next to the oldest of the page before,
// keyed by the time each change is at (milliseconds), at reaches it. Those
// at that time that the page before held are read again, and told apart
// by take (in read). A page that holds changes of one time alone, as many
// as were asked, is asked again for twice as many, maxHistoryGrow at most:
// no change is lost where a page ends among changes at one time.
func pages(ctx context.Context, c *prom.Client, p *historyPages, until int64, path func(until int64, n int) string, read func([]byte) ([]int64, error),
	at func(ms int64) int64, what string) error {
	limit := historyPage
	for {
		body, n, err := page(ctx, c, func(n int) string { return path(until, n) }, limit)
		if err != nil {
			return fmt.Errorf("reading %s: %w", what, err)
		}
		// Pages that each hold one change not seen, or are asked again
		// and again, can't go on for ever.
		if p.read += int64(len(body)); p.read > historyBytes {
			return p.tooMuch()
		}
		times, err := read(body)
		if err != nil {
			return err
		}
		if len(times) < n {
			return nil
		}
		oldest := slices.Min(times)
		next := at(oldest)
		if next >= until {
			// All of one time: asked again for more, unless the answer was
			// too long to.
			if n < limit || n >= maxHistoryGrow {
				return fmt.Errorf("reading %s: more than %d changes of state at one time, more than a page", what, n)
			}
			limit = min(2*n, maxHistoryGrow)
			continue
		}
		until, limit = next, n
	}
}

// tagLabels reads an annotation's tags as labels, NAME:VALUE each, the
// name to the first colon; ok is false for a tag without one, a name given
// twice, or more than the bounds.
func tagLabels(tags []string) (map[string]string, bool) {
	if len(tags) > maxHistoryLabels {
		return nil, false
	}
	l := make(map[string]string, len(tags))
	for _, t := range tags {
		k, v, ok := strings.Cut(t, ":")
		if _, twice := l[k]; !ok || twice || k == "" || len(k) > maxHistoryName || len(v) > maxHistoryValue {
			return nil, false
		}
		l[k] = v
	}
	return l, true
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
// when, which alert and which states.
func readLokiHistory(ctx context.Context, c *prom.Client, from, to time.Time, p *historyPages) error {
	until := to.Unix()
	if !to.Truncate(time.Second).Equal(to) {
		until++
	}
	return pages(ctx, c, p, until, func(until int64, n int) string {
		q := url.Values{"from": {strconv.FormatInt(from.Unix(), 10)}, "to": {strconv.FormatInt(until, 10)}, "limit": {strconv.Itoa(n)}}
		return "/api/v1/rules/history?" + q.Encode()
	}, func(body []byte) ([]int64, error) {
		times, lines, err := lokiFrame(body)
		if err != nil {
			return nil, fmt.Errorf("reading Grafana's alert state history: %w", err)
		}
		for i, ms := range times {
			var l lokiLine
			if json.Unmarshal(lines[i], &l) != nil {
				// Counted once, however often it is read again.
				if k := lokiKey(ms, lines[i]); !p.seen[k] {
					p.seen[k] = true
					p.passed++
				}
				continue
			}
			rule := l.Labels["alertname"]
			if rule == "" {
				rule = l.RuleTitle
			}
			// When, which alert and which states; the line whole, when Grafana
			// gives no fingerprint of the alert.
			key := lokiKey(ms, []byte(l.RuleUID+"\x00"+l.Fingerprint+"\x00"+l.Previous+"\x00"+l.Current))
			if l.Fingerprint == "" {
				key = lokiKey(ms, lines[i])
			}
			if err := p.take(key, time.UnixMilli(ms), rule, l.Labels, l.Previous, l.Current); err != nil {
				return nil, err
			}
		}
		return times, nil
	}, func(ms int64) int64 { return ms/1000 + 1 }, "Grafana's alert state history")
}

// lokiKey is a change's key, kept to tell it apart from those read
// again: a hash, so that what is kept of each is small, however long what
// it is of.
func lokiKey(ms int64, of []byte) string {
	sum := sha256.Sum256(of)
	return "l" + strconv.FormatInt(ms, 10) + hex.EncodeToString(sum[:16])
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
