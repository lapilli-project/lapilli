package firing

import (
	"encoding/json"
	"fmt"
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
)

// The recipes for Datadog's monitors and New Relic's alert conditions, run
// against servers standing in for their APIs, answering as their
// documentation shows.

// keyFiles writes each key to a file of its own, a line ending it, and
// gives their paths.
func keyFiles(t *testing.T, keys ...string) []string {
	t.Helper()
	dir := t.TempDir()
	var paths []string
	for i, k := range keys {
		p := filepath.Join(dir, fmt.Sprintf("key-%d", i))
		if err := os.WriteFile(p, []byte(k+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	return paths
}

func TestDatadogRecipe(t *testing.T) {
	needTools(t)
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		auth := r.Header.Get("DD-API-KEY") + " " + r.Header.Get("DD-APPLICATION-KEY")
		if b := r.Header.Get("Authorization"); b != "" {
			auth = b
		}
		mu.Lock()
		asked = append(asked, auth)
		mu.Unlock()
		if r.Method != http.MethodGet || r.URL.Path != "/api/v2/events" || q.Get("filter[query]") != "@monitor.id:*" ||
			q.Get("sort") != "timestamp" || q.Get("page[limit]") != "1000" {
			t.Errorf("%s %s", r.Method, r.URL)
		}
		// From a day before SINCE, to know which monitors were on already,
		// to now, in milliseconds.
		if q.Get("filter[from]") != "1793404800000" {
			t.Errorf("filter[from] %q", q.Get("filter[from]"))
		}
		if _, err := strconv.ParseInt(q.Get("filter[to]"), 10, 64); err != nil {
			t.Errorf("filter[to] %q", q.Get("filter[to]"))
		}
		page := "datadog-1.json"
		if q.Get("page[cursor]") == "page-2" {
			page = "datadog-2.json"
		}
		b, _ := os.ReadFile(recipes + "testdata/" + page)
		w.Write(b)
	}))
	defer srv.Close()
	keys := keyFiles(t, "key1", "app1", "tok1")
	shop := map[string]string{"alarm_id": "1001", "severity": "P2", "env": "prod", "team": "shop"}
	with := func(l map[string]string, k, v string) map[string]string {
		m := map[string]string{k: v}
		for k, v := range l {
			m[k] = v
		}
		return m
	}
	for _, auth := range [][]string{
		{"DATADOG_TOKEN_FILE=", "DATADOG_API_KEY_FILE=" + keys[0], "DATADOG_APP_KEY_FILE=" + keys[1]},
		{"DATADOG_TOKEN_FILE=" + keys[2]},
	} {
		got := runRecipe(t, "datadog.sh", append(auth, "DATADOG_API="+srv.URL, "SINCE=2026-11-01T00:00:00Z", "UNTIL=2026-11-08T00:00:00Z")...)
		// Queue backlog triggered the day before and notified again in the
		// period: on already, left out. One group's firing is from its first
		// error, notified again, to its ok, whose time is in RFC 3339 only;
		// the other's from its warn, through its error, to its ok. The
		// monitor's name is the one before its template variables are
		// filled in. The group's service, not the monitor's tag; a tag of
		// no value, and one over 256 bytes, left out. Disk full is still
		// on, its priority a legacy one; CPU high's event names two hosts,
		// so neither; Login errors triggered after UNTIL.
		wantFirings(t, got,
			Firing{Rule: "Checkout 5xx on {{service.name}}", Labels: with(shop, "service", "checkout"),
				Start: ts("2026-11-02T03:10:00.25Z"), End: ts("2026-11-02T03:40:00Z")},
			Firing{Rule: "Checkout 5xx on {{service.name}}", Labels: with(shop, "service", "cart"),
				Start: ts("2026-11-03T05:00:00Z"), End: ts("2026-11-03T05:30:00Z")},
			Firing{Rule: "Disk full", Labels: map[string]string{"alarm_id": "4004"}, Start: ts("2026-11-04T01:00:00Z")},
			Firing{Rule: "CPU high", Labels: map[string]string{"alarm_id": "6006", "severity": "P5"},
				Start: ts("2026-11-05T00:00:00Z"), End: ts("2026-11-05T00:05:00Z")})
	}
	if want := []string{"key1 app1", "key1 app1", "Bearer tok1", "Bearer tok1"}; strings.Join(asked, ",") != strings.Join(want, ",") {
		t.Errorf("signed in as %q, want %q", asked, want)
	}
}

// nrqlQuery is the query newrelic.sh asks NerdGraph, and the millisecond
// it pages from.
var nrqlQuery = regexp.MustCompile(`^\{ actor \{ account\(id: 1234567\) \{ nrql\(query: "SELECT \* FROM NrAiIncident WHERE event IN \('open', 'close'\) SINCE ([0-9]+) UNTIL ([0-9]+) ORDER BY timestamp ASC LIMIT 1000"\) \{ results \} \} \} \}$`)

// nerdGraph stands in for NerdGraph, answering each query with page, the
// results by the millisecond asked from.
func nerdGraph(t *testing.T, page func(since string) []any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" || r.Header.Get("API-Key") != "NRAK-TEST" ||
			r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("%s %s %q", r.Method, r.URL.Path, r.Header.Get("API-Key"))
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		m := nrqlQuery.FindStringSubmatch(body.Query)
		if m == nil {
			t.Errorf("query %q", body.Query)
			http.Error(w, "no", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"actor": map[string]any{"account": map[string]any{
			"nrql": map[string]any{"results": page(m[1])}}}}})
	}))
}

// results reads a page of NerdGraph's NRQL results from testdata.
func results(t *testing.T, name string) []any {
	t.Helper()
	var p struct {
		Data struct {
			Actor struct {
				Account struct {
					NRQL struct {
						Results []any `json:"results"`
					} `json:"nrql"`
				} `json:"account"`
			} `json:"actor"`
		} `json:"data"`
	}
	b, err := os.ReadFile(recipes + "testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return p.Data.Actor.Account.NRQL.Results
}

func TestNewRelicRecipe(t *testing.T) {
	needTools(t)
	first, second := results(t, "newrelic-1.json"), results(t, "newrelic-2.json")
	srv := nerdGraph(t, func(since string) []any {
		switch since {
		case "1793487600000": // an hour before SINCE
			// A full page: the rest of it events of incidents opened
			// before, closing at its last millisecond.
			page := append([]any{}, first...)
			for i := len(page); i < 1000; i++ {
				page = append(page, map[string]any{"incidentId": fmt.Sprintf("before-%d", i), "event": "close", "timestamp": 1793591070000})
			}
			return page
		case "1793591070000": // the last page's last millisecond, read again
			return second
		}
		t.Errorf("since %s", since)
		return nil
	})
	defer srv.Close()
	keys := keyFiles(t, "NRAK-TEST")
	got := runRecipe(t, "newrelic.sh", "NEW_RELIC_API="+srv.URL+"/graphql", "NEW_RELIC_API_KEY_FILE="+keys[0],
		"NEW_RELIC_ACCOUNT_ID=1234567", "SINCE=2026-11-01T00:00:00Z", "UNTIL=2026-11-08T00:00:00Z")
	shop := map[string]string{"alarm_id": "685305", "severity": "critical", "policy": "Shop", "entity": "checkout-api",
		"service": "checkout", "team": "shop"}
	// Queue depth's first incident opened before SINCE: left out, though it
	// closed in the period. A tag of two lines is left out. Host not
	// reporting has no entity, so its target is named, and is still open.
	// The third opened half a second before its event was written: from
	// when it opened. The last opened before UNTIL, its event written after;
	// one opened after UNTIL is the next period's.
	wantFirings(t, got,
		Firing{Rule: "Checkout errors", Labels: shop, Start: ts("2026-11-02T03:10:00Z"), End: ts("2026-11-02T03:44:30Z")},
		Firing{Rule: "Host not reporting", Labels: map[string]string{"alarm_id": "701", "severity": "high", "policy": "Infra", "entity": "web-7"},
			Start: ts("2026-11-03T01:00:00Z")},
		Firing{Rule: "Checkout errors", Labels: shop, Start: ts("2026-11-04T08:59:59.5Z"), End: ts("2026-11-04T09:20:00Z")},
		Firing{Rule: "Queue depth", Labels: map[string]string{"alarm_id": "700", "severity": "high", "policy": "Data"},
			Start: ts("2026-11-07T23:59:59.9Z")})
}

// TestVendorRecipesFailLoudly: an answer that is refused, or that can't be
// read, or might hold part of the history only, stops a recipe with
// nothing written, and what it says on stderr holds no key.
func TestVendorRecipesFailLoudly(t *testing.T) {
	needTools(t)
	keys := keyFiles(t, "key1", "app1", "NRAK-TEST")
	dd := []string{"DATADOG_TOKEN_FILE=", "DATADOG_API_KEY_FILE=" + keys[0], "DATADOG_APP_KEY_FILE=" + keys[1],
		"SINCE=2026-11-01T00:00:00Z", "UNTIL=2026-11-08T00:00:00Z"}
	nr := []string{"NEW_RELIC_API_KEY_FILE=" + keys[2], "NEW_RELIC_ACCOUNT_ID=1234567",
		"SINCE=2026-11-01T00:00:00Z", "UNTIL=2026-11-08T00:00:00Z"}
	event := func(ms int64) string {
		return fmt.Sprintf(`{"id": "e", "attributes": {"attributes": {"monitor_id": 1, "status": "error", "timestamp": %d}}}`, ms)
	}
	row := func(open int64) map[string]any {
		return map[string]any{"incidentId": "i", "event": "open", "timestamp": 1793589000000, "openTime": open, "conditionName": "c"}
	}
	full := func(ms int64) string {
		rows := make([]any, 1000)
		for i := range rows {
			rows[i] = map[string]any{"incidentId": fmt.Sprint(i), "event": "close", "timestamp": ms}
		}
		b, _ := json.Marshal(map[string]any{"data": map[string]any{"actor": map[string]any{"account": map[string]any{"nrql": map[string]any{"results": rows}}}}})
		return string(b)
	}
	nrql := func(rows ...any) string {
		b, _ := json.Marshal(map[string]any{"data": map[string]any{"actor": map[string]any{"account": map[string]any{"nrql": map[string]any{"results": rows}}}}})
		return string(b)
	}
	for _, c := range []struct {
		name, script string
		code         int
		answer       string
		env          []string
		says         string
	}{
		{"refused", "datadog.sh", 403, `{"errors": ["Forbidden"]}`, dd, "403"},
		{"warnings", "datadog.sh", 200, `{"data": [], "meta": {"warnings": [{"code": "unknown_index", "title": "One or several indexes are missing or invalid."}]}}`, dd, "in part only"},
		{"not events", "datadog.sh", 200, `{"data": "events"}`, dd, "isn't a list of events"},
		{"not JSON", "datadog.sh", 200, `<html>`, dd, "isn't a list of events"},
		{"same page", "datadog.sh", 200, `{"data": [` + event(1793589000000) + `], "meta": {"page": {"after": "again"}}}`, dd, "the same page again"},
		{"pages", "datadog.sh", 200, `{"data": [` + event(1793589000000) + `], "meta": {"page": {"after": "next"}}}`, append([]string{"MAX_PAGES=1"}, dd...), "more than 1 pages"},
		{"seconds", "datadog.sh", 200, `{"data": [` + event(1793589000) + `]}`, dd, "no time in milliseconds"},
		{"refused", "newrelic.sh", 403, `{"errors": [{"message": "Forbidden"}]}`, nr, "403"},
		{"errors", "newrelic.sh", 200, `{"data": {"actor": {"account": null}}, "errors": [{"message": "Invalid query"}]}`, nr, `NerdGraph says ["Invalid query"]`},
		{"no results", "newrelic.sh", 200, `{"data": {"actor": {"account": {"nrql": null}}}}`, nr, "no NRQL results"},
		{"not JSON", "newrelic.sh", 200, `<html>`, nr, "isn't JSON"},
		{"one millisecond", "newrelic.sh", 200, full(1793487600000), nr, "more than 1000 events in one millisecond"},
		{"pages", "newrelic.sh", 200, full(1793589000000), append([]string{"MAX_PAGES=1"}, nr...), "more than 1 pages"},
		{"seconds", "newrelic.sh", 200, nrql(row(1793589000)), nr, "no time in milliseconds"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.code)
			w.Write([]byte(c.answer))
		}))
		api := "DATADOG_API=" + srv.URL
		if c.script == "newrelic.sh" {
			api = "NEW_RELIC_API=" + srv.URL + "/graphql"
		}
		cmd := exec.Command("sh", recipes+c.script)
		cmd.Env = append(append(os.Environ(), api), c.env...)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		srv.Close()
		if err == nil || stdout.Len() != 0 || !strings.Contains(stderr.String(), c.says) {
			t.Errorf("%s %s: %v, wrote %q, said %q", c.script, c.name, err, stdout.String(), stderr.String())
		}
		for _, k := range []string{"key1", "app1", "NRAK-TEST"} {
			if strings.Contains(stderr.String(), k) {
				t.Errorf("%s %s said the key: %q", c.script, c.name, stderr.String())
			}
		}
	}
}

// TestVendorRecipesKeepKeysOutOfArguments: the keys go to curl on stdin,
// where ps doesn't show them, not among its arguments.
func TestVendorRecipesKeepKeysOutOfArguments(t *testing.T) {
	needTools(t)
	keys := keyFiles(t, "key1", "app1", "NRAK-TEST")
	for _, c := range []struct {
		script string
		env    []string
		stdin  string
	}{
		{"datadog.sh", []string{"DATADOG_TOKEN_FILE=", "DATADOG_API_KEY_FILE=" + keys[0], "DATADOG_APP_KEY_FILE=" + keys[1],
			"SINCE=2026-11-01T00:00:00Z"}, "DD-API-KEY: key1\nDD-APPLICATION-KEY: app1\n"},
		{"newrelic.sh", []string{"NEW_RELIC_API_KEY_FILE=" + keys[2], "NEW_RELIC_ACCOUNT_ID=1234567",
			"SINCE=2026-11-01T00:00:00Z"}, "API-Key: NRAK-TEST\n"},
	} {
		path, calls := fakeCLI(t, "curl", `cat >"$(dirname "$0")/stdin"; echo 'denied' >&2; exit 22`)
		cmd := exec.Command("sh", recipes+c.script)
		cmd.Env = append(append(os.Environ(), "PATH="+path), c.env...)
		out, err := cmd.Output()
		if err == nil || len(out) != 0 {
			t.Errorf("%s: curl failed, but %v, wrote %q", c.script, err, out)
		}
		args, _ := os.ReadFile(calls)
		stdin, _ := os.ReadFile(filepath.Join(filepath.Dir(calls), "stdin"))
		if len(args) == 0 || strings.Contains(string(args), "key1") || strings.Contains(string(args), "app1") || strings.Contains(string(args), "NRAK") {
			t.Errorf("%s: curl's arguments %q", c.script, args)
		}
		if string(stdin) != c.stdin {
			t.Errorf("%s: curl's stdin %q, want %q", c.script, stdin, c.stdin)
		}
	}
}

// TestVendorRecipesRefuse: what isn't a time, a site, an account, a region,
// https or a key is refused before the API is asked, and the refusal says
// why.
func TestVendorRecipesRefuse(t *testing.T) {
	needTools(t)
	keys := keyFiles(t, "key1", "app1", "NRAK-TEST", "two\nlines")
	dd := []string{"DATADOG_TOKEN_FILE=", "DATADOG_API_KEY_FILE=" + keys[0], "DATADOG_APP_KEY_FILE=" + keys[1]}
	nr := []string{"NEW_RELIC_API_KEY_FILE=" + keys[2], "NEW_RELIC_ACCOUNT_ID=1234567"}
	for _, c := range []struct {
		script string
		env    []string
		says   string
	}{
		{"datadog.sh", append([]string{"SINCE=2026-11-01"}, dd...), "is not a time"},
		{"datadog.sh", append([]string{"SINCE=2026-11-01T00:00:00Z\n&page[cursor]=x"}, dd...), "is not a time"},
		{"datadog.sh", append([]string{"SINCE=2026-11-01T00:00:00Z", "DATADOG_SITE=datadoghq.com/x"}, dd...), "is not a site"},
		{"datadog.sh", append([]string{"SINCE=2026-11-01T00:00:00Z", "DATADOG_SITE=datadoghq.com\nexample.com"}, dd...), "is not a site"},
		{"datadog.sh", append([]string{"SINCE=2026-11-01T00:00:00Z", "DATADOG_API=http://example.com"}, dd...), "is not https"},
		{"datadog.sh", append([]string{"SINCE=2026-11-01T00:00:00Z", "DATADOG_API=http://localhost:1@example.com"}, dd...), "is not https"},
		{"datadog.sh", append([]string{"SINCE=2026-11-01T00:00:00Z", "MAX_PAGES=all"}, dd...), "MAX_PAGES"},
		{"datadog.sh", []string{"SINCE=2026-11-01T00:00:00Z", "DATADOG_TOKEN_FILE=" + keys[3]}, "doesn't hold a key"},
		{"datadog.sh", []string{"SINCE=2026-11-01T00:00:00Z", "DATADOG_TOKEN_FILE=", "DATADOG_API_KEY_FILE=" + keys[0], "DATADOG_APP_KEY_FILE=" + keys[3]}, "doesn't hold a key"},
		{"newrelic.sh", append([]string{"SINCE=2026-11-01"}, nr...), "is not a time"},
		{"newrelic.sh", append([]string{"SINCE=2026-11-01T00:00:00Z", "UNTIL=now"}, nr...), "is not a time"},
		{"newrelic.sh", []string{"SINCE=2026-11-01T00:00:00Z", "NEW_RELIC_API_KEY_FILE=" + keys[2], "NEW_RELIC_ACCOUNT_ID=1) { x"}, "is not an account id"},
		{"newrelic.sh", append([]string{"SINCE=2026-11-01T00:00:00Z", "NEW_RELIC_REGION=moon"}, nr...), "is us, eu or jp"},
		{"newrelic.sh", append([]string{"SINCE=2026-11-01T00:00:00Z", "NEW_RELIC_API=http://example.com/graphql"}, nr...), "is not https"},
		{"newrelic.sh", append([]string{"SINCE=2026-11-01T00:00:00Z", "MAX_PAGES=all"}, nr...), "MAX_PAGES"},
		{"newrelic.sh", []string{"SINCE=2026-11-01T00:00:00Z", "NEW_RELIC_ACCOUNT_ID=1234567", "NEW_RELIC_API_KEY_FILE=" + keys[3]}, "doesn't hold a key"},
	} {
		path, calls := fakeCLI(t, "curl", "echo '{}'")
		cmd := exec.Command("sh", recipes+c.script)
		cmd.Env = append(append(os.Environ(), "PATH="+path), c.env...)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		_, called := os.Stat(calls)
		if err == nil || stdout.Len() != 0 || called == nil || !strings.Contains(stderr.String(), c.says) {
			t.Errorf("%s %q: %v, wrote %q, said %q, curl called: %v", c.script, c.env, err, stdout.String(), stderr.String(), called == nil)
		}
		if strings.Contains(stderr.String(), "lines") {
			t.Errorf("%s %q said what the key file holds: %q", c.script, c.env, stderr.String())
		}
	}
}
