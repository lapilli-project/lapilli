package serve

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// activityServer is verdictServer with changes of each kind to find: a
// note on payments' Errors, one on search's SearchLatency, a task of
// payments', a role given, and a quiet window named for the rules' path.
func activityServer(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	s := verdictServer(t, nil)
	o, err := s.owners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pay, _ := o.team("payments")
	if err := s.l.PutNote(ctx, ledger.Note{Rule: "Errors", Note: "pay's"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if err := s.l.PutNote(ctx, ledger.Note{Rule: "SearchLatency", Note: "search's"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutTask(ctx, ledger.Task{Title: "tune it", Kind: "tune", TeamID: pay.ID}, "ben"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutTask(ctx, ledger.Task{Title: "no one's"}, "ben"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutWindow(ctx, ledger.Window{Title: s.cfg.RulesPath, Start: now.Add(-2 * time.Hour), End: now.Add(-time.Hour)}, "ana", s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	return s
}

// activityOf is what GET /api/v1/activity gives who for a query.
func activityOf(t *testing.T, h http.Handler, query, who string) ([]apiLogged, string) {
	t.Helper()
	code, body := getAs(t, h, "/api/v1/activity?"+query, who)
	if code != http.StatusOK {
		t.Fatalf("%s as %s: %d %s", query, who, code, body)
	}
	var out struct {
		Activity []apiLogged `json:"activity"`
		Next     string      `json:"next"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	return out.Activity, out.Next
}

func whatsOf(ls []apiLogged) []string {
	var out []string
	for _, l := range ls {
		out = append(out, l.What)
	}
	return out
}

// The activity log filters by who, kind, team and period, as the page and
// the API alike read them, and refuses what it can't read.
func TestActivityFilters(t *testing.T) {
	parallel(t)
	s := activityServer(t)
	h := s.Handler()
	got, _ := activityOf(t, h, "who=ben", vViewer)
	if w := whatsOf(got); len(w) != 2 || !strings.HasPrefix(w[0], "task ") || !strings.HasPrefix(w[1], "task ") {
		t.Errorf("ben's: %q", w)
	}
	got, _ = activityOf(t, h, "kind=note", vViewer)
	if w := whatsOf(got); len(w) != 2 || w[0] != "note on SearchLatency kept" {
		t.Errorf("notes: %q", w)
	}
	// payments': its team, its rule's note and its task; not search's
	// note, nor the task of no team.
	got, _ = activityOf(t, h, "team=payments", vViewer)
	w := strings.Join(whatsOf(got), "|")
	if !strings.Contains(w, "note on Errors kept") || !strings.Contains(w, "team payments kept") || strings.Count(w, "task ") != 1 ||
		strings.Contains(w, "SearchLatency") || strings.Contains(w, "quiet window") {
		t.Errorf("payments': %s", w)
	}
	got, _ = activityOf(t, h, "since="+now.AddDate(1, 0, 0).Format("2006-01-02"), vViewer)
	if len(got) != 0 {
		t.Errorf("since next year: %q", whatsOf(got))
	}
	for _, q := range []string{"kind=everything", "team=nobody", "since=yesterday", "until=2026-01-01&since=2026-02-01", "limit=0", "limit=1001",
		"format=xml", "before=x", "who=" + strings.Repeat("x", ledger.MaxName+1)} {
		if code, body := getAs(t, h, "/api/v1/activity?"+q, vViewer); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", q, code, body)
		}
		if code, _ := getAs(t, h, "/activity?"+q, vViewer); code != http.StatusBadRequest && !strings.HasPrefix(q, "limit") && !strings.HasPrefix(q, "format") {
			t.Errorf("the page with %s: %d", q, code)
		}
	}
}

// The activity log's period is from and to, as every other period on the
// API is; since and until are still taken as their old names, to taking its
// day whole as until did; a name and its old one together are refused,
// naming both.
func TestActivityFromTo(t *testing.T) {
	parallel(t)
	s := activityServer(t)
	h := s.Handler()
	day := now.AddDate(1, 0, 0).Format("2006-01-02")
	next := now.AddDate(1, 0, 1).Format("2006-01-02")
	all, _ := activityOf(t, h, "", vViewer)
	if len(all) == 0 {
		t.Fatal("no changes kept")
	}
	for _, c := range [][2]string{{"from=" + next, "since=" + next}, {"to=" + day, "until=" + day}, {"from=" + day + "&to=" + day, "since=" + day + "&until=" + day}} {
		a, _ := activityOf(t, h, c[0], vViewer)
		b, _ := activityOf(t, h, c[1], vViewer)
		if strings.Join(whatsOf(a), "|") != strings.Join(whatsOf(b), "|") || len(a) != len(b) {
			t.Errorf("%s: %q, but %s: %q", c[0], whatsOf(a), c[1], whatsOf(b))
		}
	}
	if got, _ := activityOf(t, h, "from="+next, vViewer); len(got) != 0 {
		t.Errorf("from past the changes: %q", whatsOf(got))
	}
	if got, _ := activityOf(t, h, "to="+next, vViewer); len(got) != len(all) {
		t.Errorf("to a day past the changes: %d of %d", len(got), len(all))
	}
	for q, names := range map[string][]string{"from=" + day + "&since=" + day: {"from", "since"}, "to=" + day + "&until=" + day: {"to", "until"}} {
		code, body := getAs(t, h, "/api/v1/activity?"+q, vViewer)
		if code != http.StatusBadRequest || !strings.Contains(body, names[0]) || !strings.Contains(body, names[1]) {
			t.Errorf("%s: %d %s", q, code, body)
		}
	}
	if code, body := getAs(t, h, "/api/v1/activity?from=2026-02-01&to=2026-01-01", vViewer); code != http.StatusBadRequest {
		t.Errorf("to before from: %d %s", code, body)
	}
}

// Past a page, the activity log goes on from where it ended, on the page
// as through the API, with nothing given twice or left out.
func TestActivityPages(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s := activityServer(t)
	for i := range 150 {
		if err := s.l.PutNote(ctx, ledger.Note{Rule: "Errors", Note: fmt.Sprint(i)}, "cy"); err != nil {
			t.Fatal(err)
		}
	}
	h := s.Handler()
	_, page := getAs(t, h, "/activity?who=cy", vViewer)
	if n := strings.Count(page, "note on Errors kept"); n != activityShown {
		t.Errorf("the first page has %d", n)
	}
	i := strings.Index(page, `">Older</a>`)
	j := strings.LastIndex(page[:max(i, 0)], `href="`)
	if i < 0 || j < 0 {
		t.Fatalf("no link to the older ones")
	}
	next := strings.ReplaceAll(page[j+len(`href="`):i], "&amp;", "&")
	_, older := getAs(t, h, next, vViewer)
	if n := strings.Count(older, "note on Errors kept"); n != 50 || strings.Contains(older, `">Older</a>`) || !strings.Contains(older, "The newest") {
		t.Errorf("%s has %d", next, n)
	}
	seen := map[int64]bool{}
	q := "who=cy&limit=60"
	for range 5 {
		got, n := activityOf(t, h, q, vViewer)
		for _, c := range got {
			if seen[c.ID] {
				t.Errorf("%d given twice", c.ID)
			}
			seen[c.ID] = true
		}
		if n == "" {
			break
		}
		q = "who=cy&limit=60&before=" + url.QueryEscape(n)
	}
	if len(seen) != 150 {
		t.Errorf("%d of 150 read through the API", len(seen))
	}
}

// The roles given and taken are an admin's to read, on the page, through
// the API and in the export, as on Run history; anyone but an admin reads
// the paths of serve's files as the flags that gave them.
func TestActivityWhoReadsWhat(t *testing.T) {
	parallel(t)
	s := activityServer(t)
	h := s.Handler()
	for _, c := range []struct {
		who   string
		admin bool
	}{{vViewer, false}, {vEditor, false}, {vAdmin, true}} {
		for _, p := range []string{"/activity", "/activity?kind=role", "/api/v1/activity", "/api/v1/activity?kind=role", "/api/v1/activity?format=csv"} {
			code, body := getAs(t, h, p, c.who)
			if code != http.StatusOK {
				t.Fatalf("%s as %s: %d", p, c.who, code)
			}
			if has := strings.Contains(body, ledger.RoleGiven); has != c.admin {
				t.Errorf("%s as %s: roles shown %v", p, c.who, has)
			}
			if p != "/activity?kind=role" && p != "/api/v1/activity?kind=role" {
				if has := strings.Contains(body, s.cfg.RulesPath); has != c.admin {
					t.Errorf("%s as %s: the rules' path shown %v", p, c.who, has)
				}
			}
		}
	}
	// Who no role lets in reads none of it.
	s.cfg.Viewers = []string{vAdmin, vEditor}
	for _, p := range []string{"/activity", "/api/v1/activity", "/api/v1/activity?format=csv"} {
		if code, body := getAs(t, h, p, "stranger@example.com"); code != http.StatusForbidden || strings.Contains(body, "note on") {
			t.Errorf("%s as a stranger: %d", p, code)
		}
	}
}

// The CSV export takes no cell for a formula: one that starts with =, +,
// -, @, a tab or a carriage return, or their full-width forms, starts with
// '. It is the API's, with the page's filters, and kept by no cache.
func TestActivityCSV(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s := activityServer(t)
	evil := []string{`=HYPERLINK("https://evil.example","x")`, "+1+1", "-2+3", "@SUM(A1)", "\t=1", "  =1", "＝1"}
	for _, by := range evil {
		if err := s.l.PutNote(ctx, ledger.Note{Rule: "Errors", Note: by}, by); err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/activity?format=csv&kind=note", nil)
	r.Header.Set("X-Forwarded-Email", vViewer)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	code, body, hd := w.Code, w.Body.String(), w.Header()
	if code != http.StatusOK || !strings.HasPrefix(hd.Get("Content-Type"), "text/csv") || hd.Get("Cache-Control") != "no-store" ||
		!strings.Contains(hd.Get("Content-Disposition"), "attachment") {
		t.Fatalf("%d %v", code, hd)
	}
	rows, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rows[0], ",") != "id,at,by,kind,what,before,after" {
		t.Errorf("the header: %v", rows[0])
	}
	got := map[string]bool{}
	for _, r := range rows[1:] {
		for _, cell := range r {
			if t0 := strings.TrimLeft(cell, " "); t0 != "" && strings.ContainsAny(t0[:1], "=+-@\t\r") || strings.HasPrefix(cell, "＝") {
				t.Errorf("a cell a spreadsheet takes for a formula: %q", cell)
			}
		}
		got[r[2]] = true
	}
	for _, by := range evil {
		if !got["'"+by] {
			t.Errorf("%q isn't said as text: %v", by, got)
		}
	}
	if csvCell("ana") != "ana" || csvCell("") != "" || csvCell("\r=1") != "'\r=1" || csvCell("＋1") != "'＋1" {
		t.Error("csvCell")
	}
	// After a space of Unicode's, or one of no width.
	for _, v := range []string{"\u00a0=1+1", "\u3000@SUM(A1)", "\u2003-1", " \u00a0 +1", "\u202f=1", "\u200b=1", "\ufeff=1"} {
		if got := csvCell(v); got != "'"+v {
			t.Errorf("csvCell(%q) = %q", v, got)
		}
	}
	for _, v := range []string{"\u00a0ana", "\u3000", "ana = 1", "a-b"} {
		if got := csvCell(v); got != v {
			t.Errorf("csvCell(%q) = %q", v, got)
		}
	}
}

// --keep-activity keeps the activity log past --keep: a change older than
// the firings kept stays, one older than the activity log kept goes.
func TestKeepActivity(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, _ := server(t, func(c *Config) { c.Keep, c.KeepActivity = 30*24*time.Hour, 60*24*time.Hour })
	if err := s.l.PutNote(ctx, ledger.Note{Rule: "Errors", Note: "x"}, "ana"); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		t.Helper()
		cs, err := s.l.Changes(ctx, 1000)
		if err != nil {
			t.Fatal(err)
		}
		return len(cs)
	}
	before := count()
	// The change, made now by the wall clock, is 45 days old: past the
	// firings' 30, within the activity log's 60.
	s.now = func() time.Time { return time.Now().Add(45 * 24 * time.Hour) }
	if err := s.prune(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != before {
		t.Errorf("pruned at 45 days: %d of %d changes kept", n, before)
	}
	s.now = func() time.Time { return time.Now().Add(61 * 24 * time.Hour) }
	s.l.SetMeta(ctx, "pruned", "")
	if err := s.prune(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 0 {
		t.Errorf("pruned at 61 days: %d changes kept", n)
	}
}

// A team's changes are found by looking each task and proposal up: a
// request looks up activityLookups at most, says where it stopped, and the
// next goes on from there, so that no change is passed over.
func TestActivityTeamBounded(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s := activityServer(t)
	for i := range activityLookups + 50 {
		if _, err := s.l.PutTask(ctx, ledger.Task{Title: fmt.Sprint("no one's ", i)}, "dan"); err != nil {
			t.Fatal(err)
		}
	}
	h := s.Handler()
	q, pages, found := "team=payments", 0, false
	for ; pages < 10; pages++ {
		code, body := getAs(t, h, "/api/v1/activity?"+q, vViewer)
		var out struct {
			Activity []apiLogged `json:"activity"`
			Next     string      `json:"next"`
			Cut      bool        `json:"cut"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(body), &out) != nil {
			t.Fatalf("%s: %d %s", q, code, body)
		}
		if pages == 0 && (!out.Cut || out.Next == "" || len(out.Activity) != 0) {
			t.Errorf("the first request didn't stop: %+v", out)
		}
		for _, c := range out.Activity {
			found = found || c.What == "note on Errors kept"
			if c.By == "dan" {
				t.Errorf("another team's task given: %+v", c)
			}
		}
		if out.Next == "" {
			break
		}
		q = "team=payments&before=" + url.QueryEscape(out.Next)
	}
	if !found || pages < 1 {
		t.Errorf("payments' note not found in %d requests", pages+1)
	}
}

// GET /api/v1/activity is worked out as a page is: with every page's turn
// taken, it waits its turn and is answered 503, to try again; given one,
// it answers, and gives the turn back. A team's changes come with their
// before and after, read once each is kept.
func TestActivityTakesATurn(t *testing.T) {
	s := activityServer(t)
	h := s.Handler()
	was := renderWait
	defer func() { renderWait = was }()
	renderWait = 50 * time.Millisecond
	for range pageRenders {
		s.renders <- struct{}{}
	}
	if code, body := getAs(t, h, "/api/v1/activity?format=csv", vViewer); code != http.StatusServiceUnavailable || !strings.Contains(body, "busy with many pages at once") {
		t.Errorf("all turns taken: %d %s", code, body)
	}
	<-s.renders
	got, _ := activityOf(t, h, "team=payments", vViewer)
	i := slices.IndexFunc(got, func(c apiLogged) bool { return strings.HasPrefix(c.What, "task ") })
	if i < 0 || !strings.Contains(string(got[i].After), `"tune it"`) {
		t.Errorf("payments' task without its after: %+v", got)
	}
	if len(s.renders) != pageRenders-1 {
		t.Errorf("%d turns held, not %d", len(s.renders), pageRenders-1)
	}
	for range pageRenders - 1 {
		<-s.renders
	}
}

// An export ends where its changes' text passes activityBytes, and says
// where to go on from: paged through, as JSON and as CSV alike, every
// change is given once, a team's too, whose before and after are read
// after it is looked through.
func TestActivityBytesBounded(t *testing.T) {
	s := activityServer(t)
	h := s.Handler()
	ctx := context.Background()
	for i := range 20 {
		if err := s.l.PutNote(ctx, ledger.Note{Rule: "Errors", Note: fmt.Sprintf("%02d %s", i, strings.Repeat("n", 4000))}, "ana"); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{"", "team=payments"} {
		whole, next := activityOf(t, h, query, vViewer)
		if next != "" || len(whole) < 20 {
			t.Fatalf("%q under 8 MiB: %d changes, next %q", query, len(whole), next)
		}
		func() {
			was := activityBytes
			defer func() { activityBytes = was }()
			activityBytes = 30_000
			var ids []int64
			before := ""
			for pages := 0; ; pages++ {
				if pages > len(whole) {
					t.Fatalf("%q: no end to the pages", query)
				}
				q := query
				if before != "" {
					q += "&before=" + url.QueryEscape(before)
				}
				code, body := getAs(t, h, "/api/v1/activity?"+q, vViewer)
				if code != http.StatusOK {
					t.Fatalf("%q: %d %s", q, code, body)
				}
				// Three notes' changes of 8 KB, and what JSON adds, on one
				// line: not indented.
				if len(body) > activityBytes+2_000 || strings.Count(body, "\n") != 1 {
					t.Errorf("%q: a page of %d bytes, past %d, or of %d lines", q, len(body), activityBytes, strings.Count(body, "\n"))
				}
				var out struct {
					Activity []apiLogged `json:"activity"`
					Next     string      `json:"next"`
				}
				if err := json.Unmarshal([]byte(body), &out); err != nil {
					t.Fatal(err)
				}
				for _, c := range out.Activity {
					ids = append(ids, c.ID)
				}
				csvQ := q + "&format=csv"
				_, rows, head := getAsWithHeaders(t, h, "/api/v1/activity?"+strings.TrimPrefix(csvQ, "&"), vViewer)
				records, err := csv.NewReader(strings.NewReader(rows)).ReadAll()
				if err != nil {
					t.Fatal(err)
				}
				if len(records)-1 != len(out.Activity) || (out.Next != "") != (head.Get("Link") != "") {
					t.Errorf("%q: CSV of %d changes, link %q; JSON of %d, next %q", q, len(records)-1, head.Get("Link"), len(out.Activity), out.Next)
				}
				if out.Next == "" {
					break
				}
				before = out.Next
			}
			if len(ids) != len(whole) {
				t.Errorf("%q: paged through %d changes, not %d", query, len(ids), len(whole))
			}
			for i, c := range whole {
				if i < len(ids) && ids[i] != c.ID {
					t.Errorf("%q: change %d is %d paged through, %d whole", query, i, ids[i], c.ID)
					break
				}
			}
		}()
	}
}

// unread is a reader who doesn't read: its first write waits until goOn
// is closed, after writing is.
type unread struct {
	*httptest.ResponseRecorder
	once          sync.Once
	writing, goOn chan struct{}
}

func (w *unread) Write(b []byte) (int, error) {
	w.once.Do(func() {
		close(w.writing)
		<-w.goOn
	})
	return w.ResponseRecorder.Write(b)
}

// An export holds one of the exports' turns until its reader has read it,
// and no page's: what readers who don't read make serve hold is a few
// exports', and pages are worked out meanwhile.
func TestActivityExportHeldUntilRead(t *testing.T) {
	s := activityServer(t)
	h := s.Handler()
	for _, format := range []string{"json", "csv"} {
		w := &unread{ResponseRecorder: httptest.NewRecorder(), writing: make(chan struct{}), goOn: make(chan struct{})}
		r := httptest.NewRequest(http.MethodGet, "/api/v1/activity?format="+format, nil)
		r.Header.Set("X-Forwarded-Email", vViewer)
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.ServeHTTP(w, r)
		}()
		<-w.writing
		if len(s.exports) != 1 || len(s.renders) != 0 {
			t.Errorf("%s being read: %d exports' turns held, %d pages'; want 1 and 0", format, len(s.exports), len(s.renders))
		}
		close(w.goOn)
		<-done
		if len(s.exports) != 0 || w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "pay's") {
			t.Errorf("%s read: %d exports' turns held, %d %s", format, len(s.exports), w.Code, w.Body.String())
		}
	}
	if cap(s.exports) != activityExports {
		t.Errorf("%d exports' turns, not %d", cap(s.exports), activityExports)
	}
	// All taken, the next waits, then is told to come back.
	was := renderWait
	defer func() { renderWait = was }()
	renderWait = time.Millisecond
	for range activityExports {
		s.exports <- struct{}{}
	}
	if code, body := getAs(t, h, "/api/v1/activity", vViewer); code != http.StatusServiceUnavailable {
		t.Errorf("every export's turn taken: %d %s", code, body)
	}
	for range activityExports {
		<-s.exports
	}
}

// An export is held to activityBytes as written, not as kept: JSON writes
// < as six bytes, and CSV a quote as two. Past them it ends, but for its
// first change, and goes on from there, every change given once. A team's
// changes are asked of the ledger 200 at a time, as the others' are.
func TestActivityBytesAsWritten(t *testing.T) {
	s := activityServer(t)
	h := s.Handler()
	ctx := context.Background()
	for i := range 12 {
		if err := s.l.PutNote(ctx, ledger.Note{Rule: "Errors", Note: fmt.Sprintf("%02d %s", i, strings.Repeat(`<"`, 1000))}, "ana"); err != nil {
			t.Fatal(err)
		}
	}
	whole, _ := activityOf(t, h, "", vViewer)
	was := activityBytes
	defer func() { activityBytes = was }()
	activityBytes = 30_000
	for _, format := range []string{"json", "csv"} {
		seen := 0
		before := ""
		for pages := 0; ; pages++ {
			if pages > len(whole) {
				t.Fatalf("%s: no end to the pages", format)
			}
			q := "format=" + format
			if before != "" {
				q += "&before=" + url.QueryEscape(before)
			}
			code, body, head := getAsWithHeaders(t, h, "/api/v1/activity?"+q, vViewer)
			if code != http.StatusOK {
				t.Fatalf("%s: %d %s", q, code, body)
			}
			n, next := 0, ""
			if format == "json" {
				var out struct {
					Activity []apiLogged `json:"activity"`
					Next     string      `json:"next"`
				}
				if err := json.Unmarshal([]byte(body), &out); err != nil {
					t.Fatal(err)
				}
				n, next = len(out.Activity), out.Next
			} else {
				records, err := csv.NewReader(strings.NewReader(body)).ReadAll()
				if err != nil {
					t.Fatal(err)
				}
				n = len(records) - 1
				if l := head.Get("Link"); l != "" {
					u, _ := url.Parse(strings.Trim(strings.SplitN(l, ";", 2)[0], "<>"))
					next = u.Query().Get("before")
				}
			}
			if n > 1 && len(body) > activityBytes {
				t.Errorf("%s: %d changes in %d bytes, past %d", q, n, len(body), activityBytes)
			}
			seen += n
			if next == "" {
				break
			}
			before = next
		}
		if seen != len(whole) {
			t.Errorf("%s: paged through %d changes, not %d", format, seen, len(whole))
		}
	}
	// A team's, 200 a read.
	defer func(r func(*ledger.Ledger, context.Context, ledger.ActivityQuery) ([]ledger.LogEntry, error)) {
		readActivity = r
	}(readActivity)
	most := 0
	readActivity = func(l *ledger.Ledger, ctx context.Context, q ledger.ActivityQuery) ([]ledger.LogEntry, error) {
		most = max(most, q.Limit)
		return l.Activity(ctx, q)
	}
	activityOf(t, h, "team=payments", vViewer)
	if most != activityBatch {
		t.Errorf("a team's export asked the ledger for %d changes at once, not %d", most, activityBatch)
	}
}

// One export at a time a person: another of theirs is told to come back
// while one goes out, someone else's isn't. A small one, 50 changes at
// most, takes no export's turn, and holds a mebibyte at most.
func TestActivityExportOneAPerson(t *testing.T) {
	s := activityServer(t)
	h := s.Handler()
	w := &unread{ResponseRecorder: httptest.NewRecorder(), writing: make(chan struct{}), goOn: make(chan struct{})}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/activity", nil)
	r.Header.Set("X-Forwarded-Email", vViewer)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(w, r)
	}()
	<-w.writing
	if code, body := getAs(t, h, "/api/v1/activity?format=csv", vViewer); code != http.StatusServiceUnavailable {
		t.Errorf("a second export of one person's: %d %s", code, body)
	}
	if code, body := getAs(t, h, "/api/v1/activity", vEditor); code != http.StatusOK {
		t.Errorf("another person's: %d %s", code, body)
	}
	close(w.goOn)
	<-done
	if code, body := getAs(t, h, "/api/v1/activity", vViewer); code != http.StatusOK {
		t.Errorf("once the first was read: %d %s", code, body)
	}
	// Every export's turn taken: a small one goes out all the same.
	for range activityExports {
		s.exports <- struct{}{}
	}
	defer func() {
		for range activityExports {
			<-s.exports
		}
	}()
	if code, body := getAs(t, h, "/api/v1/activity?limit=50", vViewer); code != http.StatusOK || len(body) > activitySmallBytes {
		t.Errorf("a small export beside every turn taken: %d, %d bytes", code, len(body))
	}
	// So many small ones at once, of all readers, and past them, a 503.
	was := renderWait
	defer func() { renderWait = was }()
	renderWait = time.Millisecond
	for range activitySmalls {
		s.smallExports <- struct{}{}
	}
	defer func() {
		for range activitySmalls {
			<-s.smallExports
		}
	}()
	r = httptest.NewRequest(http.MethodGet, "/api/v1/activity?limit=50", nil)
	r.Header.Set("X-Forwarded-Email", vEditor)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Errorf("a small export past %d at once: %d, Retry-After %q", activitySmalls, rec.Code, rec.Header().Get("Retry-After"))
	}
}
