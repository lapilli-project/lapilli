package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang/snappy"
	"github.com/lapilli-project/lapilli/internal/casefile"
	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/prompb"
)

// The sealed case this package was built against: fifteen series from a real Prometheus.
const (
	s2Case    = "../../cases/s2-periodic-saturation"
	s2Metrics = s2Case + "/metrics.jsonl.gz"
	// The instant a replay of that case evaluates "now" at: its freeze_time, 1791228354.43095, rounded
	// to the millisecond the way a request's time is. The sealed-case test checks it against freeze.json.
	s2Freeze = 1791228354431
)

var stale = math.Float64frombits(value.StaleNaN)

type apiResponse struct {
	Raw    []byte `json:"-"`
	Code   int    `json:"-"`
	Kind   string `json:"errorType"`
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
			Values [][2]any          `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

func call(t *testing.T, api *API, path string, params url.Values) apiResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path+"?"+params.Encode(), nil))
	var out apiResponse
	json.Unmarshal(rec.Body.Bytes(), &out) // a scalar's result is not a list; Raw is there for those
	out.Raw, out.Code = rec.Body.Bytes(), rec.Code
	if out.Status == "" {
		t.Fatalf("%s: not an API response: %s", path, rec.Body)
	}
	return out
}

func seconds(ms int64) string { return strconv.FormatFloat(float64(ms)/1000, 'f', 3, 64) }

func TestValuesSurviveTheFileBitForBit(t *testing.T) {
	in := []float64{0, math.Copysign(0, -1), 1.0 / 3, 5902.174954327807, math.MaxFloat64, math.SmallestNonzeroFloat64, math.Inf(1), math.Inf(-1), math.NaN(), stale}
	ts := make([]int64, len(in))
	for i := range ts {
		ts[i] = int64(1000 * (i + 1))
	}
	a := &Store{}
	if err := a.Add(map[string]string{"__name__": "m", "k": "v"}, ts, in); err != nil {
		t.Fatal(err)
	}
	var file bytes.Buffer
	if err := a.Write(&file); err != nil {
		t.Fatal(err)
	}
	b, err := Read(bytes.NewReader(file.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	for i, sm := range b.series[0].samples {
		if got, want := math.Float64bits(sm.F()), math.Float64bits(in[i]); got != want || sm.T() != ts[i] {
			t.Errorf("sample %d: %016x at %d, want %016x at %d", i, got, sm.T(), want, ts[i])
		}
	}
	var again bytes.Buffer
	b.Write(&again)
	if !bytes.Equal(file.Bytes(), again.Bytes()) {
		t.Error("the same samples wrote two different files")
	}
}

func TestReadRefusesAFileThatDecompressesToTooMuch(t *testing.T) {
	a := &Store{}
	a.Add(map[string]string{"__name__": "m"}, []int64{1000, 2000, 3000}, []float64{1, 2, 3})
	var file bytes.Buffer
	a.Write(&file)
	defer func(n int64) { MaxBytes = n }(MaxBytes)
	MaxBytes = 16
	if _, err := Read(bytes.NewReader(file.Bytes())); err == nil || !strings.Contains(err.Error(), "more than 16 bytes") {
		t.Errorf("a file over the limit: %v", err)
	}
	MaxBytes = 1 << 20
	if _, err := Read(bytes.NewReader(file.Bytes())); err != nil {
		t.Errorf("a file under the limit: %v", err)
	}
}

func TestAddRejectsWhatWouldCorruptAQuery(t *testing.T) {
	s := &Store{}
	if err := s.Add(map[string]string{"__name__": "m"}, []int64{2, 1}, []float64{0, 0}); err == nil {
		t.Error("descending timestamps were accepted")
	}
	if err := s.Add(map[string]string{"__name__": "m"}, []int64{1}, []float64{0, 0}); err == nil {
		t.Error("mismatched lengths were accepted")
	}
	s.Add(map[string]string{"__name__": "m"}, []int64{1}, []float64{0})
	if err := s.Add(map[string]string{"__name__": "m"}, []int64{2}, []float64{0}); err == nil {
		t.Error("the same series was accepted twice")
	}
}

// The numbers are the ones Prometheus's own TSDB reader returns for the block this case was frozen
// from, at the instant a replay of the case uses (testdata/reference/README.md says how to get them
// again). An engine upgrade that moves them is a finding, not a flake.
func TestTheSealedCaseAnswersAsItsPrometheusDid(t *testing.T) {
	store, err := Load(s2Metrics)
	if err != nil {
		t.Fatal(err)
	}
	if n, m := store.Size(); n != 15 || m != 2064 {
		t.Fatalf("store holds %d series and %d samples, want 15 and 2064", n, m)
	}
	info, err := casefile.LoadFreezeInfo(s2Case)
	if err != nil || FromSeconds(info.FreezeTime).UnixMilli() != s2Freeze {
		t.Fatalf("the digits below are pinned at %d; the case replays at %+v (%v)", s2Freeze, info, err)
	}
	freeze := time.UnixMilli(s2Freeze)
	api := NewAPI(store, freeze, func() time.Time { return freeze })
	for query, want := range map[string]map[string]string{
		`sum by (client) (increase(thumb_requests_total[10m]))`: {
			"catalog-indexer": "5902.174954327808", "web-frontend": "729.0744049169665", "image-proxy": "434.6331407640582", "email-renderer": "171.4530178466593"},
		`sum by (code) (increase(thumb_requests_total[30m]))`:       {"200": "7683.182281123499", "503": "895.1944709103255"},
		`topk(1, sum by (client) (rate(thumb_requests_total[2m])))`: {"catalog-indexer": "10.148179067097987"},
		`sum(rate(thumb_requests_total{code="503"}[5m]))`:           {"": "1.4135545303236259"},
		`max_over_time(thumb_inflight[10m])`:                        {"": "33"},
		`count(up)`:                                                 {"": "1"},
	} {
		res := call(t, api, "/api/v1/query", url.Values{"query": {query}})
		got := map[string]string{}
		for _, s := range res.Data.Result {
			got[s.Metric["client"]+s.Metric["code"]] = s.Value[1].(string)
		}
		if res.Status != "success" || !reflect.DeepEqual(got, want) {
			t.Errorf("%s\n got %v (%s %s)\nwant %v", query, got, res.Status, res.Error, want)
		}
	}
}

func TestTheClockStandsStillAtTheFreeze(t *testing.T) {
	store, err := Load(s2Metrics)
	if err != nil {
		t.Fatal(err)
	}
	freeze := time.UnixMilli(s2Freeze)
	const query = `sum(increase(thumb_requests_total[10m]))`
	stampOf := func(ms int64) float64 { return float64(ms) / 1000 }
	at := func(now time.Time, params url.Values) (string, float64) {
		params.Set("query", query)
		res := call(t, NewAPI(store, freeze, func() time.Time { return now }), "/api/v1/query", params)
		if len(res.Data.Result) != 1 {
			t.Fatalf("at %s with %v: %d results (%s)", now, params, len(res.Data.Result), res.Error)
		}
		return res.Data.Result[0].Value[1].(string), res.Data.Result[0].Value[0].(float64)
	}

	// "The last ten minutes", asked at the freeze and asked a month later, is the same ten minutes —
	// and the answer carries the incident's own time both times, not the day it was asked.
	then, stamp := at(freeze, url.Values{})
	later := freeze.Add(31 * 24 * time.Hour)
	now, lateStamp := at(later, url.Values{})
	if then != now {
		t.Errorf("the answer drifted with the wall clock: %s at the freeze, %s a month later", then, now)
	}
	if want := stampOf(s2Freeze); stamp != want || lateStamp != want {
		t.Errorf("answers are stamped %v and %v; the incident's own time is %v", stamp, lateStamp, want)
	}

	// "Five minutes ago" on the caller's clock is five minutes before the freeze.
	fiveAgo, stamp := at(later, url.Values{"time": {seconds(later.Add(-5 * time.Minute).UnixMilli())}})
	direct, _ := at(freeze, url.Values{"time": {seconds(freeze.Add(-5 * time.Minute).UnixMilli())}})
	if fiveAgo != direct || fiveAgo == then {
		t.Errorf("five minutes ago: %s a month later, %s at the freeze, %s for now", fiveAgo, direct, then)
	}
	if want := stampOf(freeze.Add(-5 * time.Minute).UnixMilli()); stamp != want {
		t.Errorf("five minutes ago is stamped %v, want %v", stamp, want)
	}

	// A time at or before the freeze is the incident's own: the agent read it in a pod log. It means
	// itself however late the question is asked.
	inLog := freeze.Add(-5 * time.Minute)
	literal, stamp := at(later, url.Values{"time": {inLog.UTC().Format(time.RFC3339Nano)}})
	if literal != direct || stamp != stampOf(inLog.UnixMilli()) {
		t.Errorf("asked a month later about %s: %s stamped %v, want %s", inLog.UTC().Format(time.RFC3339), literal, stamp, direct)
	}
	if atFreeze, stamp := at(later, url.Values{"time": {seconds(freeze.UnixMilli())}}); atFreeze != then || stamp != stampOf(s2Freeze) {
		t.Errorf("the freeze instant itself: %s stamped %v", atFreeze, stamp)
	}

	// A range asked on the wall clock and the same range asked in the incident's time are one answer.
	api := NewAPI(store, freeze, func() time.Time { return later })
	points := func(start, end time.Time) [][2]any {
		res := call(t, api, "/api/v1/query_range", url.Values{"query": {`thumb_inflight`}, "step": {"60"},
			"start": {seconds(start.UnixMilli())}, "end": {seconds(end.UnixMilli())}})
		if len(res.Data.Result) != 1 || len(res.Data.Result[0].Values) != 11 {
			t.Fatalf("range %s..%s: %+v %s", start, end, res.Data.Result, res.Error)
		}
		return res.Data.Result[0].Values
	}
	wallClock, incident := points(later.Add(-10*time.Minute), later), points(freeze.Add(-10*time.Minute), freeze)
	if !reflect.DeepEqual(wallClock, incident) {
		t.Errorf("asked on the wall clock: %v\nasked in incident time: %v", wallClock, incident)
	}
	if first, last := incident[0][0].(float64), incident[10][0].(float64); first != stampOf(freeze.Add(-10*time.Minute).UnixMilli()) || last != stampOf(s2Freeze) {
		t.Errorf("the window runs %v..%v, want the ten minutes before the freeze", first, last)
	}

	// PromQL's own clock agrees with the stamps: time() is the incident's now, not the caller's.
	res := call(t, api, "/api/v1/query", url.Values{"query": {`time()`}})
	if string(res.Raw) == "" || !strings.Contains(string(res.Raw), `"1791228354.431"`) {
		t.Errorf("time() = %s", res.Raw)
	}
}

// Prometheus rounds a request's time to the millisecond. Truncating instead evaluates some requests
// one millisecond early, which is enough to move an extrapolated rate.
func TestRequestTimesAreRoundedToTheMillisecond(t *testing.T) {
	for in, want := range map[string]int64{
		"1791228354.431":   1791228354431, // not representable; the nearest float is just below
		"1791228354.43095": 1791228354431,
		"1791228354.4304":  1791228354430,
		"1791228354":       1791228354000,
		"0.0005":           1,
	} {
		got, err := parseTime(in)
		if err != nil || got.UnixMilli() != want {
			t.Errorf("parseTime(%q) = %d ms (%v), want %d", in, got.UnixMilli(), err, want)
		}
	}
	if got, err := parseTime("2026-10-05T19:25:54.431Z"); err != nil || got.UnixMilli() != 1791228354431 {
		t.Errorf("RFC 3339: %v %v", got, err)
	}
	if _, err := parseTime("yesterday"); err == nil {
		t.Error("nonsense was accepted as a time")
	}
}

// Why the file format keeps staleness markers, shown on the engine itself.
func TestAStalenessMarkerEndsASeriesAtOnce(t *testing.T) {
	const scrape = 15_000
	ask := func(vs []float64, query string) int {
		ts := make([]int64, len(vs))
		for i := range ts {
			ts[i] = int64(i+1) * scrape
		}
		s := &Store{}
		if err := s.Add(map[string]string{"__name__": "gone", "pod": "old"}, ts, vs); err != nil {
			t.Fatal(err)
		}
		at := time.UnixMilli(ts[len(ts)-1] + 60_000) // one minute after the last sample: inside the 5m lookback
		res := call(t, NewAPI(s, at, func() time.Time { return at }), "/api/v1/query", url.Values{"query": {query}})
		if res.Status != "success" {
			t.Fatal(res.Error)
		}
		return len(res.Data.Result)
	}
	if n := ask([]float64{1, 2, 3, stale}, `gone`); n != 0 {
		t.Errorf("a series that ended with a marker still answered (%d results)", n)
	}
	if n := ask([]float64{1, 2, 3}, `gone`); n != 1 {
		t.Errorf("without the marker the series should linger for the lookback, got %d results", n)
	}
	if n := ask([]float64{1, 2, 3, stale}, `count_over_time(gone[10m]) == 3`); n != 1 {
		t.Error("a range selector counted the marker as a sample")
	}
}

func TestDiscoveryEndpoints(t *testing.T) {
	store, err := Load(s2Metrics)
	if err != nil {
		t.Fatal(err)
	}
	h := NewAPI(store, time.UnixMilli(s2Freeze), nil).Handler()
	get := func(path string, into any) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var env struct {
			Status string          `json:"status"`
			Data   json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Status != "success" {
			t.Fatalf("%s: %v %s", path, err, rec.Body)
		}
		json.Unmarshal(env.Data, into)
	}
	var clients []string
	get("/api/v1/label/client/values", &clients)
	if want := []string{"catalog-indexer", "email-renderer", "image-proxy", "web-frontend"}; !reflect.DeepEqual(clients, want) {
		t.Errorf("clients = %v", clients)
	}
	var names []string
	get("/api/v1/labels", &names)
	if len(names) == 0 || names[0] != "__name__" {
		t.Errorf("label names = %v", names)
	}
	// match[] narrows both lookups, as it does on a real Prometheus: HolmesGPT's toolset relies on it.
	var metricNames []string
	get("/api/v1/label/__name__/values?"+url.Values{"match[]": {`{client="catalog-indexer"}`}}.Encode(), &metricNames)
	if !reflect.DeepEqual(metricNames, []string{"thumb_requests_total"}) {
		t.Errorf("metric names with a client label = %v", metricNames)
	}
	var some, upOnly []string
	get("/api/v1/labels?"+url.Values{"match[]": {`up`, `thumb_inflight`}}.Encode(), &some)
	get("/api/v1/labels?"+url.Values{"match[]": {`up`}}.Encode(), &upOnly)
	if len(some) == 0 || len(some) >= len(names) || len(upOnly) > len(some) {
		t.Errorf("labels of two selectors = %v, of one = %v, of everything = %v", some, upOnly, names)
	}
	var info map[string]any
	get("/api/v1/status/buildinfo", &info)
	if info[FrozenAtField] != float64(s2Freeze)/1000 {
		t.Errorf("buildinfo = %v", info)
	}
	// What was not frozen is answered empty, in the shape a client expects, and what is not served at
	// all is refused in the API's own error shape rather than as a page a client cannot parse.
	var rules map[string][]any
	get("/api/v1/rules", &rules)
	if groups, ok := rules["groups"]; !ok || len(groups) != 0 {
		t.Errorf("rules = %v", rules)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/tsdb/snapshot", nil))
	var refusal struct{ Status, ErrorType string }
	if json.Unmarshal(rec.Body.Bytes(), &refusal); rec.Code != http.StatusNotFound || refusal.Status != "error" || refusal.ErrorType != "not_found" {
		t.Errorf("an endpoint that is not served answered %d %s", rec.Code, rec.Body)
	}
	// A POSTed form is how most clients send a long query.
	rec = httptest.NewRecorder()
	post := httptest.NewRequest(http.MethodPost, "/api/v1/query", strings.NewReader(url.Values{"query": {"count(up)"}}.Encode()))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, post)
	if !strings.Contains(rec.Body.String(), `"1"`) || rec.Code != http.StatusOK {
		t.Errorf("POST /api/v1/query answered %d %s", rec.Code, rec.Body)
	}
	var series []map[string]string
	get("/api/v1/series?"+url.Values{"match[]": {`thumb_requests_total{client="catalog-indexer"}`}}.Encode(), &series)
	if len(series) == 0 || series[0]["client"] != "catalog-indexer" {
		t.Errorf("series = %v", series)
	}
}

func TestExportReadsRawSamplesOverRemoteRead(t *testing.T) {
	at := time.UnixMilli(s2Freeze)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		raw, err := snappy.Decode(nil, body)
		var req prompb.ReadRequest
		if err != nil || req.Unmarshal(raw) != nil || r.URL.Path != "/api/v1/read" || len(req.Queries) != 1 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		q := req.Queries[0]
		if q.EndTimestampMs != s2Freeze || q.StartTimestampMs != s2Freeze-600_000 || len(q.Matchers) != 2 ||
			q.Matchers[0].Type != prompb.LabelMatcher_EQ || q.Matchers[0].Name != "job" || q.Matchers[0].Value != "thumb-api" ||
			q.Matchers[1].Type != prompb.LabelMatcher_EQ || q.Matchers[1].Name != "__name__" || q.Matchers[1].Value != "up" {
			http.Error(w, "unexpected query", http.StatusBadRequest)
			return
		}
		out, _ := (&prompb.ReadResponse{Results: []*prompb.QueryResult{{Timeseries: []*prompb.TimeSeries{{
			Labels:  []prompb.Label{{Name: "__name__", Value: "up"}, {Name: "job", Value: "thumb-api"}},
			Samples: []prompb.Sample{{Timestamp: 1000, Value: 1}, {Timestamp: 2000, Value: stale}},
		}}}}}).Marshal()
		w.Write(snappy.Encode(nil, out))
	}))
	defer srv.Close()

	store, err := Export(context.Background(), srv.Client(), srv.URL+"/", at, 10*time.Minute, `up{job="thumb-api"}`)
	if err != nil {
		t.Fatal(err)
	}
	if n, m := store.Size(); n != 1 || m != 2 {
		t.Fatalf("exported %d series and %d samples", n, m)
	}
	if last := store.series[0].samples[1]; !value.IsStaleNaN(last.F()) || last.T() != 2000 {
		t.Errorf("the staleness marker did not survive the export: %v at %d", last.F(), last.T())
	}
	if _, err := Export(context.Background(), srv.Client(), srv.URL, at, 10*time.Minute, `{job="other"}`); err == nil {
		t.Error("a refused read was reported as an empty store")
	}
	// This store was asked nothing but for samples and said nothing else, which is not a failure.
	if store.Source != (Source{}) {
		t.Errorf("a store that describes itself nowhere was described as %+v", store.Source)
	}
}

// What an answer depends on that is not a sample is asked for at the freeze: the version, and the
// interval a subquery without a step is evaluated at.
func TestExportAsksThePrometheusAboutItself(t *testing.T) {
	read, _ := (&prompb.ReadResponse{Results: []*prompb.QueryResult{{Timeseries: []*prompb.TimeSeries{
		{Labels: []prompb.Label{{Name: "__name__", Value: "up"}, {Name: "job", Value: "web"}}, Samples: []prompb.Sample{{Timestamp: 1000, Value: 1}}},
		{Labels: []prompb.Label{{Name: "__name__", Value: "up"}, {Name: "job", Value: "batch"}}, Samples: []prompb.Sample{{Timestamp: 1000, Value: 1}}},
	}}}}).Marshal()
	for _, c := range []struct {
		what, build, config, flags string
		want                       Source
	}{
		{"a Prometheus", `{"status":"success","data":{"version":"3.5.0","revision":"x"}}`,
			`{"status":"success","data":{"yaml":"global:\n  scrape_interval: 5s\n  evaluation_interval: 15s\nscrape_configs:\n- job_name: a\n  basic_auth:\n    password: <secret>\n"}}`,
			`{"status":"success","data":{"query.lookback-delta":"5m","query.timeout":"2m","enable-feature":""}}`,
			Source{Version: "3.5.0", EvaluationInterval: 15 * time.Second, LookbackDelta: 5 * time.Minute}},
		{"one that gives its version and no configuration", `{"status":"success","data":{"version":"2.53.1"}}`, ``, ``, Source{Version: "2.53.1"}},
		{"one whose configuration is not one", `{"status":"success","data":{"version":"3.5.0"}}`, `{"status":"success","data":{"yaml":"global: [not, a, map]"}}`, `{"status":"success","data":["not","a","map"]}`, Source{Version: "3.5.0"}},
		{"one whose interval is no duration", ``, `{"status":"success","data":{"yaml":"global:\n  evaluation_interval: often\n"}}`, `{"status":"success","data":{"query.lookback-delta":"far"}}`, Source{}},
		{"one set to look back two minutes", ``, ``, `{"status":"success","data":{"query.lookback-delta":"2m"}}`, Source{LookbackDelta: 2 * time.Minute}},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch answer := map[string]string{"/api/v1/status/buildinfo": c.build, "/api/v1/status/config": c.config, "/api/v1/status/flags": c.flags}[r.URL.Path]; {
			case r.URL.Path == "/api/v1/read":
				w.Write(snappy.Encode(nil, read))
			case answer != "":
				io.WriteString(w, answer)
			default:
				http.NotFound(w, r)
			}
		}))
		store, err := Export(context.Background(), srv.Client(), srv.URL, time.UnixMilli(2000), time.Minute)
		srv.Close()
		if err != nil || store.Source != c.want {
			t.Errorf("%s was described as %+v (%v), want %+v", c.what, store.Source, err, c.want)
		}
		// And its series are kept in the order it listed them, which is not the order of their labels.
		if err == nil && (len(store.series) != 2 || store.series[0].lset.Get("job") != "web") {
			t.Errorf("%s listed web before batch, and the store holds %v", c.what, store.series)
		}
	}
}

// A subquery that names no step is evaluated at the evaluation interval of the Prometheus the case
// was frozen from. With no one to ask, the engine panicked, and the client was told "EOF".
func TestASubqueryWithoutAStep(t *testing.T) {
	freeze := time.UnixMilli(s2Freeze)
	points := func(interval time.Duration) (int, string) {
		store, err := Load(s2Metrics)
		if err != nil {
			t.Fatal(err)
		}
		store.Source.EvaluationInterval = interval
		srv := httptest.NewServer(NewAPI(store, freeze, func() time.Time { return freeze }).Handler())
		defer srv.Close()
		resp, err := srv.Client().Get(srv.URL + "/api/v1/query?query=" + url.QueryEscape(`count_over_time(up[2m:])`))
		if err != nil {
			t.Fatalf("with an evaluation interval of %s the store did not answer: %v", interval, err)
		}
		defer resp.Body.Close()
		var res apiResponse
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil || res.Status != "success" || len(res.Data.Result) != 1 {
			t.Fatalf("with an evaluation interval of %s: %v %s %q", interval, err, res.Status, res.Error)
		}
		n, _ := strconv.Atoi(res.Data.Result[0].Value[1].(string))
		return n, res.Error
	}
	if n, _ := points(15 * time.Second); n != 8 {
		t.Errorf("two minutes at fifteen seconds are %d points, want 8", n)
	}
	if n, _ := points(0); n != 2 { // not known: Prometheus's default, one minute
		t.Errorf("two minutes at the default interval are %d points, want 2", n)
	}
}

// What goes wrong before the engine has a query to recover from is an answer too, not a closed connection.
func TestAPanicIsAnAnswer(t *testing.T) {
	rec := httptest.NewRecorder()
	answered(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("no one to ask") })).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/query", nil))
	var res apiResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || rec.Code != http.StatusInternalServerError || res.Status != "error" || res.Kind != "internal" || !strings.Contains(res.Error, "no one to ask") {
		t.Errorf("a panic was answered %d %s (%v)", rec.Code, rec.Body, err)
	}
	// And it is what stands before every path of the API, not something beside it: an API with no engine
	// panics as soon as it is asked a query.
	broken := &API{store: &Store{}, freeze: time.UnixMilli(s2Freeze), now: time.Now}
	if res := call(t, broken, "/api/v1/query", url.Values{"query": {"up"}}); res.Code != http.StatusInternalServerError || res.Kind != "internal" {
		t.Errorf("an API that panics answered %d %s %q", res.Code, res.Kind, res.Error)
	}
}

// What test/replay-diff found the first time it put promq to a Prometheus and to its frozen copy
// (2026-10-08): 42 of 546 queries answered otherwise, 12 refused in other words, and 55 that the
// frozen store itself answered two ways. The four tests below are the four reasons.

// A Prometheus has had the `@` modifier and negative offsets on since 2.33. An engine built without
// saying so refuses both, and a frozen case said "@ modifier is disabled" to a query its Prometheus answers.
func TestTheEngineIsTheOneAPrometheusRuns(t *testing.T) {
	store, err := Load(s2Metrics)
	if err != nil {
		t.Fatal(err)
	}
	freeze := time.UnixMilli(s2Freeze)
	api := NewAPI(store, freeze, func() time.Time { return freeze })
	for _, query := range []string{`up @ end()`, `up @ start()`, `up offset -1m`, `timestamp(up @ ` + seconds(s2Freeze-60000) + `)`} {
		if res := call(t, api, "/api/v1/query", url.Values{"query": {query}}); res.Status != "success" || len(res.Data.Result) != 1 {
			t.Errorf("%s: %s %q, %d results", query, res.Status, res.Error, len(res.Data.Result))
		}
	}
}

// A refusal is in Prometheus's words: an agent corrects its query by them.
func TestARefusalIsWordedAsPrometheusWordsIt(t *testing.T) {
	store, err := Load(s2Metrics)
	if err != nil {
		t.Fatal(err)
	}
	freeze := time.UnixMilli(s2Freeze)
	api := NewAPI(store, freeze, func() time.Time { return freeze })
	window := func(more ...string) url.Values {
		v := url.Values{"query": {"up"}, "start": {seconds(s2Freeze - 60000)}, "end": {seconds(s2Freeze)}, "step": {"15"}}
		for i := 0; i < len(more); i += 2 {
			v.Set(more[i], more[i+1])
		}
		return v
	}
	for _, c := range []struct {
		path   string
		params url.Values
		want   string
	}{
		{"/api/v1/query", url.Values{"query": {"sum("}}, `invalid parameter "query": 1:5: parse error: unclosed left parenthesis`},
		{"/api/v1/query", url.Values{"query": {"rate(up)"}}, `invalid parameter "query": 1:6: parse error: expected type range vector in call to function "rate", got instant vector`},
		{"/api/v1/query", url.Values{"query": {"up"}, "time": {"soon"}}, `invalid parameter "time": invalid time value for 'time': cannot parse "soon" to a valid timestamp`},
		{"/api/v1/series", url.Values{}, `no match[] parameter provided`},
		{"/api/v1/series", url.Values{"match[]": {"up{"}}, `invalid parameter "match[]": 1:4: parse error: unexpected end of input inside braces`},
		{"/api/v1/series", url.Values{"match[]": {`{job=~".*"}`}}, `invalid parameter "match[]": match[] must contain at least one non-empty matcher`},
		{"/api/v1/series", url.Values{"match[]": {"up"}, "start": {"yesterday"}}, `invalid parameter "start": invalid time value for 'start': cannot parse "yesterday" to a valid timestamp`},
		{"/api/v1/labels", url.Values{"match[]": {"up{"}}, `1:4: parse error: unexpected end of input inside braces`},
		{"/api/v1/labels", url.Values{"end": {"later"}}, `invalid parameter "end": invalid time value for 'end': cannot parse "later" to a valid timestamp`},
		{"/api/v1/label/job/values", url.Values{"match[]": {"{}"}}, `match[] must contain at least one non-empty matcher`},
		{"/api/v1/query_range", window("query", "sum("), `invalid parameter "query": 1:5: parse error: unclosed left parenthesis`},
		{"/api/v1/query_range", window("start", ""), `invalid parameter "start": cannot parse "" to a valid timestamp`},
		{"/api/v1/query_range", window("end", "later"), `invalid parameter "end": cannot parse "later" to a valid timestamp`},
		{"/api/v1/query_range", window("end", seconds(s2Freeze-120000)), `invalid parameter "end": end timestamp must not be before start time`},
		{"/api/v1/query_range", window("step", "often"), `invalid parameter "step": cannot parse "often" to a valid duration`},
		{"/api/v1/query_range", window("step", "0s"), `invalid parameter "step": zero or negative query resolution step widths are not accepted. Try a positive integer`},
		{"/api/v1/query_range", window("start", seconds(s2Freeze-30*86400000), "step", "1"), `exceeded maximum resolution of 11,000 points per timeseries. Try decreasing the query resolution (?step=XX)`},
		// What a query may be asked with besides its times: each is read, and refused if it is not what it should be.
		{"/api/v1/query", url.Values{"query": {"up"}, "limit": {"abc"}}, `invalid parameter "limit": strconv.Atoi: parsing "abc": invalid syntax`},
		{"/api/v1/query", url.Values{"query": {"up"}, "limit": {"-1"}}, `invalid parameter "limit": limit must be non-negative`},
		{"/api/v1/query", url.Values{"query": {"up"}, "timeout": {"abc"}}, `invalid parameter "timeout": cannot parse "abc" to a valid duration`},
		{"/api/v1/query", url.Values{"query": {"up"}, "lookback_delta": {"abc"}}, `error parsing lookback delta duration: cannot parse "abc" to a valid duration`},
		{"/api/v1/query_range", window("limit", "many"), `invalid parameter "limit": strconv.Atoi: parsing "many": invalid syntax`},
		{"/api/v1/query_range", window("step", "1e30"), `invalid parameter "step": cannot parse "1e30" to a valid duration. It overflows int64`},
		{"/api/v1/query_range", window("timeout", "soon"), `invalid parameter "timeout": cannot parse "soon" to a valid duration`},
		// Wrong in two ways, a request is told of the one Prometheus looks at first.
		{"/api/v1/query_range", window("start", "", "step", "0s", "query", "sum("), `invalid parameter "start": cannot parse "" to a valid timestamp`},
		{"/api/v1/query_range", window("step", "0s", "query", "sum("), `invalid parameter "step": zero or negative query resolution step widths are not accepted. Try a positive integer`},
		{"/api/v1/query_range", window("limit", "x", "start", ""), `invalid parameter "limit": strconv.Atoi: parsing "x": invalid syntax`},
		{"/api/v1/query", url.Values{"query": {"sum("}, "time": {"soon"}, "timeout": {"x"}}, `invalid parameter "time": invalid time value for 'time': cannot parse "soon" to a valid timestamp`},
		{"/api/v1/query", url.Values{"query": {"sum("}, "timeout": {"x"}, "lookback_delta": {"y"}}, `invalid parameter "timeout": cannot parse "x" to a valid duration`},
		{"/api/v1/query", url.Values{"query": {"sum("}, "lookback_delta": {"y"}}, `error parsing lookback delta duration: cannot parse "y" to a valid duration`},
		{"/api/v1/query_range", window("end", "later", "step", "0s"), `invalid parameter "end": cannot parse "later" to a valid timestamp`},
		{"/api/v1/query_range", window("end", seconds(s2Freeze-120000), "step", "often"), `invalid parameter "end": end timestamp must not be before start time`},
		{"/api/v1/query_range", window("step", "often", "timeout", "x"), `invalid parameter "step": cannot parse "often" to a valid duration`},
		{"/api/v1/query_range", window("start", seconds(s2Freeze-30*86400000), "step", "1", "timeout", "x"), `exceeded maximum resolution of 11,000 points per timeseries. Try decreasing the query resolution (?step=XX)`},
		{"/api/v1/query_range", window("timeout", "x", "query", "sum("), `invalid parameter "timeout": cannot parse "x" to a valid duration`},
		{"/api/v1/query_range", window("lookback_delta", "y", "query", "sum("), `error parsing lookback delta duration: cannot parse "y" to a valid duration`},
		// The names, the values and the series are limited and refused as a query's answer is.
		{"/api/v1/labels", url.Values{"limit": {"some"}, "start": {"x"}}, `invalid parameter "limit": strconv.Atoi: parsing "some": invalid syntax`},
		{"/api/v1/label/job/values", url.Values{"limit": {"-2"}}, `invalid parameter "limit": limit must be non-negative`},
		{"/api/v1/series", url.Values{"match[]": {"up{"}, "limit": {"some"}}, `invalid parameter "limit": strconv.Atoi: parsing "some": invalid syntax`},
		{"/api/v1/series", url.Values{"limit": {"some"}}, `no match[] parameter provided`},
		{"/api/v1/series", url.Values{"match[]": {"up{"}, "start": {"x"}}, `invalid parameter "start": invalid time value for 'start': cannot parse "x" to a valid timestamp`},
	} {
		if res := call(t, api, c.path, c.params); res.Status != "error" || res.Error != c.want || res.Code != http.StatusBadRequest || res.Kind != "bad_data" {
			t.Errorf("%s %v\n got %d %s %s %q\nwant %q", c.path, c.params, res.Code, res.Status, res.Kind, res.Error, c.want)
		}
	}
	// Of several selectors each is read before any is weighed: one that does not parse is told of before one that matches everything.
	if res := call(t, api, "/api/v1/series", url.Values{"match[]": {"{}", "up{"}}); res.Code != http.StatusBadRequest || !strings.HasPrefix(res.Error, `invalid parameter "match[]": 1:4: parse error`) {
		t.Errorf("a selector that matches everything and one that does not parse: %d %q", res.Code, res.Error)
	}
	// A label's name has no slash in it: a path with one more is a path a Prometheus does not have.
	if res := call(t, api, "/api/v1/label/a/b/values", url.Values{}); res.Code != http.StatusNotFound || res.Status != "error" {
		t.Errorf("/api/v1/label/a/b/values: %d %s %q", res.Code, res.Status, res.Error)
	}
	// A query that parses and cannot be evaluated is not a bad request, and one that ran out of time is neither.
	if res := call(t, api, "/api/v1/query", url.Values{"query": {`thumb_requests_total + on (job) thumb_requests_total`}}); res.Code != http.StatusUnprocessableEntity || res.Kind != "execution" || res.Error == "" {
		t.Errorf("a query that cannot be evaluated: %d %s %q", res.Code, res.Kind, res.Error)
	}
	if res := call(t, api, "/api/v1/query", url.Values{"query": {`sum(rate(thumb_requests_total[5m]))`}, "timeout": {"0.000000001"}}); res.Code != http.StatusServiceUnavailable || res.Kind != "timeout" {
		t.Errorf("a query given a nanosecond: %d %s %q", res.Code, res.Kind, res.Error)
	}
	// A query whose asker went away is called off, and is said to have been.
	gone, leave := context.WithCancel(context.Background())
	leave()
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/query?query=sum(rate(thumb_requests_total%5B5m%5D))", nil).WithContext(gone))
	var left apiResponse
	if json.Unmarshal(rec.Body.Bytes(), &left); rec.Code != 499 || left.Kind != "canceled" {
		t.Errorf("a query whose asker had gone: %d %s %q", rec.Code, left.Kind, left.Error)
	}
	// Only what a Prometheus's API answers is answered: a GET or a POST, and yes to a browser that asks first.
	for method, want := range map[string]struct {
		code int
		body string
	}{http.MethodGet: {200, `"status":"success"`}, http.MethodPost: {200, `"status":"success"`}, http.MethodOptions: {204, ""}, http.MethodDelete: {405, "Method Not Allowed\n"}, http.MethodPut: {405, "Method Not Allowed\n"}} {
		rec := httptest.NewRecorder()
		api.Handler().ServeHTTP(rec, httptest.NewRequest(method, "/api/v1/query?query=up", nil))
		if body := rec.Body.String(); rec.Code != want.code || (want.code == 200) != strings.Contains(body, `"status":"success"`) || want.code != 200 && body != want.body {
			t.Errorf("%s /api/v1/query: %d %q, want %d %q", method, rec.Code, body, want.code, want.body)
		}
	}
}

// What a query may be asked with changes its answer: no more series than a limit, and a sample no
// older than the lookback. A request with either used to be answered as if it had neither.
func TestAQueryIsAnsweredAsItWasAsked(t *testing.T) {
	store, err := Load(s2Metrics)
	if err != nil {
		t.Fatal(err)
	}
	freeze := time.UnixMilli(s2Freeze)
	api := NewAPI(store, freeze, func() time.Time { return freeze })
	warnings := func(res apiResponse) []string {
		var body struct{ Warnings []string }
		json.Unmarshal(res.Raw, &body)
		return body.Warnings
	}
	if res := call(t, api, "/api/v1/query", url.Values{"query": {"thumb_requests_total"}}); len(res.Data.Result) != 8 || len(warnings(res)) != 0 {
		t.Fatalf("with no limit: %d series, warnings %q", len(res.Data.Result), warnings(res))
	}
	for path, params := range map[string]url.Values{
		"/api/v1/query":       {"query": {"thumb_requests_total"}, "limit": {"2"}},
		"/api/v1/query_range": {"query": {"thumb_requests_total"}, "limit": {"2"}, "start": {seconds(s2Freeze - 60000)}, "end": {seconds(s2Freeze)}, "step": {"30"}},
	} {
		if res := call(t, api, path, params); len(res.Data.Result) != 2 || !reflect.DeepEqual(warnings(res), []string{"results truncated due to limit"}) {
			t.Errorf("%s with a limit of 2: %d series, warnings %q", path, len(res.Data.Result), warnings(res))
		}
	}
	if res := call(t, api, "/api/v1/query", url.Values{"query": {"thumb_requests_total"}, "limit": {"8"}}); len(res.Data.Result) != 8 || len(warnings(res)) != 0 {
		t.Errorf("with a limit of all there are: %d series, warnings %q", len(res.Data.Result), warnings(res))
	}
	// The first of them, as they came, not any two.
	all := call(t, api, "/api/v1/query", url.Values{"query": {"thumb_requests_total"}}).Data.Result
	if two := call(t, api, "/api/v1/query", url.Values{"query": {"thumb_requests_total"}, "limit": {"2"}}).Data.Result; !reflect.DeepEqual(two, all[:2]) {
		t.Errorf("a limit of 2 kept %v, want the first two, %v", two, all[:2])
	}
	// Names, values and series likewise.
	listed := func(path string, params url.Values) (data []any, said []string) {
		var body struct {
			Data     []any
			Warnings []string
		}
		json.Unmarshal(call(t, api, path, params).Raw, &body)
		return body.Data, body.Warnings
	}
	for path, params := range map[string]url.Values{"/api/v1/labels": {}, "/api/v1/label/client/values": {}, "/api/v1/series": {"match[]": {"thumb_requests_total"}}} {
		whole, said := listed(path, params)
		if len(whole) < 3 || len(said) != 0 {
			t.Fatalf("%s with no limit: %d of them, warnings %q", path, len(whole), said)
		}
		params.Set("limit", "2")
		if some, said := listed(path, params); !reflect.DeepEqual(some, whole[:2]) || !reflect.DeepEqual(said, []string{"results truncated due to limit"}) {
			t.Errorf("%s with a limit of 2: %v, warnings %q; want the first two of %v", path, some, said, whole)
		}
		params.Set("limit", strconv.Itoa(len(whole)))
		if some, said := listed(path, params); len(some) != len(whole) || len(said) != 0 {
			t.Errorf("%s with a limit of all there are: %d of them, warnings %q", path, len(some), said)
		}
	}
	// A series that two selectors match is one series.
	if twice, _ := listed("/api/v1/series", url.Values{"match[]": {"thumb_requests_total", `{code="200"}`}}); len(twice) != 8 {
		t.Errorf("the series of two selectors that overlap: %d, want 8", len(twice))
	}
	// A window that begins and ends at one instant is that instant, and is not refused: a recorded run asked for one.
	if res := call(t, api, "/api/v1/query_range", url.Values{"query": {"up"}, "start": {seconds(s2Freeze - 60000)}, "end": {seconds(s2Freeze - 60000)}, "step": {"15"}}); res.Status != "success" || len(res.Data.Result) != 1 || len(res.Data.Result[0].Values) != 1 {
		t.Errorf("a window of one instant: %s %q %v", res.Status, res.Error, res.Data.Result)
	}
	// And a window looks back as far as it is asked to, as an instant does.
	for lookback, want := range map[string]int{"": 1, "1ms": 0} {
		res := call(t, api, "/api/v1/query_range", url.Values{"query": {"up"}, "start": {seconds(s2Freeze)}, "end": {seconds(s2Freeze)}, "step": {"15"}, "lookback_delta": {lookback}})
		if res.Status != "success" || len(res.Data.Result) != want {
			t.Errorf("a window of up, looked back for %q: %s, %d series, want %d", lookback, res.Status, len(res.Data.Result), want)
		}
	}
	// The newest sample is 3.9 seconds before the freeze: looked back for a second, there is none.
	for lookback, want := range map[string]int{"": 1, "10s": 1, "1": 0, "1ms": 0} {
		if res := call(t, api, "/api/v1/query", url.Values{"query": {"up"}, "lookback_delta": {lookback}}); res.Status != "success" || len(res.Data.Result) != want {
			t.Errorf("up, looked back for %q: %s, %d series, want %d", lookback, res.Status, len(res.Data.Result), want)
		}
	}
	// And a case whose Prometheus was set to look back another distance is replayed with that one.
	short, _ := Load(s2Metrics)
	short.Source.LookbackDelta = time.Second
	if res := call(t, NewAPI(short, freeze, func() time.Time { return freeze }), "/api/v1/query", url.Values{"query": {"up"}}); len(res.Data.Result) != 0 {
		t.Errorf("a case from a Prometheus that looks back one second answered up with %d series", len(res.Data.Result))
	}
	// The two instants Prometheus writes for "from the beginning" and "to the end" are times, though their years have five digits.
	for _, when := range []string{minTime.Format(time.RFC3339Nano), maxTime.Format(time.RFC3339Nano)} {
		if res := call(t, api, "/api/v1/labels", url.Values{"start": {when}, "end": {when}}); res.Status != "success" {
			t.Errorf("labels from %s: %s %q", when, res.Status, res.Error)
		}
	}
}

// A caller that asks about now names the instant its own clock showed a moment before the request
// arrived. Moved back by the server's clock, that landed some milliseconds before the freeze, a
// different few each time, and the same range of `rate` was another number in its fifth digit.
func TestARequestAboutNowIsAboutTheFreezeExactly(t *testing.T) {
	store, err := Load(s2Metrics)
	if err != nil {
		t.Fatal(err)
	}
	freeze := time.UnixMilli(s2Freeze)
	server := freeze.Add(3 * time.Hour)
	api := NewAPI(store, freeze, func() time.Time { return server })
	const query = `rate(thumb_requests_total{client="catalog-indexer",code="200"}[1m])`
	asked := func(end time.Time) (lastStamp float64, values []any) {
		res := call(t, api, "/api/v1/query_range", url.Values{"query": {query}, "start": {seconds(end.Add(-5 * time.Minute).UnixMilli())}, "end": {seconds(end.UnixMilli())}, "step": {"30"}})
		if res.Status != "success" || len(res.Data.Result) != 1 {
			t.Fatalf("a window ending %s: %s %q, %d series", end.Sub(server), res.Status, res.Error, len(res.Data.Result))
		}
		points := res.Data.Result[0].Values
		for _, p := range points {
			values = append(values, p[1])
		}
		return points[len(points)-1][0].(float64), values
	}
	atTheFreeze, want := asked(freeze) // named: taken as written
	if atTheFreeze != float64(s2Freeze)/1000 {
		t.Fatalf("a window named to end at the freeze ended at %v", atTheFreeze)
	}
	for _, early := range []time.Duration{0, 7 * time.Millisecond, 400 * time.Millisecond, -3 * time.Millisecond, nowSlack} {
		if last, got := asked(server.Add(-early)); last != atTheFreeze || !reflect.DeepEqual(got, want) {
			t.Errorf("a window ending %s before the server's now ended at %v, with %v; want the freeze, %v, with %v", early, last, got, atTheFreeze, want)
		}
	}
	// "Five minutes ago" is not now: it stays five minutes before the freeze.
	if last, _ := asked(server.Add(-5 * time.Minute)); last != float64(s2Freeze-300000)/1000 {
		t.Errorf("a window ending five minutes before now ended at %v, want five minutes before the freeze", last)
	}
	// And an instant: a client that sends its own now gets the freeze, as one that sends nothing does.
	for _, early := range []time.Duration{0, 7 * time.Millisecond, -3 * time.Millisecond} {
		res := call(t, api, "/api/v1/query", url.Values{"query": {"time()"}, "time": {seconds(server.Add(-early).UnixMilli())}})
		var scalar struct {
			Data struct{ Result [2]any }
		}
		json.Unmarshal(res.Raw, &scalar)
		if got := fmt.Sprint(scalar.Data.Result[1]); got != seconds(s2Freeze) {
			t.Errorf("time() asked %s before the server's now is %s, want the freeze, %s", early, got, seconds(s2Freeze))
		}
	}
}

// A time past the freeze is not in the case, and a request that reaches there is taken for one of
// three things by a rule that does not change with the age of the replay; what it was taken for is
// said beside the answer. Four requests of round 38's recorded runs reached past the freeze, each
// some minutes after its case was frozen: they are here as they were asked, at the age the replay then
// had (test/fixtures/case-runs/2026-10-07-round38/s2-periodic-saturation, the frozen runs of
// HolmesGPT 3, 4, 5 and 6: steps 8, 10, 6 and 13).
func TestARequestPastTheFreeze(t *testing.T) {
	store, err := Load(s2Metrics)
	if err != nil {
		t.Fatal(err)
	}
	sec := func(at time.Time) float64 { return float64(at.UnixMilli()) / 1000 }
	type answer struct {
		first, last float64
		points      int
		kind        string
		said        []string
	}
	window := func(api *API, query string, start, end time.Time, step string) answer {
		res := call(t, api, "/api/v1/query_range", url.Values{"query": {query}, "start": {seconds(start.UnixMilli())}, "end": {seconds(end.UnixMilli())}, "step": {step}})
		if res.Status != "success" {
			t.Fatalf("a window %s..%s: %s %q", start.UTC().Format("15:04:05.000"), end.UTC().Format("15:04:05.000"), res.Status, res.Error)
		}
		var body struct{ Infos []string }
		json.Unmarshal(res.Raw, &body)
		out := answer{kind: res.Data.ResultType, said: body.Infos}
		if len(res.Data.Result) > 0 {
			values := res.Data.Result[0].Values
			out.first, out.last, out.points = values[0][0].(float64), values[len(values)-1][0].(float64), len(values)
		}
		return out
	}
	remarked := func(said []string, words string) bool {
		return len(said) == 1 && strings.HasPrefix(said[0], remarkPrefix) && strings.Contains(said[0], words)
	}
	const cut, moved = "taken as written and cut there", "taken to be about the present, and moved back by "

	frozen := time.UnixMilli(1791366156177) // 09:42:36.177
	clock := func(hms string) time.Time {
		at, err := time.Parse("2006-01-02T15:04:05.000Z", "2026-10-07T"+hms+"Z")
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	for _, c := range []struct {
		start, end, step, wantLast string
		age                        time.Duration
	}{
		// The incident's own time to the next whole minute, and three windows to a now that was a minute or
		// two stale. Each begins before the freeze: each is as it was asked, up to its last step that is not
		// past the freeze. (Moved back by the age of the replay, as they were, the first ended at 09:35:44.)
		{"09:18:00.000", "09:43:00.000", "30", "09:42:30.000", 435568 * time.Millisecond},
		{"09:22:45.300", "09:52:45.300", "15", "09:42:30.300", 732787 * time.Millisecond},
		{"08:55:20.000", "09:55:20.000", "60", "09:42:20.000", 819340 * time.Millisecond},
		{"08:57:00.000", "09:57:00.000", "60", "09:42:00.000", 954428 * time.Millisecond},
	} {
		server := frozen.Add(c.age)
		got := window(NewAPI(store, frozen, func() time.Time { return server }), `time()`, clock(c.start), clock(c.end), c.step)
		if got.first != sec(clock(c.start)) || got.last != sec(clock(c.wantLast)) || !remarked(got.said, cut) || !strings.Contains(got.said[0], "it ends at 2026-10-07T09:42:36.177Z") {
			at := func(f float64) string { return time.UnixMilli(int64(f * 1000)).UTC().Format("15:04:05.000") }
			t.Errorf("asked %s..%s, %s after the freeze: evaluated %s..%s and said %q; want %s..%s, and that it was cut", c.start, c.end, c.age, at(got.first), at(got.last), got.said, c.start, c.wantLast)
		}
	}

	// What a request is taken for does not go by how old the replay is: five minutes, a quarter of an
	// hour, an hour, a day and a month after the freeze, the same reading.
	freeze := time.UnixMilli(s2Freeze)
	const traffic = `sum(rate(thumb_requests_total[1m]))`
	for _, age := range []time.Duration{5 * time.Minute, 15 * time.Minute, time.Hour, 24 * time.Hour, 31 * 24 * time.Hour} {
		server := freeze.Add(age)
		api := NewAPI(store, freeze, func() time.Time { return server })
		// About now, by a clock a little behind this one or a little ahead of it: onto the freeze, the
		// whole window, and nothing said — this is what promq asks.
		for _, off := range []time.Duration{0, 1900 * time.Millisecond, -1900 * time.Millisecond} {
			if got := window(api, traffic, server.Add(-off-10*time.Minute), server.Add(-off), "30"); got.first != sec(freeze.Add(-10*time.Minute)) || got.last != sec(freeze) || len(got.said) != 0 {
				t.Errorf("%s after the freeze, the ten minutes to %s before now ran %v..%v and said %q; want the ten minutes before the freeze and nothing said", age, off, got.first, got.last, got.said)
			}
		}
		// The incident's own time, overshot — by seconds, by most of an hour, by most of a day: as written, cut.
		for _, c := range []struct{ back, over time.Duration }{{10 * time.Minute, 24 * time.Second}, {8 * time.Minute, 52 * time.Minute}, {10 * time.Hour, 14 * time.Hour}} {
			start := freeze.Add(-c.back).Truncate(time.Minute)
			if got := window(api, `time()`, start, freeze.Add(c.over), "60"); got.first != sec(start) || got.last > sec(freeze) || got.last <= sec(freeze.Add(-time.Minute)) || !remarked(got.said, cut) {
				t.Errorf("%s after the freeze, a window from %s before it to %s after ran %v..%v and said %q; want it from %v to the freeze, %v, and cut", age, c.back, c.over, got.first, got.last, got.said, sec(start), sec(freeze))
			}
		}
		// Cut at the freeze and not a step after it, however fine the steps.
		if got := window(api, `time()`, freeze.Add(-10*time.Second), freeze.Add(10*time.Second), "1"); got.last != sec(freeze) || got.points != 11 {
			t.Errorf("%s after the freeze, twenty seconds about it in steps of one ran to %v in %d points; want to the freeze, %v, in 11", age, got.last, got.points, sec(freeze))
		}
		// And what is evaluated of it is the incident: the traffic at its end is what the freeze says, not a rate run out of samples.
		atFreeze := call(t, api, "/api/v1/query", url.Values{"query": {traffic}}).Data.Result[0].Value[1]
		res := call(t, api, "/api/v1/query_range", url.Values{"query": {traffic}, "start": {seconds(freeze.Add(-5 * time.Minute).UnixMilli())}, "end": {seconds(freeze.Add(4 * time.Minute).UnixMilli())}, "step": {"60"}})
		if values := res.Data.Result[0].Values; len(values) != 6 || values[5][1] != atFreeze {
			t.Errorf("%s after the freeze, a window that runs four minutes past it has %d points, the last %v; want six, the last what the freeze says, %v", age, len(values), values[len(values)-1], atFreeze)
		}
		// A present the case does not have: a window that begins after the freeze, moved back by the age of the replay, whole.
		if age > 20*time.Minute {
			for _, ago := range []time.Duration{10 * time.Minute, 2100 * time.Millisecond} {
				got := window(api, `time()`, server.Add(-ago-5*time.Minute), server.Add(-ago), "30")
				if got.first != sec(freeze.Add(-ago-5*time.Minute)) || got.last != sec(freeze.Add(-ago)) || !remarked(got.said, moved+age.String()) {
					t.Errorf("%s after the freeze, the five minutes to %s ago ran %v..%v and said %q; want them before the freeze, and that they were moved", age, ago, got.first, got.last, got.said)
				}
			}
		}
		// An instant past the freeze likewise, whatever the age: "five minutes ago" is five minutes
		// before the freeze when the replay is six minutes old as when it is a month old.
		for _, ago := range []time.Duration{4 * time.Minute, 5 * time.Minute, 3 * time.Second} {
			if ago >= age {
				continue
			}
			res := call(t, api, "/api/v1/query", url.Values{"query": {`timestamp(up)`}, "time": {seconds(server.Add(-ago).UnixMilli())}})
			var body struct{ Infos []string }
			json.Unmarshal(res.Raw, &body)
			// up was last scraped 3.9s before the freeze: asked about an instant, it is stamped with its own sample's time, which says which instant was asked.
			want := sec(freeze.Add(-ago))
			if len(res.Data.Result) != 1 || res.Data.Result[0].Value[0].(float64) != want || !remarked(body.Infos, moved+age.String()) {
				t.Errorf("%s after the freeze, %s ago: %v, said %q; want it evaluated at %v and said to have been moved", age, ago, res.Data.Result, body.Infos, want)
			}
		}
		// A time to come is past the freeze however it is moved: an instant is answered at the freeze, and a
		// window has no step to evaluate — an empty answer of a window's kind, to a query a window can ask.
		var clockOf struct {
			Data struct{ Result [2]any }
		}
		json.Unmarshal(call(t, api, "/api/v1/query", url.Values{"query": {`time()`}, "time": {seconds(server.Add(time.Hour).UnixMilli())}}).Raw, &clockOf)
		if got := fmt.Sprint(clockOf.Data.Result[1]); got != seconds(s2Freeze) {
			t.Errorf("%s after the freeze, time() an hour from now is %s, want the freeze, %s", age, got, seconds(s2Freeze))
		}
		if got := window(api, traffic, server.Add(time.Minute), server.Add(time.Hour), "30"); got.points != 0 || got.kind != "matrix" || !remarked(got.said, moved) {
			t.Errorf("%s after the freeze, a window of the hour to come: %d points of kind %q, said %q", age, got.points, got.kind, got.said)
		}
		if got := window(api, traffic, server.Add(-10*time.Minute), server.Add(time.Hour), "30"); age > 20*time.Minute && (got.first != sec(freeze.Add(-10*time.Minute)) || got.last != sec(freeze)) {
			t.Errorf("%s after the freeze, a window from ten minutes ago to an hour from now ran %v..%v; want the ten minutes before the freeze", age, got.first, got.last)
		}
		for query, want := range map[string]string{"sum(": `invalid parameter "query": 1:5: parse error`, "thumb_inflight[5m]": `invalid parameter "query": invalid expression type "range vector" for range query`, `"a string"`: `invalid parameter "query": invalid expression type "string" for range query`} {
			res := call(t, api, "/api/v1/query_range", url.Values{"query": {query}, "start": {seconds(server.Add(time.Minute).UnixMilli())}, "end": {seconds(server.Add(time.Hour).UnixMilli())}, "step": {"30"}})
			if res.Code != http.StatusBadRequest || !strings.HasPrefix(res.Error, want) {
				t.Errorf("%s after the freeze, %s over a window to come: %d %q; want it refused as a window's query is", age, query, res.Code, res.Error)
			}
		}
		// And a request that does not reach past the freeze is as written, with nothing said.
		if got := window(api, traffic, freeze.Add(-10*time.Minute), freeze, "30"); got.first != sec(freeze.Add(-10*time.Minute)) || got.last != sec(freeze) || len(got.said) != 0 {
			t.Errorf("%s after the freeze, the ten minutes up to it ran %v..%v and said %q", age, got.first, got.last, got.said)
		}
	}

	// What the rule gets wrong it gets wrong at any age, and says: "from twenty minutes ago to ten
	// minutes ago", asked of a replay fifteen minutes old, begins before the freeze and is taken as written.
	server := freeze.Add(15 * time.Minute)
	api := NewAPI(store, freeze, func() time.Time { return server })
	if got := window(api, traffic, server.Add(-20*time.Minute), server.Add(-10*time.Minute), "30"); got.first != sec(freeze.Add(-5*time.Minute)) || got.last != sec(freeze) || !remarked(got.said, cut) {
		t.Errorf("a window from twenty to ten minutes ago, fifteen minutes after the freeze: %v..%v, said %q", got.first, got.last, got.said)
	}
	// promq prints what was said, for whoever asked to read; and prints nothing where nothing was.
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()
	var out bytes.Buffer
	if err := Promq(&out, srv.Client(), srv.URL, []string{`count(up)`, "--at", seconds(server.Add(-3 * time.Minute).UnixMilli())}, server); err != nil ||
		out.String() != "{} 1\n(frozen case: it ends at 2026-10-05T19:25:54.431Z; this was taken to be about the present, and moved back by 15m0s — name a time at or before the end to ask about that time)\n" {
		t.Errorf("promq about three minutes ago printed %q (%v)", out.String(), err)
	}
	out.Reset()
	if err := Promq(&out, srv.Client(), srv.URL, []string{`count(up)`, "--range", "1m", "--step", "30s"}, server); err != nil || strings.Contains(out.String(), "frozen case") {
		t.Errorf("promq about the last minute printed %q (%v)", out.String(), err)
	}
}

// PromQL leaves the order of an instant vector open, and each store has its own. What promq prints
// does not depend on it.
func TestPromqPrintsSeriesInAnOrderOfItsOwn(t *testing.T) {
	answer := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, answer) }))
	defer srv.Close()
	vector := func(series ...string) string {
		return `{"status":"success","data":{"resultType":"vector","result":[` + strings.Join(series, ",") + `]}}`
	}
	one := func(name, client, value string) string {
		return `{"metric":{"__name__":"` + name + `","client":"` + client + `"},"value":[1,"` + value + `"]}`
	}
	asCreated := vector(one("x", "web", "5"), one("x", "batch", "9"), one("a", "web", "5"), one("x", "mail", "5"))
	for _, c := range []struct{ what, query, answer, want string }{
		{"a selector", `{client=~".+"}`, asCreated, "a{client=web} 5\nx{client=batch} 9\nx{client=mail} 5\nx{client=web} 5\n"},
		{"an aggregation", `sum by (client) (x)`, asCreated, "a{client=web} 5\nx{client=batch} 9\nx{client=mail} 5\nx{client=web} 5\n"},
		{"sorted by value: equals in the order of their labels, and nothing else moved", `sort_desc(x)`,
			vector(one("x", "batch", "9"), one("x", "web", "5"), one("a", "web", "5"), one("x", "mail", "5"), one("x", "cron", "1")),
			"x{client=batch} 9\na{client=web} 5\nx{client=mail} 5\nx{client=web} 5\nx{client=cron} 1\n"},
		{"sorted inside an expression is still sorted", `100 * sort(x)`, vector(one("x", "web", "500"), one("x", "batch", "900")), "x{client=web} 500\nx{client=batch} 900\n"},
		{"and with the number on the other side, equals left where they came", `sort_desc(x) * 100`, vector(one("x", "web", "500"), one("x", "batch", "500")), "x{client=web} 500\nx{client=batch} 500\n"},
		{"the k largest", `topk(2, x)`, vector(one("x", "web", "9"), one("x", "batch", "9")), "x{client=batch} 9\nx{client=web} 9\n"},
		{"the k largest, which are not in the order of their labels", `topk(2, x)`, vector(one("x", "web", "9"), one("x", "batch", "5")), "x{client=web} 9\nx{client=batch} 5\n"},
		{"the k smallest, in brackets", `(bottomk by (job) (2, x))`, vector(one("x", "web", "1"), one("x", "batch", "5")), "x{client=web} 1\nx{client=batch} 5\n"},
		{"sorted by a label: as it came, equals and all", `sort_by_label(x, "client")`, vector(one("x", "web", "5"), one("x", "batch", "5")), "x{client=web} 5\nx{client=batch} 5\n"},
		// Each series of something sorted is still in that order, and no longer has the values it was sorted
		// by: equals are not put right, which would be to sort it again by the wrong thing.
		{"rounded after it was sorted", `round(sort_desc(x))`, vector(one("x", "web", "5"), one("x", "batch", "5"), one("x", "cron", "1")), "x{client=web} 5\nx{client=batch} 5\nx{client=cron} 1\n"},
		{"the time of each of a sorted vector", `timestamp(sort_desc(x))`, vector(one("x", "web", "60"), one("x", "batch", "60")), "x{client=web} 60\nx{client=batch} 60\n"},
		{"a sorted vector, negated", `-sort(x)`, vector(one("x", "web", "-5"), one("x", "batch", "-5")), "x{client=web} -5\nx{client=batch} -5\n"},
		// An aggregation over something sorted has thrown the order away: by label, like any other.
		{"summed after the k largest were taken", `sum by (client) (topk(5, x))`, vector(one("x", "web", "9"), one("x", "batch", "5")), "x{client=batch} 5\nx{client=web} 9\n"},
		{"counted after it was sorted", `count by (client) (sort_desc(x))`, vector(one("x", "web", "1"), one("x", "batch", "1")), "x{client=batch} 1\nx{client=web} 1\n"},
		{"a query that does not parse and says sort", `sort_desc(x`, vector(one("x", "web", "500"), one("x", "batch", "900")), "x{client=web} 500\nx{client=batch} 900\n"},
		{"a range, which the engine has sorted", `x`, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"client":"web"},"values":[[1,"5"]]},{"metric":{"client":"batch"},"values":[[1,"9"]]}]}}`,
			"{client=web} 00:00:01=5\n{client=batch} 00:00:01=9\n"},
	} {
		answer = c.answer
		var out bytes.Buffer
		if err := Promq(&out, srv.Client(), srv.URL, []string{c.query}, time.Now()); err != nil || out.String() != c.want {
			t.Errorf("%s, promq %q printed\n%s(%v)\nwant\n%s", c.what, c.query, out.String(), err, c.want)
		}
	}
	// What a case is checked with goes by the same order: an aggregation by label, whatever order the
	// store holds its series in, and a sorted one as the engine sorted it.
	held := &Store{}
	for _, c := range []struct {
		client string
		v      float64
	}{{"web", 5}, {"batch", 9}, {"mail", 1}} {
		held.Add(map[string]string{"__name__": "x", "client": c.client}, []int64{50_000}, []float64{c.v})
	}
	check := NewAPI(held, time.UnixMilli(60_000), nil)
	for query, want := range map[string]string{
		`sum by (client) (x)`: "{client=batch} 9\n{client=mail} 1\n{client=web} 5\n",
		`sort_desc(x)`:        "x{client=batch} 9\nx{client=web} 5\nx{client=mail} 1\n",
		`x`:                   "x{client=batch} 9\nx{client=mail} 1\nx{client=web} 5\n",
	} {
		if got, err := check.Text(context.Background(), query); err != nil || got != want {
			t.Errorf("the text a case is checked with, for %s:\n%s(%v)\nwant\n%s", query, got, err, want)
		}
	}

	// More series than are printed: the same ones, whatever order the store had them in.
	var series []string
	for i := 99; i >= 0; i-- {
		series = append(series, one("x", fmt.Sprintf("c%02d", i), "1"))
	}
	answer = vector(series...)
	var out bytes.Buffer
	Promq(&out, srv.Client(), srv.URL, []string{`x`}, time.Now())
	if lines := strings.Split(out.String(), "\n"); len(lines) != maxSeries+2 || lines[0] != "x{client=c00} 1" || lines[maxSeries-1] != fmt.Sprintf("x{client=c%02d} 1", maxSeries-1) || !strings.HasPrefix(lines[maxSeries], "(40 more series") {
		t.Errorf("a hundred series printed %d lines, from %q to %q, then %q", len(lines), lines[0], lines[maxSeries-1], lines[maxSeries])
	}
}

// An engine goes by the order its store hands it the series in, and a Prometheus hands them over as
// its head created them. A case keeps the order it was given: through its file, to the engine, and
// out of the API. Sorted by label, as they used to be kept, `topk` over equals kept other series than
// the Prometheus had.
func TestAStoreKeepsTheOrderItWasGiven(t *testing.T) {
	at := time.UnixMilli(60_000)
	build := func(clients ...string) *Store {
		s := &Store{}
		for _, c := range clients {
			if err := s.Add(map[string]string{"__name__": "x", "client": c}, []int64{50_000}, []float64{5}); err != nil {
				t.Fatal(err)
			}
		}
		return s
	}
	clients := func(res apiResponse) (out []string) {
		for _, r := range res.Data.Result {
			out = append(out, r.Metric["client"])
		}
		return out
	}
	given := []string{"web", "batch", "mail", "cron"}
	store := build(given...)
	var file bytes.Buffer
	if err := store.Write(&file); err != nil {
		t.Fatal(err)
	}
	back, err := Read(&file)
	if err != nil {
		t.Fatal(err)
	}
	for what, s := range map[string]*Store{"as built": store, "read back from its file": back} {
		api := NewAPI(s, at, func() time.Time { return at })
		if got := clients(call(t, api, "/api/v1/query", url.Values{"query": {"x"}})); !reflect.DeepEqual(got, given) {
			t.Errorf("%s, the store handed its series over as %v, want %v", what, got, given)
		}
		if got := clients(call(t, api, "/api/v1/query", url.Values{"query": {"sum by (client) (x)"}})); !reflect.DeepEqual(got, given) {
			t.Errorf("%s, an aggregation's groups came out as %v, want %v", what, got, given)
		}
	}
	// Which of equals `topk` keeps is the order's doing: the same four series, handed over the other way round, and it keeps others.
	kept := func(s *Store) []string {
		got := clients(call(t, NewAPI(s, at, func() time.Time { return at }), "/api/v1/query", url.Values{"query": {"topk(2, x)"}}))
		sort.Strings(got)
		return got
	}
	if a, b := kept(store), kept(build("cron", "mail", "batch", "web")); len(a) != 2 || len(b) != 2 || reflect.DeepEqual(a, b) {
		t.Errorf("topk(2) of four equal series kept %v of one order and %v of the other: it does not go by the order, and this test is not testing anything", a, b)
	}
	// Asked for sorted, a querier sorts; the engine does not ask.
	q, _ := store.Querier(0, 60_000)
	var sorted []string
	for set := q.Select(context.Background(), true, nil); set.Next(); {
		sorted = append(sorted, set.At().Labels().Get("client"))
	}
	if want := []string{"batch", "cron", "mail", "web"}; !reflect.DeepEqual(sorted, want) {
		t.Errorf("asked for its series sorted, the store gave %v", sorted)
	}
	// The series endpoint: one selector as held, several merged and so by label — as a Prometheus's does.
	api := NewAPI(store, at, func() time.Time { return at })
	names := func(res apiResponse) (out []string) {
		var body struct{ Data []map[string]string }
		json.Unmarshal(res.Raw, &body)
		for _, m := range body.Data {
			out = append(out, m["client"])
		}
		return out
	}
	if got := names(call(t, api, "/api/v1/series", url.Values{"match[]": {"x"}})); !reflect.DeepEqual(got, given) {
		t.Errorf("one selector's series were listed as %v, want %v", got, given)
	}
	if got := names(call(t, api, "/api/v1/series", url.Values{"match[]": {`x{client=~"w.*|m.*"}`, `x{client=~"b.*|c.*"}`}})); !reflect.DeepEqual(got, []string{"batch", "cron", "mail", "web"}) {
		t.Errorf("two selectors' series were listed as %v, want them by label", got)
	}
}

// A client that reads the API itself reads these bytes: a time without a fraction where it has none,
// a value with an exponent only where Prometheus writes one, and what the engine remarked on beside
// the answer. promq reads past all three, and the comparison of what it prints did not see them.
func TestAnAnswerIsWrittenAsPrometheusWritesIt(t *testing.T) {
	for ms, want := range map[int64]string{1791228354000: "1791228354", 1791228354005: "1791228354.005", 1791228354430: "1791228354.430", 0: "0", -1500: "-1.500", 999: "0.999"} {
		if got := string(stamp(ms)); got != want {
			t.Errorf("%d ms is written %s, want %s", ms, got, want)
		}
	}
	for f, want := range map[float64]string{0: "0", 1: "1", 0.1: "0.1", 5902.174954327808: "5902.174954327808", 0.000001: "0.000001", 0.0000001: "1e-07", 1e20: "100000000000000000000", 1e21: "1e+21", -1e-9: "-1e-09", math.Inf(1): "+Inf", math.Inf(-1): "-Inf"} {
		if got := sampleValue(f); got != want {
			t.Errorf("%v is written %s, want %s", f, got, want)
		}
	}
	if got := sampleValue(math.NaN()); got != "NaN" {
		t.Errorf("NaN is written %s", got)
	}
	store, err := Load(s2Metrics)
	if err != nil {
		t.Fatal(err)
	}
	freeze := time.UnixMilli(s2Freeze)
	api := NewAPI(store, freeze, func() time.Time { return freeze })
	remarks := func(path string, params url.Values) (warnings, infos []string) {
		var body struct{ Warnings, Infos []string }
		json.Unmarshal(call(t, api, path, params).Raw, &body)
		return body.Warnings, body.Infos
	}
	const notACounter = `PromQL info: metric might not be a counter, name does not end in _total/_sum/_count/_bucket: "thumb_inflight" (1:6)`
	if w, i := remarks("/api/v1/query", url.Values{"query": {`rate(thumb_inflight[1m])`}}); len(w) != 0 || !reflect.DeepEqual(i, []string{notACounter}) {
		t.Errorf("rate of a gauge was answered with warnings %q and infos %q", w, i)
	}
	if w, i := remarks("/api/v1/query_range", url.Values{"query": {`rate(thumb_inflight[1m])`}, "start": {seconds(s2Freeze - 60000)}, "end": {seconds(s2Freeze)}, "step": {"30"}}); len(w) != 0 || !reflect.DeepEqual(i, []string{notACounter}) {
		t.Errorf("a range of the same was answered with warnings %q and infos %q", w, i)
	}
	raw := string(call(t, api, "/api/v1/query", url.Values{"query": {`rate(thumb_requests_total[1m])`}}).Raw)
	if strings.Contains(raw, "infos") || strings.Contains(raw, "warnings") {
		t.Errorf("an answer with nothing to remark on carries a remark: %s", raw)
	}
}

// The comparison of a Prometheus with its frozen copy asks both about the freeze by name. Of the frozen
// store that has to be the question an agent's promq asks without naming anything.
func TestPromqAboutANamedInstant(t *testing.T) {
	store, err := Load(s2Metrics)
	if err != nil {
		t.Fatal(err)
	}
	freeze := time.UnixMilli(s2Freeze)
	later := freeze.Add(3 * time.Hour)
	var asked []string
	api := NewAPI(store, freeze, func() time.Time { return later }).Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path+"?time="+r.FormValue("time")+"&end="+r.FormValue("end"))
		api.ServeHTTP(w, r)
	}))
	defer srv.Close()
	said := func(err error, out *bytes.Buffer) string {
		if err != nil {
			return "error: " + err.Error()
		}
		return out.String()
	}
	now := func(args ...string) string {
		var out bytes.Buffer
		return said(Promq(&out, srv.Client(), srv.URL, args, later), &out)
	}
	at := func(when time.Time, args ...string) string {
		var out bytes.Buffer
		args = append(append([]string{}, args...), "--at", strconv.FormatFloat(float64(when.UnixMilli())/1000, 'f', 3, 64))
		return said(Promq(&out, srv.Client(), srv.URL, args, later), &out)
	}
	for _, args := range [][]string{
		{`sum by (client) (increase(thumb_requests_total[10m]))`}, {`thumb_inflight`}, {`time()`}, {`thumb_inflight[20s]`},
		{`thumb_inflight`, "--range", "2m", "--step", "60s"}, {`rate(thumb_requests_total[1m])`, "--range", "5m"}, {`sum(`}, {`up`, "--range", "soon"},
	} {
		if a, b := now(args...), at(freeze, args...); a != b || a == "" {
			t.Errorf("promq %q: about now it printed\n%s\nand about the freeze, by name,\n%s", args, a, b)
		}
	}
	// Named, the instant is sent; unnamed, it is left to the endpoint, and a range is sent its end either way.
	asked = nil
	now(`up`)
	at(freeze, `up`)
	at(freeze, `up`, "--range", "1m")
	stamp := strconv.FormatFloat(float64(freeze.UnixMilli())/1000, 'f', 3, 64)
	if want := []string{"/api/v1/query?time=&end=", "/api/v1/query?time=" + stamp + "&end=", "/api/v1/query_range?time=&end=" + stamp}; !reflect.DeepEqual(asked, want) {
		t.Errorf("the requests were %q, want %q", asked, want)
	}
	// And an earlier instant is that instant, not the freeze.
	if a, b := at(freeze.Add(-time.Minute), `thumb_inflight`, "--range", "2m", "--step", "60s"), at(freeze, `thumb_inflight`, "--range", "2m", "--step", "60s"); a == b || !strings.Contains(a, "19:24:54=31") || strings.Contains(a, "19:25:54") {
		t.Errorf("a minute before the freeze, the range printed %q; at the freeze, %q", a, b)
	}
	if a, want := at(freeze.Add(-time.Minute), `time()`), strconv.FormatInt(freeze.Add(-time.Minute).Unix(), 10)+"\n"; a != want { // promq prints ten digits
		t.Errorf("time() a minute before the freeze printed %q, want %q", a, want)
	}
	// As a time is written in a log, too; and what is not a time is said to be none, not taken for now.
	var out bytes.Buffer
	if err := Promq(&out, srv.Client(), srv.URL, []string{`thumb_inflight`, "--range", "2m", "--step", "60s", "--at", freeze.Add(-time.Minute).UTC().Format(time.RFC3339Nano)}, later); err != nil ||
		out.String() != at(freeze.Add(-time.Minute), `thumb_inflight`, "--range", "2m", "--step", "60s") {
		t.Errorf("--at as RFC 3339 printed %q (%v)", out.String(), err)
	}
	// What promq is given that it does not take is said, and nothing is asked: a flag it does not have
	// used to be dropped and the question answered about now, and a query in the wrong place was asked
	// as the query `--at`.
	for what, args := range map[string][]string{
		"--at with a clock time":        {`up`, "--at", "19:24"},
		"--at with a number of minutes": {`up`, "--at", "0930"},
		"--at with nothing after it":    {`up`, "--at"},
		"a flag promq does not have":    {`up`, "--time", seconds(s2Freeze)},
		"a word that is no flag":        {`up`, "now"},
		"--at in milliseconds":          {`up`, "--at", "1791228354431"},
		"--at with no end to it":        {`up`, "--at", "Inf"},
	} {
		asked = nil
		if err := Promq(io.Discard, srv.Client(), srv.URL, args, later); err == nil || len(asked) != 0 || strings.Contains(err.Error(), "query comes first") {
			t.Errorf("%s (%q): %v, and %d requests were made", what, args, err, len(asked))
		}
	}
	// And a query in the wrong place is said to be in the wrong place, not asked as the query `--range`.
	for what, args := range map[string][]string{
		"the query after its flags": {"--at", seconds(s2Freeze), `count(up)`},
		"a flag and no query":       {"--range", "5m"},
		"a flag alone":              {"--range"},
		"a call for help":           {"--help"},
		"a shorter call for help":   {"-h"},
	} {
		asked = nil
		if err := Promq(io.Discard, srv.Client(), srv.URL, args, later); err == nil || len(asked) != 0 || !strings.Contains(err.Error(), "query comes first") || !strings.Contains(err.Error(), PromqUsage) {
			t.Errorf("%s (%q): %v, and %d requests were made", what, args, err, len(asked))
		}
	}
}

func TestPromqPrintsOneSeriesPerLine(t *testing.T) {
	store, err := Load(s2Metrics)
	if err != nil {
		t.Fatal(err)
	}
	freeze := time.UnixMilli(s2Freeze)
	later := freeze.Add(3 * time.Hour) // the agent's clock; the store's stands still
	srv := httptest.NewServer(NewAPI(store, freeze, func() time.Time { return later }).Handler())
	defer srv.Close()
	ask := func(args ...string) string {
		var out bytes.Buffer
		if err := Promq(&out, srv.Client(), srv.URL, args, later); err != nil {
			return "error: " + err.Error()
		}
		return out.String()
	}

	want := "{client=catalog-indexer} 5902.174954\n{client=email-renderer} 171.4530178\n{client=image-proxy} 434.6331408\n{client=web-frontend} 729.0744049\n"
	if got := ask(`sum by (client) (increase(thumb_requests_total[10m]))`); got != want {
		t.Errorf("instant query printed\n%s\nwant\n%s", got, want)
	}
	// The decisive evidence of the case this store belongs to is a pattern over exactly this output.
	c, err := casefile.Load(s2Case)
	if err != nil || len(c.Patterns(casefile.StoreMetrics)) != 1 {
		t.Fatalf("the case's metrics evidence: %+v %v", c, err)
	}
	if rx := regexp.MustCompile(c.Patterns(casefile.StoreMetrics)[0]); !rx.MatchString(want) {
		t.Error("the output no longer matches the evidence pattern in the case's answer key")
	}

	// A window asked for on the agent's clock is the window before the freeze, and is printed in the
	// incident's own time — the time in the pod logs beside it, not three hours later.
	got := ask(`thumb_inflight`, "--range", "2m", "--step", "60s")
	if !strings.HasPrefix(got, "thumb_inflight{instance=thumb-api.media.svc:8080,job=thumb-api,") || !strings.HasSuffix(got, "} 19:23:54=1 19:24:54=31 19:25:54=0\n") {
		t.Errorf("range query printed %q", got)
	}
	if got := ask(`nothing_by_this_name`); got != "(empty result)\n" {
		t.Errorf("empty result printed %q", got)
	}
	if got := ask(`scalar(count(up))`); got != "1\n" {
		t.Errorf("scalar printed %q", got)
	}
	if got := ask(`sum(`); !strings.HasPrefix(got, "error: query failed") {
		t.Errorf("a query that does not parse printed %q", got)
	}
	if got := ask(`up`, "--range", "soon"); !strings.HasPrefix(got, "error: --range") {
		t.Errorf("a bad range printed %q", got)
	}
	if err := Promq(io.Discard, srv.Client(), srv.URL, nil, later); err == nil || err.Error() != PromqUsage {
		t.Errorf("no arguments gave %v", err)
	}

	// What a case is checked with: the same text, at the freeze, every series.
	text, err := NewAPI(store, freeze, nil).Text(context.Background(), AllSeries)
	if err != nil || strings.Count(text, "\n") != 15 || !strings.Contains(text, "thumb_requests_total{client=catalog-indexer,code=200,") {
		t.Errorf("Text(all series) = %d lines (%v)", strings.Count(text, "\n"), err)
	}
	if _, err := NewAPI(store, freeze, nil).Text(context.Background(), "sum("); err == nil {
		t.Error("a query that does not parse gave text")
	}
}

// Two counters a few requests apart must not print as the same number: their difference is what an
// agent is asked to quantify.
func TestNumbersKeepWhatAnAgentHasToSubtract(t *testing.T) {
	for in, want := range map[string]string{
		"1234567": "1234567", "1234580": "1234580", "5902.174954327808": "5902.174954", "0.18181500000000002": "0.181815",
		"33": "33", "0": "0", "1791228354.431": "1791228354", "1e-9": "1e-09", "3e20": "3e+20", "NaN": "NaN", "+Inf": "+Inf",
	} {
		if got := number(in); got != want {
			t.Errorf("number(%s) = %s, want %s", in, got, want)
		}
	}
}
