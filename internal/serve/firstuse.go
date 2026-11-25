package serve

import (
	"net/http"
	"strconv"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// What a first-time reader would otherwise get stuck on (docs/design/ui.md,
// "First use"): a time a run took as Go writes it to the nanosecond; the
// firings of a day nothing fired on.

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

// firingsSaid is, of the firings page asked for no day, which day it shows:
// the last with firings (Latest), or today, as none fired in the twelve
// weeks the pages show (None).
type firingsSaid struct{ Latest, None bool }

// lastFiringDay is the last day with firings in the twelve weeks the pages
// show, of the rule and only those that caught nothing when asked: one
// firing read, the latest, by the firings' index on their start. The labels
// picked aren't looked for: that is a scan of a day's firings, which the
// page does for the day it shows. With none, today.
func (s *Server) lastFiringDay(r *http.Request, rule string, black bool, response string) (int64, firingsSaid, error) {
	today := s.l.Day(s.now())
	fs, err := s.l.Firings(r.Context(), ledger.FiringQuery{From: s.shownFrom(), To: s.l.Midnight(today + 1), Rule: rule, Black: black, Response: response, Limit: 1})
	if err != nil || len(fs) == 0 {
		return today, firingsSaid{None: err == nil}, err
	}
	return s.l.Day(fs[0].Start), firingsSaid{Latest: true}, nil
}
