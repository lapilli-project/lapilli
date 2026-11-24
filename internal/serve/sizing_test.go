package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/ledger/ledgertest"
	"lapilli.dev/internal/verdict"
)

// What a ledger takes on disk and what serve takes in memory, by how many
// rules, how many firings a week and how many weeks are kept: the numbers
// docs/serve.md ("Sizing") gives. Opt in with LAPILLI_SIZING=1; it logs
// the table, and writes it to LAPILLI_SIZING_OUT when that is set.
// LAPILLI_SIZING_BIG=1 adds a million firings a week, up to four weeks.
// Scratch files go where TMPDIR says; each ledger is deleted when its rows
// are measured, so the most on disk is the largest ledger (and its copy
// in WAL before the checkpoint).
//
// With LAPILLI_TEST_POSTGRES set (internal/ledger/ledgertest) the ledgers
// are Postgres's instead, each in a schema of its own, dropped when its
// rows are measured; "on disk" is then pg_total_relation_size (tables,
// indexes and TOAST) summed over the schema's tables. Each hourly run
// rewrites the rows of the days and the week it falls in, so a table holds
// rows no longer live until it is vacuumed: autovacuum, given the hour
// between two runs, would have done it, but the test writes a week in
// seconds, so it runs VACUUM on the schema's tables itself after each run
// (not VACUUM FULL: the space freed is reused, not given back, as
// autovacuum's is). serve runs no VACUUM of its own. The server's own
// files (its WAL, other schemas, the catalogs) are not in it. The table
// says which in its Ledger column.
//
// Nothing here is held to a bound: the test measures. A ledger is written
// as hourly runs write it (ledger.Record, one run an hour), weeks of it
// one after the other, and measured after weeks 1, 4 and 12 of the one
// ledger. serve is measured in a process of its own for each row (this
// test binary, run again), so that its peak is its own and not the
// writing's: it opens the ledger and does what a visit does.

const sizingVisitEnv = "LAPILLI_SIZING_VISIT"

// sizingPGFileEnv is the file holding the Postgres ledger's connection
// string that the child, which measures serve's memory, opens.
const sizingPGFileEnv = "LAPILLI_SIZING_PGFILE"

var sizingStart = time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC) // a Monday

type sizingRow struct {
	Ledger                         string // "sqlite" or "postgres"
	Rules, PerWeek, Weeks, Firings int
	Disk, WAL                      int64 // after the checkpoint; the WAL before it
	Base, Rest, Peak, After, RSS   uint64
}

func TestSizing(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	if os.Getenv("LAPILLI_SIZING") != "1" {
		t.Skip("set LAPILLI_SIZING=1")
	}
	if os.Getenv(sizingVisitEnv) != "" {
		t.Skip("the child's")
	}
	ctx := context.Background()
	perWeeks := []int{10_000, 100_000}
	if os.Getenv("LAPILLI_SIZING_BIG") == "1" {
		perWeeks = append(perWeeks, 1_000_000)
	}
	var rows []sizingRow
	kind := "sqlite"
	if ledgertest.On() {
		kind = "postgres"
	}
	for _, rules := range []int{100, 1000} {
		for _, perWeek := range perWeeks {
			maxWeeks := 12
			if perWeek >= 1_000_000 {
				maxWeeks = 4
			}
			t.Run(fmt.Sprintf("rules=%d/perweek=%d", rules, perWeek), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "ledger.db")
				pg := ledgertest.URL(t) // a schema of this cell's, dropped after it; "" for SQLite
				vacuum := func() {}
				if pg != "" {
					c, err := pgx.Connect(ctx, pg)
					if err != nil {
						t.Fatal(err)
					}
					defer c.Close(ctx)
					vacuum = func() { sizingVacuum(t, ctx, c) }
				}
				r := rand.New(rand.NewPCG(uint64(rules), uint64(perWeek)))
				services := []string{"checkout", "search", "cart", "auth", "billing", "media"}
				var firings int
				for week := 0; week < maxWeeks; {
					var l *ledger.Ledger
					var err error
					if pg != "" {
						l, err = ledger.OpenPostgres(ctx, pg, time.UTC)
					} else {
						l, err = ledger.Open(ctx, path, time.UTC)
					}
					if err != nil {
						t.Fatal(err)
					}
					// The weeks up to the next one measured: 1, 4, 12.
					upTo := map[int]int{0: 1, 1: 4, 4: 12, 12: 12}[week]
					for ; week < min(upTo, maxWeeks); week++ {
						began := time.Now()
						firings += sizingWeek(t, ctx, l, r, week, rules, perWeek, services, vacuum)
						t.Logf("rules %d, %d a week: week %d written, %d firings, in %s", rules, perWeek, week+1, firings, time.Since(began).Round(time.Second))
					}
					row := sizingRow{Ledger: kind, Rules: rules, PerWeek: perWeek, Weeks: week, Firings: firings}
					if pg != "" {
						if err := l.Close(); err != nil {
							t.Fatal(err)
						}
						row.Disk = sizingSchemaBytes(t, ctx, pg)
					} else {
						row.WAL = sizingBytes(path + "-wal")
						if err := l.Close(); err != nil { // the last connection's close checkpoints the WAL
							t.Fatal(err)
						}
						row.Disk = sizingBytes(path) + sizingBytes(path+"-wal") + sizingBytes(path+"-shm")
					}
					sizingVisit(t, dir, pg, sizingStart.Add(time.Duration(week)*7*24*time.Hour), &row)
					t.Logf("rules %d, %d a week, %d weeks: %.1f MB, %d bytes a firing, heap +%.1f MB at rest, +%.1f MB at most", rules, perWeek, week,
						float64(row.Disk)/1e6, row.Disk/int64(firings), float64(row.Rest-row.Base)/1e6, float64(row.Peak-row.Base)/1e6)
					rows = append(rows, row)
				}
				os.RemoveAll(dir)
			})
		}
	}
	var b strings.Builder
	if len(rows) == 0 {
		t.Fatal("nothing measured")
	}
	b.WriteString("| Ledger | Rules | Firings a week | Weeks kept | Firings kept | Ledger on disk | Bytes a firing | Heap at rest | Heap at most while reading | Heap kept after | Process memory at most |\n")
	b.WriteString("|:---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	mb := func(n uint64) string { return fmt.Sprintf("%.0f MB", float64(n)/1e6) }
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %s | %s | %d | %s | %s | %s | %s |\n", r.Ledger, sizingInt(r.Rules), sizingInt(r.PerWeek), r.Weeks, sizingInt(r.Firings),
			mb(uint64(r.Disk)), r.Disk/int64(r.Firings), mb(r.Rest-r.Base), mb(r.Peak-r.Base), mb(r.After-r.Base), mb(r.RSS))
	}
	fmt.Fprintf(&b, "\nHeap is Go's HeapInuse above the %s the test binary held before serve opened the ledger; process memory is the peak resident set, the binary among it.\n", mb(rows[0].Base))
	t.Log("\n" + b.String())
	if out := os.Getenv("LAPILLI_SIZING_OUT"); out != "" {
		if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func sizingInt(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// sizingSchemaBytes is what the Postgres ledger in the schema a connection
// string's search_path names takes: every table with its indexes and TOAST.
func sizingSchemaBytes(t *testing.T, ctx context.Context, conn string) int64 {
	t.Helper()
	c, err := pgx.Connect(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	var n int64
	if err := c.QueryRow(ctx, `SELECT coalesce(sum(pg_total_relation_size(c.oid)), 0)::bigint FROM pg_class c
		WHERE c.relnamespace = current_schema()::regnamespace AND c.relkind IN ('r', 'p')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// sizingVacuum runs VACUUM on the tables of the schema c's search_path
// names, as autovacuum would in the hour between two runs.
func sizingVacuum(t *testing.T, ctx context.Context, c *pgx.Conn) {
	t.Helper()
	rows, err := c.Query(ctx, `SELECT quote_ident(current_schema()) || '.' || quote_ident(relname) FROM pg_class
		WHERE relnamespace = current_schema()::regnamespace AND relkind = 'r' ORDER BY relname`)
	if err != nil {
		t.Fatal(err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		return
	}
	if _, err := c.Exec(ctx, "VACUUM "+strings.Join(names, ", ")); err != nil {
		t.Fatal(err)
	}
}

func sizingBytes(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// sizingWeek writes a week as the hourly runs do, an hour a run, and gives
// the firings written. A busy hour's firings start in it, a minute to
// an hour long, with the labels of TestScale's. after is called after
// each run.
func sizingWeek(t *testing.T, ctx context.Context, l *ledger.Ledger, r *rand.Rand, week, rules, perWeek int, services []string, after func()) int {
	t.Helper()
	o := verdict.Options{Lead: 15 * time.Minute, RefireWithin: time.Hour, UnknownLen: time.Hour, Match: []string{"service"}, Zone: time.UTC,
		Hours: verdict.DefaultHours(time.UTC)}
	from := sizingStart.Add(time.Duration(week) * 7 * 24 * time.Hour)
	var incs []incident.Incident
	for i := 0; i < 40; i++ {
		s := from.Add(time.Duration(r.IntN(7*24*60)) * time.Minute)
		incs = append(incs, incident.Incident{ID: fmt.Sprintf("INC-%d-%d", week, i), Start: s, End: s.Add(45 * time.Minute),
			Labels: incident.Labels{"service": {services[r.IntN(len(services))]}}})
	}
	if _, err := l.SyncFile(ctx, incs, o); err != nil {
		t.Fatal(err)
	}
	total, owed := 0, 0
	for h := 0; h < 7*24; h++ {
		at := from.Add(time.Duration(h) * time.Hour)
		// A week's firings spread over its hours, the remainder carried.
		n := (perWeek*(h+1))/(7*24) - owed
		owed += n
		fs := make([]firing.Firing, 0, n)
		for i := 0; i < n; i++ {
			s := at.Add(time.Duration(r.IntN(3600)) * time.Second).Truncate(time.Minute)
			fs = append(fs, firing.Firing{
				Rule:   fmt.Sprintf("Rule%04d", r.IntN(rules)),
				Labels: map[string]string{"service": services[r.IntN(len(services))], "pod": fmt.Sprintf("p%d", r.IntN(50)), "severity": "page"},
				Start:  s, End: s.Add(time.Duration(1+r.IntN(60)) * time.Minute),
			})
		}
		total += n
		f := ledger.Found{From: at, To: at.Add(time.Hour), Firings: fs, Run: ledger.Run{Kind: "hourly", Started: at.Add(time.Hour), Finished: at.Add(time.Hour)}}
		if err := l.Record(ctx, f, o); err != nil {
			t.Fatal(err)
		}
		after()
	}
	return total
}

// sizingVisit runs this binary's TestSizingVisit on the ledger in dir and
// reads the row's memory from what it prints.
func sizingVisit(t *testing.T, dir, pg string, end time.Time, row *sizingRow) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSizingVisit$", "-test.v")
	cmd.Env = append(os.Environ(), sizingVisitEnv+"="+dir, "LAPILLI_SIZING_END="+end.Format(time.RFC3339))
	if pg != "" { // the child opens the same schema, from a file as serve takes it
		f := filepath.Join(dir, "ledger-url")
		if err := os.WriteFile(f, []byte(pg+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd.Env = append(cmd.Env, sizingPGFileEnv+"="+f)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("visit: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(line, "SIZING "); ok {
			var m struct{ Base, Rest, Peak, After, RSS uint64 }
			if err := json.Unmarshal([]byte(rest), &m); err != nil {
				t.Fatal(err)
			}
			row.Base, row.Rest, row.Peak, row.After, row.RSS = m.Base, m.Rest, m.Peak, m.After, m.RSS
			return
		}
	}
	t.Fatalf("visit printed no numbers:\n%s", out)
}

// TestSizingVisit is the child's: serve opens the ledger in the directory
// the environment names, as at start, and what a person's visit does is
// done: the overview, the rules over twelve weeks, a rule's page, a week's
// report, each worked out afresh. Heap is Go's HeapInuse after a GC, at
// rest and after the visits, and at most sampled while they run; memory is
// the process's peak resident set, SQLite's page cache and the binary
// among it.
func TestSizingVisit(t *testing.T) {
	dir := os.Getenv(sizingVisitEnv)
	if dir == "" {
		t.Skip("run by TestSizing")
	}
	end, err := time.Parse(time.RFC3339, os.Getenv("LAPILLI_SIZING_END"))
	if err != nil {
		t.Fatal(err)
	}
	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapInuse
	}
	var m struct{ Base, Rest, Peak, After, RSS uint64 }
	m.Base = heap() // the test binary's own, before serve opens anything
	s, _ := server(t, func(c *Config) {
		c.Ledger = dir
		if f := os.Getenv(sizingPGFileEnv); f != "" {
			c.LedgerURLFile = f // the parent's schema, not the empty one server made
		}
		c.Options.Hours = verdict.DefaultHours(time.UTC)
	})
	s.now = func() time.Time { return end }
	h := s.Handler()
	visit := func() {
		for _, path := range []string{"/", "/rules?weeks=12", "/rules/Rule0007?weeks=12", "/api/v1/report?firings=false"} {
			s.cache.Lock()
			s.cache.gen = "" // worked out afresh
			s.cache.Unlock()
			s.outs.Lock()
			s.outs.v = nil
			s.outs.Unlock()
			s.creds.Lock()
			s.creds.by = nil // worked out afresh
			s.creds.Unlock()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if w.Code != http.StatusOK {
				t.Errorf("%s: %d", path, w.Code)
			}
		}
	}
	m.Rest = heap()
	done, peak := make(chan struct{}), make(chan uint64)
	go func() {
		var most uint64
		for {
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			most = max(most, ms.HeapInuse)
			select {
			case <-done:
				peak <- most
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	visit()
	close(done)
	m.Peak = <-peak
	m.After = heap()
	m.RSS = sizingPeakRSS()
	b, _ := json.Marshal(m)
	fmt.Printf("SIZING %s\n", b)
}

// sizingPeakRSS is this process's peak resident set, in bytes. On Linux it
// is /proc's VmHWM, this address space's own: getrusage's ru_maxrss there
// is kept across execve, and Go starts a child with vfork, sharing the
// parent's address space until the exec, so it would be the parent's peak,
// the writing's. Elsewhere ru_maxrss (bytes on macOS).
func sizingPeakRSS() uint64 {
	if b, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if rest, ok := strings.CutPrefix(line, "VmHWM:"); ok {
				if kb, err := strconv.ParseUint(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "kB")), 10, 64); err == nil {
					return kb * 1024
				}
			}
		}
	}
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return uint64(ru.Maxrss)
	}
	return uint64(ru.Maxrss) * 1024 // KB elsewhere
}
