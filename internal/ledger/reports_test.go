package ledger

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// A report is kept, read back as kept, changed, and deleted with what was
// kept of its sending; each a change noted with who made it.
func TestScheduled(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	team, err := l.PutTeam(ctx, Team{Name: "payments", Rules: "^Errors$"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	id, err := l.PutScheduled(ctx, Scheduled{Name: " payments daily ", TeamID: team, Zone: "team", Schedule: "0 9 * * 1-5", Span: "168h",
		Sections: []string{"tasks", "summary"}, Dests: []string{"ops", "team:payments"}, Enabled: true}, "ana", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r, err := l.ScheduledByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	// The name trimmed, the sections in a report's order, the span as
	// people write it.
	if r.Name != "payments daily" || r.TeamID != team || r.Zone != "team" || r.Span != "7d" || !slices.Equal(r.Sections, []string{"summary", "tasks"}) ||
		!slices.Equal(r.Dests, []string{"ops", "team:payments"}) || !r.Enabled || r.CreatedBy != "ana" || r.Created.IsZero() {
		t.Errorf("kept: %+v", r)
	}
	if s, err := r.When(); err != nil || !s.Cron() {
		t.Errorf("its schedule: %v %v", s, err)
	}
	// None picked: the four a report holds by default.
	id2, err := l.PutScheduled(ctx, Scheduled{Name: "everyone weekly", Schedule: "weekly mon@09:00", Dests: []string{"slack"}}, "bo", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := l.ScheduledByID(ctx, id2)
	if !slices.Equal(r2.Sections, DefaultSections) || r2.Zone != "ledger" || r2.Enabled {
		t.Errorf("by default: %+v", r2)
	}
	// Changed, by someone else.
	r.Schedule, r.Span, r.UpdatedBy = "daily@08:00", "", "x"
	if _, err := l.PutScheduled(ctx, r, "bo", time.Now()); err != nil {
		t.Fatal(err)
	}
	r, _ = l.ScheduledByID(ctx, id)
	if r.Schedule != "daily@08:00" || r.UpdatedBy != "bo" || r.CreatedBy != "ana" {
		t.Errorf("changed: %+v", r)
	}
	all, err := l.Scheduleds(ctx)
	if err != nil || len(all) != 2 || all[0].Name != "everyone weekly" {
		t.Errorf("by name: %v %v", all, err)
	}
	// Deleted, and what TellOnce kept of it with it.
	if err := l.TellOnce(ctx, ReportSentKey(id), "1", "report.ready", nil); err != nil {
		t.Fatal(err)
	}
	if v, _ := l.Told(ctx, ReportSentKey(id)); v != "1" {
		t.Fatalf("told: %q", v)
	}
	if err := l.DeleteScheduled(ctx, id, "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ScheduledByID(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted: %v", err)
	}
	if v, _ := l.Told(ctx, ReportSentKey(id)); v != "" {
		t.Errorf("its sending kept: %q", v)
	}
	if err := l.DeleteScheduled(ctx, id, "ana"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted twice: %v", err)
	}
	r.ID = id
	if _, err := l.PutScheduled(ctx, r, "ana", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("one deleted, changed: %v", err)
	}
	cs, err := l.Changes(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var what []string
	for _, c := range cs {
		what = append(what, c.By+" "+c.What)
	}
	for _, want := range []string{"ana report payments daily kept", "bo report payments daily kept", "bo report everyone weekly kept", "ana report payments daily deleted"} {
		if !slices.Contains(what, want) {
			t.Errorf("no change %q in %v", want, what)
		}
	}
}

// What can't be a report is refused, saying why, in a form the reader's
// language can be given; and there are 50 at most, with names unique.
func TestScheduledRefused(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	ok := Scheduled{Name: "r", Schedule: "daily@09:00", Dests: []string{"ops"}}
	for _, c := range []struct {
		change func(*Scheduled)
		says   string
	}{
		{func(r *Scheduled) { r.Name = "" }, "a report's name"},
		{func(r *Scheduled) { r.Name = "<b>" }, "a report's name"},
		{func(r *Scheduled) { r.Name = strings.Repeat("x", 101) }, "a report's name"},
		{func(r *Scheduled) { r.Schedule = "* * * * *" }, "hourly at most"},
		{func(r *Scheduled) { r.Schedule = "daily@9" }, "09:00"},
		{func(r *Scheduled) { r.Span = "24h" }, "only with a cron"},
		{func(r *Scheduled) { r.Schedule, r.Span = "0 9 * * *", "1w" }, "24h or 7d"},
		{func(r *Scheduled) { r.Schedule, r.Span = "0 9 * * *", "93d" }, "24h or 7d"},
		{func(r *Scheduled) { r.Schedule, r.Span = "0 9 * * *", "30m" }, "an hour at least"},
		{func(r *Scheduled) { r.Zone = "team" }, "a team's"},
		{func(r *Scheduled) { r.Zone = "utc" }, "ledger or team"},
		{func(r *Scheduled) { r.Sections = []string{"summary", "everything"} }, "some of summary"},
		{func(r *Scheduled) { r.Sections = []string{"summary", "summary"} }, "each once"},
		{func(r *Scheduled) { r.Dests = nil }, "1 to 10"},
		{func(r *Scheduled) { r.Dests = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"} }, "1 to 10"},
		{func(r *Scheduled) { r.Dests = []string{"https://hooks.example.com/x"} }, "--report-dest"},
		{func(r *Scheduled) { r.Dests = []string{"Ops"} }, "--report-dest"},
		{func(r *Scheduled) { r.Dests = []string{"ops", "ops"} }, "each once"},
		{func(r *Scheduled) { r.TeamID = 99 }, "no team 99"},
	} {
		r := ok
		r.Sections, r.Dests = slices.Clone(ok.Sections), slices.Clone(ok.Dests)
		c.change(&r)
		_, err := l.PutScheduled(ctx, r, "ana", time.Now())
		var inv ErrInvalid
		if !errors.As(err, &inv) || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%+v: %v, want one saying %q", r, err, c.says)
		}
	}
	if _, err := l.PutScheduled(ctx, ok, "ana", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutScheduled(ctx, ok, "ana", time.Now()); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("a name twice: %v", err)
	}
	for i := 1; i < MaxScheduled; i++ {
		r := ok
		r.Name = "r" + strings.Repeat("x", i)
		if _, err := l.PutScheduled(ctx, r, "ana", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	r := ok
	r.Name = "one too many"
	if _, err := l.PutScheduled(ctx, r, "ana", time.Now()); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("the 51st: %v", err)
	}
	// A report changed isn't one more.
	rs, _ := l.Scheduleds(ctx)
	rs[0].Schedule = "daily@10:00"
	if _, err := l.PutScheduled(ctx, rs[0], "ana", time.Now()); err != nil {
		t.Errorf("one of 50 changed: %v", err)
	}
}
