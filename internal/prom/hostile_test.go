package prom

// What a hostile or compromised Prometheus-compatible API can make lapilli
// hold in memory: no more than what was asked (0.34.1).

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// gzipped is body, gzip-compressed.
func gzipped(t *testing.T, body []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	z, _ := gzip.NewWriterLevel(&b, gzip.BestCompression)
	if _, err := z.Write(body); err != nil {
		t.Fatal(err)
	}
	z.Close()
	return b.Bytes()
}

// hostile serves body, gzip-encoded, to every query, counting the bytes
// it put on the wire.
func hostile(t *testing.T, body []byte) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	gz := gzipped(t, body)
	var wire atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(gz)))
		n, _ := w.Write(gz)
		wire.Add(int64(n))
	}))
	t.Cleanup(srv.Close)
	return srv, &wire
}

// heapAfterGC is the live heap.
func heapAfterGC() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// grown is how much the live heap grew since before; none when it shrank.
func grown(before uint64) uint64 {
	return max(heapAfterGC(), before) - before
}

// peakDuring samples the heap every millisecond while f runs: for the logs
// alone. A sampled peak counts garbage the collector hasn't reached yet,
// which a busy runner's load moves: on CI one run in a few read four to
// eight times what the next did, the same code both times. What a read
// holds is weighed where it holds it (answerHeld), after a collection.
func peakDuring(f func()) (peak uint64) {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			peak = max(peak, m.HeapAlloc)
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	f()
	close(stop)
	<-done
	return peak
}

// lapilli asks for an hour at one minute, 61 points, and a hostile API
// answers one series with millions of samples, gzip-encoded, under 100 KB
// on the wire: those out of the range aren't kept, nor more than the steps
// asked for, and the answer isn't held whole: what the read holds once the
// series is read, or once its values are found longer than is read of
// them (answerHeld), is weighed, the live heap after a collection, not a
// sampled peak, which is logged.
func TestAnswerSamplesPastWhatWasAsked(t *testing.T) {
	const n = 2 << 20 // samples: 16 MiB of `[1,"1"],`
	var b bytes.Buffer
	b.WriteString(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"job":"api"},"values":[`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`[1,"1"]`)
	}
	b.WriteString(`]}]}}`)
	body := b.Bytes()
	srv, wire := hostile(t, body)
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	c := New(srv.URL, "t")
	before := heapAfterGC()
	var held uint64
	read := 0
	answerHeld = func(int, bool) { read, held = read+1, max(held, grown(before)) }
	defer func() { answerHeld = nil }()
	var got []Series
	var err error
	peak := peakDuring(func() { got, err = c.QueryRange(context.Background(), "up", t0, t0.Add(time.Hour), time.Minute) })
	kept := grown(before)
	samples := 0
	for _, s := range got {
		samples += len(s.Samples)
	}
	runtime.KeepAlive(got)
	t.Logf("asked 61 points; wire %d B, decoded %d B, samples kept %d, held +%d MiB once read, heap peak sampled +%d MiB, kept +%d MiB, err %v",
		wire.Load(), len(body), samples, held>>20, (max(peak, before)-before)>>20, kept>>20, err)
	if samples > 61 {
		t.Errorf("an answer of %d samples to a request for 61 was kept (%d MiB live, %d MiB held, from %d bytes on the wire): want those past the range or the step refused",
			samples, kept>>20, held>>20, wire.Load())
	}
	if read == 0 || held > uint64(len(body)) {
		t.Errorf("the heap held %d MiB reading a %d MiB answer, weighed %d times", held>>20, len(body)>>20, read)
	}
}

// The same with many series of one sample: each ~30-byte series becomes a
// map, a Series and a key in QueryRange's index. Past the series an
// answer may hold (50,000; 5,000 here), the answer is too big, and isn't
// kept. What the read holds is weighed where it holds the most, as it
// finds the answer too big, the series it read kept (answerHeld): the live
// heap after a collection, which no runner's load moves, as a sampled peak
// of what the collector hasn't reached yet did. The sampled peak is logged.
func TestAnswerManySeries(t *testing.T) {
	was := maxAnswerSeries
	t.Cleanup(func() { maxAnswerSeries = was })
	maxAnswerSeries = 5000
	const n = 64 << 10 // series
	var b bytes.Buffer
	b.WriteString(`{"status":"success","data":{"resultType":"matrix","result":[`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"metric":{"i":"`)
		b.WriteString(strconv.Itoa(i))
		b.WriteString(`"},"values":[[1,"1"]]}`)
	}
	b.WriteString(`]}}`)
	body := b.Bytes()
	srv, wire := hostile(t, body)
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	c := New(srv.URL, "t")
	before := heapAfterGC()
	var held uint64
	full := 0
	answerHeld = func(_ int, big bool) {
		if big {
			full, held = full+1, max(held, grown(before))
		}
	}
	defer func() { answerHeld = nil }()
	var got []Series
	var err error
	sampled := peakDuring(func() {
		wire.Store(0)
		got, err = c.QueryRange(context.Background(), "up", t0, t0.Add(time.Hour), time.Minute)
	})
	kept := grown(before)
	runtime.KeepAlive(got)
	ratio := float64(held) / float64(len(body))
	t.Logf("wire %d B, decoded %d B (%d series), held +%d MiB (%.1fx the decoded body) when found too big %d times, heap peak sampled +%d MiB, kept +%d MiB, err %v",
		wire.Load(), len(body), len(got), held>>20, ratio, full, (max(sampled, before)-before)>>20, kept>>20, err)
	if err == nil || !Refused(err) || full == 0 {
		t.Errorf("an answer of %d series, %d allowed: %v, found too big %d times", n, maxAnswerSeries, err, full)
	}
	if kept > uint64(len(body)) {
		t.Errorf("a refused answer of %d MiB left the heap %d MiB up", len(body)>>20, kept>>20)
	}
	if ratio > 4 {
		t.Errorf("a %d MiB answer (%d KiB on the wire) held the heap up %d MiB, %.1f times its size: at the 256 MiB cap that is ~%d GiB a request, and --parallel requests at once",
			len(body)>>20, wire.Load()>>10, held>>20, ratio, int(ratio*256/1024+0.5))
	}
}
