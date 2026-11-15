package serve

import (
	"strconv"
	"time"

	"lapilli.dev/internal/verdict"
)

// What a first-time reader would otherwise get stuck on (docs/design/ui.md,
// "First use"): a time a run took as Go writes it to the nanosecond.

// shownDur is a duration as a page shows it, rounded as a person reads one:
// under a tenth of a second, to the millisecond (40ms); under a minute, to a
// tenth of a second (3.4s); longer, to the second (2m5s). Whole minutes and
// hours, a rule's for among them, are as verdict.Duration writes them (15m,
// 1h30m).
func shownDur(d time.Duration) string {
	if d < 0 {
		return "-" + shownDur(-d)
	}
	switch {
	case d == 0:
		return "0s"
	case d < 100*time.Millisecond:
		return max(d.Round(time.Millisecond), time.Millisecond).String()
	case d < time.Minute:
		if r := d.Round(100 * time.Millisecond); r < time.Minute {
			return strconv.FormatFloat(r.Seconds(), 'f', -1, 64) + "s"
		}
	}
	return verdict.Duration(d.Round(time.Second))
}
