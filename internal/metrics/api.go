package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/storage"
)

// API serves the part of the Prometheus HTTP API an investigating agent uses, over a frozen store.
//
// The clock is the point. An agent asks for "the last ten minutes"; against samples that end an hour
// ago the honest answer is nothing, which is not what the incident looked like.
//
// What is mapped is the question, never the data. A case ends at the freeze. A request that ends at or
// before it names the incident's own time — an agent read "19:21:05" in a pod log and asks the metrics
// about 19:21:05 — and is taken as written. One that reaches past the end is moved back, whole, so
// that it ends there: "the last ten minutes" becomes the last ten minutes of the incident (past).
//
// The server has no clock of its own. What it answers, and what it says beside the answer, goes by the
// request and the case and by nothing else, so the same request is answered the same a second after
// the freeze and a month after.
//
// An answer is stamped with the incident's own time, whichever way it was asked — unless the query
// itself reaches forward, with a negative offset or an `@` later than the freeze, as it may of a
// Prometheus too. The Kubernetes half of a case keeps its timestamps as well, so a spike in the metrics
// and a line in a pod log that happened together carry the same time. (An earlier version moved the
// answers forward to the caller's clock. Replayed a day later, the metrics then said "just now" and
// the logs beside them said "yesterday".)
type API struct {
	store  *Store
	engine *promql.Engine
	freeze time.Time
}

// NewAPI builds the server over a store and the instant its case ends at.
func NewAPI(store *Store, freeze time.Time) *API {
	// A subquery that names no step is evaluated at the Prometheus's evaluation interval, which the
	// engine asks its caller for. Asked of nothing, it panicked, and a frozen case answered
	// `max_over_time(x[5m:])` by closing the connection.
	step := store.Source.EvaluationInterval
	if step <= 0 {
		step = defaultEvaluationInterval
	}
	opts := engineOpts
	opts.NoStepSubqueryIntervalFn = func(int64) int64 { return step.Milliseconds() }
	// And how far back an instant looks for a sample is the Prometheus's --query.lookback-delta,
	// where the freeze learned it: five minutes unless somebody set it.
	lookback := store.Source.LookbackDelta
	if lookback <= 0 {
		lookback = defaultLookbackDelta
	}
	opts.LookbackDelta = lookback
	return &API{store: store, engine: promql.NewEngine(opts), freeze: freeze}
}

// engineOpts is the engine a Prometheus runs with nothing configured: the `@` modifier and a negative
// offset have been on by default since 2.33, and an engine built without saying so refuses both.
var engineOpts = promql.EngineOpts{MaxSamples: 50_000_000, Timeout: 30 * time.Second, EnableAtModifier: true, EnableNegativeOffset: true}

// Prometheus's own defaults, for a case that does not say what its Prometheus was set to.
const (
	defaultEvaluationInterval = time.Minute
	defaultLookbackDelta      = 5 * time.Minute
)

// past is what is said of a request that reaches past the end of the case, given the instant it ends
// at; it is nothing for one that does not, and that one is taken as written.
//
// A request that reaches past the end is moved back so that it ends at the freeze: a window whole,
// with its length — it then begins that length before the freeze — and an instant onto the freeze.
// Nothing else decides it: not how long ago the case was frozen, not where the window begins, not a
// clock. And so no step of a request is evaluated past the freeze, where an engine that does not know
// the case ends would carry the last sample forward for as long as it looks back and let a `rate` run
// out of samples, and answer with a traffic that fell to nothing in the minute after the freeze, which
// nobody measured. (A query can still reach there itself, with a negative offset or an `@`.)
//
// This is the rule after four others, three of them written to put the one before right and each
// wrong in a way of its own (docs/design-case.md §3). No rule can tell a clock time from "ten minutes
// ago", which are the same number, so this one does not try; what the others cost:
//
//   - Moved back by the age of the replay, whatever it asked: a window of the incident's own time that
//     overshot the freeze — 09:18:00 to 09:43:00, of a case frozen at 09:42:36 — was answered up to
//     09:35:44 seven minutes after the freeze, and with nothing a day after; and "now" landed some
//     milliseconds before the freeze, a different few each time, so that the same window of `rate`
//     asked for twice was two numbers from the fourth or fifth digit on.
//   - Taken as written when it began before the freeze: evaluated past it, and the traffic fell away.
//   - Told for the caller's present or the incident's time by which its end lay nearer to, or by where
//     it began: the same request, the same numbers, was read one way by a young replay and another by
//     an old one.
//
// What this one costs: a request that means "some time ago" is not answered as that. One that still
// lies before the freeze is taken as written — "ten minutes ago", to a replay seven minutes old, is
// three minutes before the freeze, and nothing is said — and one that lies past it is the end of the
// case. A window is moved by where it ends, not by where its last step falls: one whose steps all
// lie inside the case and whose end does not is moved all the same. And a window of the incident's
// own clock that lies wholly after the freeze is answered with the one before it.
//
// So it says so, beside the answer, every time it moves a request. It cannot know that a caller meant
// "now" by the time it named; a caller that did knows it, and promq, which is one, does not print
// what is said of a time it did not name.
func (a *API) past(ends time.Time, window bool) []string {
	if !ends.After(a.freeze) {
		return nil
	}
	by := howLong(ends.Sub(a.freeze))
	what := "the instant asked about is " + by + " after that, and this is the answer as of the end — name an instant at or before the end to be answered about it"
	if window {
		what = "the window asked for ends " + by + " after that, and was moved back by that much, whole: each point is that much earlier than the one asked for — name an end at or before the case's to be answered about a window as it is written"
	}
	return []string{remarkPrefix + "it ends at " + a.freeze.UTC().Format("2006-01-02T15:04:05.000Z") + "; " + what}
}

// howLong writes the distance between two instants. Further than a Duration counts — a time in
// milliseconds read as seconds, a query about the year nought — it is said to be that.
func howLong(d time.Duration) string {
	if d == math.MaxInt64 {
		return "more than 292 years"
	}
	return d.String()
}

// looked is the store as one request's query sees it. An engine tells its store, with every selector
// it has the store select for, the stretch of time that selector looks at — an instant with what it
// looks back for a sample, a range, an offset, an `@` — and the earliest of that is kept here. (Not
// the stretch it asks its querier for: a query with no selector at all asks for one from 1970.)
type looked struct {
	*Store
	back atomic.Int64
}

func (a *API) looking() *looked {
	l := &looked{Store: a.store}
	l.back.Store(math.MaxInt64) // a query with no selector looks at nothing
	return l
}

func (l *looked) Querier(mint, maxt int64) (storage.Querier, error) {
	q, err := l.Store.Querier(mint, maxt)
	return &lookingQuerier{q, l}, err
}

type lookingQuerier struct {
	storage.Querier
	seen *looked
}

func (q *lookingQuerier) Select(ctx context.Context, sorted bool, hints *storage.SelectHints, ms ...*labels.Matcher) storage.SeriesSet {
	for hints != nil {
		looks := hints.Start
		switch {
		case hints.End < math.MaxInt64 && looks == hints.End+1: // it looks back less than a millisecond: at its own instant, and no further
			looks = hints.End
		case looks > hints.End: // an instant so far off that the numbers ran out on the way to what it looks at, back or forward
			looks = math.MinInt64
		}
		if had := q.seen.back.Load(); looks >= had || q.seen.back.CompareAndSwap(had, looks) {
			break
		}
	}
	return q.Querier.Select(ctx, sorted, hints, ms...)
}

// before is what is said of a query that looked further back than the case's metrics reach, which
// was evaluated from start to end.
//
// A case holds a window of its Prometheus and not the Prometheus: before the window there is nothing
// in it, and a query that looks there is answered with the nothing — a series that seems to begin
// with the window, an `increase` over half the time it was asked over. Asked of the Prometheus the
// same query had more to go on, and nothing in the answer says which it was: that the metric began
// then, or that the case did. So it is said beside the answer, among its warnings — where a
// Prometheus says that an answer may not be whole — about any query that looked before the
// beginning; and promq prints it, since no caller can know it otherwise.
//
// It goes by what the query looks at and not by what turned out to be missing, which a case cannot
// know: an instant a minute after the beginning looks four minutes before it for its sample, and is
// told so though the sample it found is the one the Prometheus found. What it is told is how much
// further back the query looks, and — of a window — the instant from which each step looks at
// nothing before the beginning: the points from there on are whole, and the ones before it are the
// ones that may be missing or come of less. Both are counted to the edge of what a step asks for (as
// below): the step a millisecond before that instant asks for the millisecond before the beginning
// too, where no sample can lie, and is counted with the ones before it. A query that pins a selector
// to an instant of its own, with `@`, looks back as far at every step, and is told only that it does.
//
// Nothing is said where the beginning is not known (Store.From).
func (a *API) before(l *looked, query string, start, end time.Time) []string {
	from, back := a.store.From, l.back.Load()
	if from == 0 || back >= from {
		return nil
	}
	// How much further is counted to the edge of what was asked, which is a millisecond before the first
	// instant a selector takes: five minutes back is everything after the instant five minutes ago.
	by := time.UnixMilli(from).Sub(time.UnixMilli(back))
	if by <= math.MaxInt64-time.Millisecond {
		by += time.Millisecond
	} else {
		by = math.MaxInt64
	}
	said := beginRemark + time.UnixMilli(from).UTC().Format("2006-01-02T15:04:05.000Z") + ", and this query looks " + howLong(by) + " further back than that"
	if whole := start.Add(by); by < math.MaxInt64 && !whole.After(end) && !pinned(query) {
		return []string{said + ": its points before " + whole.UTC().Format("2006-01-02T15:04:05.000Z") + " may be missing, or come of less than its Prometheus had"}
	}
	return []string{said + ": its Prometheus may have had more to answer from"}
}

// pinned says whether a query holds any selector to an instant of its own, with `@`.
func pinned(query string) bool {
	expr, err := everyFunction.ParseExpr(query)
	if err != nil {
		return true
	}
	found := false
	parser.Inspect(expr, func(node parser.Node, _ []parser.Node) error {
		switch n := node.(type) {
		case *parser.VectorSelector:
			found = found || n.Timestamp != nil || n.StartOrEnd != 0
		case *parser.SubqueryExpr:
			found = found || n.Timestamp != nil || n.StartOrEnd != 0
		}
		return nil
	})
	return found
}

// remarkPrefix begins what a frozen store says beside an answer, and nothing a Prometheus sends does:
// among the infos, about a request it moved back from past its end, which promq prints when it was
// given the time to ask about; and among the warnings, beginning as beginRemark does, about a query
// that looked before the case's beginning, which promq prints whenever it is said.
const (
	remarkPrefix = "frozen case: "
	beginRemark  = remarkPrefix + "it holds no samples before "
)

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/query", a.query)
	mux.HandleFunc("/api/v1/query_range", a.queryRange)
	mux.HandleFunc("/api/v1/labels", a.labelNames)
	mux.HandleFunc("/api/v1/label/", a.labelValues)
	mux.HandleFunc("/api/v1/series", a.series)
	mux.HandleFunc("/api/v1/metadata", a.metadata)
	// Nothing was frozen for these, and a client that asks should be told so in the shape it expects:
	// an empty answer, not a page it cannot parse.
	for path, empty := range map[string]any{
		"/api/v1/rules":           map[string]any{"groups": []any{}},
		"/api/v1/alerts":          map[string]any{"alerts": []any{}},
		"/api/v1/targets":         map[string]any{"activeTargets": []any{}, "droppedTargets": []any{}},
		"/api/v1/query_exemplars": []any{},
	} {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) { ok(w, empty) })
	}
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		fail(w, http.StatusNotFound, "not_found", fmt.Errorf("%s is not served by a frozen store", r.URL.Path))
	})
	mux.HandleFunc("/api/v1/status/buildinfo", func(w http.ResponseWriter, _ *http.Request) {
		ok(w, map[string]any{"version": "lapilli-case frozen store", FrozenAtField: float64(a.freeze.UnixMilli()) / 1000})
	})
	for _, p := range []string{"/-/healthy", "/-/ready"} {
		mux.HandleFunc(p, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "frozen store is up") })
	}
	return answered(mux)
}

// takesPost are the paths a Prometheus answers to POST as it does to GET: a query can be too long
// for an address.
var takesPost = map[string]bool{"/api/v1/query": true, "/api/v1/query_range": true, "/api/v1/query_exemplars": true, "/api/v1/labels": true, "/api/v1/series": true}

// getAlone says whether a path is one this store serves and a Prometheus has for GET alone, which it
// refuses another method. A path that is neither that nor one of takesPost is not served here, to
// any method — what a Prometheus has for POST and a case has not is among them — and is told so.
func getAlone(path string) bool {
	switch path {
	case "/api/v1/metadata", "/api/v1/rules", "/api/v1/alerts", "/api/v1/targets", "/api/v1/status/buildinfo":
		return true
	}
	name, values := strings.CutSuffix(strings.TrimPrefix(path, "/api/v1/label/"), "/values")
	return strings.HasPrefix(path, "/api/v1/label/") && values && name != "" && !strings.Contains(name, "/")
}

// answered turns a panic into an answer. The engine recovers from what goes wrong while it evaluates;
// what goes wrong before that used to end the connection, and the client was told "EOF". And it
// answers only what a Prometheus's API answers: a GET or a POST.

func answered(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			switch r.Method {
			case http.MethodGet:
			case http.MethodPost:
				if getAlone(r.URL.Path) {
					http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
					return
				}
			case http.MethodOptions: // answered with nothing, as a Prometheus answers it; what a browser wants beside that is not sent
				w.WriteHeader(http.StatusNoContent)
				return
			default:
				http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
				return
			}
		}
		defer func() {
			if p := recover(); p == http.ErrAbortHandler {
				panic(p) // the client went away, which is nobody's failure and no one is left to answer
			} else if p != nil {
				fail(w, http.StatusInternalServerError, "internal", fmt.Errorf("the frozen store could not answer: %v", p))
			}
		}()
		h.ServeHTTP(w, r)
	})
}

func ok(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": data})
}

func fail(w http.ResponseWriter, code int, kind string, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"status": "error", "errorType": kind, "error": err.Error()})
}

// parseTime reads seconds or RFC 3339. Seconds are rounded to the millisecond, as Prometheus does:
// a float cannot hold "…354.431" exactly, and truncating would evaluate some requests a millisecond early.
// The two instants Prometheus itself writes for "from the beginning" and "to the end" are read as it
// reads them: they have five-digit years, which the standard library does not parse.
func parseTime(s string) (time.Time, error) {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		// A number too large to be an instant is the last instant there is, and one too small the first:
		// converted as it stands, it is whatever the processor makes of it, past the freeze on one machine
		// and before the beginning of time on another. And "NaN" is a number to no one.
		switch ms := math.Round(f * 1000); {
		case math.IsNaN(ms):
			return time.Time{}, errors.New("not a time")
		case ms >= math.MaxInt64:
			return maxTime, nil
		case ms <= math.MinInt64:
			return minTime, nil
		}
		return FromSeconds(f), nil
	}
	switch s {
	case minTime.Format(time.RFC3339Nano):
		return minTime, nil
	case maxTime.Format(time.RFC3339Nano):
		return maxTime, nil
	}
	// To the millisecond, which is all the engine keeps of it: "…54.4314Z" is the instant "…54.431" is,
	// and not a time four tenths of a millisecond past a freeze at the second.
	t, err := time.Parse(time.RFC3339Nano, s)
	return t.Truncate(time.Millisecond), err
}

var (
	minTime = time.Unix(math.MinInt64/1000+62135596801, 0).UTC()
	maxTime = time.Unix(math.MaxInt64/1000-62135596801, 999999999).UTC()
)

// selectorParser reads series selectors with the engine's default dialect: no experimental syntax.
var selectorParser = parser.NewParser(parser.Options{})

// FrozenAtField is the key under which /api/v1/status/buildinfo says when the store was frozen, in
// Unix seconds. A real Prometheus has no such key.
const FrozenAtField = "lapilliFrozenAt"

// FromSeconds reads an instant written in seconds as a Prometheus reads one: to the millisecond, and
// by rounding the fraction of the second, by itself. (Multiplied whole by a thousand first, as it was
// here, a time written to nine decimals came out a millisecond later than a Prometheus has it about
// one time in eight thousand — `…354.43149999` did: the product is rounded once on the way.)
func FromSeconds(f float64) time.Time {
	sec, frac := math.Modf(f)
	return time.Unix(int64(sec), int64(math.Round(frac*1000))*int64(time.Millisecond))
}

// parseDuration reads seconds or a duration, and refuses in Prometheus's words.
func parseDuration(s string) (time.Duration, error) {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		d := f * float64(time.Second)
		if d > float64(math.MaxInt64) || d < float64(math.MinInt64) {
			return 0, fmt.Errorf("cannot parse %q to a valid duration. It overflows int64", s)
		}
		return time.Duration(d), nil
	}
	if d, err := model.ParseDuration(s); err == nil {
		return time.Duration(d), nil
	}
	return 0, fmt.Errorf("cannot parse %q to a valid duration", s)
}

// A request that is refused is refused in Prometheus's words. An agent reads them and corrects its
// query by them, and a client written against Prometheus matches on them: `invalid parameter "query":
// 1:5: parse error: …`, not this package's paraphrase.
func invalid(w http.ResponseWriter, parameter string, err error) {
	fail(w, http.StatusBadRequest, "bad_data", fmt.Errorf("invalid parameter %q: %w", parameter, err))
}

func noTimestamp(s string) error { return fmt.Errorf("cannot parse %q to a valid timestamp", s) }

// parseLimit reads how many series an answer may hold; none means all of them.
func parseLimit(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	limit, err := strconv.Atoi(s)
	if err == nil && limit < 0 {
		err = errors.New("limit must be non-negative")
	}
	return limit, err
}

// asked reads what a query may be asked with besides itself and its times: how long it may take, and
// how far back an instant looks. Both change the answer, and a request with either used to be answered
// as if it had neither.
func (a *API) asked(w http.ResponseWriter, r *http.Request) (ctx context.Context, done func(), opts promql.QueryOpts, fine bool) {
	ctx, done = r.Context(), func() {}
	if s := r.FormValue("timeout"); s != "" {
		timeout, err := parseDuration(s)
		if err != nil {
			invalid(w, "timeout", err)
			return nil, nil, nil, false
		}
		ctx, done = context.WithTimeout(ctx, timeout)
	}
	if s := r.FormValue("lookback_delta"); s != "" {
		d, err := parseDuration(s)
		if err != nil {
			done()
			fail(w, http.StatusBadRequest, "bad_data", fmt.Errorf("error parsing lookback delta duration: %w", err))
			return nil, nil, nil, false
		}
		opts = promql.NewPrometheusQueryOpts(false, d)
	}
	return ctx, done, opts, true
}

func (a *API) query(w http.ResponseWriter, r *http.Request) {
	// In the order Prometheus looks, here and for a range, so that a request wrong in two ways is told of the same one.
	limit, err := parseLimit(r.FormValue("limit"))
	if err != nil {
		invalid(w, "limit", err)
		return
	}
	var named *time.Time
	if s := r.FormValue("time"); s != "" {
		t, err := parseTime(s)
		if err != nil {
			invalid(w, "time", fmt.Errorf("invalid time value for 'time': %w", noTimestamp(s)))
			return
		}
		named = &t
	}
	ctx, done, opts, fine := a.asked(w, r)
	if !fine {
		return
	}
	defer done()
	at := a.freeze // a query with no time means "now", and now is the freeze
	var said []string
	if named != nil {
		if at, said = *named, a.past(*named, false); said != nil {
			at = a.freeze
		}
	}
	seen := a.looking()
	q, err := a.engine.NewInstantQuery(ctx, seen, opts, r.FormValue("query"), at)
	if err != nil {
		invalid(w, "query", err)
		return
	}
	a.run(w, ctx, q, r.FormValue("query"), limit, said, seen, at, at)
}

func (a *API) queryRange(w http.ResponseWriter, r *http.Request) {
	limit, err := parseLimit(r.FormValue("limit"))
	if err != nil {
		invalid(w, "limit", err)
		return
	}
	start, err := parseTime(r.FormValue("start"))
	if err != nil {
		invalid(w, "start", noTimestamp(r.FormValue("start")))
		return
	}
	end, err := parseTime(r.FormValue("end"))
	if err != nil {
		invalid(w, "end", noTimestamp(r.FormValue("end")))
		return
	}
	if end.Before(start) {
		invalid(w, "end", errors.New("end timestamp must not be before start time"))
		return
	}
	step, err := parseDuration(r.FormValue("step"))
	if err != nil {
		invalid(w, "step", err)
		return
	}
	if step <= 0 {
		invalid(w, "step", errors.New("zero or negative query resolution step widths are not accepted. Try a positive integer"))
		return
	}
	if end.Sub(start)/step > 11000 {
		fail(w, http.StatusBadRequest, "bad_data", errors.New("exceeded maximum resolution of 11,000 points per timeseries. Try decreasing the query resolution (?step=XX)"))
		return
	}
	ctx, done, opts, fine := a.asked(w, r)
	if !fine {
		return
	}
	defer done()
	said := a.past(end, true)
	if said != nil { // the window is one request: it is moved whole, to end at the freeze, or not at all
		start, end = a.freeze.Add(-end.Sub(start)), a.freeze
	}
	seen := a.looking()
	q, err := a.engine.NewRangeQuery(ctx, seen, opts, r.FormValue("query"), start, end, step)
	if err != nil {
		invalid(w, "query", err)
		return
	}
	a.run(w, ctx, q, r.FormValue("query"), limit, said, seen, start, end)
}

// beside answers with what stands beside the result: the engine's warnings and remarks, and this
// store's own on how it read the request.
func beside(w http.ResponseWriter, data any, warnings, infos []string) {
	answer := map[string]any{"status": "success", "data": data}
	if len(warnings) > 0 {
		answer["warnings"] = warnings
	}
	if len(infos) > 0 {
		answer["infos"] = infos
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(answer)
}

func (a *API) run(w http.ResponseWriter, ctx context.Context, q promql.Query, query string, limit int, said []string, seen *looked, start, end time.Time) {
	defer q.Close()
	res := q.Exec(ctx)
	if res.Err != nil { // by what went wrong, as Prometheus sorts it: out of time, called off, or a query that cannot be evaluated
		var timedOut promql.ErrQueryTimeout
		var calledOff promql.ErrQueryCanceled
		switch {
		case errors.As(res.Err, &calledOff) || errors.Is(res.Err, context.Canceled):
			fail(w, 499, "canceled", res.Err)
		case errors.As(res.Err, &timedOut):
			fail(w, http.StatusServiceUnavailable, "timeout", res.Err)
		default:
			fail(w, http.StatusUnprocessableEntity, "execution", res.Err)
		}
		return
	}
	if limit > 0 { // no more series than were asked for, and a word that there were more
		switch v := res.Value.(type) {
		case promql.Vector:
			if len(v) > limit {
				res.Value, res.Warnings = v[:limit], res.Warnings.Add(errTruncated)
			}
		case promql.Matrix:
			if len(v) > limit {
				res.Value, res.Warnings = v[:limit], res.Warnings.Add(errTruncated)
			}
		}
	}
	// What the engine remarked on beside its answer — `rate` of something not named like a counter —
	// goes out beside it, as Prometheus sends it: ten of each at the most.
	warnings, infos := res.Warnings.AsStrings(query, 10, 10)
	// And after them what the store has to say: among the warnings, that the query looked before the
	// case's beginning; among the infos, that the request was moved back from past its end.
	beside(w, map[string]any{"resultType": res.Value.Type(), "result": encode(res.Value)}, append(warnings, a.before(seen, query, start, end)...), append(infos, said...))
}

var errTruncated = errors.New("results truncated due to limit")

// stamp writes a time in seconds as Prometheus writes it: the milliseconds after a point, three
// digits of them, and nothing after the seconds when there are none.
func stamp(tMillis int64) json.Number {
	sign := ""
	if tMillis < 0 {
		sign, tMillis = "-", -tMillis
	}
	if tMillis%1000 == 0 {
		return json.Number(sign + strconv.FormatInt(tMillis/1000, 10))
	}
	return json.Number(fmt.Sprintf("%s%d.%03d", sign, tMillis/1000, tMillis%1000))
}

// sampleValue writes a sample's value as Prometheus writes it: in full, and with an exponent only
// below a millionth and from 1e21 up.
func sampleValue(f float64) string {
	format := byte('f')
	if abs := math.Abs(f); abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	return strconv.FormatFloat(f, format, -1, 64)
}

// point renders a timestamp in seconds and a value as a string, the way Prometheus does.
func point(tMillis int64, f float64) [2]any {
	return [2]any{stamp(tMillis), sampleValue(f)}
}

func metric(lset labels.Labels, dropName bool) map[string]string {
	m := lset.Map()
	if dropName {
		delete(m, labels.MetricName)
	}
	return m
}

// encode renders a result with the timestamps it has, each kind as a Prometheus writes it. Native
// histograms are not carried by a case, so only float samples appear.
func encode(v parser.Value) any {
	switch v := v.(type) {
	case promql.Vector:
		out := make([]map[string]any, 0, len(v))
		for _, s := range v {
			if s.H == nil {
				out = append(out, map[string]any{"metric": metric(s.Metric, s.DropName), "value": point(s.T, s.F)})
			}
		}
		return out
	case promql.Matrix:
		out := make([]map[string]any, 0, len(v))
		for _, s := range v {
			values := make([][2]any, len(s.Floats))
			for i, p := range s.Floats {
				values[i] = point(p.T, p.F)
			}
			out = append(out, map[string]any{"metric": metric(s.Metric, s.DropName), "values": values})
		}
		return out
	// A scalar and a string are written by other code of a Prometheus than a sample is (promql.Scalar's
	// and promql.String's own MarshalJSON, where a sample goes through the API's codec): the instant
	// as the shortest number that is it, so that half past a second ends in `.5` where a sample's ends
	// in `.500`; and a scalar's value in full, with no exponent however large or small.
	case promql.Scalar:
		return [2]any{float64(v.T) / 1000, strconv.FormatFloat(v.V, 'f', -1, 64)}
	case promql.String:
		return [2]any{float64(v.T) / 1000, v.V}
	}
	return nil
}

// selectors reads match[] as Prometheus does, and refuses what it refuses in its words: a selector
// that does not parse, and one that would match everything by matching nothing in particular.
func (a *API) selectors(r *http.Request) ([][]*labels.Matcher, error) {
	r.ParseForm()
	var out [][]*labels.Matcher
	for _, s := range r.Form["match[]"] { // all of them read first: one that does not parse is told before one that matches everything
		ms, err := selectorParser.ParseMetricSelector(s)
		if err != nil {
			return nil, err
		}
		out = append(out, ms)
	}
	for _, ms := range out {
		particular := false
		for _, m := range ms {
			particular = particular || !m.Matches("")
		}
		if !particular {
			return nil, errors.New("match[] must contain at least one non-empty matcher")
		}
	}
	return out, nil
}

// The discovery endpoints honour match[], and answer for all a case holds whatever start and end
// say: a case is one window. (A Prometheus answers the names and values of labels for everything in
// the stores a window touches, its whole head among them, and lists the series whose chunks reach
// into the window; a case has neither stores nor chunks.) A start or an end that is not a time is
// refused all the same, as it is by a Prometheus, which reads them before it uses them.
func window(w http.ResponseWriter, r *http.Request) bool {
	for _, name := range []string{"start", "end"} {
		if s := r.FormValue(name); s != "" {
			if _, err := parseTime(s); err != nil {
				invalid(w, name, fmt.Errorf("invalid time value for '%s': %w", name, noTimestamp(s)))
				return false
			}
		}
	}
	return true
}

// union runs a label lookup once per selector, or once over everything when there is none, and merges.
func (a *API) union(w http.ResponseWriter, r *http.Request, lookup func(ms ...*labels.Matcher) []string) {
	limit, err := parseLimit(r.FormValue("limit"))
	if err != nil {
		invalid(w, "limit", err)
		return
	}
	if !window(w, r) {
		return
	}
	sels, err := a.selectors(r)
	if err != nil { // without the parameter's name here, and with it for the series: as Prometheus has it
		fail(w, http.StatusBadRequest, "bad_data", err)
		return
	}
	if len(sels) == 0 {
		sels = [][]*labels.Matcher{nil}
	}
	seen := map[string]struct{}{}
	for _, ms := range sels {
		for _, v := range lookup(ms...) {
			seen[v] = struct{}{}
		}
	}
	limited(w, sortedKeys(seen), limit)
}

// limited answers with no more than was asked for, and a word that there was more: as Prometheus does
// for names, values and series as for a query's.
func limited[T any](w http.ResponseWriter, found []T, limit int) {
	answer := map[string]any{"status": "success", "data": found}
	if limit > 0 && len(found) > limit {
		answer["data"], answer["warnings"] = found[:limit], []string{errTruncated.Error()}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(answer)
}

func (a *API) labelNames(w http.ResponseWriter, r *http.Request) {
	q, _ := a.store.Querier(math.MinInt64, math.MaxInt64)
	a.union(w, r, func(ms ...*labels.Matcher) []string {
		names, _, _ := q.LabelNames(r.Context(), nil, ms...)
		return names
	})
}

func (a *API) labelValues(w http.ResponseWriter, r *http.Request) {
	name, found := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/label/"), "/values")
	if !found || name == "" || strings.Contains(name, "/") {
		fail(w, http.StatusNotFound, "not_found", fmt.Errorf("expected /api/v1/label/<name>/values"))
		return
	}
	q, _ := a.store.Querier(math.MinInt64, math.MaxInt64)
	a.union(w, r, func(ms ...*labels.Matcher) []string {
		values, _, _ := q.LabelValues(r.Context(), name, nil, ms...)
		return values
	})
}

func (a *API) series(w http.ResponseWriter, r *http.Request) {
	if r.ParseForm(); len(r.Form["match[]"]) == 0 {
		fail(w, http.StatusBadRequest, "bad_data", errors.New("no match[] parameter provided"))
		return
	}
	limit, err := parseLimit(r.FormValue("limit"))
	if err != nil {
		invalid(w, "limit", err)
		return
	}
	if !window(w, r) {
		return
	}
	sels, err := a.selectors(r)
	if err != nil {
		invalid(w, "match[]", err)
		return
	}
	// Every series the selectors match, whatever window is asked for: a case is one window, and a
	// Prometheus's own rule for which series a window lists goes by its chunks, which a case does not
	// have. The window's start is read for one thing, the order.
	//
	// One selector's series come as the store holds them; several are merged, and so by label. And so
	// are one selector's, where the Prometheus would have reached a block for them: a request that
	// names no start is about everything it holds.
	from := int64(math.MinInt64)
	if s := r.FormValue("start"); s != "" {
		if t, err := parseTime(s); err == nil {
			from = t.UnixMilli()
		}
	}
	var found []labels.Labels
	for _, se := range a.store.series {
		for _, ms := range sels {
			if matches(se.lset, ms) {
				found = append(found, se.lset)
				break
			}
		}
	}
	if len(sels) > 1 || a.store.byLabel(from) {
		sort.SliceStable(found, func(i, j int) bool { return labels.Compare(found[i], found[j]) < 0 })
	}
	out := make([]map[string]string, len(found))
	for i, lset := range found {
		out[i] = lset.Map()
	}
	limited(w, out, limit)
}
