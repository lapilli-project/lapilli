package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
)

// realServer is a server with a hundred firings on 2026-11-01 that caught
// nothing, each of a service of its own, ana and bo its editors, and notices
// told to a webhook; and the firings' ids.
func realServer(t *testing.T) (*Server, []string) {
	t.Helper()
	s, _ := server(t, func(c *Config) {
		c.UserHeader, c.Editors = "X-Forwarded-Email", []string{"ana@example.com", "bo@example.com"}
	})
	s.l.TellTo([]string{"webhook"})
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var fs []firing.Firing
	for i := 0; i < 100; i++ {
		at := day.Add(time.Duration(i) * 10 * time.Minute)
		fs = append(fs, firing.Firing{Rule: fmt.Sprintf("R%02d", i%7), Start: at, End: at.Add(3 * time.Minute), Labels: map[string]string{"service": fmt.Sprint("s", i)}})
	}
	keep(t, s, day, day.Add(24*time.Hour), fs)
	got, err := s.l.Firings(context.Background(), ledger.FiringQuery{From: day, To: day.Add(24 * time.Hour), Black: true, Limit: 200})
	if err != nil || len(got) != 100 {
		t.Fatalf("%d firings: %v", len(got), err)
	}
	var ids []string
	for _, f := range got {
		ids = append(ids, fmt.Sprint(f.ID))
	}
	return s, ids
}

// writtenOnce checks that each firing is written down once: an incident
// from each, and a notice of each, told once.
func writtenOnce(t *testing.T, s *Server, what string, n int) {
	t.Helper()
	ctx := context.Background()
	incs, err := s.l.Incidents(ctx, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), false)
	if err != nil {
		t.Fatal(err)
	}
	from := map[string]int{}
	for _, in := range incs {
		if in.Origin == "ui" {
			from[in.Start.UTC().Format(time.RFC3339)+" "+in.Title]++
		}
	}
	ns, err := s.l.Due(ctx, "webhook", time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), 10000)
	if err != nil {
		t.Fatal(err)
	}
	told := map[string]int{}
	for _, x := range ns {
		if x.Event != "incident.written" {
			continue
		}
		var b struct {
			Incident struct {
				Title string
				Start time.Time
			}
		}
		if err := json.Unmarshal(x.Body, &b); err != nil {
			t.Fatal(err)
		}
		told[b.Incident.Start.UTC().Format(time.RFC3339)+" "+b.Incident.Title]++
	}
	more, all, notices := 0, 0, 0
	for _, c := range told {
		notices += c
	}
	for k, c := range from {
		all += c
		if c != 1 || told[k] != 1 {
			more++
			if more <= 3 {
				t.Errorf("%s: %s written %d times, told %d", what, k, c, told[k])
			}
		}
	}
	if len(from) != n || len(told) != n || more > 0 {
		t.Errorf("%s: %d firings written down, %d told, of %d; %d more than once: %d incidents, %d notices", what, len(from), len(told), n, more, all, notices)
	}
}

// TestRealOnce: "These were real" for a hundred firings posted four times
// at once, with each firing's own "This was real" pressed at the same
// time, by two people, writes each firing down once: one incident, one
// notice. The second press, or the later post, finds it caught.
func TestRealOnce(t *testing.T) {
	s, ids := realServer(t)
	h := s.Handler()
	var wg sync.WaitGroup
	codes := make(chan int, 4+len(ids))
	for g := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _, _ := postAs(t, h, "/firings/real", url.Values{"firing": ids, "back": {"/firings?day=2026-11-01"}},
				[]string{"ana@example.com", "bo@example.com"}[g%2], "same-origin")
			codes <- code
		}()
	}
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _, _ := postAs(t, h, "/firings/"+id+"/real", nil, []string{"ana@example.com", "bo@example.com"}[i%2], "same-origin")
			codes <- code
		}()
	}
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusSeeOther {
			t.Errorf("a post answered %d", c)
		}
	}
	writtenOnce(t, s, "at once", len(ids))
}

// TestRealWrittenFrom: a firing whose incident is written down, but which
// the ledger has as caught nothing still (as a ledger changed from outside
// could), is not written down again: one incident from it is there.
func TestRealWrittenFrom(t *testing.T) {
	s, ids := realServer(t)
	h := s.Handler()
	if code, _, hd := postAs(t, h, "/firings/"+ids[0]+"/real", nil, "ana@example.com", "same-origin"); code != http.StatusSeeOther || hd.Get("Location") == "" {
		t.Fatalf("%d", code)
	}
	ledgerExec(t, s, "UPDATE firings SET white = 0 WHERE id = ?", ids[0])
	if code, _, hd := postAs(t, h, "/firings/"+ids[0]+"/real", nil, "bo@example.com", "same-origin"); code != http.StatusSeeOther || hd.Get("Location") != "/firings?day=2026-11-01" {
		t.Errorf("again: %d to %q", code, hd.Get("Location"))
	}
	if code, _, hd := postAs(t, h, "/firings/real", url.Values{"firing": ids[:2], "back": {"/firings?day=2026-11-01"}}, "bo@example.com", "same-origin"); code != http.StatusSeeOther ||
		hd.Get("Location") != "/firings?already=1&bulk=real&day=2026-11-01&done=1" {
		t.Errorf("both: %d to %q", code, hd.Get("Location"))
	}
	writtenOnce(t, s, "again", 2)
}

// TestRealBack: "These were real" goes back to the firings it came from,
// with the team and the period they were looked at for.
func TestRealBack(t *testing.T) {
	s, ids := realServer(t)
	h := s.Handler()
	if _, err := s.l.PutTeam(context.Background(), ledger.Team{Name: "payments", Rules: "^R0"}, "ana"); err != nil {
		t.Fatal(err)
	}
	_, body := getAs(t, h, "/firings?day=2026-11-01&team=payments&weeks=12&label=service%3Ds1", "ana@example.com")
	want := `<input type="hidden" name="back" value="/firings?day=2026-11-01&amp;label=service%3Ds1&amp;team=payments&amp;weeks=12">`
	if !strings.Contains(body, want) {
		t.Fatalf("no %s", want)
	}
	code, _, hd := postAs(t, h, "/firings/real", url.Values{"firing": ids[:1], "back": {"/firings?day=2026-11-01&label=service%3Ds1&team=payments&weeks=12"}}, "ana@example.com", "same-origin")
	if want := "/firings?bulk=real&day=2026-11-01&done=1&label=service%3Ds1&team=payments&weeks=12"; code != http.StatusSeeOther || hd.Get("Location") != want {
		t.Errorf("%d to %q, not %q", code, hd.Get("Location"), want)
	}
}
