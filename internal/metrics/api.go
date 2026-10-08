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
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
)

// API serves the part of the Prometheus HTTP API an investigating agent uses, over a frozen store.
//
// The clock is the point. An agent asks for "the last ten minutes"; against samples that end an hour
// ago the honest answer is nothing, which is not what the incident looked like.
//
// What is mapped is the question, never the data. A time after the freeze does not exist in the case,
// so a request that reaches past it is asking about a present the case does not have: it is moved back
// by the distance from the freeze to the caller's now, and "the last ten minutes" becomes the last ten
// minutes of the incident. A request that ends at or before the freeze names the incident's own time
// — an agent read "19:21:05" in a pod log and asks the metrics about 19:21:05 — and is taken as written.
//
// Every answer is stamped with the incident's own time, whichever way it was asked. The Kubernetes half
// of a case keeps its timestamps too, so a spike in the metrics and a line in a pod log that happened
// together carry the same time. (An earlier version moved the answers forward to the caller's clock.
// Replayed a day later, the metrics then said "just now" and the logs beside them said "yesterday".)
type API struct {
	store  *Store
	engine *promql.Engine
	freeze time.Time
	now    func() time.Time
}

// NewAPI builds the server. now is injectable for tests; nil means the wall clock.
func NewAPI(store *Store, freeze time.Time, now func() time.Time) *API {
	if now == nil {
		now = time.Now
	}
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
	return &API{store: store, engine: promql.NewEngine(opts), freeze: freeze, now: now}
}

// engineOpts is the engine a Prometheus runs with nothing configured: the `@` modifier and a negative
// offset have been on by default since 2.33, and an engine built without saying so refuses both.
var engineOpts = promql.EngineOpts{MaxSamples: 50_000_000, Timeout: 30 * time.Second, EnableAtModifier: true, EnableNegativeOffset: true}

// Prometheus's own defaults, for a case that does not say what its Prometheus was set to.
const (
	defaultEvaluationInterval = time.Minute
	defaultLookbackDelta      = 5 * time.Minute
)

// nowSlack is how far from this server's now an instant may lie and still be the caller's now. A
// caller that asks about now names its own clock's instant, read a moment before the request arrived.
const nowSlack = 2 * time.Second

// place says how far a request is moved back, and what it was taken for, given the instants it begins
// and ends at (for an instant query, the one instant twice).
//
// A request that ends at or before the freeze names the incident's own time, and is taken as written.
// A time past the freeze is not in the case, and a request that reaches there is one of three things.
// No rule tells them apart every time — a clock time and "ten minutes ago" are the same number — so
// the rule is one that does not change with the age of the replay, and what it took a request for it
// says beside the answer (remark), where an agent reads it and can ask again.
//
// About now: it ends within nowSlack of this server's now. That is the caller's now itself, read a
// moment before the request arrived, and it is moved onto the freeze exactly. (Moved back by this
// server's clock it landed some milliseconds before the freeze, a different few each time, and `rate`
// over a window a few milliseconds elsewhere is another number in its fourth or fifth digit: the same
// question asked twice of a frozen case got two answers.) This is what promq asks, and nothing is
// remarked on it.
//
// The incident's own time, overshot: a window that begins at or before the freeze. An agent read
// 09:42 in a pod log and asks for 09:18:00 to 09:43:00, of a case frozen at 09:42:36; or for the whole
// hour, or the whole day. It is taken as written and cut at the freeze. (Every request past the
// freeze was once moved back by the age of the replay: a recorded run asked for exactly that first
// window seven minutes after the freeze and was answered up to 09:35:44, and a day later it would
// have been answered with nothing.)
//
// A present the case does not have: a window that begins after the freeze, and any instant past it —
// "an hour ago", asked a day later. It is moved back by the distance from the freeze to this server's
// now.
//
// What the rule gets wrong, it gets wrong whatever the age of the replay, and says so: a window
// meant as "from twenty minutes ago to ten minutes ago", asked of a replay younger than that, begins
// before the freeze and is taken as written; an instant meant as the incident's own time and named
// some seconds past the freeze is moved back.
//
// Whichever it is, no step is evaluated past the freeze (query, queryRange). The engine does not
// know the case ends there: it would carry the last sample forward for as long as it looks back and
// let a `rate` run out of samples, and the answer would be a traffic that fell to nothing in the
// minute after the freeze, which nobody measured.
func (a *API) place(begins, ends time.Time) (back time.Duration, taken reading) {
	if !ends.After(a.freeze) {
		return 0, asWritten
	}
	now := a.now()
	switch {
	case now.Sub(ends).Abs() <= nowSlack:
		return ends.Sub(a.freeze), aboutNow
	case !begins.After(a.freeze):
		return 0, overshot
	}
	return now.Sub(a.freeze), aPresent
}

// reading is what a request was taken for.
type reading int

const (
	asWritten reading = iota // it ends at or before the freeze
	aboutNow                 // it ends at the caller's now
	overshot                 // a window of the incident's own time that runs past the freeze
	aPresent                 // about a present the case does not have
)

// remarkPrefix begins what a frozen store says beside an answer about how it read the request. promq
// prints a remark that begins so, and nothing a Prometheus sends does.
const remarkPrefix = "frozen case: "

// remark says how a request past the freeze was read, for the two readings that are a choice.
func (a *API) remark(taken reading, back time.Duration) []string {
	ends := a.freeze.UTC().Format("2006-01-02T15:04:05.000Z")
	switch taken {
	case overshot:
		return []string{remarkPrefix + "it ends at " + ends + "; this window begins before that, and was taken as written and cut there"}
	case aPresent:
		return []string{remarkPrefix + "it ends at " + ends + "; this was taken to be about the present, and moved back by " + back.Round(time.Millisecond).String() +
			" — name a time at or before the end to ask about that time"}
	}
	return nil
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/query", a.query)
	mux.HandleFunc("/api/v1/query_range", a.queryRange)
	mux.HandleFunc("/api/v1/labels", a.labelNames)
	mux.HandleFunc("/api/v1/label/", a.labelValues)
	mux.HandleFunc("/api/v1/series", a.series)
	// Nothing was frozen for these, and a client that asks should be told so in the shape it expects:
	// an empty answer, not a page it cannot parse.
	for path, empty := range map[string]any{
		"/api/v1/metadata":        map[string]any{},
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

// answered turns a panic into an answer. The engine recovers from what goes wrong while it evaluates;
// what goes wrong before that used to end the connection, and the client was told "EOF". And it
// answers only what a Prometheus's API answers: a GET or a POST.
func answered(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			switch r.Method {
			case http.MethodGet, http.MethodPost:
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
		return FromSeconds(f), nil
	}
	switch s {
	case minTime.Format(time.RFC3339Nano):
		return minTime, nil
	case maxTime.Format(time.RFC3339Nano):
		return maxTime, nil
	}
	return time.Parse(time.RFC3339Nano, s)
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

// FromSeconds converts a Unix time in seconds, such as the freeze_time of a case, to the millisecond.
func FromSeconds(f float64) time.Time { return time.UnixMilli(int64(math.Round(f * 1000))) }

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
		back, taken := a.place(*named, *named)
		if at, said = named.Add(-back), a.remark(taken, back); at.After(a.freeze) {
			at = a.freeze // still past it: a time to come, and the latest the case has is the freeze
		}
	}
	q, err := a.engine.NewInstantQuery(ctx, a.store, opts, r.FormValue("query"), at)
	if err != nil {
		invalid(w, "query", err)
		return
	}
	a.run(w, ctx, q, r.FormValue("query"), limit, said)
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
	back, taken := a.place(start, end) // the window is one request: it is moved whole or not at all
	start, end, said := start.Add(-back), end.Add(-back), a.remark(taken, back)
	if end.After(a.freeze) { // and it stops at the freeze: the steps that are evaluated are the ones not past it
		if start.After(a.freeze) {
			// None is: the window is of a time to come. The query is read as a window's is, so that
			// one a window cannot ask is still refused, and there is nothing to answer it with.
			q, err := a.engine.NewRangeQuery(ctx, a.store, opts, r.FormValue("query"), a.freeze, a.freeze, step)
			if err != nil {
				invalid(w, "query", err)
				return
			}
			q.Close()
			beside(w, map[string]any{"resultType": "matrix", "result": []any{}}, nil, said)
			return
		}
		end = a.freeze
	}
	q, err := a.engine.NewRangeQuery(ctx, a.store, opts, r.FormValue("query"), start, end, step)
	if err != nil {
		invalid(w, "query", err)
		return
	}
	a.run(w, ctx, q, r.FormValue("query"), limit, said)
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

func (a *API) run(w http.ResponseWriter, ctx context.Context, q promql.Query, query string, limit int, said []string) {
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
	beside(w, map[string]any{"resultType": res.Value.Type(), "result": encode(res.Value)}, warnings, append(infos, said...))
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

// encode renders a result with the timestamps it has. Native histograms are not carried by a case,
// so only float samples appear.
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
	case promql.Scalar:
		return point(v.T, v.V)
	case promql.String:
		return [2]any{stamp(v.T), v.V}
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

// The discovery endpoints honour match[] and ignore start and end: a case is one incident window, and
// all of it is in scope. A start or an end that is not a time is refused all the same, as it is by a
// Prometheus, which reads them before it uses them.
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
	// One selector's series come as the store holds them; several are merged, and so by label.
	var found []labels.Labels
	for _, se := range a.store.series {
		for _, ms := range sels {
			if matches(se.lset, ms) {
				found = append(found, se.lset)
				break
			}
		}
	}
	if len(sels) > 1 {
		sort.SliceStable(found, func(i, j int) bool { return labels.Compare(found[i], found[j]) < 0 })
	}
	out := make([]map[string]string, len(found))
	for i, lset := range found {
		out[i] = lset.Map()
	}
	limited(w, out, limit)
}
