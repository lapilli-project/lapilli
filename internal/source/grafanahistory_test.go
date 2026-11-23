package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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
	// byUID is the id of each rule's uid, for alertUID; renamed, the title
	// an answer of one rule alone gives a rule of an id.
	byUID   map[string]int64
	renamed map[int64]string
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
		uid := q.Get("alertUID")
		for _, a := range g.notes {
			if uid != "" && (g.byUID[uid] == 0 || a.AlertID != g.byUID[uid]) {
				continue
			}
			if title, ok := g.renamed[a.AlertID]; ok && uid != "" {
				a.AlertName = title
			}
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

// grafanaTags are labels as Grafana 12.4 writes them as an annotation's
// tags, read from its source (pkg/services/ngalert/state/historian,
// convertLabelsToTags; pkg/services/tag, ParseTagPairs and JoinTagPairs):
// sorted by name, each colon an underscore, those whose name or value is
// then past 100 characters left out, and those past tags_length (500 by
// default) as JSON; stored with spaces either end trimmed, one with no
// value as its name alone, and none with no name.
func grafanaTags(labels map[string]string, most int) []string {
	keys := slices.Sorted(maps.Keys(labels))
	var out []string
	length := 2
	for _, k := range keys {
		sk, sv := strings.ReplaceAll(k, ":", "_"), strings.ReplaceAll(labels[k], ":", "_")
		if utf8.RuneCountInString(sk) > 100 || utf8.RuneCountInString(sv) > 100 {
			continue
		}
		tag := sk + ":" + sv
		b, _ := json.Marshal(tag)
		n := len(b)
		if len(out) > 0 {
			n++
		}
		if most > 0 && length+n > most {
			continue
		}
		length += n
		sk, sv = strings.Trim(sk, " "), strings.Trim(sv, " ")
		switch {
		case sk == "":
		case sv == "":
			out = append(out, sk)
		default:
			out = append(out, sk+":"+sv)
		}
	}
	return out
}

// note is an alert annotation as Grafana 12.4 writes one, its labels as
// tags.
func note(id int64, at time.Time, rule, from, to string, labels ...string) annotation {
	l := map[string]string{"alertname": rule}
	for i := 0; i+1 < len(labels); i += 2 {
		l[labels[i]] = labels[i+1]
	}
	return annotation{ID: id, AlertName: rule, PrevState: from, NewState: to, Time: at.UnixMilli(), Tags: grafanaTags(l, 500)}
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

func transitions(t *testing.T, ts []firing.Transition, from time.Time) []firing.Firing {
	t.Helper()
	fs, unsure := firing.FromTransitions(ts, from)
	if len(unsure) > 0 {
		t.Errorf("unsure: %v", unsure)
	}
	return fs
}

// Where Grafana keeps its state history is read from its frontend
// settings: annotations from 12.4 on, which hold an alert's labels, or
// Loki, alone or first of several; with several, or in Prometheus,
// GRAFANA_ALERTS may be kept too.
func TestGrafanaHistoryWhere(t *testing.T) {
	g := &fakeGrafana{}
	h, _ := g.start(t)
	for settings, want := range map[string]HistoryWhere{
		`{"buildInfo":{"version":"12.4.14"},"unifiedAlerting":{"alertStateHistoryBackend":"annotations"}}`:                                          {Serves: HistoryAnnotations},
		`{"buildInfo":{"version":"13.3.1+security-01"},"unifiedAlerting":{"alertStateHistoryBackend":"annotations"}}`:                               {Serves: HistoryAnnotations},
		`{"buildInfo":{"version":"12.4.0-pre"},"unifiedAlerting":{"alertStateHistoryBackend":"multiple","alertStateHistoryPrimary":"annotations"}}`: {Serves: HistoryAnnotations, Series: true},
		`{"buildInfo":{"version":"12.3.2"},"unifiedAlerting":{"alertStateHistoryBackend":"annotations"}}`:                                           {},
		`{"buildInfo":{"version":"11.6.16"},"unifiedAlerting":{"alertStateHistoryBackend":"multiple","alertStateHistoryPrimary":"annotations"}}`:    {Series: true},
		`{"buildInfo":{"version":"11.6.16"},"unifiedAlerting":{"alertStateHistoryBackend":"loki"}}`:                                                 {Serves: HistoryLoki},
		`{"unifiedAlerting":{"alertStateHistoryBackend":"multiple","alertStateHistoryPrimary":"loki"}}`:                                             {Serves: HistoryLoki, Series: true},
		`{"unifiedAlerting":{"alertStateHistoryBackend":"prometheus"}}`:                                                                             {Series: true},
		`{"unifiedAlerting":{"alertStateHistoryBackend":"multiple","alertStateHistoryPrimary":"https://x.example"}}`:                                {Series: true},
		`{"unifiedAlerting":{"alertStateHistoryBackend":"annotations"}}`:                                                                            {Serves: HistoryAnnotations},
		`{"unifiedAlerting":{}}`: {},
		`{}`:                     {},
	} {
		g.settings = settings
		if got, err := h.Where(context.Background()); err != nil || got != want {
			t.Errorf("%s: %+v %v, want %+v", settings, got, err, want)
		}
	}
	g.settings = `not json`
	if _, err := h.Where(context.Background()); !errors.Is(err, ErrUnusable) {
		t.Errorf("settings that aren't JSON: %v", err)
	}
	if g.auth[0] != "Bearer glsa_viewer" {
		t.Errorf("sent %q", g.auth[0])
	}
	// Refused is of no use, and won't be asked again; no answer is worth
	// asking again.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusForbidden) }))
	defer srv.Close()
	if _, err := (GrafanaHistory{URL: srv.URL}).Where(context.Background()); !errors.Is(err, ErrUnusable) {
		t.Errorf("403: %v", err)
	}
	if _, err := (GrafanaHistory{URL: "http://127.0.0.1:1"}).Where(context.Background()); err == nil || errors.Is(err, ErrUnusable) {
		t.Errorf("no answer: %v", err)
	}
}

// Labels as Grafana 12.4 keeps them in an annotation's tags, and as the
// replay's are set beside them: a colon an underscore, as an instance's
// port is; spaces trimmed, and a value of spaces none, a tag of its name
// alone; one past 100 characters not there at all.
func TestAnnotationLabels(t *testing.T) {
	long := strings.Repeat("x", 101)
	labels := map[string]string{"alertname": "Up", "instance": "10.0.0.1:9100", "pad": "  ", "note": " a b ", "long": long, "a:b": "c", "ok": strings.Repeat("y", 100)}
	g := &fakeGrafana{notes: []annotation{
		{ID: 1, AlertName: "Up", PrevState: "Normal", NewState: "Alerting", Time: min_(1).UnixMilli(), Tags: grafanaTags(labels, 0)},
	}}
	h, _ := g.start(t)
	ts, read, err := h.Read(context.Background(), HistoryAnnotations, min_(0), min_(10))
	if err != nil || read.Passed != 0 || len(ts) != 1 {
		t.Fatalf("%+v %+v %v", ts, read, err)
	}
	want := AnnotationLabels(labels)
	delete(want, "alertname")
	if !maps.Equal(ts[0].Labels, want) {
		t.Errorf("read %v\nwant %v", ts[0].Labels, want)
	}
	for k, v := range map[string]string{"instance": "10.0.0.1_9100", "pad": "", "note": "a b", "a_b": "c"} {
		if ts[0].Labels[k] != v {
			t.Errorf("%s: %q, want %q", k, ts[0].Labels[k], v)
		}
	}
	if _, ok := ts[0].Labels["long"]; ok {
		t.Error("a label past 100 characters kept")
	}
	if AnnotationName(" Disk: full ") != "Disk_ full" || AnnotationLabels(nil) != nil {
		t.Error("AnnotationName")
	}
}

// Annotations with no tags, as Grafana before 12.4 writes them, say
// nothing of which alert each is of: two pods would be one. They can't be
// used at all.
func TestGrafanaHistoryBefore124(t *testing.T) {
	g := &fakeGrafana{notes: []annotation{
		{ID: 2, AlertName: "Up", PrevState: "Normal", NewState: "Alerting", Time: min_(1).UnixMilli()},
		{ID: 1, AlertName: "Up", PrevState: "Normal", NewState: "Alerting", Time: min_(1).UnixMilli()},
	}}
	h, _ := g.start(t)
	if _, _, err := h.Read(context.Background(), HistoryAnnotations, min_(0), min_(10)); !errors.Is(err, ErrUnusable) {
		t.Errorf("%v", err)
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
	if got := firingsOf(transitions(t, ts, min_(0))); got != want {
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
// the most a page asks for can't be read a page at a time: of no use, not
// read in part, and not worth asking again.
func TestGrafanaHistoryAnnotationsOneTime(t *testing.T) {
	defer func(n, m int) { historyPage, historyGrow[HistoryAnnotations] = n, m }(historyPage, historyGrow[HistoryAnnotations])
	historyPage, historyGrow[HistoryAnnotations] = 2, 4
	g := &fakeGrafana{notes: []annotation{note(9, min_(1), "Up", "Normal", "Alerting", "pod", "x")}}
	for i := range 3 {
		g.notes = append(g.notes, note(int64(i+1), min_(4), "Up", "Normal", "Alerting", "pod", fmt.Sprint(i)))
	}
	h, _ := g.start(t)
	if ts, _, err := h.Read(context.Background(), HistoryAnnotations, min_(0), min_(10)); err != nil || len(ts) != 4 {
		t.Errorf("%d read, %v", len(ts), err)
	}
	g.notes = append(g.notes, note(4, min_(4), "Up", "Normal", "Alerting", "pod", "3"))
	if _, _, err := h.Read(context.Background(), HistoryAnnotations, min_(0), min_(10)); !errors.Is(err, ErrUnusable) || errors.Is(err, ErrTooManyChanges) ||
		!strings.Contains(err.Error(), "more than 4 changes of state at one time") {
		t.Errorf("%v", err)
	}
}

// What another system sends is bounded: a change past the bounds goes,
// with every change of its alert, is counted, and its rule is unsure; past
// MaxChanges, the read fails as too many, which lapilli serve tells apart.
func TestGrafanaHistoryBounds(t *testing.T) {
	long := strings.Repeat("x", maxHistoryValue+1)
	longName := strings.Repeat("n", maxHistoryName+1)
	many := []string{"alertname:Many"}
	for i := range maxHistoryLabels {
		many = append(many, fmt.Sprintf("l%d:v", i))
	}
	tags := func(tt ...string) []string { return tt }
	at := min_(1).UnixMilli()
	g := &fakeGrafana{notes: []annotation{
		{ID: 1, AlertName: "Long", PrevState: "Normal", NewState: "Alerting", Time: at, Tags: tags("alertname:Long", "pod:"+long)},
		{ID: 2, AlertName: "Long", PrevState: "Alerting", NewState: "Normal", Time: min_(2).UnixMilli(), Tags: tags("alertname:Long", "pod:"+long)},
		{ID: 3, AlertName: "Many", PrevState: "Normal", NewState: "Alerting", Time: at, Tags: many},
		{ID: 4, AlertName: "LongName", PrevState: "Normal", NewState: "Alerting", Time: at, Tags: tags("alertname:LongName", longName+":v")},
		// A state Grafana has no name for, and one too long: their alerts'
		// other changes go too, which alone would say they fired from the
		// start.
		note(5, min_(1), "Odd", "Normal", "Firing"),
		note(6, min_(2), "Odd", "Alerting", "Normal"),
		note(7, min_(1), "Wordy", "Normal", "Alerting ("+strings.Repeat("r", maxHistoryState)+")"),
		note(8, min_(2), "Wordy", "Alerting", "Normal"),
		{ID: 9, AlertName: "Twice", PrevState: "Normal", NewState: "Alerting", Time: at, Tags: tags("alertname:Twice", "a:1", "a:2")},
		{ID: 10, AlertName: strings.Repeat("r", maxHistoryRule+1), PrevState: "Normal", NewState: "Alerting", Time: at, Tags: tags("x:1")},
		note(11, min_(3), "Fine", "Normal", "Alerting"),
	}}
	h, _ := g.start(t)
	ts, read, err := h.Read(context.Background(), HistoryAnnotations, min_(0), min_(10))
	if err != nil {
		t.Fatal(err)
	}
	if got := firingsOf(transitions(t, ts, min_(0))); got != "Fine map[] 07:03-on" {
		t.Errorf("kept %s", got)
	}
	if read.Passed != 10 {
		t.Errorf("passed over %d, want 10", read.Passed)
	}
	if strings.Join(read.Unsure, ",") != "Long,LongName,Many,Odd,Twice,Wordy" || read.UnsureMore {
		t.Errorf("unsure: %v %v", read.Unsure, read.UnsureMore)
	}

	defer func(n int) { MaxChanges = n }(MaxChanges)
	MaxChanges = 1
	if _, _, err := h.Read(context.Background(), HistoryAnnotations, min_(0), min_(10)); !errors.Is(err, ErrTooManyChanges) || !errors.Is(err, ErrUnusable) {
		t.Errorf("past MaxChanges: %v", err)
	}
	MaxChanges = 200000
	// Pages each with a change not seen, or asked again and again, end
	// past what is read of a history's answers, all told; or past the
	// requests a read asks.
	defer func(n int64, m, r int) { historyBytes, historyPage, historyRequests = n, m, r }(historyBytes, historyPage, historyRequests)
	historyBytes, historyPage = 3000, 2
	if _, _, err := h.Read(context.Background(), HistoryAnnotations, min_(0), min_(10)); !errors.Is(err, ErrTooManyChanges) {
		t.Errorf("past what is read all told: %v", err)
	}
	historyBytes, historyRequests = 1<<30, 3
	if _, _, err := h.Read(context.Background(), HistoryAnnotations, min_(0), min_(10)); !errors.Is(err, ErrTooManyChanges) || !strings.Contains(err.Error(), "requests") {
		t.Errorf("past the requests: %v", err)
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
	if err != nil || len(ts) != 1 || ts[0].Rule != "Fine" || read.Passed != 1 || strings.Join(read.Unsure, ",") != "Old" {
		t.Errorf("%+v %+v %v", ts, read, err)
	}
}

// A read is asked a week at a time at most, within Grafana's Loki's 721h.
func TestGrafanaHistorySpans(t *testing.T) {
	for _, where := range []string{HistoryAnnotations, HistoryLoki} {
		g := &fakeGrafana{}
		h, _ := g.start(t)
		if _, _, err := h.Read(context.Background(), where, min_(0), min_(0).Add(17*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if len(g.asked) != 3 {
			t.Errorf("%s: asked %v", where, g.asked)
		}
		for _, a := range g.asked {
			u, _ := url.Parse(a)
			from, _ := strconv.ParseInt(u.Query().Get("from"), 10, 64)
			to, _ := strconv.ParseInt(u.Query().Get("to"), 10, 64)
			span := time.Duration(to-from) * time.Second
			if where == HistoryAnnotations {
				span /= 1000
			}
			if span > 7*24*time.Hour+time.Second {
				t.Errorf("%s: asked %s, over %s", where, a, span)
			}
		}
	}
}

// Whether the history holds a change from before a time: one at or before
// it, the newest, asked with both ends, as Grafana filters by time only
// then.
func TestGrafanaHistoryReaches(t *testing.T) {
	g := &fakeGrafana{notes: []annotation{note(1, min_(5), "Up", "Normal", "Alerting")},
		lines: []fakeLine{{min_(5).UnixMilli(), "a", map[string]any{"previous": "Normal", "current": "Alerting", "labels": map[string]string{"alertname": "Up"}}}}}
	h, _ := g.start(t)
	for _, where := range []string{HistoryAnnotations, HistoryLoki} {
		for at, want := range map[time.Time]bool{min_(4): false, min_(5): true, min_(60): true} {
			if got, err := h.Reaches(context.Background(), where, at); err != nil || got != want {
				t.Errorf("%s at %s: %v %v", where, at.Format("15:04"), got, err)
			}
		}
	}
	if !strings.Contains(g.asked[0], "from=1&") {
		t.Errorf("asked %s", g.asked[0])
	}
}

// An answer longer than is read asks again for a page half the size,
// down to ten changes; a token goes nowhere it wasn't sent, a redirect not
// followed.
func TestGrafanaHistoryLongAndRedirect(t *testing.T) {
	defer func(n int64) { historyBody = n }(historyBody)
	historyBody = 2500
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
	// Longer than that at ten changes: of no use.
	historyBody = 100
	if _, _, err := h.Read(context.Background(), HistoryAnnotations, min_(0), min_(10)); !errors.Is(err, ErrUnusable) {
		t.Errorf("too long at ten: %v", err)
	}
	historyBody = 2500

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
// the page before, those seen told apart; a line that can't be read
// counted once, however often it is read again.
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
		line(min_(1), "b", "Up", "Normal", "Alerting", map[string]string{"pod": "a", "instance": "10.0.0.1:9100"}),
		line(min_(3), "a", "Down", "Normal", "Alerting (NoData)", nil),
		line(min_(4), "b", "Up", "Alerting", "Recovering", map[string]string{"pod": "a", "instance": "10.0.0.1:9100"}),
		line(min_(4).Add(300*time.Millisecond), "b", "Up", "Normal", "NoData", map[string]string{"ref_id": "A"}),
		line(min_(4).Add(600*time.Millisecond), "a", "Down", "Alerting (NoData)", "Normal", nil),
		line(min_(5), "b", "Up", "Recovering", "Normal", map[string]string{"pod": "a", "instance": "10.0.0.1:9100"}),
		line(min_(7), "a", "Up", "NoData", "Normal", map[string]string{"ref_id": "A"}),
	}}
	h, _ := g.start(t)
	ts, read, err := h.Read(context.Background(), HistoryLoki, min_(0), min_(10))
	if err != nil || read.Passed != 0 || len(ts) != 7 {
		t.Fatalf("%d changes, %+v, %v", len(ts), read, err)
	}
	want := strings.Join([]string{
		"Up map[instance:10.0.0.1:9100 pod:a] 07:01-07:05",
		"Down map[] 07:03-07:04",
		"Up map[grafana_alertstate:nodata ref_id:A] 07:04-07:07",
	}, "\n")
	if got := firingsOf(transitions(t, ts, min_(0))); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	for _, a := range g.asked {
		if !strings.HasPrefix(a, "/api/v1/rules/history?") || strings.Contains(a, "ruleUID") {
			t.Errorf("asked %s", a)
		}
	}
	// A line that can't be read, at one time with others, across pages.
	g.lines[3].line = map[string]any{"previous": 5}
	g.lines[4].ms = g.lines[3].ms
	if _, read, err := h.Read(context.Background(), HistoryLoki, min_(0), min_(10)); err != nil || read.Passed != 1 {
		t.Errorf("a line that isn't one: %+v %v", read, err)
	}
}

// An answer that isn't Grafana's state history kept in Loki isn't taken,
// and isn't worth asking again.
func TestGrafanaHistoryLokiNotAFrame(t *testing.T) {
	for _, body := range []string{`{}`, `{"schema":{"fields":[{"name":"time"},{"name":"line"}]},"data":{"values":[[1,2],[{}]]}}`,
		`{"schema":{"fields":[{"name":"time"},{"name":"text"}]},"data":{"values":[[1],["x"]]}}`, `[]`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		if _, _, err := (GrafanaHistory{URL: srv.URL}).Read(context.Background(), HistoryLoki, min_(0), min_(10)); !errors.Is(err, ErrUnusable) {
			t.Errorf("%s: %v", body, err)
		}
		srv.Close()
	}
	if _, _, err := (GrafanaHistory{URL: "http://127.0.0.1:1"}).Read(context.Background(), "prometheus", min_(0), min_(10)); !errors.Is(err, ErrUnusable) {
		t.Error("read from where it can't be")
	}
}

// Kept in Loki, labels come as they are, as JSON: bounded as tags are.
func TestGrafanaHistoryLokiBounds(t *testing.T) {
	line := func(at time.Time, rule string, labels map[string]any) fakeLine {
		labels["alertname"] = rule
		return fakeLine{at.UnixMilli(), "a", map[string]any{"previous": "Normal", "current": "Alerting", "ruleTitle": rule, "fingerprint": rule, "labels": labels}}
	}
	many := map[string]any{}
	for i := range maxHistoryLabels {
		many[fmt.Sprint("l", i)] = "v"
	}
	g := &fakeGrafana{lines: []fakeLine{
		line(min_(1), "LongName", map[string]any{strings.Repeat("n", maxHistoryName+1): "v"}),
		line(min_(1), "LongValue", map[string]any{"pod": strings.Repeat("v", maxHistoryValue+1)}),
		line(min_(1), "Many", many),
		line(min_(2), "Fine", map[string]any{"pod": "a"}),
	}}
	h, _ := g.start(t)
	ts, read, err := h.Read(context.Background(), HistoryLoki, min_(0), min_(10))
	if err != nil || len(ts) != 1 || read.Passed != 3 || strings.Join(read.Unsure, ",") != "LongName,LongValue,Many" {
		t.Errorf("%d kept, %+v, %v", len(ts), read, err)
	}
}

// Refused for good (403) is of no use; a token not let in (401), expired
// or not yet rotated in, is worth asking again, as a later ask may have it
// right.
func TestGrafanaHistoryRefused(t *testing.T) {
	for status, unusable := range map[int]bool{http.StatusForbidden: true, http.StatusUnauthorized: false, http.StatusNotFound: true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", status) }))
		h := GrafanaHistory{URL: srv.URL}
		if _, _, err := h.Read(context.Background(), HistoryAnnotations, min_(0), min_(10)); err == nil || errors.Is(err, ErrUnusable) != unusable {
			t.Errorf("read, %d: %v", status, err)
		}
		if _, err := h.Reaches(context.Background(), HistoryLoki, min_(0)); err == nil || errors.Is(err, ErrUnusable) != unusable {
			t.Errorf("reaches, %d: %v", status, err)
		}
		srv.Close()
	}
}

// Past the alerts a read keeps, however small their labels, it fails as
// too many.
func TestGrafanaHistoryAlertsBound(t *testing.T) {
	defer func(n int) { historyAlerts = n }(historyAlerts)
	historyAlerts = 3
	g := &fakeGrafana{}
	for i := range 4 {
		g.notes = append(g.notes, note(int64(i+1), min_(i), "Up", "Normal", "Alerting", "pod", fmt.Sprint(i)))
	}
	h, _ := g.start(t)
	if _, _, err := h.Read(context.Background(), HistoryAnnotations, min_(0), min_(10)); !errors.Is(err, ErrTooManyChanges) || !strings.Contains(err.Error(), "alerts") {
		t.Errorf("%v", err)
	}
	g.notes = g.notes[:3]
	if ts, _, err := h.Read(context.Background(), HistoryAnnotations, min_(0), min_(10)); err != nil || len(ts) != 3 {
		t.Errorf("%d, %v", len(ts), err)
	}
}

// A page of changes at one time alone grows to 16,000 from annotations,
// and to 5,000 from Loki, its max_entries_limit_per_query by default,
// which Loki refuses past.
func TestGrafanaHistoryGrowth(t *testing.T) {
	for where, most := range map[string]int{HistoryAnnotations: 16000, HistoryLoki: 5000} {
		asked := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			asked = max(asked, n)
			var b strings.Builder
			if where == HistoryLoki {
				times, lines := make([]string, n), make([]string, n)
				for i := range n {
					times[i], lines[i] = fmt.Sprint(min_(5).UnixMilli()), fmt.Sprintf(`{"previous":"Normal","current":"Alerting","fingerprint":"%d","labels":{"alertname":"Up","pod":"%d"}}`, i, i)
				}
				fmt.Fprintf(&b, `{"schema":{"fields":[{"name":"time"},{"name":"line"}]},"data":{"values":[[%s],[%s]]}}`, strings.Join(times, ","), strings.Join(lines, ","))
			} else {
				b.WriteByte('[')
				for i := range n {
					if i > 0 {
						b.WriteByte(',')
					}
					fmt.Fprintf(&b, `{"id":%d,"time":%d,"prevState":"Normal","newState":"Alerting","tags":["alertname:Up","pod:%d"]}`, i+1, min_(5).UnixMilli(), i)
				}
				b.WriteByte(']')
			}
			io.WriteString(w, b.String())
		}))
		_, _, err := GrafanaHistory{URL: srv.URL}.Read(context.Background(), where, min_(0), min_(10))
		srv.Close()
		if !errors.Is(err, ErrUnusable) || asked != most {
			t.Errorf("%s: asked %d at most, want %d; %v", where, asked, most, err)
		}
	}
}

// Past a thousand rules unsure, a read says so, and names a thousand.
func TestGrafanaHistoryUnsureMore(t *testing.T) {
	g := &fakeGrafana{}
	for i := range maxUnsure + 1 {
		g.notes = append(g.notes, note(int64(i+1), min_(1), fmt.Sprint("R", i), "Normal", "Firing"))
	}
	h, _ := g.start(t)
	_, read, err := h.Read(context.Background(), HistoryAnnotations, min_(0), min_(10))
	if err != nil || !read.UnsureMore || len(read.Unsure) != maxUnsure {
		t.Errorf("%d unsure, more %v, %v", len(read.Unsure), read.UnsureMore, err)
	}
}
