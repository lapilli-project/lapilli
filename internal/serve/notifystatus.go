package serve

import (
	"context"
	"net/http"
	"slices"
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
	Failed                  int // notices made in the last day whose last try failed
	Loc                     *time.Location
}

// Warned says something on is going nowhere, or failed to go.
func (n notifyStatus) Warned() bool { return n.DigestNowhere || n.ReportsNowhere > 0 || n.Failed > 0 }

func (s *Server) notifyStatus(ctx context.Context) (*notifyStatus, error) {
	known := s.reportDests()
	n := &notifyStatus{Webhook: s.cfg.NotifyWebhook != "", Slack: s.cfg.NotifySlack && s.cfg.Slack != "", ReportDests: len(s.cfg.ReportDests),
		TeamDigests: len(s.cfg.TeamDigests), Loc: s.cfg.Loc}
	d, err := s.digestNow(ctx)
	if err != nil {
		return nil, err
	}
	n.DigestWhen, n.DigestOn, n.DigestSet = d.When.String(), d.On, d.Set != nil
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
	st, err := s.l.Standing(ctx, dests, time.Now().Add(-24*time.Hour))
	if err != nil {
		return nil, err
	}
	n.LastSent, n.Failed = st.LastSent, st.Failed
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
