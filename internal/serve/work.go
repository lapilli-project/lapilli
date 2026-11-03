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
		}
	}
	if run {
		s.runs(ctx, o)
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
		s.status.err, s.status.errWhat = "", ""
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
		select {
		case s.wake <- struct{}{}: // to review it
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
	rctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	ran, fired := replay.RunEach(rctx, s.h, [][]replay.Version{{{Rules: gone}}, {{Rules: came}}}, from, to, s.cfg.Step, s.cfg.Parallel)
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
	for _, r := range slices.Concat(gone, came) {
		changed = append(changed, r.Name)
	}
	cmp := verdict.Compare(verdict.Judge(ran[0], fired[0], plain, o), verdict.Judge(ran[1], fired[1], plain, o), changed, len(was)-len(gone))
	cmp.ChangedOnly = true
	cmp.Before.Firings, cmp.After.Firings = nil, nil
	if err := s.l.AddResult(ctx, "review", at, from, to, cmp); err != nil {
		return err
	}
	s.log.Info("change reviewed", "version", after.ID, "rules", len(cmp.Rules))
	return s.l.AddRun(ctx, ledger.Run{Kind: "review", Started: s.now(), Finished: s.now(), From: from, To: to})
}

// digest judges the week before, suggests longer "for"s for its noisy
// rules, and keeps them, once a week at --digest, or as soon as it can when
// that time was missed; then posts the report to Slack, trying again each
// hour until it goes.
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
	if done != fmt.Sprint(week) {
		s.status.set("the weekly digest")
		began := s.now()
		rep, err := s.l.Report(ctx, from, to, o, false)
		if err != nil {
			return err
		}
		var sugg []verdict.Suggestion
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
				sugg = source.Suggest(rctx, s.h, vs[0].Rules, rep, incs, oo, s.cfg.Step)
				cancel()
			}
		}
		if err := s.l.AddResult(ctx, "suggestions", s.now(), from, to, orNone(sugg)); err != nil {
			return err
		}
		if err := s.l.AddRun(ctx, ledger.Run{Kind: "digest", Started: began, Finished: s.now(), From: from, To: to}); err != nil {
			return err
		}
		if err := s.l.SetMeta(ctx, "digest", fmt.Sprint(week)); err != nil {
			return err
		}
	}
	if s.cfg.Slack == "" {
		return nil
	}
	if sent, err := s.l.Meta(ctx, "digest-sent"); err != nil || sent == fmt.Sprint(week) {
		return err
	}
	if s.now().Sub(s.slackTried) < time.Hour {
		return nil
	}
	s.slackTried = s.now()
	rep, err := s.l.Report(ctx, from, to, o, true)
	if err != nil {
		return err
	}
	if rs, err := s.l.Results(ctx, "suggestions", 1); err != nil {
		return err
	} else if len(rs) > 0 && rs[0].From.Equal(from) {
		if err := rs[0].Decode(&rep.Suggestions); err != nil {
			return err
		}
	}
	var b bytes.Buffer
	if err := verdict.Text(&b, rep, s.cfg.Loc); err != nil {
		return err
	}
	if line, err := s.tasksLine(ctx, from, to); err != nil {
		return err
	} else if line != "" {
		b.WriteString("\n" + line + "\n")
	}
	sctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := report.Slack(sctx, s.cfg.Slack, b.String()); err != nil {
		s.l.AddRun(ctx, ledger.Run{Kind: "digest", Started: s.now(), Finished: s.now(), From: from, To: to, Error: err.Error()})
		return err
	}
	return s.l.SetMeta(ctx, "digest-sent", fmt.Sprint(week))
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

// tasksLine says, for the digest, how the follow-ups stand: open, overdue,
// and done in the week.
func (s *Server) tasksLine(ctx context.Context, from, to time.Time) (string, error) {
	open, late, done, err := s.l.TaskCounts(ctx, from, to)
	if err != nil || open+done == 0 {
		return "", err
	}
	return tr(s.cfg.Lang, "Tasks: %d open, %d overdue, %d done in the week.", open, late, done), nil
}
