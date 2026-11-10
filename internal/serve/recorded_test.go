package serve

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

const firingRecipes = "../../recipes/firings/"

// fakeCLI puts a script named name first on PATH, standing in for a cloud's
// CLI, as the recipes' own tests do.
func fakeCLI(t *testing.T, name, script string) string {
	t.Helper()
	bin := t.TempDir()
	body := "#!/bin/sh\nd=" + firingRecipes + "testdata\n" + script
	if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin + string(os.PathListSeparator) + os.Getenv("PATH")
}

// The recipes' fixtures, written by each recipe and sent through the port
// with put.sh, land in the ledger a source each; sent again, they change
// nothing.
func TestFiringsThroughThePort(t *testing.T) {
	needTools(t)
	const token = "a-firings-token-long-enough"
	s, _ := server(t, func(c *Config) { c.IncidentsListen, c.FiringsToken = "127.0.0.1:0", tokenFile(t, token) })
	port := httptest.NewServer(s.IncidentsHandler())
	defer port.Close()
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := "google-cloud-1.json"
		if r.URL.Query().Get("pageToken") == "page-2" {
			page = "google-cloud-2.json"
		}
		b, _ := os.ReadFile(firingRecipes + "testdata/" + page)
		w.Write(b)
	}))
	defer google.Close()
	period := []string{"SINCE=2026-11-01T00:00:00Z", "UNTIL=2026-11-08T00:00:00Z"}
	sources := []struct {
		source, script string
		env            []string
		firings        int
	}{
		{"cloudwatch-prod", "cloudwatch.sh", []string{"PATH=" + fakeCLI(t, "aws", `case "$*" in
  *describe-alarm-history*) cat "$d/cloudwatch-history.json" ;;
  *describe-alarms*) cat "$d/cloudwatch-alarms.json" ;;
  *) exit 2 ;;
esac`)}, 4},
		{"azure-shop", "azure-monitor.sh", []string{"SUBSCRIPTIONS=s1", "PATH=" + fakeCLI(t, "az", `case "$*" in
  *"--skip-token page-2"*) cat "$d/azure-monitor-2.json" ;;
  *) cat "$d/azure-monitor-1.json" ;;
esac`)}, 4},
		{"google-shop", "google-cloud.sh", []string{"GOOGLE_MONITORING_API=" + google.URL, "GOOGLE_TOKEN_FILE=" + tokenFile(t, "tok"),
			"GOOGLE_CLOUD_PROJECT=shop"}, 2},
		{"oci-shop", "oci.sh", []string{"COMPARTMENT_ID=ocid1.compartment.oc1..x", "PATH=" + fakeCLI(t, "oci", `case "$*" in
  *"alarm list"*) cat "$d/oci-alarms.json" ;;
  *aaaa*"--page page-2"*) cat "$d/oci-history-a2.json" ;;
  *aaaa*) cat "$d/oci-history-a.json" ;;
  *bbbb*) cat "$d/oci-history-b.json" ;;
  *) exit 2 ;;
esac`)}, 3},
	}
	put := func(source, firings, tok string) (string, error) {
		cmd := exec.Command("sh", firingRecipes+"put.sh", port.URL, source)
		cmd.Env = append(append(os.Environ(), period...), "LAPILLI_TOKEN_FILE="+tokenFile(t, tok))
		cmd.Stdin = strings.NewReader(firings)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	written := map[string]string{}
	for _, c := range sources {
		cmd := exec.Command("sh", firingRecipes+c.script)
		cmd.Env = append(append(os.Environ(), period...), c.env...)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: %v", c.script, err)
		}
		written[c.source] = string(out)
		if msg, err := put(c.source, string(out), token); err != nil || !strings.Contains(msg, "added") {
			t.Fatalf("put.sh %s: %v\n%s", c.source, err, msg)
		}
	}
	ctx := context.Background()
	srcs, err := s.l.Sources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != len(sources) {
		t.Fatalf("sources: %+v", srcs)
	}
	for _, c := range sources {
		i := slices.IndexFunc(srcs, func(x ledger.Source) bool { return x.Name == c.source })
		if i < 0 || srcs[i].Firings != c.firings || srcs[i].By != ledger.FiringsCopier || !srcs[i].From.Equal(utc("2026-11-01T00:00:00Z")) {
			t.Errorf("%s: %+v", c.source, srcs)
		}
	}
	// Two clouds have an alarm called checkout-5xx: the second is told
	// apart by its source.
	table, _ := s.l.RuleTable(ctx)
	var names []string
	for _, r := range table {
		if r.Source != "" {
			names = append(names, r.Name)
		}
	}
	if !slices.Contains(names, "checkout-5xx") || !slices.Contains(names, "checkout-5xx [azure-shop]") {
		t.Errorf("recorded rules: %q", names)
	}
	// The same again: nothing changes.
	cs, _ := s.l.ChangesSince(ctx, time.Unix(0, 0), 1000)
	if msg, err := put("cloudwatch-prod", written["cloudwatch-prod"], token); err != nil || !strings.Contains(msg, "0 added, 0 taken away") {
		t.Errorf("sent again: %v\n%s", err, msg)
	}
	if again, _ := s.l.ChangesSince(ctx, time.Unix(0, 0), 1000); len(again) != len(cs) {
		t.Errorf("sent again, %d changes noted, was %d", len(again), len(cs))
	}
	// The wrong token writes nothing, and the script says so.
	if msg, err := put("cloudwatch-prod", "", "not the token at all"); err == nil || !strings.Contains(msg, "401") {
		t.Errorf("the wrong token: %v\n%s", err, msg)
	}
	if fs, _ := s.l.Firings(ctx, ledger.FiringQuery{From: utc("2026-11-01T00:00:00Z"), To: utc("2026-11-08T00:00:00Z")}); len(fs) != 13 {
		t.Errorf("%d firings in the ledger, want 13", len(fs))
	}
	// What isn't firings is refused before anything is sent.
	if msg, err := put("cloudwatch-prod", "{\"incidents\": []}\n", token); err == nil || !strings.Contains(msg, "want firings") {
		t.Errorf("not firings: %v\n%s", err, msg)
	}
	if msg, err := put("Cloud Watch", "", token); err == nil || !strings.Contains(msg, "lowercase") {
		t.Errorf("a bad source: %v\n%s", err, msg)
	}
}

// The firings token writes firings and nothing else, and the incidents token
// can't write firings; neither believes a header; a write that is too big,
// or of a period that can't be, is refused with nothing written.
func TestFiringsPort(t *testing.T) {
	const incTok, firTok = "an-incidents-token-long-enough", "a-firings-token-long-enough"
	s, _ := server(t, func(c *Config) {
		c.IncidentsListen, c.IncidentsToken, c.FiringsToken = "127.0.0.1:0", tokenFile(t, incTok), tokenFile(t, firTok)
		c.UserHeader, c.Editors = "X-Forwarded-Email", []string{"ana@example.com"}
	})
	port, ui := s.IncidentsHandler(), s.Handler()
	send := func(h http.Handler, method, path, tok string, body string) (int, string) {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		r.Header.Set("X-Forwarded-Email", "ana@example.com")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	week := "?from=2026-11-02T00:00:00Z&to=2026-11-09T00:00:00Z"
	body := `{"rule":"checkout-5xx","labels":{"service":"checkout"},"start":"2026-11-03T02:10:00Z","end":"2026-11-03T02:40:00Z"}
{"rule":"checkout-5xx","labels":{"service":"checkout"},"start":"2026-11-08T23:50:00Z"}
`
	for _, c := range []struct {
		method, path, tok, body string
		want                    int
	}{
		{"PUT", "/api/v1/firings/cw" + week, "", body, http.StatusUnauthorized},
		{"PUT", "/api/v1/firings/cw" + week, incTok, body, http.StatusUnauthorized},
		{"PUT", "/api/v1/incidents/A-1", firTok, `{"start": "2026-11-03T02:00:00Z"}`, http.StatusUnauthorized},
		{"PUT", "/api/v1/firings/cw?from=2026-11-02T00:00:00Z", firTok, body, http.StatusBadRequest},
		{"PUT", "/api/v1/firings/cw?from=2026-11-02&to=2026-11-09", firTok, body, http.StatusBadRequest},
		{"PUT", "/api/v1/firings/cw?from=2026-10-01T00:00:00Z&to=2026-11-09T00:00:00Z", firTok, body, http.StatusBadRequest},
		{"PUT", "/api/v1/firings/cw?from=2026-11-04T00:00:00Z&to=2026-11-09T00:00:00Z", firTok, body, http.StatusBadRequest},
		{"PUT", "/api/v1/firings/CW" + week, firTok, body, http.StatusBadRequest},
		{"PUT", "/api/v1/firings/cw" + week, firTok, "not json\n", http.StatusBadRequest},
		{"PUT", "/api/v1/firings/cw" + week, firTok, `{"rule":"x"}` + "\n", http.StatusBadRequest},
		{"PUT", "/api/v1/firings/cw" + week, firTok, strings.Repeat(`{"rule":"x","start":"2026-11-03T02:10:00Z","labels":{"a":"`+strings.Repeat("b", 900)+`"}}`+"\n", 20000), http.StatusRequestEntityTooLarge},
		{"GET", "/api/v1/sources", firTok, "", http.StatusNotFound},
	} {
		if code, msg := send(port, c.method, c.path, c.tok, c.body); code != c.want {
			t.Errorf("%s %s: %d %s, want %d", c.method, c.path, code, msg, c.want)
		}
	}
	ctx := context.Background()
	if fs, _ := s.l.Firings(ctx, ledger.FiringQuery{From: utc("2026-10-01T00:00:00Z"), To: utc("2026-11-10T00:00:00Z")}); len(fs) != 0 {
		t.Fatalf("refused writes kept %d firings", len(fs))
	}
	code, msg := send(port, "PUT", "/api/v1/firings/cw"+week, firTok, body)
	if code != http.StatusOK || !strings.Contains(msg, `"added": 2`) || !strings.Contains(msg, `"changed": true`) {
		t.Fatalf("written: %d %s", code, msg)
	}
	// Noted as the token, whatever a header says; not anyone's use of
	// Lapilli.
	cs, _ := s.l.ChangesSince(ctx, time.Unix(0, 0), 10)
	if len(cs) != 1 || cs[0].By != ledger.FiringsCopier || cs[0].What != "firings" {
		t.Errorf("changes: %+v", cs)
	}
	if outs, err := s.outcomes(ctx); err != nil || outs.Use.Edits != 0 {
		t.Errorf("edits with the token counted as use: %+v %v", outs.Use, err)
	}
	// The UI's port: no firings path; the token is no one there.
	if code, _ := send(ui, "PUT", "/api/v1/firings/cw"+week, firTok, body); code != http.StatusNotFound && code != http.StatusMethodNotAllowed && code != http.StatusForbidden {
		t.Errorf("firings on the UI's port: %d", code)
	}
	// Where they come from is shown: the API, the runs page, the rule,
	// /metrics.
	if code, msg := send(ui, "GET", "/api/v1/sources", "", ""); code != http.StatusOK || !strings.Contains(msg, `"name": "cw"`) || !strings.Contains(msg, `"firings": 2`) {
		t.Errorf("sources: %d %s", code, msg)
	}
	if _, page, _ := get(t, ui, "/runs"); !strings.Contains(page, "Firings copied in") || !strings.Contains(page, ">cw<") {
		t.Errorf("the runs page doesn't list the source")
	}
	if _, page, _ := get(t, ui, "/rules/checkout-5xx"); !strings.Contains(page, "Recorded in cw") {
		t.Errorf("the rule's page doesn't say where it was recorded:\n%s", page)
	}
	if _, page, _ := get(t, ui, "/metrics"); !strings.Contains(page, `lapilli_serve_source_last_timestamp_seconds{source="cw"}`) || !strings.Contains(page, `lapilli_serve_source_firings{source="cw"} 2`) {
		t.Errorf("/metrics doesn't say when the source was written")
	}
	// Each token has its own turn: a long write of incidents doesn't hold
	// up firings.
	s.tokenBusy <- struct{}{}
	if code, msg := send(port, "PUT", "/api/v1/firings/cw"+week, firTok, body); code != http.StatusOK {
		t.Errorf("firings while incidents are written: %d %s", code, msg)
	}
	<-s.tokenBusy
	// The same token in both files: neither is taken.
	os.WriteFile(s.cfg.IncidentsToken, []byte(firTok+"\n"), 0o600)
	if code, _ := send(port, "PUT", "/api/v1/firings/cw"+week, firTok, body); code != http.StatusUnauthorized {
		t.Errorf("firings with the token both files hold: %d", code)
	}
	if code, _ := send(port, "PUT", "/api/v1/incidents/A-9", firTok, `{"start": "2026-11-03T02:00:00Z"}`); code != http.StatusUnauthorized {
		t.Errorf("incidents with the token both files hold: %d", code)
	}
	os.WriteFile(s.cfg.IncidentsToken, []byte(incTok), 0o600)
	// Read-only refuses them.
	s.cfg.ReadOnly = true
	if code, _ := send(port, "PUT", "/api/v1/firings/cw"+week, firTok, body); code != http.StatusForbidden {
		t.Errorf("read-only: %d", code)
	}
}

// --impact alone, with no rules, replays nothing, whatever rules the ledger
// kept from before.
func TestImpactAlone(t *testing.T) {
	s, _ := server(t, nil)
	ctx := context.Background()
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.cfg.RulesPath, s.cfg.FiringsToken = "", "set"
	f, _, err := s.find(ctx, now.Add(-24*time.Hour), now)
	if err != nil || len(f.Rules) != 0 || !f.ImpactOn {
		t.Errorf("rules %+v, impact %v, %v", f.Rules, f.ImpactOn, err)
	}
}

// With only the firings token, there are no incidents paths on the port.
func TestFiringsPortAlone(t *testing.T) {
	s, _ := server(t, func(c *Config) {
		c.IncidentsListen, c.FiringsToken = "127.0.0.1:0", tokenFile(t, "a-firings-token-long-enough")
	})
	r := httptest.NewRequest("PUT", "/api/v1/incidents/A-1", strings.NewReader(`{"start": "2026-11-03T02:00:00Z"}`))
	r.Header.Set("Authorization", "Bearer a-firings-token-long-enough")
	w := httptest.NewRecorder()
	s.IncidentsHandler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
		t.Errorf("incidents with only the firings token: %d", w.Code)
	}
}

// lapilli serve runs on firings copied in alone: no rules, no Prometheus.
// Its jobs judge what comes and replay nothing; the pages, the digest and
// MCP's reading tools work, and compare says why it can't.
func TestServeWithoutRules(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Options: verdict.Options{Lead: 15 * time.Minute, RefireWithin: time.Hour, UnknownLen: time.Hour, Match: []string{"service"}},
		Step:    time.Minute, Loc: time.UTC, Ledger: filepath.Join(dir, "ledger"),
		Every: time.Hour, Settle: time.Hour, Backfill: 3 * 24 * time.Hour, Watch: time.Minute,
		Digest: Weekly{time.Monday, 9, 0}, Timeout: time.Minute, Parallel: 4, Listen: "127.0.0.1:0", Version: "test",
		AllowedHosts: []string{"example.com"}, IncidentsListen: "127.0.0.1:0", FiringsToken: tokenFile(t, "a-firings-token-long-enough"),
	}
	s, err := New(context.Background(), cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	o := cfg.Options
	week := ledger.Week(s.l.Day(now)) - 7
	from := s.l.Midnight(week)
	if _, err := s.l.Put(ctx, incidentAt(from.Add(26*time.Hour+5*time.Minute)), "ana", o); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutFirings(ctx, "cw", from, from.Add(7*24*time.Hour), firingsAt(from), ledger.FiringsCopier, o); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	s.status.Lock()
	failed := s.status.err
	s.status.Unlock()
	if failed != "" {
		t.Errorf("the jobs failed: %s", failed)
	}
	// The week's digest is worked out though no run moved the cursor.
	if done, err := s.l.Meta(ctx, "digest"); err != nil || done == "" {
		t.Errorf("no digest: %q %v", done, err)
	}
	if runs, _ := s.l.Runs(ctx, 10); slices.ContainsFunc(runs, func(r ledger.Run) bool { return r.Kind == "hourly" || r.Kind == "backfill" }) {
		t.Errorf("a run with nothing to replay: %+v", runs)
	}
	h := s.Handler()
	for _, path := range []string{"/", "/rules", "/rules/checkout-5xx", "/incidents", "/firings", "/runs", "/outcomes", "/api/v1/rules", "/metrics"} {
		if code, page, _ := get(t, h, path); code != http.StatusOK {
			t.Errorf("%s: %d %s", path, code, page)
		}
	}
	if _, page, _ := get(t, h, "/api/v1/rules"); !strings.Contains(page, "checkout-5xx") {
		t.Errorf("the recorded rule isn't listed:\n%s", page)
	}
	if _, out, err := s.toolCompare(ctx, nil, compareIn{}); err == nil || !strings.Contains(err.Error(), "no rules to replay") {
		t.Errorf("compare: %v %+v", err, out)
	}
	// A team owns it by the labels its alerts have.
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "payments", Labels: map[string][]string{"service": {"checkout"}}}, "ana"); err != nil {
		t.Fatal(err)
	}
	own, err := s.owners(ctx)
	if err != nil || own.teamOf("checkout-5xx") != "payments" {
		t.Errorf("owner: %q %v", own.teamOf("checkout-5xx"), err)
	}
}

func incidentAt(at time.Time) incident.Incident {
	return incident.Incident{ID: "INC-9", Title: "Checkout down", Start: at, End: at.Add(30 * time.Minute), Labels: incident.Labels{"service": {"checkout"}}}
}

// firingsAt are a cloud alarm's firings over the week from from: one that
// catches incidentAt(from + 26h05m), and one for nothing.
func firingsAt(from time.Time) []firing.Firing {
	l := map[string]string{"service": "checkout"}
	return []firing.Firing{
		{Rule: "checkout-5xx", Labels: l, Start: from.Add(26 * time.Hour), End: from.Add(27 * time.Hour)},
		{Rule: "checkout-5xx", Labels: l, Start: from.Add(50 * time.Hour), End: from.Add(51 * time.Hour)},
	}
}

// The weekly digest, which Slack and the webhook get, tells of firings
// copied in and of the notifications sent; a team's, of its rules'
// notifications only.
func TestDigestTellsCopiedAndSent(t *testing.T) {
	s := notifyServer(t, func(c *Config) {
		c.IncidentsListen, c.FiringsToken = "127.0.0.1:0", tokenFile(t, "a-firings-token-long-enough")
	})
	ctx := context.Background()
	if code, out, _ := send(t, s.NotifyHandler(), "/v1/alertmanager", notifyFixture(t, "alertmanager.json"), bearer(notifyToken)); code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	o := s.cfg.Options
	from := s.l.Midnight(ledger.Week(s.l.Day(now)))
	to := from.Add(7 * 24 * time.Hour)
	if _, err := s.l.PutFirings(ctx, "cw", from, to, firingsAt(from), ledger.FiringsCopier, o); err != nil {
		t.Fatal(err)
	}
	rep, err := s.l.Report(ctx, from, to, o, false)
	if err != nil {
		t.Fatal(err)
	}
	text, err := s.digestText(ctx, rep, nil, from, to, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Firings copied in from elsewhere, judged with the rest: cw, 2 firings.", "Notifications sent, as the alert managers sent them: 1, for 1 rules"} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in the digest:\n%s", want, text)
		}
	}
	about := digestAbout(rep, text, "", nil)
	if err := s.aboutMore(ctx, about, rep, from, to, true); err != nil {
		t.Fatal(err)
	}
	if cs, ok := about["sources"].([]copiedIn); !ok || len(cs) != 1 || cs[0].Firings != 2 {
		t.Errorf("sources told: %#v", about["sources"])
	}
	if sent, ok := about["notifications"].(*sentLine); !ok || sent.Notifications != 1 {
		t.Errorf("notifications told: %#v", about["notifications"])
	}
	// A team that owns none of the notified rules hears of none, nor of the
	// sources.
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "cloud", Rules: "^checkout-5xx$"}, "ana"); err != nil {
		t.Fatal(err)
	}
	own, err := s.owners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	team, _ := own.team("cloud")
	text, err = s.digestText(ctx, teamReport(rep, own, team, o.Hours), nil, from, to, &team)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "copied in from elsewhere") || strings.Contains(text, "Notifications sent") || !strings.Contains(text, "checkout-5xx") {
		t.Errorf("the team's digest:\n%s", text)
	}
}

// A source nothing has been copied in from for longer than --source-quiet
// is told of, once, to the webhook and Slack; written again and quiet
// again, once more.
func TestSourceQuiet(t *testing.T) {
	hook := filepath.Join(t.TempDir(), "hook")
	os.WriteFile(hook, []byte("https://hooks.example/x"), 0o600)
	s, _ := server(t, func(c *Config) {
		c.IncidentsListen, c.FiringsToken, c.NotifyWebhook = "127.0.0.1:0", tokenFile(t, "a-firings-token-long-enough"), hook
	})
	ctx := context.Background()
	o := s.cfg.Options
	from := s.l.Midnight(ledger.Week(s.l.Day(now)))
	put := func() {
		t.Helper()
		if _, err := s.l.PutFirings(ctx, "cw", from, from.Add(24*time.Hour), nil, ledger.FiringsCopier, o); err != nil {
			t.Fatal(err)
		}
	}
	quiet := func() int {
		t.Helper()
		if err := s.tellQuiet(ctx); err != nil {
			t.Fatal(err)
		}
		ns, err := s.l.Due(ctx, "webhook", time.Now().Add(time.Hour), 100)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, x := range ns {
			if x.Event == "source.quiet" {
				n++
			}
		}
		return n
	}
	put()
	s.now = time.Now
	if n := quiet(); n != 0 {
		t.Errorf("told of a source written just now: %d", n)
	}
	s.now = func() time.Time { return time.Now().Add(27 * time.Hour) }
	if n := quiet(); n != 1 {
		t.Errorf("told %d times of a quiet source, want once", n)
	}
	if n := quiet(); n != 1 {
		t.Errorf("told again: %d", n)
	}
	time.Sleep(1100 * time.Millisecond)
	put()
	if n := quiet(); n != 2 {
		t.Errorf("quiet again after a write: told %d times in all, want 2", n)
	}
	srcs, _ := s.l.Sources(ctx)
	line := s.line(notice{Event: "source.quiet", Source: &srcs[0]})
	if !strings.Contains(line, "nothing has been copied in from cw since") {
		t.Errorf("the Slack line: %q", line)
	}
	s.cfg.SourceQuiet = 48 * time.Hour
	if s.quiet(srcs[0]) {
		t.Errorf("--source-quiet 48h: quiet after 27h")
	}
}

// With no runs, a week is in the ledger once every source not gone quiet
// has written through it.
func TestCopiedTo(t *testing.T) {
	s, _ := server(t, func(c *Config) {
		c.IncidentsListen, c.FiringsToken = "127.0.0.1:0", tokenFile(t, "a-firings-token-long-enough")
	})
	ctx := context.Background()
	o := s.cfg.Options
	week := s.l.Midnight(ledger.Week(s.l.Day(now)) - 7)
	for _, c := range []struct {
		source string
		to     time.Time
	}{{"behind", week.Add(3 * 24 * time.Hour)}, {"ahead", week.Add(7 * 24 * time.Hour)}} {
		if _, err := s.l.PutFirings(ctx, c.source, week, c.to, nil, ledger.FiringsCopier, o); err != nil {
			t.Fatal(err)
		}
	}
	s.now = time.Now
	if got, err := s.copiedTo(ctx); err != nil || !got.Equal(week.Add(3*24*time.Hour)) {
		t.Errorf("copied to %s %v: the source behind holds it", got, err)
	}
	// Gone quiet, it holds nothing up.
	s.now = func() time.Time { return time.Now().Add(27 * time.Hour) }
	if got, _ := s.copiedTo(ctx); !got.After(time.Now()) {
		t.Errorf("copied to %s: a quiet source held it", got)
	}
}

// With no runs, the weekly digest waits for every source not gone quiet to
// have written through the week, so it isn't made without its firings.
func TestDigestWaitsForSources(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Options: verdict.Options{Lead: 15 * time.Minute, RefireWithin: time.Hour, UnknownLen: time.Hour},
		Step:    time.Minute, Loc: time.UTC, Ledger: filepath.Join(dir, "ledger"), Every: time.Hour, Watch: time.Minute,
		Digest: Weekly{time.Monday, 0, 5}, Timeout: time.Minute, Parallel: 4, Listen: "127.0.0.1:0", Version: "test",
		IncidentsListen: "127.0.0.1:0", FiringsToken: tokenFile(t, "a-firings-token-long-enough"),
	}
	s, err := New(context.Background(), cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.now = time.Now
	ctx := context.Background()
	due := cfg.Digest.Last(time.Now(), time.UTC)
	week := s.l.Midnight(ledger.Week(s.l.Day(due)) - 7)
	put := func(to time.Time) {
		t.Helper()
		if _, err := s.l.PutFirings(ctx, "cw", week, to, firingsAt(week), ledger.FiringsCopier, cfg.Options); err != nil {
			t.Fatal(err)
		}
	}
	put(week.Add(4 * 24 * time.Hour))
	if _, err := s.digest(ctx, cfg.Options); err != nil {
		t.Fatal(err)
	}
	if done, _ := s.l.Meta(ctx, "digest"); done != "" {
		t.Errorf("a digest made with the source four days into the week: %q", done)
	}
	put(week.Add(7 * 24 * time.Hour))
	if _, err := s.digest(ctx, cfg.Options); err != nil {
		t.Fatal(err)
	}
	if done, _ := s.l.Meta(ctx, "digest"); done == "" {
		t.Errorf("no digest once the source wrote the week through")
	}
}

// An alert whose rule name a recorded rule was sent under, but named apart,
// isn't tied to that rule's firings: what is counted against a rule is by
// the alert's rule name.
func TestNamedApartNotTied(t *testing.T) {
	s := notifyServer(t, nil)
	ctx := context.Background()
	o := s.cfg.Options
	start := time.Date(2026, 11, 2, 7, 0, 0, 0, time.UTC)
	for _, c := range []struct{ source, res string }{{"azure-shop", "db-1"}, {"azure-other", "db-2"}} {
		labels := map[string]string{"severity": "Sev3", "resource": c.res, "resource_group": "data"}
		if _, err := s.l.PutFirings(ctx, c.source, start.Add(-time.Hour), start.Add(time.Hour), []firing.Firing{{Rule: "disk-full", Labels: labels, Start: start}}, ledger.FiringsCopier, o); err != nil {
			t.Fatal(err)
		}
	}
	body := fmt.Sprintf(`{"schemaId": "azureMonitorCommonAlertSchema", "data": {"essentials": {"alertId": "a2", "alertRule": "disk-full", "severity": "Sev3",
		"monitorCondition": "Fired", "alertTargetIDs": ["/subscriptions/s/resourceGroups/data/providers/x/db-2"], "configurationItems": ["db-2"], "firedDateTime": %q}}}`, start.Format(time.RFC3339))
	if code, out, _ := send(t, s.NotifyHandler(), "/v1/azure", []byte(body), basicAuth("x", notifyToken)); code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	ps, err := s.l.Pages(ctx, start.Add(-24*time.Hour), now.Add(time.Hour), func(string) time.Duration { return 2 * time.Minute })
	if err != nil || len(ps) != 1 || ps[0].Stone != ledger.PageUntied {
		t.Errorf("pages %+v %v", ps, err)
	}
}
