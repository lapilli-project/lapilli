package serve

import (
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
// out: past them the run keeps the first of them and says it stopped.
const MaxRoutedFirings = 300000

// mutePad is how long before the first mute of an hour, and after the last,
// a silence the event recorder told of is taken to be in force: the model's
// flushes and Alertmanager's differ by a second or so, and the recorder tells
// of a mute only as Alertmanager asks.
const mutePad = 10 * time.Second

// routedLeadMin and routedLeadMax bound the lead-in: the hours before a
// run's own whose firings are taken in as well. The lead-in is the longest
// repeat_interval of the routes, so that a group that began before the
// hours judged, and an alert that inhibits one in them, is there to be
// seen; at least an hour, at most a day. A firing that began before it and
// is still on is taken in whole, from its start: the model starts at an
// alert's own start, so its repeats are counted in full.
const (
	routedLeadMin = time.Hour
	routedLeadMax = 24 * time.Hour
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

// routingTryEvery is how long after a read that failed, with none kept, the
// next is tried.
const routingTryEvery = 5 * time.Minute

// routingConfig is the alert manager's routing configuration, or Grafana's,
// read once an hour by the runs; one that can't be read leaves the one
// before, and the log says why once for each why.
func (s *Server) routingConfig(ctx context.Context) *routing.Config {
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
	got, sum, err := s.readRouting(ctx)
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
	s.routed.cfg, s.routed.sum, s.routed.at = got, sum, s.now()
	return got
}

// readRouting reads the routing configuration: Alertmanager's, with
// --alertmanager-config, or else Grafana's policies, contact points and
// mute timings, with --grafana.
func (s *Server) readRouting(ctx context.Context) (*routing.Config, string, error) {
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
		c, err := content.ReadAlertmanagerRouting(cfg)
		if err != nil {
			return nil, "", err
		}
		h := sha256.Sum256(cfg)
		return c, hex.EncodeToString(h[:]), nil
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
	c, err := content.ReadGrafanaRouting(cps, ps, mute)
	if err != nil {
		return nil, "", err
	}
	h := sha256.Sum256(slices.Concat(cps, []byte{0}, ps, []byte{0}, mute))
	return c, hex.EncodeToString(h[:]), nil
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

// mutedSilences are the silences the event recorder told of in [from, to):
// a mute of an alert by a silence is a silence of that alert's labels from
// the first time Alertmanager told of it in an hour to the last, which is
// how long the silence is known to have muted it. They are in addition to
// --silences, which says more, silences that muted nothing included.
func (s *Server) mutedSilences(ctx context.Context, from, to time.Time) ([]routing.Silence, error) {
	has, err := s.l.HasMutes(ctx)
	if err != nil || !has {
		return nil, err
	}
	ms, err := s.l.Mutes(ctx, from, to.Add(time.Hour))
	if err != nil {
		return nil, err
	}
	var out []routing.Silence
	for _, m := range ms {
		if m.Kind != ledger.MutedSilence {
			continue
		}
		labels := map[string]string{"alertname": m.Rule}
		for k, v := range m.Labels {
			labels[k] = v
		}
		// A few seconds either side: the model's flush comes a moment before
		// Alertmanager's own, whose first mute the recorder told of.
		sil := routing.Silence{ID: m.Ref, Start: m.At.Add(-mutePad), End: m.Last.Add(mutePad)}
		if sil.ID == "" {
			sil.ID = "recorded"
		}
		for _, k := range slices.Sorted(maps.Keys(labels)) {
			mt, err := routing.NewMatcher(k, "=", labels[k])
			if err != nil {
				continue
			}
			sil.Matchers = append(sil.Matchers, mt)
		}
		out = append(out, sil)
		if len(out) >= routing.MaxSilences {
			break
		}
	}
	return out, nil
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
// same way. A failure is logged and counted, and never fails the run.
func (s *Server) workOutRouted(ctx context.Context, from, to time.Time) {
	if !s.notifies() {
		return
	}
	cfg := s.routingConfig(ctx)
	if cfg == nil {
		return
	}
	err := s.routeHours(ctx, cfg, from, to)
	s.routed.Lock()
	defer s.routed.Unlock()
	s.routed.last = s.now()
	s.routed.runs++
	if err != nil {
		s.routed.failed = err.Error()
		s.log.Warn("the notifications of the hours judged weren't worked out", "from", from, "to", to, "err", err)
		return
	}
	s.routed.failed = ""
}

// routeHours is the work of workOutRouted, for a configuration.
func (s *Server) routeHours(ctx context.Context, cfg *routing.Config, from, to time.Time) error {
	lead := routedLead(cfg)
	fs, more, err := s.l.FiringsOverlapping(ctx, from.Add(-lead), to, MaxRoutedFirings)
	if err != nil {
		return err
	}
	stopped := ""
	if more {
		stopped = fmt.Sprintf("more than %d firings in the hours worked out: the notifications of the first %d are", MaxRoutedFirings, MaxRoutedFirings)
	}
	judged := make([]verdict.Judged, len(fs))
	for i, f := range fs {
		judged[i] = verdict.Judged{Firing: firing.Firing{Rule: f.Rule, Labels: f.Labels, Start: f.Start, End: f.End}}
	}
	alerts := verdict.NotificationAlerts(judged, s.routedRules(ctx))
	sils := s.fileSilences()
	if recorded, err := s.mutedSilences(ctx, from.Add(-lead), to); err != nil {
		return err
	} else {
		sils = slices.Concat(sils, recorded)
	}
	res, err := cfg.Run(alerts, routing.Options{ExternalLabels: s.cfg.ExternalLabels, Silences: sils, PagingReceivers: s.cfg.PagingReceivers, Until: to})
	if err != nil {
		return err
	}
	if res.Stopped != "" {
		stopped = res.Stopped
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
	var kept []ledger.RoutedFiring
	for i, f := range fs {
		if !f.End.IsZero() && f.End.Before(from) {
			continue
		}
		x := routedOf(f.ID, res.Alerts[i], res.Notifications)
		x.Grouped = slices.ContainsFunc(res.Alerts[i].Notifications, func(k int) bool { return several[k] })
		kept = append(kept, x)
	}
	if err := s.l.KeepRouted(ctx, from, to, ns, kept); err != nil {
		return err
	}
	s.routed.Lock()
	s.routed.stopped = stopped
	s.routed.Unlock()
	return nil
}

// routedOf is what a firing's alert reached, as the ledger keeps it.
func routedOf(id int64, a routing.AlertResult, all []routing.Notification) ledger.RoutedFiring {
	out := ledger.RoutedFiring{FiringID: id, Notified: a.Notified, Paged: a.Paged, FirstPage: a.FirstPage}
	type key struct{ receiver, kind string }
	idx := map[key]int{}
	for _, k := range a.Notifications {
		nt := all[k]
		x := key{nt.Receiver, nt.Kind}
		i, ok := idx[x]
		if !ok {
			i = len(out.Receivers)
			idx[x] = i
			out.Receivers = append(out.Receivers, ledger.RoutedReceiver{Receiver: nt.Receiver, Kind: nt.Kind})
		}
		out.Receivers[i].Notified++
		if nt.Page {
			out.Receivers[i].Paged++
			out.PageAts = append(out.PageAts, ledger.RoutedPageAt{At: nt.At, Receiver: nt.Receiver})
		}
	}
	for _, name := range a.Empty {
		out.Receivers = append(out.Receivers, ledger.RoutedReceiver{Receiver: name, Empty: true})
	}
	for _, m := range a.Muted {
		mute := ledger.RoutedMute{Kind: string(m.Kind), By: m.By}
		out.Muted = append(out.Muted, mute)
	}
	return out
}

// catchUpRouted works out the notifications of the days the ledger has
// firings for, back as far as --backfill, in pieces of a day: for a ledger
// that began to work them out after it had firings, and one that was told
// to again (--notifications was turned on). It does nothing once a span is
// worked out.
func (s *Server) catchUpRouted(ctx context.Context, to time.Time) {
	if !s.notifies() || s.routingConfig(ctx) == nil {
		return
	}
	if _, _, ok, err := s.l.RoutedSpan(ctx); err != nil || ok {
		return
	}
	from := to.Add(-s.cfg.Backfill)
	if cap := to.Add(-weeks * 7 * 24 * time.Hour); from.Before(cap) {
		from = cap
	}
	if fs, _, err := s.l.FiringsOverlapping(ctx, from, to, 1); err != nil || len(fs) == 0 {
		return // no firing to work the notifications of out
	}
	for at := from.Truncate(24 * time.Hour); at.Before(to) && ctx.Err() == nil; at = at.Add(24 * time.Hour) {
		s.workOutRouted(ctx, at, minTime(at.Add(24*time.Hour), to))
		if _, _, ok, _ := s.l.RoutedSpan(ctx); !ok {
			return // nothing could be worked out
		}
	}
}
