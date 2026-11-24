package preflight

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/cloud"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
)

// read is the rules as a run would replay them, and what reading them
// found: nil when there are none to read (--alerts, or firings copied in
// alone).
type read struct {
	all []rules.Rule // as serve would replay them, those that can't among them
	// recording is how many recording rules the files hold, -1 where they
	// came from an API, which gives the alerting ones alone.
	recording int
	// failed says the rules, or some of them, couldn't be read: the
	// estimates need them.
	failed bool
	// dataSources are Grafana's, by uid: what reading each answered, ""
	// for read; kinds their types.
	dataSources map[string]string
	kinds       map[string]string
	unread      int // data sources past the twenty read
}

// agent is what the preflight's requests say they come from.
func (p *run) agent() string { return "lapilli/" + p.c.Version }

// historyClient is the history's client, as serve asks it, made once; nil
// with the error it couldn't be made with.
func (p *run) historyClient(ctx context.Context) (*prom.Client, error) {
	if p.h == nil && p.hErr == nil {
		if p.historyBase() == "" {
			p.hErr = fmt.Errorf("there is no --prometheus to ask")
		} else {
			p.h, p.hErr = p.c.History.History(ctx, p.agent())
		}
	}
	return p.h, p.hErr
}

// rules reads the rules once, from where serve's watcher would read them,
// and says rules.read, rules.replayable and rules.history.
func (p *run) rules(ctx context.Context) *read {
	s := p.c.Serve
	following := s.RulesHistory != "" && s.RulesPath == ""
	main := s.Loaded || s.Grafana != "" || s.Ruler != "" || following || s.RulesPath != ""
	if s.Alerts || !main && len(s.Alarms) == 0 && s.LokiRules == "" && !s.LokiLoaded {
		return nil
	}
	rd := &read{recording: -1}
	var rs []rules.Rule
	var from, failed, unasked, notes []string
	var picked rules.Picked
	agent := p.agent()
	fail := func(flag string, err error) {
		failed = append(failed, flag+": "+said(err))
		rd.failed = true
	}
	ask := func(flag string) bool {
		if p.c.Offline {
			unasked = append(unasked, flag)
			rd.failed = true
			return false
		}
		return true
	}
	each, cancel := context.WithTimeout(ctx, eachTime)
	defer cancel()
	switch {
	case s.Loaded:
		from = append(from, "--loaded-rules")
		if ask("--loaded-rules") {
			if h, err := p.historyClient(ctx); err != nil {
				fail("--loaded-rules", err)
			} else if got, _, err := source.Loaded(each, h); err != nil {
				fail("--loaded-rules", err)
			} else {
				rs = append(rs, got...)
			}
		}
	case s.Grafana != "":
		from = append(from, "--grafana")
		if ask("--grafana") {
			if got, _, err := source.Grafana(each, s.Grafana, s.GrafanaToken, agent, nil); err != nil {
				fail("--grafana", err)
			} else {
				rs = append(rs, got...)
			}
		}
	case s.Ruler != "":
		from = append(from, "--ruler")
		if ask("--ruler") {
			if got, err := p.readRuler(each); err != nil {
				fail("--ruler", err)
			} else {
				rs = append(rs, got...)
			}
		}
	case following:
		from = append(from, "--rules-history")
		got, err := p.newest()
		if err != nil {
			fail("--rules-history", err)
		}
		rs = append(rs, got...)
	case s.RulesPath != "":
		from = append(from, "--rules")
		var rec []rules.Recording
		got, _, pk, err := rules.ReadPicked(s.RulesPath, rules.Picking{Select: s.RulesSelect, Recordings: &rec})
		picked.Add(pk)
		if err != nil {
			fail("--rules", err)
		} else {
			rs = append(rs, got...)
			rd.recording = len(rec)
		}
	}
	for _, at := range s.Alarms {
		flag := "--alarms " + at.Source
		from = append(from, flag)
		if !ask(flag) {
			continue
		}
		got, err := p.readAlarms(ctx, at.Source, at.URL)
		if err != nil {
			fail(flag, err)
			continue
		}
		rs = append(rs, got...)
	}
	var places map[rules.Place]time.Duration
	switch {
	case s.LokiLoaded:
		from = append(from, "--loki-loaded-rules")
		if ask("--loki-loaded-rules") {
			got, _, pl, err := source.LokiLoaded(each, p.c.Loki, agent, func(what string) { notes = append(notes, what) })
			if err != nil {
				fail("--loki-loaded-rules", err)
			} else {
				rs, places = append(rs, got...), pl
			}
		}
	case s.LokiRules != "":
		from = append(from, "--loki-rules")
		got, _, ns, err := source.LokiFiles(s.LokiRules)
		notes = append(notes, ns...)
		if err != nil {
			fail("--loki-rules", err)
		} else {
			rs = append(rs, got...)
		}
	}
	if s.RuleRE != nil {
		rs = slices.DeleteFunc(rs, func(r rules.Rule) bool { return !s.RuleRE.MatchString(r.Name) })
	}
	source.TellApart(rs)
	if s.Grafana != "" && !p.c.Offline {
		p.dataSources(ctx, rd, rs)
	}
	rules.Replaying{Late: s.Late, Scrape: s.ScrapeInterval, DataSources: p.ds, Kinds: rd.kinds, Places: places,
		NoLoki: s.Loki == "", NoGrafanaLoki: s.Loki == "" && s.Grafana == ""}.Apply(rs)
	rd.all = rs

	where := strings.Join(from, ", ")
	switch {
	case len(failed) > 0:
		p.add(Item{ID: "rules.read", State: Problem, What: "the rules couldn't all be read, from " + where, Why: strings.Join(failed, "; "),
			Fix: "a source that can't be read leaves the rules as they were each --watch: put it right before serve starts, which has no rules to replay until then"})
	case len(unasked) > 0:
		p.add(Item{ID: "rules.read", State: Unknown, What: counted(rs, rd.recording) + ", from " + strings.Join(slices.DeleteFunc(slices.Clone(from), func(f string) bool { return slices.Contains(unasked, f) }), ", "),
			Why: "--offline: " + strings.Join(unasked, ", ") + " not asked"})
	case len(rs) == 0:
		p.add(Item{ID: "rules.read", State: Problem, What: "no alert rules, from " + where, Fix: "serve replays nothing until there are some: check the path, --rules-select and --rule"})
	default:
		it := Item{ID: "rules.read", State: OK, What: counted(rs, rd.recording) + ", from " + where}
		if len(notes) > 0 {
			it.Why = clip(strings.Join(notes, "; "), 400)
		}
		p.add(it)
	}
	p.replayable(rs, picked)
	p.rulesHistory()
	return rd
}

// counted says how many rules of each kind, and recording rules where known.
func counted(rs []rules.Rule, recording int) string {
	kinds := map[string]int{}
	for _, r := range rs {
		switch {
		case r.Cloud != "":
			kinds["a cloud's alarms"]++
		case r.Graph != nil:
			kinds["Grafana"]++
		case r.Logs != "":
			kinds["Loki"]++
		default:
			kinds["Prometheus"]++
		}
	}
	var parts []string
	for _, k := range []string{"Prometheus", "Grafana", "Loki", "a cloud's alarms"} {
		if kinds[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", kinds[k], k))
		}
	}
	out := plural(len(rs), "alert rule")
	if len(parts) > 1 {
		out += " (" + strings.Join(parts, ", ") + ")"
	} else if len(parts) == 1 && len(rs) > 0 {
		out += " (" + strings.TrimPrefix(parts[0], fmt.Sprint(len(rs))+" ") + ")"
	}
	if recording >= 0 {
		out += ", " + plural(recording, "recording rule")
	}
	return out
}

// replayable says which rules can't be replayed, and why, as the rules
// list would; the names told apart; what --rules-select passed over.
func (p *run) replayable(rs []rules.Rule, picked rules.Picked) {
	if len(rs) == 0 {
		return
	}
	why := map[string][]string{}
	apart := 0
	for _, r := range rs {
		if r.Unsupported != "" {
			reason := prom.Steady(r.Unsupported)
			why[reason] = append(why[reason], r.Name)
		}
		if r.Name != r.AlertName() {
			apart++
		}
	}
	not := 0
	var reasons []string
	for _, reason := range slices.Sorted(maps.Keys(why)) {
		not += len(why[reason])
		reasons = append(reasons, fmt.Sprintf("%s (%s)", clip(reason, 200), names(why[reason], 3)))
	}
	it := Item{ID: "rules.replayable", State: OK, What: fmt.Sprintf("%d can be replayed", len(rs)-not)}
	if not > 0 {
		it.What += fmt.Sprintf("; %d can't, each with why", not)
		it.Why = strings.Join(reasons, "; ")
	}
	if apart > 0 {
		it.What += fmt.Sprintf("; %s named apart, as several rules shared a name", plural(apart, "rule"))
	}
	if said := picked.String(); said != "" {
		it.What += "; --rules-select passed over " + said
	}
	if not == len(rs) {
		it.State = Problem
		it.Fix = "serve records no firing of a rule it can't replay"
	}
	p.add(it)
	p.sources(rs)
}

// sources says when Grafana's rules name more than one data source for
// their PromQL: each is replayed against one history, --prometheus, and
// whether the data sources are one Prometheus isn't known here. Nothing is
// said of one or none.
func (p *run) sources(rs []rules.Rule) {
	uids := rules.HistoryUIDs(rs)
	if len(uids) < 2 {
		return
	}
	p.add(Item{ID: "rules.sources", State: Unknown,
		What: fmt.Sprintf("Grafana's rules query %d data sources: %s", len(uids), names(uids, 5)),
		Why:  "each is replayed against one history, --prometheus; whether they are one Prometheus isn't known"})
}

// rulesHistory says what --rules-history holds.
func (p *run) rulesHistory() {
	dir := p.c.Serve.RulesHistory
	if dir == "" {
		return
	}
	hs, err := rules.History(dir)
	if err != nil {
		p.add(Item{ID: "rules.history", State: Problem, What: "--rules-history can't be read", Why: said(err),
			Fix: "a first start's backfill then replays every day with today's rules"})
		return
	}
	start := p.now.Add(-p.c.Serve.Backfill)
	it := Item{ID: "rules.history", State: OK, What: fmt.Sprintf("%s, from %s to %s", plural(len(hs), "version"),
		hs[0].At.UTC().Format(time.RFC3339), hs[len(hs)-1].At.UTC().Format(time.RFC3339))}
	if hs[0].At.After(start) {
		it.Why = fmt.Sprintf("none is from before the backfill starts (%s): the earliest is taken as the rules from before then, as serve warns at its first start",
			start.Format(time.RFC3339))
	}
	p.add(it)
}

// newest is the newest version of --rules-history from before now that
// reads, as serve follows it.
func (p *run) newest() ([]rules.Rule, error) {
	s := p.c.Serve
	hs, err := rules.History(s.RulesHistory)
	if err != nil {
		return nil, err
	}
	for i := len(hs) - 1; i >= 0; i-- {
		if hs[i].At.After(p.now) {
			continue
		}
		rs, _, _, err := rules.ReadPicked(hs[i].Path, rules.Picking{Select: s.RulesSelect, Trim: true})
		if err == nil && len(rs) > 0 {
			return rs, nil
		}
	}
	return nil, fmt.Errorf("%s holds no version from before now that reads", s.RulesHistory)
}

// readRuler reads --ruler once, with its credentials, as serve does.
func (p *run) readRuler(ctx context.Context) ([]rules.Rule, error) {
	s := p.c.Serve
	r, err := source.NewRuler(s.Ruler, p.agent())
	if err != nil {
		return nil, err
	}
	r.Tenant = s.RulerTenant
	r.Warn = func(string) {}
	switch r.Kind {
	case source.RulerConfig:
		r.Reads.TokenFile = s.RulerToken
	default:
		kind := "aws"
		if r.Kind == source.RulerAzure {
			kind = "azure"
		}
		if r.Reads.Sign, err = cloud.Request(ctx, kind, s.Ruler, ""); err != nil {
			return nil, err
		}
	}
	rs, docs, err := r.Read(ctx)
	p.read("ruler", docs)
	return rs, err
}

// read keeps the documents a read of the rules gave, by where they were
// read (ruler, alarms:SOURCE): what --changes-in names among them is what
// the checks of writes ask about.
func (p *run) read(where string, docs []rules.Doc) {
	if p.docs == nil {
		p.docs = map[string][]rules.Doc{}
	}
	p.docs[where] = docs
}

// readAlarms reads a cloud's alarms once, with its credentials, as serve
// does.
func (p *run) readAlarms(ctx context.Context, src, raw string) ([]rules.Rule, error) {
	a, err := source.NewAlarms(src, raw, p.agent())
	if err != nil {
		return nil, err
	}
	a.Warn = func(string) {}
	kind := map[string]string{source.AlarmsAWS: "aws", source.AlarmsAzure: "azure", source.AlarmsGoogle: "google", source.AlarmsOCI: "oci"}[a.Kind]
	each, cancel := context.WithTimeout(ctx, eachTime)
	defer cancel()
	if a.Reads.Sign, err = cloud.Request(each, kind, a.URL, ""); err != nil {
		return nil, err
	}
	docs, err := a.Docs(each, time.Minute)
	if err != nil {
		return nil, err
	}
	p.read("alarms:"+src, docs)
	return rules.FromDocs(docs)
}

// mostDataSources is how many of Grafana's data sources are read.
const mostDataSources = 20

// dataSources reads the Grafana data sources the rules name, twenty at
// most, as serve reads them each hour: what each answered, for
// history.grafana, and their types, which say whether their queries are
// PromQL or LogQL.
func (p *run) dataSources(ctx context.Context, rd *read, rs []rules.Rule) {
	var uids []string
	for _, r := range rs {
		for _, u := range r.GrafanaUIDs() {
			if !slices.Contains(uids, u) {
				uids = append(uids, u)
			}
		}
	}
	slices.Sort(uids)
	if len(uids) > mostDataSources {
		rd.unread = len(uids) - mostDataSources
		uids = uids[:mostDataSources]
	}
	rd.dataSources, rd.kinds = map[string]string{}, map[string]string{}
	host, path, ok := at(p.c.Serve.Grafana)
	if !ok || len(uids) == 0 {
		return
	}
	for _, u := range uids {
		p.al.add(allow{api: "grafana", method: http.MethodGet, host: host, path: path + "/api/datasources/uid/" + u})
	}
	each, cancel := context.WithTimeout(ctx, eachTime)
	defer cancel()
	ds, kinds, missed, err := source.GrafanaDataSources(each, p.c.Serve.Grafana, p.c.Serve.GrafanaToken, p.agent(), uids)
	for _, u := range uids {
		rd.dataSources[u] = ""
	}
	if err != nil {
		for _, u := range uids {
			rd.dataSources[u] = said(err)
		}
		return
	}
	for _, m := range missed {
		uid, status, _ := strings.Cut(m, " ")
		rd.dataSources[uid] = strings.Trim(status, "()")
	}
	p.ds, rd.kinds = ds, kinds
}

// lokiLabels is the path Loki's labels are read at over the last five
// minutes: the cheapest read of a Loki.
func (p *run) lokiLabels() string {
	q := url.Values{"start": {fmt.Sprint(p.now.Add(-5 * time.Minute).UnixNano())}, "end": {fmt.Sprint(p.now.UnixNano())}}
	return "/loki/api/v1/labels?" + q.Encode()
}

// nothing is a history that is never asked: what replay.Plan needs to know
// which rules have one.
type nothing struct{}

func (nothing) QueryRange(context.Context, string, time.Time, time.Time, time.Duration) ([]prom.Series, error) {
	return nil, fmt.Errorf("not asked")
}

// planned is the history replay.Plan is given: one for every rule a run
// would have one for, as source.Logs gives them.
func (p *run) planned() replay.History {
	s := p.c.Serve
	return replay.WithLogs(nothing{}, func(r rules.Rule) (replay.History, error) {
		if r.Graph != nil && s.Grafana != "" || s.Loki != "" {
			return nothing{}, nil
		}
		return nil, fmt.Errorf("no Loki")
	})
}
