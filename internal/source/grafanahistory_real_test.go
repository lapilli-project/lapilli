package source

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// What real Grafanas kept in their alert state history, beside what they
// wrote to Prometheus as GRAFANA_ALERTS over the same time, kept as
// testdata (docs/design/grafanahistory.md): each Grafana's answers, as its
// API gave them, served again by a fake that pages as it does. Each firing
// read from the history is one GRAFANA_ALERTS has, starting and ending at
// the same evaluation, and none other.
type realHistory struct {
	dir, where string
	// alertsDir is the dir whose GRAFANA_ALERTS the history is set
	// beside, when not dir: for a Grafana keeping its history in Loki
	// alone, another Grafana's of the same version, running the same rules
	// from the same time.
	alertsDir string
	// read is when the history is read from; from and to what is set
	// beside GRAFANA_ALERTS.
	read, from, to time.Time
	firings        int
	// unseen are the firings GRAFANA_ALERTS has from before read to after
	// to, which changed state in none of it: the history has nothing to
	// say of them.
	unseen int
	// unsure are the rules whose alerts the history doesn't tell apart, as
	// its annotations' tags leave out what does.
	unsure []string
}

func (c realHistory) file(dir, name string) []byte {
	f, err := os.Open(filepath.Join("testdata", dir, name))
	if err != nil {
		panic(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		panic(err)
	}
	b, err := io.ReadAll(z)
	if err != nil {
		panic(err)
	}
	return b
}

// recorded is a fake Grafana answering from what a real one answered:
// its frontend settings, its alert annotations, newest first, from and to
// within, as many as asked, one rule's alone with alertUID; its state
// history kept in Loki likewise, to before to, in seconds.
func (c realHistory) recorded(t *testing.T) GrafanaHistory {
	t.Helper()
	settings, err := os.ReadFile(filepath.Join("testdata", c.dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The title of each rule's uid, as GRAFANA_ALERTS labels its alerts:
	// an annotation names its rule by its id and its title alone.
	titleOf := map[string]string{}
	if c.where == HistoryAnnotations {
		for _, f := range c.alerts(t) {
			titleOf[f.Labels["grafana_rule_uid"]] = f.Rule
		}
	}
	var notes []json.RawMessage
	var lines struct {
		Schema json.RawMessage `json:"schema"`
		Data   struct {
			Values [3][]json.RawMessage `json:"values"`
		} `json:"data"`
	}
	if c.where == HistoryAnnotations {
		if err := json.Unmarshal(c.file(c.dir, "annotations.json.gz"), &notes); err != nil {
			t.Fatal(err)
		}
	} else if err := json.Unmarshal(c.file(c.dir, "history.json.gz"), &lines); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var from, to, limit int64
		fmt.Sscan(q.Get("from"), &from)
		fmt.Sscan(q.Get("to"), &to)
		fmt.Sscan(q.Get("limit"), &limit)
		switch r.URL.Path {
		case "/api/frontend/settings":
			w.Write(settings)
		case "/api/annotations":
			var out []json.RawMessage
			uid := q.Get("alertUID")
			for _, n := range notes {
				var a annotation
				json.Unmarshal(n, &a)
				if uid != "" && (titleOf[uid] == "" || a.AlertName != titleOf[uid]) {
					continue
				}
				if a.Time >= from && a.Time <= to {
					out = append(out, n)
				}
			}
			out = out[:min(len(out), int(limit))] // as recorded, newest first
			json.NewEncoder(w).Encode(out)
		case "/api/v1/rules/history":
			v := lines.Data.Values
			var keep []int
			for i := range v[0] {
				var ms int64
				json.Unmarshal(v[0][i], &ms)
				if ms >= from*1000 && ms < to*1000 {
					keep = append(keep, i)
				}
			}
			// The newest as many as asked, each stream's together as
			// recorded.
			byTime := slices.Clone(keep)
			slices.SortStableFunc(byTime, func(a, b int) int { return strings.Compare(string(v[0][b]), string(v[0][a])) })
			if limit > 0 && int(limit) < len(byTime) {
				cut := map[int]bool{}
				for _, i := range byTime[:limit] {
					cut[i] = true
				}
				keep = slices.DeleteFunc(keep, func(i int) bool { return !cut[i] })
			}
			var out [3][]json.RawMessage
			for _, i := range keep {
				for k := range 3 {
					out[k] = append(out[k], v[k][i])
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"schema": lines.Schema, "data": map[string]any{"values": out}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return GrafanaHistory{URL: srv.URL, Agent: "test"}
}

// alerts is GRAFANA_ALERTS as the Prometheus it was written to answered,
// every 10s.
func (c realHistory) alerts(t *testing.T) []firing.Firing {
	t.Helper()
	dir := c.dir
	if c.alertsDir != "" {
		dir = c.alertsDir
	}
	body := c.file(dir, "alerts.json.gz")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer srv.Close()
	ss, err := prom.New(srv.URL, "test").QueryRange(context.Background(), `GRAFANA_ALERTS{alertstate="firing",grafana_alertstate!="error"}`, c.read, c.to, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return firing.FromAlerts(ss, 10*time.Second, c.to)
}

// within is firings over [from, to], one on since before from starting
// at it, as a check sees them; GRAFANA_ALERTS's own label of a rule's uid,
// which the history has no label for, left out.
func within(fs []firing.Firing, from, to time.Time) []string {
	var out []string
	for _, f := range fs {
		if !f.Active(from, to) || !f.End.IsZero() && !f.End.After(from) || !f.Start.Before(to) {
			continue
		}
		start, end := f.Start, "on"
		if start.Before(from) {
			start = from
		}
		if !f.End.IsZero() && f.End.Before(to) {
			end = f.End.Format("15:04:05")
		}
		l := maps.Clone(f.Labels)
		delete(l, "grafana_rule_uid")
		out = append(out, fmt.Sprintf("%s %v %s-%s", f.Rule, l, start.Format("15:04:05"), end))
	}
	slices.Sort(out)
	return out
}

func TestRealGrafanaHistory(t *testing.T) {
	for _, c := range realHistories {
		t.Run(c.dir, func(t *testing.T) {
			h := c.recorded(t)
			where, err := h.Where(context.Background())
			if err != nil || where.Serves != c.where {
				t.Fatalf("where: %+v %v", where, err)
			}
			ts, read, err := h.Read(context.Background(), where.Serves, c.read, c.to)
			if err != nil || read.Passed != 0 {
				t.Fatalf("%+v %v", read, err)
			}
			alerts := c.alerts(t)
			raw := slices.Clone(alerts)
			unseen := 0
			alerts = slices.DeleteFunc(alerts, func(f firing.Firing) bool {
				all := !f.Start.After(c.read) && (f.End.IsZero() || f.End.After(c.to))
				if all {
					unseen++
				}
				return all
			})
			if unseen != c.unseen {
				t.Errorf("%d firings on all through what was read, want %d", unseen, c.unseen)
			}
			fs, unsure := firing.FromTransitions(ts, c.read)
			if len(unsure) != len(c.unsure) || !slices.Equal(unsure, c.unsure) {
				t.Errorf("unsure: %v, want %v", unsure, c.unsure)
			}
			// As the check sets them beside each other: in annotations, as
			// their tags keep labels; the rules unsure not checked.
			if c.where == HistoryAnnotations {
				for i := range alerts {
					// As Grafana writes them, tags_length (500) too.
					l := maps.Clone(alerts[i].Labels)
					l["alertname"] = alerts[i].Rule
					delete(l, "grafana_rule_uid")
					kept, _ := tagLabels(grafanaTags(l, 500))
					alerts[i].Rule = kept["alertname"]
					delete(kept, "alertname")
					alerts[i].Labels = kept
				}
			}
			fs = slices.DeleteFunc(fs, func(f firing.Firing) bool { return slices.Contains(c.unsure, f.Rule) })
			alerts = slices.DeleteFunc(alerts, func(f firing.Firing) bool { return slices.Contains(c.unsure, f.Rule) })
			got, want := within(fs, c.from, c.to), within(alerts, c.from, c.to)
			var differ []string
			for _, l := range got {
				if !slices.Contains(want, l) {
					differ = append(differ, "only in the history: "+l)
				}
			}
			for _, l := range want {
				if !slices.Contains(got, l) {
					differ = append(differ, "only in GRAFANA_ALERTS: "+l)
				}
			}
			if len(differ) > 0 || len(got) != c.firings {
				t.Errorf("%d firings, want %d; %d differ:\n%s", len(got), c.firings, len(differ), strings.Join(differ, "\n"))
			}
			// As the check counts them: every one alike.
			var known []rules.Rule
			for _, f := range alerts {
				known = append(known, rules.Rule{Name: f.Rule, Interval: 10 * time.Second, Graph: &rules.Graph{}})
			}
			check := verdict.CheckReplay(context.Background(), known, fs, alerts, c.from, c.to, 10*time.Second)
			if check.Alike != check.Replayed || check.Alike != check.Real || check.Alike == 0 {
				t.Errorf("check: %d from the history, %d from GRAFANA_ALERTS, %d alike", check.Replayed, check.Real, check.Alike)
			} else {
				t.Logf("%d of %d alike", check.Alike, check.Real)
			}
			if c.where != HistoryAnnotations {
				return
			}
			// And as the check sets the replay beside annotations: its
			// labels as AnnotationLabels gives them, the history's as they
			// are, without what tags_length left out of them; every one
			// alike still.
			raw = slices.DeleteFunc(raw, func(f firing.Firing) bool {
				return slices.Contains(c.unsure, AnnotationName(f.Rule)) || !f.Start.After(c.read) && (f.End.IsZero() || f.End.After(c.to))
			})
			for i := range raw {
				raw[i].Rule, raw[i].Labels = AnnotationName(raw[i].Rule), AnnotationLabels(raw[i].Labels)
			}
			if again := verdict.CheckReplay(context.Background(), known, raw, fs, c.from, c.to, 10*time.Second); again.Alike != check.Alike || again.Alike != again.Real {
				t.Errorf("as the check sets them: %d of %d alike, %d from the replay's side", again.Alike, again.Real, again.Replayed)
			}
		})
	}
}

// Recorded on 2026-11-16: four Grafanas started at 07:17:30 UTC with the
// same rules (docs/design/grafanahistory.md), set beside GRAFANA_ALERTS
// from 07:30 to 08:30. Grafana 12.4.14 keeping its history in annotations
// alone, as it does unless told otherwise, and 13.3.1 keeping it in Loki
// alone, are set beside the GRAFANA_ALERTS of the one of their version
// writing both. The annotations are read from before the Grafanas started;
// Loki's from 07:25, as it took nothing before 07:21 (its disk full).
var realHistories = func() []realHistory {
	start, loki := time.Date(2026, 11, 16, 7, 17, 0, 0, time.UTC), time.Date(2026, 11, 16, 7, 25, 0, 0, time.UTC)
	from, to := time.Date(2026, 11, 16, 7, 30, 0, 0, time.UTC), time.Date(2026, 11, 16, 8, 30, 0, 0, time.UTC)
	return []realHistory{
		{dir: "grafana-12.4.14-history", where: HistoryAnnotations, read: start, from: from, to: to, firings: 426},
		{dir: "grafana-12.4.14-default-history", alertsDir: "grafana-12.4.14-history", where: HistoryAnnotations, read: start, from: from, to: to, firings: 426},
		{dir: "grafana-13.3.1-history", where: HistoryAnnotations, read: start, from: from, to: to, firings: 426},
		{dir: "grafana-13.3.1-loki-history", alertsDir: "grafana-13.3.1-history", where: HistoryLoki, read: loki, from: from, to: to, firings: 420, unseen: 6},
		// Recorded after the review, from 10:00:15 to 10:30, rules whose
		// labels show what the tags keep of them: an instance's port, a
		// URL, a title with a colon, a value of spaces, nine labels of 60
		// characters, the eighth of them past tags_length; and two alerts
		// told apart only by a path of 120 characters, which the tags leave
		// out: one alert to the history, its changes crossing, unsure.
		{dir: "grafana-12.4.14-tags", where: HistoryAnnotations, read: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC),
			from: time.Date(2026, 11, 16, 10, 5, 0, 0, time.UTC), to: time.Date(2026, 11, 16, 10, 30, 0, 0, time.UTC), firings: 68, unsure: []string{"Long label"}},
	}
}()
