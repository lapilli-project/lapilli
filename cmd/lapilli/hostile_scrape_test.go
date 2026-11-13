package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
)

// Every lapilli verdict run with --prometheus and no --scrape-interval
// (and lapilli serve, once a day) asks /api/v1/status/config as a read it
// does without when it fails, within 30 seconds; the YAML in it is parsed
// whole, into a node per value, some 90 times its size. A hostile
// Prometheus answers a few KB on the wire, gzipped, for MiBs of YAML: past
// 2 MiB, it isn't parsed.
func TestStatusConfigYAMLBlowUp(t *testing.T) {
	const n = 2 << 20 // flow sequence items: 4 MiB of YAML
	var y strings.Builder
	y.WriteString("global:\n  scrape_interval: 1m\nscrape_configs: []\npadding: [")
	for i := range n {
		if i > 0 {
			y.WriteByte(',')
		}
		y.WriteByte('0')
	}
	y.WriteString("]\n")
	body, _ := json.Marshal(map[string]any{"status": "success", "data": map[string]string{"yaml": y.String()}})
	var gz bytes.Buffer
	z, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	z.Write(body)
	z.Close()
	var wire atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/status/config" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(gz.Len()))
		k, _ := w.Write(gz.Bytes())
		wire.Add(int64(k))
	}))
	defer srv.Close()
	h := prom.New(srv.URL, "t")
	f := &verdictFlags{}
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	before := m.HeapAlloc
	var peak uint64
	stop, done := make(chan struct{}), make(chan struct{})
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
	start := time.Now()
	var said bytes.Buffer
	f.askScrapes(context.Background(), h, &said, nil)
	took := time.Since(start)
	close(stop)
	<-done
	grew := peak - before
	t.Logf("wire %d B, JSON %d B, YAML %d B: heap +%d MiB at peak (%.0fx the YAML), %s; scrapes read: %v",
		wire.Load(), len(body), y.Len(), grew>>20, float64(grew)/float64(y.Len()), took.Round(time.Millisecond), f.scrapes != nil)
	if !strings.Contains(said.String(), "configuration is larger than is read of it") || !strings.Contains(said.String(), "--scrape-interval") {
		t.Errorf("a configuration too large to read, said: %q", said.String())
	}
	if grew > 64<<20 {
		t.Errorf("a %d KiB answer took the heap up %d MiB (%.0fx its YAML): at the 256 MiB cap, ~%d GiB",
			wire.Load()>>10, grew>>20, float64(grew)/float64(y.Len()), int(float64(grew)/float64(y.Len())*256/1024))
	}
}
