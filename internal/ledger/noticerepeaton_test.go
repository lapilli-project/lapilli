package ledger

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// A notice of a window or a marked window kept carries the row as the API
// gives it: its repeat day as repeat_on, and as on, the old name, beside it.
func TestNoticeKeptHasRepeatOn(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	l.TellTo([]string{"webhook"})
	at := time.Date(2026, 11, 15, 2, 0, 0, 0, time.UTC)
	if _, err := l.PutWindow(ctx, Window{Title: "month end", Start: at, End: at.Add(time.Hour), Every: "month", On: "15"}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutMarked(ctx, Marked{Kind: "sale", Title: "Autumn sale", Start: at, End: at.Add(time.Hour), Every: "month", On: "15"}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	due, err := l.Due(ctx, "webhook", time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, n := range due {
		var body map[string]any
		if err := json.Unmarshal(n.Body, &body); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"window", "marked"} {
			row, ok := body[key].(map[string]any)
			if !ok {
				continue
			}
			seen[key] = true
			if row["on"] != "15" || row["repeat_on"] != "15" {
				t.Errorf("%s's notice %s: on %v, repeat_on %v, want both 15", n.Event, key, row["on"], row["repeat_on"])
			}
		}
	}
	if !seen["window"] || !seen["marked"] {
		t.Errorf("notices seen %v, want a window's and a marked window's", seen)
	}
}
