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
	if n == nil || n.GroupWait != 5*time.Second || n.GroupInterval != 2*time.Minute || n.RepeatInterval != 90*time.Minute ||
		!slices.Equal(n.GroupBy, []string{"cluster", "alertname"}) || !slices.Equal(n.MuteTimeIntervals, []string{"nights"}) || !slices.Equal(n.ActiveTimeIntervals, []string{"weekdays"}) {
		t.Fatalf("read %+v", n)
	}
	if bad := notifySettings(map[string]any{"receiver": "p", "group_wait": "soon"}); bad == nil || bad.GroupWait != 0 {
		t.Fatalf("an unreadable timing: %+v", bad)
	}
}
