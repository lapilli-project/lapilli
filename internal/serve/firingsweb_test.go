package serve

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
)

// ledgerIncident is an incident of a service, as written down.
func ledgerIncident(id string, start, end time.Time, service string) incident.Incident {
	return incident.Incident{ID: id, Title: id, Start: start, End: end, Labels: incident.Labels{"service": {service}}}
}

// keep keeps firings in a server's ledger, as a run over [from, to)
// would.
func keep(t *testing.T, s *Server, from, to time.Time, fs []firing.Firing) {
	t.Helper()
	if err := s.l.Record(context.Background(), ledger.Found{From: from, To: to, Firings: fs, Run: ledger.Run{Kind: "backfill", Started: from, Finished: from}},
		s.cfg.Options); err != nil {
		t.Fatal(err)
	}
}

// firingsOf is the firings page for a query, worked out as the page is.
func firingsOf(t *testing.T, s *Server, query string) firingsView {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/firings?"+query, nil)
	r, vr := s.withContext(r, "firings")
	_ = r
	v, err := s.firingsPage(vr)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return v.(firingsView)
}

// labelServer is a server with a day, 2026-11-02, of 250 firings of two
// rules, each with a service, a zone and a pod: some catching INC-1, some
// excused by a quiet window.
func labelServer(t *testing.T, change func(*Config)) *Server {
	t.Helper()
	s, _ := server(t, change)
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	var fs []firing.Firing
	for i := 0; i < 250; i++ {
		at := day.Add(time.Duration(i) * 5 * time.Minute)
		svc := []string{"checkout", "search", "cart"}[i%3]
		fs = append(fs, firing.Firing{Rule: []string{"Errors", "Latency"}[i%2], Start: at, End: at.Add(3 * time.Minute),
			Labels: map[string]string{"service": svc, "zone": fmt.Sprintf("z%d", i%4), "pod": fmt.Sprintf("p%d", i), "env": "prod"}})
	}
	keep(t, s, day, day.Add(24*time.Hour), fs)
	if _, err := s.l.PutWindow(context.Background(), ledger.Window{Title: "deploy", Start: day.Add(15 * time.Hour), End: day.Add(16 * time.Hour)}, "ana", s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	s.jobs(context.Background(), false) // the window's span judged again
	return s
}

// TestFiringLabels: the firings are picked by labels, each a name=value,
// all of them, five at most; each firing's labels are chips that pick its
// day's firings with one more; a picked label's chip takes it away; and a
// label that isn't one is left out, not said back.
func TestFiringLabels(t *testing.T) {
	s := labelServer(t, nil)
	h := s.Handler()
	all := firingsOf(t, s, "day=2026-11-02")
	if len(all.Lines) != firingsPage || all.Next == "" || len(all.Labels) != 0 {
		t.Fatalf("all: %d lines, next %q", len(all.Lines), all.Next)
	}
	// Each label picked, and the two together; then page by page.
	for _, c := range []struct {
		query string
		want  func(f ledger.Firing) bool
	}{
		{"label=service%3Dsearch", func(f ledger.Firing) bool { return f.Labels["service"] == "search" }},
		{"label=service%3Dsearch&label=zone%3Dz1", func(f ledger.Firing) bool { return f.Labels["service"] == "search" && f.Labels["zone"] == "z1" }},
		{"label=service%3Dsearch&label=service%3Dsearch&black=1", func(f ledger.Firing) bool { return f.Labels["service"] == "search" && !f.White && !f.Excused }},
		{"label=zone%3Dz2&rule=Errors", func(f ledger.Firing) bool { return f.Labels["zone"] == "z2" && f.Rule == "Errors" }},
		{"label=zone%3Dz9", func(f ledger.Firing) bool { return false }},
		// Three pages of them.
		{"label=env%3Dprod", func(f ledger.Firing) bool { return true }},
	} {
		day, err := s.l.Firings(context.Background(), ledger.FiringQuery{From: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)})
		if err != nil {
			t.Fatal(err)
		}
		var want []int64
		for _, f := range day {
			if c.want(f) {
				want = append(want, f.ID)
			}
		}
		var got []int64
		q := "day=2026-11-02&" + c.query
		for pages := 0; ; pages++ {
			v := firingsOf(t, s, q)
			for _, l := range v.Lines {
				got = append(got, l.ID)
			}
			n := 0
			for _, x := range v.Strip.Hours {
				n += x.Firings
			}
			if n != len(want) {
				t.Errorf("%s: the strip counts %d, of %d", q, n, len(want))
			}
			if v.Next == "" || pages > 5 {
				break
			}
			q = strings.TrimPrefix(v.Next, "/firings?")
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: %d firings, not %d", c.query, len(got), len(want))
		}
	}

	// A firing's chips: each a link to its day's with that label too, the
	// rest of what was asked kept; one picked already, none.
	_, body, _ := get(t, h, "/firings?day=2026-11-02&rule=Errors&black=1&label=service%3Dsearch&team=&weeks=4")
	for _, want := range []string{
		`<a class="chip label" href="/firings?black=1&amp;day=2026-11-02&amp;label=service%3Dsearch&amp;label=zone%3Dz2&amp;rule=Errors&amp;team=&amp;weeks=4"><span class="vh">Only those with the label </span><bdi>zone=z2</bdi></a>`,
		`<span class="chip label on"><bdi>service=search</bdi></span>`,
		`<a class="chip" href="/firings?black=1&amp;day=2026-11-02&amp;rule=Errors&amp;team=&amp;weeks=4"><span class="vh">Remove filter: </span>Label: <bdi>service=search</bdi> <span aria-hidden="true">✕</span></a>`,
		`<input type="hidden" name="label" value="service=search">`,
		`<a href="/firings?black=1&amp;day=2026-11-01&amp;label=service%3Dsearch&amp;rule=Errors&amp;team=&amp;weeks=4">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("no %s", want)
		}
	}

	// Five at most; then no chip adds one. What isn't a label is left out,
	// and not said back.
	q := "day=2026-11-02"
	for _, l := range []string{"service=search", "zone=z1", "pod=p1", "a=1", "b=2", "c=3", "nolabel", "=x", "x=", "long=" + strings.Repeat("v", maxLabelValue+1)} {
		q += "&label=" + url.QueryEscape(l)
	}
	v := firingsOf(t, s, q)
	if len(v.Labels) != maxLabels || !v.More {
		t.Errorf("%d labels picked, more %v", len(v.Labels), v.More)
	}
	_, body, _ = get(t, h, "/firings?"+q)
	// What the page says, and links to: the languages' links name the page
	// as it was asked, to come back to.
	body = regexp.MustCompile(`(?s)<nav class="langs".*?</nav>`).ReplaceAllString(body, "")
	if strings.Contains(body, "c=3") || strings.Contains(body, "nolabel") || strings.Contains(body, "vvvv") || !strings.Contains(body, "Five labels at most") {
		t.Error("a label left out is said back, or isn't said to be left out")
	}
	if strings.Contains(body, `<a class="chip label"`) {
		t.Error("with five picked, a chip still adds one")
	}
}

// TestHourStrip: the day's firings an hour at a time are those the ledger
// has for that day, by the hour of the clock each started in, in zones
// whose clocks change: a day whose midnight is skipped starts at 01:00,
// and an hour the clocks go back over counts both.
func TestHourStrip(t *testing.T) {
	for _, c := range []struct {
		zone, day string
		clocks    map[int]int // the hours not once
	}{
		{"UTC", "2026-11-02", nil},
		{"Asia/Seoul", "2026-10-20", nil},
		{"America/Santiago", "2026-09-06", map[int]int{0: 0}},
		{"America/Santiago", "2026-04-04", map[int]int{23: 2}},
		{"Europe/Berlin", "2026-10-25", map[int]int{2: 2}},
		{"America/New_York", "2026-03-08", map[int]int{2: 0}},
	} {
		loc, err := time.LoadLocation(c.zone)
		if err != nil {
			t.Fatal(err)
		}
		s, _ := server(t, func(cf *Config) { cf.Loc = loc })
		d, _ := dateDay(c.day)
		from, to := s.l.Midnight(d), s.l.Midnight(d+1)
		r := rand.New(rand.NewPCG(uint64(d), 7))
		var fs []firing.Firing
		for i := 0; i < 400; i++ {
			// From three hours before the day to three after.
			at := from.Add(-3*time.Hour + time.Duration(r.IntN(int(to.Sub(from)/time.Minute)+360))*time.Minute)
			fs = append(fs, firing.Firing{Rule: "Errors", Start: at, End: at.Add(time.Minute), Labels: map[string]string{"service": []string{"checkout", "search"}[i%2], "i": fmt.Sprint(i)}})
		}
		// An incident to catch some of them.
		at := from.Add(10 * time.Hour)
		keep(t, s, from.Add(-4*time.Hour), to.Add(4*time.Hour), fs)
		if _, err := s.l.Put(context.Background(), ledgerIncident("X-1", at, at.Add(time.Hour), "checkout"), "ana", s.cfg.Options); err != nil {
			t.Fatal(err)
		}
		// The day's, as its date on the clock says, not as the ledger's
		// day starts do.
		kept, err := s.l.Firings(context.Background(), ledger.FiringQuery{From: from.Add(-24 * time.Hour), To: to.Add(24 * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		day := slices.DeleteFunc(kept, func(f ledger.Firing) bool { return f.Start.In(loc).Format("2006-01-02") != c.day })
		var want [24][3]int
		for _, f := range day {
			h := f.Start.In(loc).Hour()
			switch {
			case f.White:
				want[h][1]++
			default:
				want[h][2]++
			}
			want[h][0]++
		}
		v := firingsOf(t, s, "day="+c.day)
		sum, white := 0, 0
		for h, x := range v.Strip.Hours {
			if [3]int{x.Firings, x.White, x.Black} != want[h] {
				t.Errorf("%s %s %02d:00: %d %d %d, the ledger %v", c.zone, c.day, h, x.Firings, x.White, x.Black, want[h])
			}
			clock := 1
			if n, ok := c.clocks[h]; ok {
				clock = n
			}
			if x.Clock != clock {
				t.Errorf("%s %s %02d:00: on the clock %d times, not %d", c.zone, c.day, h, x.Clock, clock)
			}
			sum += x.Firings
			white += x.White
		}
		if sum != len(day) || white == 0 || len(day) < 100 {
			t.Errorf("%s %s: %d counted of %d, %d caught something", c.zone, c.day, sum, len(day), white)
		}
		// The table says the same, and an hour off the clock says so.
		_, body, _ := get(t, s.Handler(), "/firings?day="+c.day)
		for h, x := range want {
			if row := fmt.Sprintf(`<tr><th scope="row">%02d:00</th><td class="n">%d</td><td class="n">%d</td><td class="n">%d</td>`, h, x[0], x[1], x[2]); !strings.Contains(body, row) {
				t.Errorf("%s %s: no %s", c.zone, c.day, row)
			}
		}
		if (c.clocks != nil) != strings.Contains(body, "On the clock") {
			t.Errorf("%s %s: the hours off the clock aren't said", c.zone, c.day)
		}
	}
}

// TestBulkReal: firings picked are written down as real, each as its own
// button does: the same incidents and the same lines in the changes log
// as one at a time; one that caught something by its turn is left. Only
// an editor may, at most 100, from this site; whoever may not sees the
// buttons disabled, with why.
func TestBulkReal(t *testing.T) {
	ctx := context.Background()
	const ed = "ed@example.com"
	twin := func() (*Server, []string) {
		s, _, _ := bulkServer(t, nil)
		day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
		var fs []firing.Firing
		for i := 0; i < 6; i++ {
			// Two that overlap, then apart.
			at := day.Add(time.Duration(i/2)*4*time.Hour + time.Duration(i%2)*5*time.Minute)
			fs = append(fs, firing.Firing{Rule: "SearchErrors", Start: at, End: at.Add(20 * time.Minute), Labels: map[string]string{"service": "search", "n": fmt.Sprint(i)}})
		}
		keep(t, s, day, day.Add(24*time.Hour), fs)
		got, err := s.l.Firings(ctx, ledger.FiringQuery{From: day, To: day.Add(24 * time.Hour), Rule: "SearchErrors"})
		if err != nil || len(got) != 6 {
			t.Fatalf("%d firings: %v", len(got), err)
		}
		var ids []string
		for _, f := range got {
			ids = append(ids, fmt.Sprint(f.ID))
		}
		slices.Reverse(ids) // the earliest first
		return s, ids
	}
	bulk, ids := twin()
	one, oneIDs := twin()
	code, body, hd := postAs(t, bulk.Handler(), "/firings/real", url.Values{"firing": ids[:4], "back": {"/firings?day=2026-11-01&rule=SearchErrors"}}, ed, "same-origin")
	if code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	if loc := hd.Get("Location"); loc != "/firings?already=2&bulk=real&day=2026-11-01&done=2&rule=SearchErrors" {
		t.Errorf("back to %q", loc)
	}
	for _, id := range oneIDs[:4] {
		postAs(t, one.Handler(), "/firings/"+id+"/real", nil, ed, "same-origin")
	}
	incs := func(s *Server) []string {
		is, err := s.l.Incidents(ctx, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), now, true)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, in := range is {
			out = append(out, fmt.Sprintf("%s %s %s %s %v %s", in.ID, in.Title, in.Start, in.End, in.Labels, in.By))
		}
		return out
	}
	made := func(is []string) int {
		n := 0
		for _, x := range is {
			if strings.Contains(x, " caught by SearchErrors ") {
				n++
			}
		}
		return n
	}
	b, o := incs(bulk), incs(one)
	if !slices.Equal(b, o) || made(b) != 2 {
		t.Errorf("incidents\n%v\none at a time\n%v", b, o)
	}
	if _, _, b := worked(t, bulk); !slices.Equal(b, func() []string { _, _, o := worked(t, one); return o }()) {
		t.Errorf("the changes log differs from one at a time's")
	}

	// Refused whole.
	many := make([]string, 101)
	for i := range many {
		many[i] = fmt.Sprint(i + 1)
	}
	h := bulk.Handler()
	for _, c := range []struct {
		what, who, site string
		form            url.Values
		want            int
	}{
		{"a viewer", "vi@example.com", "same-origin", url.Values{"firing": ids[4:]}, http.StatusForbidden},
		{"a team's editor", "pat@example.com", "same-origin", url.Values{"firing": ids[4:]}, http.StatusForbidden},
		{"from another site", ed, "cross-site", url.Values{"firing": ids[4:]}, http.StatusForbidden},
		{"101 firings", ed, "same-origin", url.Values{"firing": many}, http.StatusBadRequest},
		{"not a firing", ed, "same-origin", url.Values{"firing": {ids[4], "x"}}, http.StatusBadRequest},
	} {
		if code, body, _ := postAs(t, h, "/firings/real", c.form, c.who, c.site); code != c.want {
			t.Errorf("%s: %d, not %d: %s", c.what, code, c.want, body)
		}
	}
	if n := len(incs(bulk)); n != len(b) {
		t.Errorf("%d incidents after refusals, not %d", n, len(b))
	}

	// Disabled, with why.
	for who, want := range map[string]string{"pat@example.com": "Editors only", "": "Read-only: no one is signed in"} {
		_, body := getAs(t, h, "/firings?day=2026-11-01", who)
		for _, w := range []string{`<button disabled aria-describedby="real-why">These were real</button>`, `<span id="real-why" class="why">` + want + `</span>`,
			`<button class="quiet-button" type="button" disabled aria-describedby="real-why">This was real</button>`, `name="firing" value="` + ids[5] + `" aria-label="SearchErrors, 2026-11-01 08:05" disabled>`} {
			if !strings.Contains(body, w) {
				t.Errorf("%q: no %s", who, w)
			}
		}
	}
	_, body = getAs(t, h, "/firings?day=2026-11-01", ed)
	if strings.Contains(body, "disabled") || !strings.Contains(body, `<form method="post" action="/firings/`+ids[5]+`/real">`) {
		t.Error("an editor's firings are disabled, or lack their own buttons")
	}
}
