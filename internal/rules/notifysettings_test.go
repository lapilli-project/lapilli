package rules

import (
	"slices"
	"testing"
	"time"
)

func TestNotifySettings(t *testing.T) {
	if notifySettings(map[string]any{"group_wait": "5s"}) != nil {
		t.Fatal("settings without a contact point were read")
	}
	n := notifySettings(map[string]any{
		"receiver": "pager", "group_by": []any{"cluster", "alertname"}, "group_wait": "5s", "group_interval": "2m",
		"repeat_interval": "1h30m", "mute_time_intervals": []any{"nights"}, "active_time_intervals": []any{"weekdays"},
	})
	if n == nil || n.GroupWait == nil || *n.GroupWait != 5*time.Second || n.GroupInterval == nil || *n.GroupInterval != 2*time.Minute || n.RepeatInterval == nil || *n.RepeatInterval != 90*time.Minute ||
		!slices.Equal(n.GroupBy, []string{"cluster", "alertname"}) || !slices.Equal(n.MuteTimeIntervals, []string{"nights"}) || !slices.Equal(n.ActiveTimeIntervals, []string{"weekdays"}) {
		t.Fatalf("read %+v", n)
	}
	if bad := notifySettings(map[string]any{"receiver": "p", "group_wait": "soon"}); bad == nil || bad.GroupWait != nil {
		t.Fatalf("an unreadable timing: %+v", bad)
	}
	// An explicit 0s is zero, which is not a timing left out.
	z := notifySettings(map[string]any{"receiver": "p", "group_wait": "0s", "repeat_interval": "0"})
	if z == nil || z.GroupWait == nil || *z.GroupWait != 0 || z.RepeatInterval == nil || *z.RepeatInterval != 0 || z.GroupInterval != nil {
		t.Fatalf("explicit zeros: %+v", z)
	}
}
