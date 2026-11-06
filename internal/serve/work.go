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
	"strconv"
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
	"lapilli.dev/internal/sse"
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
	// Taken before the settings are read, so a change saved in between is
	// judged with.
	rejudge := s.rejudge.Swap(false)
	o, err := s.options(ctx)
	if err != nil {
		s.rejudge.Store(rejudge || s.rejudge.Load())
		s.fail("settings", err)
		return
	}
	// Spans a quiet window left to judge again, should its write have been
	// cut short.
	if err := s.l.JudgePending(ctx, o); err != nil {
		s.fail("judging again", err)
	}
	if rejudge {
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
		} else {
			s.status.Lock()
			if s.status.errWhat == "incidents" {
				s.status.err, s.status.errWhat = "", ""
			}
			s.status.Unlock()
		}
	}
	if run {
		s.runs(ctx, o)
		if err := s.keepBegan(ctx); err != nil {
			s.fail("a run", err)
		}
	}
	if len(s.dests()) > 0 {
		if _, err := s.l.Overdue(ctx, s.l.Midnight(s.l.Day(s.now()))); err != nil {
			s.fail("notifying", err)
		}
	}
	if !s.cfg.Alerts {
		if err := s.reviewChanges(ctx, o); err != nil {
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
	s.status.errWhat, s.status.err = what, err.Error()
	s.status.Unlock()
}

func (s *Server) syncIncidents(ctx context.Context, o verdict.Options) error {
	incs, err := incident.Load(s.cfg.Incidents)
	if err != nil {
		return err
	}
	changed, err := s.l.SyncFile(ctx, incs, o)
	var taken *ledger.TakenError
	if errors.As(err, &taken) {
		s.passedOver.Store(int64(len(taken.IDs)))
	} else if err == nil {
		s.passedOver.Store(0)
	}
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
	if !s.cfg.Alerts {
		// Nothing to replay until the rules have been read: the cursor
		// waits, so no hour is passed over.
		if _, ok, err := s.l.Latest(ctx); err != nil || !ok {
			if err == nil {
				err = errors.New("the rules haven't been read yet; runs wait for them")
			}
			s.fail("a run", err)
			return
		}
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
		s.status.set("replaying %s → %s", from.In(s.cfg.Loc).Format("2006-01-02 15:04"), to.In(s.cfg.Loc).Format("2006-01-02 15:04"))
		began := s.now()
		found, unfinished, err := s.find(ctx, from, to)
		switch {
		case unfinished && to.Sub(cursor) > s.cfg.Every:
			// Too much at once for the time given: half as much.
			piece = to.Sub(cursor) / 2
			s.log.Warn("a run didn't finish within --timeout; replaying less at once", "hours", piece.Hours())
			continue
		case err != nil:
			s.count(kind, "failed")
			s.fail("a run", err)
			s.l.AddRun(context.WithoutCancel(ctx), ledger.Run{Kind: kind, Started: began, Finished: s.now(), From: from, To: to, Error: err.Error()})
			return
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
		// What is wrong with the incidents file is for reading it to clear.
		if s.status.errWhat != "incidents" {
			s.status.err, s.status.errWhat = "", ""
		}
		s.status.Unlock()
		s.log.Info("run", "kind", kind, "from", from, "to", to, "firings", len(found.Firings), "failed", len(found.Run.Failed), "took", s.now().Sub(began).Round(time.Millisecond))
		cursor, settle = to, s.cfg.Settle
		if !unfinished && piece < 24*time.Hour {
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
// whether time ran out, for the run to be tried over less.
func (s *Server) find(parent context.Context, from, to time.Time) (ledger.Found, bool, error) {
	f := ledger.Found{From: from, To: to, Since: map[string]time.Time{}, Fresh: map[string]time.Time{},
		Keep: map[string]bool{}, Missed: map[string]time.Time{}}
	failed := map[string]string{}
	f.Run.Failed = failed
	unfinished := false
	// A query on its own that runs out of time: the run is tried over
	// less, or not recorded.
	slow := func(ctx context.Context, err error) (ledger.Found, bool, error) {
		if ctx.Err() == context.DeadlineExceeded {
			return f, true, errSlow
		}
		return f, false, err
	}
	// Impact first, with time of its own: one query, which a replay that
	// runs out of time shouldn't take along.
	if s.cfg.Impact != "" {
		ctx, cancel := context.WithTimeout(parent, s.cfg.Timeout)
		defer cancel()
		f.ImpactOn, f.ImpactSince = true, from.Add(-time.Hour)
		series, err := s.h.QueryRange(ctx, s.cfg.Impact, f.ImpactSince, to, s.cfg.Step)
		if err != nil {
			return slow(ctx, fmt.Errorf("--impact: %w", err))
		}
		f.Impact = incident.FromImpact(series, s.cfg.Step, s.cfg.ImpactFor)
	}
	ctx, cancel := context.WithTimeout(parent, s.cfg.Timeout)
	defer cancel()
	if s.cfg.Alerts {
		since := from.Add(-time.Hour)
		series, err := s.h.QueryRange(ctx, `ALERTS{alertstate="firing"}`, since, to, s.cfg.Step)
		if err != nil {
			return slow(ctx, fmt.Errorf("reading ALERTS: %w", err))
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
		return f, false, nil
	}

	// Each rule's replay starts early enough to see the pending time of
	// the first firing it records: from its From, or where a run before
	// missed it, less its margin. Rules that start alike are replayed
	// together and share their requests.
	missed, err := s.l.Missed(ctx)
	if err != nil {
		return f, false, err
	}
	versions, err := s.versions(ctx, from, to)
	if err != nil {
		return f, false, err
	}
	need, fresh := map[string]time.Duration{}, map[string]time.Duration{}
	for _, v := range versions {
		for _, r := range v.Rules {
			need[r.Name] = max(need[r.Name], margin(r, s.cfg.Step))
			fresh[r.Name] = max(fresh[r.Name], r.For+r.Interval+s.cfg.Step)
		}
	}
	groups := map[time.Time]map[string]bool{}
	for name, m := range need {
		start := from
		if at, ok := missed[name]; ok && at.Before(from) {
			start = at
			f.Missed[name] = at
		}
		since := start.Add(-m).Truncate(s.cfg.Step)
		if groups[since] == nil {
			groups[since] = map[string]bool{}
		}
		groups[since][name] = true
	}
	meta := map[string]*ledger.Rule{}
	var order []string
	// The latest first: the rules the run is for, then those catching up,
	// so one rule far behind doesn't keep the rest from their turn.
	for _, since := range slices.SortedFunc(maps.Keys(groups), func(a, b time.Time) int { return b.Compare(a) }) {
		vs, err := s.versions(ctx, since, to)
		if err != nil && ctx.Err() == nil {
			return f, false, err
		}
		if ctx.Err() != nil {
			// Out of time before their turn: kept as they were, and
			// replayed from here by the next run.
			unfinished = true
			for name := range groups[since] {
				f.Keep[name] = true
				failed[name] = replay.Unfinished
			}
			continue
		}
		var set []replay.Version
		for _, v := range vs {
			rv := replay.Version{From: v.Since}
			if !v.Since.After(since) {
				rv.From = time.Time{}
			}
			for _, r := range v.Rules {
				if groups[since][r.Name] {
					rv.Rules = append(rv.Rules, r)
				}
			}
			set = append(set, rv)
		}
		ran, fs := replay.Run(ctx, s.h, set, since, to, s.cfg.Step, s.cfg.Parallel)
		for _, r := range ran {
			f.Since[r.Name] = since
			f.Fresh[r.Name] = since.Add(fresh[r.Name])
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
	slices.Sort(order)
	for _, name := range order {
		f.Rules = append(f.Rules, *meta[name])
	}
	return f, unfinished, nil
}

// errSlow is a query on its own that didn't finish within --timeout.
var errSlow = errors.New("a query didn't finish within --timeout")

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
		rs, err := s.parse(ctx, v)
		if err != nil {
			return nil, err
		}
		out = append(out, version{v, rs})
	}
	return out, nil
}

func (s *Server) parse(ctx context.Context, v ledger.Version) ([]rules.Rule, error) {
	k := fmt.Sprint(s.cfg.Ledger, "\x00", v.ID)
	parsed.Lock()
	rs, ok := parsed.m[k]
	parsed.Unlock()
	if ok {
		return rs, nil
	}
	all, names, err := s.named(ctx, v.ID)
	if err != nil {
		return nil, err
	}
	for j, r := range all {
		if names[j] != "" {
			r.Name = names[j]
			rs = append(rs, r)
		}
	}
	parsed.Lock()
	if len(parsed.m) >= 128 {
		for k := range parsed.m { // one out, any one: most are in use
			delete(parsed.m, k)
			break
		}
	}
	parsed.m[k] = rs
	parsed.Unlock()
	return rs, nil
}

// named reads a version's rules again, as they were read, and the name
// each is judged under ("" for one --rule leaves out). Should a newer
// lapilli read the documents into other rules than the names were given
// to, the rules are named afresh.
func (s *Server) named(ctx context.Context, id int64) ([]rules.Rule, []string, error) {
	full, err := s.l.VersionByID(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	all, err := rules.FromDocs(full.Docs)
	if err != nil {
		return nil, nil, err
	}
	if len(all) == len(full.Names) {
		return all, full.Names, nil
	}
	s.log.Warn("a version of the rules reads differently now; naming its rules afresh", "version", id)
	names := make([]string, len(all))
	var keep []rules.Rule
	var at []int
	for i, r := range all {
		if s.cfg.RuleRE == nil || s.cfg.RuleRE.MatchString(r.Name) {
			keep = append(keep, r)
			at = append(at, i)
		}
	}
	source.TellApart(keep)
	for j, i := range at {
		names[i] = keep[j].Name
	}
	return all, names, nil
}

// watch reads the rules every --watch and records a new version when
// anything that decides when they fire has changed. raw is what the
// documents read last hashed to.
func (s *Server) watch(ctx context.Context, raw string) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.cfg.Watch):
		}
		if err := s.look(ctx, &raw); err != nil {
			s.fail("reading the rules", err)
		}
	}
}

// identity is what a rule keeps its name by from one version to the next.
func identity(r rules.Rule) string {
	return fmt.Sprintf("%q %q %q %q", r.Group, r.Name, sse.LabelKey(r.Labels), r.UID)
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
	sum := hex.EncodeToString(h.Sum(nil))
	if sum == *raw {
		return nil
	}
	var keep []rules.Rule
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
		*raw = sum
		return nil
	}
	// A rule keeps the name it had in the version before; a new one is
	// named apart from the rules alongside it and those before.
	had := map[string]string{}
	var was []rules.Rule
	if ok {
		all, names, err := s.named(ctx, latest.ID)
		if err != nil {
			return err
		}
		for i, r := range all {
			if names[i] != "" {
				had[identity(r)] = names[i]
				was = append(was, r)
			}
		}
	}
	plain := slices.Clone(keep)
	source.TellApart(keep, was)
	names := make([]string, len(rs))
	for j, i := range at {
		names[i] = keep[j].Name
		if n, ok := had[identity(plain[j])]; ok {
			names[i] = n
		}
	}
	since := s.now().Truncate(time.Second)
	if !ok {
		since = time.Time{} // the rules as they stood before the ledger began
	}
	id, err := s.l.AddVersion(ctx, ledger.Version{Since: since, Digest: digest, Docs: docs, Names: names})
	if err != nil {
		return err
	}
	*raw = sum
	s.log.Info("rules changed: a new version", "version", id, "rules", len(keep))
	if ok {
		s.poke() // to review it
	}
	return nil
}

// poke asks the worker to look at its jobs now, if it isn't already asked.
func (s *Server) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
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

// reviewChanges reviews each version of the rules recorded since the last
// one reviewed: kept in the ledger, so a restart doesn't drop one.
func (s *Server) reviewChanges(ctx context.Context, o verdict.Options) error {
	done, err := s.l.Meta(ctx, "reviewed")
	if err != nil {
		return err
	}
	last, _ := strconv.ParseInt(done, 10, 64)
	vs, err := s.l.Versions(ctx, time.Unix(0, 0), s.now(), false)
	if err != nil {
		return err
	}
	for i, v := range vs {
		if v.ID <= last {
			continue
		}
		if i > 0 && last > 0 {
			if err := s.reviewChange(ctx, vs[i-1], v, o); err != nil {
				return err
			}
		}
		if err := s.l.SetMeta(ctx, "reviewed", fmt.Sprint(v.ID)); err != nil {
			return err
		}
		last = v.ID
	}
	return nil
}

// reviewChange replays the rules a version changed, as they were and as
// they are, over the week before it, and keeps the comparison.
func (s *Server) reviewChange(ctx context.Context, before, after ledger.Version, o verdict.Options) error {
	was, err := s.parse(ctx, before)
	if err != nil {
		return err
	}
	is, err := s.parse(ctx, after)
	if err != nil {
		return err
	}
	_, gone, came := source.Split(was, is)
	if len(gone)+len(came) == 0 {
		return nil
	}
	s.status.set("reviewing a change to the rules")
	at := after.Since
	to := at.Truncate(s.cfg.Step)
	from := to.Add(-7 * 24 * time.Hour)
	cmp, err := s.replayChange(ctx, gone, came, len(was)-len(gone), from, to, s.cfg.Timeout, o)
	if err != nil {
		return err
	}
	if err := s.l.AddResult(ctx, "review", at, from, to, cmp); err != nil {
		return err
	}
	s.log.Info("change reviewed", "version", after.ID, "rules", len(cmp.Rules))
	return s.l.AddRun(ctx, ledger.Run{Kind: "review", Started: s.now(), Finished: s.now(), From: from, To: to})
}

// replayChange replays the rules a change took away and those it brought
// over [from, to), within timeout, and sets them side by side, judged
// against the ledger's incidents; same is how many rules it left alike.
func (s *Server) replayChange(ctx context.Context, gone, came []rules.Rule, same int, from, to time.Time, timeout time.Duration, o verdict.Options) (*verdict.Comparison, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ran, fired := replay.RunEach(rctx, s.h, [][]replay.Version{{{Rules: gone}}, {{Rules: came}}}, from, to, s.cfg.Step, s.cfg.Parallel)
	incs, err := s.l.Incidents(ctx, from, to, false)
	if err != nil {
		return nil, err
	}
	plain := make([]incident.Incident, len(incs))
	for i, in := range incs {
		plain[i] = in.Incident
	}
	o.From, o.To, o.SomeRules = from, to, true
	var changed []string
	for _, r := range slices.Concat(gone, came) {
		changed = append(changed, r.Name)
	}
	cmp := verdict.Compare(verdict.Judge(ran[0], fired[0], plain, o), verdict.Judge(ran[1], fired[1], plain, o), changed, same)
	cmp.ChangedOnly = true
	cmp.Before.Firings, cmp.After.Firings = nil, nil
	return cmp, nil
}

// digest judges the week before, suggests longer "for"s for its noisy
// rules, and keeps them, once a week at --digest, or as soon as it can when
// that time was missed; then tells of it and posts it.
func (s *Server) digest(ctx context.Context, o verdict.Options) error {
	due := s.cfg.Digest.Last(s.now(), s.cfg.Loc)
	week := ledger.Week(s.l.Day(due))
	from, to := s.l.Midnight(week-7), s.l.Midnight(week)
	cursor, err := s.l.Cursor(ctx)
	if err != nil || cursor.Before(to) {
		return err // the week isn't all in the ledger yet
	}
	done, err := s.l.Meta(ctx, "digest")
	if err != nil {
		return err
	}
	fresh := done != fmt.Sprint(week)
	if fresh {
		s.status.set("the weekly digest")
		began := s.now()
		rep, err := s.l.Report(ctx, from, to, o, false)
		if err != nil {
			return err
		}
		var sugg []verdict.Suggestion
		var near []verdict.NearMiss
		if !s.cfg.Alerts {
			vs, err := s.versions(ctx, from, to)
			if err != nil {
				return err
			}
			incs := make([]incident.Incident, len(rep.Incidents))
			for i, in := range rep.Incidents {
				incs[i] = in.Incident
			}
			oo := o
			oo.From, oo.To = from, to
			if len(vs) == 1 {
				rctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
				sugg = source.Suggest(rctx, s.h, vs[0].Rules, rep, incs, oo, s.cfg.Step)
				cancel()
			}
			// The incidents nothing caught, against the rules as they stood.
			rvs := make([]replay.Version, len(vs))
			for i, v := range vs {
				rvs[i] = replay.Version{From: v.Since, Rules: v.Rules}
			}
			rctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
			near = source.NearMisses(rctx, s.h, rvs, rep, incs, oo, s.cfg.Step, s.cfg.Parallel)
			cancel()
		}
		if err := s.l.AddResult(ctx, "suggestions", s.now(), from, to, orNone(sugg)); err != nil {
			return err
		}
		if near == nil {
			near = []verdict.NearMiss{}
		}
		if err := s.l.AddResult(ctx, "near_misses", s.now(), from, to, near); err != nil {
			return err
		}
		if err := s.l.AddRun(ctx, ledger.Run{Kind: "digest", Started: began, Finished: s.now(), From: from, To: to}); err != nil {
			return err
		}
		if err := s.l.SetMeta(ctx, "digest", fmt.Sprint(week)); err != nil {
			return err
		}
	}
	return errors.Join(s.tellDigest(ctx, fmt.Sprint(week), fresh, from, to, o), s.postDigest(ctx, fmt.Sprint(week), from, to, o))
}

// postDigest posts the week's report to Slack, trying again each hour until
// it goes.
func (s *Server) postDigest(ctx context.Context, week string, from, to time.Time, o verdict.Options) error {
	if s.cfg.Slack == "" {
		return nil
	}
	if sent, err := s.l.Meta(ctx, "digest-sent"); err != nil || sent == week {
		return err
	}
	if s.now().Sub(s.slackTried) < time.Hour {
		return nil
	}
	s.slackTried = s.now()
	rep, err := s.digestReport(ctx, from, to, o)
	if err != nil {
		return err
	}
	spare, err := s.spareRules(ctx)
	if err != nil {
		return err
	}
	text, err := s.digestText(ctx, rep, spare, from, to, nil)
	if err != nil {
		return err
	}
	sctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := report.Slack(sctx, s.cfg.Slack, text); err != nil {
		s.l.AddRun(ctx, ledger.Run{Kind: "digest", Started: s.now(), Finished: s.now(), From: from, To: to, Error: err.Error()})
		return err
	}
	return s.l.SetMeta(ctx, "digest-sent", week)
}

// digestReport is the week's report, with the suggestions worked out for it.
func (s *Server) digestReport(ctx context.Context, from, to time.Time, o verdict.Options) (*verdict.Report, error) {
	rep, err := s.l.Report(ctx, from, to, o, false)
	if err != nil {
		return nil, err
	}
	rs, err := s.l.Results(ctx, "suggestions", 1)
	if err != nil {
		return nil, err
	}
	if len(rs) > 0 && rs[0].From.Equal(from) {
		if err := rs[0].Decode(&rep.Suggestions); err != nil {
			return nil, err
		}
	}
	if rs, err = s.l.Results(ctx, "near_misses", 1); err != nil {
		return nil, err
	}
	if len(rs) > 0 && rs[0].From.Equal(from) {
		if err := rs[0].Decode(&rep.NearMisses); err != nil {
			return nil, err
		}
	}
	return rep, nil
}

// spareRules are the rules spare over the twelve weeks kept: a week alone
// rarely has the incidents to tell.
func (s *Server) spareRules(ctx context.Context) ([]string, error) {
	t, err := s.tallies(ctx, weeks)
	if err != nil {
		return nil, err
	}
	credit, err := s.credit(ctx, t, weeks)
	if err != nil {
		return nil, err
	}
	var out []string
	for name, r := range credit {
		if r.Spare {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out, nil
}

// digestMax is how long a digest's text is, at most, in what is told of it.
const digestMax = 256 << 10

// digestText is the week's report, and how things stand: for everyone, or
// for one team.
func (s *Server) digestText(ctx context.Context, rep *verdict.Report, spare []string, from, to time.Time, team *ledger.Team) (string, error) {
	shown := *rep
	shown.Rules = slices.Clone(rep.Rules)
	for i := range shown.Rules {
		shown.Rules[i].Spare = false // said over twelve weeks, below
	}
	var report bytes.Buffer
	if err := verdict.Text(&report, &shown, s.cfg.Loc); err != nil {
		return "", err
	}
	var id int64
	var outs string
	if team != nil {
		id = team.ID
	} else {
		var err error
		if outs, err = s.outcomesLine(ctx); err != nil {
			return "", err
		}
	}
	tasks, err := s.tasksLine(ctx, from, to, id)
	if err != nil {
		return "", err
	}
	line := ""
	if len(spare) > 0 {
		names := make([]string, 0, len(spare))
		for _, name := range spare[:min(len(spare), 20)] {
			names = append(names, verdict.Clean(name))
		}
		line = tr(s.cfg.Lang, "Over twelve weeks, another rule caught every incident these caught, as soon, at the same severity or above: %s.", strings.Join(names, ", "))
		if n := len(spare) - len(names); n > 0 {
			line += " " + tr(s.cfg.Lang, "And %d more.", n)
		}
	}
	// These go after the report's first lines, not at its end: on a large
	// install, what is too long for a chat is cut from the end.
	head, rest, _ := strings.Cut(report.String(), "\n\n")
	var b strings.Builder
	b.WriteString(head + "\n")
	for _, x := range []string{line, tasks, outs} {
		if x != "" {
			b.WriteString("\n" + x + "\n")
		}
	}
	if rest != "" {
		b.WriteString("\n" + rest)
	}
	return strings.ToValidUTF8(b.String()[:min(b.Len(), digestMax)], ""), nil
}

// digestAbout is what is told of a digest: its numbers, for a receiver to
// use as it likes, and the report as text, for one to pass on to a chat:
// as written, and escaped for Slack.
func digestAbout(rep *verdict.Report, text, team string, spare []string) map[string]any {
	firings, white := 0, 0
	noisy, silent := []string{}, 0
	for _, r := range rep.Rules {
		firings += r.Firings
		white += r.White
		if r.Noisy {
			noisy = append(noisy, r.Rule)
		}
		if r.Firings+r.Excused+r.Incidents == 0 && r.Unsupported == "" {
			silent++
		}
	}
	missed, onlyNoisy := 0, 0
	for _, in := range rep.Incidents {
		if in.Missed() {
			missed++
		}
		if in.OnlyNoisy() {
			onlyNoisy++
		}
	}
	type suggestion struct {
		Rule   string `json:"rule"`
		For    string `json:"for"`
		NewFor string `json:"new_for"`
		Fired  int    `json:"fired"`
		Would  int    `json:"would_fire"`
	}
	suggestions := []suggestion{}
	for _, x := range rep.Suggestions {
		suggestions = append(suggestions, suggestion{x.Rule, verdict.Duration(x.For), verdict.Duration(x.NewFor), x.Before.Firings, x.After.Firings})
	}
	if spare == nil {
		spare = []string{}
	}
	about := map[string]any{"from": rep.From.UTC(), "to": rep.To.UTC(), "firings": firings, "caught": white, "nothing": firings - white,
		"never_fired": silent, "incidents": len(rep.Incidents), "missed": missed, "only_noisy": onlyNoisy,
		"noisy_rules": noisy, "spare_rules": spare, "suggestions": suggestions,
		// text is the report as written, from rule names and incident titles
		// others wrote: escape it for where it goes. slack_text is it as the
		// Slack digest posts it.
		"text": text, "slack_text": report.SlackText(text)}
	if team != "" {
		about["team"] = team
	}
	return about
}

// teamReport is a report's part for one team: its rules, and the incidents
// it owns.
func teamReport(rep *verdict.Report, o *owners, t ledger.Team) *verdict.Report {
	out := *rep
	out.Firings = nil
	out.Rules = slices.DeleteFunc(slices.Clone(rep.Rules), func(r verdict.RuleResult) bool { return o.rule[r.Rule] != t.ID })
	out.Suggestions = slices.DeleteFunc(slices.Clone(rep.Suggestions), func(x verdict.Suggestion) bool { return o.rule[x.Rule] != t.ID })
	out.Incidents = slices.DeleteFunc(slices.Clone(rep.Incidents), func(in verdict.IncidentResult) bool {
		return !o.ownsIncident(t, ledger.Incident{Incident: in.Incident, CaughtBy: in.CaughtBy})
	})
	return &out
}

// tellDigest tells of the week's digest, once: to the webhook, for everyone
// and for each team, and to each team that has a webhook of its own. A team
// to which nothing happened hears nothing. It goes through the outbox, so a
// receiver that is down holds up nothing and is tried again.
func (s *Server) tellDigest(ctx context.Context, week string, fresh bool, from, to time.Time, o verdict.Options) error {
	if s.cfg.NotifyWebhook == "" && len(s.cfg.TeamDigests) == 0 {
		return nil
	}
	told0, err := s.l.Meta(ctx, "digest-told")
	if err != nil || told0 == week {
		return err
	}
	// A webhook added, or an upgrade, on a Friday: Monday's digest is old news.
	if told0 == "" && !fresh {
		return s.l.SetMeta(ctx, "digest-told", week)
	}
	rep, err := s.digestReport(ctx, from, to, o)
	if err != nil {
		return err
	}
	spare, err := s.spareRules(ctx)
	if err != nil {
		return err
	}
	own, err := s.owners(ctx)
	if err != nil {
		return err
	}
	for name := range s.cfg.TeamDigests {
		if _, ok := own.team(name); !ok {
			s.log.Warn("--team-digest names a team the ledger doesn't have", "team", name)
		}
	}
	var told []ledger.Told
	if s.cfg.NotifyWebhook != "" {
		text, err := s.digestText(ctx, rep, spare, from, to, nil)
		if err != nil {
			return err
		}
		told = append(told, ledger.Told{Dest: "webhook", About: digestAbout(rep, text, "", spare)})
	}
	for _, t := range own.teams {
		_, own1 := s.cfg.TeamDigests[t.Name]
		if s.cfg.NotifyWebhook == "" && !own1 {
			continue
		}
		part := teamReport(rep, own, t)
		fired := slices.ContainsFunc(part.Rules, func(r verdict.RuleResult) bool { return r.Firings+r.Excused > 0 })
		if !fired && len(part.Incidents) == 0 {
			continue
		}
		mine := slices.DeleteFunc(slices.Clone(spare), func(name string) bool { return own.rule[name] != t.ID })
		text, err := s.digestText(ctx, part, mine, from, to, &t)
		if err != nil {
			return err
		}
		about := digestAbout(part, text, t.Name, mine)
		if s.cfg.NotifyWebhook != "" {
			told = append(told, ledger.Told{Dest: "webhook", About: about})
		}
		if own1 {
			told = append(told, ledger.Told{Dest: teamDest, About: about})
		}
	}
	return s.l.TellOnce(ctx, "digest-told", week, "digest.ready", told)
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

// tasksLine says, for the digest, how the follow-ups stand, of a team's or
// of everyone's: open, overdue, and done in the week.
func (s *Server) tasksLine(ctx context.Context, from, to time.Time, team int64) (string, error) {
	open, late, done, err := s.l.TeamTaskCounts(ctx, from, to, team)
	if err != nil || open+done == 0 {
		return "", err
	}
	return tr(s.cfg.Lang, "Tasks: %d open, %d overdue, %d done in the week.", open, late, done), nil
}
