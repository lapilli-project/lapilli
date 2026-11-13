package prom

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// An answer to a range query keeps only what was asked: samples in the
// range, as many as it has steps, and series and samples up to what an
// answer may hold, past which it is too big and a shorter range is asked.
func TestAnswerBounds(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) int64 { return t0.Add(d).Unix() }
	series := func(values string) string {
		return `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"job":"api"},"values":[` + values + `]}]}}`
	}
	var asked atomic.Int32
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.Write([]byte(body.Load().(string)))
	}))
	defer srv.Close()
	c := New(srv.URL, "t")
	ask := func(b string) ([]Series, error) {
		body.Store(b)
		asked.Store(0)
		return c.QueryRange(context.Background(), "up", t0, t0.Add(10*time.Minute), time.Minute)
	}

	// Before the start, after the end: dropped.
	got, err := ask(series(fmt.Sprintf(`[%d,"1"],[%d,"2"],[%d,"3"],[%d,"4"]`, at(-time.Minute), at(0), at(10*time.Minute), at(11*time.Minute))))
	if err != nil || len(got) != 1 || len(got[0].Samples) != 2 || got[0].Samples[0].V != 2 || got[0].Samples[1].V != 3 {
		t.Errorf("samples out of the range: %v %v", got, err)
	}
	// More than the eleven steps asked for, in the range: unreadable.
	var many []string
	for i := range 12 {
		many = append(many, fmt.Sprintf(`[%d,"1"]`, at(time.Duration(i)*time.Second)))
	}
	if _, err := ask(series(strings.Join(many, ","))); err == nil || !strings.Contains(err.Error(), "more samples than the 11 steps") || !Refused(err) {
		t.Errorf("12 samples for 11 steps: %v", err)
	}
	// A series with no sample in the range is kept, as one with none.
	if got, err := ask(series(fmt.Sprintf(`[%d,"1"]`, at(time.Hour)))); err != nil || len(got) != 1 || len(got[0].Samples) != 0 {
		t.Errorf("a series with none in the range: %v %v", got, err)
	}

	// Past the series an answer may holds: too big, the range halved till
	// it is a step, then refused.
	was := maxAnswerSeries
	t.Cleanup(func() { maxAnswerSeries = was })
	maxAnswerSeries = 3
	var b strings.Builder
	b.WriteString(`{"status":"success","data":{"resultType":"matrix","result":[`)
	for i := range 4 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"metric":{"i":"%d"},"values":[[%d,"1"]]}`, i, at(0))
	}
	b.WriteString(`]}}`)
	if _, err := ask(b.String()); err == nil || !strings.Contains(err.Error(), "more than 3 series") || !Refused(err) || asked.Load() < 3 {
		t.Errorf("4 series, 3 allowed: %v, asked %d times", err, asked.Load())
	}
	maxAnswerSeries = was
	wasSamples := maxAnswerSamples
	t.Cleanup(func() { maxAnswerSamples = wasSamples })
	maxAnswerSamples = 5
	a, err := readAnswer(strings.NewReader(series(strings.Join(many[:6], ","))), t0, t0.Add(time.Hour), time.Second)
	if err != nil || !strings.Contains(a.big, "more than 5 samples") {
		t.Errorf("6 samples, 5 allowed: %v %q", err, a.big)
	}
	maxAnswerSamples = wasSamples

	// An answer over what is read of it: too big, not a page.
	wasBody := maxBody
	t.Cleanup(func() { maxBody = wasBody })
	maxBody = 64
	if _, err := ask(series(strings.Join(many[:4], ","))); err == nil || !strings.Contains(err.Error(), "the answer is over") || !Refused(err) {
		t.Errorf("an answer over the cap: %v", err)
	}
}

// What isn't a range answer is said as before: not the API's, or
// unreadable, without the token.
func TestAnswerNotTheAPIs(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for body, want := range map[string]string{
		`<html>sign in</html>`: "not a Prometheus API response: <html>sign in</html>",
		`{"status":"success","data":{"resultType":"matrix","result":[]}} x`:                                         "not a Prometheus API response",
		`{"status":"success","data":{"resultType":"matrix","result":{}}}`:                                           "not a Prometheus API response",
		`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"a":1}}]}}`:                         "not a Prometheus API response",
		`{"status":"success","data":{"resultType":"matrix","result":[{"values":[1]}]}}`:                             "not a Prometheus API response",
		`{"status":"success","data":{"resultType":"matrix","result":[{"values":[["1",1]]}]}}`:                       `unreadable sample ["\"1\"", "1"]`,
		`{"status":"success","data":{"resultType":"matrix","result":[{"values":[[1,"x"]]}]}}`:                       `unreadable sample ["1", "\"x\""]`,
		`{"status":"success","data":{"resultType":"matrix","result":[{"values":[[1,"s3cret-token-0123456789"]]}]}}`: "[redacted]",
		`{"status":"success","data":{"result":[],"resultType":"vector"}}`:                                           `want a matrix, got "vector"`,
		`{"status":"error","error":"bad_data: parse error"}`:                                                        "bad_data: parse error",
		`{"status":"success","warnings":["result truncated"],"data":{"resultType":"matrix","result":[]}}`:           "left series out",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
		c := New(srv.URL, "t", WithToken(func(context.Context) (string, error) { return "s3cret-token-0123456789", nil }))
		_, err := c.QueryRange(context.Background(), "up", t0, t0.Add(time.Hour), time.Minute)
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%s: %v, want %q", body, err, want)
		}
	}
}

// oldAnswer is a range answer as json.Unmarshal read it whole before
// 0.34.1: what readAnswer must read alike, but for what it keeps.
func oldAnswer(body []byte) (status, resultType string, out []Series, err error) {
	var r struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string    `json:"metric"`
				Values [][2]json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", "", nil, err
	}
	for _, res := range r.Data.Result {
		s := Series{Labels: res.Metric, Samples: []Sample{}}
		for _, v := range res.Values {
			ts, err1 := ParseTime(string(v[0]))
			f, err2 := strconv.ParseFloat(strings.Trim(string(v[1]), `"`), 64)
			if err1 != nil || err2 != nil {
				return r.Status, r.Data.ResultType, nil, errors.New("unreadable")
			}
			s.Samples = append(s.Samples, Sample{T: ts, V: f})
		}
		out = append(out, s)
	}
	return r.Status, r.Data.ResultType, out, nil
}

// FuzzRangeAnswer: whatever an API answers, readAnswer doesn't panic; and
// an answer json.Unmarshal read is read alike, the samples out of the
// range dropped.
func FuzzRangeAnswer(f *testing.F) {
	f.Add([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"job":"api"},"values":[[1700000000,"1"],[1700000060.5,"NaN"],[1800000000,"2"]]}]}}`))
	f.Add([]byte(`{"status":"success","data":{"result":[{"values":[[1700000000,1,2],[1700000001,"+Inf",{"x":[1]}]],"metric":{}}],"resultType":"matrix","stats":{"a":[1,{"b":"]"}]}}}`))
	f.Add([]byte(`{"status":"error","errorType":"bad_data","error":"x"}`))
	f.Add([]byte(`{"data":{"resultType":"matrix","result":[{"values":null},{"values":[["1","1"]]}]},"status":"success"}`))
	start, end := time.Unix(1600000000, 0).UTC(), time.Unix(1750000000, 0).UTC()
	f.Fuzz(func(t *testing.T, body []byte) {
		a, err := readAnswer(bytes.NewReader(body), start, end, time.Second)
		status, typ, old, oldErr := oldAnswer(body)
		if oldErr != nil || typ != "matrix" {
			return
		}
		// Read again from the answer as json.Unmarshal took it, a key given
		// twice once.
		canon, _ := json.Marshal(map[string]any{"status": status, "data": map[string]any{"resultType": typ, "result": encodeSeries(old)}})
		if a, err = readAnswer(bytes.NewReader(canon), start, end, time.Second); err != nil || a.bad != nil || a.big != "" {
			t.Fatalf("%s: %v %v %q", canon, err, a.bad, a.big)
		}
		for i := range old {
			kept := old[i].Samples[:0]
			for _, x := range old[i].Samples {
				if !x.T.Before(start) && !x.T.After(end) {
					kept = append(kept, x)
				}
			}
			old[i].Samples = kept
			if old[i].Labels == nil {
				old[i].Labels = map[string]string{}
			}
		}
		for i := range a.series {
			if a.series[i].Labels == nil {
				a.series[i].Labels = map[string]string{}
			}
		}
		if len(old) == 0 && len(a.series) == 0 {
			return
		}
		if a.status != status || !reflect.DeepEqual(bits(a.series), bits(old)) {
			t.Fatalf("%s:\n%+v\nwant %+v", canon, a.series, old)
		}
	})
}

// bits is series with their values' bits, as NaN isn't equal to itself.
func bits(ss []Series) []any {
	var out []any
	for _, s := range ss {
		var vs []any
		for _, x := range s.Samples {
			vs = append(vs, [2]any{x.T, math.Float64bits(x.V)})
		}
		out = append(out, []any{s.Labels, vs})
	}
	return out
}

// encodeSeries is series as an API writes them.
func encodeSeries(ss []Series) []any {
	out := []any{}
	for _, s := range ss {
		var vs [][2]any
		for _, x := range s.Samples {
			ns, sign := x.T.UnixNano(), ""
			if ns < 0 {
				ns, sign = -ns, "-"
			}
			vs = append(vs, [2]any{json.Number(fmt.Sprintf("%s%d.%09d", sign, ns/1e9, ns%1e9)), strconv.FormatFloat(x.V, 'g', -1, 64)})
		}
		out = append(out, map[string]any{"metric": s.Labels, "values": vs})
	}
	return out
}

// An unreadable sample is quoted with what the request carried taken out
// before it is cut, a token split in two by a character that doesn't show
// too; what doesn't show is quoted as \u…, so it shows.
func TestUnreadableSampleRedacted(t *testing.T) {
	token := "ya29." + strings.Repeat("A1b2C3d4E5", 20)
	req, _ := http.NewRequest("GET", "http://prom/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	for raw, want := range map[string]string{
		`"` + token + `"`: "[redacted]",
		`"` + token[:30] + "​" + token[30:] + `"`: "[redacted]",
		"\"\u009b31m‮RED\"":                       `\u009b31m\u202eRED`,
	} {
		got := clip(quotedText(raw, req), 64)
		if strings.Contains(got, token[:20]) || !strings.Contains(fmt.Sprintf("%q", got), want) {
			t.Errorf("%.40q: %q", raw, got)
		}
	}
}
