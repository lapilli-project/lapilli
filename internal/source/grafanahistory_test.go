package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
)

// fakeGrafana answers as Grafana 12.4 and 13.3 were measured to
// (docs/design/grafanahistory.md): its frontend settings; its alert
// annotations, newest first, from and to in milliseconds and both within,
// limit 100 unless asked; and its state history kept in Loki, newest
// first across streams, each stream's together, from and to in seconds.
type fakeGrafana struct {
	mu       sync.Mutex
	settings string
	notes    []annotation
	lines    []fakeLine
	asked    []string
	auth     []string
	// stream is a label each line's stream has, for Loki's answers.
}

type fakeLine struct {
	ms     int64
	stream string
	line   map[string]any
}

func (g *fakeGrafana) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked = append(g.asked, r.URL.RequestURI())
	g.auth = append(g.auth, r.Header.Get("Authorization"))
	q := r.URL.Query()
	num := func(k string, or int64) int64 {
		if n, err := strconv.ParseInt(q.Get(k), 10, 64); err == nil {
			return n
		}
		return or
	}
	switch r.URL.Path {
	case "/api/frontend/settings":
		fmt.Fprint(w, g.settings)
	case "/api/annotations":
		from, to, limit := num("from", 0), num("to", 1<<62), num("limit", 100)
		if limit <= 0 {
			limit = 100
		}
		var out []annotation
		for _, a := range g.notes {
			if a.Time >= from && a.Time <= to {
				out = append(out, a)
			}
		}
		slices.SortStableFunc(out, func(a, b annotation) int {
			if a.Time != b.Time {
				return int(b.Time - a.Time)
			}
			return int(b.ID - a.ID)
		})
		json.NewEncoder(w).Encode(out[:min(len(out), int(limit))])
	case "/api/v1/rules/history":
		from, to, limit := num("from", 0)*1000, num("to", 1<<40)*1000, num("limit", 0)
		var in []fakeLine
		for _, l := range g.lines {
			if l.ms >= from && l.ms < to {
				in = append(in, l)
			}
		}
		slices.SortStableFunc(in, func(a, b fakeLine) int { return int(b.ms - a.ms) })
		if limit > 0 {
			in = in[:min(len(in), int(limit))]
		}
		// Each stream's lines together, newest first in each, as Grafana
		// joins Loki's streams.
		slices.SortStableFunc(in, func(a, b fakeLine) int { return strings.Compare(a.stream, b.stream) })
		times, lines, labels := []int64{}, []any{}, []any{}
		for _, l := range in {
			times = append(times, l.ms)
			lines = append(lines, l.line)
			labels = append(labels, map[string]string{"from": "state-history", "group": l.stream})
		}
		json.NewEncoder(w).Encode(map[string]any{
			"schema": map[string]any{"name": "states", "fields": []map[string]any{{"name": "time", "type": "time"}, {"name": "line", "type": "other"}, {"name": "labels", "type": "other"}}},
			"data":   map[string]any{"values": []any{times, lines, labels}},
		})
	default:
		http.NotFound(w, r)
	}
}

func (g *fakeGrafana) start(t *testing.T) (GrafanaHistory, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("glsa_viewer\n"), 0o600)
	return GrafanaHistory{URL: srv.URL, TokenFile: tok, Agent: "test"}, srv
}

// note is an alert annotation as Grafana writes one, its labels as tags.
func note(id int64, at time.Time, rule, from, to string, labels ...string) annotation {
	tags := []string{"alertname:" + rule}
	for i := 0; i+1 < len(labels); i += 2 {
		tags = append(tags, labels[i]+":"+labels[i+1])
	}
	return annotation{ID: id, AlertName: rule, PrevState: from, NewState: to, Time: at.UnixMilli(), Tags: tags}
}

var h0 = time.Date(2026, 11, 16, 7, 0, 0, 0, time.UTC)

func min_(m int) time.Time { return h0.Add(time.Duration(m) * time.Minute) }

func firingsOf(fs []firing.Firing) string {
	var out []string
	for _, f := range fs {
		end := "on"
		if !f.End.IsZero() {
			end = f.End.Format("15:04")
		}
		out = append(out, fmt.Sprintf("%s %v %s-%s", f.Rule, f.Labels, f.Start.Format("15:04"), end))
	}
	return strings.Join(out, "\n")
}

// Where Grafana keeps its state history is read from its frontend
// settings: annotations or Loki, alone or first of several; nothing it
// serves otherwise.
func TestGrafanaHistoryWhere(t *testing.T) {
	g := &fakeGrafana{}
	h, _ := g.start(t)
	for settings, want := range map[string]string{
		`{"unifiedAlerting":{"alertStateHistoryBackend":"annotations"}}`:                                             HistoryAnnotations,
		`{"unifiedAlerting":{"alertStateHistoryBackend":"multiple","alertStateHistoryPrimary":"annotations"}}`:       HistoryAnnotations,
		`{"unifiedAlerting":{"alertStateHistoryBackend":"loki"}}`:                                                    HistoryLoki,
		`{"unifiedAlerting":{"alertStateHistoryBackend":"multiple","alertStateHistoryPrimary":"loki"}}`:              HistoryLoki,
		`{"unifiedAlerting":{"alertStateHistoryBackend":"prometheus"}}`:                                              "",
		`{"unifiedAlerting":{"alertStateHistoryBackend":"multiple","alertStateHistoryPrimary":"https://x.example"}}`: "",
		`{"unifiedAlerting":{}}`: "",
		`{}`:                     "",
	} {
		g.settings = settings
		if got, err := h.Where(context.Background()); err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", settings, got, err, want)
		}
	}
	g.settings = `not json`
	if _, err := h.Where(context.Background()); err == nil {
		t.Error("settings that aren't JSON were taken")
	}
	if g.auth[0] != "Bearer glsa_viewer" {
		t.Errorf("sent %q", g.auth[0])
	}
}

// The annotations are read a page at a time, each to the time of the
// oldest of the page before, those at that time again told apart by id:
// none lost or read twice where a page ends among changes at one time.
func TestGrafanaHistoryAnnotations(t *testing.T) {
	defer func(n int) { historyPage = n }(historyPage)
	historyPage = 3
	g := &fakeGrafana{notes: []annotation{
		note(1, min_(1), "Up", "Normal", "Pending", "pod", "a"),
		note(2, min_(2), "Up", "Pending", "Alerting", "pod", "a"),
		// Three at one time, across a page's end.
		note(3, min_(4), "Up", "Alerting", "Normal", "pod", "a"),
		note(4, min_(4), "Up", "Normal", "Alerting", "pod", "b"),
		note(5, min_(4), "Up", "Normal", "NoData", "ref_id", "A"),
		note(6, min_(6), "Up", "NoData", "Normal", "ref_id", "A"),
		note(7, min_(7), "Disk {full}, a=b", "Normal", "Alerting", "mount", "/, x=y} z", "note", `a "b", c={d}`),
		// Another rule's error, left out.
		note(8, min_(8), "Errs", "Normal", "Error"),
	}}
	h, _ := g.start(t)
	ts, read, err := h.Read(context.Background(), HistoryAnnotations, min_(0), min_(10))
	if err != nil || read.Passed != 0 || len(ts) != 8 {
		t.Fatalf("%d changes, %+v, %v", len(ts), read, err)
	}
	want := strings.Join([]string{
		"Up map[pod:a] 07:02-07:04",
		"Up map[pod:b] 07:04-on",
		"Up map[grafana_alertstate:nodata ref_id:A] 07:04-07:06",
		"Disk {full}, a=b map[mount:/, x=y} z note:a \"b\", c={d}] 07:07-on",
	}, "\n")
	if got := firingsOf(firing.FromTransitions(ts, min_(0))); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	for _, a := range g.asked {
		if !strings.HasPrefix(a, "/api/annotations?") || !strings.Contains(a, "type=alert") || !strings.Contains(a, "limit=3") && !strings.Contains(a, "limit=6") {
			t.Errorf("asked %s", a)
		}
	}
	if len(g.asked) < 4 {
		t.Errorf("asked %d pages, want 4 or more", len(g.asked))
	}
}

// A page of changes at one time alone is asked again for more; more than
// the most a page asks for can't be read a page at a time: said, not read
// in part.
func TestGrafanaHistoryAnnotationsOneTime(t *testing.T) {
	defer func(n, m int) { historyPage, maxHistoryGrow = n, m }(historyPage, maxHistoryGrow)
	historyPage, maxHistoryGrow = 2, 4
	g := &fakeGrafana{notes: []annotation{note(9, min_(1), "Up", "Normal", "Alerting", "pod", "x")}}
	for i := range 3 {
		g.notes = append(g.notes, note(int64(i+1), min_(4), "Up", "Normal", "Alerting", "pod", fmt.Sprint(i)))
	}
	h, _ := g.start(t)
	if ts, _, err := h.Read(context.Background(), HistoryAnnotations, min_(0), min_(10)); err != nil || len(ts) != 4 {
		t.Errorf("%d read, %v", len(ts), err)
	}
	g.notes = append(g.notes, note(4, min_(4), "Up", "Normal", "Alerting", "pod", "3"))
	if _, _, err := h.Read(context.Background(), HistoryAnnotations, min_(0), min_(10)); err == nil || !strings.Contains(err.Error(), "more than 4 changes of state at one time") {
		t.Errorf("%v", err)
	}
}

// What another system sends is bounded: a change past the bounds goes,
// with every change of its alert, and is counted; past MaxChanges, the
// read fails as too many, which lapilli serve tells apart.
func TestGrafanaHistoryBounds(t *testing.T) {
	long := strings.Repeat("x", maxHistoryValue+1)
	many := []string{}
	for i := range maxHistoryLabels + 1 {
		many = append(many, fmt.Sprintf("l%d", i), "v")
	}
	g := &fakeGrafana{notes: []annotation{
		note(1, min_(1), "Long", "Normal", "Alerting", "pod", long),
		note(2, min_(2), "Long", "Alerting", "Normal", "pod", long),
		note(3, min_(1), "Many", "Normal", "Alerting", many...),
		// A state Grafana has no name for: its alert's other change goes
		// too, which alone would say it fired from the start.
		note(4, min_(1), "Odd", "Normal", "Firing"),
		note(5, min_(2), "Odd", "Alerting", "Normal"),
		// A time outside what was asked.
		note(6, min_(1), "Late", "Normal", "Alerting"),
		{ID: 7, AlertName: "NoColon", PrevState: "Normal", NewState: "Alerting", Time: min_(1).UnixMilli(), Tags: []string{"alertname:NoColon", "plain"}},
		{ID: 8, AlertName: "Twice", PrevState: "Normal", NewState: "Alerting", Time: min_(1).UnixMilli(), Tags: []string{"alertname:Twice", "a:1", "a:2"}},
		{ID: 9, AlertName: strings.Repeat("r", maxHistoryRule+1), PrevState: "Normal", NewState: "Alerting", Time: min_(1).UnixMilli()},
		note(10, min_(3), "Fine", "Normal", "Alerting"),
	}}
	// The fake answers what is asked from 07:00; Late is moved past it.
	g.notes[5].Time = min_(30).UnixMilli()
	h, srv := g.start(t)
	_ = srv
	ts, read, err := h.Read(context.Background(), HistoryAnnotations, min_(0), min_(10))
	if err != nil {
		t.Fatal(err)
	}
	if got := firingsOf(firing.FromTransitions(ts, min_(0))); got != "Fine map[] 07:03-on" {
		t.Errorf("kept %s", got)
	}
	if read.Passed != 8 {
		t.Errorf("passed over %d, want 8", read.Passed)
	}
	// Late, asked to 07:10, isn't answered by Grafana at all; one answered
	// outside what was asked is passed over.
	g.notes[5].Time = min_(5).UnixMilli()
	ts, read, err = h.Read(context.Background(), HistoryAnnotations, min_(6), min_(10))
	if err != nil || len(ts) != 0 || read.Passed != 0 {
		t.Errorf("%d kept, %+v, %v", len(ts), read, err)
	}

	defer func(n int) { MaxChanges = n }(MaxChanges)
	MaxChanges = 1
	if _, _, err := h.Read(context.Background(), HistoryAnnotations, min_(0), min_(10)); !errors.Is(err, ErrTooManyChanges) {
		t.Errorf("past MaxChanges: %v", err)
	}
	// Pages each with a change not seen, or asked again and again, end
	// past what is read of a history's answers, all told.
	MaxChanges = 200000
	defer func(n int64, m int) { historyBytes, historyPage = n, m }(historyBytes, historyPage)
	historyBytes, historyPage = 3000, 2
	if _, _, err := h.Read(context.Background(), HistoryAnnotations, min_(0), min_(10)); !errors.Is(err, ErrTooManyChanges) {
		t.Errorf("past what is read all told: %v", err)
	}
}

// A time outside what was asked, as a Grafana answering other than it
// was asked could send, is passed over.
func TestGrafanaHistoryOutside(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]annotation{note(1, min_(-90), "Old", "Normal", "Alerting"), note(2, min_(3), "Fine", "Normal", "Alerting")})
	}))
	defer srv.Close()
	ts, read, err := GrafanaHistory{URL: srv.URL}.Read(context.Background(), HistoryAnnotations, min_(0), min_(10))
	if err != nil || len(ts) != 1 || ts[0].Rule != "Fine" || read.Passed != 1 {
		t.Errorf("%+v %+v %v", ts, read, err)
	}
}

// An answer longer than is read asks again for a page half the size,
// down to ten changes; a token goes nowhere it wasn't sent, a redirect not
// followed.
func TestGrafanaHistoryLongAndRedirect(t *testing.T) {
	defer func(n int64) { historyBody = n }(historyBody)
	historyBody = 2000
	g := &fakeGrafana{}
	for i := range 40 {
		g.notes = append(g.notes, note(int64(i+1), min_(i/4), "Up", "Normal", "Alerting", "pod", fmt.Sprint(i)))
	}
	h, _ := g.start(t)
	ts, _, err := h.Read(context.Background(), HistoryAnnotations, min_(0), min_(10))
	if err != nil || len(ts) != 40 {
		t.Fatalf("%d, %v", len(ts), err)
	}
	if !strings.Contains(g.asked[0], "limit=1000") || !slices.ContainsFunc(g.asked, func(a string) bool { return strings.Contains(a, "limit=15") }) {
		t.Errorf("asked %v", g.asked)
	}

	hit := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer other.Close()
	moved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/api/annotations", http.StatusFound)
	}))
	defer moved.Close()
	h.URL = moved.URL
	if _, _, err := h.Read(context.Background(), HistoryAnnotations, min_(0), min_(10)); err == nil || hit {
		t.Errorf("a redirect followed: %v, hit %v", err, hit)
	}
	if _, err := h.Where(context.Background()); err == nil || hit {
		t.Errorf("a redirect followed: %v, hit %v", err, hit)
	}
}

// Kept in Loki, the changes are read from the state history API, newest
// first across streams, a page at a time to the second of the oldest of
// the page before, those seen told apart.
func TestGrafanaHistoryLoki(t *testing.T) {
	defer func(n int) { historyPage = n }(historyPage)
	historyPage = 3
	line := func(at time.Time, stream, rule, from, to string, labels map[string]string) fakeLine {
		l := map[string]any{"alertname": rule}
		for k, v := range labels {
			l[k] = v
		}
		fp := fmt.Sprint(len(rule), labels)
		return fakeLine{at.UnixMilli(), stream, map[string]any{"schemaVersion": 1, "previous": from, "current": to, "ruleTitle": rule, "ruleUID": strings.ToLower(rule),
			"fingerprint": fp, "labels": l}}
	}
	g := &fakeGrafana{lines: []fakeLine{
		line(min_(1), "b", "Up", "Normal", "Alerting", map[string]string{"pod": "a"}),
		line(min_(3), "a", "Down", "Normal", "Alerting (NoData)", nil),
		line(min_(4), "b", "Up", "Alerting", "Recovering", map[string]string{"pod": "a"}),
		line(min_(4).Add(300*time.Millisecond), "b", "Up", "Normal", "NoData", map[string]string{"ref_id": "A"}),
		line(min_(4).Add(600*time.Millisecond), "a", "Down", "Alerting (NoData)", "Normal", nil),
		line(min_(5), "b", "Up", "Recovering", "Normal", map[string]string{"pod": "a"}),
		line(min_(7), "a", "Up", "NoData", "Normal", map[string]string{"ref_id": "A"}),
	}}
	h, _ := g.start(t)
	ts, read, err := h.Read(context.Background(), HistoryLoki, min_(0), min_(10))
	if err != nil || read.Passed != 0 || len(ts) != 7 {
		t.Fatalf("%d changes, %+v, %v", len(ts), read, err)
	}
	want := strings.Join([]string{
		"Up map[pod:a] 07:01-07:05",
		"Down map[] 07:03-07:04",
		"Up map[grafana_alertstate:nodata ref_id:A] 07:04-07:07",
	}, "\n")
	if got := firingsOf(firing.FromTransitions(ts, min_(0))); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	for _, a := range g.asked {
		if !strings.HasPrefix(a, "/api/v1/rules/history?") || strings.Contains(a, "ruleUID") {
			t.Errorf("asked %s", a)
		}
	}
	g.lines[0].line = map[string]any{"previous": 5}
	if _, read, err := h.Read(context.Background(), HistoryLoki, min_(0), min_(10)); err != nil || read.Passed != 1 {
		t.Errorf("a line that isn't one: %+v %v", read, err)
	}
}

// An answer that isn't Grafana's state history kept in Loki isn't taken.
func TestGrafanaHistoryLokiNotAFrame(t *testing.T) {
	for _, body := range []string{`{}`, `{"schema":{"fields":[{"name":"time"},{"name":"line"}]},"data":{"values":[[1,2],[{}]]}}`,
		`{"schema":{"fields":[{"name":"time"},{"name":"text"}]},"data":{"values":[[1],["x"]]}}`, `[]`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		if _, _, err := (GrafanaHistory{URL: srv.URL}).Read(context.Background(), HistoryLoki, min_(0), min_(10)); err == nil {
			t.Errorf("%s: taken", body)
		}
		srv.Close()
	}
	if _, _, err := (GrafanaHistory{URL: "http://127.0.0.1:1"}).Read(context.Background(), "prometheus", min_(0), min_(10)); err == nil {
		t.Error("read from where it can't be")
	}
}
