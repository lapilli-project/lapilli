package replay

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// What a real Loki 3.7.9's ruler, and a real Grafana 13.3.1 on that Loki,
// fired, beside the replay of the same rules against the same logs, kept
// as testdata (docs/design/loki.md). Logs were written every second, each
// stamped at .95 of its second and sent before it, so no line was late,
// and none fell in the fraction of a second a Loki group runs past its
// second. To record again, with Loki, Prometheus (where Loki's ruler and
// Grafana write what they fired) and Grafana running as the design says:
//
//	LAPILLI_RECORD=http://127.0.0.1:9090 LAPILLI_RECORD_LOKI=http://127.0.0.1:3100 \
//	LAPILLI_RECORD_GRAFANA=http://127.0.0.1:3000 LAPILLI_RECORD_GRAFANA_TOKEN=FILE \
//	go test ./internal/replay -run TestRealLoki
//
// which asks Loki, and Grafana's data source proxy, for every answer the
// replay needs, Prometheus for what really fired, and writes them here.

// logsCase is a recording of rules written in LogQL: realCase's, and the
// Loki's answers apart.
type logsCase struct {
	realCase
}

// logs is the recording of what Loki answered, or, when recording, a
// history that keeps what it asks of live, a Loki's client.
func (c logsCase) logs(t *testing.T, live *prom.Client) *recorded {
	t.Helper()
	rec := &recorded{Asks: map[string][]prom.Series{}}
	if os.Getenv("LAPILLI_RECORD") != "" {
		rec.h = live
		return rec
	}
	f, err := os.Open(c.path("logs.json.gz"))
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
	return rec
}

// saveLogs writes what was asked of Loki, when recording.
func (c logsCase) saveLogs(t *testing.T, rec *recorded) {
	if rec.h == nil {
		return
	}
	f, err := os.Create(c.path("logs.json.gz"))
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

// kept is a file of the recording, or, when recording, what url answers,
// written there first.
func (c logsCase) kept(t *testing.T, name, url string, header map[string]string) []byte {
	t.Helper()
	if os.Getenv("LAPILLI_RECORD") != "" && url != "" {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range header {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %v", url, resp.StatusCode, err)
		}
		if err := os.WriteFile(c.path(name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(c.path(name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// lokiRules reads a Loki ruler's configuration API's answer, a namespace
// to its groups, as source.LokiLoaded reads it: a document a namespace.
func lokiRules(t *testing.T, b []byte) []rules.Rule {
	t.Helper()
	var all map[string][]any
	if err := yaml.Unmarshal(b, &all); err != nil {
		t.Fatal(err)
	}
	var docs []rules.Doc
	for _, ns := range slices.Sorted(func(yield func(string) bool) {
		for k := range all {
			if !yield(k) {
				return
			}
		}
	}) {
		data, err := yaml.Marshal(map[string]any{"groups": all[ns]})
		if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, rules.Doc{Source: "loki/" + ns, Namespace: ns, Logs: rules.LokiRuler, Data: data})
	}
	rs, err := rules.FromDocs(docs)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

// TestRealLoki: a Loki 3.7.9 ruler's rules, read as written from its
// configuration API, each group placed where Loki runs it as its rules
// API says: count_over_time, rate, bytes_over_time, max_over_time and
// quantile_over_time over an unwrap, absent_over_time, sum by, a for, and
// a keep_firing_for and a query_offset, which Loki 3.7.9 drops from what
// it holds, and doesn't honour. Each firing the replay has, Loki's ruler
// had, within the 10s ALERTS is read every; and none other.
func TestRealLoki(t *testing.T) {
	c := logsCase{realCase{dir: "loki-3.7.9", series: `ALERTS{alertstate="firing"}`,
		from: time.Date(2026, 11, 11, 19, 35, 0, 0, time.UTC), to: realTo(time.Date(2026, 11, 11, 20, 15, 0, 0, time.UTC)), step: 10 * time.Second}}
	base := os.Getenv("LAPILLI_RECORD_LOKI")
	rs := lokiRules(t, c.kept(t, "rules.yaml", base+"/loki/api/v1/rules", nil))
	places, err := rules.Places(c.kept(t, "api.json", base+"/prometheus/api/v1/rules", nil))
	if err != nil || len(places) == 0 {
		t.Fatalf("places %v: %v", places, err)
	}
	rules.Replaying{Places: places}.Apply(rs)
	rec, real := c.recorded(t)
	logs := c.logs(t, prom.New(base, "lapilli-test", prom.Loki()))
	h := WithLogs(rec, func(r rules.Rule) (History, error) { return logs, nil })
	ran, fs := Run(context.Background(), h, []Version{{Rules: rs}}, c.from, c.to, c.step, 4)
	c.save(t, rec)
	c.saveLogs(t, logs)
	for _, r := range ran {
		if _, placed := places[rules.Place{Namespace: r.Namespace, Group: r.Group}]; r.Unsupported != "" || r.Logs != rules.LokiRuler || !placed {
			t.Errorf("%s: %q, logs %q, placed %v", r.Name, r.Unsupported, r.Logs, placed)
		}
		if r.KeepFiringFor != 0 || r.QueryOffset != 0 {
			t.Errorf("%s: keep_firing_for %s, query_offset %s, which Loki doesn't hold", r.Name, r.KeepFiringFor, r.QueryOffset)
		}
	}
	// A firing on when the period starts is, for the replay, on from the
	// group's first evaluation in the period, up to its interval later.
	first := map[string]time.Time{}
	for _, r := range rs {
		iv := int64(Every(r, c.step) / time.Second)
		at := c.from.Unix() + ((int64(r.Phase/time.Second)-c.from.Unix())%iv+iv)%iv
		first[r.Name] = time.Unix(at, 0).UTC()
	}
	for i := range real {
		if !real[i].Start.After(c.from) {
			real[i].Start = first[real[i].Rule]
		}
	}
	// ALERTS is read every 10s from the epoch; a group placed past it
	// fires up to a step before what is read says.
	if diff := sameFirings(fs, real, c.from, c.step); len(diff) > 0 {
		t.Errorf("%d differ:\n%s", len(diff), strings.Join(diff, "\n"))
	}
	if len(fs) < 40 {
		t.Errorf("only %d firings: the recording is too short to say much", len(fs))
	}
	check := verdict.CheckReplay(context.Background(), ran, fs, real, c.from, c.to, c.step)
	if check.Alike != check.Replayed || check.Alike != check.Real {
		t.Errorf("check: %d replayed, %d real, %d alike", check.Replayed, check.Real, check.Alike)
	}
	// On the steps' grid, not where Loki runs each group, they don't.
	plain := slices.Clone(rs)
	for i := range plain {
		plain[i].Phase = 0
	}
	_, unplaced := Run(context.Background(), h, []Version{{Rules: plain}}, c.from, c.to, c.step, 4)
	if diff := sameFirings(unplaced, real, c.from, c.step); len(diff) == 0 {
		t.Error("on the steps' grid, the firings of a group placed 11s into its minute should start later than Loki started them")
	}
}

// TestRealLokiGrafana: Grafana 13.3.1's rules on a Loki 3.7.9 data source,
// asked through Grafana's data source proxy as --grafana asks them: an
// instant query, with NoData, OK and KeepLast; range queries reduced by
// last, max, mean, sum, count and min, at Grafana's step for each (1s,
// 3s, a query's own 10s with $__auto); a math expression over two
// queries; a for; series by level that come and go. A classic condition
// over Loki, a condition reading Prometheus and Loki together and one
// reading a MySQL data source are listed, not replayed. Each firing alike,
// starting and ending at the same evaluation.
func TestRealLokiGrafana(t *testing.T) {
	c := logsCase{realCase{dir: "grafana-13.3.1-loki", series: `GRAFANA_ALERTS{alertstate="firing",grafana_alertstate!="error"}`,
		from: time.Date(2026, 11, 11, 19, 35, 0, 0, time.UTC), to: realTo(time.Date(2026, 11, 11, 20, 15, 0, 0, time.UTC)), step: 10 * time.Second}}
	gf := os.Getenv("LAPILLI_RECORD_GRAFANA")
	auth := map[string]string{}
	var token string
	if f := os.Getenv("LAPILLI_RECORD_GRAFANA_TOKEN"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		token = strings.TrimSpace(string(b))
		auth["Authorization"] = "Bearer " + token
	}
	rs, err := rules.Parse(c.kept(t, "rules.yaml", gf+"/api/v1/provisioning/alert-rules/export?format=yaml", auth))
	if err != nil {
		t.Fatal(err)
	}
	// The types of the data sources, as Grafana said them.
	kinds := map[string]string{}
	if os.Getenv("LAPILLI_RECORD") != "" {
		for _, uid := range []string{"loki", "prom", "orders"} {
			var d struct{ Type string }
			if err := json.Unmarshal(c.kept(t, "ds-"+uid+".json", gf+"/api/datasources/uid/"+uid, auth), &d); err != nil {
				t.Fatal(err)
			}
			kinds[uid] = d.Type
		}
		b, _ := json.Marshal(kinds)
		if err := os.WriteFile(c.path("datasources.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
		for _, uid := range []string{"loki", "prom", "orders"} {
			os.Remove(c.path("ds-" + uid + ".json"))
		}
	} else if err := json.Unmarshal(c.kept(t, "datasources.json", "", nil), &kinds); err != nil {
		t.Fatal(err)
	}
	rules.Replaying{Kinds: kinds}.Apply(rs)
	rec, real := c.recorded(t)
	var opts []prom.Option
	if token != "" {
		opts = append(opts, prom.WithToken(func(context.Context) (string, error) { return token, nil }))
	}
	logs := c.logs(t, prom.New(gf+"/api/datasources/proxy/uid/loki", "lapilli-test", append(opts, prom.Loki())...))
	h := WithLogs(rec, func(r rules.Rule) (History, error) {
		if r.Logs != "loki" {
			t.Errorf("%s asks %q", r.Name, r.Logs)
		}
		return logs, nil
	})
	ran, fs := Run(context.Background(), h, []Version{{Rules: rs}}, c.from, c.to, c.step, 4)
	c.save(t, rec)
	c.saveLogs(t, logs)
	listed := map[string]string{
		"LkClassic": "classic conditions over a Loki query", "LkMixed": "Prometheus and Loki together", "LkSQL": "a mysql data source",
	}
	for _, r := range ran {
		want := listed[r.Name]
		if want == "" && (r.Unsupported != "" || r.Logs != "loki") || want != "" && !strings.Contains(r.Unsupported, want) {
			t.Errorf("%s: %q, logs %q", r.Name, r.Unsupported, r.Logs)
		}
	}
	real = slices.DeleteFunc(real, func(f firing.Firing) bool { return listed[f.Rule] != "" })
	if diff := sameFirings(fs, real, c.from, 0); len(diff) > 0 {
		t.Errorf("%d differ:\n%s", len(diff), strings.Join(diff, "\n"))
	}
	if len(fs) < 80 {
		t.Errorf("only %d firings: the recording is too short to say much", len(fs))
	}
	check := verdict.CheckReplay(context.Background(), ran, fs, real, c.from, c.to, c.step)
	if check.Alike != check.Replayed || check.Alike != check.Real {
		t.Errorf("check: %d replayed, %d real, %d alike", check.Replayed, check.Real, check.Alike)
	}
}
