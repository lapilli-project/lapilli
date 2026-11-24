package replay

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/content"
	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// What a real Grafana and a real Prometheus fired, beside the replay of the
// same rules against the same history, kept as testdata so it is checked on
// every change (docs/design/states.md). The rules' queries are functions of
// time() alone, so the history the replay reads is the one Grafana and
// Prometheus read.
//
// Every directory of testdata named for a server and its version
// (grafana-13.3.1, prometheus-3.5.0, loki-3.7.9, with a suffix for a
// different set of rules: -export, -scraped, -change, -loki) is a recording,
// and each test replays every one of its kind: a new version's recording
// is a new directory and nothing else. The period of a recording is in
// its period.json (from, to, step, and for -change when the rule was
// changed). To record again, test/e2e/measure.sh runs the servers and
// does it:
//
//	LAPILLI_RECORD=http://127.0.0.1:19090 LAPILLI_RECORD_DIR=grafana-13.3.1 \
//	go test ./internal/replay -run 'TestRealGrafana$'
//
// which asks that Prometheus for every answer the replay needs, and for
// what really fired, and writes them into that directory, whose period.json
// and rules (rules.json, rules.yaml, or rules.yml for Prometheus's) are
// already there. A server runs the rules of every recording at once, so
// what really fired is taken for the rules of the recording alone.

// recorded is a history kept: each question asked, with its answer.
type recorded struct {
	mu   sync.Mutex
	h    History // asked when recording; nil when replaying what was kept
	Asks map[string][]prom.Series
}

func askKey(expr string, start, end time.Time, step time.Duration) string {
	return fmt.Sprintf("%s\x00%d\x00%d\x00%s", expr, start.UnixMilli(), end.UnixMilli(), step)
}

// refusedKey keeps, beside an ask's key, that the server refused it as
// written, with the answer's message: a query Prometheus can't read.
const refusedKey = "refused\x00"

func (r *recorded) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	k := askKey(expr, start, end, step)
	r.mu.Lock()
	s, ok := r.Asks[k]
	refused, no := r.Asks[refusedKey+k]
	r.mu.Unlock()
	if ok {
		return s, nil
	}
	if no && len(refused) == 1 {
		return nil, &prom.APIError{Status: http.StatusBadRequest, Msg: refused[0].Labels["error"]}
	}
	if r.h == nil {
		return nil, fmt.Errorf("not recorded: %q from %s to %s every %s", expr, start, end, step)
	}
	s, err := r.h.QueryRange(ctx, expr, start, end, step)
	var api *prom.APIError
	if errors.As(err, &api) && api.Status == http.StatusBadRequest {
		r.mu.Lock()
		r.Asks[refusedKey+k] = []prom.Series{{Labels: map[string]string{"error": api.Msg}}}
		r.mu.Unlock()
	}
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
	changed     time.Time // when a rule was changed, if a recording does
}

func (c realCase) path(name string) string { return filepath.Join("testdata", c.dir, name) }

// period reads the recording's period.json: from, to and step, and, for a
// recording where a rule was changed, when.
func (c *realCase) period(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile(c.path("period.json"))
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		From, To, Step, Changed string
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		t.Fatalf("%s: %v", c.path("period.json"), err)
	}
	if c.from, err = time.Parse(time.RFC3339, p.From); err != nil {
		t.Fatal(err)
	}
	if c.to, err = time.Parse(time.RFC3339, p.To); err != nil {
		t.Fatal(err)
	}
	if c.step, err = time.ParseDuration(p.Step); err != nil || c.step <= 0 || !c.to.After(c.from) {
		t.Fatalf("%s: from %s, to %s, step %q", c.path("period.json"), p.From, p.To, p.Step)
	}
	if p.Changed != "" {
		if c.changed, err = time.Parse(time.RFC3339, p.Changed); err != nil {
			t.Fatal(err)
		}
	}
}

// recordings runs fn for every recording of a server (grafana,
// prometheus, loki) with the suffix (none, or export, scraped, change,
// loki) that testdata holds, each as a subtest named for its directory.
// When recording, only the directory LAPILLI_RECORD_DIR names, which the
// recording's script has made: one server has one version.
func recordings(t *testing.T, server, suffix, series string, fn func(t *testing.T, c realCase)) {
	t.Helper()
	re := regexp.MustCompile(`^` + server + `-[0-9][0-9.]*(?:-([a-z]+))?$`)
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	only := ""
	if os.Getenv("LAPILLI_RECORD") != "" {
		if only = os.Getenv("LAPILLI_RECORD_DIR"); only == "" {
			t.Fatal("recording: LAPILLI_RECORD_DIR names the directory to record into (one version of one server)")
		}
	}
	n := 0
	for _, e := range entries {
		m := re.FindStringSubmatch(e.Name())
		if !e.IsDir() || m == nil || m[1] != suffix || only != "" && e.Name() != only {
			continue
		}
		n++
		t.Run(e.Name(), func(t *testing.T) {
			c := realCase{dir: e.Name(), series: series}
			c.period(t)
			fn(t, c)
		})
	}
	if n == 0 {
		t.Errorf("no recording of %s in testdata (recording only %q)", re, only)
	}
}

// grafanaFired is what Grafana's state history says fired, and
// promFired what Prometheus's ALERTS does.
const (
	grafanaFired = `GRAFANA_ALERTS{alertstate="firing",grafana_alertstate!="error"}`
	promFired    = `ALERTS{alertstate="firing"}`
)

// onlyRules is the firings of the rules named, of what really fired: the
// servers that were recorded run every recording's rules at once.
func onlyRules(real []firing.Firing, rs []rules.Rule) []firing.Firing {
	known := map[string]bool{}
	for _, r := range rs {
		known[r.Name] = true
	}
	return slices.DeleteFunc(real, func(f firing.Firing) bool { return !known[f.Rule] })
}

// load reads, or records, a case: its rules, what was asked and answered,
// and what really fired.
func (c realCase) load(t *testing.T) ([]rules.Rule, *recorded, []firing.Firing) {
	t.Helper()
	base := os.Getenv("LAPILLI_RECORD")
	var rs []rules.Rule
	var err error
	if b, ferr := os.ReadFile(c.path("rules.json")); ferr == nil {
		rs, err = rules.Parse(b)
	} else if b, ferr := os.ReadFile(c.path("rules.yaml")); ferr == nil {
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
	rec, real := c.recorded(t)
	return rs, rec, real
}

// recorded is the case's recording, or, when recording, a history that
// keeps what it asks; and what really fired.
func (c realCase) recorded(t *testing.T) (*recorded, []firing.Firing) {
	t.Helper()
	base := os.Getenv("LAPILLI_RECORD")
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
	return rec, firing.FromAlerts(real, c.step, c.to)
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
				// A template the replay doesn't fill in, as the check passes it over.
				if w, ok := g.Labels[k]; ok && w != v && !strings.Contains(v, "{{") {
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
	recordings(t, "grafana", "", grafanaFired, func(t *testing.T, c realCase) {
		rs, rec, real := c.load(t)
		real = onlyRules(real, rs)
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
	})
}

// TestRealGrafanaExport: rules as Grafana 13.3.1's provisioning export,
// which --grafana reads, writes them: a title and labels with each $
// written $$, and queries with $__rate_interval, $__range and
// ${__interval}, which hold only at the values Grafana puts in. Each
// firing alike, as Grafana fired it under its own title and labels.
func TestRealGrafanaExport(t *testing.T) {
	recordings(t, "grafana", "export", grafanaFired, func(t *testing.T, c realCase) {
		rs, rec, real := c.load(t)
		ran, fs := Run(context.Background(), rec, []Version{{Rules: rs}}, c.from, c.to, c.step, 4)
		c.save(t, rec)
		for _, r := range ran {
			if r.Unsupported != "" {
				t.Errorf("%s: %s", r.Name, r.Unsupported)
			}
		}
		known := map[string]bool{}
		for _, r := range rs {
			known[r.Name] = true
		}
		real = slices.DeleteFunc(real, func(f firing.Firing) bool { return !known[f.Rule] })
		if diff := sameFirings(fs, real, c.from, 0); len(diff) > 0 || len(fs) != 3 {
			t.Errorf("%d firings; %d differ:\n%s", len(fs), len(diff), strings.Join(diff, "\n"))
		}
		if !slices.ContainsFunc(fs, func(f firing.Firing) bool { return f.Rule == "Cost $5" && f.Labels["budget"] == "$5" }) {
			t.Errorf("Cost $5: %+v", fs)
		}
	})
}

// TestRealPrometheus: groups placed where Prometheus placed them, as its
// rules API says. The same 40s window every three minutes, at a one-minute
// interval, fires only for groups placed in its first 40s; a group with
// query_offset reads a minute back.
func TestRealPrometheus(t *testing.T) {
	recordings(t, "prometheus", "", promFired, func(t *testing.T, c realCase) {
		rs, rec, real := c.load(t)
		real = onlyRules(real, rs)
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
	})
}

// scrapedSettings are how the rules of grafana-13.3.1-scraped are
// replayed: the scrape interval each data source said it had, as Grafana
// answered for them, and Prometheus's configuration and targets, as it
// served them.
func scrapedSettings(t *testing.T, c realCase) (rules.DataSources, content.Scrapes) {
	t.Helper()
	b, err := os.ReadFile(c.path("datasources.json"))
	if err != nil {
		t.Fatal(err)
	}
	var said struct{ DataSources map[string]string }
	if err := json.Unmarshal(b, &said); err != nil {
		t.Fatal(err)
	}
	ds := rules.DataSources{}
	for uid, v := range said.DataSources {
		d, _ := time.ParseDuration(v)
		ds[uid] = d
	}
	cfg, err := os.ReadFile(c.path("prometheus.yml"))
	if err != nil {
		t.Fatal(err)
	}
	scrapes, ok := content.ScrapeConfig(string(cfg))
	targets, err := os.ReadFile(c.path("targets.json"))
	if !ok || err != nil || !scrapes.Targets(targets) {
		t.Fatalf("configuration %v, targets %v", ok, err)
	}
	return ds, scrapes
}

// TestRealGrafanaScraped: what the verification before release found, on
// a gauge scraped every 15s, 100 for 15s every two minutes, one 100 the
// other way round, and one there for a minute of every 15. Grafana 13.3.1
// put $__rate_interval in with its data source's 30s; classic max, min,
// avg, diff and last and a reduce's max over a minute and five, which the
// server's own resolution, a minute, steps over; a query's nothing seen
// on a 10s rule, a minute of every 15; and a query beside the condition,
// returning nothing or refused as written, which Grafana takes as no
// data. Each firing alike, starting and ending at the same evaluation,
// read as verdict and serve read them when Prometheus serves its
// configuration, and when it doesn't (Grafana's 1s), and at
// --scrape-interval 15s: a reduce asked every 5s, a step of the rule's
// 10s evaluations, which asked every 15s, on the epoch's grid, started 60
// of the firings and ended 20 an evaluation after Grafana.
func TestRealGrafanaScraped(t *testing.T) {
	recordings(t, "grafana", "scraped", grafanaFired, func(t *testing.T, c realCase) {
		all, rec, real := c.load(t)
		var rs []rules.Rule
		known := map[string]bool{}
		for _, r := range all {
			if r.Group == "fid2/fid2" {
				rs = append(rs, r)
				known[r.Name] = true
			}
		}
		real = slices.DeleteFunc(real, func(f firing.Firing) bool { return !known[f.Rule] })
		ds, scrapes := scrapedSettings(t, c)
		for _, set := range []struct {
			name string
			p    rules.Replaying
		}{
			{"as Prometheus's configuration says", rules.Replaying{DataSources: ds, ScrapeOf: scrapes.Shortest}},
			{"not knowing how often it scrapes", rules.Replaying{DataSources: ds}},
			{"--scrape-interval 15s", rules.Replaying{DataSources: ds, Scrape: 15 * time.Second}},
		} {
			got := slices.Clone(rs)
			set.p.Apply(got)
			ran, fs := Run(context.Background(), rec, []Version{{Rules: got}}, c.from, c.to, c.step, 4)
			for _, r := range ran {
				if r.Unsupported != "" || len(r.Unseen()) > 0 {
					t.Errorf("%s: %s: %q %v", set.name, r.Name, r.Unsupported, r.Unseen())
				}
			}
			if diff := sameFirings(fs, real, c.from, 0); len(diff) > 0 || len(fs) < 100 {
				t.Errorf("%s: %d firings, %d differ:\n%s", set.name, len(fs), len(diff), strings.Join(diff, "\n"))
			}
		}
		c.save(t, rec)
	})
}

// TestRealGrafanaChange: three rules Grafana changed through its API at
// the time period.json gives (17:45:05 in the first recording): K1's threshold, which starts its alert afresh, pending
// for its for again; K2's summary and K3's keepFiringFor, which carry
// theirs on. The versions read from Grafana's export before and after.
func TestRealGrafanaChange(t *testing.T) {
	recordings(t, "grafana", "change", grafanaFired, func(t *testing.T, c realCase) {
		var versions []Version
		known := map[string]bool{}
		for i, name := range []string{"rules-before.yaml", "rules-after.yaml"} {
			b, err := os.ReadFile(c.path(name))
			if err != nil {
				t.Fatal(err)
			}
			all, err := rules.Parse(b)
			if err != nil {
				t.Fatal(err)
			}
			v := Version{}
			if i == 1 {
				v.From = c.changed
			}
			for _, r := range all {
				if r.Group == "fid2/fid2api" {
					v.Rules = append(v.Rules, r)
					known[r.Name] = true
				}
			}
			versions = append(versions, v)
		}
		rec, real := c.recorded(t)
		real = slices.DeleteFunc(real, func(f firing.Firing) bool { return !known[f.Rule] })
		ran, fs := Run(context.Background(), rec, versions, c.from, c.to, c.step, 4)
		c.save(t, rec)
		for _, r := range ran {
			if r.Unsupported != "" {
				t.Errorf("%s: %s", r.Name, r.Unsupported)
			}
		}
		// K1's alert, started afresh, reads as firing at the evaluation the
		// change took effect at too: Grafana stops its series a moment after
		// that evaluation (its stale marker). The rest alike to the second.
		// Evaluations are on the group's 10s grid: the change, between two,
		// takes effect at the next, and K1, for 30s, fires 30s after it.
		next := c.changed.Truncate(c.step).Add(c.step)
		k1End := [2]string{next.Format("15:04:05"), next.Add(c.step).Format("15:04:05")}
		afresh := next.Add(30 * time.Second)
		exact := slices.DeleteFunc(sameFirings(fs, real, c.from, 0), func(d string) bool {
			return strings.Contains(d, "K1 threshold changed") && (strings.HasSuffix(d, c.from.Format("15:04:05")+"-"+k1End[0]) || strings.HasSuffix(d, c.from.Format("15:04:05")+"-"+k1End[1]))
		})
		if diff := sameFirings(fs, real, c.from, c.step); len(diff) > 0 || len(exact) > 0 || len(known) != 3 {
			t.Errorf("%d firings; %d differ, %d by more than K1's end:\n%s", len(fs), len(diff), len(exact), strings.Join(append(diff, exact...), "\n"))
		}
		if !slices.ContainsFunc(fs, func(f firing.Firing) bool {
			return f.Rule == "K1 threshold changed" && f.Start.Equal(afresh)
		}) {
			t.Errorf("K1 isn't pending afresh: %+v", fs)
		}
		var k2 []firing.Firing
		for _, f := range fs {
			if f.Rule == "K2 annotation changed" {
				k2 = append(k2, f)
			}
		}
		if len(k2) != 1 || !k2[0].End.IsZero() {
			t.Errorf("K2's alert isn't carried across what it says changing: %+v", k2)
		}
	})
}
