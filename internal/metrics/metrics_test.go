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
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang/snappy"
	"github.com/lapilli-project/lapilli/internal/casefile"
	"github.com/prometheus/prometheus/model/labels"
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
	api := NewAPI(store, freeze)
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
	api := NewAPI(store, freeze)
	at := func(params url.Values) (string, float64) {
		params.Set("query", query)
		res := call(t, api, "/api/v1/query", params)
		if len(res.Data.Result) != 1 {
			t.Fatalf("with %v: %d results (%s)", params, len(res.Data.Result), res.Error)
		}
		return res.Data.Result[0].Value[1].(string), res.Data.Result[0].Value[0].(float64)
	}

	// "The last ten minutes", asked with no time named and asked by a caller whose clock is a month
	// later, is the same ten minutes — and the answer carries the incident's own time both times, not
	// the day it was asked. The server has no clock to go by: the month is only in what the caller sends.
	then, stamp := at(url.Values{})
	later := freeze.Add(31 * 24 * time.Hour)
	now, lateStamp := at(url.Values{"time": {seconds(later.UnixMilli())}})
	if then != now {
		t.Errorf("the answer drifted with the wall clock: %s at the freeze, %s a month later", then, now)
	}
	if want := stampOf(s2Freeze); stamp != want || lateStamp != want {
		t.Errorf("answers are stamped %v and %v; the incident's own time is %v", stamp, lateStamp, want)
	}

	// A time past the end of the case is its end: "five minutes ago", a month later, is past it, and is
	// answered as of the freeze. (It was once five minutes before the freeze, by a rule that moved
	// every such request back by the age of the replay; TestARequestPastTheFreeze has what that cost.)
	fiveAgo, stamp := at(url.Values{"time": {seconds(later.Add(-5 * time.Minute).UnixMilli())}})
	direct, _ := at(url.Values{"time": {seconds(freeze.Add(-5 * time.Minute).UnixMilli())}})
	if fiveAgo != then || fiveAgo == direct {
		t.Errorf("five minutes ago: %s a month later, %s for now, %s five minutes before the freeze", fiveAgo, then, direct)
	}
	if want := stampOf(s2Freeze); stamp != want {
		t.Errorf("five minutes ago is stamped %v, want the freeze, %v", stamp, want)
	}

	// A time at or before the freeze is the incident's own: the agent read it in a pod log. It means
	// itself however late the question is asked.
	inLog := freeze.Add(-5 * time.Minute)
	literal, stamp := at(url.Values{"time": {inLog.UTC().Format(time.RFC3339Nano)}})
	if literal != direct || stamp != stampOf(inLog.UnixMilli()) {
		t.Errorf("asked about %s: %s stamped %v, want %s", inLog.UTC().Format(time.RFC3339), literal, stamp, direct)
	}
	if atFreeze, stamp := at(url.Values{"time": {seconds(freeze.UnixMilli())}}); atFreeze != then || stamp != stampOf(s2Freeze) {
		t.Errorf("the freeze instant itself: %s stamped %v", atFreeze, stamp)
	}

	// A range asked on the wall clock and the same range asked in the incident's time are one answer.
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

	// A Prometheus writes the instant of a scalar and of a string as the shortest number that is it, and
	// the instant of a sample to the thousandth where it is not a whole second; a scalar's value in
	// full, and a sample's with an exponent from 1e21 up and below a millionth. An instant whose
	// thousandths end in nothing is where the two part, which is one freeze in ten.
	for _, c := range []struct{ query, at, want string }{
		{`time()`, "1791228300.5", `"result":[1791228300.5,"1791228300.5"],"resultType":"scalar"`},
		{`time()`, "1791228300.25", `"result":[1791228300.25,"1791228300.25"],"resultType":"scalar"`},
		{`time()`, "1791228300.431", `"result":[1791228300.431,"1791228300.431"],"resultType":"scalar"`},
		{`time()`, "1791228300", `"result":[1791228300,"1791228300"],"resultType":"scalar"`},
		{`"a string"`, "1791228300.5", `"result":[1791228300.5,"a string"],"resultType":"string"`},
		{`"a string"`, "1791228300", `"result":[1791228300,"a string"],"resultType":"string"`},
		{`1e30`, "1791228300.5", `"result":[1791228300.5,"1000000000000000000000000000000"],"resultType":"scalar"`},
		{`1e-7`, "1791228300.5", `"result":[1791228300.5,"0.0000001"],"resultType":"scalar"`},
		{`scalar(vector(1e21))`, "1791228300.5", `"result":[1791228300.5,"1000000000000000000000"]`},
		{`vector(1e21)`, "1791228300.5", `"result":[{"metric":{},"value":[1791228300.500,"1e+21"]}],"resultType":"vector"`},
		{`vector(1e-7)`, "1791228300.25", `"value":[1791228300.250,"1e-07"]`},
		{`vector(1)`, "1791228300", `"value":[1791228300,"1"]`},
	} {
		if got := string(call(t, api, "/api/v1/query", url.Values{"query": {c.query}, "time": {c.at}}).Raw); !strings.Contains(got, c.want) {
			t.Errorf("%s at %s: %s, want %s in it", c.query, c.at, got, c.want)
		}
	}
	if got := string(call(t, api, "/api/v1/query_range", url.Values{"query": {`1e30`}, "start": {"1791228300.5"}, "end": {"1791228300.5"}, "step": {"1"}}).Raw); !strings.Contains(got, `"values":[[1791228300.500,"1e+30"]]`) {
		t.Errorf("1e30 over a window, which is a series of samples: %s", got)
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
		res := call(t, NewAPI(s, at), "/api/v1/query", url.Values{"query": {query}})
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
	h := NewAPI(store, time.UnixMilli(s2Freeze)).Handler()
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
		// The ten minutes asked for, and the five before them that an instant at their beginning looks back.
		if q.EndTimestampMs != s2Freeze || q.StartTimestampMs != s2Freeze-900_000 || len(q.Matchers) != 2 ||
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
	// And the export says where it began reading, which is where the case's metrics begin.
	if store.From != s2Freeze-900_000 {
		t.Errorf("the export began at %d, and says it began at %d", s2Freeze-900_000, store.From)
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
		srv := httptest.NewServer(NewAPI(store, freeze).Handler())
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
	broken := &API{store: &Store{}, freeze: time.UnixMilli(s2Freeze)}
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
	api := NewAPI(store, freeze)
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
	api := NewAPI(store, freeze)
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
	api := NewAPI(store, freeze)
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
	if res := call(t, NewAPI(short, freeze), "/api/v1/query", url.Values{"query": {"up"}}); len(res.Data.Result) != 0 {
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
// arrived. Moved back by the server's clock, as it once was, that landed some milliseconds before the
// freeze, a different few each time, and the same range of `rate` was another number in its fifth
// digit. The server has no clock now, and whatever ends past the freeze ends at the freeze.
func TestARequestAboutNowIsAboutTheFreezeExactly(t *testing.T) {
	store, err := Load(s2Metrics)
	if err != nil {
		t.Fatal(err)
	}
	freeze := time.UnixMilli(s2Freeze)
	callers := freeze.Add(3 * time.Hour) // the caller's clock
	api := NewAPI(store, freeze)
	const query = `rate(thumb_requests_total{client="catalog-indexer",code="200"}[1m])`
	asked := func(end time.Time) (lastStamp float64, values []any) {
		res := call(t, api, "/api/v1/query_range", url.Values{"query": {query}, "start": {seconds(end.Add(-5 * time.Minute).UnixMilli())}, "end": {seconds(end.UnixMilli())}, "step": {"30"}})
		if res.Status != "success" || len(res.Data.Result) != 1 {
			t.Fatalf("a window ending %s: %s %q, %d series", end.Sub(callers), res.Status, res.Error, len(res.Data.Result))
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
	// A now read a moment ago, one read two seconds ago or five minutes, and one an hour ahead.
	for _, early := range []time.Duration{0, 7 * time.Millisecond, 400 * time.Millisecond, -3 * time.Millisecond, 2 * time.Second, 2100 * time.Millisecond, 5 * time.Minute, -time.Hour} {
		if last, got := asked(callers.Add(-early)); last != atTheFreeze || !reflect.DeepEqual(got, want) {
			t.Errorf("a window ending %s before the caller's now ended at %v, with %v; want the freeze, %v, with %v", early, last, got, atTheFreeze, want)
		}
	}
	// And an instant: a client that sends its own now gets the freeze, as one that sends nothing does.
	for _, early := range []time.Duration{0, 7 * time.Millisecond, -3 * time.Millisecond} {
		res := call(t, api, "/api/v1/query", url.Values{"query": {"time()"}, "time": {seconds(callers.Add(-early).UnixMilli())}})
		var scalar struct {
			Data struct{ Result [2]any }
		}
		json.Unmarshal(res.Raw, &scalar)
		if got := fmt.Sprint(scalar.Data.Result[1]); got != seconds(s2Freeze) {
			t.Errorf("time() asked %s before the caller's now is %s, want the freeze, %s", early, got, seconds(s2Freeze))
		}
	}
}

// A case ends at the freeze, and a request that reaches past the end is moved back to end there: a
// window whole, an instant onto the freeze. Nothing else decides it — the server has no clock, and
// does not look where a window begins — so no step of a request is evaluated past the freeze, and
// what is answered and what is said beside it go by the request and the case alone.
func TestARequestPastTheFreeze(t *testing.T) {
	store, err := Load(s2Metrics)
	if err != nil {
		t.Fatal(err)
	}
	sec := func(at time.Time) float64 { return float64(at.UnixMilli()) / 1000 }
	num := func(at time.Time) string { return seconds(at.UnixMilli()) }
	type answer struct {
		first, last float64
		points      int
		said        []string
		end         any
	}
	window := func(api *API, query, start, end, step string) answer {
		res := call(t, api, "/api/v1/query_range", url.Values{"query": {query}, "start": {start}, "end": {end}, "step": {step}})
		if res.Status != "success" || len(res.Data.Result) != 1 || len(res.Data.Result[0].Values) == 0 {
			t.Fatalf("a window %s..%s of %s: %s %q, %d series", start, end, query, res.Status, res.Error, len(res.Data.Result))
		}
		var body struct{ Infos []string }
		json.Unmarshal(res.Raw, &body)
		values := res.Data.Result[0].Values
		return answer{values[0][0].(float64), values[len(values)-1][0].(float64), len(values), body.Infos, values[len(values)-1][1]}
	}
	instant := func(api *API, query, when string) (stamp float64, value any, said []string) {
		res := call(t, api, "/api/v1/query", url.Values{"query": {query}, "time": {when}})
		if res.Status != "success" || len(res.Data.Result) != 1 {
			t.Fatalf("%s at %q: %s %q, %d series", query, when, res.Status, res.Error, len(res.Data.Result))
		}
		var body struct{ Infos []string }
		json.Unmarshal(res.Raw, &body)
		return res.Data.Result[0].Value[0].(float64), res.Data.Result[0].Value[1], body.Infos
	}
	// What is said, whole: of an instant, and of a window.
	asOfTheEnd := func(ends, by string) []string {
		return []string{"frozen case: it ends at " + ends + "; the instant asked about is " + by + " after that, and this is the answer as of the end — name an instant at or before the end to be answered about it"}
	}
	movedBack := func(ends, by string) []string {
		return []string{"frozen case: it ends at " + ends + "; the window asked for ends " + by + " after that, and was moved back by that much, whole: each point is that much earlier than the one asked for — name an end at or before the case's to be answered about a window as it is written"}
	}
	clock := func(at float64) string { return time.UnixMilli(int64(at * 1000)).UTC().Format("15:04:05.000") }

	// Four requests of round 38's recorded runs reached past the freeze of their case, each some minutes
	// after it was frozen (test/fixtures/case-runs/2026-10-07-round38/s2-periodic-saturation, the frozen
	// runs of HolmesGPT 3, 4, 5 and 6: in each, the one window that ends after 09:42:36). They are here
	// as that agent's tool reported having sent them, and the fourth run's also as its model wrote it.
	// Each is the length it was asked, to the end of the case. (Moved back by the age of the replay, as
	// they were then, the first ended at 09:35:44 and the others a minute or two short.)
	recorded := NewAPI(store, time.UnixMilli(1791366156177)) // frozen at 09:42:36.177
	for _, c := range []struct{ start, end, step, first, by string }{
		{"2026-10-07T09:18:00Z", "2026-10-07T09:43:00Z", "30", "09:17:36.177", "23.823s"},
		{"2026-10-07T09:22:45Z", "2026-10-07T09:52:45Z", "15", "09:12:36.177", "10m8.823s"},
		{"2026-10-07T09:22:45.299958+00:00", "2026-10-07T09:52:45.299958+00:00", "15", "09:12:36.177", "10m9.122s"},
		{"2026-10-07T08:55:20Z", "2026-10-07T09:55:20Z", "60", "08:42:36.177", "12m43.823s"},
		{"2026-10-07T08:57:00Z", "2026-10-07T09:57:00Z", "60", "08:42:36.177", "14m23.823s"},
	} {
		got := window(recorded, `time()`, c.start, c.end, c.step)
		if clock(got.first) != c.first || clock(got.last) != "09:42:36.177" || !reflect.DeepEqual(got.said, movedBack("2026-10-07T09:42:36.177Z", c.by)) {
			t.Errorf("asked %s..%s: evaluated %s..%s and said %q; want %s..09:42:36.177, moved back by %s", c.start, c.end, clock(got.first), clock(got.last), got.said, c.first, c.by)
		}
	}

	freeze, day := time.UnixMilli(s2Freeze), 24*time.Hour
	const ends, traffic = "2026-10-05T19:25:54.431Z", `sum(rate(thumb_requests_total[1m]))`
	api := NewAPI(store, freeze)
	_, atFreeze, _ := instant(api, traffic, "")
	for _, c := range []struct {
		what       string
		start, end time.Duration // from the freeze
		step       string
		points     int
	}{
		{"the incident's ten minutes, overshot by 24 seconds", -10*time.Minute + 24*time.Second, 24 * time.Second, "30", 21},
		{"its hour, overshot by 52 minutes", -8 * time.Minute, 52 * time.Minute, "60", 61},
		{"its day, overshot by 14 hours", -10 * time.Hour, 14 * time.Hour, "3600", 25},
		{"a window that begins a millisecond after the freeze", time.Millisecond, time.Minute, "1", 60},
		{"one that begins at the freeze", 0, time.Minute, "15", 5},
		{"twenty seconds, in steps of one", -10 * time.Second, 10 * time.Second, "1", 21},
		{"no time at all, an hour on", time.Hour, time.Hour, "15", 1},
		{"the five minutes to a now seven minutes on", 2 * time.Minute, 7 * time.Minute, "30", 11},
		{"three hours that begin an hour on", time.Hour, 4 * time.Hour, "60", 181},
		{"the hour to a now a day on", 23 * time.Hour, day, "30", 121},
		{"ten minutes, forty days on", 40*day - 10*time.Minute, 40 * day, "30", 21},
		{"an hour, a year on", 365*day - time.Hour, 365 * day, "60", 61},
		{"an hour, a hundred years on", 36500*day - time.Hour, 36500 * day, "60", 61},
		// Its two steps, as asked, are 42m36s and 12m36s before the freeze, both in the case. It is moved by
		// where it ends all the same: that is the one rule, and what is said has by how much.
		{"a window with no step past the freeze, whose end is", -42*time.Minute - 36*time.Second, 16*time.Minute + 24*time.Second, "1800", 2},
	} {
		// The steps count from where the window begins, and the last of them is the end when the window is a
		// whole number of them long.
		length := c.end - c.start
		step, _ := time.ParseDuration(c.step + "s")
		first := freeze.Add(-length)
		last := first.Add(time.Duration(c.points-1) * step)
		got := window(api, `time()`, num(freeze.Add(c.start)), num(freeze.Add(c.end)), c.step)
		if got.first != sec(first) || got.last != sec(last) || last.After(freeze) || got.points != c.points || !reflect.DeepEqual(got.said, movedBack(ends, c.end.String())) {
			t.Errorf("%s: evaluated %s..%s in %d points and said %q; want %s..%s in %d points, moved back by %s", c.what, clock(got.first), clock(got.last), got.points, got.said, clock(sec(first)), clock(sec(last)), c.points, c.end)
		}
		// And what stands at its end is what the freeze says, not a rate run out of samples.
		if last.Equal(freeze) && length >= time.Minute {
			if got := window(api, traffic, num(freeze.Add(c.start)), num(freeze.Add(c.end)), c.step); got.last != sec(freeze) || got.end != atFreeze {
				t.Errorf("the traffic at the end of %s is %v at %s; want what the freeze says, %v", c.what, got.end, clock(got.last), atFreeze)
			}
		}
	}

	// An instant past the end is answered as of the end, in whichever spelling of a time it comes.
	kst := time.FixedZone("KST", 9*3600)
	for _, past := range []time.Duration{time.Millisecond, 10 * time.Second, 5 * time.Minute, 25 * time.Hour, 40 * day, 36500 * day} {
		at := freeze.Add(past)
		for _, when := range []string{num(at), at.UTC().Format(time.RFC3339Nano), at.In(kst).Format(time.RFC3339Nano)} {
			if stamp, value, said := instant(api, traffic, when); stamp != sec(freeze) || value != atFreeze || !reflect.DeepEqual(said, asOfTheEnd(ends, past.String())) {
				t.Errorf("the traffic at %s, %s past the freeze: %v stamped %s, said %q; want what the freeze says, %v", when, past, value, clock(stamp), said, atFreeze)
			}
		}
	}

	// Further than a duration counts — a time in milliseconds sent for one in seconds, the last day of
	// the year 9999, the end of time as a Prometheus writes it, a number that is no instant — it is the
	// end of the case still. (Taken back by how far it was past, it was not: the distance ran out at 292
	// years, and the answer was nothing, with a remark that it stood for the end.)
	for _, when := range []string{seconds(s2Freeze * 1000), "9999-12-31T23:59:59Z", maxTime.Format(time.RFC3339Nano), "1e300", "+Inf"} {
		if stamp, value, said := instant(api, traffic, when); stamp != sec(freeze) || value != atFreeze || !reflect.DeepEqual(said, asOfTheEnd(ends, "more than 292 years")) {
			t.Errorf("the traffic at %s: %v stamped %s, said %q; want what the freeze says, %v", when, value, clock(stamp), said, atFreeze)
		}
	}
	for _, c := range []struct {
		start, end, step string
		length           time.Duration
	}{
		{"9999-12-31T23:49:59Z", "9999-12-31T23:59:59Z", "30", 10 * time.Minute},
		{seconds((s2Freeze - 600_000) * 1000), seconds(s2Freeze * 1000), "30000", 600_000 * time.Second}, // ten minutes, in milliseconds
	} {
		got := window(api, `time()`, c.start, c.end, c.step)
		if got.points != 21 || got.first != sec(freeze.Add(-c.length)) || got.last != sec(freeze) || !reflect.DeepEqual(got.said, movedBack(ends, "more than 292 years")) {
			t.Errorf("a window %s..%s: %d points, %v..%v, said %q", c.start, c.end, got.points, got.first, got.last, got.said)
		}
	}

	// A request that does not reach past the end is as written, and nothing is said of it: to the end
	// exactly; in the incident's own time, in any zone; and when its instant is written to more than a
	// millisecond, which is all a Prometheus keeps of it. A now that lies before the freeze is such a
	// request too — a clock that runs behind, or a now more stale than the replay is old — and nothing
	// can tell it from a time read in a log.
	if got := window(api, `time()`, num(freeze.Add(-10*time.Minute)), num(freeze), "30"); got.first != sec(freeze.Add(-10*time.Minute)) || got.last != sec(freeze) || len(got.said) != 0 {
		t.Errorf("the ten minutes up to the freeze ran %s..%s and said %q", clock(got.first), clock(got.last), got.said)
	}
	for when, want := range map[string]time.Time{
		num(freeze):                          freeze,
		"2026-10-05T19:25:54.4314Z":          freeze,
		"2026-10-05T19:25:54.4316Z":          freeze,
		"2026-10-05T19:22:54.431Z":           freeze.Add(-3 * time.Minute),
		"2026-10-06T04:22:54.431+09:00":      freeze.Add(-3 * time.Minute),
		"2026-10-05T12:22:54.431-07:00":      freeze.Add(-3 * time.Minute),
		num(freeze.Add(-10 * time.Second)):   freeze.Add(-10 * time.Second),
		num(freeze.Add(-90 * time.Second)):   freeze.Add(-90 * time.Second),
		num(freeze.Add(-time.Millisecond)):   freeze.Add(-time.Millisecond),
		"2026-10-05T19:25:54.430999999Z":     freeze.Add(-time.Millisecond),
		seconds(s2Freeze)[:10] + ".4314":     freeze,
		seconds(s2Freeze)[:10] + ".43149999": freeze,
	} {
		if stamp, _, said := instant(api, `count(up)`, when); stamp != sec(want) || len(said) != 0 {
			t.Errorf("count(up) at %s is stamped %s and said %q; want %s and nothing", when, clock(stamp), said, clock(sec(want)))
		}
	}
	// In seconds, an instant is rounded to the millisecond, as a Prometheus rounds it; and this one is
	// then a millisecond past the end.
	if stamp, _, said := instant(api, `count(up)`, seconds(s2Freeze)[:10]+".4316"); stamp != sec(freeze) || !reflect.DeepEqual(said, asOfTheEnd(ends, "1ms")) {
		t.Errorf("count(up) at …354.4316 is stamped %s and said %q", clock(stamp), said)
	}
	if res := call(t, api, "/api/v1/query", url.Values{"query": {`count(up)`}, "time": {"NaN"}}); res.Code != http.StatusBadRequest || res.Kind != "bad_data" {
		t.Errorf("an instant that is not a number was answered %d %s %q", res.Code, res.Kind, res.Error)
	}

	// The end of the case is said in UTC and to the millisecond, wherever the server keeps its time.
	whole := NewAPI(store, time.Unix(s2Freeze/1000, 0).In(kst))
	if _, _, said := instant(whole, `count(up)`, seconds(s2Freeze + 6000)[:10]); !reflect.DeepEqual(said, asOfTheEnd("2026-10-05T19:25:54.000Z", "6s")) {
		t.Errorf("a case that ends on the second, in a zone nine hours ahead, says %q", said)
	}

	// What the engine has to say goes first, and what the store says of the request after it.
	res := call(t, api, "/api/v1/query", url.Values{"query": {`rate(thumb_inflight[1m])`}, "time": {num(freeze.Add(time.Hour))}})
	var body struct{ Infos []string }
	if json.Unmarshal(res.Raw, &body); len(body.Infos) != 2 || !strings.HasPrefix(body.Infos[0], "PromQL info: ") || body.Infos[1] != asOfTheEnd(ends, "1h0m0s")[0] {
		t.Errorf("the rate of a gauge an hour past the freeze was answered with %q beside it", body.Infos)
	}

	// promq prints what the store says of a time it was given, for an instant and for a window, and
	// nothing of what an engine says, which it never printed. Of a time it was not given it prints
	// nothing: unasked it means now, the store moves that to the end of the case and says so, and the
	// same question by name — which is how the two are compared (test/replay-diff) — is not moved.
	later := freeze.Add(15 * time.Minute) // promq's clock
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()
	threePoints := "{} 19:24:54=1 19:25:24=1 19:25:54=1\n"
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{`count(up)`, "--at", num(freeze.Add(6 * time.Second))}, "{} 1\n(" + asOfTheEnd(ends, "6s")[0] + ")\n"},
		{[]string{`count(up)`, "--range", "1m", "--step", "30s", "--at", num(freeze.Add(66500 * time.Millisecond))}, threePoints + "(" + movedBack(ends, "1m6.5s")[0] + ")\n"},
		{[]string{`count(rate(thumb_inflight[1m]))`, "--at", num(freeze.Add(6 * time.Second))}, "{} 1\n(" + asOfTheEnd(ends, "6s")[0] + ")\n"},
		{[]string{`count(up)`, "--at", "2026-10-06T04:26:00.431+09:00"}, "{} 1\n(" + asOfTheEnd(ends, "6s")[0] + ")\n"},
		{[]string{`count(up)`, "--at", "9999999999"}, "{} 1\n(" + asOfTheEnd(ends, time.Unix(9999999999, 0).Sub(freeze).String())[0] + ")\n"},
		{[]string{`count(up)`, "--range", "1m", "--step", "30s"}, threePoints},
		{[]string{`count(up)`, "--range", "1m", "--step", "30s", "--at", num(freeze)}, threePoints},
		{[]string{`count(up)`}, "{} 1\n"},
		{[]string{`count(up)`, "--at", num(freeze)}, "{} 1\n"},
		{[]string{`count(up)`, "--at", "2026-10-06T04:25:54.431+09:00"}, "{} 1\n"},
		{[]string{`count(rate(thumb_inflight[1m]))`}, "{} 1\n"},
		{[]string{`count(up)`, "--at", "1000000000"}, "(empty result)\n"},
	} {
		var out bytes.Buffer
		if err := Promq(&out, srv.Client(), srv.URL, c.args, later); err != nil || out.String() != c.want {
			t.Errorf("promq %q printed\n%s(%v)\nwant\n%s", c.args, out.String(), err, c.want)
		}
	}
	// And a time promq takes is one between 2001 and 2286: below is a count of something, above is
	// milliseconds, and the store would answer both as it answers any number.
	for _, at := range []string{"999999999", "10000000000", seconds(s2Freeze * 1000), "1930", "NaN"} {
		var out bytes.Buffer
		if err := Promq(&out, srv.Client(), srv.URL, []string{`count(up)`, "--at", at}, later); err == nil || !strings.Contains(err.Error(), "not a time") || out.Len() != 0 {
			t.Errorf("promq --at %s printed %q (%v)", at, out.String(), err)
		}
	}
}

// A case holds a window of its Prometheus. A query that looks further back than the window is
// answered with the nothing the case has there, and the case says so beside the answer, since nothing
// in the answer tells a metric that began then from a case that did.
func TestAQueryThatLooksBeforeTheCaseBegins(t *testing.T) {
	// Ten minutes were asked for, and read with the five before them that an instant looks back: one
	// series every fifteen seconds from there, a counter and a gauge.
	freeze := time.UnixMilli(1791455059751)
	from := freeze.Add(-15 * time.Minute)
	store := &Store{From: from.UnixMilli()}
	var ts []int64
	var counted, level []float64
	for at := from.Add(9 * time.Second); !at.After(freeze); at = at.Add(15 * time.Second) {
		ts, counted, level = append(ts, at.UnixMilli()), append(counted, float64(len(ts))*3), append(level, 20)
	}
	store.Add(map[string]string{"__name__": "requests_total"}, ts, counted)
	store.Add(map[string]string{"__name__": "queue_depth"}, ts, level)
	api := NewAPI(store, freeze)
	num := func(at time.Time) string { return seconds(at.UnixMilli()) }
	const begins = "frozen case: it holds no samples before 2026-10-08T10:09:19.751Z, and this query looks "
	// Of an instant, and of a window none of whose steps is whole: how much further back. Of a window some
	// of whose steps are whole: that, and the instant from which they are.
	all := func(by string) []string {
		return []string{begins + by + " further back than that: its Prometheus may have had more to answer from"}
	}
	before := func(by, whole string) []string {
		return []string{begins + by + " further back than that: its points before 2026-10-08T" + whole + "Z may be missing, or come of less than its Prometheus had"}
	}
	said := func(path string, params url.Values) (warnings, infos []string) {
		res := call(t, api, path, params)
		var body struct{ Warnings, Infos []string }
		if json.Unmarshal(res.Raw, &body); res.Status != "success" {
			t.Fatalf("%s %v: %s %q", path, params, res.Status, res.Error)
		}
		return body.Warnings, body.Infos
	}
	window := func(query string, start, end time.Time, step string) []string {
		warnings, _ := said("/api/v1/query_range", url.Values{"query": {query}, "start": {num(start)}, "end": {num(end)}, "step": {step}})
		return warnings
	}
	instant := func(query string, at time.Time) []string {
		warnings, _ := said("/api/v1/query", url.Values{"query": {query}, "time": {num(at)}})
		return warnings
	}

	lookingBack := func(delta string) []string {
		warnings, _ := said("/api/v1/query", url.Values{"query": {`queue_depth`}, "time": {num(freeze.Add(-time.Minute))}, "lookback_delta": {delta}})
		return warnings
	}
	// What was asked for is whole: an instant anywhere in the ten minutes, the ten minutes as a window,
	// a rate over five minutes at their very beginning. And what looks at no series looks at nothing.
	tenAgo := freeze.Add(-10 * time.Minute)
	for what, got := range map[string][]string{
		"an instant at the freeze":                  instant(`queue_depth`, freeze),
		"an instant at the beginning of the window": instant(`queue_depth`, tenAgo),
		"the window": window(`queue_depth`, tenAgo, freeze, "15"),
		"a rate over five minutes, over the window":              window(`rate(requests_total[5m])`, tenAgo, freeze, "30"),
		"a rate over five minutes at the window's beginning":     instant(`rate(requests_total[5m])`, tenAgo),
		"what an instant looks back, to the millisecond":         instant(`queue_depth`, tenAgo.Add(-time.Millisecond)),
		"a rate over the whole of it":                            instant(`rate(requests_total[15m])`, freeze.Add(-time.Millisecond)),
		"fourteen minutes ago, by an offset":                     instant(`queue_depth offset 9m59s999ms`, freeze),
		"no series at all":                                       instant(`vector(1) + time()`, from.Add(-time.Hour)),
		"what looks back a millisecond, inside the case":         lookingBack("0.001"),
		"what looks back less than a millisecond":                lookingBack("0.0005"),
		"a series that is not there, looked for inside the case": instant(`no_such_metric`, freeze),
	} {
		if len(got) != 0 {
			t.Errorf("%s: said %q, and it does not look before the case begins", what, got)
		}
	}
	// What looks further back is told how much further — by an instant's looking back, a range, an offset,
	// an `@`, a subquery; the furthest of what a query's selectors look at — and is answered all the same.
	for _, c := range []struct {
		what      string
		got, want []string
	}{
		{"an instant two milliseconds before the window", instant(`queue_depth`, tenAgo.Add(-2*time.Millisecond)), all("2ms")},
		{"an hour ago, by an offset", instant(`queue_depth offset 1h`, freeze), all("50m0s")},
		{"an hour ago and now, in one query", instant(`queue_depth - queue_depth offset 1h`, freeze), all("50m0s")},
		{"a subquery over half an hour", instant(`max_over_time(rate(requests_total[1m])[30m:1m])`, freeze), all("16m0s")},
		{"an instant by its timestamp, before the case", instant(`queue_depth @ `+num(from.Add(-2*time.Hour)), freeze), all("2h5m0s")},
		{"the year 1970", instant(`queue_depth @ 0`, freeze), all(from.Sub(time.UnixMilli(-300_000)).String())},
		{"four centuries back", instant(`queue_depth @ -10000000000`, freeze), all("more than 292 years")},
		{"so far back that the lookback runs off the end of the numbers", instant(`queue_depth @ -9223372036854775`, freeze), all("more than 292 years")},
		{"so far back that a millisecond more is past what a duration counts", instant(`queue_depth @ -7431922036.855`, freeze), all("more than 292 years")},
		{"a series that is not there, looked for before the case", instant(`no_such_metric offset 20m`, freeze), all("10m0s")},
		// A window is told from which of its points on it is whole: where a step no longer looks before the beginning.
		{"a window that begins a minute early", window(`queue_depth`, tenAgo.Add(-time.Minute), freeze, "15"), before("1m0s", "10:14:19.751")},
		{"a window of something a minute later than each step, which begins two minutes early", window(`queue_depth offset -1m`, tenAgo.Add(-2*time.Minute), freeze.Add(-time.Minute), "15"), before("1m0s", "10:13:19.751")},
		{"a rate over ten minutes, over the window", window(`rate(requests_total[10m])`, tenAgo, freeze, "30"), before("5m0s", "10:19:19.751")},
		{"a window whose last step is the first whole one", window(`queue_depth`, tenAgo.Add(-time.Minute), tenAgo, "15"), before("1m0s", "10:14:19.751")},
		// And only how much further, where none of it is: a window that ends before any step is whole, and
		// one whose selector is held to an instant of its own, which looks as far back at every step.
		{"a window that ends a millisecond before any of it is whole", window(`queue_depth`, tenAgo.Add(-time.Minute), tenAgo.Add(-time.Millisecond), "15"), all("1m0s")},
		{"a window wholly before the case", window(`queue_depth`, from.Add(-30*time.Minute), from.Add(-20*time.Minute), "60"), all("35m0s")},
		{"a window of something held to an instant before the case", window(`queue_depth @ `+num(from.Add(-time.Hour)), tenAgo, freeze, "60"), all("1h5m0s")},
		{"a window of something held to an instant a minute before the case", window(`queue_depth @ `+num(from.Add(-time.Minute)), tenAgo, freeze, "60"), all("6m0s")},
		{"a window of a subquery held to an instant", window(`max_over_time(queue_depth[20m:1m] @ `+num(freeze)+`)`, tenAgo, freeze, "60"), all("10m0s")},
		{"a window of something held to its own start, over twenty minutes", window(`max_over_time(queue_depth[20m] @ start())`, tenAgo, freeze, "60"), all("15m0s")},
		{"a window that begins a minute early, of something held to its start", window(`queue_depth @ start()`, tenAgo.Add(-time.Minute), freeze, "15"), all("1m0s")},
		{"a window of a subquery held to its end", window(`max_over_time(queue_depth[20m:1m] @ end())`, tenAgo, freeze, "60"), all("10m0s")},
	} {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s: said %q, want %q", c.what, c.got, c.want)
		}
	}
	// It is said among the warnings, after the engine's own, and not among the infos, where what is said
	// of a request moved back from past the end stays.
	res := call(t, api, "/api/v1/query", url.Values{"query": {`rate(queue_depth[20m]) or histogram_quantile(0.9, queue_depth offset 1m)`}, "time": {num(freeze.Add(time.Hour))}})
	var body struct{ Warnings, Infos []string }
	json.Unmarshal(res.Raw, &body)
	if len(body.Infos) != 2 || !strings.HasPrefix(body.Infos[0], "PromQL info: ") || !strings.HasPrefix(body.Infos[1], "frozen case: it ends at ") ||
		len(body.Warnings) != 2 || !strings.HasPrefix(body.Warnings[0], "PromQL warning: ") || body.Warnings[1] != all("5m0s")[0] {
		t.Errorf("a query the engine remarks on twice, asked an hour past the end and looking before the beginning: warnings %q, infos %q", body.Warnings, body.Infos)
	}
	// A query that is refused is refused, with nothing beside it.
	if res := call(t, api, "/api/v1/query", url.Values{"query": {`queue_depth offset 1h +`}}); res.Code != http.StatusBadRequest || strings.Contains(string(res.Raw), "frozen case") {
		t.Errorf("a query that does not parse: %d %s", res.Code, res.Raw)
	}

	// promq prints it under the answer, whether or not it was given a time: no caller can know it otherwise.
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()
	later := freeze.Add(3 * time.Hour) // promq's clock
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{`queue_depth offset 1h`}, "(empty result)\n(" + all("50m0s")[0] + ")\n"},
		{[]string{`queue_depth offset 1h`, "--at", num(freeze)}, "(empty result)\n(" + all("50m0s")[0] + ")\n"},
		{[]string{`count(queue_depth)`, "--range", "11m", "--step", "5m"}, "{} 10:13:19=1 10:18:19=1 10:23:19=1\n(" + before("1m0s", "10:14:19.751")[0] + ")\n"},
		{[]string{`count(queue_depth)`, "--range", "10m", "--step", "5m"}, "{} 10:14:19=1 10:19:19=1 10:24:19=1\n"},
		{[]string{`count(queue_depth)`}, "{} 1\n"},
		// What the engine warns of beside an answer is the engine's, and promq does not print it.
		{[]string{`histogram_quantile(0.9, queue_depth)`}, "(empty result)\n"},
		// Given a time past the end as well: what is said of the beginning, and then of the end.
		{[]string{`count(queue_depth offset 20m)`, "--at", num(freeze.Add(time.Minute))}, "(empty result)\n(" + all("10m0s")[0] + ")\n(frozen case: it ends at 2026-10-08T10:24:19.751Z; the instant asked about is 1m0s after that, and this is the answer as of the end — name an instant at or before the end to be answered about it)\n"},
	} {
		var out bytes.Buffer
		if err := Promq(&out, srv.Client(), srv.URL, c.args, later); err != nil || out.String() != c.want {
			t.Errorf("promq %q printed\n%s(%v)\nwant\n%s", c.args, out.String(), err, c.want)
		}
	}

	// A case that does not say where its metrics begin — one frozen before it was recorded — is asked
	// the same and says nothing: where its oldest sample lies is not where it begins.
	unknown := &Store{}
	unknown.Add(map[string]string{"__name__": "queue_depth"}, ts, level)
	api = NewAPI(unknown, freeze)
	for _, query := range []string{`queue_depth offset 1h`, `queue_depth @ -1000`} { // an hour back, and before 1970
		if got := instant(query, freeze); len(got) != 0 {
			t.Errorf("a case that does not say where it begins said %q of %s", got, query)
		}
	}
}

// A Prometheus hands a query its series as its head made them, and by label where the query reaches a
// block. A case frozen from one that has blocks keeps the head's order and where the blocks ended, and
// hands them over the one way or the other by how far back a query looks.
func TestACaseKeepsTheOrderOfItsPrometheusHead(t *testing.T) {
	series := func(name, x string, ts ...int64) *prompb.TimeSeries {
		out := &prompb.TimeSeries{Labels: []prompb.Label{{Name: "__name__", Value: name}, {Name: "x", Value: x}}}
		for _, at := range ts {
			out.Samples = append(out.Samples, prompb.Sample{Timestamp: at, Value: 7})
		}
		return out
	}
	// What a read that reaches into blocks returns: by label. The head made them 3, 1, 2; `gone` ended before it.
	byLabel := []*prompb.TimeSeries{series("a", "1", 1000, 9000), series("a", "2", 1000, 9000), series("a", "3", 1000, 9000), series("b", "1", 9000), series("gone", "9", 1000)}
	const inHead = `{"status":"success","data":[{"__name__":"a","x":"3"},{"__name__":"b","x":"1"},{"__name__":"a","x":"1"},{"__name__":"a","x":"2"},{"__name__":"later","x":"0"}]}`
	blocks := func(ends ...int64) string {
		out := []string{}
		for _, end := range ends {
			out = append(out, fmt.Sprintf(`{"ulid":"01","minTime":0,"maxTime":%d,"stats":{"numSeries":4}}`, end))
		}
		return `{"status":"success","data":{"blocks":[` + strings.Join(out, ",") + `]}}`
	}
	head := func(minTime int64) string {
		return fmt.Sprintf(`{"status":"success","data":{"headStats":{"numSeries":4,"minTime":%d,"maxTime":9000}}}`, minTime)
	}
	const some, none = `{"status":"success","data":["__name__"]}`, `{"status":"success","data":[]}`
	held := func(store *Store) string {
		var out []string
		for _, se := range store.series {
			out = append(out, se.lset.Get("__name__")+se.lset.Get("x")+se.lset.Get("cluster"))
		}
		return strings.Join(out, " ")
	}
	const asRead = "a1 a2 a3 b1 gone9"
	everything := `/api/v1/series?end=10.000&match[]={__name__=~".+"}&start=5.000`
	byProbe := []string{"/api/v1/status/tsdb/blocks?", "/api/v1/status/tsdb?", "/api/v1/labels?end=4.999&limit=1"}
	for _, c := range []struct {
		what                  string
		blocks, status, older string            // what it says of its blocks, of its head, and of what is older than its head
		listed                map[string]string // what it lists of its head, by selector; "" for any
		selectors             []string
		config                string
		read                  []*prompb.TimeSeries
		wantEnd               int64
		wantKnown             bool
		wantOrder             string
		wantAsked             []string
	}{
		// A Prometheus that lists its blocks: the latest end among them is where a query begins to reach one.
		{what: "a Prometheus that lists its blocks", blocks: blocks(3000, 5000, 4000), listed: map[string]string{"": inHead},
			wantEnd: 5000, wantKnown: true, wantOrder: "a3 b1 a1 a2 gone9", wantAsked: []string{"/api/v1/status/tsdb/blocks?", everything}},
		{what: "one that lists none", blocks: blocks(), listed: map[string]string{"": inHead},
			wantEnd: 0, wantKnown: true, wantOrder: asRead, wantAsked: []string{"/api/v1/status/tsdb/blocks?"}},
		// One that does not: where its head begins, and whether it knows of anything older.
		{what: "one that says where its head begins and knows of something older", status: head(5000), older: some, listed: map[string]string{"": inHead},
			wantEnd: 5000, wantKnown: true, wantOrder: "a3 b1 a1 a2 gone9", wantAsked: append(append([]string{}, byProbe...), everything)},
		{what: "one that holds nothing older than its head", status: head(5000), older: none, listed: map[string]string{"": inHead},
			wantEnd: 0, wantKnown: true, wantOrder: asRead, wantAsked: byProbe},
		{what: "one that says neither", wantOrder: asRead, wantAsked: byProbe[:2]},
		{what: "one whose status has no head in it", status: `{"status":"success","data":{"seriesCountByMetricName":[]}}`, wantOrder: asRead, wantAsked: byProbe[:2]},
		{what: "one that cannot say what is older", status: head(5000), older: `{"status":"error","error":"no"}`, wantOrder: asRead, wantAsked: byProbe},
		{what: "one that says nothing is older, with no list to say it in", status: head(5000), older: `{"status":"success","data":null}`, wantOrder: asRead, wantAsked: byProbe},
		{what: "one that says what is older under an error", status: head(5000), older: strings.Replace(some, "success", "error", 1), wantOrder: asRead, wantAsked: byProbe},
		{what: "one that says what is older with a status that is not 200", status: head(5000), older: "503 " + some, wantOrder: asRead, wantAsked: byProbe},
		{what: "one whose list of blocks is no list", blocks: `{"status":"success","data":{}}`, wantOrder: asRead, wantAsked: byProbe[:2]},
		{what: "one that says success of its blocks with a status that is not 200", blocks: "503 " + blocks(5000), wantOrder: asRead, wantAsked: byProbe[:2]},
		{what: "one that sends its blocks under an error", blocks: strings.Replace(blocks(5000), `"success"`, `"error"`, 1), wantOrder: asRead, wantAsked: byProbe[:2]},
		// A head that cannot be listed, or whose listing does not account for what was read, is not believed:
		// the case is left as it was read, and says nothing of its order.
		{what: "one that will not list its head", blocks: blocks(5000), listed: map[string]string{"": `{"status":"error","error":"too many"}`},
			wantOrder: asRead, wantAsked: []string{"/api/v1/status/tsdb/blocks?", everything}},
		{what: "one that lists nothing where a list should be", blocks: blocks(5000), listed: map[string]string{"": `{"status":"success","data":null}`},
			wantOrder: asRead, wantAsked: []string{"/api/v1/status/tsdb/blocks?", everything}},
		{what: "one whose listing lacks a series that was read from the head", blocks: blocks(5000), listed: map[string]string{"": `{"status":"success","data":[{"__name__":"a","x":"3"},{"__name__":"a","x":"1"},{"__name__":"a","x":"2"}]}`},
			wantOrder: asRead, wantAsked: []string{"/api/v1/status/tsdb/blocks?", everything}},
		{what: "one whose listing is of an empty head", blocks: blocks(5000), listed: map[string]string{"": none},
			wantOrder: asRead, wantAsked: []string{"/api/v1/status/tsdb/blocks?", everything}},
		// Nothing listed is not a head with nothing in it, even where nothing was read that it would have had to list.
		{what: "one that lists nothing where a list should be, of which nothing was read since its blocks ended", blocks: blocks(5000), listed: map[string]string{"": `{"status":"success","data":null}`},
			read: []*prompb.TimeSeries{series("gone", "9", 1000)}, wantOrder: "gone9", wantAsked: []string{"/api/v1/status/tsdb/blocks?", everything}},
		// What the listing has to account for is every series with a sample since the blocks ended: one whose
		// last sample is at that very instant, which is the head's first, and one that began before it.
		{what: "one whose listing lacks a series whose last sample is where the blocks end", blocks: blocks(5000), listed: map[string]string{"": inHead},
			read: append(append([]*prompb.TimeSeries{}, byLabel...), series("on", "0", 5000)), wantOrder: asRead + " on0", wantAsked: []string{"/api/v1/status/tsdb/blocks?", everything}},
		{what: "one whose listing lacks a series read from before the blocks ended and after", blocks: blocks(5000), listed: map[string]string{"": `{"status":"success","data":[{"__name__":"a","x":"3"},{"__name__":"b","x":"1"},{"__name__":"a","x":"1"}]}`},
			wantOrder: asRead, wantAsked: []string{"/api/v1/status/tsdb/blocks?", everything}},
		// The series the head did not hold come after the others by label, however they were read.
		{what: "one of which two series were read that ended before the blocks did, the later by label first", blocks: blocks(5000), listed: map[string]string{"": inHead},
			read:    append(append([]*prompb.TimeSeries{}, byLabel...), series("gone", "1", 1000)),
			wantEnd: 5000, wantKnown: true, wantOrder: "a3 b1 a1 a2 gone1 gone9", wantAsked: []string{"/api/v1/status/tsdb/blocks?", everything}},
		// Blocks that end at the freeze itself leave an instant of the head to list; ones that end after it leave none.
		{what: "one whose blocks end at the freeze", blocks: blocks(10000), listed: map[string]string{"": none},
			wantEnd: 10000, wantKnown: true, wantOrder: asRead, wantAsked: []string{"/api/v1/status/tsdb/blocks?", `/api/v1/series?end=10.000&match[]={__name__=~".+"}&start=10.000`}},
		{what: "one whose blocks end after the freeze", blocks: blocks(20000),
			wantEnd: 20000, wantKnown: true, wantOrder: asRead, wantAsked: []string{"/api/v1/status/tsdb/blocks?"}},
		// Two selectors: what was read came one selector after the other, which is no order of the head's, and
		// a head lists several by label. Nothing is asked, and nothing is said to be known.
		{what: "one asked of two selectors", blocks: blocks(5000), selectors: []string{"b", `{x=~"1|2|3"}`}, listed: map[string]string{"": inHead}, wantOrder: asRead},
		{what: "one asked of two selectors, that has no block", blocks: blocks(), selectors: []string{"b", "a"}, wantOrder: asRead},
		// A listing of any length is read whole.
		{what: "one whose head lists many more series than were read", blocks: blocks(5000),
			listed:  map[string]string{"": strings.Replace(inHead, `{"__name__":"later","x":"0"}`, strings.TrimSuffix(strings.Repeat(`{"__name__":"later","x":"0","with":"a label long enough to count for something"},`, 40), ","), 1)},
			wantEnd: 5000, wantKnown: true, wantOrder: "a3 b1 a1 a2 gone9", wantAsked: []string{"/api/v1/status/tsdb/blocks?", everything}},
		// A series the head lists twice is where it was listed first.
		{what: "one whose head lists a series twice", blocks: blocks(5000),
			listed:  map[string]string{"": `{"status":"success","data":[{"__name__":"a","x":"2"},{"__name__":"b","x":"1"},{"__name__":"a","x":"3"},{"__name__":"a","x":"1"},{"__name__":"a","x":"2"}]}`},
			wantEnd: 5000, wantKnown: true, wantOrder: "a2 b1 a3 a1 gone9", wantAsked: []string{"/api/v1/status/tsdb/blocks?", everything}},
		// A block that does not say where it ends is no list to go by: the head is asked instead.
		{what: "one whose block says no end", blocks: `{"status":"success","data":{"blocks":[{"ulid":"01","minTime":0}]}}`, status: head(5000), older: some, listed: map[string]string{"": inHead},
			wantEnd: 5000, wantKnown: true, wantOrder: "a3 b1 a1 a2 gone9", wantAsked: append(append([]string{}, byProbe...), everything)},
		{what: "one whose block ends at nothing", blocks: blocks(5000, 0), wantOrder: asRead, wantAsked: byProbe[:2]},
		// What it adds to every series it sends elsewhere is taken off again: its own listing does not have
		// it, and neither do its own answers. A label of that name with another value is the series's own.
		// (Which series have such a label of their own it is asked first: TestExternalLabelsAreTakenOffWhatWasRead.)
		{what: "one with external labels", blocks: blocks(5000), listed: map[string]string{"": inHead, `{cluster="prod"}`: none, `{x="2"}`: none},
			config: `{"status":"success","data":{"yaml":"global:\n  external_labels:\n    cluster: prod\n    x: \"2\"\n"}}`,
			read: []*prompb.TimeSeries{
				{Labels: []prompb.Label{{Name: "__name__", Value: "a"}, {Name: "cluster", Value: "prod"}, {Name: "x", Value: "1"}}, Samples: []prompb.Sample{{Timestamp: 9000, Value: 7}}},
				{Labels: []prompb.Label{{Name: "__name__", Value: "a"}, {Name: "cluster", Value: "prod"}, {Name: "x", Value: "3"}}, Samples: []prompb.Sample{{Timestamp: 9000, Value: 7}}},
				{Labels: []prompb.Label{{Name: "__name__", Value: "b"}, {Name: "cluster", Value: "east"}, {Name: "x", Value: "1"}}, Samples: []prompb.Sample{{Timestamp: 1000, Value: 7}}},
			},
			wantEnd: 5000, wantKnown: true, wantOrder: "a3 a1 b1east",
			wantAsked: []string{`/api/v1/series?end=10.000&match[]={cluster="prod"}&start=-291.000`, `/api/v1/series?end=10.000&match[]={x="2"}&start=-291.000`, "/api/v1/status/tsdb/blocks?", everything}},
	} {
		if c.read == nil {
			c.read = byLabel
		}
		read, _ := (&prompb.ReadResponse{Results: []*prompb.QueryResult{{Timeseries: c.read}}}).Marshal()
		var asked []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			query, _ := url.QueryUnescape(r.URL.RawQuery)
			answer := func(with string) {
				if code, rest, coded := strings.Cut(with, " "); coded && len(code) == 3 && code[0] != '{' {
					n, _ := strconv.Atoi(code)
					w.WriteHeader(n)
					with = rest
				} else if with == "" {
					w.WriteHeader(http.StatusNotFound)
					with = "404 page not found"
				}
				io.WriteString(w, with)
			}
			switch r.URL.Path {
			case "/api/v1/read":
				w.Write(snappy.Encode(nil, read))
			case "/api/v1/status/config":
				answer(c.config)
			case "/api/v1/status/tsdb/blocks":
				asked = append(asked, r.URL.Path+"?"+query)
				answer(c.blocks)
			case "/api/v1/status/tsdb":
				asked = append(asked, r.URL.Path+"?"+query)
				answer(c.status)
			case "/api/v1/labels":
				asked = append(asked, r.URL.Path+"?"+query)
				answer(c.older)
			case "/api/v1/series":
				asked = append(asked, r.URL.Path+"?"+query)
				if one, named := c.listed[r.FormValue("match[]")]; named {
					answer(one)
				} else {
					answer(c.listed[""])
				}
			default:
				http.NotFound(w, r)
			}
		}))
		store, err := Export(context.Background(), srv.Client(), srv.URL, time.UnixMilli(10000), time.Second, c.selectors...)
		srv.Close()
		if err != nil || store.HeadFrom != c.wantEnd || store.OrderKnown != c.wantKnown || held(store) != c.wantOrder || !reflect.DeepEqual(asked, c.wantAsked) {
			t.Errorf("%s: its blocks are said to end at %d, its order to be known: %v, and its series are held %q, after asking %q (%v);\nwant %d, %v, %q, %q",
				c.what, store.HeadFrom, store.OrderKnown, held(store), asked, err, c.wantEnd, c.wantKnown, c.wantOrder, c.wantAsked)
		}
		if c.config != "" && !reflect.DeepEqual(store.ExternalLabels, map[string]string{"cluster": "prod", "x": "2"}) {
			t.Errorf("%s: the export says it took off %v", c.what, store.ExternalLabels)
		}
	}

	// A replay: three equal series the head made 3, 1, 2, from a head that began at 5s, looking back two seconds.
	store := &Store{HeadFrom: 5000, Source: Source{LookbackDelta: 2 * time.Second}}
	for _, x := range []string{"3", "1", "2"} {
		store.Add(map[string]string{"__name__": "a", "x": x}, []int64{1000, 2000, 3000, 4000, 5000, 6000, 7000, 8000, 9000, 10000}, []float64{7, 7, 7, 7, 7, 7, 7, 7, 7, 7})
	}
	order := func(api *API, path string, params url.Values) string {
		res := call(t, api, path, params)
		var body struct {
			Data json.RawMessage
		}
		var vector struct {
			Result []struct{ Metric map[string]string }
		}
		var listed []map[string]string
		if json.Unmarshal(res.Raw, &body); res.Status != "success" {
			t.Fatalf("%s %v: %s %q", path, params, res.Status, res.Error)
		}
		out := ""
		if json.Unmarshal(body.Data, &vector) == nil {
			for _, r := range vector.Result {
				out += r.Metric["x"]
			}
		} else if json.Unmarshal(body.Data, &listed) == nil {
			for _, lset := range listed {
				out += lset["x"]
			}
		}
		return out
	}
	api, unknown := NewAPI(store, time.UnixMilli(10000)), &Store{Source: store.Source}
	unknown.series = store.series
	for _, c := range []struct {
		what, path string
		params     url.Values
		want       string
	}{
		{"an instant in the head", "/api/v1/query", url.Values{"query": {`a`}}, "312"},
		{"which of three equals topk keeps, in the head", "/api/v1/query", url.Values{"query": {`topk(1, a)`}}, "3"},
		{"an instant that looks back to where the blocks ended, to the millisecond", "/api/v1/query", url.Values{"query": {`a`}, "time": {"6.999"}}, "312"},
		{"an instant that looks back a millisecond before it", "/api/v1/query", url.Values{"query": {`a`}, "time": {"6.998"}}, "123"},
		{"which of three equals topk keeps, where a block is reached", "/api/v1/query", url.Values{"query": {`topk(1, a)`}, "time": {"6"}}, "1"},
		{"an instant in the head, of a query that also looks back past it", "/api/v1/query", url.Values{"query": {`a and a offset 6s`}}, "123"},
		{"a range that stays in the head", "/api/v1/query", url.Values{"query": {`max_over_time(a[4s])`}}, "312"},
		{"a range that reaches a block", "/api/v1/query", url.Values{"query": {`max_over_time(a[6s])`}}, "123"},
		{"a query that asks for an order has it", "/api/v1/query", url.Values{"query": {`sort_desc(a + on (x) group_left () (a * 0 + (a == bool 7)))`}}, "312"},
		{"the series of one selector, with no start", "/api/v1/series", url.Values{"match[]": {`a`}}, "123"},
		{"the series of one selector, from where the blocks ended", "/api/v1/series", url.Values{"match[]": {`a`}, "start": {"5"}}, "312"},
		{"the series of one selector, from a millisecond before", "/api/v1/series", url.Values{"match[]": {`a`}, "start": {"4.999"}}, "123"},
		{"the series of two selectors, in the head", "/api/v1/series", url.Values{"match[]": {`a`, `{x="1"}`}, "start": {"8"}}, "123"},
		{"the series of one selector, from where the blocks ended to the freeze", "/api/v1/series", url.Values{"match[]": {`a`}, "start": {"5"}, "end": {"10"}}, "312"},
		{"the series of one selector, over a second of the head", "/api/v1/series", url.Values{"match[]": {`a`}, "start": {"7.5"}, "end": {"8.5"}}, "312"},
		{"the first two series of one selector, in the head", "/api/v1/series", url.Values{"match[]": {`a`}, "start": {"5"}, "limit": {"2"}}, "31"},
	} {
		if got := order(api, c.path, c.params); got != c.want {
			t.Errorf("%s: the series came %q, want %q", c.what, got, c.want)
		}
		// A case that does not say where its blocks ended hands them over as it holds them, whatever is asked.
		if got := order(NewAPI(unknown, time.UnixMilli(10000)), c.path, c.params); len(c.params["match[]"]) < 2 && !strings.Contains(c.params.Get("query"), "sort") && len(got) == 3 && got != "312" {
			t.Errorf("%s, of a case that does not say where its blocks ended: the series came %q", c.what, got)
		}
	}
	// Where the blocks ended is held to the millisecond, of a listing's start as of a query's: a case
	// whose blocks ended a millisecond after five seconds.
	late := &Store{HeadFrom: 5001, Source: store.Source}
	late.series = store.series
	for start, want := range map[string]string{"5.001": "312", "5": "123"} {
		if got := order(NewAPI(late, time.UnixMilli(10000)), "/api/v1/series", url.Values{"match[]": {`a`}, "start": {start}}); got != want {
			t.Errorf("the series of one selector from %s, of a case whose blocks ended at 5.001: they came %q, want %q", start, got, want)
		}
	}
}

// A Prometheus adds its external labels to every series a remote read returns, and a series may have
// had such a label of its own. Which, what is read does not say; the Prometheus's listing of its own
// series does. A label is taken off only where what is left is a series the Prometheus has.
func TestExternalLabelsAreTakenOffWhatWasRead(t *testing.T) {
	series := func(pairs ...string) *prompb.TimeSeries {
		out := &prompb.TimeSeries{Samples: []prompb.Sample{{Timestamp: 9000, Value: 1}}}
		for i := 0; i < len(pairs); i += 2 {
			out.Labels = append(out.Labels, prompb.Label{Name: pairs[i], Value: pairs[i+1]})
		}
		return out
	}
	held := func(store *Store) []string {
		var out []string
		for _, se := range store.series {
			out = append(out, se.lset.String())
		}
		sort.Strings(out)
		return out
	}
	const config = `{"status":"success","data":{"yaml":"global:\n  external_labels:\n    cluster: prod\n    job: api\n"}}`
	for _, c := range []struct {
		what string
		read []*prompb.TimeSeries
		own  map[string]string // by the selector asked: the series that have the label of their own
		want []string
	}{
		{"no series has either of its own", []*prompb.TimeSeries{series("__name__", "up", "cluster", "prod", "job", "api"), series("__name__", "up", "cluster", "prod", "job", "db")},
			map[string]string{`{cluster="prod"}`: `[]`, `{job="api"}`: `[]`}, []string{`{__name__="up", job="db"}`, `{__name__="up"}`}},
		{"one has the cluster of its own, and the job was added to it", []*prompb.TimeSeries{series("__name__", "up", "cluster", "prod", "job", "api"), series("__name__", "up", "cluster", "prod", "job", "db")},
			map[string]string{`{cluster="prod"}`: `[{"__name__":"up","cluster":"prod"}]`, `{job="api"}`: `[]`}, []string{`{__name__="up", cluster="prod"}`, `{__name__="up", job="db"}`}},
		{"one has the job of its own, as most have a job", []*prompb.TimeSeries{series("__name__", "up", "cluster", "prod", "job", "api"), series("__name__", "up", "cluster", "prod", "job", "db")},
			map[string]string{`{cluster="prod"}`: `[]`, `{job="api"}`: `[{"__name__":"up","job":"api"}]`}, []string{`{__name__="up", job="api"}`, `{__name__="up", job="db"}`}},
		{"one has both of its own", []*prompb.TimeSeries{series("__name__", "up", "cluster", "prod", "job", "api")},
			map[string]string{`{cluster="prod"}`: `[{"__name__":"up","cluster":"prod","job":"api"}]`, `{job="api"}`: `[{"__name__":"up","cluster":"prod","job":"api"}]`}, []string{`{__name__="up", cluster="prod", job="api"}`}},
		{"another series has the cluster of its own, with other labels", []*prompb.TimeSeries{series("__name__", "up", "cluster", "prod", "job", "db")},
			map[string]string{`{cluster="prod"}`: `[{"__name__":"down","cluster":"prod","job":"db"}]`, `{job="api"}`: `[]`}, []string{`{__name__="up", job="db"}`}},
		// Where it will not say which series have the label of their own, the label is taken off wherever it has the value.
		{"it will not list them", []*prompb.TimeSeries{series("__name__", "up", "cluster", "prod", "job", "api"), series("__name__", "up", "cluster", "east", "job", "api2")},
			map[string]string{`{cluster="prod"}`: `error`, `{job="api"}`: `error`}, []string{`{__name__="up", cluster="east", job="api2"}`, `{__name__="up"}`}},
		{"it lists nothing where a list should be", []*prompb.TimeSeries{series("__name__", "up", "cluster", "prod", "job", "api")},
			map[string]string{`{cluster="prod"}`: `null`, `{job="api"}`: `null`}, []string{`{__name__="up"}`}},
		// A listing names a series whole: one that was had says of a series what another, not had, would have.
		{"one has both of its own, and only one of the two listings is had", []*prompb.TimeSeries{series("__name__", "up", "cluster", "prod", "job", "api")},
			map[string]string{`{cluster="prod"}`: `[{"__name__":"up","cluster":"prod","job":"api"}]`, `{job="api"}`: `error`}, []string{`{__name__="up", cluster="prod", job="api"}`}},
		{"one has the job of its own, and that listing is not had", []*prompb.TimeSeries{series("__name__", "up", "cluster", "prod", "job", "api")},
			map[string]string{`{cluster="prod"}`: `[]`, `{job="api"}`: `error`}, []string{`{__name__="up"}`}},
		// Two listed series that one read series could be, each with one label of its own: the first by name, every time.
		{"it could be either of two", []*prompb.TimeSeries{series("__name__", "up", "cluster", "prod", "job", "api")},
			map[string]string{`{cluster="prod"}`: `[{"__name__":"up","cluster":"prod"}]`, `{job="api"}`: `[{"__name__":"up","job":"api"}]`}, []string{`{__name__="up", cluster="prod"}`}},
		// Of two listed series it could be, one with more labels of its own than the other: the one that takes the fewest off.
		{"it could be either of two, one of them whole", []*prompb.TimeSeries{series("__name__", "up", "cluster", "prod", "job", "api")},
			map[string]string{`{cluster="prod"}`: `[{"__name__":"up","cluster":"prod"},{"__name__":"up","cluster":"prod","job":"api"}]`, `{job="api"}`: `[{"__name__":"up","cluster":"prod","job":"api"}]`}, []string{`{__name__="up", cluster="prod", job="api"}`}},
		// And a listed series is not the one that was read if it has a label that one was not read with:
		// what a store sends that does not add every external label is not made up to one that does.
		{"a listed series has an external label that what was read has not", []*prompb.TimeSeries{series("__name__", "up", "cluster", "prod")},
			map[string]string{`{cluster="prod"}`: `[{"__name__":"up","cluster":"prod","job":"api"}]`, `{job="api"}`: `[{"__name__":"up","cluster":"prod","job":"api"}]`}, []string{`{__name__="up"}`}},
		// A listed series with a label the read one has not, or with another value, is another series.
		{"a listed series has a label more than what was read", []*prompb.TimeSeries{series("__name__", "up", "cluster", "prod", "job", "api")},
			map[string]string{`{cluster="prod"}`: `[{"__name__":"up","cluster":"prod","zone":"a"}]`, `{job="api"}`: `[]`}, []string{`{__name__="up"}`}},
	} {
		for again := 0; again < 20 && c.what == "it could be either of two"; again++ { // the order of a map is another each time
			e := &externalLabels{of: map[string]string{"cluster": "prod", "job": "api"}, listed: map[string][]map[string]string{`{__name__="up"}`: {{"__name__": "up", "job": "api"}, {"__name__": "up", "cluster": "prod"}}}}
			if got := labels.FromMap(e.takenOff(map[string]string{"__name__": "up", "cluster": "prod", "job": "api"})).String(); got != c.want[0] {
				t.Fatalf("%s, listed the other way round: it is taken for %s", c.what, got)
			}
		}
		read, _ := (&prompb.ReadResponse{Results: []*prompb.QueryResult{{Timeseries: c.read}}}).Marshal()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/read":
				w.Write(snappy.Encode(nil, read))
			case "/api/v1/status/config":
				io.WriteString(w, config)
			case "/api/v1/series":
				if own := c.own[r.FormValue("match[]")]; own == "error" || own == "" { // (and "null" is an answer with no list in it)
					io.WriteString(w, `{"status":"error","error":"no"}`)
				} else {
					io.WriteString(w, `{"status":"success","data":`+own+`}`)
				}
			default:
				http.NotFound(w, r)
			}
		}))
		store, err := Export(context.Background(), srv.Client(), srv.URL, time.UnixMilli(10000), time.Second)
		srv.Close()
		if err != nil || !reflect.DeepEqual(held(store), c.want) || !reflect.DeepEqual(store.ExternalLabels, map[string]string{"cluster": "prod", "job": "api"}) {
			t.Errorf("%s: the case holds %q (%v), having taken off %v; want %q", c.what, held(store), err, store.ExternalLabels, c.want)
		}
	}

	// What it is asked, for a label whose name or value is not plain; and that each listing has its own
	// time to answer in: one that never does costs that long, and the next is asked all the same.
	waited := describeTimeout
	describeTimeout = 300 * time.Millisecond
	defer func() { describeTimeout = waited }()
	var asked []string
	read, _ := (&prompb.ReadResponse{Results: []*prompb.QueryResult{{Timeseries: []*prompb.TimeSeries{series("__name__", "up", "a", "never", "k8s.cluster", "prod", "zone", `n"or\th`)}}}}).Marshal()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/read":
			w.Write(snappy.Encode(nil, read))
		case "/api/v1/status/config":
			io.WriteString(w, `{"status":"success","data":{"yaml":"global:\n  external_labels:\n    a: never\n    k8s.cluster: prod\n    zone: 'n\"or\\th'\n"}}`)
		case "/api/v1/series":
			asked = append(asked, r.FormValue("match[]"))
			switch r.FormValue("match[]") {
			case `{a="never"}`:
				<-r.Context().Done() // it never answers
			case `{zone="n\"or\\th"}`:
				io.WriteString(w, `{"status":"success","data":[{"__name__":"up","zone":"n\"or\\th"}]}`)
			default:
				io.WriteString(w, `{"status":"success","data":[]}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	began := time.Now()
	store, err := Export(context.Background(), srv.Client(), srv.URL, time.UnixMilli(10000), time.Second)
	srv.Close()
	if want := []string{`{a="never"}`, `{"k8s.cluster"="prod"}`, `{zone="n\"or\\th"}`}; err != nil || !reflect.DeepEqual(asked, want) || !reflect.DeepEqual(held(store), []string{`{__name__="up", zone="n\"or\\th"}`}) || time.Since(began) > 10*time.Second {
		t.Errorf("a Prometheus with a label it never lists, one whose name is not plain and one whose value is not: asked %q in %v and holds %q (%v); want %q", asked, time.Since(began), held(store), err, want)
	}

	// Two series that were read alike: one has the label of its own, the other was given it, and which
	// samples are whose is not in what was read. That is refused.
	read, _ = (&prompb.ReadResponse{Results: []*prompb.QueryResult{{Timeseries: []*prompb.TimeSeries{series("__name__", "up", "cluster", "prod"), series("__name__", "up", "cluster", "prod")}}}}).Marshal()
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/read":
			w.Write(snappy.Encode(nil, read))
		case "/api/v1/status/config":
			io.WriteString(w, `{"status":"success","data":{"yaml":"global:\n  external_labels:\n    cluster: prod\n"}}`)
		case "/api/v1/series":
			io.WriteString(w, `{"status":"success","data":[{"__name__":"up","cluster":"prod"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	_, err = Export(context.Background(), srv.Client(), srv.URL, time.UnixMilli(10000), time.Second)
	srv.Close()
	if err == nil || !strings.Contains(err.Error(), `two series were read as {__name__="up", cluster="prod"}`) {
		t.Errorf("two series read alike: %v", err)
	}
	// And the same series under two selectors is one series, as it always was.
	read, _ = (&prompb.ReadResponse{Results: []*prompb.QueryResult{{Timeseries: []*prompb.TimeSeries{series("__name__", "up")}}, {Timeseries: []*prompb.TimeSeries{series("__name__", "up")}}}}).Marshal()
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/read" {
			w.Write(snappy.Encode(nil, read))
			return
		}
		http.NotFound(w, r)
	}))
	store, err = Export(context.Background(), srv.Client(), srv.URL, time.UnixMilli(10000), time.Second, "up", `{__name__="up"}`)
	srv.Close()
	if err != nil || !reflect.DeepEqual(held(store), []string{`{__name__="up"}`}) {
		t.Errorf("one series under two selectors: the case holds %q (%v)", held(store), err)
	}

	// A label's name as a selector takes it: plain where it is made of what a name was once held to, and in quotes otherwise.
	for name, want := range map[string]string{"cluster": "cluster", "_a9": "_a9", "k8s.cluster": `"k8s.cluster"`, "9lives": `"9lives"`, "": `""`, "온도": `"온도"`} {
		if got := selectorName(name); got != want {
			t.Errorf("the label name %q is asked for as %s, want %s", name, got, want)
		}
	}

	// Many external labels are no more work than few: a series read with twenty-four of them.
	many := &externalLabels{of: map[string]string{}, listed: map[string][]map[string]string{}}
	with := map[string]string{"__name__": "up"}
	for i := 0; i < 24; i++ {
		many.of[fmt.Sprintf("l%d", i)], with[fmt.Sprintf("l%d", i)] = "v", "v"
	}
	began = time.Now()
	for i := 0; i < 2000; i++ {
		if got := many.takenOff(with); len(got) != 1 {
			t.Fatalf("a series read with twenty-four external labels is held as %v", got)
		}
	}
	if time.Since(began) > 5*time.Second {
		t.Errorf("two thousand series with twenty-four external labels took %v", time.Since(began))
	}
}

// A listing of series is of the whole case, whatever window it is asked for: which series a
// Prometheus lists for a window goes by its chunks, and a case has none. The window's start is read
// for the order alone (TestACaseKeepsTheOrderOfItsPrometheusHead).
func TestAListingOfSeriesIsOfTheWholeCase(t *testing.T) {
	store := &Store{}
	for _, name := range []string{"whole", "ended", "began"} {
		ts := map[string][]int64{"whole": {1000, 4000, 7000, 10000}, "ended": {1000, 2000, 3000, 4000}, "began": {8000, 9000, 10000}}[name]
		store.Add(map[string]string{"__name__": name}, ts, make([]float64, len(ts)))
	}
	api := NewAPI(store, time.UnixMilli(10000))
	for what, params := range map[string]url.Values{"no window": {}, "the second half": {"start": {"5"}, "end": {"10"}}, "before the case": {"end": {"0.5"}}, "a day after it": {"start": {"86409"}, "end": {"86410"}},
		"a start and no end": {"start": {"9"}}, "a window the wrong way round": {"start": {"8"}, "end": {"3"}}} {
		params["match[]"] = []string{`{__name__=~".+"}`}
		res := call(t, api, "/api/v1/series", params)
		var body struct{ Data []map[string]string }
		json.Unmarshal(res.Raw, &body)
		var got []string
		for _, lset := range body.Data {
			got = append(got, lset["__name__"])
		}
		if res.Status != "success" || strings.Join(got, " ") != "whole ended began" {
			t.Errorf("the series of %s: %q (%s %s), want all three as the case holds them", what, strings.Join(got, " "), res.Status, res.Error)
		}
	}
	// And the endpoints a Prometheus has for GET alone are refused another method, as it refuses them;
	// the ones a query may be too long for are not.
	// And a path that is not served is not served to a POST either, as it is not to a GET: what a
	// Prometheus has for POST and a case has not, and what neither has.
	for path, want := range map[string]int{"/api/v1/label/__name__/values": 405, "/api/v1/targets": 405, "/api/v1/rules": 405, "/api/v1/alerts": 405, "/api/v1/metadata": 405, "/api/v1/status/buildinfo": 405,
		"/api/v1/labels": 200, "/api/v1/series?match[]=whole": 200, "/api/v1/query?query=whole": 200, "/api/v1/query_range?query=whole&start=1&end=2&step=1": 200, "/api/v1/query_exemplars": 200,
		"/api/v1/no_such_endpoint": 404, "/api/v1/read": 404, "/api/v1/format_query": 404, "/api/v1/label/__name__/names": 404, "/api/v1/label/a/b/values": 404} {
		rec := httptest.NewRecorder()
		api.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code != want {
			t.Errorf("POST %s was answered %d, want %d", path, rec.Code, want)
		}
	}
}

// An agent asks what kind of metric a name is before it takes a rate of it — one of round 38 asked a
// case five times — and a Prometheus answers from what its targets last said. A case carries that
// answer, and gives it as a Prometheus does.
func TestACaseSaysWhatKindAMetricIs(t *testing.T) {
	read, _ := (&prompb.ReadResponse{Results: []*prompb.QueryResult{{Timeseries: []*prompb.TimeSeries{
		{Labels: []prompb.Label{{Name: "__name__", Value: "up"}}, Samples: []prompb.Sample{{Timestamp: 1000, Value: 1}}},
	}}}}).Marshal()
	counter, gauge := Metadata{Type: "counter", Help: "Requests served.", Unit: ""}, Metadata{Type: "gauge", Help: `Temperature "as read" \ per room, in °C.`, Unit: "celsius"}
	// Two targets that do not agree about one family, in the order a map happened to give them.
	described := `{"status":"success","data":{"temp_celsius":[{"type":"gauge","help":"Temperature \"as read\" \\ per room, in °C.","unit":"celsius"}],` +
		`"queue_depth":[{"type":"unknown","help":"Items waiting.","unit":""},{"type":"gauge","help":"Items waiting.","unit":""},{"type":"gauge","help":"Items in the queue.","unit":""}],` +
		`"requests_total":[{"type":"counter","help":"Requests served.","unit":""}]}}`
	want := map[string][]Metadata{"temp_celsius": {gauge}, "requests_total": {counter},
		"queue_depth": {{Type: "gauge", Help: "Items in the queue."}, {Type: "gauge", Help: "Items waiting."}, {Type: "unknown", Help: "Items waiting."}}}
	for _, c := range []struct {
		what, answer string
		code         int
		want         map[string][]Metadata
	}{
		{"a Prometheus", described, 200, want},
		{"one that has no target", `{"status":"success","data":{}}`, 200, map[string][]Metadata{}},
		{"a store that does not have the endpoint", `404 page not found`, 404, nil},
		{"one that refuses", `{"status":"error","errorType":"unavailable","error":"no"}`, 503, nil},
		{"one that sends it with a status that is not 200", described, 503, nil},
		{"one that sends it under an error", strings.Replace(described, `"success"`, `"error"`, 1), 200, nil},
		{"one that says success and sends nothing", `{"status":"success"}`, 200, nil},
		{"one that does not send JSON", `<html>`, 200, nil},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/read":
				w.Write(snappy.Encode(nil, read))
			case "/api/v1/metadata":
				w.WriteHeader(c.code)
				io.WriteString(w, c.answer)
			default:
				http.NotFound(w, r)
			}
		}))
		store, err := Export(context.Background(), srv.Client(), srv.URL, time.UnixMilli(2000), time.Minute)
		srv.Close()
		if err != nil || !reflect.DeepEqual(store.Metadata, c.want) || (store.Metadata == nil) != (c.want == nil) {
			t.Errorf("%s: the export holds %#v of its metrics (%v), want %#v", c.what, store.Metadata, err, c.want)
		}
	}

	// Written and read again it is the same, and the same bytes whatever order it was held in.
	dir := t.TempDir()
	shuffled := map[string][]Metadata{"requests_total": {counter}, "temp_celsius": {gauge}, "queue_depth": {want["queue_depth"][2], want["queue_depth"][0], want["queue_depth"][1]}}
	asHeld := append([]Metadata(nil), shuffled["queue_depth"]...)
	if err := SaveMetadata(filepath.Join(dir, "a.json"), want); err != nil {
		t.Fatal(err)
	}
	if err := SaveMetadata(filepath.Join(dir, "b.json"), shuffled); err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(asHeld, want["queue_depth"]) || !reflect.DeepEqual(shuffled["queue_depth"], asHeld) { // what is written is put in order, and what was handed over is left as it was
		t.Errorf("writing the metadata left the caller's own as %v, which it held as %v", shuffled["queue_depth"], asHeld)
	}
	a, _ := os.ReadFile(filepath.Join(dir, "a.json"))
	b, _ := os.ReadFile(filepath.Join(dir, "b.json"))
	back, err := LoadMetadata(filepath.Join(dir, "a.json"))
	if err != nil || !reflect.DeepEqual(back, want) || !bytes.Equal(a, b) || !bytes.HasPrefix(a, []byte("{\n \"queue_depth\": [\n")) {
		t.Errorf("the metadata file read back as %#v (%v); written twice:\n%s\n%s", back, err, a, b)
	}
	if _, err := LoadMetadata(filepath.Join(dir, "none.json")); err == nil {
		t.Error("a metadata file that is not there was read")
	}
	// The file ends in a newline, as a file does; and one written by another hand, its entries in any
	// order and two of them apart only in their unit, is read into the one order — by type, help, unit.
	if !bytes.HasSuffix(a, []byte("\n ]\n}\n")) {
		t.Errorf("the metadata file ends %q", a[len(a)-8:])
	}
	os.WriteFile(filepath.Join(dir, "hand.json"), []byte(`{"d": [{"type":"gauge","help":"h","unit":"seconds"},{"type":"gauge","help":"h","unit":"bytes"},{"type":"counter","help":"z","unit":""}]}`), 0o644)
	byHand, err := LoadMetadata(filepath.Join(dir, "hand.json"))
	if err != nil || !reflect.DeepEqual(byHand["d"], []Metadata{{Type: "counter", Help: "z"}, {Type: "gauge", Help: "h", Unit: "bytes"}, {Type: "gauge", Help: "h", Unit: "seconds"}}) {
		t.Errorf("a metadata file written in another order was read as %v (%v)", byHand, err)
	}
	os.WriteFile(filepath.Join(dir, "broken.json"), []byte(`{"d": [`), 0o644)
	if _, err := LoadMetadata(filepath.Join(dir, "broken.json")); err == nil {
		t.Error("a metadata file that is not JSON was read")
	}
	// And it is written in that order whatever order it is held in.
	SaveMetadata(filepath.Join(dir, "c.json"), map[string][]Metadata{"d": {{Type: "gauge", Help: "h", Unit: "seconds"}, {Type: "counter", Help: "z"}, {Type: "gauge", Help: "h", Unit: "bytes"}}})
	if c, _ := os.ReadFile(filepath.Join(dir, "c.json")); bytes.Index(c, []byte("counter")) > bytes.Index(c, []byte("bytes")) || bytes.Index(c, []byte("bytes")) > bytes.Index(c, []byte("seconds")) {
		t.Errorf("metadata held in another order was written as\n%s", c)
	}

	store := &Store{Metadata: want}
	store.Add(map[string]string{"__name__": "up"}, []int64{1000}, []float64{1})
	api := NewAPI(store, time.UnixMilli(2000))
	all := `{"queue_depth":[{"type":"gauge","help":"Items in the queue.","unit":""},{"type":"gauge","help":"Items waiting.","unit":""},{"type":"unknown","help":"Items waiting.","unit":""}],` +
		`"requests_total":[{"type":"counter","help":"Requests served.","unit":""}],"temp_celsius":[{"type":"gauge","help":"Temperature \"as read\" \\ per room, in °C.","unit":"celsius"}]}`
	for _, c := range []struct {
		params url.Values
		code   int
		want   string
	}{
		{url.Values{}, 200, all},
		{url.Values{"metric": {"requests_total"}}, 200, `{"requests_total":[{"type":"counter","help":"Requests served.","unit":""}]}`},
		{url.Values{"metric": {"requests"}}, 200, `{}`}, // a family by its whole name, as a Prometheus looks it up
		{url.Values{"metric": {"no_such_metric"}}, 200, `{}`},
		{url.Values{"limit": {"2"}}, 200, all[:strings.Index(all, `,"temp_celsius"`)] + `}`},
		{url.Values{"limit": {"0"}}, 200, `{}`},
		{url.Values{"limit": {"-1"}}, 200, all},
		{url.Values{"limit": {"900"}}, 200, all},
		{url.Values{"limit_per_metric": {"1"}}, 200, `{"queue_depth":[{"type":"gauge","help":"Items in the queue.","unit":""}],` + all[strings.Index(all, `"requests_total"`):]},
		{url.Values{"limit_per_metric": {"2"}, "metric": {"queue_depth"}}, 200, `{"queue_depth":[{"type":"gauge","help":"Items in the queue.","unit":""},{"type":"gauge","help":"Items waiting.","unit":""}]}`},
		{url.Values{"limit_per_metric": {"0"}}, 200, all}, // nothing a Prometheus limits by: it reads a limit only above nothing
		{url.Values{"limit_per_metric": {"-4"}}, 200, all},
		{url.Values{"limit": {"1"}, "limit_per_metric": {"1"}}, 200, `{"queue_depth":[{"type":"gauge","help":"Items in the queue.","unit":""}]}`},
		{url.Values{"limit": {"some"}}, 400, `limit must be a number`},
		{url.Values{"limit": {"1.5"}}, 400, `limit must be a number`},
		{url.Values{"limit_per_metric": {"few"}}, 400, `limit_per_metric must be a number`},
	} {
		res := call(t, api, "/api/v1/metadata", c.params)
		var body struct {
			Data json.RawMessage
		}
		json.Unmarshal(res.Raw, &body)
		var got, wanted any
		json.Unmarshal(body.Data, &got)
		json.Unmarshal([]byte(c.want), &wanted)
		if c.code == 200 && (res.Code != 200 || res.Status != "success" || !reflect.DeepEqual(got, wanted) || got == nil) {
			t.Errorf("metadata with %v: %d %s\n got %s\nwant %s", c.params, res.Code, res.Status, body.Data, c.want)
		}
		if c.code != 200 && (res.Code != c.code || res.Kind != "bad_data" || res.Error != c.want) {
			t.Errorf("metadata with %v: %d %s %q, want %d bad_data %q", c.params, res.Code, res.Kind, res.Error, c.code, c.want)
		}
	}
	// As a Prometheus has it, the endpoint is for GET alone.
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/metadata", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/v1/metadata was answered %d %s", rec.Code, rec.Body)
	}
	// A case that carries none says it knows of none, and is not refused.
	store.Metadata = nil
	if res := call(t, NewAPI(store, time.UnixMilli(2000)), "/api/v1/metadata", url.Values{"metric": {"up"}}); res.Code != 200 || !strings.Contains(string(res.Raw), `"data":{}`) {
		t.Errorf("a case with no metadata answered %d %s", res.Code, res.Raw)
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
	check := NewAPI(held, time.UnixMilli(60_000))
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
		api := NewAPI(s, at)
		if got := clients(call(t, api, "/api/v1/query", url.Values{"query": {"x"}})); !reflect.DeepEqual(got, given) {
			t.Errorf("%s, the store handed its series over as %v, want %v", what, got, given)
		}
		if got := clients(call(t, api, "/api/v1/query", url.Values{"query": {"sum by (client) (x)"}})); !reflect.DeepEqual(got, given) {
			t.Errorf("%s, an aggregation's groups came out as %v, want %v", what, got, given)
		}
	}
	// Which of equals `topk` keeps is the order's doing: the same four series, handed over the other way round, and it keeps others.
	kept := func(s *Store) []string {
		got := clients(call(t, NewAPI(s, at), "/api/v1/query", url.Values{"query": {"topk(2, x)"}}))
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
	api := NewAPI(store, at)
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
	api := NewAPI(store, freeze)
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
	api := NewAPI(store, freeze).Handler()
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
	srv := httptest.NewServer(NewAPI(store, freeze).Handler())
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
	text, err := NewAPI(store, freeze).Text(context.Background(), AllSeries)
	if err != nil || strings.Count(text, "\n") != 15 || !strings.Contains(text, "thumb_requests_total{client=catalog-indexer,code=200,") {
		t.Errorf("Text(all series) = %d lines (%v)", strings.Count(text, "\n"), err)
	}
	if _, err := NewAPI(store, freeze).Text(context.Background(), "sum("); err == nil {
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

// What an export learned of its Prometheus besides the samples is kept in a file beside the metrics
// file, since a file of samples does not hold it; read back with that file, the store is what the
// export made.
func TestWhatAnExportLearnedIsKeptBesideItsFile(t *testing.T) {
	made := func() *Store {
		s := &Store{Source: Source{Version: "3.15.0", EvaluationInterval: 15 * time.Second, LookbackDelta: 2 * time.Minute},
			From: 1000, HeadFrom: 5000, OrderKnown: true, ExternalLabels: map[string]string{"cluster": "prod"},
			Metadata: map[string][]Metadata{"up": {{Type: "gauge", Help: "z"}, {Type: "counter", Help: "a"}}}}
		s.Add(map[string]string{"__name__": "up", "job": "z"}, []int64{1000, 9000}, []float64{1, 1})
		s.Add(map[string]string{"__name__": "up", "job": "a"}, []int64{2000}, []float64{0})
		return s
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "m.jsonl.gz")
	exported := made()
	if err := exported.Save(file); err != nil {
		t.Fatal(err)
	}
	if err := exported.SaveLearned(file); err != nil {
		t.Fatal(err)
	}
	if got := exported.Metadata["up"]; got[0].Type != "gauge" { // what was handed over is left in the order it was in
		t.Errorf("writing what was learned put the store's own metadata in another order: %v", got)
	}
	back, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	if back.From != 0 || back.OrderKnown || back.Metadata != nil { // the metrics file alone holds none of it
		t.Fatalf("a metrics file read alone says %d, %v, %v", back.From, back.OrderKnown, back.Metadata)
	}
	found, err := back.LoadLearned(file)
	want := made()
	sortMetadata(want.Metadata["up"])
	if err != nil || !found || back.Source != want.Source || back.From != 1000 || back.HeadFrom != 5000 || !back.OrderKnown ||
		!reflect.DeepEqual(back.ExternalLabels, want.ExternalLabels) || !reflect.DeepEqual(back.Metadata, want.Metadata) {
		t.Errorf("read back with what was learned (%v, %v), the store says %+v from %d, blocks to %d, order known %v, without %v, of its metrics %v",
			found, err, back.Source, back.From, back.HeadFrom, back.OrderKnown, back.ExternalLabels, back.Metadata)
	}
	// The same store leaves the same bytes, whatever order it held its metadata in; and they name what freeze.json names.
	first, _ := os.ReadFile(LearnedPath(file))
	again := made()
	again.Metadata["up"][0], again.Metadata["up"][1] = again.Metadata["up"][1], again.Metadata["up"][0]
	again.SaveLearned(file)
	second, _ := os.ReadFile(LearnedPath(file))
	if !bytes.Equal(first, second) || !bytes.HasSuffix(first, []byte("}\n")) {
		t.Errorf("the same store left other bytes the second time:\n%s\n%s", first, second)
	}
	for _, key := range []string{`"series": 2`, `"samples": 3`, `"oldest_ms": 1000`, `"newest_ms": 9000`, `"prometheus_version": "3.15.0"`, `"evaluation_interval_ms": 15000`, `"lookback_delta_ms": 120000`,
		`"from_ms": 1000`, `"head_from_ms": 5000`, `"series_order": "head"`, `"external_labels": {`, `"metadata": {`} {
		if !strings.Contains(string(first), key) {
			t.Errorf("what was learned does not say %s:\n%s", key, first)
		}
	}

	// A metrics file with nothing beside it is read as it is, and that is not an error.
	alone := filepath.Join(dir, "alone.jsonl.gz")
	exported.Save(alone)
	bare, _ := Load(alone)
	if found, err := bare.LoadLearned(alone); found || err != nil || bare.From != 0 || bare.OrderKnown || bare.Metadata != nil || bare.ExternalLabels != nil {
		t.Errorf("a metrics file with nothing beside it: found %v (%v), and the store says %d, %v", found, err, bare.From, bare.OrderKnown)
	}
	// A store that knows nothing leaves a file that says nothing, and reads back knowing nothing.
	plain := &Store{}
	plain.Add(map[string]string{"__name__": "up"}, []int64{1}, []float64{1})
	little := filepath.Join(dir, "little.jsonl.gz")
	plain.Save(little)
	plain.SaveLearned(little)
	said, _ := os.ReadFile(LearnedPath(little))
	reread, _ := Load(little)
	if found, err := reread.LoadLearned(little); !found || err != nil || reread.From != 0 || reread.OrderKnown || reread.Metadata != nil || strings.Contains(string(said), "series_order") || strings.Contains(string(said), "from_ms") {
		t.Errorf("a store that knows nothing: found %v (%v), reads back %+v, and left %s", found, err, reread.Source, said)
	}

	// What is beside another metrics file than the one it was written for is refused, by each of the
	// four numbers it is known by; and so is one that cannot be read, or says an order there is none of.
	// (what was written is of two series and three samples, from 1000 to 9000: each of these is other by one of the four)
	of := func(series ...[]int64) *Store {
		s := &Store{}
		for i, ts := range series {
			if err := s.Add(map[string]string{"__name__": "up", "job": fmt.Sprint(i)}, ts, make([]float64, len(ts))); err != nil {
				t.Fatal(err)
			}
		}
		return s
	}
	for what, another := range map[string]*Store{
		"with a series more":       of([]int64{1000}, []int64{9000}, []int64{2000}),
		"with a sample more":       of([]int64{1000, 2000, 9000}, []int64{2000}),
		"that begins a moment on":  of([]int64{1001, 9000}, []int64{2000}),
		"that ends a moment later": of([]int64{1000, 9001}, []int64{2000}),
	} {
		elsewhere := filepath.Join(dir, "other.jsonl.gz")
		another.Save(elsewhere)
		os.WriteFile(LearnedPath(elsewhere), first, 0o644)
		loaded, err := Load(elsewhere)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if found, err := loaded.LoadLearned(elsewhere); err == nil || found || !strings.Contains(err.Error(), "is of another metrics file") || loaded.From != 0 {
			t.Errorf("what was learned of one metrics file, beside another %s: found %v, %v, and the store now says it begins at %d", what, found, err, loaded.From)
		}
	}
	if got := LearnedPath("metrics.jsonl.gz"); got != "metrics.jsonl.gz.learned.json" { // the name the documents give it
		t.Errorf("what an export learned is kept in %s", got)
	}
	for what, content := range map[string]string{"that is no JSON": "{", "that says an order there is none of": strings.Replace(string(first), `"series_order": "head"`, `"series_order": "label"`, 1)} {
		os.WriteFile(LearnedPath(file), []byte(content), 0o644)
		loaded, _ := Load(file)
		if found, err := loaded.LoadLearned(file); err == nil || found || !strings.Contains(err.Error(), LearnedPath(file)) || loaded.From != 0 || loaded.OrderKnown {
			t.Errorf("a file of what was learned %s: found %v, %v", what, found, err)
		}
	}
}
