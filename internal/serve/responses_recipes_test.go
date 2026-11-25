package serve

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const responseRecipes = "../../recipes/responses/"

// responseFixture is a file from the response recipes' test data, written
// from what each tool's published schema or reference gives.
func responseFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(responseRecipes + "testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// runResponses runs a response recipe with an environment and gives what it
// writes and what it says on stderr; it fails the test when the recipe
// fails.
func runResponses(t *testing.T, script string, env ...string) (string, string) {
	t.Helper()
	out, said, err := runResponsesErr(script, env...)
	if err != nil {
		t.Fatalf("%s: %v\n%s%s", script, err, out, said)
	}
	return out, said
}

func runResponsesErr(script string, env ...string) (string, string, error) {
	cmd := exec.Command("sh", responseRecipes+script)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// wantResponses holds what a recipe wrote to exactly the JSON wanted: the
// same keys and values, nothing more, in the same order of pages. The
// loader of the responses file is another package's; this is the file's
// format, from docs/design/responses.md.
func wantResponses(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("what was written isn't JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g, w) {
		a, _ := json.MarshalIndent(g, "", " ")
		b, _ := json.MarshalIndent(w, "", " ")
		t.Errorf("wrote\n%s\nwant\n%s", a, b)
	}
}

// noPeople fails when a recipe's output names a person: the tests' fixtures
// carry names and ids of users, and none may get through.
func noPeople(t *testing.T, out string, names ...string) {
	t.Helper()
	for _, n := range names {
		if strings.Contains(out, n) {
			t.Errorf("the output names %q: %s", n, out)
		}
	}
}

const responsesPeriod = "2026-11-20T00:00:00Z"
const responsesUntil = "2026-11-21T00:00:00Z"

func TestPagerDutyResponses(t *testing.T) {
	parallel(t)
	needTools(t)
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Token token=pd-secret" || !strings.Contains(r.Header.Get("Accept"), "pagerduty+json;version=2") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		asked = append(asked, r.URL.Path+"?"+q.Get("offset"))
		// The incidents of a period over two pages, each one's alerts and
		// log entries, and the first one's log over two pages too.
		alerts := map[string]string{"1": "1", "2": "2", "3": "3", "4": "4", "6": "6"}
		switch {
		case r.URL.Path == "/incidents":
			if q.Get("since") != responsesPeriod || q.Get("until") != responsesUntil || q.Get("limit") != "100" {
				t.Errorf("query = %v", q)
			}
			if q.Get("offset") == "100" {
				w.Write(responseFixture(t, "pagerduty-2.json"))
			} else {
				w.Write(responseFixture(t, "pagerduty-1.json"))
			}
		case strings.HasSuffix(r.URL.Path, "/alerts"):
			n := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/incidents/Q2RESP0000000"), "/alerts")
			if _, ok := alerts[n]; !ok {
				t.Errorf("asked for %s", r.URL.Path)
				http.NotFound(w, r)
				return
			}
			w.Write(responseFixture(t, "pagerduty-alerts-"+n+".json"))
		case strings.HasSuffix(r.URL.Path, "/log_entries"):
			n := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/incidents/Q2RESP0000000"), "/log_entries")
			switch {
			case n == "1" && q.Get("offset") == "100":
				w.Write(responseFixture(t, "pagerduty-log-entries-1b.json"))
			case n == "1":
				w.Write(responseFixture(t, "pagerduty-log-entries-1a.json"))
			case n == "6":
				w.Write([]byte(`{"log_entries": [], "limit": 100, "offset": 0, "more": false, "total": null}`))
			default:
				w.Write(responseFixture(t, "pagerduty-log-entries-"+n+".json"))
			}
		default:
			t.Errorf("asked for %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	env := []string{"PAGERDUTY_API=" + srv.URL, "PAGERDUTY_TOKEN_FILE=" + tokenFile(t, "pd-secret"), "SINCE=" + responsesPeriod, "UNTIL=" + responsesUntil}
	out, said := runResponses(t, "pagerduty.sh", env...)
	wantResponses(t, out, `{"responses": [
	  {"id": "PD-201", "alerts": [{"rule": "KubePodCrashLooping", "labels": {"namespace": "shop", "pod": "cart-7f9c"}}],
	   "sent": "2026-11-20T03:12:00Z", "acknowledged": "2026-11-20T03:14:00Z", "resolved": "2026-11-20T03:20:00Z", "resolved_by": "alert"},
	  {"id": "PD-202", "alerts": [{"rule": "NodeDiskFilling", "labels": {"instance": "node-4", "service": "search"}}],
	   "sent": "2026-11-20T04:00:00Z", "resolved": "2026-11-20T04:04:00Z", "resolved_by": "alert"},
	  {"id": "PD-203", "alerts": [{"rule": "CartLatency", "labels": {"namespace": "shop", "service": "cart"}}],
	   "sent": "2026-11-20T05:00:00Z", "resolved": "2026-11-20T05:30:00Z", "resolved_by": "person"},
	  {"id": "PD-204", "alerts": [{"rule": "SearchTimeouts", "labels": {"namespace": "shop", "service": "search"}}],
	   "sent": "2026-11-20T09:15:00Z"}
	]}`)
	// The log entries' ids and agents, which name who acted, are not in it.
	noPeople(t, out, "PAGENT", "an agent", "user_reference")
	if want := "pagerduty.sh: left out 1 of the 5 alerts linked to the incidents: their details carry no alertname\n" +
		"pagerduty.sh: left out 1 of the 5 incidents: no alert Lapilli takes\n"; said != want {
		t.Errorf("said %q, want %q", said, want)
	}
	// The first incident's log read over its two pages, and the one before
	// the period and the low-urgency one not asked for at all.
	joined := strings.Join(asked, " ")
	for _, want := range []string{"/incidents/Q2RESP00000001/log_entries?0", "/incidents/Q2RESP00000001/log_entries?100"} {
		if !strings.Contains(joined, want) {
			t.Errorf("never asked for %s: %s", want, joined)
		}
	}
	for _, not := range []string{"Q2RESP00000005", "Q2RESP00000007"} {
		if strings.Contains(joined, not) {
			t.Errorf("asked for %s: %s", not, joined)
		}
	}
	// ALERT_LABELS keeps the labels it names alone.
	out, _ = runResponses(t, "pagerduty.sh", append(env, "ALERT_LABELS=service")...)
	if !strings.Contains(strings.Join(strings.Fields(out), ""), `{"rule":"NodeDiskFilling","labels":{"service":"search"}}`) {
		t.Errorf("ALERT_LABELS=service: %s", out)
	}
	// Past MAX_REQUESTS, nothing is written.
	stdout, _, err := runResponsesErr("pagerduty.sh", append(env, "MAX_REQUESTS=3")...)
	if err == nil || stdout != "" {
		t.Errorf("past MAX_REQUESTS: %v, wrote %q", err, stdout)
	}
}

// opsAlerts answers an operations API's alerts and their logs: the
// alerts a page at a time (list at the offset), and each alert's log by its
// id, a page of it by the cursor it was asked for. A request for anything
// else fails the test.
type opsStandIn struct {
	t      *testing.T
	auth   func(r *http.Request) bool
	alerts func(offset string) []byte
	// logs gives the log pages of an alert id by cursor ("" the first), or
	// nil for one not to be asked for.
	logs  func(id, after string) []byte
	mu    sync.Mutex
	asked []string
	query []url.Values
}

func (s *opsStandIn) handler(base string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.auth(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		s.mu.Lock()
		path := strings.TrimPrefix(r.URL.Path, base)
		s.asked = append(s.asked, path+"?"+q.Get("offset")+q.Get("after"))
		if path == "/alerts" {
			s.query = append(s.query, q)
		}
		s.mu.Unlock()
		switch {
		case path == "/alerts":
			w.Write(s.alerts(q.Get("offset")))
		case strings.HasPrefix(path, "/alerts/") && strings.HasSuffix(path, "/logs"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/alerts/"), "/logs")
			b := s.logs(id, q.Get("after"))
			if b == nil {
				s.t.Errorf("asked for the log of %s", id)
				http.NotFound(w, r)
				return
			}
			w.Write(b)
		default:
			s.t.Errorf("asked for %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}
}

func (s *opsStandIn) askedFor() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.asked, " ")
}

func periodSeconds(t *testing.T) (string, string) {
	t.Helper()
	return strconv.FormatInt(utc(responsesPeriod).Unix(), 10), strconv.FormatInt(utc(responsesUntil).Unix(), 10)
}

func TestJSMResponses(t *testing.T) {
	parallel(t)
	needTools(t)
	const cloud = "35273b54-3f06-40d2-880f-dd28cf8daafa"
	const id = "a1000000-0000-4000-8000-%012d"
	stand := &opsStandIn{t: t,
		auth: func(r *http.Request) bool {
			u, p, ok := r.BasicAuth()
			return ok && u == "ana@example.com" && p == "jsm-secret"
		},
		alerts: func(offset string) []byte {
			if offset == "100" {
				return responseFixture(t, "jsm-alerts-2.json")
			}
			return responseFixture(t, "jsm-alerts-1.json")
		},
		logs: func(alert, after string) []byte {
			switch {
			case alert == fmt.Sprintf(id, 1) && after == "":
				return responseFixture(t, "jsm-logs-1a.json")
			case alert == fmt.Sprintf(id, 1) && after == "1763608800958_1763608800958234593":
				return responseFixture(t, "jsm-logs-1b.json")
			case alert == fmt.Sprintf(id, 2) && after == "":
				return responseFixture(t, "jsm-logs-2.json")
			case alert == fmt.Sprintf(id, 3) && after == "":
				return responseFixture(t, "jsm-logs-3.json")
			case alert == fmt.Sprintf(id, 5) && after == "":
				return responseFixture(t, "jsm-logs-5.json")
			}
			return nil
		}}
	srv := httptest.NewServer(stand.handler("/jsm/ops/api/" + cloud + "/v1"))
	defer srv.Close()
	env := []string{"JSM_API=" + srv.URL, "JSM_CLOUD_ID=" + cloud, "JIRA_EMAIL=ana@example.com", "JIRA_TOKEN_FILE=" + tokenFile(t, "jsm-secret"),
		"SINCE=" + responsesPeriod, "UNTIL=" + responsesUntil, "JSM_QUERY=tag: prometheus"}
	out, said := runResponses(t, "jsm.sh", env...)
	wantResponses(t, out, `{"responses": [
	  {"id": "JSM-3001", "alerts": [{"rule": "KubePodCrashLooping", "labels": {"namespace": "shop", "pod": "cart-7f9c"}}],
	   "sent": "2026-11-20T03:12:00.958Z", "acknowledged": "2026-11-20T03:15:00.958Z", "resolved": "2026-11-20T03:20:00.958Z", "resolved_by": "alert"},
	  {"id": "JSM-3002", "alerts": [{"rule": "NodeDiskFilling", "labels": {"instance": "node-4", "service": "search"}}],
	   "sent": "2026-11-20T04:00:00Z", "resolved": "2026-11-20T04:04:00Z", "resolved_by": "alert"},
	  {"id": "JSM-3003", "alerts": [{"rule": "CartLatency", "labels": {"namespace": "shop", "service": "cart"}}],
	   "sent": "2026-11-20T05:00:00Z", "resolved": "2026-11-20T05:30:00Z", "resolved_by": "person"},
	  {"id": "JSM-3004", "alerts": [{"rule": "SearchTimeouts", "labels": {"namespace": "shop", "service": "search"}}],
	   "sent": "2026-11-20T09:15:00Z"},
	  {"id": "JSM-3005", "alerts": [{"rule": "QueueBacklog", "labels": {"queue": "mail"}}],
	   "sent": "2026-11-20T10:00:00Z", "acknowledged": "2026-11-20T10:07:00Z"}
	]}`)
	noPeople(t, out, "Ana", "Ben", "Cy Park", "Kim", "Lee", "owner")
	if want := "jsm.sh: left out 1 of the 6 alerts: their extra properties carry no alertname\n"; said != want {
		t.Errorf("said %q, want %q", said, want)
	}
	// The period as seconds and the search as given; the first alert's log
	// read over its two pages; no log read for an alert nobody acknowledged
	// that is open, one Lapilli doesn't take, or one before the period.
	from, to := periodSeconds(t)
	if len(stand.query) != 2 {
		t.Errorf("the alerts asked in %d requests", len(stand.query))
	}
	for _, q := range stand.query {
		if q.Get("from") != from || q.Get("to") != to || q.Get("size") != "100" || q.Get("sort") != "createdAt" || q.Get("order") != "asc" || q.Get("query") != "tag: prometheus" {
			t.Errorf("query = %v", q)
		}
	}
	asked := stand.askedFor()
	for _, want := range []string{"/alerts?0", "/alerts?100", "/alerts/" + fmt.Sprintf(id, 1) + "/logs?1763608800958_1763608800958234593"} {
		if !strings.Contains(asked, want) {
			t.Errorf("never asked for %s: %s", want, asked)
		}
	}
	for _, n := range []int{4, 6, 7} {
		if strings.Contains(asked, fmt.Sprintf(id, n)) {
			t.Errorf("asked for the log of alert %d: %s", n, asked)
		}
	}
	// Past MAX_LOGS, nothing is written.
	stdout, said, err := runResponsesErr("jsm.sh", append(env, "MAX_LOGS=3")...)
	if err == nil || stdout != "" || !strings.Contains(said, "more than 3 alerts to read the logs of") {
		t.Errorf("past MAX_LOGS: %v, wrote %q, said %q", err, stdout, said)
	}
}

// A 429 from the operations API is waited out as its Retry-After says, and
// the run goes on.
func TestJSMResponsesWaitOutTooMany(t *testing.T) {
	parallel(t)
	needTools(t)
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			http.Error(w, "too many", http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"values": [], "links": {}}`))
	}))
	defer srv.Close()
	path, slept := fakeSleep(t)
	out, _ := runResponses(t, "jsm.sh", "JSM_API="+srv.URL, "JSM_CLOUD_ID=35273b54", "JIRA_EMAIL=ana@example.com", "JIRA_TOKEN_FILE="+tokenFile(t, "x"),
		"SINCE="+responsesPeriod, "UNTIL="+responsesUntil, path)
	wantResponses(t, out, `{"responses": []}`)
	if got := sleeps(t, slept); got != "7" {
		t.Errorf("slept %q, want 7", got)
	}
}

func TestCompassResponses(t *testing.T) {
	parallel(t)
	needTools(t)
	const cloud = "35273b54-3f06-40d2-880f-dd28cf8daafa"
	const id = "c1000000-0000-4000-8000-%012d"
	stand := &opsStandIn{t: t,
		auth: func(r *http.Request) bool {
			u, p, ok := r.BasicAuth()
			return ok && u == "ana@example.com" && p == "compass-secret"
		},
		alerts: func(offset string) []byte {
			if offset == "100" {
				return responseFixture(t, "compass-alerts-2.json")
			}
			return responseFixture(t, "compass-alerts-1.json")
		},
		logs: func(alert, after string) []byte {
			switch {
			case alert == fmt.Sprintf(id, 1) && after == "":
				return responseFixture(t, "compass-logs-1a.json")
			case alert == fmt.Sprintf(id, 1) && after == "1763608320000_1763608320000234593":
				return responseFixture(t, "compass-logs-1b.json")
			case alert == fmt.Sprintf(id, 2) && after == "":
				return responseFixture(t, "compass-logs-2.json")
			case alert == fmt.Sprintf(id, 3) && after == "":
				return responseFixture(t, "compass-logs-3.json")
			}
			return nil
		}}
	srv := httptest.NewServer(stand.handler(""))
	defer srv.Close()
	env := []string{"COMPASS_API=" + srv.URL, "COMPASS_CLOUD_ID=" + cloud, "COMPASS_EMAIL=ana@example.com", "COMPASS_TOKEN_FILE=" + tokenFile(t, "compass-secret"),
		"SINCE=" + responsesPeriod, "UNTIL=" + responsesUntil}
	out, said := runResponses(t, "compass.sh", env...)
	// The first was acknowledged by a person, and closed by "System", its
	// log's latest close, after a reopening; the second never acknowledged,
	// closed by "System"; the third acknowledged and closed by a person,
	// in a zone that isn't UTC; the fourth is open.
	wantResponses(t, out, `{"responses": [
	  {"id": "CP-4001", "alerts": [{"rule": "KubePodCrashLooping", "labels": {"namespace": "shop", "pod": "cart-7f9c"}}],
	   "sent": "2026-11-20T03:12:00Z", "acknowledged": "2026-11-20T03:15:00Z", "resolved": "2026-11-20T03:35:12.500Z", "resolved_by": "alert"},
	  {"id": "CP-4002", "alerts": [{"rule": "NodeDiskFilling", "labels": {"instance": "node-4"}}],
	   "sent": "2026-11-20T04:00:00Z", "resolved": "2026-11-20T04:05:00Z", "resolved_by": "alert"},
	  {"id": "CP-4003", "alerts": [{"rule": "CartLatency", "labels": {"service": "cart"}}],
	   "sent": "2026-11-20T14:00:00+09:00", "acknowledged": "2026-11-20T14:10:00+09:00", "resolved": "2026-11-20T14:30:00+09:00", "resolved_by": "person"},
	  {"id": "CP-4004", "alerts": [{"rule": "SearchTimeouts", "labels": {"service": "search"}}], "sent": "2026-11-20T09:15:00Z"}
	]}`)
	noPeople(t, out, "Ana", "owner")
	if said != "" {
		t.Errorf("said %q", said)
	}
	asked := stand.askedFor()
	for _, want := range []string{"/alerts?0", "/alerts?100", "/alerts/" + fmt.Sprintf(id, 1) + "/logs?1763608320000_1763608320000234593"} {
		if !strings.Contains(asked, want) {
			t.Errorf("never asked for %s: %s", want, asked)
		}
	}
	if strings.Contains(asked, fmt.Sprintf(id, 4)) {
		t.Errorf("asked for the log of an open alert nobody acknowledged: %s", asked)
	}
}

func TestGrafanaIRMResponses(t *testing.T) {
	parallel(t)
	needTools(t)
	var mu sync.Mutex
	var queries []url.Values
	var headers []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer glsa_oncall_secret" || r.Header.Get("X-Grafana-URL") != "https://example.grafana.net" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/oncall/api/v1/alert_groups/" {
			t.Errorf("asked for %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		queries = append(queries, r.URL.Query())
		headers = append(headers, r.Header)
		mu.Unlock()
		if r.URL.Query().Get("page") == "2" {
			w.Write(responseFixture(t, "grafana-irm-oncall-2.json"))
			return
		}
		w.Write(responseFixture(t, "grafana-irm-oncall-1.json"))
	}))
	defer srv.Close()
	out, said := runResponses(t, "grafana-irm.sh", "GRAFANA_ONCALL_API="+srv.URL+"/oncall", "GRAFANA_URL=https://example.grafana.net",
		"GRAFANA_IRM_TOKEN_FILE="+tokenFile(t, "glsa_oncall_secret"), "SINCE="+responsesPeriod, "UNTIL="+responsesUntil)
	// Only what a user did is written: a resolve whose resolved_by is null
	// is not said to be the alert source's, and an acknowledgement whose
	// acknowledged_by is null is not a person's.
	wantResponses(t, out, `{"responses": [
	  {"id": "IRM-I1AAAAAAAAAA1", "alerts": [{"rule": "KubePodCrashLooping", "labels": {"namespace": "shop", "pod": "cart-7f9c"}}],
	   "sent": "2026-11-20T03:12:00.429805Z", "acknowledged": "2026-11-20T03:15:00.429805Z"},
	  {"id": "IRM-I2AAAAAAAAAA2", "alerts": [{"rule": "NodeDiskFilling", "labels": {"instance": "node-4", "service": "search"}}],
	   "sent": "2026-11-20T04:00:00Z"},
	  {"id": "IRM-I3AAAAAAAAAA3", "alerts": [{"rule": "CartLatency", "labels": {"namespace": "shop", "service": "cart"}}],
	   "sent": "2026-11-20T05:00:00Z", "resolved": "2026-11-20T05:30:00Z", "resolved_by": "person"},
	  {"id": "IRM-I4AAAAAAAAAA4", "alerts": [{"rule": "SearchTimeouts", "labels": {"namespace": "shop", "service": "search"}}],
	   "sent": "2026-11-20T09:15:00Z"},
	  {"id": "IRM-I5AAAAAAAAAA5", "alerts": [{"rule": "QueueBacklog", "labels": {"queue": "mail"}}],
	   "sent": "2026-11-20T10:00:00Z"},
	  {"id": "IRM-I6AAAAAAAAAA6", "alerts": [{"rule": "PaymentsDown", "labels": {"service": "payments"}}],
	   "sent": "2026-11-20T11:00:00Z", "acknowledged": "2026-11-20T11:02:00Z", "resolved": "2026-11-20T11:40:00Z", "resolved_by": "person"}
	]}`)
	noPeople(t, out, "UCGEIXI1MR1NZ", "UQ2WVHZPT6KD4", "acknowledged_by")
	if want := "grafana-irm.sh: left out 1 of the 7 alert groups: their last alert's payload carries no alert with an alertname\n" +
		"grafana-irm.sh: 2 resolved alert groups have no user who resolved them, which does not say the alert source did: kept with no resolve\n"; said != want {
		t.Errorf("said %q, want %q", said, want)
	}
	// Both pages, by number, over the period in UTC to the second.
	if len(queries) != 2 || queries[0].Get("page") != "1" || queries[1].Get("page") != "2" {
		t.Fatalf("queries %v", queries)
	}
	if got, want := queries[0].Get("started_at"), "2026-11-20T00:00:00_2026-11-20T23:59:59"; got != want {
		t.Errorf("started_at = %q, want %q", got, want)
	}
	if headers[0].Get("Content-Type") != "application/json" {
		t.Errorf("headers %v", headers[0])
	}
}

// responsesPeriodEnv is the period every response recipe is given.
var responsesPeriodEnv = []string{"SINCE=" + responsesPeriod, "UNTIL=" + responsesUntil}

// put.sh sends the file to serve's responses of its source for the
// period, with its token as a bearer; what it is given that isn't a list of
// responses is refused before anything is sent.
func TestResponsesPutSendsThePeriod(t *testing.T) {
	parallel(t)
	needTools(t)
	var mu sync.Mutex
	var method, path, auth, ctype, body string
	var query url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		method, path, auth, ctype, body, query = r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(b), r.URL.Query()
		mu.Unlock()
		w.Write([]byte(`{"source": "pagerduty-prod", "responses": 1}`))
	}))
	defer srv.Close()
	put := func(stdin string, env ...string) (string, error) {
		cmd := exec.Command("sh", responseRecipes+"put.sh", srv.URL, "pagerduty-prod")
		cmd.Env = append(os.Environ(), append([]string{"LAPILLI_TOKEN_FILE=" + tokenFile(t, "serve secret token"),
			"SINCE=" + responsesPeriod, "UNTIL=" + responsesUntil}, env...)...)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	file := `{"responses": [{"id": "PD-201", "alerts": [{"rule": "KubePodCrashLooping"}], "sent": "2026-11-20T03:12:00Z"}]}`
	said, err := put(file)
	mu.Lock()
	defer mu.Unlock()
	if err != nil || method != "PUT" || path != "/api/v1/responses/pagerduty-prod" || auth != "Bearer serve secret token" || ctype != "application/json" || body != file ||
		query.Get("from") != responsesPeriod || query.Get("to") != responsesUntil {
		t.Errorf("%v: %s %s?%v, Authorization %q, %q, body %q; said %q", err, method, path, query, auth, ctype, body, said)
	}
	if !strings.Contains(said, `"pagerduty-prod": 1 responses written (200)`) {
		t.Errorf("said %q", said)
	}
	method = ""
	for _, bad := range []string{"", "not json", `{"incidents": []}`, `{"responses": [{"id": "x"}]}`, `{"responses": ["x"]}`, `[]`} {
		said, err := put(bad)
		if err == nil || method != "" || !strings.Contains(said, "want JSON on stdin, with a list of responses") {
			t.Errorf("%q: %v, sent %q, said %q", bad, err, method, said)
		}
	}
	// A source that isn't lowercase letters, digits and dashes, and a
	// period that isn't whole seconds, are refused as the firings' are.
	cmd := exec.Command("sh", responseRecipes+"put.sh", srv.URL, "Pagerduty_Prod")
	cmd.Env = append(os.Environ(), "LAPILLI_TOKEN_FILE="+tokenFile(t, "x"), "SINCE="+responsesPeriod, "UNTIL="+responsesUntil)
	cmd.Stdin = strings.NewReader(file)
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "a source is lowercase letters, digits and dashes") || method != "" {
		t.Errorf("a bad source: %v, %q", err, out)
	}
	if said, err := put(file, "SINCE=2026-11-20"); err == nil || !strings.Contains(said, "is not a time in whole seconds") || method != "" {
		t.Errorf("a bad period: %v, %q", err, said)
	}
}

// PagerDuty's log entries, read at their edges: an acknowledgement by a
// service or an integration is no person's, and the schema's other
// spelling of an acknowledgement's type is taken; a resolve whose agent is
// none of the three types the schema gives is left out, said by a count; an
// incident that was resolved and then triggered again is open, whatever its
// log says of the first resolve; the earliest acknowledgement counts
// whatever order the entries come in.
func TestPagerDutyResponsesEdges(t *testing.T) {
	parallel(t)
	needTools(t)
	entry := func(typ, at, agent string) string {
		return fmt.Sprintf(`{"id": "R", "type": %q, "created_at": %q, "agent": {"id": "PX", "type": %q}}`, typ, at, agent)
	}
	incident := func(n int, status string) string {
		return fmt.Sprintf(`{"id": "Q2EDGE000000%d", "incident_number": %d, "title": "x", "created_at": "2026-11-20T0%d:00:00Z", "status": %q, "urgency": "high"}`, n, 300+n, n, status)
	}
	logs := map[string]string{
		"1": entry("acknowledge_log_entry", "2026-11-20T01:01:00Z", "service_reference") + "," + entry("acknowledge_log_entry", "2026-11-20T01:02:00Z", "integration_reference") + "," +
			entry("acknowledgement_log_entry", "2026-11-20T01:09:00Z", "user_reference") + "," + entry("acknowledgement_log_entry", "2026-11-20T01:05:00Z", "user_reference") + "," +
			entry("unacknowledge_log_entry", "2026-11-20T01:07:00Z", "service_reference") + "," + entry("resolve_log_entry", "2026-11-20T01:20:00Z", "oddity_reference"),
		"2": entry("resolve_log_entry", "2026-11-20T02:10:00Z", "user_reference") + "," + entry("trigger_log_entry", "2026-11-20T02:30:00Z", "service_reference"),
		"3": entry("trigger_log_entry", "2026-11-20T03:00:00Z", "service_reference"),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/incidents":
			fmt.Fprintf(w, `{"incidents": [%s, %s, %s], "more": false}`, incident(1, "resolved"), incident(2, "triggered"), incident(3, "resolved"))
		case strings.HasSuffix(r.URL.Path, "/alerts"):
			w.Write(responseFixture(t, "pagerduty-alerts-1.json"))
		case strings.HasSuffix(r.URL.Path, "/log_entries"):
			n := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/incidents/Q2EDGE000000"), "/log_entries")
			fmt.Fprintf(w, `{"log_entries": [%s], "more": false}`, logs[n])
		default:
			t.Errorf("asked for %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	out, said := runResponses(t, "pagerduty.sh", "PAGERDUTY_API="+srv.URL, "PAGERDUTY_TOKEN_FILE="+tokenFile(t, "x"), "SINCE="+responsesPeriod, "UNTIL="+responsesUntil)
	alerts := `[{"rule": "KubePodCrashLooping", "labels": {"namespace": "shop", "pod": "cart-7f9c"}}]`
	wantResponses(t, out, `{"responses": [
	  {"id": "PD-301", "alerts": `+alerts+`, "sent": "2026-11-20T01:00:00Z", "acknowledged": "2026-11-20T01:05:00Z"},
	  {"id": "PD-302", "alerts": `+alerts+`, "sent": "2026-11-20T02:00:00Z"},
	  {"id": "PD-303", "alerts": `+alerts+`, "sent": "2026-11-20T03:00:00Z"}
	]}`)
	if !strings.Contains(said, "2 incidents resolved by an agent the log entries do not say: kept with no resolve") {
		t.Errorf("said %q", said)
	}
}

// A low-urgency incident pages no one: it is left out unless URGENCY=all,
// and any other value is refused before a request is made.
func TestPagerDutyResponsesUrgency(t *testing.T) {
	parallel(t)
	needTools(t)
	incident := func(n int, urgency string) string {
		return fmt.Sprintf(`{"id": "Q2URG000000%d", "incident_number": %d, "title": "x", "created_at": "2026-11-20T0%d:00:00Z", "status": "triggered", "urgency": %q}`, n, 400+n, n, urgency)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/incidents":
			fmt.Fprintf(w, `{"incidents": [%s, %s], "more": false}`, incident(1, "high"), incident(2, "low"))
		case strings.HasSuffix(r.URL.Path, "/alerts"):
			w.Write(responseFixture(t, "pagerduty-alerts-1.json"))
		case strings.HasSuffix(r.URL.Path, "/log_entries"):
			fmt.Fprint(w, `{"log_entries": [], "more": false}`)
		default:
			t.Errorf("asked for %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	env := []string{"PAGERDUTY_API=" + srv.URL, "PAGERDUTY_TOKEN_FILE=" + tokenFile(t, "x"), "SINCE=" + responsesPeriod, "UNTIL=" + responsesUntil}
	alerts := `[{"rule": "KubePodCrashLooping", "labels": {"namespace": "shop", "pod": "cart-7f9c"}}]`
	page := func(n int) string {
		return fmt.Sprintf(`{"id": "PD-%d", "alerts": %s, "sent": "2026-11-20T0%d:00:00Z"}`, 400+n, alerts, n)
	}
	out, _ := runResponses(t, "pagerduty.sh", env...)
	wantResponses(t, out, `{"responses": [`+page(1)+`]}`)
	out, _ = runResponses(t, "pagerduty.sh", append(env, "URGENCY=high")...)
	wantResponses(t, out, `{"responses": [`+page(1)+`]}`)
	out, _ = runResponses(t, "pagerduty.sh", append(env, "URGENCY=all")...)
	wantResponses(t, out, `{"responses": [`+page(1)+`,`+page(2)+`]}`)
	stdout, said, err := runResponsesErr("pagerduty.sh", append(env, "URGENCY=low")...)
	if err == nil || stdout != "" || !strings.Contains(said, "URGENCY is high or all, not low") {
		t.Errorf("URGENCY=low: wrote %q, said %q, err %v", stdout, said, err)
	}
}
