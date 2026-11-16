package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
)

// floodAnnotations answers every page full, each annotation a change of
// state one millisecond older than the last, with labels tags labels of
// their own: of one alert, or each of an alert of its own. It counts the
// annotations it sent.
func floodAnnotations(t *testing.T, labels int, distinct bool) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var sent atomic.Int64
	var tags bytes.Buffer
	for i := range labels {
		fmt.Fprintf(&tags, `,"l%d:v"`, i)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		to, _ := strconv.ParseInt(q.Get("to"), 10, 64)
		from, _ := strconv.ParseInt(q.Get("from"), 10, 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		var b bytes.Buffer
		b.WriteByte('[')
		for i := range limit {
			at := to - int64(i) - 1
			if at < from {
				break
			}
			if i > 0 {
				b.WriteByte(',')
			}
			n := sent.Add(1)
			states := `"prevState":"Normal","newState":"Alerting"`
			if n%2 == 0 {
				states = `"prevState":"Alerting","newState":"Normal"`
			}
			alert := ""
			if distinct {
				alert = fmt.Sprintf(`,"pod:p%d"`, n)
			}
			fmt.Fprintf(&b, `{"id":%d,"time":%d,"alertName":"Flood",%s,"tags":["alertname:Flood"%s%s]}`, n, at, states, tags.String(), alert)
		}
		b.WriteByte(']')
		w.Write(b.Bytes())
	}))
	t.Cleanup(srv.Close)
	return srv, &sent
}

// heap is the heap in use, once collected.
func heap() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// grown is how much the heap grew since before, once collected.
func grown(before uint64) uint64 {
	if after := heap(); after > before {
		return after - before
	}
	return 0
}

// What a read keeps of each change is its time and states, and of each
// alert its labels, once: 50,000 changes of one alert with 128 labels each
// keep a few MiB, where a copy of the labels each would keep hundreds; the
// firings of the alert share them too.
func TestGrafanaHistoryMemory(t *testing.T) {
	defer func(n int) { MaxChanges = n }(MaxChanges)
	MaxChanges = 50000
	srv, sent := floodAnnotations(t, 127, false)
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	before := heap()
	ts, read, err := GrafanaHistory{URL: srv.URL}.Read(context.Background(), HistoryAnnotations, from, from.Add(40*time.Second))
	if err != nil || len(ts) != 40000 || read.Passed != 0 {
		t.Fatalf("%d changes of %d sent, %+v, %v", len(ts), sent.Load(), read, err)
	}
	fs, _ := firing.FromTransitions(ts, from)
	kept := grown(before)
	runtime.KeepAlive(ts)
	runtime.KeepAlive(fs)
	t.Logf("40,000 changes, %d firings: %d KiB kept", len(fs), kept>>10)
	if kept > 24<<20 {
		t.Errorf("40,000 changes and %d firings of one alert with 128 labels keep %d MiB, want 24 at most", len(fs), kept>>20)
	}
}

// Alerts of their own, each with 128 labels, are kept until their labels
// take what a read may keep of them, and the read fails as too many; it
// doesn't read on until MaxChanges.
func TestGrafanaHistoryMemoryAlerts(t *testing.T) {
	srv, sent := floodAnnotations(t, 126, true)
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	before := heap()
	_, _, err := GrafanaHistory{URL: srv.URL}.Read(context.Background(), HistoryAnnotations, from, from.Add(time.Hour))
	if !errors.Is(err, ErrTooManyChanges) || !strings.Contains(err.Error(), "alerts") {
		t.Fatalf("%v", err)
	}
	// Some 9 KB an alert as counted: 64 MiB is some 7,300 of them, read a
	// page of 1,000 at a time.
	if n := sent.Load(); n > 9000 {
		t.Errorf("read %d alerts' changes before giving up", n)
	}
	if kept := grown(before); kept > 8<<20 {
		t.Errorf("%d MiB kept after a read that failed", kept>>20)
	}
}

// Changes told apart are kept as ids, or hashes, and counted: lines that
// can't be read, each its own, end the read past historySeen of them.
func TestGrafanaHistoryMemorySeen(t *testing.T) {
	defer func(n int) { historySeen = n }(historySeen)
	historySeen = 3000
	var sent atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		to, _ := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		var times, lines bytes.Buffer
		for i := range limit {
			if i > 0 {
				times.WriteByte(',')
				lines.WriteByte(',')
			}
			fmt.Fprintf(&times, "%d", (to-int64(i/10)-1)*1000)
			fmt.Fprintf(&lines, `{"previous":%d}`, sent.Add(1))
		}
		fmt.Fprintf(w, `{"schema":{"fields":[{"name":"time"},{"name":"line"}]},"data":{"values":[[%s],[%s]]}}`, times.String(), lines.String())
	}))
	defer srv.Close()
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	_, _, err := GrafanaHistory{URL: srv.URL}.Read(context.Background(), HistoryLoki, from, from.Add(24*time.Hour))
	if !errors.Is(err, ErrTooManyChanges) || !strings.Contains(err.Error(), "told apart") {
		t.Fatalf("%v", err)
	}
	if n := sent.Load(); n > 4000 {
		t.Errorf("read %d lines before giving up", n)
	}
}
