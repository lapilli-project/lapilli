package ledger

import (
	"context"
	"slices"
	"testing"
	"time"
)

// keepChange notes a change at a time of its own, as the writes note them.
func keepChange(t *testing.T, l *Ledger, at time.Time, by, what, after string) {
	t.Helper()
	if _, err := l.w.ExecContext(context.Background(), "INSERT INTO changes (at, by, what, before, after) VALUES (?, ?, ?, ?, ?)",
		at.Unix(), by, what, "", after); err != nil {
		t.Fatal(err)
	}
}

// The activity log reads changes back by who, kind and period, the latest
// first, a page at a time from where the last ended, on either ledger.
func TestActivity(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	keepChange(t, l, t0, "ana", "setting lead", `"10m"`)
	keepChange(t, l, t0.Add(time.Hour), "ben", "task 3 made", `{"title":"x"}`)
	keepChange(t, l, t0.Add(time.Hour), "ana", "verdict on Errors {\"service\":\"checkout\"} at 2026-11-01T00:00:00Z", "")
	keepChange(t, l, t0.Add(2*time.Hour), "config", RoleGiven+"user:cy", "")
	keepChange(t, l, t0.Add(3*time.Hour), "ana", "firings", "")
	keepChange(t, l, t0.Add(4*time.Hour), "ana", "firings from elsewhere", "") // not the whole what: other
	whats := func(q ActivityQuery) []string {
		t.Helper()
		got, err := l.Activity(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range got {
			out = append(out, c.Kind+":"+c.By)
		}
		return out
	}
	if got, want := whats(ActivityQuery{}), []string{"other:ana", "firings:ana", "role:config", "verdict:ana", "task:ben", "setting:ana"}; !slices.Equal(got, want) {
		t.Errorf("all: %v, want %v", got, want)
	}
	if got, want := whats(ActivityQuery{By: "ana"}), []string{"other:ana", "firings:ana", "verdict:ana", "setting:ana"}; !slices.Equal(got, want) {
		t.Errorf("ana's: %v, want %v", got, want)
	}
	if got, want := whats(ActivityQuery{Kind: "role"}), []string{"role:config"}; !slices.Equal(got, want) {
		t.Errorf("roles: %v, want %v", got, want)
	}
	if got, want := whats(ActivityQuery{Kind: KindOther}), []string{"other:ana"}; !slices.Equal(got, want) {
		t.Errorf("other: %v, want %v", got, want)
	}
	if got, want := whats(ActivityQuery{Since: t0.Add(time.Hour), Until: t0.Add(3 * time.Hour)}), []string{"role:config", "verdict:ana", "task:ben"}; !slices.Equal(got, want) {
		t.Errorf("a period: %v, want %v", got, want)
	}
	if _, err := l.Activity(ctx, ActivityQuery{Kind: "everything"}); err == nil {
		t.Error("a kind there isn't is taken")
	}

	// Two pages of two, the second from where the first ended, two
	// changes of one second among them.
	first, err := l.Activity(ctx, ActivityQuery{Limit: 2, Since: t0, Until: t0.Add(3 * time.Hour)})
	if err != nil || len(first) != 2 {
		t.Fatalf("the first page: %v %v", first, err)
	}
	c, err := ParseActivityCursor(first[1].Cursor().String())
	if err != nil || c != first[1].Cursor() {
		t.Fatalf("the cursor %q read back as %v, %v", first[1].Cursor(), c, err)
	}
	second, err := l.Activity(ctx, ActivityQuery{Limit: 2, Since: t0, Until: t0.Add(3 * time.Hour), Before: c})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, x := range append(first, second...) {
		kinds = append(kinds, x.Kind)
	}
	// The verdict and the task share their second: the later id first.
	if want := []string{"role", "verdict", "task", "setting"}; !slices.Equal(kinds, want) {
		t.Errorf("two pages: %v, want %v", kinds, want)
	}
	for _, bad := range []string{"x", "1.", ".1", "1.0", "1.-2"} {
		if _, err := ParseActivityCursor(bad); err == nil {
			t.Errorf("cursor %q taken", bad)
		}
	}
}

// Read Light, a change's before and after are left out but a bulk
// verdict's after, which says its rules; ActivityBodies fills them in for
// the changes kept, by id.
func TestActivityLight(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range [][3]string{
		{"setting lead", `"5m"`, `"10m"`},
		{"verdicts: needed action yes, on 1 firings of 1 rules", "", `{"firings":[{"rule":"Errors"}]}`},
		{"task 3 changed", `{"title":"w"}`, `{"title":"x"}`},
	} {
		if _, err := l.w.ExecContext(ctx, "INSERT INTO changes (at, by, what, before, after) VALUES (?, ?, ?, ?, ?)", t0.Unix(), "ana", c[0], c[1], c[2]); err != nil {
			t.Fatal(err)
		}
	}
	got, err := l.Activity(ctx, ActivityQuery{Light: true})
	if err != nil || len(got) != 3 {
		t.Fatalf("%v %v", got, err)
	}
	bodies := func() []string {
		var out []string
		for _, c := range got {
			out = append(out, c.Before+"|"+c.After)
		}
		return out
	}
	// The latest first: the task, the bulk verdict, the setting.
	if want := []string{"|", `|{"firings":[{"rule":"Errors"}]}`, "|"}; !slices.Equal(bodies(), want) {
		t.Errorf("read light: %q, want %q", bodies(), want)
	}
	if err := l.ActivityBodies(ctx, got[:2]); err != nil {
		t.Fatal(err)
	}
	if want := []string{`{"title":"w"}|{"title":"x"}`, `|{"firings":[{"rule":"Errors"}]}`, "|"}; !slices.Equal(bodies(), want) {
		t.Errorf("the first two filled in: %q, want %q", bodies(), want)
	}
}

// What a change is of: the rules, a task or a proposal, for the activity
// log's team.
func TestChangeSubject(t *testing.T) {
	for what, want := range map[string]struct {
		rules          []string
		task, proposal int64
	}{
		`verdict on Disk full {"host":"a"} at 2026-11-01T00:00:00Z undone`: {rules: []string{"Disk full"}},
		"note on Errors kept":                        {rules: []string{"Errors"}},
		"note on Errors taken away":                  {rules: []string{"Errors"}},
		"comment on rule Errors":                     {rules: []string{"Errors"}},
		"what rule Old kept moved to New":            {rules: []string{"Old"}},
		"task 12 changed":                            {task: 12},
		"comment on task 7":                          {task: 7},
		"comment on task 7 copied in from its issue": {task: 7},
		"comment 3 posted to task 9's issue":         {task: 9},
		"proposal #41 approved":                      {proposal: 41},
		"setting lead":                               {},
	} {
		rules, task, proposal := ChangeSubject(what, "")
		if !slices.Equal(rules, want.rules) || task != want.task || proposal != want.proposal {
			t.Errorf("%q: %v %d %d, want %+v", what, rules, task, proposal, want)
		}
	}
	rules, _, _ := ChangeSubject("verdicts: needed action yes, on 3 firings of 2 rules", `{"action":"yes","firings":[{"rule":"A"},{"rule":"B"},{"rule":"A"}]}`)
	if !slices.Equal(rules, []string{"A", "B"}) {
		t.Errorf("a bulk verdict's rules: %v", rules)
	}
}

// Prune keeps the activity log to its own time: as long as asked, longer
// than the firings, or all of it.
func TestPruneKeepsActivity(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	keepChange(t, l, t0, "ana", "setting lead", "")
	keepChange(t, l, t0.AddDate(1, 0, 0), "ana", "setting refire", "")
	count := func() int {
		t.Helper()
		var n int
		if err := l.r.QueryRowContext(ctx, "SELECT count(*) FROM changes").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	cut := t0.AddDate(1, 6, 0)
	// All of it kept, the firings pruned.
	if err := l.PruneKeeping(ctx, cut, time.Time{}); err != nil || count() != 2 {
		t.Errorf("kept whole: %v, %d changes", err, count())
	}
	// Up to its own time: the change before it goes, the one after stays.
	if err := l.PruneKeeping(ctx, cut, t0.AddDate(0, 6, 0)); err != nil || count() != 1 {
		t.Errorf("kept to its own time: %v, %d changes", err, count())
	}
	// Prune alone drops them with the firings, as it did.
	if err := l.Prune(ctx, cut); err != nil || count() != 0 {
		t.Errorf("pruned with the firings: %v, %d changes", err, count())
	}
}
