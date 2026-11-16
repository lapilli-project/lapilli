package serve

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/prom"
)

// The marked file: read each run, its windows kept as it says them, teams
// by name; the same file again reads nothing more; a file with no marked
// key, a key it doesn't know, or a team that isn't there, is refused, and
// the ledger keeps what it had.
func TestMarkedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "marked.yaml")
	write := func(text string) { os.WriteFile(path, []byte(text), 0o644) }
	at := now.Add(-50 * time.Hour)
	write(fmt.Sprintf(`marked:
  - kind: sale
    title: Weekend sale
    start: %s
    end: %s
  - {kind: launch, title: "Search, new ranking", start: %s, end: %s, labels: {service: search}, team: search}
  - {kind: sale, title: Friday flash sale, start: 2026-10-30T20:00:00Z, end: 2026-10-30T22:00:00Z, every: week, until: 2027-03-31}
`, at.Format(time.RFC3339), at.Add(4*time.Hour).Format(time.RFC3339), at.Format(time.RFC3339), at.Add(time.Hour).Format(time.RFC3339)))
	s, _ := server(t, func(c *Config) { c.Marked = path })
	ctx := context.Background()
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "search", Rules: "^Errors$"}, "ana"); err != nil {
		t.Fatal(err)
	}
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	ms, err := s.l.AllMarked(ctx)
	if err != nil || len(ms) != 3 {
		t.Fatalf("windows: %+v %v", ms, err)
	}
	by := map[string]ledger.Marked{}
	for _, m := range ms {
		by[m.Title] = m
	}
	if m := by["Search, new ranking"]; m.TeamID == 0 || m.Labels["service"] != "search" || m.Origin != ledger.OriginFile {
		t.Errorf("the launch: %+v", m)
	}
	if m := by["Friday flash sale"]; m.Every != "week" || m.Until.IsZero() {
		t.Errorf("the flash sale: %+v", m)
	}
	fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: at, To: at.Add(5 * time.Hour), Kind: "sale"})
	if err != nil || len(fs) == 0 {
		t.Fatalf("no firing marked with the sale: %v", err)
	}
	read := s.markedRead.Load()
	if read != now.Unix() {
		t.Errorf("read at %d, want %d", read, now.Unix())
	}
	// Each refused, the windows as they were, and the status says why.
	for name, text := range map[string]string{
		"no marked key":  "deploys: []\n",
		"an unknown key": "marked:\n  - {kind: sale, title: x, start: 2026-11-01T00:00:00Z, end: 2026-11-01T01:00:00Z, lables: {service: x}}\n",
		"no such team":   "marked:\n  - {kind: sale, title: x, start: 2026-11-01T00:00:00Z, end: 2026-11-01T01:00:00Z, team: nobody}\n",
		"a kind taken":   "marked:\n  - {kind: deploy, title: x, start: 2026-11-01T00:00:00Z, end: 2026-11-01T01:00:00Z}\n",
		"a bad until":    "marked:\n  - {kind: sale, title: x, start: 2026-11-01T00:00:00Z, end: 2026-11-01T01:00:00Z, every: day, until: soon}\n",
		"an empty file":  "",
	} {
		write(text)
		if err := s.syncMarked(ctx, s.cfg.Options); err == nil {
			t.Errorf("%s: taken", name)
		}
		if got, err := s.l.AllMarked(ctx); err != nil || len(got) != 3 {
			t.Errorf("%s: %d windows after it was refused", name, len(got))
		}
	}
	// marked: [] takes them away.
	write("marked: []\n")
	if err := s.syncMarked(ctx, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.l.AllMarked(ctx); len(got) != 0 {
		t.Errorf("%d windows after marked: []", len(got))
	}
}

// The API: an editor keeps, changes and deletes a marked window; a viewer
// reads them; the marked file's are the file's; the window's occurrences
// in the period come with it; a team's editor keeps the team's alone.
func TestMarkedAPI(t *testing.T) {
	s, pay, search := rolesServer(t, nil)
	h := s.Handler()
	const ed, boss, pat, vi = "ed@example.com", "boss@example.com", "pat@example.com", "vi@example.com"
	body := `{"kind": "sale", "title": "Autumn sale", "start": "2026-11-02T00:00:00Z", "end": "2026-11-02T06:00:00Z", "labels": {"service": "checkout"}}`
	code, out := callAs(t, h, "POST", "/api/v1/marked", ed, "", body, "same-origin")
	if code != http.StatusCreated {
		t.Fatalf("POST: %d %s", code, out)
	}
	id := id(t, out)
	if code, out := callAs(t, h, "POST", "/api/v1/marked", vi, "", body, "same-origin"); code != http.StatusForbidden {
		t.Errorf("a viewer's POST: %d %s", code, out)
	}
	code, out = callAs(t, h, "GET", "/api/v1/marked?from=2026-11-01T00:00:00Z&to=2026-11-03T00:00:00Z", vi, "", "", "")
	var got []struct {
		ledger.Marked
		Occurrences []ledger.Occurrence `json:"occurrences"`
	}
	if err := json.Unmarshal([]byte(out), &got); code != 200 || err != nil || len(got) != 1 || got[0].Kind != "sale" || len(got[0].Occurrences) != 1 ||
		got[0].Origin != "ui" || got[0].Labels["service"] != "checkout" {
		t.Fatalf("GET: %d %s", code, out)
	}
	if code, out := callAs(t, h, "PUT", "/api/v1/marked/"+itoa(id), ed, "", strings.Replace(body, "Autumn", "Winter", 1), "same-origin"); code != 200 || !strings.Contains(out, "Winter sale") {
		t.Errorf("PUT: %d %s", code, out)
	}
	for _, bad := range []string{
		`{"kind": "deploy", "title": "x", "start": "2026-11-02T00:00:00Z", "end": "2026-11-02T06:00:00Z"}`,
		`{"kind": "week", "title": "x", "start": "2026-11-02T00:00:00Z", "end": "2026-11-02T06:00:00Z"}`,
		`{"kind": "sale", "title": "x", "start": "2026-11-02T00:00:00Z", "end": "2026-12-12T06:00:00Z"}`,
		`{"kind": "sale", "title": "x", "start": "2028-11-02T00:00:00Z", "end": "2028-11-02T06:00:00Z"}`,
	} {
		if code, out := callAs(t, h, "POST", "/api/v1/marked", ed, "", bad, "same-origin"); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", bad, code, out)
		}
	}
	// A team's editor: the team's, and no one else's.
	if code, out := callAs(t, h, "POST", "/api/v1/roles", boss, "", `{"who": "user:pat@example.com", "team_id": `+itoa(pay)+`}`, ""); code != http.StatusCreated {
		t.Fatalf("giving pat payments: %d %s", code, out)
	}
	// Of a kind there is: a team's editor makes none.
	team := fmt.Sprintf(`{"kind": "sale", "title": "x", "start": "2026-11-02T00:00:00Z", "end": "2026-11-02T01:00:00Z", "team_id": %d}`, pay)
	if code, out := callAs(t, h, "POST", "/api/v1/marked", pat, "", team, "same-origin"); code != http.StatusCreated {
		t.Errorf("pat, payments': %d %s", code, out)
	}
	if code, out := callAs(t, h, "POST", "/api/v1/marked", pat, "", strings.Replace(team, `"sale"`, `"launch"`, 1), "same-origin"); code != http.StatusBadRequest {
		t.Errorf("pat, a kind there isn't: %d %s", code, out)
	}
	for _, theirs := range []string{fmt.Sprintf(`{"kind": "sale", "title": "x", "start": "2026-11-02T00:00:00Z", "end": "2026-11-02T01:00:00Z", "team_id": %d}`, search),
		`{"kind": "sale", "title": "x", "start": "2026-11-02T00:00:00Z", "end": "2026-11-02T01:00:00Z"}`} {
		if code, out := callAs(t, h, "POST", "/api/v1/marked", pat, "", theirs, "same-origin"); code != http.StatusForbidden {
			t.Errorf("pat, %s: %d %s", theirs, code, out)
		}
	}
	if code, _ := callAs(t, h, "DELETE", "/api/v1/marked/"+itoa(id), pat, "", "", "same-origin"); code != http.StatusForbidden {
		t.Errorf("pat deleting everyone's: %d", code)
	}
	// The file's are the file's.
	if _, err := s.l.SyncMarked(context.Background(), []ledger.Marked{{Kind: "sale", Title: "file's", Start: now.Add(-time.Hour), End: now}}, s.cfg.Options, ledger.MarkedRead{}); err != nil {
		t.Fatal(err)
	}
	all, _ := s.l.AllMarked(context.Background())
	file := all[len(all)-1]
	if code, out := callAs(t, h, "DELETE", "/api/v1/marked/"+itoa(file.ID), ed, "", "", "same-origin"); code != http.StatusConflict {
		t.Errorf("deleting the file's: %d %s", code, out)
	}
	if code, out := callAs(t, h, "DELETE", "/api/v1/marked/"+itoa(id), ed, "", "", "same-origin"); code != http.StatusNoContent && code != 200 {
		t.Errorf("DELETE: %d %s", code, out)
	}
	if code, _ := callAs(t, h, "GET", "/api/v1/marked/"+itoa(id), vi, "", "", ""); code != http.StatusNotFound {
		t.Errorf("GET a deleted one: %d", code)
	}
}

// --read-only refuses every write of a marked window.
func TestMarkedReadOnly(t *testing.T) {
	s, _ := server(t, func(c *Config) { c.ReadOnly = true })
	h := s.Handler()
	if code, _ := api(t, h, "POST", "/api/v1/marked", `{"kind": "sale", "title": "x", "start": "2026-11-02T00:00:00Z", "end": "2026-11-02T06:00:00Z"}`); code != http.StatusForbidden {
		t.Errorf("POST with --read-only: %d", code)
	}
	if code, _, _ := post(t, h, "/marked", url.Values{"kind": {"sale"}, "title": {"x"}, "start": {"2026-11-02T00:00"}, "end": {"2026-11-02T06:00"}}, "same-origin"); code != http.StatusForbidden {
		t.Errorf("the form with --read-only: %d", code)
	}
}

// The page, under Monitoring beside Incidents: each kind's occurrences,
// with their firings inside and the weeks before; a window's own page;
// the form keeps one, and what people wrote is shown escaped and set
// apart in <bdi>.
func TestMarkedPages(t *testing.T) {
	s := pagesServer(t)
	h := s.Handler()
	code, body, _ := get(t, h, "/marked")
	if code != 200 || !strings.Contains(body, "<h2><bdi>sale</bdi></h2>") || !strings.Contains(body, `<bdi>Autumn sale</bdi>`) ||
		!strings.Contains(body, `href="/firings?day=2026-11-02&amp;kind=sale"`) {
		t.Fatalf("/marked: %d\n%s", code, body)
	}
	// A team's name, set apart as a rule's is.
	if !strings.Contains(body, `<span class="quiet"><bdi>payments</bdi></span>`) {
		t.Error("/marked: a team's name not in <bdi>")
	}
	nav := body[strings.Index(body, `href="/incidents"`):]
	if i, j := strings.Index(nav, `href="/marked"`), strings.Index(nav, `href="/firings"`); i < 0 || j < 0 || i > j {
		t.Error("Marked windows isn't under Monitoring, after Incidents")
	}
	for _, p := range []string{"/marked/1", "/marked/2", "/marked/3", "/marked?team=payments", "/firings?day=2026-11-02&kind=sale"} {
		if code, body, _ := get(t, h, p); code != 200 {
			t.Errorf("%s: %d\n%s", p, code, body)
		}
	}
	if code, _, _ := get(t, h, "/marked/99"); code != http.StatusNotFound {
		t.Errorf("/marked/99: %d", code)
	}
	// A title and a kind from someone, through the form.
	f := url.Values{"kind": {"launch"}, "title": {`<script>alert(1)</script> & co`}, "start": {"2026-11-02T08:00"}, "end": {"2026-11-02T09:00"},
		"labels": {"service=search"}}
	if code, _, hd := post(t, h, "/marked", f, "same-origin"); code != http.StatusSeeOther || !strings.HasPrefix(hd.Get("Location"), "/marked/") {
		t.Fatalf("the form: %d %v", code, hd)
	}
	_, body, _ = get(t, h, "/marked")
	if strings.Contains(body, "<script>alert(1)") || !strings.Contains(body, "<bdi>&lt;script&gt;alert(1)&lt;/script&gt; &amp; co</bdi>") {
		t.Error("a title from someone isn't escaped and set apart")
	}
	// Two values of a label: a marked window's label has one.
	f.Set("labels", "service=search service=cart")
	if code, _, _ := post(t, h, "/marked", f, "same-origin"); code != http.StatusBadRequest {
		t.Errorf("two values of a label: %d", code)
	}
}

// An expression's windows: each series on its own, gaps of two steps or
// fewer not splitting them, from the first sample to a step after the
// last, none merged across series; no labels unless --mark-match keeps
// them.
func TestFromMarks(t *testing.T) {
	step := time.Minute
	at := now.Add(-time.Hour)
	series := func(labels map[string]string, mins ...int) prom.Series {
		s := prom.Series{Labels: labels}
		for _, m := range mins {
			s.Samples = append(s.Samples, prom.Sample{T: at.Add(time.Duration(m) * time.Minute), V: 1})
		}
		return s
	}
	ss := []prom.Series{
		series(map[string]string{"region": "kr", "instance": "a"}, 0, 1, 3, 4, 10, 11),
		series(map[string]string{"region": "us", "instance": "b"}, 2, 3),
	}
	ws := FromMarks(ss, step, "sale", nil)
	var spans []string
	for _, w := range ws {
		spans = append(spans, fmt.Sprintf("%s-%s %v", w.Start.Sub(at), w.End.Sub(at), w.Labels))
	}
	want := []string{"0s-5m0s map[]", "2m0s-4m0s map[]", "10m0s-12m0s map[]"} // the gap of two steps from 1 to 3 doesn't split
	if !slices.Equal(spans, want) {
		t.Errorf("windows %q, want %q", spans, want)
	}
	ws = FromMarks(ss, step, "sale", []string{"region"})
	if len(ws) != 3 || ws[0].Labels["region"] != "kr" || ws[1].Labels["region"] != "us" || len(ws[0].Labels) != 1 {
		t.Errorf("with --mark-match region: %+v", ws)
	}
	// Two series on at once are two windows, not merged.
	if ws := FromMarks([]prom.Series{series(map[string]string{"i": "a"}, 0, 1), series(map[string]string{"i": "b"}, 0, 1)}, step, "sale", nil); len(ws) != 2 {
		t.Errorf("two series at once: %d windows", len(ws))
	}
}

// --mark and --mark-match as given: KIND=PROMQL, each kind once, eight at
// most, a --mark-match of a kind given.
func TestParseMarks(t *testing.T) {
	ms, err := ParseMarks([]string{"sale=shop_sale_active == 1", " launch = flag_on{name=\"ranking\"} "}, []string{"sale=region, country"})
	if err != nil || len(ms) != 2 || ms[0].Kind != "sale" || ms[0].Expr != "shop_sale_active == 1" || !slices.Equal(ms[0].Match, []string{"country", "region"}) ||
		ms[1].Kind != "launch" || ms[1].Expr != `flag_on{name="ranking"}` || ms[1].Match != nil {
		t.Errorf("%+v %v", ms, err)
	}
	if ms[0].Key() == (Mark{Kind: "sale", Expr: "shop_sale_active == 1"}).Key() {
		t.Error("--mark-match doesn't change the key")
	}
	nine := make([]string, 9)
	for i := range nine {
		nine[i] = fmt.Sprintf("k%d=x", i)
	}
	for name, c := range map[string][2][]string{
		"no expression":   {{"sale="}, nil},
		"no kind":         {{"=x"}, nil},
		"deploy":          {{"deploy=x"}, nil},
		"week":            {{"week=x"}, nil},
		"twice":           {{"sale=x", "sale=y"}, nil},
		"nine":            {nine, nil},
		"match no kind":   {{"sale=x"}, {"launch=region"}},
		"match no labels": {{"sale=x"}, {"sale="}},
		"match a bad one": {{"sale=x"}, {"sale=re-gion"}},
	} {
		if _, err := ParseMarks(c[0], c[1]); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
}

// --mark: read each run, over the run's period and the hour before, its
// windows marking every firing while it is on, whatever labels its series
// carry; with --mark-match region, only the firings that agree with each
// series' region, which these, without one, don't. A new key reads it again
// over the four weeks before, in place of what it found.
func TestMarkExpression(t *testing.T) {
	s, _ := server(t, func(c *Config) {
		c.Marks = []Mark{{Kind: "sale", Expr: "sale_active == 1"}}
	})
	ctx := context.Background()
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	marked := func() int {
		t.Helper()
		fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: now.Add(-72 * time.Hour), To: now, Kind: "sale"})
		if err != nil {
			t.Fatal(err)
		}
		return len(fs)
	}
	ms, err := s.l.AllMarked(ctx)
	if err != nil || len(ms) != 2 || ms[0].Origin != ledger.OriginExpr || len(ms[0].Labels) != 0 {
		t.Fatalf("windows: %+v %v", ms, err)
	}
	// Errors fires for search two minutes into each hour from fifty hours
	// ago (its for): four of them start in the sale's four hours.
	if n := marked(); n != 4 {
		t.Errorf("%d marked with the sale, want 4", n)
	}
	if k, _ := s.l.MarkKeyOf(ctx, "sale"); k != s.cfg.Marks[0].Key() {
		t.Errorf("key %q", k)
	}
	// With --mark-match region: read again, its windows replaced, and they
	// mark none of these.
	s.cfg.Marks[0].Match = []string{"region"}
	s.jobs(ctx, false)
	ms, err = s.l.AllMarked(ctx)
	if err != nil || len(ms) != 2 || ms[0].Labels["region"] == "" {
		t.Fatalf("windows read again: %+v %v", ms, err)
	}
	if n := marked(); n != 0 {
		t.Errorf("%d marked with the sale by region, want 0", n)
	}
	// No longer given: its windows stay as they were found.
	s.cfg.Marks = nil
	s.forgotMarks.Store(false)
	s.cfg.Marks = []Mark{{Kind: "launch", Expr: "impact"}}
	s.jobs(ctx, false)
	if k, _ := s.l.MarkKeyOf(ctx, "sale"); k != "" {
		t.Errorf("the sale's key after --mark sale was taken away: %q", k)
	}
	if ms, _ := s.l.AllMarked(ctx); !slices.ContainsFunc(ms, func(m ledger.Marked) bool { return m.Kind == "sale" }) {
		t.Error("the sale's windows went with its expression")
	}
}

// marked: in --config, its fifth key: read as the UI's forms take one,
// kept declared, read-only in the UI, and exported in /config.yaml as the
// file would declare it; what it can't say is refused.
func TestMarkedConfig(t *testing.T) {
	body := configFile + `marked:
  - kind: sale
    title: Autumn sale
    start: 2026-11-01T00:00
    end: 2026-11-03T00:00
  - kind: launch
    title: "Checkout, new flow"
    team: payments
    start: 2026-11-02T00:00
    end: 2026-11-02T06:00
    every: week
    until: 2027-03-31
    labels: {service: checkout}
`
	s, _ := configServer(t, body, nil)
	if e := s.configShown().ErrText("en"); e != "" {
		t.Fatal(e)
	}
	ctx := context.Background()
	ms, err := s.l.AllMarked(ctx)
	if err != nil || len(ms) != 2 || !ms[0].Declared || ms[1].Labels["service"] != "checkout" || ms[1].TeamID == 0 || ms[1].Every != "week" {
		t.Fatalf("declared: %+v %v", ms, err)
	}
	h := s.Handler()
	if code, _ := callAs(t, h, "DELETE", "/api/v1/marked/"+itoa(ms[0].ID), "boss@example.com", "", "", "same-origin"); code != http.StatusConflict {
		t.Errorf("deleting a declared one: %d", code)
	}
	if _, page := callAs(t, h, "GET", "/marked/"+itoa(ms[0].ID), "boss@example.com", "", "", ""); !strings.Contains(page, "Declared in config — change it in values") {
		t.Error("a declared one's page offers to change it")
	}
	_, out := callAs(t, h, "GET", "/config.yaml", "boss@example.com", "", "", "")
	if !strings.Contains(out, "marked:\n") || !strings.Contains(out, "kind: launch") || !strings.Contains(out, "until: \"2027-03-31\"") && !strings.Contains(out, "until: 2027-03-31") {
		t.Errorf("/config.yaml:\n%s", out)
	}
	d, err := ParseConfig([]byte(out), time.UTC)
	if err != nil || len(d.Marked) != 2 || d.Marked[1].Team != "payments" || d.Marked[1].Labels["service"] != "checkout" {
		t.Errorf("read back: %+v %v", d.Marked, err)
	}
	for name, text := range map[string]string{
		"an unknown key":     "marked:\n  - {kind: sale, title: x, start: 2026-11-01T00:00, end: 2026-11-01T01:00, rules: ^X}\n",
		"no kind":            "marked:\n  - {title: x, start: 2026-11-01T00:00, end: 2026-11-01T01:00}\n",
		"two values":         "marked:\n  - {kind: sale, title: x, start: 2026-11-01T00:00, end: 2026-11-01T01:00, labels: {service: [a, b]}}\n",
		"nothing after team": "marked:\n  - {kind: sale, title: x, start: 2026-11-01T00:00, end: 2026-11-01T01:00, team: }\n",
	} {
		if _, err := ParseConfig([]byte(text), time.UTC); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
}

// The rules list inside a kind, or outside it: during:sale in the search
// box is during=sale, each rule counted by its firings during the sale
// over the weeks picked; outside=sale the rest; during:deploy, the rules
// with firings during deploys. A kind it can't be is said.
func TestRulesDuringKind(t *testing.T) {
	s := pagesServer(t)
	h := s.Handler()
	code, _, hd := get(t, h, "/rules?q=during%3Asale")
	if code != http.StatusSeeOther || hd.Get("Location") != "/rules?during=sale" {
		t.Fatalf("during:sale: %d %v", code, hd)
	}
	rows := func(path string) map[string]string {
		t.Helper()
		code, body, _ := get(t, h, path)
		if code != 200 {
			t.Fatalf("%s: %d\n%s", path, code, body)
		}
		out := map[string]string{}
		for _, part := range strings.Split(body, `<td class="rule"><a href="/rules/`)[1:] {
			name, _, _ := strings.Cut(part[:strings.Index(part, `"`)], "?")
			// The firings, the first number after the rule's cells.
			fired := part[strings.Index(part, `<td class="n">`)+len(`<td class="n">`):]
			out[name] = fired[:strings.Index(fired, "<")]
		}
		return out
	}
	all, during, outside := rows("/rules?weeks=12"), rows("/rules?weeks=12&during=sale"), rows("/rules?weeks=12&outside=sale")
	if len(during) == 0 || len(during) >= len(all) && len(outside) == 0 {
		t.Fatalf("during the sale %v, outside %v, of %v", during, outside, all)
	}
	for name, n := range during {
		var a, d, o int
		fmt.Sscan(all[name], &a)
		fmt.Sscan(n, &d)
		fmt.Sscan(outside[name], &o)
		if d+o != a || d == 0 {
			t.Errorf("%s: %d during the sale and %d outside it, of %d", name, d, o, a)
		}
	}
	if code, body, _ := get(t, h, "/rules?weeks=12&during=sale"); code != 200 || !strings.Contains(body, `<bdi>sale</bdi>`) || !strings.Contains(body, `aria-current="page">during <bdi>sale</bdi>`) {
		t.Errorf("the chip and the link of the kind shown: %d", code)
	}
	if dep := rows("/rules?weeks=12&during=deploy"); len(dep) != 1 || dep["Flapping"] == "" {
		t.Errorf("during deploys: %v", dep)
	}
	if code, body, _ := get(t, h, "/rules?q=during%3ANo+outside%3Adeploy"); code != 200 || !strings.Contains(body, "during:No") {
		t.Errorf("kinds it can't be: %d", code)
	}
}

// The digest and a report's marked section: each occurrence on in the
// span, inside its kind beside the weeks before, and each kind's firings
// inside beside outside; digest.ready carries the numbers. A span with
// none says so.
func TestMarkedDigest(t *testing.T) {
	s := nocServer(t)
	ctx := context.Background()
	from := s.l.Midnight(ledger.Week(s.l.Day(now)) - 7)
	to := from.Add(7 * 24 * time.Hour)
	rep, err := s.digestReport(ctx, from, to, s.cfg.Options)
	if err != nil {
		t.Fatal(err)
	}
	text, err := s.digestText(ctx, rep, nil, from, to, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"During sale: 3 firings, 0 of them catching something; precision 0% inside,", "During launch: 1 firings, 1 of them catching something; precision 100% inside,",
		"sale Sunday sale, 2026-11-01 19:00 → 2026-11-01 20:45: 3 firings, precision 0%"} {
		if !strings.Contains(text, want) {
			t.Errorf("digest: no %q in\n%s", want, text)
		}
	}
	about := digestAbout(rep, text, "", nil)
	if err := s.aboutMore(ctx, about, rep, from, to, nil); err != nil {
		t.Fatal(err)
	}
	mk, ok := about["marked"].(*markedReport)
	if !ok || len(mk.Occurrences) != 2 || mk.Kinds["launch"].Caught != 1 || mk.Occurrences[1].Inside.Impacts.Caught != 1 {
		t.Fatalf("digest.ready's marked: %+v", about["marked"])
	}
	b, _ := json.Marshal(about["marked"])
	for _, f := range []string{`"kinds"`, `"occurrences"`, `"precision_outside"`, `"inside"`, `"before"`, `"key"`} {
		if !strings.Contains(string(b), f) {
			t.Errorf("digest.ready's marked: no %s in %s", f, b)
		}
	}
	got, err := s.preview(ctx, ledger.Scheduled{Name: "sales", Schedule: "weekly mon@09:00", Sections: []string{"marked"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Text, "During sale: 3 firings") || got.Numbers["marked"] == nil {
		t.Errorf("a report's marked section:\n%s", got.Text)
	}
	// A week with none: said.
	text, err = s.digestText(ctx, rep, nil, from.Add(-14*24*time.Hour), from.Add(-7*24*time.Hour), nil)
	if err != nil || strings.Contains(text, "During sale") {
		t.Errorf("a week with no marked window: %v\n%s", err, text)
	}
}

// Where else a kind shows: a chip on the incidents during it, a rule's
// lines inside each kind beside outside, its latest firings, the
// overview's card of the latest occurrence, and MCP's firings.
func TestMarkedShown(t *testing.T) {
	s := pagesServer(t)
	h := s.Handler()
	for path, want := range map[string]string{
		"/incidents?weeks=4":                  `INC-1</a> Checkout down <span class="badge status-unknown"><bdi>sale</bdi></span>`,
		"/incidents/INC-1":                    `<span class="badge status-unknown"><bdi>sale</bdi></span>`,
		"/rules/Flapping?tab=firings":         `<h2>During marked windows</h2>`,
		"/rules/Flapping?tab=firings&weeks=4": `<span class="badge status-unknown"><bdi>launch</bdi></span>`,
		"/":                                   `id="marked-tile-h"`,
	} {
		code, body, _ := get(t, h, path)
		if code != 200 || !strings.Contains(body, want) {
			t.Errorf("%s: %d, no %s", path, code, want)
		}
	}
	_, body, _ := get(t, h, "/rules/Flapping?tab=firings")
	if !strings.Contains(body, "4 firings, 0 of them catching something, 3 re-fired: precision 0% inside, – outside.") {
		t.Error("/rules/Flapping: its line inside the launch")
	}
	f, err := s.l.Firings(context.Background(), ledger.FiringQuery{From: now.Add(-72 * time.Hour), To: now, Rule: "Flapping"})
	if err != nil || len(f) == 0 || !slices.Contains(firingOutOf(f[0]).Marked, "launch") {
		t.Errorf("MCP's firing: %+v %v", f, err)
	}
}

// A marked window kept in the UI is told, marked.kept: to the webhook as
// JSON, with the window and a link to its page, and to Slack as a line.
func TestMarkedKeptTold(t *testing.T) {
	var mu sync.Mutex
	got := map[string][]string{}
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		got[r.URL.Path] = append(got[r.URL.Path], string(b))
	}))
	defer recv.Close()
	dir := t.TempDir()
	hook, slack := filepath.Join(dir, "hook"), filepath.Join(dir, "slack")
	os.WriteFile(hook, []byte(recv.URL+"/hook"), 0o600)
	os.WriteFile(slack, []byte(recv.URL+"/slack"), 0o600)
	s, _ := server(t, func(c *Config) {
		c.UserHeader, c.NotifyWebhook, c.Slack, c.NotifySlack = "X-Forwarded-Email", hook, slack, true
		c.ExternalURL = "https://lapilli.example.com/"
	})
	h := s.Handler()
	if code, body := api(t, h, "POST", "/api/v1/marked", `{"kind": "sale", "title": "Autumn sale", "start": "2026-11-20T00:00:00Z", "end": "2026-11-21T00:00:00Z"}`); code != http.StatusCreated {
		t.Fatalf("POST: %d %s", code, body)
	}
	for _, d := range s.dests() {
		if err := s.sendDue(context.Background(), d, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got["/hook"]) != 1 || !strings.Contains(got["/hook"][0], `"event":"marked.kept"`) || !strings.Contains(got["/hook"][0], `"url":"https://lapilli.example.com/marked/1"`) ||
		!strings.Contains(got["/hook"][0], `"kind":"sale"`) {
		t.Errorf("the webhook: %v", got["/hook"])
	}
	if len(got["/slack"]) != 1 || !strings.Contains(got["/slack"][0], `bo@example.com kept a marked window of sale, \"Autumn sale\", from 2026-11-20 00:00`) {
		t.Errorf("Slack: %v", got["/slack"])
	}
}

// An expression changed (its --mark-match) whose read of the last four
// weeks fails: its windows and key are kept as they were, the runs' reads
// of it wait, and the next round reads it again, keeping the new key.
func TestMarkExpressionChangedWaits(t *testing.T) {
	s, _ := server(t, func(c *Config) { c.Marks = []Mark{{Kind: "sale", Expr: "sale_active == 1"}} })
	ctx := context.Background()
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	before, _ := s.l.AllMarked(ctx)
	k1, _ := s.l.MarkKeyOf(ctx, "sale")
	if len(before) != 2 || k1 != s.cfg.Marks[0].Key() {
		t.Fatalf("first: %d windows, key %s", len(before), k1)
	}
	hist(s).failLong = map[string]bool{"sale_active == 1": true}
	s.cfg.Marks[0].Match = []string{"region"}
	s.jobs(ctx, false)
	// A run's read, of the hours before: waits.
	if err := s.keepMarks(ctx, s.readMarks(ctx, s.cfg.Marks, now.Add(-2*time.Hour), now, false), s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	after, _ := s.l.AllMarked(ctx)
	if k, _ := s.l.MarkKeyOf(ctx, "sale"); k != k1 || len(after) != 2 || len(after[0].Labels) != 0 {
		t.Errorf("with the read again failing: key %s (was %s), %+v", k, k1, after)
	}
	hist(s).failLong = nil
	s.jobs(ctx, false)
	after, _ = s.l.AllMarked(ctx)
	if k, _ := s.l.MarkKeyOf(ctx, "sale"); k != s.cfg.Marks[0].Key() || len(after) != 2 || after[0].Labels["region"] == "" {
		t.Errorf("read again: key %s, %+v", k, after)
	}
}

// With every --mark taken away, the keys of the expressions are let go
// all the same: their windows stay as found.
func TestMarksForgottenWithoutAny(t *testing.T) {
	s, _ := server(t, func(c *Config) { c.Marks = []Mark{{Kind: "sale", Expr: "sale_active == 1"}} })
	ctx := context.Background()
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	s.cfg.Marks = nil
	s.forgotMarks.Store(false)
	s.jobs(ctx, false)
	if k, _ := s.l.MarkKeyOf(ctx, "sale"); k != "" {
		t.Errorf("the key with no --mark: %q", k)
	}
	if ms, _ := s.l.AllMarked(ctx); len(ms) != 2 {
		t.Errorf("%d windows: they stay as found", len(ms))
	}
}

// The summary of a week: windows counts each occurrence; an incident is
// during a team's window only when the team owns it, during everyone's
// when it agrees.
func TestMarkedSummaryWindowsAndTeams(t *testing.T) {
	s := nocServer(t)
	ctx := context.Background()
	search, ok := func() (ledger.Team, bool) { o, _ := s.owners(ctx); return o.team("search") }()
	if !ok {
		t.Fatal("no search team")
	}
	// INC-1, 2026-11-02 06:01, checkout's, missed: search owns nothing of it.
	at := time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)
	for _, m := range []ledger.Marked{
		{Kind: "campaign", Title: "Search's campaign", Start: at, End: at.Add(time.Hour), TeamID: search.ID},
		{Kind: "promo", Title: "Morning promo", Start: at, End: at.Add(time.Hour)},
		{Kind: "promo", Title: "Evening promo", Start: at.Add(12 * time.Hour), End: at.Add(13 * time.Hour)},
	} {
		if _, err := s.l.PutMarked(ctx, m, "ana", s.cfg.Options); err != nil {
			t.Fatal(err)
		}
	}
	_, body, _ := get(t, s.Handler(), "/api/v1/summary?from=2026-11-02&to=2026-11-02")
	var sum summary
	if err := json.Unmarshal([]byte(body), &sum); err != nil || len(sum.Rows) != 1 {
		t.Fatalf("%v %s", err, body)
	}
	m := sum.Rows[0].Marked
	if m["promo"].Windows != 2 || m["campaign"].Windows != 1 || m["promo"].Incidents.Total != 1 || m["promo"].Incidents.Missed != 1 || m["campaign"].Incidents.Total != 0 {
		t.Errorf("marked: %+v", m)
	}
}

// The CSV's marked: the kinds each firing came during, a JSON array, []
// for none.
func TestMarkedCSV(t *testing.T) {
	s := nocServer(t)
	_, body, _ := get(t, s.Handler(), "/api/v1/report?format=csv&from="+nocBase.Add(-24*time.Hour).UTC().Format(time.RFC3339)+"&to="+nocBase.Add(24*time.Hour).UTC().Format(time.RFC3339))
	rows, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil || rows[0][12] != "marked" {
		t.Fatalf("%v %v", err, rows[0])
	}
	count := map[string]int{}
	for _, r := range rows[1:] {
		count[r[0]+" "+r[12]]++
	}
	if count[`Flap ["sale"]`] != 2 || count[`Flap ["launch"]`] != 1 || count[`Noise ["sale"]`] != 1 || count["Noise []"] == 0 {
		t.Errorf("the marked column: %v", count)
	}
}

// Without a marked window nothing a reader sees changes either: the
// summary, the rule and incident metrics, the report, the digest and the
// pages, served before a window was kept and after it was kept and
// deleted, are the same, byte for byte; and with one kept, they differ.
func TestMarkedNothingChangesServed(t *testing.T) {
	s := nocServerOf(t, false)
	ctx := context.Background()
	h := s.Handler()
	from := s.l.Midnight(ledger.Week(s.l.Day(now)) - 7)
	to := from.Add(7 * 24 * time.Hour)
	span := "from=" + from.UTC().Format(time.RFC3339) + "&to=" + to.UTC().Format(time.RFC3339)
	snap := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		for _, p := range []string{"/api/v1/summary?from=2026-10-26&to=2026-11-02&team=*", "/api/v1/report?" + span, "/api/v1/report?format=csv&" + span,
			"/", "/rules?weeks=12", "/rules/Flap?tab=firings", "/incidents?weeks=12", "/firings?day=2026-11-01", "/api/v1/rules"} {
			code, body, _ := get(t, h, p)
			if code != 200 {
				t.Fatalf("%s: %d", p, code)
			}
			out[p] = body
		}
		_, body, _ := get(t, h, "/metrics")
		var lines []string
		for _, l := range strings.Split(body, "\n") {
			if strings.HasPrefix(l, "lapilli_rule_") || strings.HasPrefix(l, "lapilli_incident") || strings.HasPrefix(l, "lapilli_refired") || strings.Contains(l, "marked") {
				lines = append(lines, l)
			}
		}
		out["/metrics"] = strings.Join(lines, "\n")
		rep, err := s.digestReport(ctx, from, to, s.cfg.Options)
		if err != nil {
			t.Fatal(err)
		}
		if out["digest"], err = s.digestText(ctx, rep, nil, from, to, nil); err != nil {
			t.Fatal(err)
		}
		return out
	}
	want := snap()
	if strings.Contains(want["/metrics"], "marked") || strings.Contains(want["/api/v1/report?"+span], `"marked"`) {
		t.Error("with no marked window, a marked metric or field")
	}
	id, err := s.l.PutMarked(ctx, ledger.Marked{Kind: "sale", Title: "Sale", Start: nocBase.Add(-24 * time.Hour), End: nocBase.Add(24 * time.Hour)}, "ana", s.cfg.Options)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.l.JudgePending(ctx, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	with := snap()
	for _, p := range []string{"/api/v1/summary?from=2026-10-26&to=2026-11-02&team=*", "/metrics", "digest", "/rules?weeks=12"} {
		if with[p] == want[p] {
			t.Errorf("%s: the same with a window that marks the week's firings", p)
		}
	}
	if err := s.l.DeleteMarked(ctx, id, "ana", s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if err := s.l.JudgePending(ctx, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	got := snap()
	for p := range want {
		if got[p] != want[p] {
			t.Errorf("%s after the window was deleted differs from before it was kept", p)
		}
	}
}

// The digest beside the marked windows: a goal that can't be read fails
// it, as before, to be tried again, rather than going out without goals.
func TestDigestGoalsFailStill(t *testing.T) {
	s := nocServer(t)
	ctx := context.Background()
	// A goal kept, its severities not read back.
	ledgerExec(t, s, "INSERT INTO goals (name, measure, at_least, critical) VALUES ('caught', 'critical.share_caught', '0.8', 'not a list')")
	from := s.l.Midnight(ledger.Week(s.l.Day(now)) - 7)
	to := from.Add(7 * 24 * time.Hour)
	rep, err := s.digestReport(ctx, from, to, s.cfg.Options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.digestText(ctx, rep, nil, from, to, nil); err == nil {
		t.Error("a digest whose goals can't be read went out")
	}
}
