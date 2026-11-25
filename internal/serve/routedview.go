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
	Rule     string
	At       time.Time
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
	PagedFrom string
}

// PagedBasis says which the rule's pages are.
func (l pagesLine) PagedBasis() string { return pagedBasis(l.PagedRecorded, l.PagedWorkedOut) }

// pagingOfRecorded says whether a recorded notification's receiver pages,
// by the configuration: an alert manager's recorder names the integration
// too (receiver/kind, receiver/kind[1]); a webhook port names the receiver
// alone, which pages when any of its integrations does.
func pagingOfRecorded(cfg *routing.Config, re *regexp.Regexp, grafana bool) func(string) bool {
	return func(name string) bool {
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
		if re != nil && re.MatchString(base) {
			return true
		}
		if kind != "" {
			return routing.PagingKind(kind, grafana)
		}
		return slices.ContainsFunc(rc.Integrations, func(ig routing.Integration) bool { return routing.PagingKind(ig.Kind, grafana) })
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
// recorded notification covers, those worked out. A page worked out is
// left out where its receiver has recorded notifications from before it:
// the recorded ones count there.
func (s *Server) loadPageEvents(ctx context.Context, p *paged, from, to time.Time) error {
	if !s.notifies() {
		return nil
	}
	cfg := s.routingKept()
	span0, span1, ok, err := s.l.RoutedSpan(ctx)
	if err != nil {
		return err
	}
	p.routed, p.routedFrom, p.routedTo = ok, span0, span1
	if cfg == nil && !ok {
		return nil
	}
	byUseful := s.cfg.Options.NoisyBy == verdict.NoisyByUseful
	grafana := s.cfg.AlertmanagerConfig == ""
	// Receivers that really sent notifications, and from when.
	first := map[string]time.Time{}
	if cfg != nil {
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
		pages := pagingOfRecorded(cfg, s.cfg.PagingReceivers, grafana)
		for _, pg := range p.pages {
			if pages(pg.Receiver) && !pg.Resolved {
				p.events = append(p.events, pageEvent{Rule: pg.Rule, At: pg.At, Nothing: pg.Stone == ledger.PageBlack, Recorded: true})
			}
		}
	}
	p.recordedFrom = first
	if !ok {
		return nil
	}
	rows, more, err := s.l.RoutedPages(ctx, from, to, 500_000)
	if err != nil {
		return err
	}
	p.routedMore = more
	for _, r := range rows {
		nothing := r.Black
		if byUseful {
			nothing = r.Useful == 0
		}
		for _, pa := range r.PageAts {
			if pa.At.Before(from) || !pa.At.Before(to) {
				continue
			}
			if at, ok := first[pa.Receiver]; ok && !pa.At.Before(at) {
				continue // recorded: those count
			}
			p.events = append(p.events, pageEvent{Rule: r.Rule, At: pa.At, Nothing: nothing})
		}
	}
	return nil
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
	got, err := s.l.RoutedOf(ctx, ids)
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
	// Recorded says some notification was recorded at all.
	Recorded bool
	// ConfigErr says the routing configuration isn't read.
	ConfigErr bool
}

// notificationsNow works the Notifications page out: for each receiver the
// notifications worked out and recorded over the span worked out, and the
// agreement between them (docs/design/routing.md).
func (s *Server) notificationsNow(ctx context.Context) (*notificationsView, error) {
	v := &notificationsView{On: s.notifies()}
	if !v.On {
		return v, nil
	}
	cfg := s.routingKept()
	s.routed.Lock()
	v.Stopped, v.Failed = s.routed.stopped, s.routed.failed
	v.ConfigErr = cfg == nil || s.routed.readFailed != ""
	s.routed.Unlock()
	from, to, ok, err := s.l.RoutedSpan(ctx)
	if err != nil || !ok {
		return v, err
	}
	v.From, v.To = from, to
	worked, err := s.l.RoutedByReceiver(ctx, from, to)
	if err != nil {
		return nil, err
	}
	rec, err := s.l.RecordedByReceiver(ctx, from, to)
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
	recorded, firstOf := map[string]int{}, map[string]time.Time{}
	for name, c := range rec {
		recorded[base(name)] += c.Notifications
	}
	for name, at := range first {
		b := base(name)
		if was, ok := firstOf[b]; !ok || at.Before(was) {
			firstOf[b] = at
		}
	}
	v.Recorded = len(first) > 0
	names := map[string]bool{}
	for name := range worked {
		names[name] = true
	}
	for name := range recorded {
		names[name] = true
	}
	var sumMin, sumMax int
	for _, name := range slices.Sorted(maps.Keys(names)) {
		row := receiverRow{Receiver: name, From: from}
		t, hasRecorded := firstOf[name]
		if hasRecorded && !t.Before(to) {
			hasRecorded = false
		}
		if hasRecorded && t.After(from) {
			// Counted from when its first came, to the hour.
			row.From = t.Truncate(time.Hour)
		}
		w := worked[name]
		row.WorkedOut, row.WorkedOutPages = w.Notifications, w.Pages
		if hasRecorded {
			row.Compared = true
			if row.From.After(from) {
				x, err := s.l.RoutedByReceiver(ctx, row.From, to)
				if err != nil {
					return nil, err
				}
				row.WorkedOut, row.WorkedOutPages = x[name].Notifications, x[name].Pages
			}
			if row.From.After(from) {
				x, err := s.l.RecordedByReceiver(ctx, row.From, to)
				if err != nil {
					return nil, err
				}
				n := 0
				for rn, c := range x {
					if base(rn) == name {
						n += c.Notifications
					}
				}
				row.Recorded = n
			} else {
				row.Recorded = recorded[name]
			}
			sumMin += min(row.WorkedOut, row.Recorded)
			sumMax += max(row.WorkedOut, row.Recorded)
			v.Compared++
		}
		v.Notified += w.Notifications
		v.Paged += w.Pages
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
	cfg := s.routingKept()
	span0, span1, ok, err := s.l.RoutedSpan(ctx)
	if err != nil || !ok || cfg == nil {
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
	got, err := s.l.RoutedOf(ctx, ids)
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
	for i := range rep.Firings {
		j := &rep.Firings[i]
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
		nothing := useless(*j)
		for _, pa := range r.PageAts {
			hours := ""
			if h := sc.of(j.Rule); h != nil {
				hours = h.Of(pa.At)
			}
			switch hours {
			case verdict.Asleep:
				rn.PagedAsleep++
			case verdict.OffHours:
				rn.PagedOffHours++
			}
			if nothing {
				rn.PagedForNothing++
				switch hours {
				case verdict.Asleep:
					rn.PagedForNothingAsleep++
				case verdict.OffHours:
					rn.PagedForNothingOffHours++
				}
			}
		}
		j.Notifications = fn
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
		rep.Rules[i].Notifications = n
	}
	// The totals of the period, over the hours worked out.
	from, to := rep.From, rep.To
	if from.Before(span0) {
		from = span0
	}
	if to.After(span1) {
		to = span1
	}
	tot := &verdict.ReportNotifications{WorkedOut: true, Receivers: []verdict.NotificationReceiver{}}
	if from.Before(to) {
		if tot.Notified, tot.Paged, tot.Groups, err = s.l.RoutedTotals(ctx, from, to); err != nil {
			return err
		}
	}
	s.routed.Lock()
	tot.Stopped = s.routed.stopped
	s.routed.Unlock()
	if by, err := s.l.RoutedByReceiver(ctx, from, to); err == nil {
		for name := range by {
			reached[name] = true
		}
	}
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
