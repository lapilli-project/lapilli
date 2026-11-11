package serve

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
)

// The weekly digest, set in the UI or the API (docs/design/serveops.md):
// the flags set it until an admin changes it there; from then on the
// ledger does, and the flags only name where it can go.

// digestIn is the weekly digest in effect: when, where to, whether it is
// sent, and, when the ledger sets it, as it was set.
type digestIn struct {
	When  Weekly
	Dests []string
	On    bool
	Set   *ledger.DigestSet
}

// goes reports whether the digest goes to a destination.
func (d digestIn) goes(dest string) bool { return d.On && slices.Contains(d.Dests, dest) }

// flagDests are where the flags send the digest: Slack, the webhook and
// each team's own.
func (s *Server) flagDests() []string {
	var dests []string
	if s.cfg.Slack != "" {
		dests = append(dests, "slack")
	}
	if s.cfg.NotifyWebhook != "" {
		dests = append(dests, "webhook")
	}
	for _, team := range slices.Sorted(maps.Keys(s.cfg.TeamDigests)) {
		dests = append(dests, "team:"+team)
	}
	return dests
}

// digestNow is the weekly digest in effect: the ledger's, when it was set
// there, or the flags'.
func (s *Server) digestNow(ctx context.Context) (digestIn, error) {
	flags := digestIn{When: s.cfg.Digest, Dests: s.flagDests(), On: true}
	d, ok, err := s.l.Digest(ctx)
	if err != nil || !ok {
		return flags, err
	}
	w, err := ParseWeekly(strings.TrimPrefix(d.Schedule, "weekly "))
	if err != nil {
		return flags, err
	}
	return digestIn{When: w, Dests: d.Dests, On: d.Enabled, Set: &d}, nil
}

// putDigest sets the weekly digest, refusing destinations nothing names.
func (s *Server) putDigest(ctx context.Context, d ledger.DigestSet, by string) error {
	if err := s.knowsDests(ledger.Scheduled{Dests: d.Dests}); err != nil {
		return err
	}
	return s.l.PutDigest(ctx, d, by, s.now())
}

// digestPreview is the weekly digest as it would be sent now, for the last
// week it fell due for, sent nowhere: everyone's.
func (s *Server) digestPreview(ctx context.Context) (*built, error) {
	in, err := s.digestNow(ctx)
	if err != nil {
		return nil, err
	}
	o, err := s.options(ctx)
	if err != nil {
		return nil, err
	}
	due := in.When.Last(s.now(), s.cfg.Loc)
	week := ledger.Week(s.l.Day(due))
	from, to := s.l.Midnight(week-7), s.l.Midnight(week)
	rep, err := s.digestReport(ctx, from, to, o)
	if err != nil {
		return nil, err
	}
	spare, err := s.spareRules(ctx)
	if err != nil {
		return nil, err
	}
	text, err := s.digestText(ctx, rep, spare, from, to, nil)
	if err != nil {
		return nil, err
	}
	return &built{Name: "weekly digest", Due: due, From: from, To: to, Zone: "ledger", Sections: digestSections, Text: text}, nil
}

// logDigest says at start which sets the weekly digest, when the ledger
// does: --digest isn't used then.
func (s *Server) logDigest(ctx context.Context) {
	if in, err := s.digestNow(ctx); err == nil && in.Set != nil {
		s.log.Info("the weekly digest is set in the ledger; --digest isn't used", "when", in.When.String(), "on", in.On,
			"by", in.Set.By, "at", in.Set.Updated.Format(time.RFC3339))
	}
}

// digestWeek is the week the digest falls due for at now, and its span.
func (s *Server) digestWeek(when Weekly) (week int64, from, to time.Time) {
	due := when.Last(s.now(), s.cfg.Loc)
	week = ledger.Week(s.l.Day(due))
	return week, s.l.Midnight(week - 7), s.l.Midnight(week)
}
