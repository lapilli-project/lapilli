package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger/ledgertest"
)

const recipes = "../../recipes/incidents/"

// needTools skips a test where what the recipes use isn't installed.
func needTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"sh", "curl", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
}

// fixture answers with a file from the recipes' test data, after checking
// the request carries what the API asks for.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(recipes + "testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// run runs a recipe with an environment, and parses what it writes the way
// --incidents does.
func runRecipe(t *testing.T, script string, env ...string) []incident.Incident {
	t.Helper()
	incs, _ := runRecipeSaid(t, script, env...)
	return incs
}

// runRecipeSaid is runRecipe, and what the recipe said on stderr.
func runRecipeSaid(t *testing.T, script string, env ...string) ([]incident.Incident, string) {
	t.Helper()
	cmd := exec.Command("sh", recipes+script)
	cmd.Env = append(os.Environ(), env...)
	var said strings.Builder
	cmd.Stderr = &said
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v\n%s%s", script, err, out, said.String())
	}
	file := filepath.Join(t.TempDir(), "incidents.yaml")
	os.WriteFile(file, out, 0o600)
	incs, err := incident.Load(file)
	if err != nil {
		t.Fatalf("what %s wrote doesn't load: %v\n%s", script, err, out)
	}
	return incs, said.String()
}

// fakeSleep puts a sleep first on PATH that waits for nothing, and writes
// its argument, a line, to the file it gives: PATH for a recipe's
// environment, and that file.
func fakeSleep(t *testing.T) (path, slept string) {
	t.Helper()
	bin := t.TempDir()
	slept = filepath.Join(bin, "slept")
	if err := os.WriteFile(filepath.Join(bin, "sleep"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >>"+slept+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return "PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), slept
}

// sleeps reads what fakeSleep's sleep was given, space-separated.
func sleeps(t *testing.T, slept string) string {
	t.Helper()
	b, _ := os.ReadFile(slept)
	return strings.Join(strings.Fields(string(b)), " ")
}

func tokenFile(t *testing.T, token string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "token")
	os.WriteFile(file, []byte(token+"\n"), 0o600)
	return file
}

func wantIncidents(t *testing.T, got []incident.Incident, want ...incident.Incident) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("incidents: %+v, want %+v", got, want)
	}
	for i, w := range want {
		g := got[i]
		a, _ := json.Marshal(g.Labels)
		b, _ := json.Marshal(w.Labels)
		x, _ := json.Marshal(g.Alerts)
		y, _ := json.Marshal(w.Alerts)
		if g.ID != w.ID || g.Title != w.Title || !g.Start.Equal(w.Start) || !g.End.Equal(w.End) || string(a) != string(b) || string(x) != string(y) {
			t.Errorf("incident %d: %+v, want %+v", i, g, w)
		}
	}
}

func utc(s string) time.Time {
	x, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return x
}

func TestPagerDutyRecipe(t *testing.T) {
	needTools(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Token token=pd-secret" || !strings.Contains(r.Header.Get("Accept"), "pagerduty+json;version=2") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Each high-urgency incident's alerts, from the documented
		// answer's shape: one of Alertmanager's, its group's alerts in
		// its details as Alertmanager 0.30.0 sends them, one from email
		// with none, one from an Alertmanager whose details say
		// alertname, in cef_details; and the second incident's over two
		// pages, two alerts firing and one of them resolved.
		switch r.URL.Path {
		case "/incidents":
		case "/incidents/Q0AAAAAAAAAAA1/alerts":
			w.Write(fixture(t, "pagerduty-alerts-1.json"))
			return
		case "/incidents/Q0AAAAAAAAAAA3/alerts":
			if r.URL.Query().Get("offset") == "100" {
				w.Write(fixture(t, "pagerduty-alerts-3b.json"))
			} else {
				w.Write(fixture(t, "pagerduty-alerts-3.json"))
			}
			return
		default:
			t.Errorf("asked for %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("since"); got != "2026-10-01" {
			t.Errorf("since = %q", got)
		}
		name := "pagerduty-1.json"
		if r.URL.Query().Get("offset") == "100" {
			name = "pagerduty-2.json"
		}
		w.Write(fixture(t, name))
	}))
	defer srv.Close()
	env := []string{"PAGERDUTY_API=" + srv.URL, "PAGERDUTY_TOKEN_FILE=" + tokenFile(t, "pd-secret"), "SINCE=2026-10-01"}
	got, said := runRecipeSaid(t, "pagerduty.sh", env...)
	wantIncidents(t, got,
		incident.Incident{ID: "PD-101", Title: "Checkout errors", Start: utc("2026-10-28T03:01:00Z"), End: utc("2026-10-28T03:40:00Z"), Labels: incident.Labels{"service": {"checkout"}},
			// CheckoutLatency's details say alertname beside text that
			// may be anything: its rule alone.
			Alerts: []incident.Alert{{Rule: "CheckoutErrors", Labels: map[string]string{"service": "checkout", "severity": "critical"}}, {Rule: "CheckoutLatency"}}},
		incident.Incident{ID: "PD-103", Title: "Search timeouts", Start: utc("2026-10-30T09:15:00Z"), Labels: incident.Labels{"service": {"search"}},
			Alerts: []incident.Alert{{Rule: "SearchTimeouts", Labels: map[string]string{"namespace": "shop", "pod": "search-1", "service": "search"}},
				{Rule: "SearchTimeouts", Labels: map[string]string{"namespace": "shop", "pod": "search-2", "service": "search"}}}})
	if want := "pagerduty.sh: left out 1 of the 6 alerts linked to the incidents: their details carry no alertname\n"; said != want {
		t.Errorf("said %q, want %q", said, want)
	}
	// The labels ALERT_LABELS names alone; with ALERTS=no, none asked for.
	got = runRecipe(t, "pagerduty.sh", append(env, "ALERT_LABELS=service")...)
	if a := got[1].Alerts; len(a) != 1 || len(a[0].Labels) != 1 || a[0].Labels["service"] != "search" {
		t.Errorf("ALERT_LABELS=service: %+v", a)
	}
	if a := got[0].Alerts; len(a) != 2 || a[1].Rule != "CheckoutLatency" || len(a[1].Labels) != 1 || a[1].Labels["service"] != "checkout" {
		t.Errorf("ALERT_LABELS=service, the labels beside alertname it names: %+v", a)
	}
	got = runRecipe(t, "pagerduty.sh", append(env, "ALERTS=no")...)
	if len(got) != 2 || got[0].Alerts != nil || got[1].Alerts != nil {
		t.Errorf("ALERTS=no: %+v", got)
	}
}

func TestIncidentIORecipe(t *testing.T) {
	needTools(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer iio-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		// The alerts linked to the incidents kept, INC-41's over two
		// pages and INC-43's none, and each alert by its id: the
		// documented answers' shapes, attributes and all.
		switch r.URL.Path {
		case "/v2/incidents":
		case "/v2/incident_alerts":
			switch {
			case q.Get("page_size") != "50":
				t.Errorf("page_size %q", q.Get("page_size"))
			case q.Get("incident_id") == "01FCNDV6P870EA6S7TK1DSYD5H" && q.Get("after") == "":
				w.Write(fixture(t, "incident-io-incident-alerts-1.json"))
			case q.Get("incident_id") == "01FCNDV6P870EA6S7TK1DSYD5H" && q.Get("after") == "01GW2G3V0S59R238FAHPDS1A62":
				w.Write(fixture(t, "incident-io-incident-alerts-2.json"))
			case q.Get("incident_id") == "01FCNDV6P870EA6S7TK1DSYD5K":
				w.Write(fixture(t, "incident-io-incident-alerts-none.json"))
			default:
				t.Errorf("asked for the alerts of %v", q)
				http.NotFound(w, r)
			}
			return
		default:
			n, ok := map[string]string{"/v2/alerts/01GW2G3V0S59R238FAHPDS1R61": "1", "/v2/alerts/01GW2G3V0S59R238FAHPDS1R62": "2", "/v2/alerts/01GW2G3V0S59R238FAHPDS1R63": "3"}[r.URL.Path]
			if !ok {
				t.Errorf("asked for %s", r.URL.Path)
				http.NotFound(w, r)
				return
			}
			w.Write(fixture(t, "incident-io-alert-"+n+".json"))
			return
		}
		name := "incident-io-1.json"
		if q.Get("after") != "" {
			name = "incident-io-2.json"
		}
		w.Write(fixture(t, name))
	}))
	defer srv.Close()
	got, said := runRecipeSaid(t, "incident-io.sh", "INCIDENT_IO_API="+srv.URL, "INCIDENT_IO_TOKEN_FILE="+tokenFile(t, "iio-secret"))
	wantIncidents(t, got,
		incident.Incident{ID: "INC-41", Title: "Checkout errors", Start: utc("2026-10-28T03:01:00.801578Z"), End: utc("2026-10-28T03:40:00Z"), Labels: incident.Labels{"service": {"checkout", "cart"}},
			Alerts: []incident.Alert{{Rule: "CheckoutErrors", Labels: map[string]string{"service": "checkout"}}, {Rule: "CheckoutLatency", Labels: map[string]string{"service": "checkout"}}}},
		incident.Incident{ID: "INC-43", Title: "Search timeouts", Start: utc("2026-10-30T09:15:00Z")})
	if want := "incident-io.sh: left out 1 of the 3 alerts linked to the incidents: no attribute of theirs is named alertname\n"; said != want {
		t.Errorf("said %q, want %q", said, want)
	}
	// Past MAX_ALERT_REQUESTS, nothing is written.
	cmd := exec.Command("sh", recipes+"incident-io.sh")
	cmd.Env = append(os.Environ(), "INCIDENT_IO_API="+srv.URL, "INCIDENT_IO_TOKEN_FILE="+tokenFile(t, "iio-secret"), "MAX_ALERT_REQUESTS=3")
	var stdout strings.Builder
	cmd.Stdout = &stdout
	if err := cmd.Run(); err == nil || stdout.Len() != 0 {
		t.Errorf("past MAX_ALERT_REQUESTS: %v, wrote %q", err, stdout.String())
	}
}

func TestJiraRecipe(t *testing.T) {
	needTools(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "ana@example.com" || pass != "jira-secret" || r.URL.Path != "/rest/api/3/search/jql" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if got := r.URL.Query().Get("jql"); got != "project = OPS AND created >= -30d" {
			t.Errorf("jql = %q", got)
		}
		if got := r.URL.Query().Get("fields"); got != "summary,created,resolutiondate,components" {
			t.Errorf("fields = %q", got)
		}
		name := "jira-1.json"
		if tok := r.URL.Query().Get("nextPageToken"); tok != "" {
			if tok != "a token/with odd=characters" {
				t.Errorf("nextPageToken = %q", tok)
			}
			name = "jira-2.json"
		}
		w.Write(fixture(t, name))
	}))
	defer srv.Close()
	got := runRecipe(t, "jira.sh", "JIRA_URL="+srv.URL, "JIRA_EMAIL=ana@example.com", "JIRA_TOKEN_FILE="+tokenFile(t, "jira-secret"),
		"JIRA_JQL=project = OPS AND created >= -30d")
	wantIncidents(t, got,
		incident.Incident{ID: "OPS-41", Title: "Checkout errors", Start: utc("2026-10-28T12:01:00+09:00"), End: utc("2026-10-28T12:40:00+09:00"), Labels: incident.Labels{"service": {"checkout", "cart"}}},
		incident.Incident{ID: "OPS-42", Title: "Search timeouts", Start: utc("2026-10-30T09:15:00-05:00")})

	// jira.jq on its own reads as jira.sh does with nothing set.
	cmd := exec.Command("sh", "-c", `jq -s '[.[].issues[]]' "$1" "$2" | jq -L "$4" -f "$3"`, "sh",
		recipes+"testdata/jira-1.json", recipes+"testdata/jira-2.json", recipes+"jira.jq", recipes)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	file := filepath.Join(t.TempDir(), "incidents.yaml")
	os.WriteFile(file, out, 0o600)
	alone, err := incident.Load(file)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	wantIncidents(t, alone, got...)
}

// Jira Service Management's incidents, with the alerts linked to each: the
// incident API's list of their ids, over two pages, and each alert from the
// operations API, as their references show them. One Alertmanager sent,
// its group's labels in its extra properties, is one of the incident's;
// one with no alertname, and one deleted since, are left out, and so is
// an issue the API says isn't an incident. A 429 is waited out.
func TestJiraRecipeLinkedAlerts(t *testing.T) {
	needTools(t)
	const cloud = "35273b54-3f06-40d2-880f-dd28cf8daafa"
	var mu sync.Mutex
	jsm := 0
	// limit: how many requests for alerts are answered 429 first.
	limit := 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "ana@example.com" || pass != "jira-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		incidents := "/jsm/incidents/cloudId/" + cloud + "/v1/incident/"
		ops := "/jsm/ops/api/" + cloud + "/v1/alerts/"
		p := r.URL.Path
		if p == "/rest/api/3/search/jql" {
			name := "jira-1.json"
			if r.URL.Query().Get("nextPageToken") != "" {
				name = "jira-2.json"
			}
			w.Write(fixture(t, name))
			return
		}
		mu.Lock()
		jsm++
		limited := jsm <= limit
		mu.Unlock()
		if limited {
			// Eight seconds, which a shell would read as octal.
			w.Header().Set("Retry-After", "08")
			http.Error(w, `{"errors": [{"title": "Too many requests"}]}`, http.StatusTooManyRequests)
			return
		}
		switch {
		case p == incidents+"10001/alert":
			switch r.URL.Query().Get("startKey") {
			case "":
				w.Write(fixture(t, "jsm-linked-1.json"))
			case "2":
				w.Write(fixture(t, "jsm-linked-1b.json"))
			default:
				t.Errorf("startKey %q", r.URL.Query().Get("startKey"))
			}
		case p == incidents+"10002/alert":
			// "Returned if the Jira issue corresponding to the given
			// issue Id is not of type incident."
			http.Error(w, `{"errors": [{"code": "BAD_REQUEST"}]}`, http.StatusBadRequest)
		case strings.HasPrefix(p, ops):
			n, ok := map[string]string{"8dd24912-a0ad-41cd-ac80-535eaf649ed7": "1", "e0caa0ce-d52f-4500-81b9-d592d06970b6": "2",
				"28517c02-e621-4d51-9435-a7fe693b93dd-1770130534412": "3"}[strings.TrimPrefix(p, ops)]
			if !ok {
				http.Error(w, `{"errors": [{"title": "Alert not found"}]}`, http.StatusNotFound)
				return
			}
			w.Write(fixture(t, "jsm-alert-"+n+".json"))
		case !strings.Contains(p, cloud):
			// Another site's, as a wrong JSM_CLOUD_ID would ask.
			http.Error(w, `{"errors": [{"title": "Not found"}]}`, http.StatusNotFound)
		default:
			t.Errorf("asked for %s", p)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	path, slept := fakeSleep(t)
	env := []string{"JSM_API=" + srv.URL, "JSM_CLOUD_ID=" + cloud, path}
	out, said, err := runJira(t, srv.URL, env...)
	if err != nil {
		t.Fatalf("%v\n%s", err, said)
	}
	if got := sleeps(t, slept); got != "8" {
		t.Errorf("slept %q, want 8", got)
	}
	wantIncidents(t, loadIncidents(t, out),
		incident.Incident{ID: "OPS-41", Title: "Checkout errors", Start: utc("2026-10-28T12:01:00+09:00"), End: utc("2026-10-28T12:40:00+09:00"), Labels: incident.Labels{"service": {"checkout", "cart"}},
			Alerts: []incident.Alert{{Rule: "CheckoutErrors", Labels: map[string]string{"service": "checkout", "severity": "critical"}},
				{Rule: "CheckoutLatency", Labels: map[string]string{"cluster": "eu-1", "service": "checkout"}}}},
		incident.Incident{ID: "OPS-42", Title: "Search timeouts", Start: utc("2026-10-30T09:15:00-05:00")})
	if want := "jira.sh: 1 of the issues are not incidents, as the incident API says: their alerts were not read\n" +
		"jira.sh: left out 1 alerts linked to the incidents that the operations API no longer has\n" +
		"jira.sh: left out 1 of the 3 alerts linked to the incidents: their extra properties carry no alertname\n"; said != want {
		t.Errorf("said %q, want %q", said, want)
	}
	// set sets how many requests for alerts are answered 429 first, and
	// gives how many were asked for since it was last called.
	set := func(n int) int {
		mu.Lock()
		defer mu.Unlock()
		asked := jsm
		limit, jsm = n, 0
		return asked
	}
	// One 429 waited out, and seven requests for alerts besides: two pages
	// of 10001's, 10002's, and 10001's four alerts.
	if n := set(0); n != 8 {
		t.Errorf("%d requests for alerts, want 8", n)
	}

	// The labels ALERT_LABELS names alone; with ALERTS=no, or without
	// JSM_CLOUD_ID, none asked for.
	out, said, err = runJira(t, srv.URL, append(env, "ALERT_LABELS=service")...)
	if got := loadIncidents(t, out); err != nil || len(got[0].Alerts) != 2 || len(got[0].Alerts[1].Labels) != 1 || got[0].Alerts[1].Labels["service"] != "checkout" {
		t.Errorf("ALERT_LABELS=service: %v %+v\n%s", err, got, said)
	}
	for _, no := range [][]string{append(env, "ALERTS=no"), nil} {
		set(0)
		out, said, err = runJira(t, srv.URL, no...)
		if got, n := loadIncidents(t, out), set(0); err != nil || len(got) != 2 || got[0].Alerts != nil || n != 0 || said != "" {
			t.Errorf("%q: %v, %d requests for alerts, %+v, said %q", no, err, n, got, said)
		}
	}

	// Settings that can't be: refused before Jira is asked anything.
	for _, bad := range [][]string{{"ALERTS=yes"}, {"ALERTS=maybe", "JSM_CLOUD_ID=" + cloud}, {"JSM_CLOUD_ID=" + cloud + "/../x"},
		{"JSM_CLOUD_ID=" + cloud, "MAX_ALERT_REQUESTS=all"}} {
		cmd := exec.Command("sh", recipes+"jira.sh")
		cmd.Env = append(os.Environ(), append([]string{"JIRA_URL=http://127.0.0.1:1", "JIRA_EMAIL=ana@example.com",
			"JIRA_TOKEN_FILE=" + tokenFile(t, "jira-secret"), "JIRA_JQL=project = OPS"}, bad...)...)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if out, err := cmd.Output(); err == nil || len(out) != 0 || strings.Contains(stderr.String(), "curl") {
			t.Errorf("%q: %v, wrote %q, said %q", bad, err, out, stderr.String())
		}
	}

	// Past MAX_ALERT_REQUESTS, past five 429s, and an answer the API
	// shouldn't give: nothing written.
	for _, c := range []struct {
		env   []string
		limit int
		says  string
	}{
		{[]string{"MAX_ALERT_REQUESTS=3"}, 0, "more than 3 requests for alerts"},
		{nil, 6, "still says too many requests after 5 waits"},
		{[]string{"JSM_CLOUD_ID=00000000-0000-0000-0000-000000000000"}, 0, "answered 404 for the alerts of issue 10001"},
	} {
		set(c.limit)
		out, said, err := runJira(t, srv.URL, append(append([]string{}, env...), c.env...)...)
		if err == nil || out != "" || !strings.Contains(said, c.says) {
			t.Errorf("%q: %v, wrote %q, said %q", c.env, err, out, said)
		}
	}
}

// Jira's search waits out a 429 as Jira Service Management's requests do:
// as its Retry-After says (08 is eight), or else 5 seconds and twice as
// long each time; past five waits a request, it stops with nothing
// written, and any other answer that isn't a 2xx stops it, saying its
// status. Under sh and dash alike.
func TestJiraRecipeSearchWaits(t *testing.T) {
	needTools(t)
	shells := []string{"sh"}
	if _, err := exec.LookPath("dash"); err == nil {
		shells = append(shells, "dash")
	}
	for _, shell := range shells {
		for _, c := range []struct {
			name string
			// answer gives the status for the nth search request, and its
			// Retry-After; 200 for the page.
			answer func(n int) (int, string)
			sleeps string
			says   string
		}{
			{"waited out", func(n int) (int, string) {
				switch n {
				case 0:
					return http.StatusTooManyRequests, "08"
				case 2:
					return http.StatusTooManyRequests, ""
				}
				return http.StatusOK, ""
			}, "8 5", ""},
			{"past five waits", func(int) (int, string) { return http.StatusTooManyRequests, "" }, "5 10 20 40 80", "jira.sh: Jira still says too many requests after 5 waits, 155 seconds in all; stopping, writing nothing"},
			{"refused", func(int) (int, string) { return http.StatusUnauthorized, "" }, "", "jira.sh: Jira's search answered 401; writing nothing"},
		} {
			var mu sync.Mutex
			asked := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				code, after := c.answer(asked)
				asked++
				mu.Unlock()
				if r.URL.Path != "/rest/api/3/search/jql" {
					t.Errorf("asked for %s", r.URL.Path)
				}
				if code != http.StatusOK {
					if after != "" {
						w.Header().Set("Retry-After", after)
					}
					http.Error(w, `{"errorMessages": ["no"]}`, code)
					return
				}
				name := "jira-1.json"
				if r.URL.Query().Get("nextPageToken") != "" {
					name = "jira-2.json"
				}
				w.Write(fixture(t, name))
			}))
			path, slept := fakeSleep(t)
			cmd := exec.Command(shell, recipes+"jira.sh")
			cmd.Env = append(os.Environ(), "JIRA_URL="+srv.URL, "JIRA_EMAIL=ana@example.com", "JIRA_TOKEN_FILE="+tokenFile(t, "jira-secret"),
				"JIRA_JQL=project = OPS", path)
			var stdout, stderr strings.Builder
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			srv.Close()
			got := sleeps(t, slept)
			if c.says == "" {
				if err != nil || got != c.sleeps || stderr.Len() != 0 {
					t.Errorf("%s, %s: %v, slept %q, said %q", shell, c.name, err, got, stderr.String())
				} else if incs := loadIncidents(t, stdout.String()); len(incs) != 2 || incs[1].ID != "OPS-42" {
					t.Errorf("%s, %s: %+v", shell, c.name, incs)
				}
				continue
			}
			if err == nil || stdout.Len() != 0 || got != c.sleeps || strings.TrimSpace(stderr.String()) != c.says {
				t.Errorf("%s, %s: %v, slept %q, wrote %q, said %q", shell, c.name, err, got, stdout.String(), stderr.String())
			}
		}
	}
}

// Where a team keeps an incident's times and services in fields of its own,
// they are read from there, and only the fields named are asked for. A date
// is never taken for a time, nor is a time Go wouldn't read: an issue whose
// start is one is skipped, said by its key, and counted; one whose end is
// one is kept with no end. With every issue skipped, nothing is written and
// the script fails.
func TestJiraRecipeFields(t *testing.T) {
	needTools(t)
	var asked []string
	body := fixture(t, "jira-fields.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Query().Get("fields"))
		w.Write(body)
	}))
	defer srv.Close()
	run := func(env ...string) (string, string, error) {
		return runJira(t, srv.URL, env...)
	}
	titles := []string{"Payment errors", "Login slow", "Queue backlog", "Postmortem written later", "Ends on a day",
		"Ends before it starts", "Written by hand", "A key to keep off a terminal", "Times written otherwise",
		"No such day", "A leap second", "No such zone"}
	quiet := func(stderr string) {
		t.Helper()
		for _, title := range titles {
			if strings.Contains(stderr, title) {
				t.Errorf("a title on stderr: %s", stderr)
			}
		}
		if strings.ContainsAny(stderr, "\x1b\x07") {
			t.Errorf("a key from Jira reached the terminal as it was: %q", stderr)
		}
	}
	says := func(stderr string, want ...string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(stderr, w) {
				t.Errorf("stderr doesn't say %q:\n%s", w, stderr)
			}
		}
	}
	labels := "JIRA_LABELS=service=customfield_10003, service=labels,team=customfield_10004,owner=customfield_10005,region=customfield_10006,tier=customfield_10008"
	// An empty option and an empty text give nothing; a number gives its
	// text; a cascading select its parent.
	ops51 := incident.Incident{ID: "OPS-51", Title: "Payment errors", Start: utc("2026-11-05T14:30:00+09:00"), End: utc("2026-11-05T15:10:00+09:00"),
		Labels: incident.Labels{"service": {"payments", "ledger", "api"}, "team": {"payments-team"}, "owner": {"Ana Lee"}, "region": {"eu-west"}, "tier": {"1"}}}
	ops54 := incident.Incident{ID: "OPS-54", Title: "Search down", Start: utc("2026-11-09T01:00:00-05:00"), Labels: incident.Labels{"owner": {"Bo Kim", "Cy Diaz"}}}
	ops56 := incident.Incident{ID: "OPS-56", Title: "Ends on a day", Start: utc("2026-11-11T08:00:00+09:00")}
	ops57 := incident.Incident{ID: "OPS-57", Title: "Ends before it starts", Start: utc("2026-11-11T08:00:00+09:00")}
	ops60 := incident.Incident{ID: "OPS-60", Title: "Times written otherwise", Start: utc("2026-11-13T09:00:00Z"), End: utc("2026-11-13T18:30:00+09:00")}

	out, stderr, err := run("JIRA_START_FIELD=customfield_10001", "JIRA_END_FIELD=customfield_10002", labels)
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if want := "summary,customfield_10001,customfield_10002,customfield_10003,labels,customfield_10004,customfield_10005,customfield_10006,customfield_10008"; len(asked) != 1 || asked[0] != want {
		t.Errorf("fields = %q, want %q", asked, want)
	}
	wantIncidents(t, loadIncidents(t, out), ops51, ops54, ops56, ops57, ops60)
	says(stderr,
		`"OPS-52" skipped: the start field customfield_10001 holds a date and no time`+"\n",
		`"OPS-53" skipped: no start in customfield_10001`+"\n",
		`"OPS-55" skipped: the start field customfield_10001 holds a date and no time`+"\n",
		`"OPS-58" skipped: the start field customfield_10001 holds a date and nothing that reads as a time`+"\n",
		`"OPS-61" skipped: the start field customfield_10001 holds something other than a time`+"\n",
		`"OPS-62" skipped: the start field customfield_10001 holds a date and nothing that reads as a time`+"\n",
		`"OPS-63" skipped: the start field customfield_10001 holds a date and nothing that reads as a time`+"\n",
		`"OPS-56" kept with no end, taken to last an hour: the end field customfield_10002 holds a date and no time`+"\n",
		`"OPS-57" kept with no end, taken to last an hour: the end field customfield_10002 is before the start field customfield_10001`+"\n",
		`jira.sh: skipped 8 of 13 issues: `,
		`the start field customfield_10001 holds a date and no time (3)`,
		`jira.sh: kept 2 of 13 issues with no end: `)
	quiet(stderr)

	// created after the field: taken where the field is empty, and where it
	// holds a date, only on that day; not past a field holding anything
	// else. A field named twice is asked for once.
	asked = nil
	out, stderr, err = run("JIRA_START_FIELD=customfield_10001,created", "JIRA_END_FIELD=customfield_10002", labels+",topic=labels")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if want := "summary,customfield_10001,created,customfield_10002,customfield_10003,labels,customfield_10004,customfield_10005,customfield_10006,customfield_10008"; len(asked) != 1 || asked[0] != want {
		t.Errorf("fields = %q, want %q", asked, want)
	}
	ops51.Labels["topic"] = []string{"payments", "api"}
	wantIncidents(t, loadIncidents(t, out), ops51,
		incident.Incident{ID: "OPS-52", Title: "Login slow", Start: utc("2026-11-07T10:20:00+09:00")},
		incident.Incident{ID: "OPS-53", Title: "Queue backlog", Start: utc("2026-11-08T08:00:00Z")},
		ops54, ops56, ops57,
		incident.Incident{ID: "OPS-58", Title: "Written by hand", Start: utc("2026-11-12T11:00:00+09:00")},
		ops60,
		incident.Incident{ID: "OPS-62", Title: "A leap second", Start: utc("2026-11-14T10:05:00+09:00")})
	says(stderr,
		`"OPS-55" skipped: the start field customfield_10001 holds a date and no time, and created is on another day`+"\n",
		`"OPS-61" skipped: the start field customfield_10001 holds something other than a time`+"\n",
		`"OPS-63" skipped: the start field customfield_10001 holds a date and nothing that reads as a time, and created is on another day`+"\n",
		`jira.sh: skipped 4 of 13 issues: `)
	quiet(stderr)

	// No labels: components aren't asked for.
	asked = nil
	out, stderr, err = run("JIRA_LABELS=")
	if err != nil || len(asked) != 1 || asked[0] != "summary,created,resolutiondate" || strings.Contains(out, "labels") {
		t.Errorf("JIRA_LABELS empty: %v, asked %q: %s\n%s", err, asked, out, stderr)
	}

	// Every issue skipped: a field that none has, or a date picker.
	if out, stderr, err := run("JIRA_START_FIELD=customfield_10007"); err == nil || out != "" ||
		!strings.Contains(stderr, "skipped 13 of 13 issues: no start in customfield_10007 (13)") || !strings.Contains(stderr, "every issue found was skipped") {
		t.Errorf("all skipped: %v, wrote %q\n%s", err, out, stderr)
	}
	body = []byte(`{"issues": [{"key": "OPS-71", "fields": {"summary": "x", "customfield_10001": "2026-11-05"}},
		{"key": "OPS-72", "fields": {"summary": "y", "customfield_10001": "2026-11-06"}}], "isLast": true}`)
	if out, stderr, err := run("JIRA_START_FIELD=customfield_10001"); err == nil || out != "" || !strings.Contains(stderr, "every issue found was skipped") {
		t.Errorf("all dates with no time: %v, wrote %q\n%s", err, out, stderr)
	}
	// Nothing found isn't everything skipped.
	body = []byte(`{"issues": [], "isLast": true}`)
	if out, stderr, err := run("JIRA_START_FIELD=customfield_10001"); err != nil || len(loadIncidents(t, out)) != 0 {
		t.Errorf("none found: %v, wrote %q\n%s", err, out, stderr)
	}

	// Settings that aren't fields or labels are refused before Jira is asked,
	// and so are more labels than an incident has.
	asked = nil
	var many []string
	for i := range 33 {
		many = append(many, fmt.Sprintf("l%d=labels", i))
	}
	for _, env := range []string{"JIRA_START_FIELD=created&jql=x", "JIRA_END_FIELD=a,,b", "JIRA_LABELS=service",
		"JIRA_LABELS=9x=components", "JIRA_LABELS=service=components;x", "JIRA_START_FIELD=customfield 10001",
		"JIRA_LABELS=" + strings.Join(many, ",")} {
		if out, stderr, err := run(env); err == nil || out != "" || !strings.HasPrefix(stderr, "jira.sh: JIRA_") {
			t.Errorf("%.60s taken: %v, wrote %q\n%s", env, err, out, stderr)
		}
	}
	if len(asked) != 0 {
		t.Errorf("Jira asked with settings refused: %q", asked)
	}
	if _, stderr, err := run("JIRA_LABELS=" + strings.Join(many[:32], ",")); err != nil {
		t.Errorf("32 labels refused: %v\n%s", err, stderr)
	}
}

// One issue edited to hold more than serve takes doesn't have its incident
// refused: the values past it are left out, and said by the issue's key.
func TestJiraRecipeLabelBounds(t *testing.T) {
	needTools(t)
	value := func(i int, s string) map[string]string {
		return map[string]string{"value": fmt.Sprintf("%s-%02d", s, i)}
	}
	var tags []string
	for i := range 70 {
		tags = append(tags, fmt.Sprintf("t%02d", i))
	}
	tags = append(tags, strings.Repeat("x", 257))
	// Go writes U+2028 as \u2028: a value of 80 of them is 240 bytes, and
	// 480 as serve keeps it.
	var wide []map[string]string
	for i := range 30 {
		wide = append(wide, value(i, strings.Repeat("\u2028", 80)))
	}
	issues := map[string]any{"isLast": true, "issues": []any{
		map[string]any{"key": "OPS-81", "fields": map[string]any{"summary": "Many tags", "created": "2026-11-05T14:30:00.000+0900", "labels": tags}},
		map[string]any{"key": "OPS-82", "fields": map[string]any{"summary": "Wide options", "created": "2026-11-05T15:30:00.000+0900", "customfield_10003": wide}},
	}}
	body, _ := json.Marshal(issues)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer srv.Close()
	out, stderr, err := runJira(t, srv.URL, "JIRA_LABELS=tag=labels,service=customfield_10003")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	for _, want := range []string{
		`jira.sh: "OPS-81" kept with label values left out: 1 over 256 bytes, 6 past 64 a label` + "\n",
		`jira.sh: "OPS-82" kept with label values left out: `, ` past 4 KB in all` + "\n",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr doesn't say %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "Many tags") || strings.Contains(stderr, "Wide options") {
		t.Errorf("a title on stderr: %s", stderr)
	}
	got := loadIncidents(t, out)
	if len(got) != 2 || len(got[0].Labels["tag"]) != 64 || len(got[1].Labels["service"]) == 0 {
		t.Fatalf("labels: %+v", got)
	}

	// Through put.sh, serve takes both.
	file := tokenFile(t, "a-token-long-enough")
	s, _ := server(t, func(c *Config) { c.IncidentsListen, c.IncidentsToken = "127.0.0.1:0", file })
	port := httptest.NewServer(s.IncidentsHandler())
	defer port.Close()
	cmd := exec.Command("sh", recipes+"put.sh", port.URL)
	cmd.Env = append(os.Environ(), "LAPILLI_TOKEN_FILE="+file)
	cmd.Stdin = strings.NewReader(out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("serve refused one: %v\n%s", err, b)
	}
	for _, id := range []string{"OPS-81", "OPS-82"} {
		if _, err := s.l.IncidentByID(context.Background(), id); err != nil {
			t.Errorf("%s: %v", id, err)
		}
	}
}

// runJira runs jira.sh against a server standing in for Jira, with its
// stdout and stderr apart.
func runJira(t *testing.T, url string, env ...string) (string, string, error) {
	t.Helper()
	cmd := exec.Command("sh", recipes+"jira.sh")
	cmd.Env = append(os.Environ(), append([]string{"JIRA_URL=" + url, "JIRA_EMAIL=ana@example.com",
		"JIRA_TOKEN_FILE=" + tokenFile(t, "jira-secret"), "JIRA_JQL=project = OPS"}, env...)...)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

// loadIncidents parses what a recipe wrote the way --incidents does.
func loadIncidents(t *testing.T, out string) []incident.Incident {
	t.Helper()
	file := filepath.Join(t.TempDir(), "incidents.yaml")
	os.WriteFile(file, []byte(out), 0o600)
	incs, err := incident.Load(file)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return incs
}

func TestGitHubRecipe(t *testing.T) {
	needTools(t)
	cmd := exec.Command("jq", "-f", recipes+"github.jq", recipes+"testdata/github.json")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	file := filepath.Join(t.TempDir(), "incidents.yaml")
	os.WriteFile(file, out, 0o600)
	got, err := incident.Load(file)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	wantIncidents(t, got,
		incident.Incident{ID: "GH-41", Title: "Checkout errors", Start: utc("2026-10-28T03:01:00Z"), End: utc("2026-10-28T03:40:00Z"), Labels: incident.Labels{"service": {"checkout"}}},
		incident.Incident{ID: "GH-42", Title: "Search timeouts", Start: utc("2026-10-30T09:15:00Z")})
}

// From the tool to the ledger: a recipe's output, through put.sh, into the
// port that takes incidents. Done again, it changes nothing.
func TestPutRecipe(t *testing.T) {
	needTools(t)
	token := "a-token-long-enough"
	file := tokenFile(t, token)
	s, _ := server(t, func(c *Config) { c.IncidentsListen, c.IncidentsToken = "127.0.0.1:0", file })
	srv := httptest.NewServer(s.IncidentsHandler())
	defer srv.Close()
	put := func(body, tok string) (string, error) {
		cmd := exec.Command("sh", recipes+"put.sh", srv.URL)
		cmd.Env = append(os.Environ(), "LAPILLI_TOKEN_FILE="+tokenFile(t, tok))
		cmd.Stdin = strings.NewReader(body)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	body := `{"incidents": [
	  {"id": "PD-101", "title": "Checkout errors", "start": "2026-10-28T03:01:00Z", "end": "2026-10-28T03:40:00Z", "labels": {"service": ["checkout"]}},
	  {"id": "OPS-41", "title": "A zone written otherwise", "start": "2026-10-28T12:01:00+09:00"},
	  {"id": "PD-102", "title": "Too long", "start": "2026-08-01T00:00:00Z", "end": "2026-10-28T00:00:00Z"},
	  {"id": "PD-103", "title": "After one refused", "start": "2026-10-30T09:15:00Z"}
	]}`
	out, err := put(body, token)
	if err == nil || !strings.Contains(out, `"PD-102" wasn't written (400)`) {
		t.Fatalf("one refused should be told of, and fail the script: %v\n%s", err, out)
	}
	ctx := context.Background()
	for _, id := range []string{"PD-101", "OPS-41", "PD-103"} {
		in, err := s.l.IncidentByID(ctx, id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if in.By != tokenUser || in.Origin != "copy" {
			t.Errorf("%s: by %q, origin %q", id, in.By, in.Origin)
		}
	}
	changes := func() int {
		cs, err := s.l.ChangesSince(ctx, time.Unix(0, 0), 1000)
		if err != nil {
			t.Fatal(err)
		}
		return len(cs)
	}
	before, gen := changes(), s.l.Gen()
	if before != 3 {
		t.Errorf("changes = %d, want 3", before)
	}
	// The same again: nothing changes, and no page is thrown away.
	if out, err := put(strings.Replace(body, `{"id": "PD-102", "title": "Too long", "start": "2026-08-01T00:00:00Z", "end": "2026-10-28T00:00:00Z"},`, "", 1), token); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if changes() != before || s.l.Gen() != gen {
		t.Errorf("written again as it was: %d → %d changes, generation %d → %d", before, changes(), gen, s.l.Gen())
	}
	// Changed: written. The wrong token writes nothing, and says so.
	if out, err := put(`{"incidents": [{"id": "PD-103", "title": "After one refused", "start": "2026-10-30T09:15:00Z", "end": "2026-10-30T10:00:00Z"}]}`, token); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if in, _ := s.l.IncidentByID(ctx, "PD-103"); in.End.IsZero() {
		t.Error("the change wasn't written")
	}
	if out, err := put(`{"incidents": [{"id": "PD-104", "start": "2026-10-30T09:15:00Z"}]}`, "not the token at all"); err == nil {
		t.Errorf("the wrong token: %s", out)
	}
	if _, err := s.l.IncidentByID(ctx, "PD-104"); err == nil {
		t.Error("the wrong token wrote an incident")
	}
}

// The port takes incidents, from the token's holder, and nothing else; it
// believes no header, and the UI's port doesn't take the token.
func TestIncidentsPort(t *testing.T) {
	const token = "a-token-long-enough"
	file := tokenFile(t, token)
	s, _ := server(t, func(c *Config) {
		c.IncidentsListen, c.IncidentsToken = "127.0.0.1:0", file
		c.UserHeader, c.Editors = "X-Forwarded-Email", []string{"ana@example.com"}
	})
	port, ui := s.IncidentsHandler(), s.Handler()
	send := func(h http.Handler, method, path, tok string, hdr map[string]string, body string) int {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	inc := `{"title": "x", "start": "2026-10-30T09:15:00Z"}`
	if c := send(port, "PUT", "/api/v1/incidents/A-1", "", nil, inc); c != http.StatusUnauthorized {
		t.Errorf("no token: %d", c)
	}
	if c := send(port, "PUT", "/api/v1/incidents/A-1", "wrong", nil, inc); c != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", c)
	}
	if c := send(port, "PUT", "/api/v1/incidents/A-1", "", map[string]string{"X-Forwarded-Email": "ana@example.com"}, inc); c != http.StatusUnauthorized {
		t.Errorf("a named editor with no token: %d", c)
	}
	if c := send(port, "PUT", "/api/v1/incidents/A-1", token, map[string]string{"X-Forwarded-Email": "ana@example.com"}, inc); c != http.StatusOK {
		t.Errorf("the token: %d", c)
	}
	ctx := context.Background()
	if in, err := s.l.IncidentByID(ctx, "A-1"); err != nil || in.By != tokenUser {
		t.Errorf("noted as %q, not whom a header named: %v", in.By, err)
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/v1/incidents"}, {"GET", "/api/v1/incidents/A-1"}, {"POST", "/api/v1/tasks"}, {"POST", "/api/v1/teams"},
		{"POST", "/settings"}, {"GET", "/"}, {"GET", "/metrics"}, {"POST", "/mcp"},
	} {
		if code := send(port, c.method, c.path, token, nil, "{}"); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s on the incidents port: %d", c.method, c.path, code)
		}
	}
	if c := send(port, "DELETE", "/api/v1/incidents/A-1", token, nil, ""); c != http.StatusNoContent {
		t.Errorf("delete: %d", c)
	}
	// The UI's port: the token is no one, and may only look.
	if c := send(ui, "PUT", "/api/v1/incidents/A-2", token, nil, inc); c != http.StatusForbidden {
		t.Errorf("the token on the UI's port: %d", c)
	}
	// A rotated token is picked up, and the old one goes.
	os.WriteFile(file, []byte("another-token-long-enough\n"), 0o600)
	if c := send(port, "PUT", "/api/v1/incidents/A-3", token, nil, inc); c != http.StatusUnauthorized {
		t.Errorf("the old token: %d", c)
	}
	if c := send(port, "PUT", "/api/v1/incidents/A-3", "another-token-long-enough", nil, inc); c != http.StatusOK {
		t.Errorf("the new token: %d", c)
	}
	// An empty file is no token, whatever is sent.
	os.WriteFile(file, nil, 0o600)
	if c := send(port, "PUT", "/api/v1/incidents/A-4", " ", nil, inc); c != http.StatusUnauthorized {
		t.Errorf("an empty token file: %d", c)
	}
	// Writes made with it aren't anyone's use of Lapilli.
	outs, err := s.outcomes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if outs.Use.Edits != 0 {
		t.Errorf("edits with the token counted as use: %d", outs.Use.Edits)
	}
	// Read-only refuses them.
	s.cfg.ReadOnly = true
	os.WriteFile(file, []byte(token), 0o600)
	if c := send(port, "PUT", "/api/v1/incidents/A-5", token, nil, inc); c != http.StatusForbidden {
		t.Errorf("read-only: %d", c)
	}
}

// A script that fails says so, writes nothing, and put.sh takes nothing that
// isn't a list of incidents: a green job with nothing copied is the failure to
// fear.
func TestRecipesFailLoudly(t *testing.T) {
	needTools(t)
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		if pages > 1 {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		w.Write(fixture(t, "pagerduty-1.json"))
	}))
	defer srv.Close()
	cmd := exec.Command("sh", recipes+"pagerduty.sh")
	cmd.Env = append(os.Environ(), "PAGERDUTY_API="+srv.URL, "PAGERDUTY_TOKEN_FILE="+tokenFile(t, "x"))
	var stdout strings.Builder
	cmd.Stdout = &stdout
	if err := cmd.Run(); err == nil || stdout.Len() != 0 {
		t.Errorf("page 2 failing: %v, wrote %q", err, stdout.String())
	}

	file := tokenFile(t, "a-token-long-enough")
	s, _ := server(t, func(c *Config) { c.IncidentsListen, c.IncidentsToken = "127.0.0.1:0", file })
	port := httptest.NewServer(s.IncidentsHandler())
	defer port.Close()
	for _, in := range []string{"", "not json", `{"nothing": []}`, `{"incidents": "no"}`, "incidents:\n  - {id: A, start: 2026-10-30T09:15:00Z}\n",
		`{"incidents": [{"title": "no id", "start": "2026-10-30T09:15:00Z"}]}`} {
		cmd := exec.Command("sh", recipes+"put.sh", port.URL)
		cmd.Env = append(os.Environ(), "LAPILLI_TOKEN_FILE="+file)
		cmd.Stdin = strings.NewReader(in)
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Errorf("put.sh took %q: %s", in, out)
		}
	}
	if _, err := s.l.IncidentByID(context.Background(), "null"); err == nil {
		t.Error("an incident with no id was written as null")
	}
	// None is fine: there is nothing to copy.
	cmd = exec.Command("sh", recipes+"put.sh", port.URL)
	cmd.Env = append(os.Environ(), "LAPILLI_TOKEN_FILE="+file)
	cmd.Stdin = strings.NewReader(`{"incidents": []}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("an empty list: %v: %s", err, out)
	}
}

// What goes in a URL is encoded: a zone's + would otherwise reach the tool as a
// space.
func TestRecipeEncodesTimes(t *testing.T) {
	needTools(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("since"); got != "2026-10-01T00:00:00+09:00" {
			t.Errorf("since = %q", got)
		}
		w.Write([]byte(`{"incidents": [], "more": false}`))
	}))
	defer srv.Close()
	runRecipe(t, "pagerduty.sh", "PAGERDUTY_API="+srv.URL, "PAGERDUTY_TOKEN_FILE="+tokenFile(t, "x"), "SINCE=2026-10-01T00:00:00+09:00")
}

// What the token copies in is the tool's, and what people write is theirs:
// neither changes the other's, so the token can't quietly move or take away
// an incident a person wrote, and a vendor id that happens to be L-1 can't
// write over one.
func TestTokenKeepsToItsOwn(t *testing.T) {
	const token = "a-token-long-enough"
	file := tokenFile(t, token)
	s, _ := server(t, func(c *Config) {
		c.IncidentsListen, c.IncidentsToken = "127.0.0.1:0", file
		c.UserHeader, c.Editors = "X-Forwarded-Email", []string{"ana@example.com"}
	})
	port, ui := s.IncidentsHandler(), s.Handler()
	send := func(h http.Handler, method, path string, hdr map[string]string, body string) int {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	ana := map[string]string{"X-Forwarded-Email": "ana@example.com"}
	bearer := map[string]string{"Authorization": "Bearer " + token}
	inc := `{"title": "x", "start": "2026-10-30T09:15:00Z"}`
	if c := send(ui, "POST", "/api/v1/incidents", ana, inc); c != http.StatusCreated {
		t.Fatalf("ana: %d", c)
	}
	for _, c := range []struct {
		h            http.Handler
		method, path string
		hdr          map[string]string
		want         int
	}{
		{port, "PUT", "/api/v1/incidents/L-1", bearer, http.StatusConflict},
		{port, "DELETE", "/api/v1/incidents/L-1", bearer, http.StatusConflict},
		{port, "PUT", "/api/v1/incidents/PD-1", bearer, http.StatusOK},
		{ui, "PUT", "/api/v1/incidents/PD-1", ana, http.StatusConflict},
		{ui, "DELETE", "/api/v1/incidents/PD-1", ana, http.StatusConflict},
	} {
		if got := send(c.h, c.method, c.path, c.hdr, inc); got != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, got, c.want)
		}
	}
	ctx := context.Background()
	if in, err := s.l.IncidentByID(ctx, "L-1"); err != nil || in.By != "ana@example.com" {
		t.Errorf("ana's incident: %+v %v", in, err)
	}
	// A person can't take the name the token's writes are noted under.
	if c := send(ui, "PUT", "/api/v1/incidents/PD-1", map[string]string{"X-Forwarded-Email": tokenUser}, inc); c != http.StatusForbidden {
		t.Errorf("named as the token: %d", c)
	}
	// The page says where it came from, in each language.
	for _, l := range []string{"en", "ko"} {
		r := httptest.NewRequest("GET", "/incidents/PD-1", nil)
		r.Header.Set("Accept-Language", l)
		w := httptest.NewRecorder()
		ui.ServeHTTP(w, r)
		want := map[string]string{"en": "Copied in from the tool", "ko": "도구에서 가져왔어요"}[l]
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: no %q on the page", l, want)
		}
	}
}

// A job needs a few writes a second; more is told to slow down, and put.sh
// waits and tries again.
func TestTokenRate(t *testing.T) {
	const token = "a-token-long-enough"
	file := tokenFile(t, token)
	s, _ := server(t, func(c *Config) { c.IncidentsListen, c.IncidentsToken = "127.0.0.1:0", file })
	// In Postgres across a network an incident takes tens of milliseconds,
	// sixty a second or more, in which twenty a second refill the burst:
	// there, it refills slower. SQLite holds the rate New gives.
	if ledgertest.On() {
		s.tokenRate.rate = 1
	}
	port := s.IncidentsHandler()
	codes := map[int]int{}
	for i := range 60 {
		r := httptest.NewRequest("PUT", fmt.Sprintf("/api/v1/incidents/R-%d", i), strings.NewReader(`{"start": "2026-10-30T09:15:00Z"}`))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		port.ServeHTTP(w, r)
		codes[w.Code]++
		if w.Code == http.StatusTooManyRequests && w.Header().Get("Retry-After") == "" {
			t.Error("no Retry-After")
		}
	}
	if codes[http.StatusTooManyRequests] == 0 || codes[http.StatusOK] < 40 {
		t.Errorf("codes: %v", codes)
	}
	// A token rotated to one too short is no token.
	os.WriteFile(file, []byte("abc"), 0o600)
	r := httptest.NewRequest("PUT", "/api/v1/incidents/S-1", strings.NewReader(`{"start": "2026-10-30T09:15:00Z"}`))
	r.Header.Set("Authorization", "Bearer abc")
	w := httptest.NewRecorder()
	port.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("a short token: %d", w.Code)
	}
}

// put.sh: told to slow down, it tries again; sent elsewhere, it says it
// wrote nothing.
func TestPutRetriesAndRedirects(t *testing.T) {
	needTools(t)
	tries := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "moved"):
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		case tries == 0:
			tries++
			w.Header().Set("Retry-After", "1")
			http.Error(w, "slow down", http.StatusTooManyRequests)
		default:
			tries++
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	put := func(body string) (string, error) {
		cmd := exec.Command("sh", recipes+"put.sh", srv.URL)
		cmd.Env = append(os.Environ(), "LAPILLI_TOKEN_FILE="+tokenFile(t, "x"))
		cmd.Stdin = strings.NewReader(body)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := put(`{"incidents": [{"id": "A-1", "start": "2026-10-30T09:15:00Z"}]}`); err != nil || tries != 2 {
		t.Errorf("a 429: %v, %d tries: %s", err, tries, out)
	}
	out, err := put(`{"incidents": [{"id": "moved", "start": "2026-10-30T09:15:00Z"}]}`)
	if err == nil || !strings.Contains(out, "(302)") {
		t.Errorf("a redirect: %v: %s", err, out)
	}
	// An id from elsewhere is shown as JSON: nothing in it acts on a terminal.
	out, _ = put("{\"incidents\": [{\"id\": \"\\u001b]0;x\\u0007moved\", \"start\": \"2026-10-30T09:15:00Z\"}]}")
	if strings.ContainsAny(out, "\x1b\x07") || !strings.Contains(out, `\u001b`) {
		t.Errorf("an odd id: %q", out)
	}
}

// A tool that says there is always more is stopped, and nothing is written.
func TestRecipePageCap(t *testing.T) {
	needTools(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"incidents": [], "more": true, "pagination_meta": {"after": "same"}}`))
	}))
	defer srv.Close()
	for _, c := range []struct{ script, api, tok string }{
		{"pagerduty.sh", "PAGERDUTY_API", "PAGERDUTY_TOKEN_FILE"},
		{"incident-io.sh", "INCIDENT_IO_API", "INCIDENT_IO_TOKEN_FILE"},
	} {
		cmd := exec.Command("sh", recipes+c.script)
		cmd.Env = append(os.Environ(), c.api+"="+srv.URL, c.tok+"="+tokenFile(t, "x"), "MAX_PAGES=3")
		var stdout strings.Builder
		cmd.Stdout = &stdout
		if err := cmd.Run(); err == nil || stdout.Len() != 0 {
			t.Errorf("%s: %v, wrote %q", c.script, err, stdout.String())
		}
	}
}

// One token write at a time; and what is wrong with the incidents file stays
// on the status, and in the metrics, until the file reads cleanly.
func TestTokenOneAtATimeAndPassedOver(t *testing.T) {
	const token = "a-token-long-enough"
	file := tokenFile(t, token)
	s, _ := server(t, func(c *Config) { c.IncidentsListen, c.IncidentsToken = "127.0.0.1:0", file })
	port := s.IncidentsHandler()
	s.tokenBusy <- struct{}{}
	r := httptest.NewRequest("PUT", "/api/v1/incidents/A-1", strings.NewReader(`{"start": "2026-10-30T09:15:00Z"}`))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	port.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("a second at once: %d", w.Code)
	}
	<-s.tokenBusy

	ctx := context.Background()
	h := s.Handler()
	if code, body := api(t, h, "PUT", "/api/v1/incidents/INC-2", `{"title": "a person's", "start": "2026-11-02T06:01:00Z"}`); code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	os.WriteFile(s.cfg.Incidents, []byte("incidents:\n  - {id: INC-1, start: 2026-11-02T06:01:00Z}\n  - {id: INC-2, start: 2026-11-02T06:01:00Z}\n"), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	if st := s.statusView("en", true); !strings.Contains(st.Err, "INC-2") {
		t.Errorf("the status after a run: %+v", st)
	}
	_, body, _ := get(t, h, "/metrics")
	if !strings.Contains(body, "lapilli_serve_incidents_passed_over 1") {
		t.Errorf("no passed-over count in the metrics")
	}
	os.WriteFile(s.cfg.Incidents, []byte("incidents:\n  - {id: INC-1, start: 2026-11-02T06:01:00Z}\n"), 0o644)
	s.jobs(ctx, true)
	if st := s.statusView("en", true); st.Err != "" {
		t.Errorf("read cleanly, the status still says: %+v", st)
	}
	_, body, _ = get(t, h, "/metrics")
	if !strings.Contains(body, "lapilli_serve_incidents_passed_over 0") {
		t.Errorf("the passed-over count wasn't cleared")
	}
}

// A MAX_PAGES that isn't a number is refused, not taken for no cap.
func TestRecipeMaxPagesIsANumber(t *testing.T) {
	needTools(t)
	cmd := exec.Command("sh", recipes+"pagerduty.sh")
	cmd.Env = append(os.Environ(), "PAGERDUTY_API=http://127.0.0.1:1", "PAGERDUTY_TOKEN_FILE="+tokenFile(t, "x"), "MAX_PAGES=1k")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "MAX_PAGES is a number") {
		t.Errorf("%v: %s", err, out)
	}
}

func TestCompassRecipe(t *testing.T) {
	needTools(t)
	var logs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "ana@example.com" || pass != "compass-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		// The log of the closed alert that counts, a page at a time.
		if r.URL.Path == "/alerts/e0caa0ce-d52f-4500-81b9-d592d06970b6/logs" {
			logs = append(logs, r.URL.RawQuery)
			name := "compass-logs-1.json"
			if q.Get("after") == "1761620460958_1761620460958234593" {
				name = "compass-logs-2.json"
			}
			w.Write(fixture(t, name))
			return
		}
		if r.URL.Path != "/alerts" {
			t.Errorf("asked for %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if q.Get("from") != "1790812800" || q.Get("size") != "100" || q.Get("sort") != "createdAt" || q.Get("order") != "asc" || q.Get("query") != "tag: incident" {
			t.Errorf("query = %v", q)
		}
		name := "compass-1.json"
		if q.Get("offset") == "100" {
			name = "compass-2.json"
		}
		w.Write(fixture(t, name))
	}))
	defer srv.Close()
	got := runRecipe(t, "compass.sh", "COMPASS_API="+srv.URL, "COMPASS_CLOUD_ID=35273b54-3f06-40d2-880f-dd28cf8daafa", "COMPASS_EMAIL=ana@example.com",
		"COMPASS_TOKEN_FILE="+tokenFile(t, "compass-secret"), "SINCE=2026-10-01T09:00:00+09:00", "COMPASS_QUERY=tag: incident")
	// Closed when its log says, the latest close on either page, not at
	// its last update (03:40).
	wantIncidents(t, got,
		incident.Incident{ID: "CP-1041", Title: "Checkout errors", Start: utc("2026-10-28T03:01:00.958Z"), End: utc("2026-10-28T03:35:12Z"), Labels: incident.Labels{"service": {"checkout"}}},
		incident.Incident{ID: "CP-1043", Title: "Search timeouts", Start: utc("2026-10-30T09:15:00+09:00")})
	if len(logs) != 2 || logs[0] != "size=100" || logs[1] != "size=100&after=1761620460958_1761620460958234593" {
		t.Errorf("the log read as %q", logs)
	}
}

// Compass's log, read as an attacker might answer it: a next page is built
// from the alert's id and a cursor of digits, never followed as given; an
// alert id that isn't one stops the run; past MAX_LOGS, an alert ends at
// its last update.
func TestCompassLogsBounded(t *testing.T) {
	needTools(t)
	alert := func(id string) string {
		return `{"values": [{"id": "` + id + `", "tinyId": "7", "createdAt": "2026-10-28T03:00:00Z", "updatedAt": "2026-10-28T04:00:00Z",
			"message": "x", "status": "closed", "priority": "P1"}], "links": {}}`
	}
	var asked []string
	var listing string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.RequestURI())
		if r.URL.Path == "/alerts" {
			w.Write([]byte(listing))
			return
		}
		// Each page says there is another, at a cursor that isn't one, or
		// on another host.
		w.Write([]byte(`{"values": [{"logTime": "2026-10-28T03:30:00Z", "logType": "close", "log": "x"}],
			"links": {"next": "https://elsewhere.example/v1/alerts/a/logs?after=1;touch%20/tmp/x"}}`))
	}))
	defer srv.Close()
	run := func(env ...string) (string, string, error) {
		cmd := exec.Command("sh", recipes+"compass.sh")
		cmd.Env = append(os.Environ(), append([]string{"COMPASS_API=" + srv.URL, "COMPASS_CLOUD_ID=35273b54", "COMPASS_EMAIL=a@example.com",
			"COMPASS_TOKEN_FILE=" + tokenFile(t, "x")}, env...)...)
		var out, errb strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		return out.String(), errb.String(), err
	}

	listing = alert("a-1")
	out, stderr, err := run()
	if err != nil || !strings.Contains(out, "end: \"2026-10-28T03:30:00Z\"") && !strings.Contains(out, "2026-10-28T03:30:00Z") {
		t.Errorf("%v: %s %s", err, out, stderr)
	}
	for _, a := range asked {
		if strings.Contains(a, "touch") || strings.Contains(a, "elsewhere") || strings.Count(a, "after=") > 0 {
			t.Errorf("followed a next page as given: %s", a)
		}
	}
	if !strings.Contains(stderr, "without a cursor") {
		t.Errorf("a next page without a cursor isn't said: %s", stderr)
	}

	asked = nil
	listing = alert("../../alerts")
	if out, _, err := run(); err == nil || out != "" {
		t.Errorf("an alert id that isn't one: %v, wrote %q", err, out)
	}
	for _, a := range asked {
		if strings.Contains(a, "logs") {
			t.Errorf("read the log of an alert id that isn't one: %s", a)
		}
	}

	asked = nil
	listing = alert("a-2")
	out, stderr, err = run("MAX_LOGS=0")
	if err != nil || !strings.Contains(out, "2026-10-28T04:00:00Z") || !strings.Contains(stderr, "past MAX_LOGS") {
		t.Errorf("MAX_LOGS=0: %v: %s %s", err, out, stderr)
	}
	for _, a := range asked {
		if strings.Contains(a, "logs") {
			t.Errorf("read a log past MAX_LOGS: %s", a)
		}
	}
	if _, _, err := run("MAX_LOGS=5x"); err == nil {
		t.Errorf("MAX_LOGS that isn't a number taken")
	}
}

// Grafana IRM's incidents, as IncidentsService.QueryIncidentPreviews gives
// them (QueryIncidents, which the recipe read before, is deprecated in
// Grafana IRM's reference): newest first, as asked, the pages stopping at
// the first that reaches before SINCE, and every page without it. A
// preview has no refs: the recipe writes no alerts, as no documented API
// says which alerts an incident came from (docs/design/links.md).
func TestGrafanaIRMRecipe(t *testing.T) {
	needTools(t)
	var mu sync.Mutex
	var asked []string
	order := "DESC"
	// pages are the pages asked since the last call, the order to answer
	// in set.
	pages := func(next string) []string {
		mu.Lock()
		defer mu.Unlock()
		was := asked
		asked, order = nil, next
		return was
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer glsa_secret" || r.Method != http.MethodPost ||
			r.URL.Path != "/api/plugins/grafana-irm-app/resources/api/v1/IncidentsService.QueryIncidentPreviews" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// The request, as the reference documents it: a query of these
		// three and a cursor, nothing else (no dateFrom, no queryString,
		// which would have the rest ignored).
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		q, _ := req["query"].(map[string]any)
		if len(q) != 3 || q["limit"] != 100.0 || q["orderField"] != "createdTime" || q["orderDirection"] != "DESC" || len(req) > 2 {
			t.Errorf("request = %v", req)
		}
		name := "grafana-irm-1.json"
		if c, ok := req["cursor"].(map[string]any); ok {
			switch c["nextValue"] {
			case "123e4567-e89b-12d3-a456-426614174000":
				name = "grafana-irm-2.json"
			case "223e4567-e89b-12d3-a456-426614174001":
				name = "grafana-irm-3.json"
			default:
				t.Errorf("cursor = %v", c)
			}
		}
		mu.Lock()
		asked = append(asked, name)
		ascending := order == "ASC"
		mu.Unlock()
		page := fixture(t, name)
		if ascending && name == "grafana-irm-1.json" {
			page = []byte(strings.Replace(string(page), `"createdTime": "2026-10-31T02:40:00Z"`, `"createdTime": "2026-10-01T02:40:00Z"`, 1))
		}
		w.Write(page)
	}))
	defer srv.Close()
	env := []string{"GRAFANA_URL=" + srv.URL + "/", "GRAFANA_IRM_TOKEN_FILE=" + tokenFile(t, "glsa_secret")}
	got := runRecipe(t, "grafana-irm.sh", append(env, "SINCE=2026-10-01")...)
	wantIncidents(t, got,
		incident.Incident{ID: "IRM-41", Title: "Checkout errors", Start: utc("2026-10-28T03:01:00Z"), End: utc("2026-10-28T03:40:00Z"), Labels: incident.Labels{"service": {"checkout"}}},
		incident.Incident{ID: "IRM-43", Title: "Search timeouts", Start: utc("2026-10-30T09:15:00Z"), Labels: incident.Labels{"service": {"search", "cart"}}},
		incident.Incident{ID: "IRM-44", Title: "Batch stuck", Start: utc("2026-10-31T02:38:00Z"), End: utc("2026-10-31T03:10:20.574137Z")})
	if was := pages("DESC"); strings.Join(was, " ") != "grafana-irm-1.json grafana-irm-2.json" {
		t.Errorf("asked %v: the pages go on past SINCE", was)
	}
	// Created an instant after SINCE, as an offset and a fraction write
	// it: kept, and one before it not.
	if got := runRecipe(t, "grafana-irm.sh", append(env, "SINCE=2026-10-28T03:05:00.1Z")...); len(got) != 3 || got[0].ID != "IRM-41" {
		t.Errorf("SINCE to the second: %+v", got)
	}
	if got := runRecipe(t, "grafana-irm.sh", append(env, "SINCE=2026-10-28T12:05:00.2+09:00")...); len(got) != 2 || got[0].ID != "IRM-43" {
		t.Errorf("SINCE in another zone: %+v", got)
	}
	// A SINCE that reads as a date but isn't one is refused, not taken as
	// none.
	cmd := exec.Command("sh", recipes+"grafana-irm.sh")
	cmd.Env = append(append(os.Environ(), env...), "SINCE=2026-13-45")
	if out, err := cmd.Output(); err == nil || len(out) != 0 {
		t.Errorf("SINCE 2026-13-45: %v, wrote %q", err, out)
	}
	// Without SINCE, every page.
	pages("DESC")
	got = runRecipe(t, "grafana-irm.sh", env...)
	if was := pages("ASC"); len(got) != 5 || got[0].ID != "IRM-39" || got[1].ID != "IRM-40" || got[4].ID != "IRM-44" || len(was) != 3 {
		t.Errorf("without SINCE: asked %v, %+v", was, got)
	}
	// Not newest first, with SINCE: an incident after it might be left
	// out, so nothing is written.
	cmd = exec.Command("sh", recipes+"grafana-irm.sh")
	cmd.Env = append(append(os.Environ(), env...), "SINCE=2026-10-01")
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err == nil || stdout.Len() != 0 || !strings.Contains(stderr.String(), "not newest first") {
		t.Errorf("not newest first: %v, wrote %q, said %q", err, stdout.String(), stderr.String())
	}
}

func TestRootlyRecipe(t *testing.T) {
	needTools(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer rootly-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		// The alerts of the incidents kept, RL-41's over two pages,
		// as the documented list of an incident's alerts gives them.
		switch r.URL.Path {
		case "/v1/incidents":
		case "/v1/incidents/bbb4a6ce-4ee9-4c43-8e33-0b1c2d3e4f50/alerts":
			if q.Get("page[number]") == "2" {
				w.Write(fixture(t, "rootly-alerts-41b.json"))
			} else {
				w.Write(fixture(t, "rootly-alerts-41.json"))
			}
			return
		case "/v1/incidents/c1f0e2d3-0000-4000-8000-000000000043/alerts":
			w.Write(fixture(t, "rootly-alerts-43.json"))
			return
		default:
			t.Errorf("asked for %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if q.Get("filter[created_at][gte]") != "2026-10-01" || q.Get("page[size]") != "100" {
			t.Errorf("query = %v", q)
		}
		name := "rootly-1.json"
		if q.Get("page[number]") == "2" {
			name = "rootly-2.json"
		}
		w.Write(fixture(t, name))
	}))
	defer srv.Close()
	got, said := runRecipeSaid(t, "rootly.sh", "ROOTLY_API="+srv.URL, "ROOTLY_TOKEN_FILE="+tokenFile(t, "rootly-secret"), "SINCE=2026-10-01")
	wantIncidents(t, got,
		incident.Incident{ID: "RL-41", Title: "Checkout errors", Start: utc("2026-10-28T03:01:00Z"), End: utc("2026-10-28T03:40:00Z"), Labels: incident.Labels{"service": {"checkout", "cart"}},
			Alerts: []incident.Alert{{Rule: "CheckoutErrors", Labels: map[string]string{"instance": "checkout-1:9090", "job": "checkout"}}, {Rule: "CheckoutLatency", Labels: map[string]string{"job": "checkout"}}}},
		incident.Incident{ID: "RL-43", Title: "Search timeouts", Start: utc("2026-10-30T09:15:00Z"), Labels: incident.Labels{"service": {"search"}}})
	if want := "rootly.sh: left out 1 of the 3 alerts linked to the incidents: their labels carry no alertname\n"; said != want {
		t.Errorf("said %q, want %q", said, want)
	}
}

func TestFireHydrantRecipe(t *testing.T) {
	needTools(t)
	var mu sync.Mutex
	limited := 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fhb-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// The first request answered as its reference says a rate
		// limited one is, its wait of eight seconds written 08.
		mu.Lock()
		limit := limited > 0
		limited--
		mu.Unlock()
		if limit {
			w.Header().Set("RateLimit-Limit", "50")
			w.Header().Set("Retry-After", "08")
			http.Error(w, `{"error": "rate limit exceeded"}`, http.StatusTooManyRequests)
			return
		}
		q := r.URL.Query()
		// The alerts attached to the incidents kept, FH-41's over two
		// pages, as the documented list of an incident's alerts gives
		// them: one from Alertmanager's source, its labels as it names
		// them, one by email, and one with alertname as it is.
		switch r.URL.Path {
		case "/v1/incidents":
		case "/v1/incidents/6f3c2a10-1b2c-4d5e-8f90-a1b2c3d4e5f6/alerts":
			if q.Get("page") == "2" {
				w.Write(fixture(t, "firehydrant-alerts-41b.json"))
			} else {
				w.Write(fixture(t, "firehydrant-alerts-41.json"))
			}
			return
		case "/v1/incidents/8b5e4c32-3d4e-4f60-ab12-c3d4e5f6a7b8/alerts":
			w.Write(fixture(t, "firehydrant-alerts-43.json"))
			return
		default:
			t.Errorf("asked for %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if q.Get("start_date") != "2026-10-01" || q.Get("per_page") != "200" {
			t.Errorf("query = %v", q)
		}
		name := "firehydrant-1.json"
		if q.Get("page") == "2" {
			name = "firehydrant-2.json"
		}
		w.Write(fixture(t, name))
	}))
	defer srv.Close()
	path, slept := fakeSleep(t)
	env := []string{"FIREHYDRANT_API=" + srv.URL, "FIREHYDRANT_TOKEN_FILE=" + tokenFile(t, "fhb-secret"), "SINCE=2026-10-01", path}
	got, said := runRecipeSaid(t, "firehydrant.sh", env...)
	if got := sleeps(t, slept); got != "8" {
		t.Errorf("slept %q, want 8", got)
	}
	wantIncidents(t, got,
		incident.Incident{ID: "FH-41", Title: "Checkout errors", Start: utc("2026-10-28T03:01:00Z"), End: utc("2026-10-28T03:40:00Z"), Labels: incident.Labels{"service": {"checkout"}},
			Alerts: []incident.Alert{{Rule: "CheckoutErrors", Labels: map[string]string{"service": "checkout", "severity": "critical"}}, {Rule: "CheckoutLatency", Labels: map[string]string{"job": "checkout"}}}},
		incident.Incident{ID: "FH-43", Title: "Search timeouts", Start: utc("2026-10-30T09:15:00Z"), End: utc("2026-10-30T09:50:00Z")})
	if want := "firehydrant.sh: left out 1 of the 3 alerts linked to the incidents: their labels carry no alertname\n"; said != want {
		t.Errorf("said %q, want %q", said, want)
	}
	got = runRecipe(t, "firehydrant.sh", append(env, "ALERT_LABELS=service")...)
	if a := got[0].Alerts; len(a) != 2 || len(a[0].Labels) != 1 || a[0].Labels["service"] != "checkout" || a[1].Labels != nil {
		t.Errorf("ALERT_LABELS=service: %+v", a)
	}
	got = runRecipe(t, "firehydrant.sh", append(env, "ALERTS=no")...)
	if len(got) != 2 || got[0].Alerts != nil {
		t.Errorf("ALERTS=no: %+v", got)
	}
	// Past MAX_ALERT_REQUESTS, nothing is written.
	cmd := exec.Command("sh", recipes+"firehydrant.sh")
	cmd.Env = append(os.Environ(), append(env, "MAX_ALERT_REQUESTS=2")...)
	if out, err := cmd.Output(); err == nil || len(out) != 0 {
		t.Errorf("past MAX_ALERT_REQUESTS: %v, wrote %q", err, out)
	}

	// Five waits a request, and ten minutes in all a run, though each
	// page answers after its 429: past them, nothing is written.
	for _, c := range []struct {
		name, wait string
		every      int
		sleeps     string
	}{
		{"five waits", "", 0, "5 10 20 40 80"},
		{"ten minutes", "300", 2, "120 120 120 120 120"},
	} {
		asked := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			n := asked
			asked++
			mu.Unlock()
			if c.every == 0 || n%c.every == 0 {
				if c.wait != "" {
					w.Header().Set("Retry-After", c.wait)
				}
				http.Error(w, `{"error": "rate limit exceeded"}`, http.StatusTooManyRequests)
				return
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			fmt.Fprintf(w, `{"data": [], "pagination": {"page": %d, "next": %d}}`, page, max(page, 1)+1)
		}))
		path, slept := fakeSleep(t)
		cmd := exec.Command("sh", recipes+"firehydrant.sh")
		cmd.Env = append(os.Environ(), "FIREHYDRANT_API="+srv.URL, "FIREHYDRANT_TOKEN_FILE="+tokenFile(t, "fhb-secret"), path)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		srv.Close()
		if got := sleeps(t, slept); err == nil || stdout.Len() != 0 || got != c.sleeps ||
			!strings.Contains(stderr.String(), "still says too many requests after 5 waits") {
			t.Errorf("%s: %v, slept %q, wrote %q, said %q", c.name, err, got, stdout.String(), stderr.String())
		}
	}
}

// The newer recipes, alike: a SINCE that isn't a time is refused before any
// request; a page that fails, a tool that always has more, and Grafana
// IRM's 200 that says it failed, each stop the script with nothing written.
func TestNewRecipesFailLoudly(t *testing.T) {
	needTools(t)
	type recipe struct {
		script string
		env    func(url string) []string
		// more answers a page that says there is a next one, the same
		// next one each time.
		more string
	}
	newer := []recipe{
		{"compass.sh", func(u string) []string {
			return []string{"COMPASS_API=" + u, "COMPASS_CLOUD_ID=35273b54", "COMPASS_EMAIL=a@example.com", "COMPASS_TOKEN_FILE=" + tokenFile(t, "x")}
		}, `{"values": [], "links": {"next": "/v1/alerts?offset=100&size=100"}}`},
		{"grafana-irm.sh", func(u string) []string {
			return []string{"GRAFANA_URL=" + u, "GRAFANA_IRM_TOKEN_FILE=" + tokenFile(t, "x")}
		}, `{"incidentPreviews": [], "cursor": {"hasMore": true, "nextValue": "same"}, "error": ""}`},
		{"rootly.sh", func(u string) []string {
			return []string{"ROOTLY_API=" + u, "ROOTLY_TOKEN_FILE=" + tokenFile(t, "x")}
		}, `{"data": [], "links": {}, "meta": {"current_page": 1, "next_page": 2, "prev_page": null, "total_count": 0, "total_pages": 9}}`},
		{"firehydrant.sh", func(u string) []string {
			return []string{"FIREHYDRANT_API=" + u, "FIREHYDRANT_TOKEN_FILE=" + tokenFile(t, "x")}
		}, `{"data": [], "pagination": {"next": 2}}`},
	}
	run := func(script string, env []string) (string, error) {
		cmd := exec.Command("sh", recipes+script)
		cmd.Env = append(os.Environ(), env...)
		var stdout strings.Builder
		cmd.Stdout = &stdout
		err := cmd.Run()
		return stdout.String(), err
	}
	for _, c := range newer {
		asked := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			asked++
			http.Error(w, "down", http.StatusInternalServerError)
		}))
		for _, since := range []string{"yesterday", "2026-10-01; x", "2026-10-01\n2026-10-02", "2026-10-01T00:00:00"} {
			if out, err := run(c.script, append(c.env(srv.URL), "SINCE="+since)); err == nil || out != "" {
				t.Errorf("%s: SINCE %q: %v, wrote %q", c.script, since, err, out)
			}
		}
		if asked != 0 {
			t.Errorf("%s: %d requests with a SINCE that isn't a time", c.script, asked)
		}
		srv.Close()

		pages := 0
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			pages++
			if pages > 1 {
				http.Error(w, "down", http.StatusInternalServerError)
				return
			}
			w.Write([]byte(c.more))
		}))
		if out, err := run(c.script, c.env(srv.URL)); err == nil || out != "" {
			t.Errorf("%s: page 2 failing: %v, wrote %q", c.script, err, out)
		}
		srv.Close()

		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(c.more)) }))
		if out, err := run(c.script, append(c.env(srv.URL), "MAX_PAGES=3")); err == nil || out != "" {
			t.Errorf("%s: always more: %v, wrote %q", c.script, err, out)
		}
		srv.Close()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"incidentPreviews": [], "cursor": {"hasMore": false}, "error": "\u001b]0;x\u0007permission denied"}`))
	}))
	defer srv.Close()
	cmd := exec.Command("sh", recipes+"grafana-irm.sh")
	cmd.Env = append(os.Environ(), "GRAFANA_URL="+srv.URL, "GRAFANA_IRM_TOKEN_FILE="+tokenFile(t, "x"))
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err == nil || stdout.Len() != 0 || !strings.Contains(stderr.String(), "permission denied") || strings.ContainsAny(stderr.String(), "\x1b\x07") {
		t.Errorf("Grafana IRM's error: %v, wrote %q, said %q", err, stdout.String(), stderr.String())
	}
}

// TestRecipesCurlReadsNoCurlrc: every curl a recipe, or a script the chart
// ships, runs reads no .curlrc (-q, which counts only as curl's first
// argument): one in the job's HOME could send what it asks, and the token
// it is given, through a proxy of its choosing.
func TestRecipesCurlReadsNoCurlrc(t *testing.T) {
	// curl as a word followed by a flag, wherever it is in a line: after
	// a pipe, $(, {, if, !, command, env, timeout N or a backtick alike;
	// prose ("curl isn't installed", "command -v curl") gives it none.
	call := regexp.MustCompile(`(?:^|[^A-Za-z0-9_./$-])curl\s+(-\S*)`)
	n := 0
	for _, dir := range []string{"../../recipes", "../../charts/lapilli/files"} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".sh") {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "#") {
					continue
				}
				for _, m := range call.FindAllStringSubmatch(line, -1) {
					n++
					if strings.TrimSpace(m[1]) != "-q" {
						t.Errorf("%s:%d: curl without -q first: %s", path, i+1, strings.TrimSpace(line))
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if n < 20 {
		t.Errorf("found %d curls: the pattern finds too few", n)
	}
}
