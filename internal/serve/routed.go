package serve

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"sync"
	"time"

	"lapilli.dev/internal/content"
	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/routing"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
	"lapilli.dev/internal/verdict"
)

// Who each firing reached, worked out (docs/design/routing.md, "In lapilli
// serve"). After each run records its firings, the alert manager's routing
// is run over the firings of the hours the run judged and the hours before
// them that groups and repeats reach back to, from the firings in the
// ledger alone: so judging an hour again works the same out again without
// asking anything. The notifications and what each firing reached are kept
// (schema 035), marked worked out wherever a page shows them.

// MaxRoutedFirings is how many firings one run works the notifications of
// out: the firings of its own hours come first, then those of the lead-in
// (the earliest first). With more than that in the hours themselves the
// hours are not worked out; with more only in the lead-in they are, and the
// caveat is kept with them.
const MaxRoutedFirings = 300000

// A run's work-out is bounded in two ways, and stops with either: it is given
// routedDeadline to take, and routedBudget work units (routing.Options.Budget:
// a notification is 50, a flush is 1 and each alert it holds 1, each alert in a
// notification 1, and a silence, an inhibition or a regular expression
// matched against a label set 1 and more for a long value). 20 million work
// units are some 400,000 notifications, in about 100 MB, many times what a
// busy cluster's hours send. The hours of a run that stopped are recorded as
// not worked out, never as none.
//
// The catch-up, which works out the hours no run did, has routedCatchUpDeadline
// to take in all, across its pieces, so that a configuration that holds the
// model up holds the run loop for that long and no longer; a piece that
// stopped is tried again after an hour, then after two, doubling to a day.
const (
	routedDeadline        = 2 * time.Minute
	routedBudget          = 20_000_000
	routedCatchUpDeadline = 2 * time.Minute
	routedBackoffMax      = 24 * time.Hour
)

// mutePad is how long before the first mute of an hour, and after the last,
// a silence the event recorder told of is taken to be in force: the model's
// flushes and Alertmanager's differ by a second or so, and the recorder tells
// of a mute only as Alertmanager asks.
const mutePad = 10 * time.Second

// routedLeadMin and routedLeadMax bound the lead-in: the hours before a
// run's own whose firings are taken in as well. The lead-in is the longest
// repeat_interval of the routes, so that a group that began before the
// hours judged, and an alert that inhibits one in them, is there to be
// seen; at least an hour, at most a day. A firing that began before it is
// taken as beginning an hour before it (routedClamp, which is more than any
// group_wait there should be), and as carried: its group is taken as
// notified before the lead-in (routing.Options.From is the lead-in's
// start), so it gets no first notification, and its repeats are counted from
// the last notification kept for its group, or, where none is, from the
// lead-in's start. A run keeps the notifications of its own hours only; what
// a firing reached is those of all the runs that saw it.
const (
	routedLeadMin = time.Hour
	routedLeadMax = 24 * time.Hour
	routedClamp   = time.Hour
)

// routedState is the routing configuration serve last read, and what the
// last work-out said.
type routedState struct {
	sync.Mutex
	cfg    *routing.Config
	sum    string
	at     time.Time
	warned string
	// readFailed is why the configuration can't be read now ("" if it can):
	// what was read before stays in use.
	readFailed string
	// stopped is why the last work-out stopped short, "" if it didn't;
	// failed is why it failed.
	stopped, failed string
	last            time.Time
	// silences are those --silences gave, as read last, and silencesWarned
	// what was said of it last.
	silences       []routing.Silence
	silencesWarned string
	runs           int
	// reading serializes the reads of the configuration; tried is when a
	// stretch was last worked out by the catch-up, by its start (Unix
	// seconds), so that one that stopped isn't tried at every round.
	reading sync.Mutex
	tried   map[int64]routedTry
	// limits are the bounds of a work-out, for tests to lower: none is
	// the constants above.
	limits routedLimits
}

// routedLimits are the bounds of a work-out; zero is the constant.
type routedLimits struct {
	deadline time.Duration
	catchUp  time.Duration
	budget   int
	firings  int
	silences int
}

// routedTry is when the catch-up tried a stretch last, and how many times
// in a row it didn't come out.
type routedTry struct {
	at     time.Time
	failed int
}

// wait is how long after a try that failed the next is made: an hour, and
// twice as long for each failure after the first, up to a day.
func (t routedTry) wait() time.Duration {
	d := notifyEvery
	for i := 1; i < t.failed && d < routedBackoffMax; i++ {
		d *= 2
	}
	return min(d, routedBackoffMax)
}

// bounds are the limits in force: how long, how many work units, how many
// firings.
func (s *Server) routedBounds() (time.Duration, int, int) {
	s.routed.Lock()
	defer s.routed.Unlock()
	l := s.routed.limits
	return cmp.Or(l.deadline, routedDeadline), cmp.Or(l.budget, routedBudget), cmp.Or(l.firings, MaxRoutedFirings)
}

// maxSilences is how many silences a work-out takes: the model's bound.
func (s *Server) maxSilences() int {
	s.routed.Lock()
	defer s.routed.Unlock()
	return cmp.Or(s.routed.limits.silences, routing.MaxSilences)
}

// catchUpDeadline is how long the catch-up of a run takes in all.
func (s *Server) catchUpDeadline() time.Duration {
	s.routed.Lock()
	defer s.routed.Unlock()
	return cmp.Or(s.routed.limits.catchUp, routedCatchUpDeadline)
}

// notifies reports whether serve works out the notifications.
func (s *Server) notifies() bool { return s.cfg.Notifications }

// routingKept is the routing configuration as read last, without reading it:
// what a page is drawn with, which asks nothing of the network.
func (s *Server) routingKept() *routing.Config {
	s.routed.Lock()
	defer s.routed.Unlock()
	return s.routed.cfg
}

// routingForView is the routing configuration a page is drawn with: the one
// kept, or, where none is yet (a replica that isn't the leader works nothing
// out, and the leader before its first run has read nothing), the one read
// now, as the runs read it: so every replica shows the same numbers. A read
// that failed isn't tried again before routingTryEvery has passed.
func (s *Server) routingForView(ctx context.Context) *routing.Config {
	if !s.notifies() {
		return nil
	}
	if c := s.routingKept(); c != nil {
		return c
	}
	return s.routingConfig(ctx)
}

// routingSum is the hash of the configuration kept: "" for none.
func (s *Server) routingSum() string {
	s.routed.Lock()
	defer s.routed.Unlock()
	return s.routed.sum
}

// routingTryEvery is how long after a read that failed, with none kept, the
// next is tried.
const routingTryEvery = 5 * time.Minute

// routingConfig is the alert manager's routing configuration, or Grafana's,
// read once an hour by the runs; one that can't be read leaves the one
// before, and the log says why once for each why.
func (s *Server) routingConfig(ctx context.Context) *routing.Config {
	s.routed.reading.Lock()
	defer s.routed.reading.Unlock()
	s.routed.Lock()
	age := s.now().Sub(s.routed.at)
	fresh := s.routed.cfg != nil && age < notifyEvery || s.routed.cfg == nil && !s.routed.at.IsZero() && age < routingTryEvery
	cfg := s.routed.cfg
	s.routed.Unlock()
	if fresh {
		return cfg
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s.routed.Lock()
	have := s.routed.sum
	s.routed.Unlock()
	got, sum, err := s.readRouting(ctx, have)
	s.routed.Lock()
	defer s.routed.Unlock()
	told := ""
	if err != nil {
		told = err.Error()
	}
	warn := told != s.routed.warned
	s.routed.warned, s.routed.readFailed = told, told
	if err != nil {
		if warn {
			s.log.Warn("the notifications aren't worked out: the routing configuration can't be read", "err", err)
		}
		// The one read before, if any, goes on being used; read again in
		// a few minutes.
		s.routed.at = s.now().Add(-notifyEvery + routingTryEvery)
		if s.routed.cfg == nil {
			s.routed.at = s.now()
		}
		return s.routed.cfg
	}
	if got != nil {
		s.routed.cfg = got
	}
	s.routed.sum, s.routed.at = sum, s.now()
	return s.routed.cfg
}

// readRouting reads the routing configuration: Alertmanager's, with
// --alertmanager-config, or else Grafana's policies, contact points and
// mute timings, with --grafana. It is read as text and hashed first, and read
// into a configuration only when the hash isn't have: for the one already
// kept it gives none, and the hash.
func (s *Server) readRouting(ctx context.Context, have string) (*routing.Config, string, error) {
	if s.cfg.AlertmanagerConfig != "" {
		var cfg []byte
		var err error
		if source.AlertmanagerURL(s.cfg.AlertmanagerConfig) {
			var found bool
			cfg, _, found, err = source.AlertmanagerAPI(ctx, s.cfg.AlertmanagerConfig, s.cfg.RulerTenant, source.Creds{TokenFile: s.cfg.RulerToken},
				"lapilli/"+s.cfg.Version, s.cfg.RulerTransport)
			if err == nil && !found {
				err = errors.New("the tenant has no Alertmanager configuration (HTTP 404)")
			}
		} else {
			cfg, _, _, err = source.AlertmanagerFiles(s.cfg.AlertmanagerConfig)
		}
		if err != nil {
			return nil, "", err
		}
		h := sha256.Sum256(cfg)
		sum := hex.EncodeToString(h[:])
		if sum == have {
			return nil, sum, nil
		}
		c, err := content.ReadAlertmanagerRouting(cfg)
		if err != nil {
			return nil, "", err
		}
		return c, sum, nil
	}
	if s.cfg.Grafana == "" {
		return nil, "", errors.New("neither --alertmanager-config nor --grafana is given")
	}
	_, cps, ps, err := grafanaNotifications(ctx, s.cfg.Grafana, s.cfg.GrafanaToken, "lapilli/"+s.cfg.Version)
	if err != nil {
		return nil, "", err
	}
	mute, err := source.GrafanaMuteTimings(ctx, s.cfg.Grafana, s.cfg.GrafanaToken, "lapilli/"+s.cfg.Version)
	if err != nil {
		return nil, "", err
	}
	h := sha256.Sum256(slices.Concat(cps, []byte{0}, ps, []byte{0}, mute))
	sum := hex.EncodeToString(h[:])
	if sum == have {
		return nil, sum, nil
	}
	c, err := content.ReadGrafanaRouting(cps, ps, mute)
	if err != nil {
		return nil, "", err
	}
	return c, sum, nil
}

// routedLead is how far before a run's hours its lead-in goes.
func routedLead(c *routing.Config) time.Duration {
	lead := routedLeadMin
	for _, r := range c.Routes() {
		lead = max(lead, r.RepeatInterval)
	}
	return min(lead, routedLeadMax)
}

// fileSilences are the silences --silences holds, read again each run; the
// last good read is used when the file can't be read, and the log says why
// once.
func (s *Server) fileSilences() []routing.Silence {
	if s.cfg.Silences == "" {
		return nil
	}
	read := func() ([]routing.Silence, error) {
		f, err := os.Open(s.cfg.Silences)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, routing.MaxSilenceBytes+1))
		if err != nil {
			return nil, err
		}
		if len(b) > routing.MaxSilenceBytes {
			return nil, fmt.Errorf("more than %d MB", routing.MaxSilenceBytes>>20)
		}
		return routing.ReadSilences(b)
	}
	got, err := read()
	s.routed.Lock()
	defer s.routed.Unlock()
	told := ""
	if err != nil {
		told = err.Error()
	}
	if told != s.routed.silencesWarned {
		s.routed.silencesWarned = told
		if err != nil {
			s.log.Warn("--silences can't be read: the silences it held are used as they were read last", "err", err)
		}
	}
	if err == nil {
		s.routed.silences = got
	}
	return s.routed.silences
}

// mutedSilences are the silences the event recorder told of that touch
// [from, to], tied to the ledger's firings: a mute of an alert by a silence
// is a silence of the labels of the firing it is of (what the model is given
// of it: its labels and alertname, the name its alert carries) from the first
// time Alertmanager told of it in an hour to the last, which is how long the
// silence is known to have muted it. The recorded alert's labels aren't
// used: they hold the external labels and what a rule's labels were as
// sent, which the firing's don't. A mute no firing is tied to makes none.
// They are in addition to --silences, which says more, silences that muted
// nothing included. alerts are the firings' alerts, in their order. over
// says there were more than most of them: what is given is
// then not all there is.
func (s *Server) mutedSilences(ctx context.Context, from, to time.Time, fs []ledger.Firing, alerts []routing.Alert, most int) (out []routing.Silence, over bool, err error) {
	has, err := s.l.HasMutes(ctx)
	if err != nil || !has {
		return nil, false, err
	}
	ms, err := s.l.Mutes(ctx, from.Add(-mutePad), to.Add(time.Hour))
	if err != nil {
		return nil, false, err
	}
	byRule := map[string][]int{}
	for i, f := range fs {
		byRule[f.Rule] = append(byRule[f.Rule], i)
		if i < len(alerts) {
			if name := alerts[i].Labels["alertname"]; name != f.Rule {
				byRule[name] = append(byRule[name], i)
			}
		}
	}
	within := 2 * max(s.cfg.Step, time.Minute)
	seen := map[string]bool{}
	for _, m := range ms {
		// One that begins after the hours, or ends before them, doesn't
		// touch them.
		if m.Kind != ledger.MutedSilence || m.At.Add(-mutePad).After(to) || m.Last.Add(mutePad).Before(from) {
			continue
		}
		for _, i := range byRule[m.Rule] {
			f := fs[i]
			end := f.End
			if end.IsZero() {
				end = to
			}
			if f.Start.After(m.Last.Add(within)) || end.Before(m.At.Add(-within)) || !labelsAmong(f.Labels, m.Labels) {
				continue
			}
			sil := routing.Silence{ID: m.Ref, Start: m.At.Add(-mutePad), End: m.Last.Add(mutePad)}
			if sil.ID == "" {
				sil.ID = "recorded"
			}
			labels := map[string]string{}
			if i < len(alerts) {
				maps.Copy(labels, alerts[i].Labels)
			} else {
				labels["alertname"] = f.Rule
				maps.Copy(labels, f.Labels)
			}
			key := fmt.Sprint(sil.ID, sil.Start.Unix(), sil.End.Unix(), labels)
			if seen[key] {
				continue
			}
			seen[key] = true
			for _, k := range slices.Sorted(maps.Keys(labels)) {
				mt, err := routing.NewMatcher(k, "=", labels[k])
				if err != nil {
					continue
				}
				sil.Matchers = append(sil.Matchers, mt)
			}
			if len(out) >= most {
				return out, true, nil
			}
			out = append(out, sil)
		}
	}
	return out, false, nil
}

// labelsAmong reports whether every label of a is in b with its value.
func labelsAmong(a, b map[string]string) bool {
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// ruleList are the rules of the latest version, for the alerts' names,
// folders and own contact points; none with --alerts or before any is read.
func (s *Server) routedRules(ctx context.Context) []rules.Rule {
	latest, ok, err := s.l.Latest(ctx)
	if err != nil || !ok {
		return nil
	}
	rs, err := s.parse(ctx, latest)
	if err != nil {
		return nil
	}
	return rs
}

// workOutRouted works out the notifications of the hours [from, to] and
// keeps them. It is called after a run has recorded its firings, and works
// from the ledger alone: what a run judged again is worked out again the
// same way. A failure is logged and counted, and never fails the run; the
// hours are then recorded as not worked out (their pages are not known, not
// none), and the catch-up tries them again.
func (s *Server) workOutRouted(ctx context.Context, from, to time.Time) {
	s.workOutRoutedIn(ctx, ctx, from, to)
}

// workOutRoutedIn is workOutRouted whose model run ends with run (the
// catch-up's time) as well as with its own deadline; ctx is for the ledger.
func (s *Server) workOutRoutedIn(ctx, run context.Context, from, to time.Time) {
	if !s.notifies() {
		// The firings of these hours may have been recorded again: what was
		// worked out of them isn't theirs now.
		if c, err := s.l.RoutedCover(ctx); err == nil && c.Any() {
			if err := s.l.MarkRoutedUnknown(ctx, from, to, "--notifications was off while these hours were recorded"); err != nil {
				s.log.Warn("the hours recorded with --notifications off weren't marked not worked out", "err", err)
			}
		}
		return
	}
	cfg := s.routingConfig(ctx)
	if cfg == nil {
		s.routed.Lock()
		why := "the routing configuration can't be read"
		if s.routed.readFailed != "" {
			why += ": " + s.routed.readFailed
		}
		s.routed.Unlock()
		if err := s.l.MarkRoutedUnknown(ctx, from, to, why); err != nil {
			s.log.Warn("the hours judged weren't marked not worked out", "from", from, "to", to, "err", err)
		}
		return
	}
	stopped, kept, err := s.routeHours(ctx, run, cfg, from, to)
	if err != nil {
		stopped = "failed: " + err.Error()
		s.log.Warn("the notifications of the hours judged weren't worked out", "from", from, "to", to, "err", err)
	}
	if err != nil || stopped != "" && !kept {
		if merr := s.l.MarkRoutedUnknown(ctx, from, to, stopped); merr != nil {
			s.log.Warn("the hours judged weren't marked not worked out", "from", from, "to", to, "err", merr)
		}
	}
	s.routed.Lock()
	defer s.routed.Unlock()
	s.routed.last = s.now()
	s.routed.runs++
	s.routed.stopped, s.routed.failed = "", ""
	if err != nil {
		s.routed.failed = err.Error()
	} else {
		s.routed.stopped = stopped
	}
}

// routeHours is the work of workOutRouted, for a configuration. It gives
// why the hours weren't all worked out ("" when they were), or, with kept
// set, a caveat on hours worked out and kept; an error is a failure.
func (s *Server) routeHours(ctx, run context.Context, cfg *routing.Config, from, to time.Time) (string, bool, error) {
	lead := routedLead(cfg)
	begin := from.Add(-lead)
	deadline, budget, most := s.routedBounds()
	fs, more, err := s.l.FiringsOverlapping(ctx, begin, to, from, most)
	if err != nil {
		return "", false, err
	}
	caveat := ""
	if more {
		// Those of the run's own hours come first: the last one read says
		// whether the lead-in was reached.
		if last := fs[len(fs)-1]; last.End.IsZero() || !last.End.Before(from) {
			return fmt.Sprintf("more than %d firings in the hours worked out: they are not worked out", most), false, nil
		}
		caveat = fmt.Sprintf("more than %d firings in the hours and the %s before them: of those before them only the first are taken in, so what began before the hours may be missed", most, lead)
	}
	judged := make([]verdict.Judged, len(fs))
	for i, f := range fs {
		// A firing is taken as beginning no earlier than an hour before the
		// lead-in.
		judged[i] = verdict.Judged{Firing: firing.Firing{Rule: f.Rule, Labels: f.Labels, Start: maxTime(f.Start, begin.Add(-routedClamp)), End: f.End}}
	}
	alerts := verdict.NotificationAlerts(judged, s.routedRules(ctx))
	for i, f := range fs {
		// One that began before the lead-in was notified before it.
		alerts[i].Carried = f.Start.Before(begin)
	}
	sils := s.fileSilences()
	silMax := s.maxSilences()
	recorded, over, err := s.mutedSilences(ctx, begin, to, fs, alerts, silMax)
	if err != nil {
		return "", false, err
	}
	if over || len(sils)+len(recorded) > silMax {
		return fmt.Sprintf("more than %d silences to take into account: the %d of --silences and the %d the recorder told of that touch the hours: not worked out, as they would be worked out as if there were none", silMax, len(sils), len(recorded)), false, nil
	}
	sils = slices.Concat(sils, recorded)
	// What the groups were last notified at, before the lead-in: repeats
	// are counted from there.
	last, err := s.l.RoutedNotifiedBefore(ctx, begin.Add(-routedRepeatMax(cfg)), begin)
	if err != nil {
		return "", false, err
	}
	notified := make([]routing.NotifiedAt, len(last))
	for i, x := range last {
		notified[i] = routing.NotifiedAt{Group: x.Group, Receiver: x.Receiver, Kind: x.Kind, At: x.At}
	}
	rctx, cancel := context.WithTimeout(run, deadline)
	defer cancel()
	res, err := cfg.RunContext(rctx, alerts, routing.Options{ExternalLabels: s.cfg.ExternalLabels, Silences: sils, PagingReceivers: s.cfg.PagingReceivers,
		From: begin, Notified: notified, Until: to, Budget: budget})
	if err != nil {
		return "", false, err
	}
	if res.Stopped != "" {
		return res.Stopped, false, nil
	}
	var ns []ledger.RoutedNotification
	for _, nt := range res.Notifications {
		if nt.At.Before(from) {
			continue
		}
		ns = append(ns, ledger.RoutedNotification{At: nt.At, Receiver: nt.Receiver, Kind: nt.Kind, Page: nt.Page, Reason: string(nt.Reason),
			Group: nt.Group, Firings: len(nt.Firing)})
	}
	// A notification that told of firings of more than one rule.
	several := make([]bool, len(res.Notifications))
	for k, nt := range res.Notifications {
		for _, i := range nt.Firing[min(1, len(nt.Firing)):] {
			if fs[i].Rule != fs[nt.Firing[0]].Rule {
				several[k] = true
				break
			}
		}
	}
	ruleOf := func(i int) string {
		if i < 0 || i >= len(fs) {
			return ""
		}
		return fs[i].Rule
	}
	var kept []ledger.RoutedFiring
	for i, f := range fs {
		if !f.End.IsZero() && f.End.Before(from) {
			continue
		}
		x := routedOf(f.ID, res.Alerts[i], res.Notifications, from, ruleOf)
		x.Grouped = slices.ContainsFunc(res.Alerts[i].Notifications, func(k int) bool { return several[k] })
		x.Whole = !f.Start.Before(from)
		kept = append(kept, x)
	}
	if err := s.l.KeepRouted(ctx, from, to, caveat, ns, kept); err != nil {
		return "", false, err
	}
	return caveat, true, nil
}

// routedRepeatMax is the longest a group goes without a notification while
// it fires, by the routes (a repeat_interval and the group_interval the next
// flush is late by): how far before the lead-in a group's last notification
// is looked for, as one before that would have been followed by a repeat.
func routedRepeatMax(c *routing.Config) time.Duration {
	d := routedLeadMin
	for _, r := range c.Routes() {
		d = max(d, r.RepeatInterval+r.GroupInterval)
	}
	return d
}

// routedOf is what a firing's alert reached in the hours from from on, as the
// ledger is given it: the notifications that told of it (those before from
// were kept by the runs of those hours), what muted it and the receivers that
// send nothing; ruleOf gives the rule of the alert at a position, for the one
// that inhibited it.
func routedOf(id int64, a routing.AlertResult, all []routing.Notification, from time.Time, ruleOf func(int) string) ledger.RoutedFiring {
	out := ledger.RoutedFiring{FiringID: id}
	for _, k := range a.Notifications {
		nt := all[k]
		if nt.At.Before(from) {
			continue
		}
		out.Sent = append(out.Sent, ledger.RoutedSent{At: nt.At, Receiver: nt.Receiver, Kind: nt.Kind, Page: nt.Page})
	}
	for _, name := range a.Empty {
		out.Receivers = append(out.Receivers, ledger.RoutedReceiver{Receiver: name, Empty: true})
	}
	for _, m := range a.Muted {
		mute := ledger.RoutedMute{Kind: string(m.Kind), By: m.By}
		if m.Kind == routing.MutedInhibition {
			mute.SourceRule = ruleOf(m.Source)
		}
		out.Muted = append(out.Muted, mute)
	}
	return out
}

// firingsUpTo is where the ledger's firings are up to: the runs' cursor, or,
// with no runs, now (what was copied in is as far as it goes). Zero when
// runs have none yet.
func (s *Server) firingsUpTo(ctx context.Context) (time.Time, error) {
	if s.running(ctx) {
		return s.l.Cursor(ctx)
	}
	return s.now(), nil
}

// routedCatchUpDays is how many days of gaps one round of the catch-up
// works out at most.
const routedCatchUpDays = 14

// catchUpRouted works out the notifications of the hours the ledger has
// firings for that no work-out covers, back as far as --backfill, in pieces
// of a day, up to where the ledger's firings are: for a ledger that began to
// work them out after it had firings, one that was told to again
// (--notifications was turned on), and hours whose work-out failed or
// stopped, or whose routing configuration couldn't be read. A piece that
// didn't come out is tried again after an hour, not at each round.
func (s *Server) catchUpRouted(ctx context.Context, now time.Time) {
	if !s.notifies() || s.routingConfig(ctx) == nil {
		return
	}
	upTo, err := s.firingsUpTo(ctx)
	if err != nil || upTo.IsZero() {
		return
	}
	upTo = minTime(upTo, now)
	from := upTo.Add(-s.cfg.Backfill)
	if cap := upTo.Add(-weeks * 7 * 24 * time.Hour); from.Before(cap) {
		from = cap
	}
	first, _, err := s.l.Bounds(ctx)
	if err != nil || first.IsZero() {
		return
	}
	from = maxTime(from, first.Truncate(24*time.Hour))
	cover, err := s.l.RoutedCover(ctx)
	if err != nil {
		return
	}
	// The pieces share the time the catch-up of a run has.
	cctx, cancel := context.WithTimeout(ctx, s.catchUpDeadline())
	defer cancel()
	days := 0
	for _, gap := range cover.Gaps(from, upTo) {
		for at := maxTime(gap[0], gap[0].Truncate(24*time.Hour)); at.Before(gap[1]) && cctx.Err() == nil && ctx.Err() == nil; {
			// Pieces are whole days, or what is left of the gap.
			end := minTime(at.Truncate(24*time.Hour).Add(24*time.Hour), gap[1])
			if days >= routedCatchUpDays {
				return
			}
			s.routed.Lock()
			try, ok := s.routed.tried[at.Unix()]
			if !ok || s.now().Sub(try.at) >= try.wait() {
				if s.routed.tried == nil {
					s.routed.tried = map[int64]routedTry{}
				}
				s.routed.tried[at.Unix()] = routedTry{at: s.now(), failed: try.failed}
				s.routed.Unlock()
				s.workOutRoutedIn(ctx, cctx, at, end)
				days++
				// One that didn't come out waits longer each time.
				c, err := s.l.RoutedCover(ctx)
				s.routed.Lock()
				if err == nil && c.Covers(at, end) {
					delete(s.routed.tried, at.Unix())
				} else {
					s.routed.tried[at.Unix()] = routedTry{at: s.now(), failed: try.failed + 1}
				}
				s.routed.Unlock()
			} else {
				s.routed.Unlock()
			}
			at = end
		}
	}
}
