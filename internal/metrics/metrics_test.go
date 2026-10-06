package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/prompb"
)

// The sealed case this package was built against: fifteen series from a real Prometheus, frozen at this instant.
const (
	s2Metrics = "../../cases/s2-periodic-saturation/metrics.jsonl.gz"
	s2Freeze  = 1791228354430 // ms; the instant the reference reader was asked about
)

var stale = math.Float64frombits(value.StaleNaN)

type apiResponse struct {
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
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: %v: %s", path, err, rec.Body)
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

// The numbers are the ones Prometheus's own TSDB reader returned for the block this case was frozen
// from (docs/design-review-round37.md). An engine upgrade that moves them is a finding, not a flake.
func TestTheSealedCaseAnswersAsItsPrometheusDid(t *testing.T) {
	store, err := Load(s2Metrics)
	if err != nil {
		t.Fatal(err)
	}
	if n, m := store.Size(); n != 15 || m != 2064 {
		t.Fatalf("store holds %d series and %d samples, want 15 and 2064", n, m)
	}
	freeze := time.UnixMilli(s2Freeze)
	api := NewAPI(store, freeze, func() time.Time { return freeze })
	for query, want := range map[string]map[string]string{
		`sum by (client) (increase(thumb_requests_total[10m]))`: {
			"catalog-indexer": "5902.174954327807", "web-frontend": "729.0744049169664", "image-proxy": "434.6331352594811", "email-renderer": "171.45300673552757"},
		`sum by (code) (increase(thumb_requests_total[30m]))`:       {"200": "7683.171028965332", "503": "895.1931344568612"},
		`topk(1, sum by (client) (rate(thumb_requests_total[2m])))`: {"catalog-indexer": "10.148179067097987"},
		`sum(rate(thumb_requests_total{code="503"}[5m]))`:           {"": "1.413554530323626"},
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
	at := func(now time.Time, params url.Values) (string, float64) {
		params.Set("query", query)
		res := call(t, NewAPI(store, freeze, func() time.Time { return now }), "/api/v1/query", params)
		if len(res.Data.Result) != 1 {
			t.Fatalf("at %s with %v: %d results (%s)", now, params, len(res.Data.Result), res.Error)
		}
		return res.Data.Result[0].Value[1].(string), res.Data.Result[0].Value[0].(float64)
	}

	// "The last ten minutes", asked at the freeze and asked a month later, is the same ten minutes.
	then, _ := at(freeze, url.Values{})
	later := freeze.Add(31 * 24 * time.Hour)
	now, stamp := at(later, url.Values{})
	if then != now {
		t.Errorf("the answer drifted with the wall clock: %s at the freeze, %s a month later", then, now)
	}
	if want := float64(later.UnixMilli()) / 1000; stamp != want {
		t.Errorf("the answer is stamped %v; the caller's now is %v", stamp, want)
	}

	// "Five minutes ago" on the caller's clock is five minutes before the freeze.
	fiveAgo, _ := at(later, url.Values{"time": {seconds(later.Add(-5 * time.Minute).UnixMilli())}})
	direct, _ := at(freeze, url.Values{"time": {seconds(freeze.Add(-5 * time.Minute).UnixMilli())}})
	if fiveAgo != direct || fiveAgo == then {
		t.Errorf("five minutes ago: %s a month later, %s at the freeze, %s for now", fiveAgo, direct, then)
	}

	// A range is shifted at both ends, and comes back on the caller's clock.
	api := NewAPI(store, freeze, func() time.Time { return later })
	res := call(t, api, "/api/v1/query_range", url.Values{"query": {`thumb_inflight`}, "step": {"60"},
		"start": {seconds(later.Add(-10 * time.Minute).UnixMilli())}, "end": {seconds(later.UnixMilli())}})
	if len(res.Data.Result) != 1 || len(res.Data.Result[0].Values) != 11 {
		t.Fatalf("range query: %+v %s", res.Data.Result, res.Error)
	}
	if first, want := res.Data.Result[0].Values[0][0].(float64), float64(later.Add(-10*time.Minute).UnixMilli())/1000; first != want {
		t.Errorf("first point stamped %v, want %v", first, want)
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

	want := "{client=catalog-indexer} 5902.17\n{client=email-renderer} 171.453\n{client=image-proxy} 434.633\n{client=web-frontend} 729.074\n"
	if got := ask(`sum by (client) (increase(thumb_requests_total[10m]))`); got != want {
		t.Errorf("instant query printed\n%s\nwant\n%s", got, want)
	}
	// The decisive evidence of the case this store belongs to is a pattern over exactly this output.
	if rx := regexp.MustCompile(`client\W{1,4}catalog-indexer[\s\S]{0,80}?\d`); !rx.MatchString(want) {
		t.Error("the output no longer matches the evidence pattern in cases/s2-periodic-saturation/case.yaml")
	}
	got := ask(`thumb_inflight`, "--range", "2m", "--step", "60s")
	if lines := strings.Split(strings.TrimSpace(got), "\n"); len(lines) != 1 || !strings.HasPrefix(got, "thumb_inflight{instance=thumb-api.media.svc:8080,job=thumb-api,") {
		t.Errorf("range query printed %q", got)
	}
	if strings.Count(got, later.UTC().Format("15:04:05")+"=") != 1 {
		t.Errorf("the last point is not stamped with the caller's now (%s): %q", later.UTC().Format("15:04:05"), got)
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
}
