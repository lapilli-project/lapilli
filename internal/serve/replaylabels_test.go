package serve

import (
	"compress/gzip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/verdict"
)

// wireCounter counts the bytes a handler writes on the wire.
type wireCounter struct {
	http.ResponseWriter
	n *atomic.Int64
}

func (c wireCounter) Write(b []byte) (int, error) {
	n, err := c.ResponseWriter.Write(b)
	c.n.Add(int64(n))
	return n, err
}

func dirSize(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			n += fi.Size()
		}
		return nil
	})
	return n
}

// A Prometheus-compatible API that is hostile (or a proxy before one)
// answers each range query with one series whose label value is 512 KB, a
// little different each time, gzipped on the wire. The ledger caps the
// labels of every other source of firings at 4 KB (recorded firings,
// notifications, incidents: maxLabelBytes), and those of the firings it
// replays itself the same: a value over 256 bytes is cut, with its hash,
// so that a few KB on the wire don't become many MB on disk and in
// memory, more each run, and alerts apart stay apart.
func TestReplayedLabelsBounded(t *testing.T) {
	const big = 512 << 10
	var wire, asked atomic.Int64
	pad := strings.Repeat("a", big)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			http.NotFound(w, r)
			return
		}
		r.ParseForm()
		start, _ := strconv.ParseInt(r.Form.Get("start"), 10, 64)
		end, _ := strconv.ParseInt(r.Form.Get("end"), 10, 64)
		step, _ := strconv.ParseFloat(r.Form.Get("step"), 64)
		n := asked.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(wireCounter{w, &wire})
		fmt.Fprintf(zw, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"service":"checkout","x":"%s%d"},"values":[`, pad, n)
		for ts, i := start, 0; ts <= end; ts, i = ts+int64(step), i+1 {
			if i > 0 {
				io.WriteString(zw, ",")
			}
			fmt.Fprintf(zw, `[%d,"1"]`, ts)
		}
		io.WriteString(zw, `]}]}}`)
		zw.Close()
	}))
	defer srv.Close()

	dir := t.TempDir()
	rulesDir := filepath.Join(dir, "rules")
	os.MkdirAll(rulesDir, 0o755)
	os.WriteFile(filepath.Join(rulesDir, "shop.yaml"), []byte(ruleFile), 0o644)
	cfg := Config{
		RulesPath: rulesDir,
		Options:   verdict.Options{Lead: 15 * time.Minute, RefireWithin: time.Hour, UnknownLen: time.Hour, Match: []string{"service"}},
		Step:      time.Minute, Loc: time.UTC, Ledger: filepath.Join(dir, "ledger"),
		Every: time.Hour, Settle: time.Hour, Backfill: 3 * 24 * time.Hour, Watch: time.Minute,
		Digest: Weekly{time.Monday, 9, 0}, Timeout: time.Minute, Parallel: 4, Listen: "127.0.0.1:0", Version: "test",
		AllowedHosts: []string{"example.com"},
	}
	h := prom.New(srv.URL, "lapilli/test")
	s, err := New(context.Background(), cfg, h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	t.Cleanup(func() { s.Close() })

	ctx := context.Background()
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	before := dirSize(t, cfg.Ledger)
	runtime.GC()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)
	s.jobs(ctx, true)
	runtime.GC()
	runtime.GC()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	after := dirSize(t, cfg.Ledger)

	var kept int
	db, err := sql.Open("sqlite", "file:"+filepath.Join(cfg.Ledger, "ledger.db")+"?_pragma=query_only(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRowContext(ctx, "SELECT coalesce(sum(length(labels)), 0) FROM series").Scan(&kept); err != nil {
		t.Fatalf("series labels: %v", err)
	}
	t.Logf("requests %d, bytes on the wire %d (%.1f KB); ledger %d → %d bytes (+%.1f MB); series labels kept %.1f MB; heap in use after GC %+.1f MB",
		asked.Load(), wire.Load(), float64(wire.Load())/1024, before, after, float64(after-before)/(1<<20), float64(kept)/(1<<20),
		float64(int64(m1.HeapInuse)-int64(m0.HeapInuse))/(1<<20))
	if kept > 4<<10*int(asked.Load()) {
		t.Errorf("one replay of one rule kept %.1f MB of labels a hostile API chose, from %.1f KB on the wire: labels of replayed firings aren't bounded as recorded firings' are (4 KB)",
			float64(kept)/(1<<20), float64(wire.Load())/1024)
	}
	var series int
	var label string
	db.QueryRowContext(ctx, "SELECT count(*), max(json_extract(labels, '$.x')) FROM series").Scan(&series, &label)
	if series < 2 || len(label) > 256 || !strings.HasPrefix(label, "aaaa") || !strings.Contains(label, "…") {
		t.Errorf("the alerts, %d, told apart by a label cut to %d bytes: %.80q", series, len(label), label)
	}
	// Each hourly run after asks for the hour since, and keeps its answer's
	// new label set, cut.
	grown := dirSize(t, cfg.Ledger)
	for hour := 1; hour <= 4; hour++ {
		at := now.Add(time.Duration(hour) * time.Hour)
		s.now = func() time.Time { return at }
		s.jobs(ctx, true)
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		db.QueryRowContext(ctx, "SELECT coalesce(sum(length(labels)), 0) FROM series").Scan(&kept)
		t.Logf("after hourly run %d: requests %d, wire %.1f KB, series labels kept %.1f MB, ledger %.1f MB, heap in use after GC %+.1f MB",
			hour, asked.Load(), float64(wire.Load())/1024, float64(kept)/(1<<20), float64(dirSize(t, cfg.Ledger))/(1<<20),
			float64(int64(m.HeapInuse)-int64(m0.HeapInuse))/(1<<20))
	}
	if more := dirSize(t, cfg.Ledger) - grown; more > 4<<20 {
		t.Errorf("four hourly runs grew the ledger %d KiB", more>>10)
	}
}

// A rule whose replay in a run finds more alerts, told apart by their
// labels, than a run keeps (maxRuleSeries; 1 here, and the stand-in finds
// two over the backfill) is left as it was by that run, as one whose query
// failed, and says why.
func TestRuleSeriesCapped(t *testing.T) {
	recorded := map[int]int{}
	for _, most := range []int{1, 2} {
		was := maxRuleSeries
		maxRuleSeries = most
		s, _ := server(t, nil)
		ctx := context.Background()
		raw := ""
		if err := s.look(ctx, &raw); err != nil {
			t.Fatal(err)
		}
		s.jobs(ctx, true)
		maxRuleSeries = was
		fs, err := s.l.ReplayedFirings(ctx, now.Add(-7*24*time.Hour), now)
		if err != nil {
			t.Fatal(err)
		}
		s.ready.Store(true)
		_, body, _ := get(t, s.Handler(), "/api/v1/rules")
		recorded[most] = len(fs)
		if said := strings.Contains(body, fmt.Sprintf("more than %d alerts at once", most)); said != (most == 1) {
			t.Errorf("%d kept: said why %v", most, said)
		}
	}
	if recorded[1] >= recorded[2] {
		t.Errorf("firings recorded: %d with one alert kept a run, %d with two", recorded[1], recorded[2])
	}
}
