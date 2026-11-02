package prom

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestQueryRangeSplitsAndJoins(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		r.ParseForm()
		if r.Header.Get("Authorization") != "Bearer tok" || r.Form.Get("query") != "up == 0" {
			t.Errorf("auth %q query %q", r.Header.Get("Authorization"), r.Form.Get("query"))
		}
		start, _ := strconv.ParseInt(r.Form.Get("start"), 10, 64)
		end, _ := strconv.ParseInt(r.Form.Get("end"), 10, 64)
		step, _ := strconv.ParseFloat(r.Form.Get("step"), 64)
		if n := float64(end-start)/step + 1; n > 1441 {
			t.Errorf("asked for %v points in one request", n)
		}
		// One sample at the start of each chunk, for one series.
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"job":"api"},"values":[[%d,"1"]]}]}}`, start)
	}))
	defer srv.Close()
	c := New(srv.URL, "test", WithToken(func(context.Context) (string, error) { return "tok", nil }))
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	got, err := c.QueryRange(context.Background(), "up == 0", t0, t0.Add(30*24*time.Hour), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 30 || len(got) != 1 || len(got[0].Samples) != 30 || got[0].Labels["job"] != "api" {
		t.Errorf("calls %d, series %+v: want 30 days at 1m in a request a day, joined into one series", calls, got)
	}
}

func TestQueryRangeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"status":"error","errorType":"bad_data","error":"parse error at char 3"}`)
	}))
	defer srv.Close()
	_, err := New(srv.URL, "test").QueryRange(context.Background(), "up ==", time.Now().Add(-time.Hour), time.Now(), time.Minute)
	if err == nil || !strings.Contains(err.Error(), "parse error at char 3") {
		t.Errorf("err = %v", err)
	}
}

func TestRetriesAndSplits(t *testing.T) {
	old := retryWait
	retryWait = time.Millisecond
	defer func() { retryWait = old }()
	calls, busy := 0, 2
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		r.ParseForm()
		start, _ := strconv.ParseInt(r.Form.Get("start"), 10, 64)
		end, _ := strconv.ParseInt(r.Form.Get("end"), 10, 64)
		switch {
		case busy > 0:
			busy--
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, "<html>busy</html>")
		case end-start > 3600:
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"status":"error","error":"expanding series: response size 200186118 bytes exceeds maximum allowed size 200000000 bytes"}`)
		default:
			fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"job":"api"},"values":[[%d,"1"]]}]}}`, start)
		}
	}))
	defer srv.Close()
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	got, err := New(srv.URL, "test").QueryRange(context.Background(), "x", t0, t0.Add(4*time.Hour), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// Two busy answers retried, then 4h split down to pieces of an hour or less.
	if len(got) != 1 || len(got[0].Samples) < 4 {
		t.Errorf("series %+v after %d calls", got, calls)
	}
	for i := 1; i < len(got[0].Samples); i++ {
		if !got[0].Samples[i].T.After(got[0].Samples[i-1].T) {
			t.Errorf("samples out of order: %v", got[0].Samples)
		}
	}
}
