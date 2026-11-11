package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
)

// What the content check found stands until what it reads changes: a
// write to the ledger that changes no firing (a note, a notification)
// doesn't run it again; the firings judged again, or the rules read anew,
// do.
func TestContentCheckedWhenItsInputsChange(t *testing.T) {
	s, _ := saying(t, nil)
	ctx := context.Background()
	checked := func() int {
		t.Helper()
		if _, err := s.contentFindings(ctx); err != nil {
			t.Fatal(err)
		}
		s.saidRules.Lock()
		defer s.saidRules.Unlock()
		return s.saidRules.checked
	}
	n := checked()
	if err := s.l.PutNote(ctx, ledger.Note{Rule: "Errors", Note: "looked at"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if err := s.l.TellOnce(ctx, "test", "1", "test.event", nil); err != nil {
		t.Fatal(err)
	}
	if got := checked(); got != n {
		t.Errorf("checked again after writes that change no firing: %d → %d", n, got)
	}
	o, err := s.options(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.l.JudgeAll(ctx, o, func(float64) {}); err != nil {
		t.Fatal(err)
	}
	if got := checked(); got != n+1 {
		t.Errorf("not checked again once the firings were judged again: %d → %d", n, got)
	}
	s.saidRules.Lock()
	rs := s.saidRules.rules
	s.saidRules.Unlock()
	s.said(rs)
	if got := checked(); got != n+2 {
		t.Errorf("not checked again with the rules read anew: %d → %d", n, got)
	}
}

// Asked at once by many, the check runs once: the others wait for it and
// take what it found.
func TestContentCheckedOnceAtATime(t *testing.T) {
	s, _ := saying(t, nil)
	ctx := context.Background()
	var rs []rules.Rule
	for i := range 300 {
		rs = append(rs, rules.Rule{Name: fmt.Sprint("Deep", i), Labels: map[string]string{"severity": "page"},
			Expr: strings.Repeat("(", 10000) + "up" + strings.Repeat(")", 10000)})
	}
	s.saidRules.Lock()
	n := s.saidRules.checked
	s.saidRules.Unlock()
	s.said(rs)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Go(func() {
			<-start
			if _, err := s.contentFindings(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	close(start)
	wg.Wait()
	s.saidRules.Lock()
	defer s.saidRules.Unlock()
	if got := s.saidRules.checked - n; got != 1 {
		t.Errorf("checked %d times for eight asking at once", got)
	}
}

// A report's content section is the problems now, by check, the guesses
// apart, in its JSON as in its line.
func TestReportContentCounts(t *testing.T) {
	s, _ := saying(t, func(c *Config) { c.ReportDests = map[string]string{"ops": filepath.Join(t.TempDir(), "ops")} })
	ctx := context.Background()
	id, err := s.l.PutScheduled(ctx, ledger.Scheduled{Name: "content", Schedule: "daily@09:00", Sections: []string{"content"}, Dests: []string{"ops"}, Enabled: true},
		"ana", now.Add(-48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.l.ScheduledByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.preview(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	j, _ := json.Marshal(b.about()["numbers"])
	want := `{"content":{"problems":3,"rules":1,"by_check":{"label_never_seen":1,"noisy_no_for":1,"runbook_not_url":1},"guesses":2,` +
		`"guesses_by_check":{"label_not_given":1,"short_range":1},"text":"What the rules say: 3 problems in 1 rules (`
	if !strings.HasPrefix(string(j), want) {
		t.Errorf("\n got %s\nwant %s…", j, want)
	}
	if !strings.Contains(b.Text, "What the rules say: 3 problems in 1 rules") {
		t.Errorf("the line: %s", b.Text)
	}
}
