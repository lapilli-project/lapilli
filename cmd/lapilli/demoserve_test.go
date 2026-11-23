package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// syncBuffer is a buffer the demo writes to while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// noRequestsOut fails a test for each request made through
// http.DefaultTransport, which every client without a transport of its own
// sends by: lapilli demo --serve makes none.
type noRequestsOut struct{ t *testing.T }

func (n noRequestsOut) RoundTrip(r *http.Request) (*http.Response, error) {
	n.t.Errorf("lapilli demo --serve sent a request: %s %s", r.Method, r.URL.Redacted())
	return nil, errors.New("lapilli demo --serve sends nothing")
}

// demoRun is lapilli demo --serve run until ended: its URL, the test's
// own client, and how it ended.
type demoRun struct {
	t      *testing.T
	tmp    string
	url    string
	client *http.Client
	out    *syncBuffer
	errs   *syncBuffer
	done   chan int
	// ended says the demo ended, its exit taken from done.
	ended bool
}

var demoURL = regexp.MustCompile(`\n  (http://127\.0\.0\.1:[0-9]+/)\n`)

// serveDemo starts lapilli demo --serve in the zone loc, TMPDIR a directory
// of the test's own, failing the test for a history asked for or a request
// sent through the default transport, and waits for its first round: its
// URL said, and the digest of the sample's week in its run history.
func serveDemo(t *testing.T, loc *time.Location) *demoRun {
	t.Helper()
	d := &demoRun{t: t, tmp: t.TempDir(), out: &syncBuffer{}, errs: &syncBuffer{}, done: make(chan int, 1),
		// The test's own transport: the default one fails.
		client: &http.Client{Transport: &http.Transport{}}}
	t.Setenv("TMPDIR", d.tmp)
	was := newHistory
	newHistory = func(context.Context, *verdictFlags) (history, error) {
		t.Error("lapilli demo --serve asked for a history")
		return fakeHistory{}, nil
	}
	dt := http.DefaultTransport
	http.DefaultTransport = noRequestsOut{t}
	t.Cleanup(func() { newHistory, http.DefaultTransport = was, dt })
	// A test that failed before ending it ends it, before its directory
	// is removed.
	t.Cleanup(func() {
		select {
		case <-d.done:
		default:
			if !d.ended {
				syscall.Kill(os.Getpid(), syscall.SIGINT)
				<-d.done
			}
		}
	})
	go func() { d.done <- run([]string{"demo", "--serve", "--tz", loc.String()}, d.out, d.errs) }()
	for d.url == "" {
		if m := demoURL.FindStringSubmatch(d.out.String()); m != nil {
			d.url = m[1]
			break
		}
		d.wait()
	}
	for {
		var runs []struct {
			Kind     string
			From, To time.Time
		}
		if code, body := d.get("api/v1/runs", nil); code == http.StatusOK {
			if err := json.Unmarshal([]byte(body), &runs); err != nil {
				t.Fatalf("api/v1/runs: %v\n%s", err, body)
			}
		}
		if len(runs) > 0 && runs[0].Kind == "digest" {
			if week := demoWeek(loc); !runs[0].From.Equal(week) || !runs[0].To.Equal(week.AddDate(0, 0, 7)) {
				t.Fatalf("the digest of %s → %s, not the sample's week", runs[0].From, runs[0].To)
			}
			return d
		}
		d.wait()
	}
}

// wait waits a moment, failing the test if the demo ended meanwhile.
func (d *demoRun) wait() {
	d.t.Helper()
	select {
	case code := <-d.done:
		d.ended = true
		d.t.Fatalf("lapilli demo --serve ended, exit %d:\n%s\n%s", code, d.out, d.errs)
	case <-time.After(10 * time.Millisecond):
	}
}

// get asks the demo for path, with header, and gives the status and body.
func (d *demoRun) get(path string, header http.Header) (int, string) {
	d.t.Helper()
	return d.do(http.MethodGet, path, header, nil)
}

func (d *demoRun) do(method, path string, header http.Header, body io.Reader) (int, string) {
	d.t.Helper()
	req, err := http.NewRequest(method, d.url+path, body)
	if err != nil {
		d.t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if h := header.Get("Host"); h != "" {
		req.Host = h
	}
	resp, err := d.client.Do(req)
	if err != nil {
		d.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		d.t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

// end sends the process sig, which the demo takes, and checks it ended as
// it should: exit 0, saying so, and nothing left in its temporary directory.
func (d *demoRun) end(sig syscall.Signal) {
	d.t.Helper()
	if err := syscall.Kill(os.Getpid(), sig); err != nil {
		d.t.Fatal(err)
	}
	code := <-d.done
	d.ended = true
	if code != 0 || !strings.HasSuffix(d.out.String(), "lapilli demo: ended; its ledger is removed.\n") {
		d.t.Errorf("ended by %v: exit %d\n%s\n%s", sig, code, d.out, d.errs)
	}
	if left, err := os.ReadDir(d.tmp); err != nil || len(left) > 0 {
		d.t.Errorf("ended by %v, it left %v behind (%v)", sig, left, err)
	}
}

// pageText is a page's text, its tags taken out and its spaces made one.
func pageText(body string) string {
	body = regexp.MustCompile(`(?s)<(script|style)[^>]*>.*?</(script|style)>`).ReplaceAllString(body, "")
	body = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(body, " ")
	return strings.Join(strings.Fields(html.UnescapeString(body)), " ")
}

// demoReport is what a report says of each rule and incident, as lapilli
// demo --json and /api/v1/report have it.
type demoReport struct {
	Rules []struct {
		Rule                           string
		Firings, White, Black, Refired int
		Noisy                          bool
		OffHours                       int `json:"off_hours"`
		OffHoursBlack                  int `json:"off_hours_black"`
		Asleep                         int
		AsleepBlack                    int `json:"asleep_black"`
		Episodes                       struct{ Total, White int }
		Incidents, First, Alone        int
		Spare                          bool
		Severity, Group                string
		Replayed                       bool
		NoRecord                       bool `json:"no_record"`
		Unjudged                       int
	} `json:"rules"`
	Incidents []struct {
		ID       string
		Start    time.Time
		CaughtBy []struct {
			Rule  string
			Delay int64 `json:"delay_ns"`
			Noisy bool
		} `json:"caught_by"`
	} `json:"incidents"`
}

func readReport(t *testing.T, what, body string) demoReport {
	t.Helper()
	var r demoReport
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("%s: %v\n%.400s", what, err, body)
	}
	if len(r.Rules) != 7 || len(r.Incidents) != 3 {
		t.Fatalf("%s: %d rules, %d incidents\n%.400s", what, len(r.Rules), len(r.Incidents), body)
	}
	// Each in an order of its own: set side by side by name.
	sort.Slice(r.Rules, func(i, j int) bool { return r.Rules[i].Rule < r.Rules[j].Rule })
	sort.Slice(r.Incidents, func(i, j int) bool { return r.Incidents[i].ID < r.Incidents[j].ID })
	return r
}

// TestDemoServe: lapilli demo --serve serves the sample's week, judged by
// serve's hourly job, with the numbers lapilli demo prints: in
// /api/v1/report, over the week and by default, and on the pages, which
// show it as the last whole week. It is read-only, answers no other host
// name, takes no write from another site, asks no history and sends no
// request; ended by SIGINT or SIGTERM, it removes its ledger.
func TestDemoServe(t *testing.T) {
	_, cli, _ := demo(t, "--json", "--tz", "UTC")
	want := readReport(t, "lapilli demo --json", cli)

	d := serveDemo(t, time.UTC)
	if !strings.HasPrefix(d.out.String(), "lapilli demo --serve: ") || d.errs.String() != "" {
		t.Errorf("said:\n%s\n%s", d.out, d.errs)
	}
	for _, q := range []string{"from=2026-11-09T00:00:00Z&to=2026-11-16T00:00:00Z", "firings=false"} {
		code, body := d.get("api/v1/report?"+q, nil)
		if code != http.StatusOK {
			t.Fatalf("api/v1/report?%s: %d %s", q, code, body)
		}
		if got := readReport(t, "api/v1/report?"+q, body); !reflect.DeepEqual(got, want) {
			t.Errorf("api/v1/report?%s:\n%+v\nlapilli demo:\n%+v", q, got, want)
		}
	}
	en := http.Header{"Accept-Language": {"en"}}
	for page, says := range map[string][]string{
		"": {"The numbers are the week of 2026-11-09, beside the week before.", "Precision 13% of firings caught something",
			"Firings 23", "Incidents missed 1 of 3", "Woken for nothing 8",
			`Quiet CheckoutLatencyP99 "for" 1m → 5m: fired 14 → 2, still catching 1`, "No alert caught INC-43 Orders stuck 2026-11-14 03:05"},
		"incidents": {"INC-43 Orders stuck 2026-11-14 03:05 2026-11-14 03:30 incidents file ✗ nothing fired nearly caught by OrdersQueueBacklog",
			"INC-42 Search timeouts 2026-11-12 21:15 2026-11-12 21:50 incidents file ○ SearchErrors +3m",
			"INC-41 Checkout errors 2026-11-10 12:01 2026-11-10 12:39 incidents file ○ CheckoutErrors +3m"},
		"rules/CheckoutLatencyP99": {"CheckoutLatencyP99", "1m → 5m"},
	} {
		code, body := d.get(page, en)
		text := pageText(body)
		if code != http.StatusOK {
			t.Errorf("/%s: %d %.300s", page, code, text)
			continue
		}
		for _, s := range says {
			if !strings.Contains(text, s) {
				t.Errorf("/%s doesn't say %q:\n%s", page, s, text)
			}
		}
	}
	// Read-only: no write is taken, through the UI or the API.
	for _, w := range []struct{ method, path, body string }{
		{http.MethodPost, "settings", "lead=30m"},
		{http.MethodPost, "api/v1/incidents", `{"id":"INC-99","title":"x","start":"2026-11-12T10:00:00Z"}`},
		{http.MethodPost, "incidents", "id=INC-99&title=x&start=2026-11-12T10:00"},
	} {
		h := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}
		if code, body := d.do(w.method, w.path, h, strings.NewReader(w.body)); code != http.StatusForbidden {
			t.Errorf("%s /%s: %d %s", w.method, w.path, code, body)
		}
	}
	// A page of another site's: its name pointed here (DNS rebinding) is
	// refused, and so is its form posted here.
	for _, host := range []string{"lapilli.example.com", "lapilli"} {
		if code, _ := d.get("", http.Header{"Host": {host}}); code != http.StatusMisdirectedRequest {
			t.Errorf("Host %s: %d", host, code)
		}
	}
	cross := http.Header{"Sec-Fetch-Site": {"cross-site"}, "Origin": {"https://lapilli.example.com"}, "Content-Type": {"application/x-www-form-urlencoded"}}
	if code, body := d.do(http.MethodPost, "settings", cross, strings.NewReader("lead=30m")); code != http.StatusForbidden || !strings.Contains(body, "cross-origin") {
		t.Errorf("a post from another site: %d %s", code, body)
	}
	d.end(syscall.SIGINT)

	// Ended by SIGTERM as by SIGINT; in another zone, its week laid there.
	seoul, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Fatal(err)
	}
	d = serveDemo(t, seoul)
	if _, body := d.get("", en); !strings.Contains(pageText(body), "The numbers are the week of 2026-11-09, beside the week before.") {
		t.Errorf("in Asia/Seoul:\n%s", pageText(body))
	}
	d.end(syscall.SIGTERM)
}

// TestDemoServeListen: lapilli demo --serve listens on loopback alone: an
// address that isn't one, a name, every interface, a port that isn't a
// number, are refused before anything is read or written; and --listen
// and --json are refused beside what they don't go with.
func TestDemoServeListen(t *testing.T) {
	for _, args := range [][]string{
		{"--serve", "--listen", "0.0.0.0:8080"}, {"--serve", "--listen", ":8080"}, {"--serve", "--listen", "[::]:8080"},
		{"--serve", "--listen", "192.0.2.1:8080"}, {"--serve", "--listen", "localhost:8080"}, {"--serve", "--listen=lapilli.example.com:80"},
		{"--serve", "--listen", "127.0.0.1"}, {"--serve", "--listen", "127.0.0.1:http"}, {"--serve", "--listen", "127.0.0.1:+80"},
		{"--serve", "--listen", "127.0.0.1:65536"}, {"--serve", "--listen", "[fe80::1%lo0]:8080"}, {"--serve", "--listen"},
		{"--serve", "--listen="}, {"--listen", "127.0.0.1:0"}, {"--serve=false", "--listen", "127.0.0.1:0"},
		{"--serve", "--json"}, {"--serve=yes"},
	} {
		if code, out, errs := demo(t, args...); code != 2 || out != "" || !strings.Contains(errs, `Run "lapilli demo -h"`) {
			t.Errorf("%q: exit %d, %q %q", args, code, out, errs)
		}
	}
	if _, _, errs := demo(t, "--serve", "--listen", "0.0.0.0:8080"); !strings.Contains(errs, "the demo listens on this machine alone") {
		t.Errorf("0.0.0.0: %s", errs)
	}
}

// TestDemoServeSendsNothing: lapilli demo --serve's serve is made with
// nothing to read but its own files, and nothing to send to: of the
// configuration its flags give, every field is as a Config with none
// given has it, but those that say where its files are, where it listens,
// how it judges and runs, and that it is read-only. A field added to serve
// with a default fails this until it is looked at here.
func TestDemoServeSendsNothing(t *testing.T) {
	dir := t.TempDir()
	st, err := readServe(demoServeArgs(dir, demoListen, "UTC", time.UTC), false)
	if err != nil {
		t.Fatal(err)
	}
	if st.f.promURL != demoPrometheus || st.f.grafana != "" || st.f.loki != "" || st.f.ruler != "" || len(st.f.alarms) > 0 || st.f.onCall != nil ||
		st.f.slackFile != "" || st.f.tokenFile != "" || st.f.gcpProj != "" || st.f.auth != "" {
		t.Errorf("a source named: %+v", st.f)
	}
	allowed := map[string]bool{
		"RulesPath": true, "Incidents": true, "Options": true, "Step": true, "Loc": true, "Ledger": true, "Listen": true,
		"Every": true, "Backfill": true, "Keep": true, "KeepActivity": true, "Watch": true, "Digest": true, "Timeout": true,
		"Parallel": true, "ImpactFor": true, "ReadOnly": true, "Version": true, "CheckContent": true, "PromtoolMemory": true,
	}
	v := reflect.ValueOf(st.cfg)
	for i := range v.NumField() {
		name := v.Type().Field(i).Name
		if !allowed[name] && !v.Field(i).IsZero() {
			t.Errorf("Config.%s is %v: a demo's serve gives none", name, v.Field(i))
		}
	}
	if !st.cfg.ReadOnly || st.cfg.Listen != demoListen || !strings.HasPrefix(st.cfg.RulesPath, dir) || !strings.HasPrefix(st.cfg.Incidents, dir) ||
		!strings.HasPrefix(st.cfg.Ledger, dir) {
		t.Errorf("%+v", st.cfg)
	}
	if u, err := url.Parse(st.f.promURL); err == nil && u.Host != "" {
		t.Errorf("--prometheus %s names a host", st.f.promURL)
	}
}
