package verdict

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/routing"
	"lapilli.dev/internal/rules"
)

// Who each firing reached (docs/design/routing.md): the alert manager's
// routing worked out over the report's firings. Every number here is worked
// out from a configuration, not recorded: a model of what Alertmanager or
// Grafana would have done with the alerts, which the report says wherever it
// shows them. A report without it (no configuration, or --notifications=false)
// has none of these fields and reads as it always did.

// NotificationMute is what kept a firing out of a notification: Kind is
// inhibition, silence, mute_time_interval or active_time_interval, By the
// inhibition rule's name (or its number, from 1, when it has none), the
// silence's id, or the time interval's name; SourceRule, for an inhibition,
// the rule whose firing inhibited it.
type NotificationMute struct {
	Kind       string `json:"kind"`
	By         string `json:"by"`
	SourceRule string `json:"source_rule,omitempty"`
}

// NotificationSent is one notification that told of a firing: when, to which
// receiver and integration, and why it was sent.
type NotificationSent struct {
	At       time.Time `json:"at"`
	Receiver string    `json:"receiver"`
	Kind     string    `json:"kind"`
	Index    int       `json:"index"`
	// Paging is whether it was on a paging integration, and Page whether it
	// rings a phone (the one that opens an incident, on a kind that keeps one
	// for a group).
	Paging bool   `json:"paging"`
	Page   bool   `json:"page"`
	Reason string `json:"reason"`
}

// MaxSent is the most notifications a firing lists in Sent.
const MaxSent = 100

// FiringNotifications is what a firing reached, worked out.
type FiringNotifications struct {
	// Notified is the notifications that told of it firing, Paged those of
	// them that are pages, and OnPaging those on a paging integration (the
	// pages, and the repeats and updates of an incident a page opened);
	// FirstPage is when the first page went.
	Notified  int       `json:"notified"`
	Paged     int       `json:"paged"`
	OnPaging  int       `json:"on_paging"`
	FirstPage time.Time `json:"first_page,omitzero"`
	// Sent lists those notifications, the first MaxSent of them, when the
	// report was asked for them (NotificationOptions.Sent); SentMore is how
	// many more there were.
	Sent     []NotificationSent `json:"sent,omitempty"`
	SentMore int                `json:"sent_more,omitempty"`
	// Receivers are the receivers its alert reached, and NoIntegrations
	// those among them that send nothing.
	Receivers      []string `json:"receivers"`
	NoIntegrations []string `json:"no_integrations,omitempty"`
	// Muted is what kept it out of a notification, if anything did.
	Muted []NotificationMute `json:"muted,omitempty"`
}

// RuleNotifications is a rule's firings' notifications, counted.
type RuleNotifications struct {
	// Notified and Paged are the notifications that told of its firings,
	// and the pages among them, each counted for each firing it told of: a
	// notification of three of its firings counts three, so the rules'
	// counts can add up to more than the report's. OnPaging are those on a
	// paging integration, which on PagerDuty, Opsgenie, VictorOps and
	// incident.io include the repeats and updates of an incident a page
	// opened.
	Notified int `json:"notified"`
	Paged    int `json:"paged"`
	OnPaging int `json:"on_paging"`
	// PagedAsleep and PagedOffHours are the pages that went while people
	// slept and out of hours (Options.Hours, as firings are counted), 0
	// with no hours.
	PagedAsleep   int `json:"paged_asleep"`
	PagedOffHours int `json:"paged_off_hours"`
	// PagedForNothing are the pages of firings that caught nothing, or,
	// with responses, weren't useful; PagedForNothingAsleep and
	// PagedForNothingOffHours those of them while people slept and out of
	// hours.
	PagedForNothing         int `json:"paged_for_nothing"`
	PagedForNothingAsleep   int `json:"paged_for_nothing_asleep"`
	PagedForNothingOffHours int `json:"paged_for_nothing_off_hours"`
	// Inhibited are its firings that were never notified, every flush of
	// them muted by an inhibition rule, and InhibitedBy the rule whose firing
	// inhibited most of them, each firing weighing once.
	Inhibited   int    `json:"inhibited"`
	InhibitedBy string `json:"inhibited_by,omitempty"`
	// Grouped are its firings that went out in a notification with other
	// rules' alerts.
	Grouped int `json:"grouped"`
}

// NotificationReceiver is a receiver the report says something of: those
// that page, and those its firings reached. Why says whether it was taken
// as paging, and by what.
type NotificationReceiver struct {
	Name   string   `json:"name"`
	Kinds  []string `json:"kinds"`
	Paging bool     `json:"paging"`
	Why    string   `json:"why"`
}

// ReportNotifications is what a report worked out of the notifications:
// totals, which receivers page, and when it stopped.
type ReportNotifications struct {
	// WorkedOut is always true: these are worked out, not recorded.
	WorkedOut bool `json:"worked_out"`
	// Notified are the notifications sent that told of a firing, Paged the
	// pages among them and OnPaging those on a paging integration, counted
	// once each however many firings they told of, and Groups the groups
	// the alerts made.
	Notified int `json:"notified"`
	Paged    int `json:"paged"`
	OnPaging int `json:"on_paging"`
	Groups   int `json:"groups"`
	// Receivers are the receivers that page and those the firings reached.
	Receivers []NotificationReceiver `json:"receivers"`
	// Known is false when the model stopped short: the numbers are then of
	// what it worked out before it stopped, not of the period, and Stopped
	// says why. A budget of pages isn't held or broken by them.
	Known   bool   `json:"known"`
	Stopped string `json:"stopped,omitempty"`
	// Warnings are what the model changed without stopping (a timing under a
	// second read as one second).
	Warnings []string `json:"warnings,omitempty"`
}

// NotificationOptions are what a report's notifications are worked out
// with.
type NotificationOptions struct {
	Config *routing.Config
	// Run are the model's options: external labels, silences and the
	// receivers taken as paging. Until is the report's end.
	Run routing.Options
	// Rules are the rules replayed, which say each firing's alertname, as
	// written, and a Grafana rule's folder.
	Rules []rules.Rule
	// Hours, when not nil, count the pages while people slept and out of
	// hours.
	Hours *Hours
	// Also are firings of rules left alone, which take part in the model (a
	// group they share, an inhibition they cause) and are counted in none of
	// the numbers; AlsoRules are their rules.
	Also      []firing.Firing
	AlsoRules []rules.Rule
	// Sent asks for each firing's notifications to be listed (FiringNotifications.Sent).
	Sent bool
}

// NotificationAlerts are the firings as the alert manager is given them:
// alertname, the labels the firing has (the rule's and its series'), and a
// Grafana rule's grafana_folder, from the evaluation each fired on to the one
// it resolved on. rs are the rules replayed, which say the name each alert
// carries (a rule told apart from another of its name carries its own) and
// the folder.
func NotificationAlerts(fs []Judged, rs []rules.Rule) []routing.Alert {
	type known struct {
		name, folder string
		own          *routing.OwnRoute
	}
	by := map[string]known{}
	for _, r := range rs {
		if _, ok := by[r.Name]; !ok {
			by[r.Name] = known{r.AlertName(), r.Folder, OwnRoute(r)}
		}
	}
	out := make([]routing.Alert, len(fs))
	for i, j := range fs {
		l := maps.Clone(j.Labels)
		if l == nil {
			l = map[string]string{}
		}
		k, ok := by[j.Rule]
		if !ok {
			k.name = j.Rule
		}
		// Prometheus sets alertname to the rule's name, over any label of
		// the series' that has it.
		l["alertname"] = k.name
		if _, has := l["grafana_folder"]; !has && k.folder != "" {
			l["grafana_folder"] = k.folder
		}
		out[i] = routing.Alert{Labels: l, Start: j.Start, End: j.End, Own: k.own}
	}
	return out
}

// OwnRoute is the route of a Grafana rule that names its contact point
// itself: it goes to that contact point, with its own grouping and timings,
// and the notification policies aren't consulted. Nil for any other rule.
func OwnRoute(r rules.Rule) *routing.OwnRoute {
	if r.Receiver == "" || r.Notify == nil {
		return nil
	}
	n := r.Notify
	return &routing.OwnRoute{Receiver: r.Receiver, GroupBy: n.GroupBy, GroupWait: n.GroupWait, GroupInterval: n.GroupInterval,
		RepeatInterval: n.RepeatInterval, MuteTimeIntervals: n.MuteTimeIntervals, ActiveTimeIntervals: n.ActiveTimeIntervals}
}

// WorkOutNotifications works out who the report's firings reached, from the
// alert manager's routing, and sets each firing's, each rule's and the
// report's Notifications. An error is the model's (an alert without a
// start); the report is then as it was.
func (r *Report) WorkOutNotifications(o NotificationOptions) error {
	counted := len(r.Firings)
	var also []Judged
	for _, f := range o.Also {
		also = append(also, Judged{Firing: f})
	}
	alerts := NotificationAlerts(append(slices.Clone(r.Firings), also...), slices.Concat(o.Rules, o.AlsoRules))
	run := o.Run
	run.Until = r.To
	run.From = r.From
	res, err := o.Config.Run(alerts, run)
	if err != nil {
		return err
	}
	// A notification counts when it told of a firing of the report's.
	told := func(nt routing.Notification) bool {
		return slices.ContainsFunc(nt.Firing, func(i int) bool { return i < counted })
	}
	// A notification that told of firings of more than one rule.
	several := make([]bool, len(res.Notifications))
	ruleOf := func(i int) string {
		if i < counted {
			return r.Firings[i].Rule
		}
		return also[i-counted].Rule
	}
	for k, nt := range res.Notifications {
		if len(nt.Firing) < 2 {
			continue
		}
		first := ruleOf(nt.Firing[0])
		for _, i := range nt.Firing[1:] {
			if ruleOf(i) != first {
				several[k] = true
				break
			}
		}
	}
	// Whether responses are read decides what a page for nothing is.
	useless := func(j Judged) bool {
		if r.Responses != nil {
			return j.Response != nil && j.Response.Useful != nil && !*j.Response.Useful
		}
		return !j.White
	}
	byRule := map[string]*RuleNotifications{}
	forRule := func(name string) *RuleNotifications {
		if n, ok := byRule[name]; ok {
			return n
		}
		n := &RuleNotifications{}
		byRule[name] = n
		return n
	}
	inhibitors := map[string]map[string]int{}
	reached := map[string]bool{}
	for i := range r.Firings {
		j := &r.Firings[i]
		a := res.Alerts[i]
		rn := forRule(j.Rule)
		fn := &FiringNotifications{Notified: a.Notified, Paged: a.Paged, OnPaging: a.OnPaging, FirstPage: a.FirstPage,
			Receivers: slices.Clone(a.Receivers), NoIntegrations: slices.Clone(a.Empty)}
		if fn.Receivers == nil {
			fn.Receivers = []string{}
		}
		for _, name := range a.Receivers {
			reached[name] = true
		}
		// Inhibited is a firing no flush of which got through, every one
		// muted by inhibition: not one notified, and nothing else muting it.
		inhibition, otherMute, source := false, false, ""
		for _, m := range a.Muted {
			mute := NotificationMute{Kind: string(m.Kind), By: m.By}
			if m.Kind == routing.MutedInhibition {
				inhibition = true
				if m.Source >= 0 && m.Source < counted+len(also) {
					mute.SourceRule = ruleOf(m.Source)
					if source == "" {
						source = mute.SourceRule
					}
				}
			} else {
				otherMute = true
			}
			fn.Muted = append(fn.Muted, mute)
		}
		if inhibition && !otherMute && a.Notified == 0 {
			rn.Inhibited++
			if source != "" {
				if inhibitors[j.Rule] == nil {
					inhibitors[j.Rule] = map[string]int{}
				}
				inhibitors[j.Rule][source]++
			}
		}
		nothing := useless(*j)
		grouped := false
		for _, k := range a.Notifications {
			nt := res.Notifications[k]
			rn.Notified++
			if nt.Paging {
				rn.OnPaging++
			}
			grouped = grouped || several[k]
			if o.Sent {
				if len(fn.Sent) < MaxSent {
					fn.Sent = append(fn.Sent, NotificationSent{At: nt.At, Receiver: nt.Receiver, Kind: nt.Kind, Index: nt.Index,
						Paging: nt.Paging, Page: nt.Page, Reason: string(nt.Reason)})
				} else {
					fn.SentMore++
				}
			}
			if !nt.Page {
				continue
			}
			rn.Paged++
			hours := ""
			if o.Hours != nil {
				hours = o.Hours.Of(nt.At)
			}
			switch hours {
			case Asleep:
				rn.PagedAsleep++
			case OffHours:
				rn.PagedOffHours++
			}
			if nothing {
				rn.PagedForNothing++
				switch hours {
				case Asleep:
					rn.PagedForNothingAsleep++
				case OffHours:
					rn.PagedForNothingOffHours++
				}
			}
		}
		if grouped {
			rn.Grouped++
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
	for i := range r.Rules {
		n, ok := byRule[r.Rules[i].Rule]
		if !ok {
			n = &RuleNotifications{}
		}
		r.Rules[i].Notifications = n
	}
	rep := &ReportNotifications{WorkedOut: true, Groups: res.Groups, Known: res.Stopped == "", Stopped: res.Stopped,
		Warnings: res.Warnings, Receivers: []NotificationReceiver{}}
	for _, nt := range res.Notifications {
		if !told(nt) {
			continue
		}
		rep.Notified++
		if nt.Paging {
			rep.OnPaging++
		}
		if nt.Page {
			rep.Paged++
		}
	}
	paging := map[string]routing.Paging{}
	for _, p := range res.Paging {
		paging[p.Receiver] = p
		reached[p.Receiver] = true
	}
	for _, name := range slices.Sorted(maps.Keys(reached)) {
		rc, _ := o.Config.Receiver(name)
		kinds := []string{}
		for _, ig := range rc.Integrations {
			if !slices.Contains(kinds, ig.Kind) {
				kinds = append(kinds, ig.Kind)
			}
		}
		slices.SortFunc(kinds, routing.KindOrder)
		p, pages := paging[name]
		why := "a message"
		switch {
		case len(rc.Integrations) == 0:
			why = "no integrations: it sends nothing"
		case pages && len(p.Kinds) > 0 && p.ByPattern:
			why = "pages by its " + strings.Join(p.Kinds, ", ") + " and --paging-receivers"
		case pages && p.ByPattern:
			why = "pages by --paging-receivers"
		case pages:
			why = "pages by its " + strings.Join(p.Kinds, ", ")
		case len(kinds) > 0:
			why = "messages only (" + strings.Join(kinds, ", ") + ")"
		}
		rep.Receivers = append(rep.Receivers, NotificationReceiver{Name: name, Kinds: kinds, Paging: pages, Why: why})
	}
	r.Notifications = rep
	return nil
}

// Paged are the pages worked out for the report, 0 with none.
func (r *Report) Paged() int {
	if r == nil || r.Notifications == nil {
		return 0
	}
	return r.Notifications.Paged
}

const (
	routingMutedInhibition = routing.MutedInhibition
	routingMutedSilence    = routing.MutedSilence
)

// hasNotifications reports whether the report worked out the notifications.
func (r *Report) hasNotifications() bool { return r != nil && r.Notifications != nil }

// notificationsStopped is why the work-out stopped short, "" if the report
// has none, or it didn't: what was counted is then of part of the period.
func (r *Report) notificationsStopped() string {
	if r == nil || r.Notifications == nil {
		return ""
	}
	return r.Notifications.Stopped
}

// pagesNotKnown is why the pages of a comparison aren't known: the work-out
// of either side stopped short; "" when they are.
func (c *Comparison) pagesNotKnown() string {
	if s := c.Before.notificationsStopped(); s != "" {
		return s
	}
	return c.After.notificationsStopped()
}

// notKnown is how a stopped work-out's numbers are said.
func notKnown(stopped string) string {
	return "not known: the work-out stopped: " + Clean(stopped)
}

// NotificationsSaid is how every surface that shows the numbers says what
// they are.
const NotificationsSaid = "Worked out from the alert manager's configuration, not recorded: the notifications Alertmanager or Grafana would have sent for these firings."

func (n *RuleNotifications) forNothing() int {
	if n == nil {
		return 0
	}
	return n.PagedForNothing
}

func (n *RuleNotifications) forNothingAsleep() int {
	if n == nil {
		return 0
	}
	return n.PagedForNothingAsleep
}

func (n *RuleNotifications) forNothingOff() int {
	if n == nil {
		return 0
	}
	return n.PagedForNothingOffHours
}

// notificationsText writes the NOTIFICATIONS table: who each rule's firings
// reached, worked out.
func notificationsText(w io.Writer, r *Report) error {
	n := r.Notifications
	if n == nil {
		return nil
	}
	var rows [][]string
	for _, rr := range r.Rules {
		if rr.Firings == 0 || rr.Notifications == nil || rr.Unsupported != "" && !rr.ByHistory() {
			continue
		}
		c := rr.Notifications
		inhibited := "–"
		if c.Inhibited > 0 {
			inhibited = fmt.Sprintf("%.0f%%", 100*float64(c.Inhibited)/float64(rr.Firings))
			if c.InhibitedBy != "" {
				inhibited += " by " + Clean(c.InhibitedBy)
			}
		}
		row := []string{Clean(rr.Rule), fmt.Sprint(rr.Firings), fmt.Sprint(c.Notified), fmt.Sprint(c.Paged)}
		if r.Hours != nil {
			row = append(row, fmt.Sprint(c.PagedAsleep), fmt.Sprint(c.PagedOffHours))
		}
		row = append(row, fmt.Sprint(c.PagedForNothing), inhibited, fmt.Sprint(c.Grouped))
		if n.Stopped != "" {
			// What was counted is of part of the period: not a number to read.
			for k := 2; k < len(row); k++ {
				row[k] = "?"
			}
		}
		rows = append(rows, row)
	}
	fmt.Fprintln(w)
	if len(rows) > 0 {
		head := []string{"RULE", "FIRED", "NOTIFIED", "PAGED"}
		if r.Hours != nil {
			head = append(head, "PAGED ASLEEP", "PAGED OFF HOURS")
		}
		head = append(head, "PAGED FOR NOTHING", "INHIBITED", "GROUPED")
		if err := table(w, slices.Insert(rows, 0, head)); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(w, "No firing has a notification to count.")
	}
	fmt.Fprintf(w, "%s\n", NotificationsSaid)
	if n.Stopped != "" {
		fmt.Fprintf(w, "! Pages and notifications %s. The numbers above are marked ? and not to be read: what was worked out before it stopped was %s sent, %d of them pages.\n",
			notKnown(n.Stopped), plural(n.Notified, "notification", "notifications"), n.Paged)
	} else {
		fmt.Fprintf(w, "%s sent for them, %d of them pages (%d on a paging integration), in %s.\n", plural(n.Notified, "notification", "notifications"), n.Paged, n.OnPaging, plural(n.Groups, "group", "groups"))
	}
	fmt.Fprintln(w, "  PAGED counts what rings a phone: on PagerDuty, Opsgenie, VictorOps and incident.io the notification that opens an incident (the repeats and updates of it, on the same integration, update it and aren't pages); on any other paging integration, each.")
	for _, wn := range n.Warnings {
		fmt.Fprintf(w, "! %s\n", Clean(wn))
	}
	if r.Responses != nil {
		fmt.Fprintln(w, "  PAGED FOR NOTHING counts pages of firings that weren't useful (what was done about them); without responses, of firings that caught nothing.")
	} else {
		fmt.Fprintln(w, "  PAGED FOR NOTHING counts pages of firings that caught nothing.")
	}
	fmt.Fprintln(w, "  GROUPED counts firings that went out in a notification with other rules' alerts; a notification of several firings counts for each.")
	var pages []string
	for _, rc := range n.Receivers {
		if rc.Paging {
			pages = append(pages, Clean(rc.Name)+" ("+Clean(rc.Why)+")")
		}
	}
	if len(pages) > 0 {
		fmt.Fprintf(w, "Taken as paging: %s.\n", strings.Join(pages, "; "))
	} else {
		fmt.Fprintln(w, "No receiver pages: none has an integration of a paging kind, and --paging-receivers matched none.")
	}
	var empty []string
	for _, rc := range n.Receivers {
		if len(rc.Kinds) == 0 {
			empty = append(empty, Clean(rc.Name))
		}
	}
	if len(empty) > 0 {
		fmt.Fprintf(w, "Reached, and sends nothing: %s.\n", strings.Join(empty, ", "))
	}
	return nil
}

// NotificationCSVColumns are the columns appended to CSVColumns when the
// report has worked out the notifications: each firing's.
var NotificationCSVColumns = []string{"worked_out_notified", "worked_out_paged", "worked_out_first_page", "worked_out_receivers", "worked_out_muted"}

// notificationCells are a firing's cells of NotificationCSVColumns.
func notificationCells(j Judged) []string {
	n := j.Notifications
	if n == nil {
		return []string{"", "", "", "[]", "[]"}
	}
	first := ""
	if !n.FirstPage.IsZero() {
		first = csvTime(n.FirstPage)
	}
	receivers := []string{}
	for _, r := range n.Receivers {
		receivers = append(receivers, cellText(r))
	}
	muted := []string{}
	for _, m := range n.Muted {
		muted = append(muted, cellText(m.Kind+": "+m.By))
	}
	return []string{fmt.Sprint(n.Notified), fmt.Sprint(n.Paged), first, csvJSON(receivers), csvJSON(muted)}
}

// notificationMetrics are the lines of lapilli_rule_notifications_worked_out
// and lapilli_rule_pages_worked_out for a rule whose labels are l.
func notificationMetrics(l string, rr RuleResult) (notified, paged string) {
	c := rr.Notifications
	if c == nil {
		c = &RuleNotifications{}
	}
	return fmt.Sprintf("lapilli_rule_notifications_worked_out%s %d", l, c.Notified), fmt.Sprintf("lapilli_rule_pages_worked_out%s %d", l, c.Paged)
}

// pagedCells is a rule's pages as a comparison shows them: its pages before →
// after, – where it isn't, ? where it couldn't be replayed.
func pagedCells(c *Comparison, rc RuleChange) string {
	if c.pagesNotKnown() != "" {
		return "? → ?"
	}
	return both(rc, func(r RuleResult) string {
		if r.Notifications == nil {
			return "–"
		}
		return fmt.Sprint(r.Notifications.Paged)
	})
}

// pagesLine is the line a comparison starts its summary with when it worked
// out the notifications, "" when it didn't.
func (c *Comparison) pagesLine() string {
	if !c.Before.hasNotifications() || !c.After.hasNotifications() {
		return ""
	}
	what := "Pages"
	if c.ChangedOnly {
		what = "Pages of the changed rules' firings"
	}
	if stopped := c.pagesNotKnown(); stopped != "" {
		return fmt.Sprintf("%s %s", what, notKnown(stopped))
	}
	b, a := c.Before.Notifications.Paged, c.After.Notifications.Paged
	return fmt.Sprintf("%s %d → %d (worked out from the alert manager's configuration, not recorded)", what, b, a)
}

// SummaryNotifications are, on a comparison's summary, the pages worked out
// before and after the change.
//
// Known is false when the work-out of either side stopped short (Stopped
// says why): the counts are then of part of the period, and not to be read as
// pages.
type SummaryNotifications struct {
	WorkedOut      bool   `json:"worked_out"`
	PagedBefore    int    `json:"paged_before"`
	PagedAfter     int    `json:"paged_after"`
	NotifiedBefore int    `json:"notified_before"`
	NotifiedAfter  int    `json:"notified_after"`
	Known          bool   `json:"known"`
	Stopped        string `json:"stopped,omitempty"`
}

func (c *Comparison) summaryNotifications() *SummaryNotifications {
	if !c.Before.hasNotifications() || !c.After.hasNotifications() {
		return nil
	}
	b, a := c.Before.Notifications, c.After.Notifications
	stopped := c.pagesNotKnown()
	return &SummaryNotifications{WorkedOut: true, Known: stopped == "", Stopped: stopped, PagedBefore: b.Paged, PagedAfter: a.Paged, NotifiedBefore: b.Notified, NotifiedAfter: a.Notified}
}

// pagesOfFiring is how many pages a firing's alert is worked out to have
// sent; 0 when not worked out.
func pagesOfFiring(j Judged) int {
	if j.Notifications == nil {
		return 0
	}
	return j.Notifications.Paged
}
