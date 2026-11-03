package serve

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/report"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
	"lapilli.dev/internal/verdict"
)

// work does the ledger's jobs one at a time, so that Prometheus is asked
// for one thing at a time: runs, reviews of changes to the rules, the
// weekly digest, judging again after the settings change, pruning.
func (s *Server) work(ctx context.Context) {
	tick := time.NewTicker(s.cfg.Every)
	defer tick.Stop()
	next := true // a run is due
	for {
		s.jobs(ctx, next)
		next = false
		s.ready.Store(true)
		s.status.set("")
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			next = true
		case <-s.wake:
		}
	}
}

func (s *Server) jobs(ctx context.Context, run bool) {
	o, err := s.options(ctx)
	if err != nil {
		s.fail("settings", err)
		return
	}
	if s.rejudge.Swap(false) {
		s.status.set("judging every firing again with the new settings")
		err := s.l.JudgeAll(ctx, o, func(done float64) {
			s.status.Lock()
			s.status.judged = done
			s.status.Unlock()
		})
		s.status.Lock()
		s.status.judged = 0
		s.status.Unlock()
		if err != nil {
			s.rejudge.Store(true)
			s.fail("judging again", err)
		}
	}
	if s.cfg.Incidents != "" {
		if err := s.syncIncidents(ctx, o); err != nil {
			s.fail("incidents", err)
		}
	}
	if run {
		s.runs(ctx, o)
	}
	for {
		s.review.Lock()
		if len(s.review.ids) == 0 {
			s.review.Unlock()
			break
		}
		id := s.review.ids[0]
		s.review.ids = s.review.ids[1:]
		s.review.Unlock()
		if err := s.reviewChange(ctx, id, o); err != nil {
			s.fail("reviewing a change to the rules", err)
		}
	}
	if err := s.digest(ctx, o); err != nil {
		s.fail("the weekly digest", err)
	}
	if err := s.prune(ctx); err != nil {
		s.fail("pruning", err)
	}
}

func (s *Server) fail(what string, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	s.log.Error(what, "err", err)
	s.status.Lock()
	s.status.err = what + ": " + err.Error()
	s.status.Unlock()
}

func (s *Server) syncIncidents(ctx context.Context, o verdict.Options) error {
	incs, err := incident.Load(s.cfg.Incidents)
	if err != nil {
		return err
	}
	changed, err := s.l.SyncFile(ctx, incs, o)
	if changed {
		s.log.Info("incidents file read again", "incidents", len(incs))
	}
	return err
}

// runs replays from where the last run got to, less the hours to settle,
// up to now: a day at a time while catching up, and in smaller pieces when
// a day doesn't finish within the timeout.
func (s *Server) runs(ctx context.Context, o verdict.Options) {
	cursor, err := s.l.Cursor(ctx)
	if err != nil {
		s.fail("the ledger", err)
		return
	}
	now := s.now().Truncate(s.cfg.Step)
	settle := s.cfg.Settle
	if cursor.IsZero() {
		cursor, settle = now.Add(-s.cfg.Backfill), 0
	}
	piece := 24 * time.Hour
	for cursor.Before(now) && ctx.Err() == nil {
		from := cursor.Add(-settle).Truncate(s.cfg.Step)
		to := cursor.Add(piece)
		if to.After(now) {
			to = now
		}
		kind := "hourly"
		if to.Before(now) || settle == 0 {
			kind = "backfill"
		}
		s.status.set(fmt.Sprintf("replaying %s → %s", from.In(s.cfg.Loc).Format("2006-01-02 15:04"), to.In(s.cfg.Loc).Format("2006-01-02 15:04")))
		began := s.now()
		found, unfinished, err := s.find(ctx, from, to)
		switch {
		case err != nil:
			s.count(kind, "failed")
			s.fail("a run", err)
			s.l.AddRun(context.WithoutCancel(ctx), ledger.Run{Kind: kind, Started: began, Finished: s.now(), From: from, To: to, Error: err.Error()})
			return
		case unfinished && piece > s.cfg.Every:
			// Too much at once for the time given: half as much.
			piece /= 2
			s.log.Warn("a run didn't finish within --timeout; replaying less at once", "hours", piece.Hours())
			continue
		}
		found.Run = ledger.Run{Kind: kind, Started: began, Finished: s.now(), From: from, To: to, Failed: found.Run.Failed}
		if err := s.l.Record(ctx, found, o); err != nil {
			s.count(kind, "failed")
			s.fail("recording a run", err)
			return
		}
		s.count(kind, "ok")
		s.status.Lock()
		s.status.took = s.now().Sub(began)
		s.status.err = ""
		s.status.Unlock()
		s.log.Info("run", "kind", kind, "from", from, "to", to, "firings", len(found.Firings), "failed", len(found.Run.Failed), "took", s.now().Sub(began).Round(time.Millisecond))
		cursor, settle = to, s.cfg.Settle
		if piece < 24*time.Hour {
			piece *= 2
		}
	}
}

func (s *Server) count(kind, result string) {
	s.status.Lock()
	s.status.runs[kind+" "+result]++
	s.status.Unlock()
}

// margins are how far before its period a rule's replay starts, so that an
// alert pending then is seen: its "for", keep_firing_for and group
// interval, rounded up to one of these, so that rules alike share their
// requests.
var margins = []time.Duration{15 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour, 7 * 24 * time.Hour}

func margin(r rules.Rule, step time.Duration) time.Duration {
	need := r.For + r.KeepFiringFor + r.Interval + step
	for _, m := range margins {
		if need <= m {
			return m
		}
	}
	return need
}

// find replays [from, to] and asks for the impact windows in it. It reports
// whether a rule's replay ran out of time.
func (s *Server) find(ctx context.Context, from, to time.Time) (ledger.Found, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	f := ledger.Found{From: from, To: to, Since: map[string]time.Time{}, Keep: map[string]bool{}}
	failed := map[string]string{}
	unfinished := false
	if s.cfg.Alerts {
		since := from.Add(-time.Hour)
		series, err := s.h.QueryRange(ctx, `ALERTS{alertstate="firing"}`, since, to, s.cfg.Step)
		if err != nil {
			return f, false, fmt.Errorf("reading ALERTS: %w", err)
		}
		for _, x := range firing.FromAlerts(series, s.cfg.Step, to) {
			if s.cfg.RuleRE != nil && !s.cfg.RuleRE.MatchString(x.Rule) {
				continue
			}
			f.Firings = append(f.Firings, x)
			f.Since[x.Rule] = since
		}
		var names []string
		for _, x := range f.Firings {
			if !slices.Contains(names, x.Rule) {
				names = append(names, x.Rule)
				f.Rules = append(f.Rules, ledger.Rule{Name: x.Rule, Severity: x.Labels["severity"]})
			}
		}
	} else {
		// The rules in effect over the period, by how far back they need.
		versions, err := s.versions(ctx, from, to)
		if err != nil {
			return f, false, err
		}
		tiers := map[time.Duration]map[string]bool{}
		for _, v := range versions {
			for _, r := range v.Rules {
				m := margin(r, s.cfg.Step)
				// A rule whose margin changed is replayed with the longest.
				for have, names := range tiers {
					if names[r.Name] && have > m {
						m = have
					}
					if names[r.Name] && have < m {
						delete(names, r.Name)
					}
				}
				if tiers[m] == nil {
					tiers[m] = map[string]bool{}
				}
				tiers[m][r.Name] = true
			}
		}
		meta := map[string]*ledger.Rule{}
		var order []string
		for _, m := range slices.Sorted(maps.Keys(tiers)) {
			since := from.Add(-m).Truncate(s.cfg.Step)
			vs, err := s.versions(ctx, since, to)
			if err != nil {
				return f, false, err
			}
			var set []replay.Version
			for _, v := range vs {
				rv := replay.Version{From: v.Since}
				if !v.Since.After(since) {
					rv.From = time.Time{}
				}
				for _, r := range v.Rules {
					if tiers[m][r.Name] {
						rv.Rules = append(rv.Rules, r)
					}
				}
				set = append(set, rv)
			}
			ran, fs := replay.Run(ctx, s.h, set, since, to, s.cfg.Step, s.cfg.Parallel)
			for _, r := range ran {
				f.Since[r.Name] = since
				x := meta[r.Name]
				if x == nil {
					x = &ledger.Rule{Name: r.Name, Group: r.Group}
					meta[r.Name] = x
					order = append(order, r.Name)
				}
				x.Severity = ledger.Severities([]string{x.Severity, r.Labels["severity"]})
				if r.Unsupported != "" && x.Unsupported == "" {
					x.Unsupported = r.Unsupported
				}
				switch {
				case r.Unsupported == replay.Unfinished:
					unfinished = true
					f.Keep[r.Name] = true
					failed[r.Name] = r.Unsupported
				case strings.HasPrefix(r.Unsupported, "query failed"):
					f.Keep[r.Name] = true
					failed[r.Name] = r.Unsupported
				}
			}
			f.Firings = append(f.Firings, fs...)
		}
		for _, name := range order {
			f.Rules = append(f.Rules, *meta[name])
		}
	}
	if s.cfg.Impact != "" {
		f.ImpactOn, f.ImpactSince = true, from.Add(-time.Hour)
		series, err := s.h.QueryRange(ctx, s.cfg.Impact, f.ImpactSince, to, s.cfg.Step)
		if err != nil {
			return f, false, fmt.Errorf("--impact: %w", err)
		}
		f.Impact = incident.FromImpact(series, s.cfg.Step, s.cfg.ImpactFor)
	}
	f.Run.Failed = failed
	return f, unfinished, nil
}

// version is a version of the rules, read again from its documents.
type version struct {
	ledger.Version
	Rules []rules.Rule
}

// parsed keeps the versions read lately: a version doesn't change.
var parsed = struct {
	sync.Mutex
	m map[string][]rules.Rule // by the ledger's path and the version's id
}{m: map[string][]rules.Rule{}}

// versions are the versions of the rules in effect over [from, to], each
// rule under the name it is judged under.
func (s *Server) versions(ctx context.Context, from, to time.Time) ([]version, error) {
	vs, err := s.l.Versions(ctx, from, to, false)
	if err != nil {
		return nil, err
	}
	out := make([]version, 0, len(vs))
	for _, v := range vs {
		k := fmt.Sprint(s.cfg.Ledger, v.ID)
		parsed.Lock()
		rs, ok := parsed.m[k]
		parsed.Unlock()
		if !ok {
			full, err := s.l.VersionByID(ctx, v.ID)
			if err != nil {
				return nil, err
			}
			all, err := rules.FromDocs(full.Docs)
			if err != nil {
				return nil, err
			}
			if len(all) != len(v.Names) {
				return nil, fmt.Errorf("version %d of the rules: %d rules, %d names", v.ID, len(all), len(v.Names))
			}
			for j, r := range all {
				if v.Names[j] != "" {
					r.Name = v.Names[j]
					rs = append(rs, r)
				}
			}
			parsed.Lock()
			if len(parsed.m) > 64 {
				clear(parsed.m)
			}
			parsed.m[k] = rs
			parsed.Unlock()
		}
		out = append(out, version{v, rs})
	}
	return out, nil
}

// watch reads the rules every --watch and records a new version when
// anything that decides when they fire has changed.
func (s *Server) watch(ctx context.Context) {
	var raw string // the documents last read, to skip parsing them again
	for {
		if err := s.look(ctx, &raw); err != nil {
			s.fail("reading the rules", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.cfg.Watch):
		}
	}
}

func (s *Server) look(ctx context.Context, raw *string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	rs, docs, err := s.readRules(ctx)
	if err != nil {
		return err
	}
	h := sha256.New()
	for _, d := range docs {
		fmt.Fprintf(h, "%s\x00%v\x00%d\x00", d.Source, d.API, len(d.Data))
		h.Write(d.Data)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum == *raw {
		return nil
	} else {
		*raw = sum
	}
	var keep []rules.Rule
	names := make([]string, len(rs))
	var at []int
	for i, r := range rs {
		if s.cfg.RuleRE == nil || s.cfg.RuleRE.MatchString(r.Name) {
			keep = append(keep, r)
			at = append(at, i)
		}
	}
	if len(keep) == 0 {
		return errors.New("no alert rules to replay")
	}
	digest := digestOf(keep)
	latest, ok, err := s.l.Latest(ctx)
	if err != nil {
		return err
	}
	if ok && latest.Digest == digest {
		return nil
	}
	// Named apart alongside the version before, so a rule keeps its name.
	var was []rules.Rule
	if ok {
		vs, err := s.versions(ctx, s.now(), s.now())
		if err != nil {
			return err
		}
		if len(vs) > 0 {
			was = slices.Clone(vs[len(vs)-1].Rules)
		}
	}
	source.TellApart(keep, was)
	for j, i := range at {
		names[i] = keep[j].Name
	}
	since := s.now().Truncate(time.Second)
	if !ok {
		since = time.Time{} // the rules as they stood before the ledger began
	}
	id, err := s.l.AddVersion(ctx, ledger.Version{Since: since, Digest: digest, Docs: docs, Names: names})
	if err != nil {
		return err
	}
	s.log.Info("rules changed: a new version", "version", id, "rules", len(keep))
	if ok {
		s.review.Lock()
		s.review.ids = append(s.review.ids, id)
		s.review.Unlock()
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
	return nil
}

// digestOf is the same for two sets of rules that fire alike.
func digestOf(rs []rules.Rule) string {
	keys := make([]string, len(rs))
	for i, r := range rs {
		keys[i] = r.Key()
	}
	slices.Sort(keys)
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return hex.EncodeToString(sum[:])
}

func (s *Server) readRules(ctx context.Context) ([]rules.Rule, []rules.Doc, error) {
	agent := "lapilli/" + s.cfg.Version
	switch {
	case s.cfg.Loaded:
		rs, d, err := source.Loaded(ctx, s.h)
		return rs, []rules.Doc{d}, err
	case s.cfg.Grafana != "":
		rs, d, err := source.Grafana(ctx, s.cfg.Grafana, s.cfg.GrafanaToken, agent, nil)
		return rs, []rules.Doc{d}, err
	default:
		return rules.Read(s.cfg.RulesPath)
	}
}

// reviewChange replays the rules a new version changed, as they were and as
// they are, over the week before it, and keeps the comparison.
func (s *Server) reviewChange(ctx context.Context, id int64, o verdict.Options) error {
	vs, err := s.l.Versions(ctx, time.Unix(0, 0), s.now(), false)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(vs, func(v ledger.Version) bool { return v.ID == id })
	if i < 1 {
		return nil
	}
	at := vs[i].Since
	pair, err := s.versions(ctx, at.Add(-time.Second), at)
	if err != nil || len(pair) < 2 {
		return err
	}
	was, is := pair[len(pair)-2].Rules, pair[len(pair)-1].Rules
	_, before, after := source.Split(was, is)
	if len(before)+len(after) == 0 {
		return nil
	}
	s.status.set("reviewing a change to the rules")
	to := at.Truncate(s.cfg.Step)
	from := to.Add(-7 * 24 * time.Hour)
	rctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	ran, fired := replay.RunEach(rctx, s.h, [][]replay.Version{{{Rules: before}}, {{Rules: after}}}, from, to, s.cfg.Step, s.cfg.Parallel)
	incs, err := s.l.Incidents(ctx, from, to, false)
	if err != nil {
		return err
	}
	plain := make([]incident.Incident, len(incs))
	for i, in := range incs {
		plain[i] = in.Incident
	}
	o.From, o.To = from, to
	var changed []string
	for _, r := range slices.Concat(before, after) {
		changed = append(changed, r.Name)
	}
	cmp := verdict.Compare(verdict.Judge(ran[0], fired[0], plain, o), verdict.Judge(ran[1], fired[1], plain, o), changed, len(was)-len(before))
	cmp.ChangedOnly = true
	cmp.Before.Firings, cmp.After.Firings = nil, nil
	if err := s.l.AddResult(ctx, "review", at, from, to, cmp); err != nil {
		return err
	}
	s.log.Info("change reviewed", "version", id, "rules", len(cmp.Rules))
	return s.l.AddRun(ctx, ledger.Run{Kind: "review", Started: s.now(), Finished: s.now(), From: from, To: to})
}

// digest judges the week before, suggests longer "for"s for its noisy
// rules, and posts the report to Slack: once a week at --digest, or as soon
// as it can when that time was missed.
func (s *Server) digest(ctx context.Context, o verdict.Options) error {
	due := s.cfg.Digest.Last(s.now(), s.cfg.Loc)
	week := ledger.Week(s.l.Day(due))
	from, to := s.l.Midnight(week-7), s.l.Midnight(week)
	sent, err := s.l.Meta(ctx, "digest")
	if err != nil || sent == fmt.Sprint(week) {
		return err
	}
	cursor, err := s.l.Cursor(ctx)
	if err != nil || cursor.Before(to) {
		return err // the week isn't all in the ledger yet
	}
	s.status.set("the weekly digest")
	began := s.now()
	rep, err := s.l.Report(ctx, from, to, o, true)
	if err != nil {
		return err
	}
	if !s.cfg.Alerts {
		vs, err := s.versions(ctx, from, to)
		if err != nil {
			return err
		}
		if len(vs) == 1 {
			incs := make([]incident.Incident, len(rep.Incidents))
			for i, in := range rep.Incidents {
				incs[i] = in.Incident
			}
			oo := o
			oo.From, oo.To = from, to
			rctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
			rep.Suggestions = source.Suggest(rctx, s.h, vs[0].Rules, rep, incs, oo, s.cfg.Step)
			cancel()
		}
	}
	if err := s.l.AddResult(ctx, "suggestions", s.now(), from, to, orNone(rep.Suggestions)); err != nil {
		return err
	}
	run := ledger.Run{Kind: "digest", Started: began, From: from, To: to}
	if s.cfg.Slack != "" {
		var b bytes.Buffer
		if err := verdict.Text(&b, rep, s.cfg.Loc); err != nil {
			return err
		}
		sctx, cancel := context.WithTimeout(ctx, time.Minute)
		if err := report.Slack(sctx, s.cfg.Slack, b.String()); err != nil {
			run.Error = err.Error()
		}
		cancel()
	}
	run.Finished = s.now()
	if err := s.l.AddRun(ctx, run); err != nil {
		return err
	}
	if run.Error != "" {
		return errors.New(run.Error)
	}
	return s.l.SetMeta(ctx, "digest", fmt.Sprint(week))
}

func orNone(s []verdict.Suggestion) []verdict.Suggestion {
	if s == nil {
		return []verdict.Suggestion{}
	}
	return s
}

// prune drops, once a day, what is older than --keep.
func (s *Server) prune(ctx context.Context) error {
	if s.cfg.Keep <= 0 {
		return nil
	}
	today := fmt.Sprint(s.l.Day(s.now()))
	if done, err := s.l.Meta(ctx, "pruned"); err != nil || done == today {
		return err
	}
	s.status.set("pruning")
	if err := s.l.Prune(ctx, s.now().Add(-s.cfg.Keep)); err != nil {
		return err
	}
	return s.l.SetMeta(ctx, "pruned", today)
}
