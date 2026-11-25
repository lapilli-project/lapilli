package serve

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/routing"
	"lapilli.dev/internal/verdict"
)

// What the pages show of who a firing reached (docs/design/routing.md, "In
// lapilli serve"): pages, which are notifications to an integration that
// rings a phone. Where notifications were really recorded, the recorded
// pages count; where none were, the pages worked out count, and every page
// that shows them says which it shows: "recorded", "worked out", or both
// where a span has some of each.

// The basis of a count of pages.
const (
	basisRecorded  = "recorded"
	basisWorkedOut = "worked out"
	basisBoth      = "recorded and worked out"
)

// pageEvent is a page: a notification that rings a phone, for a rule, when
// it went; Nothing says it was for nothing (a firing that caught nothing,
// or, with responses, wasn't useful); Recorded, that it was really sent and
// kept, not worked out.
type pageEvent struct {
	Rule string
	At   time.Time
	// Start is when its firing started, zero where not known: the day a
	// week judged by day counts it on, as the recorded pages are.
	Start    time.Time
	Nothing  bool
	Recorded bool
}

// pagedBasis is the basis of a rule's pages: none, as nothing paged.
func pagedBasis(recorded, worked int) string {
	switch {
	case recorded > 0 && worked > 0:
		return basisBoth
	case recorded > 0:
		return basisRecorded
	case worked > 0:
		return basisWorkedOut
	}
	return ""
}

// pagesLine is a rule's pages over a span.
type pagesLine struct {
	// PagedOn says pages are counted for it: recorded, or worked out.
	PagedOn bool
	// Paged are its pages; PagedRecorded and PagedWorkedOut those of them
	// that were recorded and that were worked out; PagedAsleep and
	// PagedOffHours those that went while people slept and off hours;
	// PagedNothing those for nothing, and PagedNothingAsleep those of them
	// while people slept.
	Paged, PagedRecorded, PagedWorkedOut int
	PagedAsleep, PagedOffHours           int
	PagedNothing, PagedNothingAsleep     int
	// PagedFrom is the day pages are counted from, the first notification
	// recorded or the first hour worked out: before it, none is known.
	// PagedUnknown says some of the hours aren't known (not worked out, or
	// the work stopped or failed): no number is given for them.
	PagedFrom    string
	PagedUnknown bool
}

// PagedBasis says which the rule's pages are.
func (l pagesLine) PagedBasis() string { return pagedBasis(l.PagedRecorded, l.PagedWorkedOut) }

// pagingOfRecorded says whether a recorded notification to a receiver is a
// page, by the configuration and by the model's own rule
// (routing.PageRule): an alert manager's recorder names the integration too
// (receiver/kind, receiver/kind[1]); a webhook port names the receiver
// alone, which pages when any of its integrations does. Where the
// integration's kind is one whose tool dedups on the group key
// (routing.DedupsByGroup), a repeat or a "new alerts" notification isn't a
// new page, and only a firing notification after none, or after all
// resolved, is: the notification's reason is first; but a receiver
// --paging-receivers matches pages for each notification, whatever its
// kind. The notification of resolved alerts alone is none.
func pagingOfRecorded(cfg *routing.Config, re *regexp.Regexp, grafana bool) func(name, reason string, resolved bool) bool {
	return func(name, reason string, resolved bool) bool {
		if resolved {
			return false
		}
		base, kind := name, ""
		if _, ok := cfg.Receiver(name); !ok {
			if i := strings.LastIndex(name, "/"); i > 0 {
				base, kind = name[:i], name[i+1:]
				if j := strings.Index(kind, "["); j >= 0 {
					kind = kind[:j]
				}
			}
		}
		rc, ok := cfg.Receiver(base)
		if !ok || len(rc.Integrations) == 0 {
			return false
		}
		matched := re != nil && re.MatchString(base)
		why := routing.Reason(reason)
		if kind != "" {
			_, page := routing.PageRule(kind, grafana, matched, true, why)
			return page
		}
		// A name without an integration: it pages by any integration that
		// does.
		for _, ig := range rc.Integrations {
			if _, page := routing.PageRule(ig.Kind, grafana, matched, true, why); page {
				return true
			}
		}
		return false
	}
}

// recordedBase is the receiver a recorded name is of: the name before the
// integration an alert manager's recorder adds.
func recordedBase(cfg *routing.Config, name string) string {
	if _, ok := cfg.Receiver(name); ok {
		return name
	}
	if i := strings.LastIndex(name, "/"); i > 0 {
		if _, ok := cfg.Receiver(name[:i]); ok {
			return name[:i]
		}
	}
	return name
}

// loadPageEvents works out the pages of the weeks shown: those really
// recorded, to a receiver the configuration says pages, and, for what no
// recorded notification covers, those worked out, in the hours that were
// worked out. A page worked out is left out where its receiver has recorded
// notifications in its hour: the recorded ones count there, and a receiver's
// pages after its recording stops are worked out again. The configuration is
// read here when none is kept (routingForView), so that every replica counts
// the same.
func (s *Server) loadPageEvents(ctx context.Context, p *paged, from, to time.Time) error {
	if !s.notifies() {
		return nil
	}
	cfg := s.routingForView(ctx)
	cover, err := s.l.RoutedCover(ctx)
	if err != nil {
		return err
	}
	p.cover, p.routed = cover, len(cover) > 0
	if len(cover) > 0 {
		p.routedFrom = cover[0].From // where the work-out began, whether or not it came out
	}
	if p.upTo, err = s.firingsUpTo(ctx); err != nil {
		return err
	}
	if p.upTo.IsZero() {
		p.upTo = s.now()
	}
	if cfg == nil && !p.routed {
		return nil
	}
	byUseful := s.cfg.Options.NoisyBy == verdict.NoisyByUseful
	grafana := s.cfg.AlertmanagerConfig == ""
	// Receivers that really sent notifications, and from when, and in which
	// hours.
	first := map[string]time.Time{}
	hours := map[string]map[int64]bool{}
	p.recorded, p.pagers = map[string]map[int64]bool{}, nil
	if cfg != nil {
		p.pagers = pagingReceivers(cfg, s.cfg.PagingReceivers, grafana)
		recFirst, err := s.l.RecordedFirst(ctx)
		if err != nil {
			return err
		}
		for name, at := range recFirst {
			// Recorded from the hour its first came in, as the Notifications
			// page counts a receiver from: a page worked out a second ahead of
			// the one recorded isn't a second page.
			at = at.Truncate(time.Hour)
			b := recordedBase(cfg, name)
			if was, ok := first[b]; !ok || at.Before(was) {
				first[b] = at
			}
		}
		rh, err := s.l.RecordedHours(ctx, from.Add(-time.Hour), to)
		if err != nil {
			return err
		}
		for name, hs := range rh {
			b := recordedBase(cfg, name)
			if hours[b] == nil {
				hours[b] = map[int64]bool{}
			}
			for h := range hs {
				hours[b][h] = true
			}
		}
		p.recorded = hours
		pages := pagingOfRecorded(cfg, s.cfg.PagingReceivers, grafana)
		for _, pg := range p.pages {
			if pages(pg.Receiver, pg.Reason, pg.Resolved) {
				p.events = append(p.events, pageEvent{Rule: pg.Rule, At: pg.At, Start: pg.Start, Nothing: pg.Stone == ledger.PageBlack, Recorded: true})
			}
		}
	}
	p.recordedFrom = first
	if !p.routed {
		return nil
	}
	// Each page is a row of the notifications kept for a firing in an hour
	// worked out: a firing's pages in a week are those of its hours in the
	// week, whatever else of its life isn't known.
	rows, more, err := s.l.RoutedPages(ctx, from, to, s.pageRows())
	if err != nil {
		return err
	}
	// The rows are the earliest first: where there were more than are read,
	// the pages from the time of the last read on are cut short (those at
	// that second may be only some), and no span reaching it is known.
	p.routedMore = more
	if more && len(rows) > 0 {
		p.routedCut = rows[len(rows)-1].At
	}
	for _, r := range rows {
		nothing := r.Black
		if byUseful {
			nothing = r.Useful == 0
		}
		if !cover.Covers(r.At, r.At.Add(time.Second)) {
			continue // not worked out for the hour it went in
		}
		if recordedNear(hours[r.Receiver], r.At) {
			continue // recorded: those count
		}
		p.events = append(p.events, pageEvent{Rule: r.Rule, At: r.At, Start: r.Start, Nothing: nothing})
	}
	return nil
}

// pagingReceivers are the receivers the configuration says page: one with
// an integration of a paging kind, or one --paging-receivers matches. A page
// worked out or recorded is by one of them.
func pagingReceivers(cfg *routing.Config, re *regexp.Regexp, grafana bool) map[string]bool {
	out := map[string]bool{}
	for _, name := range cfg.Receivers() {
		rc, ok := cfg.Receiver(name)
		if !ok {
			continue
		}
		matched := re != nil && re.MatchString(name)
		for _, ig := range rc.Integrations {
			if paging, _ := routing.PageRule(ig.Kind, grafana, matched, true, routing.ReasonFirst); paging {
				out[name] = true
				break
			}
		}
	}
	return out
}

// pageWindow is how far from a worked-out page a recorded notification of
// the same receiver is taken to be its copy: the model's flushes and the
// real ones differ by seconds, and an hour boundary between them is no
// reason to count both.
const pageWindow = time.Minute

// recordedNear reports whether a receiver recorded a notification in the
// hour a page went in, or in the hour of the minute before it or after it
// (hours are the unit the recordings are kept by), so that a page worked out
// just before an hour and the one recorded just after it count once.
func recordedNear(hours map[int64]bool, at time.Time) bool {
	return hours[at.Truncate(time.Hour).Unix()] ||
		hours[at.Add(-pageWindow).Truncate(time.Hour).Unix()] ||
		hours[at.Add(pageWindow).Truncate(time.Hour).Unix()]
}

// known reports whether the pages over [from, to) are known: counted from
// when the first notification was recorded or the first hour tried (the
// part before that is counted from there, and shown as partial), and up to
// where the ledger's firings are, every hour is known for each receiver that
// pages: for a receiver that has notifications recorded in the hour, as
// recorded (those are what was sent, and a receiver's recordings in one hour
// say nothing of another's); otherwise as worked out, when the hour is. An
// hour that is neither for some receiver that pages (not tried, or tried
// and not worked out: the work stopped or failed, the configuration
// couldn't be read) makes the whole span not known, not none, whatever
// another receiver recorded there. The hours before the first one tried are
// those of the recording alone. Where the configuration isn't read, which
// receivers page isn't known, and such an hour isn't known either. Where
// notifications are only recorded, pages are as recorded.
func (p *paged) known(from, to time.Time) bool {
	if !p.routed {
		return true
	}
	lo, hi := maxTime(from, p.countedFrom()), minTime(to, p.upTo)
	if !lo.Before(hi) {
		return true
	}
	if p.routedMore && hi.After(p.routedCut) {
		return false // the pages from there on were cut short
	}
	for _, gap := range p.cover.Gaps(lo, hi) {
		// Before the first hour tried, the notifications really sent are
		// what is counted, from when the first was recorded: nothing
		// there needs working out.
		if !gap[1].After(p.routedFrom) {
			continue
		}
		gap[0] = maxTime(gap[0], p.routedFrom)
		for h := gap[0].Truncate(time.Hour); h.Before(gap[1]); h = h.Add(time.Hour) {
			if p.pagers == nil {
				return false
			}
			for name := range p.pagers {
				if !p.recorded[name][h.Unix()] {
					return false
				}
			}
		}
	}
	return true
}

// partialFrom reports whether the pages over [from, to) are counted from
// part-way into it: the first notification recorded, or the first hour
// worked out, came after from.
func (p *paged) partialFrom(from, to time.Time) bool {
	c := p.countedFrom()
	return c.After(from) && c.Before(to)
}

// pageLines are, by rule, the pages that went in [from, to), counted while
// people slept and off hours by the hours of each rule's team.
func (p *paged) pageLines(from, to time.Time, of func(rule string) *verdict.Hours) map[string]*pagesLine {
	out := map[string]*pagesLine{}
	for _, ev := range p.events {
		if ev.At.Before(from) || !ev.At.Before(to) {
			continue
		}
		x := out[ev.Rule]
		if x == nil {
			x = &pagesLine{PagedOn: true}
			out[ev.Rule] = x
		}
		x.Paged++
		if ev.Recorded {
			x.PagedRecorded++
		} else {
			x.PagedWorkedOut++
		}
		asleep := false
		if h := of(ev.Rule); h != nil {
			switch h.Of(ev.At) {
			case verdict.Asleep:
				x.PagedAsleep++
				asleep = true
			case verdict.OffHours:
				x.PagedOffHours++
			}
		}
		if ev.Nothing {
			x.PagedNothing++
			if asleep {
				x.PagedNothingAsleep++
			}
		}
	}
	return out
}

// any reports whether notifications are counted at all: some were recorded,
// or some hours are worked out.
func (p *paged) any() bool { return !p.first.IsZero() || p.routed }

// pagesCounted reports whether pages are counted at all: some were
// recorded, or some are worked out.
func (p *paged) pagesCounted() bool { return p.routed || len(p.events) > 0 }

// countedFrom is when the first number of pages counts from: the first
// notification recorded or the first hour worked out, the earlier.
func (p *paged) countedFrom() time.Time {
	switch {
	case p.first.IsZero():
		return p.routedFrom
	case !p.routed || p.first.Before(p.routedFrom):
		return p.first
	}
	return p.routedFrom
}

// basisOverall says which the pages of the weeks shown are, in the words
// basisRecorded, basisWorkedOut and basisBoth: of those counted, or, where
// none paged, of what is there to count them from.
func (p *paged) basisOverall() string {
	rec, worked := 0, 0
	for _, ev := range p.events {
		if ev.Recorded {
			rec++
		} else {
			worked++
		}
	}
	switch b := pagedBasis(rec, worked); {
	case b != "":
		return b
	case p.routed && p.first.IsZero():
		return basisWorkedOut
	case !p.routed:
		return basisRecorded
	}
	return basisBoth
}

// routedShown is what a firing reached, worked out, as a page shows it.
type routedShown struct {
	Notified, Paged int
	FirstPage       time.Time
	// Receivers are the receivers it reached, those that send nothing
	// said so; Muted what kept it out of a notification.
	Receivers []string
	Empty     []string
	Muted     []ledger.RoutedMute
}

// routedFirings gives firing lines what was worked out of each, where the
// notifications are worked out.
func (s *Server) routedFirings(ctx context.Context, lines []firingLine) error {
	ids := make([]int64, len(lines))
	for i, l := range lines {
		ids[i] = l.ID
	}
	upTo, err := s.routedUpTo(ctx)
	if err != nil {
		return err
	}
	got, err := s.l.RoutedOf(ctx, ids, upTo)
	if err != nil {
		return err
	}
	for i := range lines {
		r, ok := got[lines[i].ID]
		if !ok {
			continue
		}
		x := &routedShown{Notified: r.Notified, Paged: r.Paged, FirstPage: r.FirstPage, Muted: r.Muted}
		for _, rc := range r.Receivers {
			switch {
			case rc.Empty:
				x.Empty = append(x.Empty, rc.Receiver)
			case !slices.Contains(x.Receivers, rc.Receiver):
				x.Receivers = append(x.Receivers, rc.Receiver)
			}
		}
		lines[i].Routed = x
	}
	return nil
}

// receiverRow is a receiver's notifications over the span worked out:
// those the model worked out and those really recorded for it, from when
// each is counted.
type receiverRow struct {
	Receiver string
	// WorkedOut and Recorded are the notifications of each kind over the
	// span compared (Compared) or, for a receiver none was recorded for,
	// over the span worked out; WorkedOutPages the pages among the worked-out
	// ones.
	WorkedOut, Recorded, WorkedOutPages int
	// Compared says the receiver has both: some notification was recorded
	// for it, in the span the notifications are worked out for. From is
	// when its count starts: the first hour worked out, or when the first
	// notification was recorded for it, the later.
	Compared bool
	From     time.Time
	// RecordedOnly says the receiver has notifications recorded and none
	// worked out (a receiver of an alert that isn't a firing in the ledger,
	// as a Watchdog's): it isn't compared, and doesn't count in the
	// agreement. Webhook says the receiver is recorded by its name alone (a
	// webhook port tells of a notification once, whatever its integrations):
	// its worked-out count is that of its webhook integrations.
	RecordedOnly, Webhook bool
}

// Agreement is how much alike the two counts are: the smaller over the
// larger, 1 for the same number, 1 where both are none.
func (r receiverRow) Agreement() float64 {
	if r.WorkedOut == 0 && r.Recorded == 0 {
		return 1
	}
	return float64(min(r.WorkedOut, r.Recorded)) / float64(max(r.WorkedOut, r.Recorded))
}

// notificationsView is the Notifications page: for each receiver, what was
// worked out and what was recorded, and how far they agree.
type notificationsView struct {
	// On says the notifications are worked out; From and To the span they
	// are worked out over, as far as any run has.
	On       bool
	From, To time.Time
	Rows     []receiverRow
	// Agreement is, over the receivers with both, the sum of the smaller
	// counts over the sum of the larger: lapilli_notifications_agreement.
	// Compared is how many receivers that is; 0 gives no agreement.
	Agreement float64
	Compared  int
	// Notified and Paged are the notifications worked out over the span, and
	// the pages among them; Stopped, why the last work-out stopped short.
	Notified, Paged int
	Stopped, Failed string
	// Unknown are the stretches not worked out, or worked out with a caveat,
	// each with why in full, the latest first, at most unknownShown of them;
	// UnknownMore how many more there were.
	Unknown     []string
	UnknownMore int
	// RecordedOnly is how many receivers have notifications recorded and
	// none worked out: they are counted apart from the agreement.
	RecordedOnly int
	// Recorded says some notification was recorded at all.
	Recorded bool
	// ConfigErr says the routing configuration isn't read.
	ConfigErr bool
}

// unknownShown is how many stretches not worked out the Notifications page
// lists.
const unknownShown = 5

// countsOver are the notifications worked out, by receiver and the kind of
// integration, and those recorded, by the receiver's name as recorded, that
// went in the stretches of [from, to) that were worked out: a recorded
// notification in an hour not worked out is no one's to compare.
func (s *Server) countsOver(ctx context.Context, cover ledger.RoutedCover, from, to time.Time) (map[string]map[string]ledger.RoutedCount, map[string]ledger.RecordedCount, error) {
	worked := map[string]map[string]ledger.RoutedCount{}
	rec := map[string]ledger.RecordedCount{}
	for _, iv := range cover {
		if !iv.Worked {
			continue
		}
		a, b := maxTime(iv.From, from), minTime(iv.To, to)
		if !a.Before(b) {
			continue
		}
		w, err := s.l.RoutedByReceiverKind(ctx, a, b)
		if err != nil {
			return nil, nil, err
		}
		for name, kinds := range w {
			if worked[name] == nil {
				worked[name] = map[string]ledger.RoutedCount{}
			}
			for k, c := range kinds {
				x := worked[name][k]
				x.Notifications += c.Notifications
				x.Pages += c.Pages
				worked[name][k] = x
			}
		}
		r, err := s.l.RecordedByReceiver(ctx, a, b)
		if err != nil {
			return nil, nil, err
		}
		for name, c := range r {
			x := rec[name]
			x.Notifications += c.Notifications
			if x.First.IsZero() || c.First.Before(x.First) {
				x.First = c.First
			}
			rec[name] = x
		}
	}
	return worked, rec, nil
}

// notificationsNow works the Notifications page out: for each receiver the
// notifications worked out and recorded over the hours worked out, and the
// agreement between them (docs/design/routing.md). Worked-out rows are for
// each integration, and the recorder of an alert manager names the integration
// too, so those are compared as they are; a webhook port records a
// notification once, under the receiver's name alone, and is compared with
// that receiver's webhook integrations only. A receiver with notifications
// recorded and none worked out is counted apart (RecordedOnly), not in the
// agreement.
func (s *Server) notificationsNow(ctx context.Context) (*notificationsView, error) {
	v := &notificationsView{On: s.notifies()}
	if !v.On {
		return v, nil
	}
	cfg := s.routingForView(ctx)
	s.routed.Lock()
	v.Stopped, v.Failed = s.routed.stopped, s.routed.failed
	v.ConfigErr = cfg == nil || s.routed.readFailed != ""
	s.routed.Unlock()
	cover, err := s.l.RoutedCover(ctx)
	if err != nil {
		return nil, err
	}
	for i := len(cover) - 1; i >= 0; i-- {
		if cover[i].Why == "" {
			continue
		}
		if len(v.Unknown) == unknownShown {
			v.UnknownMore++
			continue
		}
		what := "not worked out"
		if cover[i].Worked {
			what = "worked out, with a caveat"
		}
		v.Unknown = append(v.Unknown, fmt.Sprintf("%s to %s, %s: %s", cover[i].From.In(s.cfg.Loc).Format("2006-01-02 15:04"),
			cover[i].To.In(s.cfg.Loc).Format("2006-01-02 15:04"), what, cover[i].Why))
	}
	from, ok := cover.First()
	to, _ := cover.Last()
	if !ok {
		return v, nil
	}
	v.From, v.To = from, to
	worked, rec, err := s.countsOver(ctx, cover, from, to)
	if err != nil {
		return nil, err
	}
	first, err := s.l.RecordedFirst(ctx)
	if err != nil {
		return nil, err
	}
	base := func(name string) string {
		if cfg == nil {
			return name
		}
		return recordedBase(cfg, name)
	}
	recorded, firstOf, bare := map[string]int{}, map[string]time.Time{}, map[string]bool{}
	for name, c := range rec {
		recorded[base(name)] += c.Notifications
		if base(name) == name {
			bare[name] = true
		}
	}
	for name, at := range first {
		b := base(name)
		if was, ok := firstOf[b]; !ok || at.Before(was) {
			firstOf[b] = at
		}
		if b == name {
			bare[b] = true
		}
	}
	v.Recorded = len(first) > 0
	// A receiver's worked-out count, for the kinds that are compared.
	of := func(w map[string]map[string]ledger.RoutedCount, name string) ledger.RoutedCount {
		var c ledger.RoutedCount
		for k, x := range w[name] {
			if bare[name] && k != "webhook" {
				continue
			}
			c.Notifications += x.Notifications
			c.Pages += x.Pages
		}
		return c
	}
	all := func(w map[string]map[string]ledger.RoutedCount, name string) (n int) {
		for _, x := range w[name] {
			n += x.Notifications
		}
		return n
	}
	names := map[string]bool{}
	for name := range worked {
		names[name] = true
	}
	for name := range recorded {
		names[name] = true
	}
	var sumMin, sumMax int
	for _, name := range slices.Sorted(maps.Keys(names)) {
		row := receiverRow{Receiver: name, From: from, Webhook: bare[name]}
		t, hasRecorded := firstOf[name]
		if hasRecorded && !t.Before(to) {
			hasRecorded = false
		}
		if hasRecorded && t.After(from) {
			// Counted from when its first came, to the hour.
			row.From = t.Truncate(time.Hour)
		}
		w, r := worked, rec
		if hasRecorded && row.From.After(from) {
			if w, r, err = s.countsOver(ctx, cover, row.From, to); err != nil {
				return nil, err
			}
		}
		c := of(w, name)
		row.WorkedOut, row.WorkedOutPages = c.Notifications, c.Pages
		if hasRecorded && all(w, name) == 0 && all(worked, name) == 0 {
			// Nothing was worked out for it at all: it isn't compared.
			row.RecordedOnly = true
			for rn, x := range r {
				if base(rn) == name {
					row.Recorded += x.Notifications
				}
			}
			v.RecordedOnly++
		} else if hasRecorded {
			row.Compared = true
			for rn, x := range r {
				if base(rn) == name {
					row.Recorded += x.Notifications
				}
			}
			sumMin += min(row.WorkedOut, row.Recorded)
			sumMax += max(row.WorkedOut, row.Recorded)
			v.Compared++
		}
		for _, x := range worked[name] {
			v.Notified += x.Notifications
			v.Paged += x.Pages
		}
		v.Rows = append(v.Rows, row)
	}
	if v.Compared > 0 {
		v.Agreement = 1
		if sumMax > 0 {
			v.Agreement = float64(sumMin) / float64(sumMax)
		}
	}
	return v, nil
}

// routedMetrics writes the metrics of the pages and of how far the
// notifications worked out agree with those recorded:
//
//	lapilli_rule_pages{rule, basis}: pages over the seven days to today
//	  for each of the ledger's rules, recorded or worked out.
//	lapilli_notifications_agreement: over the receivers that have both
//	  recorded and worked-out notifications in the hours worked out, the
//	  sum over them of the smaller of the two counts over the sum of the
//	  larger; 1 is the same number everywhere. Not written where no
//	  receiver has both.
//	lapilli_notifications_agreement_receiver{receiver}: the same for one
//	  receiver, the smaller count over the larger.
func (s *Server) routedMetrics(ctx context.Context, b *bytes.Buffer, today int64) error {
	p, err := s.notified(ctx)
	if err != nil {
		return err
	}
	table, err := s.l.RuleTable(ctx)
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(table))
	for _, rr := range table {
		known[rr.Name] = true
	}
	type key struct{ rule, basis string }
	by := map[key]int{}
	from, to := s.l.Midnight(today-6), s.l.Midnight(today+1)
	for _, ev := range p.events {
		if !known[ev.Rule] || ev.At.Before(from) || !ev.At.Before(to) {
			continue
		}
		basis := basisWorkedOut
		if ev.Recorded {
			basis = basisRecorded
		}
		by[key{ev.Rule, basis}]++
	}
	b.WriteString("# HELP lapilli_rule_pages Pages for each rule over the last seven days: notifications that ring a phone, by basis, recorded (really sent and kept) or worked out (from the alert manager's configuration, where none was recorded).\n# TYPE lapilli_rule_pages gauge\n")
	for _, k := range slices.SortedFunc(maps.Keys(by), func(a, b key) int { return strings.Compare(a.rule+"\x00"+a.basis, b.rule+"\x00"+b.basis) }) {
		fmt.Fprintf(b, "lapilli_rule_pages{rule=\"%s\",basis=\"%s\"} %d\n", labelValue(k.rule), k.basis, by[k])
	}
	v, err := s.notificationsNow(ctx)
	if err != nil {
		return err
	}
	if v.Compared == 0 {
		return nil
	}
	fmt.Fprintf(b, "# HELP lapilli_notifications_agreement How far the notifications worked out agree with those recorded, over the receivers that have both in the hours worked out: the sum over them of the smaller of the two counts over the sum of the larger. 1 is the same number for every receiver.\n# TYPE lapilli_notifications_agreement gauge\nlapilli_notifications_agreement %g\n", v.Agreement)
	b.WriteString("# HELP lapilli_notifications_agreement_receiver The same for one receiver: the smaller of the notifications worked out and recorded over the larger.\n# TYPE lapilli_notifications_agreement_receiver gauge\n")
	for _, r := range v.Rows {
		if r.Compared {
			fmt.Fprintf(b, "lapilli_notifications_agreement_receiver{receiver=\"%s\"} %g\n", labelValue(r.Receiver), r.Agreement())
		}
	}
	return nil
}

// reportRouted gives a report's firings, rules and totals what was worked
// out of who each reached, in the fields lapilli verdict gives them in
// (verdict.FiringNotifications and the others), marked worked_out: a
// firing not worked out has none. Where notifications were recorded, the
// report still gives what was worked out: the recorded ones are the pages'
// and the Notifications page's.
func (s *Server) reportRouted(ctx context.Context, rep *verdict.Report, o verdict.Options) error {
	cfg := s.routingForView(ctx)
	cover, err := s.l.RoutedCover(ctx)
	if err != nil || !cover.Any() || cfg == nil {
		return err
	}
	own, err := s.owners(ctx)
	if err != nil {
		return err
	}
	sc := own.schedules(o.Hours)
	ids := make([]int64, 0, len(rep.Firings))
	for _, j := range rep.Firings {
		if j.FiringID != 0 {
			ids = append(ids, j.FiringID)
		}
	}
	upTo, err := s.routedUpTo(ctx)
	if err != nil {
		return err
	}
	got, err := s.l.RoutedOf(ctx, ids, upTo)
	if err != nil {
		return err
	}
	useless := func(j verdict.Judged) bool {
		if rep.Responses != nil {
			return j.Response != nil && j.Response.Useful != nil && !*j.Response.Useful
		}
		return !j.White
	}
	byRule := map[string]*verdict.RuleNotifications{}
	inhibitors := map[string]map[string]int{}
	reached := map[string]bool{}
	// Why a rule's numbers aren't known: some hour of one of its firings
	// wasn't worked out, the firing from its start to its end (to the
	// period's end while it still fires).
	ruleStopped := map[string]string{}
	for i := range rep.Firings {
		j := &rep.Firings[i]
		if ruleStopped[j.Rule] == "" {
			end := j.End
			if end.IsZero() {
				// One that is on needs every hour to the period's end, or to
				// where the ledger's firings are, if that is sooner.
				end = minTime(rep.To, upTo)
			}
			if end.Before(j.Start) {
				end = j.Start
			}
			if !end.After(j.Start) {
				end = j.Start.Add(time.Nanosecond)
			}
			if why := cover.Unknown(j.Start, end); why != "" {
				ruleStopped[j.Rule] = why
			} else if len(cover.Gaps(j.Start, end)) > 0 {
				ruleStopped[j.Rule] = "the notifications of some of these hours aren't worked out"
			}
		}
		r, ok := got[j.FiringID]
		if !ok {
			continue
		}
		rn := byRule[j.Rule]
		if rn == nil {
			rn = &verdict.RuleNotifications{}
			byRule[j.Rule] = rn
		}
		fn := &verdict.FiringNotifications{Notified: r.Notified, Paged: r.Paged, FirstPage: r.FirstPage, Receivers: []string{}}
		for _, rc := range r.Receivers {
			if rc.Empty {
				fn.NoIntegrations = append(fn.NoIntegrations, rc.Receiver)
			}
			if !slices.Contains(fn.Receivers, rc.Receiver) {
				fn.Receivers = append(fn.Receivers, rc.Receiver)
				reached[rc.Receiver] = true
			}
		}
		inhibited := false
		for _, m := range r.Muted {
			fn.Muted = append(fn.Muted, verdict.NotificationMute{Kind: m.Kind, By: m.By, SourceRule: m.SourceRule})
			if m.Kind == string(routing.MutedInhibition) {
				inhibited = true
				if m.SourceRule != "" {
					if inhibitors[j.Rule] == nil {
						inhibitors[j.Rule] = map[string]int{}
					}
					inhibitors[j.Rule][m.SourceRule]++
				}
			}
		}
		if inhibited {
			rn.Inhibited++
		}
		if r.Grouped {
			rn.Grouped++
		}
		rn.Notified += r.Notified
		rn.Paged += r.Paged
		j.Notifications = fn
	}
	// The pages' hours, counted from every page kept for the firings, not a
	// sample of them.
	byID := map[int64]*verdict.Judged{}
	var worked []int64
	for i := range rep.Firings {
		if j := &rep.Firings[i]; j.Notifications != nil {
			byID[j.FiringID] = j
			worked = append(worked, j.FiringID)
		}
	}
	if err := s.l.EachRoutedPage(ctx, worked, func(id int64, at time.Time, _ string) {
		j := byID[id]
		rn := byRule[j.Rule]
		hours := ""
		if h := sc.of(j.Rule); h != nil {
			hours = h.Of(at)
		}
		switch hours {
		case verdict.Asleep:
			rn.PagedAsleep++
		case verdict.OffHours:
			rn.PagedOffHours++
		}
		if useless(*j) {
			rn.PagedForNothing++
			switch hours {
			case verdict.Asleep:
				rn.PagedForNothingAsleep++
			case verdict.OffHours:
				rn.PagedForNothingOffHours++
			}
		}
	}); err != nil {
		return err
	}
	for name, by := range inhibitors {
		best, n := "", 0
		for rule, c := range by {
			if c > n || c == n && rule < best {
				best, n = rule, c
			}
		}
		byRule[name].InhibitedBy = best
	}
	for i := range rep.Rules {
		n, ok := byRule[rep.Rules[i].Rule]
		if !ok {
			n = &verdict.RuleNotifications{}
		}
		n.Stopped = ruleStopped[rep.Rules[i].Rule]
		n.Known = n.Stopped == ""
		rep.Rules[i].Notifications = n
	}
	// The totals of the period, over the hours worked out: each stretch of
	// them that is, and Stopped says why for those that aren't, or are with
	// a caveat, in full, not only for the last run's.
	tot := &verdict.ReportNotifications{WorkedOut: true, Receivers: []verdict.NotificationReceiver{}}
	tot.Stopped = cover.Unknown(rep.From, rep.To)
	if gaps := cover.Gaps(rep.From, rep.To); len(gaps) > 0 && tot.Stopped == "" {
		tot.Stopped = "the notifications of some of these hours aren't worked out"
	}
	groups := map[string]bool{}
	for _, iv := range cover {
		if !iv.Worked {
			continue
		}
		from, to := maxTime(rep.From, iv.From), minTime(rep.To, iv.To)
		if !from.Before(to) {
			continue
		}
		n, pg, _, err := s.l.RoutedTotals(ctx, from, to)
		if err != nil {
			return err
		}
		tot.Notified += n
		tot.Paged += pg
		// A group that spans two stretches is one group: count the keys over
		// the whole period, not each stretch's.
		keys, err := s.l.RoutedGroupKeys(ctx, from, to)
		if err != nil {
			return err
		}
		for _, k := range keys {
			groups[k] = true
		}
		if by, err := s.l.RoutedByReceiver(ctx, from, to); err == nil {
			for name := range by {
				reached[name] = true
			}
		}
	}
	tot.Groups = len(groups)
	grafana := s.cfg.AlertmanagerConfig == ""
	for _, name := range slices.Sorted(maps.Keys(reached)) {
		rc, _ := cfg.Receiver(name)
		kinds := []string{}
		var paging []string
		for _, ig := range rc.Integrations {
			if !slices.Contains(kinds, ig.Kind) {
				kinds = append(kinds, ig.Kind)
			}
		}
		slices.SortFunc(kinds, routing.KindOrder)
		for _, k := range kinds {
			if routing.PagingKind(k, grafana) {
				paging = append(paging, k)
			}
		}
		pattern := s.cfg.PagingReceivers != nil && s.cfg.PagingReceivers.MatchString(name) && len(rc.Integrations) > 0
		why := "a message"
		switch {
		case len(rc.Integrations) == 0:
			why = "no integrations: it sends nothing"
		case pattern && len(paging) > 0:
			why = "pages by its " + strings.Join(paging, ", ") + " and --paging-receivers"
		case pattern:
			why = "pages by --paging-receivers"
		case len(paging) > 0:
			why = "pages by its " + strings.Join(paging, ", ")
		case len(kinds) > 0:
			why = "messages only (" + strings.Join(kinds, ", ") + ")"
		}
		tot.Receivers = append(tot.Receivers, verdict.NotificationReceiver{Name: name, Kinds: kinds, Paging: len(paging) > 0 || pattern, Why: why})
	}
	rep.Notifications = tot
	return nil
}
