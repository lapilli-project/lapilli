package serve

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"
)

// Where notices go, and whether they went (docs/design/ui.md, "First use"):
// on a deployment with no webhook and no Slack, the weekly digest was
// worked out every week and sent nowhere, and no page said so. The Manage
// page's card and a section of the Settings page say where what happens is
// told, how many places reports and teams' digests can go, when the digest
// is and how many places it goes to, when the outbox last sent anything and
// how many of the last day's notices failed; and, in words, when the digest
// or a report is on with nowhere to go. Counts and kinds, never a URL: the
// flags read those from files, and no page shows them. Nothing is kept for
// it: the flags, the reports and the outbox say it all.

// notifyStatus is where notices go, and whether they went.
type notifyStatus struct {
	// Webhook and Slack are where what happens is told.
	Webhook, Slack bool
	// ReportDests are the --report-dest names; TeamDigests, the teams with
	// a digest of their own.
	ReportDests, TeamDigests int
	// The weekly digest: when; whether it is sent; how many places it
	// goes to that are there; set in the ledger, not by the flags; and on,
	// going nowhere.
	DigestWhen              string
	DigestOn, DigestSet     bool
	DigestTo                int
	DigestNowhere           bool
	Reports, ReportsNowhere int // reports on, and of them those with nowhere to go
	Sends                   bool
	LastSent                time.Time
	// Failed are the notices made in the last day whose last try failed,
	// ledger.FailedCounted at most; FailedMore, that there were more.
	Failed     int
	FailedMore bool
	// Posted is, when the digest goes to Slack, posted there itself, not
	// through the outbox, how its posts went, as the ledger has it.
	Posted *digestPosted
	Loc    *time.Location
}

// digestPosted is how the weekly digest's posts to Slack went, as the
// ledger keeps it, the same on every replica and after a restart: To, the
// end of the last week posted (digest-sent); Failed, a later week whose
// digest failed, worked out or posted (a run of the digest's with an
// error), At when, and FailedTo its end.
type digestPosted struct {
	To, At, FailedTo time.Time
	Failed           bool
}

func (s *Server) digestPosted(ctx context.Context) (*digestPosted, error) {
	var p digestPosted
	v, err := s.l.Meta(ctx, "digest-sent")
	if err != nil {
		return nil, err
	}
	if week, err := strconv.ParseInt(v, 10, 64); err == nil {
		p.To = s.l.Midnight(week)
	}
	r, ok, err := s.l.LastFailed(ctx, "digest")
	if err != nil {
		return nil, err
	}
	if ok && r.To.After(p.To) {
		p.Failed, p.At, p.FailedTo = true, r.Started, r.To
	}
	return &p, nil
}

// weekdays are the days of a week, as a person reads a schedule that falls
// on one each week: "Mondays 09:00".
var weekdays = [...]string{"Sundays %s", "Mondays %s", "Tuesdays %s", "Wednesdays %s", "Thursdays %s", "Fridays %s", "Saturdays %s"}

// weeklyText is a weekly time as a person reads it, in a language.
func weeklyText(lang string, w Weekly) string {
	return tr(lang, weekdays[w.Day], fmt.Sprintf("%02d:%02d", w.Hour, w.Minute))
}

func (s *Server) notifyStatus(ctx context.Context, lang string) (*notifyStatus, error) {
	known := s.reportDests()
	n := &notifyStatus{Webhook: s.cfg.NotifyWebhook != "", Slack: s.cfg.NotifySlack && s.cfg.Slack != "", ReportDests: len(s.cfg.ReportDests),
		TeamDigests: len(s.cfg.TeamDigests), Loc: s.cfg.Loc}
	d, err := s.digestNow(ctx)
	if err != nil {
		return nil, err
	}
	n.DigestWhen, n.DigestOn, n.DigestSet = weeklyText(lang, d.When), d.On, d.Set != nil
	if d.On && s.cfg.Slack != "" && slices.Contains(d.Dests, "slack") {
		if n.Posted, err = s.digestPosted(ctx); err != nil {
			return nil, err
		}
	}
	for _, x := range d.Dests {
		if slices.Contains(known, x) {
			n.DigestTo++
		}
	}
	n.DigestNowhere = d.On && n.DigestTo == 0
	reports, err := s.l.Scheduleds(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range reports {
		if !r.Enabled {
			continue
		}
		n.Reports++
		if !slices.ContainsFunc(r.Dests, func(x string) bool { return slices.Contains(known, x) }) {
			n.ReportsNowhere++
		}
	}
	dests := s.sendsTo()
	n.Sends = len(dests) > 0
	// By the wall clock, as the outbox keeps when each notice was made.
	st, err := s.l.Standing(ctx, dests, time.Now().Add(-24*time.Hour))
	if err != nil {
		return nil, err
	}
	n.LastSent, n.Failed, n.FailedMore = st.LastSent, st.Failed, st.More
	return n, nil
}

// freshPage reports whether a page is worked out for each request, not
// kept until the ledger changes: what the outbox sent is written aside
// (ledger.Sent), so that sending empties no page kept, and the pages that
// say it would say it late. The Manage page, and the Settings page but
// when it tries settings on the last week, which takes the heavy turn.
func freshPage(name string, r *http.Request) bool {
	return name == "manage" || name == "settings" && r.URL.Query().Get("try") == ""
}
