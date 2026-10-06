package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
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
// ago the honest answer is nothing, which is not what the incident looked like. So every request is
// evaluated as if now were the moment of the freeze, and the timestamps in the answer are moved
// forward again to where the caller thinks they are.
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
	engine := promql.NewEngine(promql.EngineOpts{MaxSamples: 50_000_000, Timeout: 30 * time.Second})
	return &API{store: store, engine: engine, freeze: freeze, now: now}
}

// offset is how far the caller's clock has run past the freeze.
func (a *API) offset() time.Duration { return a.now().Sub(a.freeze) }

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/query", a.query)
	mux.HandleFunc("/api/v1/query_range", a.queryRange)
	mux.HandleFunc("/api/v1/labels", a.labelNames)
	mux.HandleFunc("/api/v1/label/", a.labelValues)
	mux.HandleFunc("/api/v1/series", a.series)
	mux.HandleFunc("/api/v1/metadata", func(w http.ResponseWriter, _ *http.Request) { ok(w, map[string]any{}) })
	mux.HandleFunc("/api/v1/status/buildinfo", func(w http.ResponseWriter, _ *http.Request) {
		ok(w, map[string]string{"version": "lapilli-case frozen store", "frozenAt": a.freeze.UTC().Format(time.RFC3339)})
	})
	for _, p := range []string{"/-/healthy", "/-/ready"} {
		mux.HandleFunc(p, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "frozen store is up") })
	}
	return mux
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
func parseTime(s string) (time.Time, error) {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return FromSeconds(f), nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

// selectorParser reads series selectors with the engine's default dialect: no experimental syntax.
var selectorParser = parser.NewParser(parser.Options{})

// FromSeconds converts a Unix time in seconds, such as the freeze_time of a case, to the millisecond.
func FromSeconds(f float64) time.Time { return time.UnixMilli(int64(math.Round(f * 1000))) }

func parseStep(s string) (time.Duration, error) {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(f * float64(time.Second)), nil
	}
	d, err := model.ParseDuration(s)
	return time.Duration(d), err
}

func (a *API) query(w http.ResponseWriter, r *http.Request) {
	off := a.offset()
	at := a.freeze // a query with no time means "now", and now is the freeze
	if s := r.FormValue("time"); s != "" {
		t, err := parseTime(s)
		if err != nil {
			fail(w, http.StatusBadRequest, "bad_data", fmt.Errorf("invalid time %q", s))
			return
		}
		at = t.Add(-off)
	}
	q, err := a.engine.NewInstantQuery(r.Context(), a.store, nil, r.FormValue("query"), at)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad_data", err)
		return
	}
	a.run(w, r.Context(), q, off)
}

func (a *API) queryRange(w http.ResponseWriter, r *http.Request) {
	off := a.offset()
	start, err1 := parseTime(r.FormValue("start"))
	end, err2 := parseTime(r.FormValue("end"))
	step, err3 := parseStep(r.FormValue("step"))
	switch {
	case err1 != nil || err2 != nil || err3 != nil:
		fail(w, http.StatusBadRequest, "bad_data", fmt.Errorf("start, end and step are required and must parse"))
		return
	case step <= 0 || end.Before(start):
		fail(w, http.StatusBadRequest, "bad_data", fmt.Errorf("step must be positive and end must not precede start"))
		return
	case end.Sub(start)/step > 11000:
		fail(w, http.StatusBadRequest, "bad_data", fmt.Errorf("more than 11000 points; raise the step"))
		return
	}
	q, err := a.engine.NewRangeQuery(r.Context(), a.store, nil, r.FormValue("query"), start.Add(-off), end.Add(-off), step)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad_data", err)
		return
	}
	a.run(w, r.Context(), q, off)
}

func (a *API) run(w http.ResponseWriter, ctx context.Context, q promql.Query, off time.Duration) {
	defer q.Close()
	res := q.Exec(ctx)
	if res.Err != nil {
		fail(w, http.StatusUnprocessableEntity, "execution", res.Err)
		return
	}
	ok(w, map[string]any{"resultType": res.Value.Type(), "result": encode(res.Value, off.Milliseconds())})
}

// point renders a timestamp in seconds and a value as a string, the way Prometheus does.
func point(tMillis int64, f float64) [2]any {
	return [2]any{json.Number(strconv.FormatFloat(float64(tMillis)/1000, 'f', 3, 64)), strconv.FormatFloat(f, 'f', -1, 64)}
}

func metric(lset labels.Labels, dropName bool) map[string]string {
	m := lset.Map()
	if dropName {
		delete(m, labels.MetricName)
	}
	return m
}

// encode shifts every timestamp forward by the clock offset. Native histograms are not carried by a
// case yet, so only float samples appear.
func encode(v parser.Value, offMillis int64) any {
	switch v := v.(type) {
	case promql.Vector:
		out := make([]map[string]any, 0, len(v))
		for _, s := range v {
			if s.H == nil {
				out = append(out, map[string]any{"metric": metric(s.Metric, s.DropName), "value": point(s.T+offMillis, s.F)})
			}
		}
		return out
	case promql.Matrix:
		out := make([]map[string]any, 0, len(v))
		for _, s := range v {
			values := make([][2]any, len(s.Floats))
			for i, p := range s.Floats {
				values[i] = point(p.T+offMillis, p.F)
			}
			out = append(out, map[string]any{"metric": metric(s.Metric, s.DropName), "values": values})
		}
		return out
	case promql.Scalar:
		return point(v.T+offMillis, v.V)
	case promql.String:
		return [2]any{json.Number(strconv.FormatFloat(float64(v.T+offMillis)/1000, 'f', 3, 64)), v.V}
	}
	return nil
}

func (a *API) selectors(r *http.Request) ([][]*labels.Matcher, error) {
	r.ParseForm()
	var out [][]*labels.Matcher
	for _, s := range r.Form["match[]"] {
		ms, err := selectorParser.ParseMetricSelector(s)
		if err != nil {
			return nil, err
		}
		out = append(out, ms)
	}
	return out, nil
}

// The discovery endpoints ignore their time range: a case is one incident window, and all of it is in scope.

func (a *API) labelNames(w http.ResponseWriter, r *http.Request) {
	q, _ := a.store.Querier(math.MinInt64, math.MaxInt64)
	names, _, _ := q.LabelNames(r.Context(), nil)
	ok(w, names)
}

func (a *API) labelValues(w http.ResponseWriter, r *http.Request) {
	name, found := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/label/"), "/values")
	if !found || name == "" {
		fail(w, http.StatusNotFound, "not_found", fmt.Errorf("expected /api/v1/label/<name>/values"))
		return
	}
	q, _ := a.store.Querier(math.MinInt64, math.MaxInt64)
	values, _, _ := q.LabelValues(r.Context(), name, nil)
	ok(w, values)
}

func (a *API) series(w http.ResponseWriter, r *http.Request) {
	sels, err := a.selectors(r)
	if err != nil || len(sels) == 0 {
		fail(w, http.StatusBadRequest, "bad_data", fmt.Errorf("at least one valid match[] selector is required"))
		return
	}
	out := []map[string]string{}
	for _, se := range a.store.series {
		for _, ms := range sels {
			if matches(se.lset, ms) {
				out = append(out, se.lset.Map())
				break
			}
		}
	}
	ok(w, out)
}
