package replay

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// What a real Grafana 13.3.1 and a real Prometheus 3.5.0 fired, beside the
// replay of the same rules against the same history, kept as testdata so
// it is checked on every change (docs/design/states.md). The rules' queries
// are functions of time() alone, so the history the replay reads is the
// one Grafana and Prometheus read. To record again, with both running as
// docs/design/states.md describes:
//
//	LAPILLI_RECORD=http://127.0.0.1:19090 go test ./internal/replay -run TestReal
//
// which asks that Prometheus for every answer the replay needs, and for
// what really fired, and writes them here.

// recorded is a history kept: each question asked, with its answer.
type recorded struct {
	mu   sync.Mutex
	h    History // asked when recording; nil when replaying what was kept
	Asks map[string][]prom.Series
}

func askKey(expr string, start, end time.Time, step time.Duration) string {
	return fmt.Sprintf("%s\x00%d\x00%d\x00%s", expr, start.UnixMilli(), end.UnixMilli(), step)
}

func (r *recorded) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	k := askKey(expr, start, end, step)
	r.mu.Lock()
	s, ok := r.Asks[k]
	r.mu.Unlock()
	if ok {
		return s, nil
	}
	if r.h == nil {
		return nil, fmt.Errorf("not recorded: %q from %s to %s every %s", expr, start, end, step)
	}
	s, err := r.h.QueryRange(ctx, expr, start, end, step)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.Asks[k] = s
	r.mu.Unlock()
	return s, nil
}

// realCase is one recording: the rules, read from a file or from the
// rules API's answer, the period, and the series that say what really
// fired.
type realCase struct {
	dir, series string
	from, to    time.Time
	step        time.Duration
}

func (c realCase) path(name string) string { return filepath.Join("testdata", c.dir, name) }

// load reads, or records, a case: its rules, what was asked and answered,
// and what really fired.
func (c realCase) load(t *testing.T) ([]rules.Rule, *recorded, []firing.Firing) {
	t.Helper()
	base := os.Getenv("LAPILLI_RECORD")
	var rs []rules.Rule
	var err error
	if b, ferr := os.ReadFile(c.path("rules.json")); ferr == nil {
		rs, err = rules.Parse(b)
	} else if base != "" {
		var api []byte
		if api, err = prom.New(base, "lapilli-test").Rules(context.Background()); err == nil {
			if err = os.WriteFile(c.path("api.json"), api, 0o644); err == nil {
				rs, err = rules.FromAPI(api)
			}
		}
	} else {
		var api []byte
		if api, err = os.ReadFile(c.path("api.json")); err == nil {
			rs, err = rules.FromAPI(api)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorded{Asks: map[string][]prom.Series{}}
	if base != "" {
		rec.h = prom.New(base, "lapilli-test")
	} else {
		f, err := os.Open(c.path("answers.json.gz"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		z, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewDecoder(z).Decode(&rec.Asks); err != nil {
			t.Fatal(err)
		}
	}
	real, err := rec.QueryRange(context.Background(), c.series, c.from, c.to, c.step)
	if err != nil {
		t.Fatal(err)
	}
	return rs, rec, firing.FromAlerts(real, c.step, c.to)
}

// save writes what was asked, when recording.
func (c realCase) save(t *testing.T, rec *recorded) {
	if rec.h == nil {
		return
	}
	f, err := os.Create(c.path("answers.json.gz"))
	if err != nil {
		t.Fatal(err)
	}
	z := gzip.NewWriter(f)
	if err := json.NewEncoder(z).Encode(rec.Asks); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// sameFirings sets two sets of firings beside each other over a period
// from from, by rule, start and end, with the labels both have agreeing, as
// verdict.CheckReplay does: what differs, written out. As there, both see
// one on since before the period as starting with it, and one over by its
// start isn't in it.
func sameFirings(replayed, real []firing.Firing, from time.Time, near time.Duration) []string {
	clamp := func(fs []firing.Firing) []firing.Firing {
		var out []firing.Firing
		for _, f := range fs {
			if !f.End.IsZero() && !f.End.After(from) {
				continue
			}
			if f.Start.Before(from) {
				f.Start = from
			}
			out = append(out, f)
		}
		return out
	}
	replayed, real = clamp(replayed), clamp(real)
	var out []string
	used := make([]bool, len(real))
	for _, f := range replayed {
		found := false
		for i, g := range real {
			if used[i] || g.Rule != f.Rule || f.Start.Sub(g.Start).Abs() > near || f.End.IsZero() != g.End.IsZero() ||
				!f.End.IsZero() && f.End.Sub(g.End).Abs() > near {
				continue
			}
			agree := true
			for k, v := range f.Labels {
				if w, ok := g.Labels[k]; ok && w != v {
					agree = false
				}
			}
			if agree {
				used[i], found = true, true
				break
			}
		}
		if !found {
			out = append(out, fmt.Sprintf("only replayed: %s %v %s-%s", f.Rule, f.Labels, f.Start.Format("15:04:05"), f.End.Format("15:04:05")))
		}
	}
	for i, g := range real {
		if !used[i] {
			out = append(out, fmt.Sprintf("only real: %s %v %s-%s", g.Rule, g.Labels, g.Start.Format("15:04:05"), g.End.Format("15:04:05")))
		}
	}
	sort.Strings(out)
	return out
}

// TestRealGrafana: Grafana's no data states, NoData, Alerting, OK and
// KeepLast, each with no "for" and with 30s; an alert whose series goes
// missing; classic conditions with every reducer no PromQL function has,
// count_non_null, avg over NaN, median and count joined by or, a window
// ending before the evaluation, and no data; a math && condition; and, as
// the review before release found them, a series there 60s of 120s with
// for: 1m and with keepFiringFor: 1m, keepFiringFor through no data and
// while its value is 0, and a pending alert whose series goes and comes
// back. Each firing the replay has, Grafana had, starting and ending at
// the same evaluation; and none other. The rule whose query fails can't be
// replayed, and its error alerts are left out of what really fired. The
// queries are functions of time(), changing every second: they are read
// as if scraped every 10s.
func TestRealGrafana(t *testing.T) {
	c := realCase{dir: "grafana-13.3.1", series: `GRAFANA_ALERTS{alertstate="firing",grafana_alertstate!="error"}`,
		from: time.Date(2026, 11, 11, 14, 26, 40, 0, time.UTC), to: time.Date(2026, 11, 11, 15, 12, 40, 0, time.UTC), step: 10 * time.Second}
	rs, rec, real := c.load(t)
	rules.Replaying{Scrape: 10 * time.Second}.Apply(rs)
	ran, fs := Run(context.Background(), rec, []Version{{Rules: rs}}, c.from, c.to, c.step, 4)
	c.save(t, rec)
	for _, r := range ran {
		if r.Unsupported != "" && r.Name != "Query error" {
			t.Errorf("%s: %s", r.Name, r.Unsupported)
		}
	}
	// The ends of the period cut firings short both ways alike.
	if diff := sameFirings(fs, real, c.from, 0); len(diff) > 0 {
		t.Errorf("%d differ:\n%s", len(diff), strings.Join(diff, "\n"))
	}
	if len(fs) < 200 {
		t.Errorf("only %d firings: the recording is too short to say much", len(fs))
	}
	check := verdict.CheckReplay(context.Background(), ran, fs, real, c.from, c.to, c.step)
	if check.Alike != check.Replayed || check.Alike != check.Real {
		t.Errorf("check: %d replayed, %d real, %d alike", check.Replayed, check.Real, check.Alike)
	}
}

// TestRealPrometheus: groups placed where Prometheus placed them, as its
// rules API says. The same 40s window every three minutes, at a one-minute
// interval, fires only for groups placed in its first 40s; a group with
// query_offset reads a minute back.
func TestRealPrometheus(t *testing.T) {
	c := realCase{dir: "prometheus-3.5.0", series: `ALERTS{alertstate="firing"}`,
		from: time.Date(2026, 11, 11, 14, 26, 40, 0, time.UTC), to: time.Date(2026, 11, 11, 15, 12, 40, 0, time.UTC), step: 10 * time.Second}
	rs, rec, real := c.load(t)
	_, fs := Run(context.Background(), rec, []Version{{Rules: rs}}, c.from, c.to, c.step, 4)
	// ALERTS is read every 10s from the epoch; a group placed 7s past it
	// fires 3s before what is read says.
	if diff := sameFirings(fs, real, c.from, c.step); len(diff) > 0 {
		t.Errorf("%d differ:\n%s", len(diff), strings.Join(diff, "\n"))
	}
	var placed []string
	for _, r := range rs {
		if r.Phase > 0 {
			placed = append(placed, r.Name)
		}
	}
	if len(placed) != len(rs) {
		t.Errorf("placed %v of %d", placed, len(rs))
	}
	// Not placed, on the steps' grid, the groups that never fire do.
	var plain []rules.Rule
	for _, r := range rs {
		r.Phase = 0
		plain = append(plain, r)
	}
	_, unplaced := Run(context.Background(), rec, []Version{{Rules: plain}}, c.from, c.to, c.step, 4)
	c.save(t, rec)
	if !slices.ContainsFunc(sameFirings(unplaced, real, c.from, c.step), func(d string) bool { return strings.HasPrefix(d, "only replayed: SpikeD") }) {
		t.Error("unplaced, SpikeD should fire where Prometheus never fired it")
	}
}
