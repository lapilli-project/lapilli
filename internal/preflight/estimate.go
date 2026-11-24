package preflight

import (
	"context"
	"fmt"
	"maps"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/serve"
	"lapilli.dev/internal/source"
)

// Estimates are the numbers the estimate items say, worked out from what
// was read, with nothing more asked (docs/design/preflight.md).
type Estimates struct {
	Hourly   *Hourly   `json:"hourly,omitempty"`
	Backfill *Backfill `json:"backfill,omitempty"`
	Nights   *Nights   `json:"nights,omitempty"`
	Beside   *Beside   `json:"beside,omitempty"`
	// Backtests is the backtest lane's, with --backtest-listen.
	Backtests *Backtests `json:"backtests,omitempty"`
	Memory    *Memory    `json:"memory,omitempty"`
}

// Hourly is the runs every --every once caught up, over a day of them.
type Hourly struct {
	// Runs are a day's; Requests a run's, on average, rounded up; Most the
	// most one asks; Day a day's, by API in By.
	Runs     int            `json:"runs"`
	Requests int            `json:"requests"`
	Most     int            `json:"most"`
	Day      int            `json:"day"`
	By       map[string]int `json:"by"`
	// Guard and Impact are a day's --guard queries and --impact reads,
	// among Day.
	Guard  int `json:"guard"`
	Impact int `json:"impact"`
	// Seconds is the most a run takes at --max-rate, at least; Share what
	// share of --max-rate the runs and what is asked beside them use.
	// Neither without --max-rate.
	Seconds float64 `json:"seconds,omitempty"`
	Share   float64 `json:"share,omitempty"`
	// WhileBacktest is the same runs while the backtest lane runs a
	// backtest the whole hour, with --backtest-listen and --max-rate.
	WhileBacktest *WhileBacktest `json:"while_backtest,omitempty"`
}

// WhileBacktest is the hourly runs while the backtest lane runs, its share
// of --max-rate kept for it (prom.Pace.Reserve): Rate what the runs have
// of it then, requests a second; Seconds the most a run takes at that
// rate, at least; Share what share of that rate the runs and what is
// asked beside them use once caught up.
type WhileBacktest struct {
	Rate    float64 `json:"rate"`
	Seconds float64 `json:"seconds"`
	Share   float64 `json:"share,omitempty"`
}

// Backtests is the backtest lane's, with --backtest-listen
// (docs/design/backtests.md): what a pull request's backtest can ask
// within --backtest-timeout, counted as serve's port counts one before it
// is queued.
type Backtests struct {
	// Share is --backtest-share; Rate the lane's requests a second, that
	// share of --max-rate, none without it; Parallel how many it asks at
	// once.
	Share    float64 `json:"share"`
	Rate     float64 `json:"rate,omitempty"`
	Parallel int     `json:"parallel"`
	// TimeoutSeconds is --backtest-timeout; Fit the most requests a
	// backtest asks within it at Rate, none without --max-rate.
	TimeoutSeconds float64 `json:"timeout_seconds"`
	Fit            int     `json:"fit,omitempty"`
	// Rules are how many rules the backtest counted changes: ten of those
	// read the port takes, or fewer; Days its period, the recipe's
	// default.
	Rules int `json:"rules"`
	Days  int `json:"days"`
	// Changed is its requests with each rule's expression changed, the
	// two sides' reads apart; Shared with each rule's for alone, 5 minutes
	// longer, the two sides sharing the reads their for doesn't move.
	Changed int `json:"changed"`
	Shared  int `json:"shared"`
	// ChangedDays and SharedDays are the most days of each that fit within
	// --backtest-timeout at Rate, 1 to 14; 0, not a day; 14 without
	// --max-rate, as none is refused for its time then; 0 with no rule.
	ChangedDays int `json:"changed_days"`
	SharedDays  int `json:"shared_days"`
	// WeekRules and DayRules are, when the rules changed don't fit Days,
	// how many of them changed fit Days, and a day.
	WeekRules int `json:"week_rules,omitempty"`
	DayRules  int `json:"day_rules,omitempty"`
}

// Backfill is a first start's: a day at a time from --backfill before now.
type Backfill struct {
	Pieces   int            `json:"pieces"`
	Requests int            `json:"requests"`
	Most     int            `json:"most"`
	By       map[string]int `json:"by"`
	Guard    int            `json:"guard"`
	Impact   int            `json:"impact"`
	// Rate is what it is asked at while catching up, the lower of
	// --max-rate and --catch-up-rate; Seconds how long it takes at that
	// rate, at least; PieceHours the pieces serve halves a day to, to
	// finish each within --timeout. None without a rate.
	Rate       float64 `json:"rate,omitempty"`
	Seconds    float64 `json:"seconds,omitempty"`
	PieceHours float64 `json:"piece_hours,omitempty"`
}

// Nights is how many nights of --catch-up-hours the backfill takes, at
// least, and how many of its requests a night holds.
type Nights struct {
	Hours    string `json:"hours"`
	Nights   int    `json:"nights"`
	PerNight int    `json:"per_night"`
}

// Beside is what serve asks a day beside the runs, by what asks it.
type Beside struct {
	Day int            `json:"day"`
	By  map[string]int `json:"by"`
}

// Memory is what a run holds at the peak, and a request and a limit for
// the pod.
type Memory struct {
	// From is what it was worked out from: alerts, a day of what fired; or
	// caps, --parallel reads of --max-samples each.
	From         string `json:"from"`
	PeakBytes    int64  `json:"peak_bytes"`
	RequestBytes int64  `json:"request_bytes"`
	LimitBytes   int64  `json:"limit_bytes"`
	// CgroupBytes is the memory limit this process runs under, 0 for none
	// known.
	CgroupBytes int64 `json:"cgroup_bytes,omitempty"`
	// BacktestBytes is what the backtest lane's reads hold at once at most
	// beside the peak, with --backtest-listen: as many as it asks at once,
	// each of --max-samples.
	BacktestBytes int64 `json:"backtest_bytes,omitempty"`
}

// cacheBytes are the pages' cache and the ledger's, which a serve holds
// beside its reads: 32 MiB each.
const cacheBytes = 64 << 20

// estimate works out the requests, the time, the nights and the memory
// serve's runs will take, from the rules read, within the preflight's
// time (ctx): what a pull request's flags and rules make it count, a day
// of runs every minute and ten years of backfill over thousands of rules,
// is unknown once --preflight-timeout is up, not counted on.
func (p *run) estimate(ctx context.Context, rd *read) {
	s := p.c.Serve
	p.r.LeftOut = []string{"change reviews, a week of each new version of the rules", "the weekly digest's backtests",
		"what people and agents ask for (a proposal's backtest, MCP's compare, the settings tried)"}
	if s.Alerts {
		p.r.LeftOut = append([]string{"the --alerts mode, which reads ALERTS and replays nothing"}, p.r.LeftOut...)
	}
	if rd == nil {
		return
	}
	rs := replayable(rd.all)
	switch {
	case rd.failed:
		for _, id := range []string{"estimate.hourly", "estimate.backfill"} {
			p.add(Item{ID: id, State: Unknown, What: "not worked out: the rules couldn't all be read"})
		}
	case len(rs) == 0:
	default:
		p.hourly(ctx, rs)
		p.backfill(ctx, rs)
	}
	p.beside(rd)
	p.whileBacktest()
	p.backtests(ctx, rd)
	p.memory(rs)
}

// apiOf is the API a planned request goes to, by what replay.Plan names.
func (p *run) apiOf(planned string) string {
	switch {
	case planned == "":
		return "prometheus"
	case strings.HasPrefix(planned, "grafana/") && p.c.Serve.Grafana != "":
		return "grafana"
	}
	return "loki"
}

// piece counts the requests of one of serve's runs over [from, to]: each
// rule's replay started earlier by its margin, the rules starting alike
// replayed together (serve's find), as replay.Plan counts them. Once ctx
// is done, what is left isn't counted: the caller looks at ctx after.
func (p *run) piece(ctx context.Context, rs []rules.Rule, from, to time.Time) map[string]int {
	step := p.c.Serve.Step
	groups := map[time.Time][]rules.Rule{}
	need := map[string]time.Duration{}
	for _, r := range rs {
		need[r.Name] = max(need[r.Name], serve.Margin(r, step))
	}
	for _, r := range rs {
		since := from.Add(-need[r.Name]).Truncate(step)
		groups[since] = append(groups[since], r)
	}
	out := map[string]int{}
	h := p.planned()
	for _, since := range slices.SortedFunc(maps.Keys(groups), func(a, b time.Time) int { return a.Compare(b) }) {
		if ctx.Err() != nil {
			break
		}
		plan := replay.Plan(h, []replay.Version{{Rules: groups[since]}}, since, to, step)
		for k, n := range plan.By {
			out[p.apiOf(k)] += n
		}
	}
	return out
}

// promRequests is how many requests prom's client sends for a range:
// 1,441 steps each at most.
func promRequests(from, to time.Time, step time.Duration) int {
	n := 0
	for t := from; !t.After(to); t = t.Add(1440*step + step) {
		n++
	}
	return n
}

// asides are the --impact read and the --guard query of a run over [from,
// to], to --prometheus; with round, the look at what each named signal
// reads that follows a round's runs, a round a run once caught up.
func (p *run) asides(from, to time.Time, round bool) (impact, guard int) {
	s := p.c.Serve
	if s.Impact != "" {
		impact = promRequests(from.Add(-time.Hour), to, s.Step)
	}
	// Each named signal is read on its own (docs/design/signals.md); and,
	// after the round's runs, what it reads, by its match labels, a
	// request a selector it counts.
	for _, d := range s.Signals {
		impact += promRequests(from.Add(-time.Hour), to, s.Step)
		if round {
			impact += seenLooks(d, s.Options.Match)
		}
	}
	if s.Guard != "" {
		guard = 1
	}
	return impact, guard
}

// seenLooks are the requests a round's look at what a named signal reads
// asks: one a selector it counts, none matched on no label.
func seenLooks(d ledger.SignalDef, match []string) int {
	if len(d.MatchOr(match)) == 0 {
		return 0
	}
	sels, err := source.SeenSelectors(d.Expr)
	if err != nil {
		return 0
	}
	return len(sels)
}

// signalsOnce are the requests a first start asks of named signals once,
// beside its runs: where the data of each selector, and stand-in, each
// reads begins (until it is found, a request each, a round at a time).
// A read again (a signal added, or its expression or for changed, on a
// ledger with runs: one query over five weeks at most) and the catch-up
// of a signal missed (read again from where it was, within a week, as a
// run reads it) come later, and only then.
func signalsOnce(s serve.Config) int {
	n := 0
	for _, d := range s.Signals {
		if sels, err := ledger.SelectorsOf(d.Expr); err == nil {
			n += len(sels)
		}
	}
	return n
}

func sum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// floor is how long requests by API take at rate a second each, at least:
// each API is paced of its own, the longest of them.
func floor(by map[string]int, rate float64) time.Duration {
	most := 0
	for _, n := range by {
		most = max(most, n)
	}
	return time.Duration(float64(most) / rate * float64(time.Second))
}

// unworked is the item of an estimate the preflight's time ran out in.
func (p *run) unworked(id string) {
	p.add(Item{ID: id, State: Unknown, What: "not worked out within --preflight-timeout",
		Fix: "give --preflight-timeout longer, or fewer rules to replay (--rule)"})
}

// hourly counts a day of hourly runs once caught up: each from the last
// run's end less --settle, every --every.
func (p *run) hourly(ctx context.Context, rs []rules.Rule) {
	s := p.c.Serve
	day := p.now.Truncate(24 * time.Hour)
	n := max(int(24*time.Hour/s.Every), 1)
	h := &Hourly{Runs: n, By: map[string]int{}}
	// How long each run takes at --max-rate, at least: each API is paced
	// of its own.
	var took, longest time.Duration
	for k := 1; k <= n; k++ {
		to := day.Add(time.Duration(k) * s.Every).Truncate(s.Step)
		from := to.Add(-s.Every - s.Settle).Truncate(s.Step)
		by := p.piece(ctx, rs, from, to)
		if ctx.Err() != nil {
			p.unworked("estimate.hourly")
			return
		}
		impact, guard := p.asides(from, to, true)
		by["prometheus"] += impact + guard
		h.Impact += impact
		h.Guard += guard
		for k, v := range by {
			h.By[k] += v
		}
		h.Most = max(h.Most, sum(by))
		p.runsBy = append(p.runsBy, by)
		if p.c.MaxRate > 0 {
			t := floor(by, p.c.MaxRate)
			took += t
			longest = max(longest, t)
		}
	}
	h.Day = sum(h.By)
	h.Requests = (h.Day + n - 1) / n
	p.r.Estimates.Hourly = h
	what := fmt.Sprintf("about %s requests a run, the most %s, %s a day%s", num(h.Requests), num(h.Most), num(h.Day), byAPI(h.By))
	var among []string
	if h.Guard > 0 {
		among = append(among, "the guard's")
	}
	if h.Impact > 0 {
		among = append(among, "--impact's")
	}
	if len(among) > 0 {
		what += ", " + strings.Join(among, " and ") + " among them"
	}
	it := Item{ID: "estimate.hourly", State: OK, What: what}
	if p.c.MaxRate > 0 {
		h.Seconds = longest.Seconds()
		it.What += fmt.Sprintf(": at --max-rate %s a run takes %s at least, the most %s", rate(p.c.MaxRate), dur(took/time.Duration(n)), dur(longest))
		if longest > s.Every {
			it.State = Problem
			it.Why = fmt.Sprintf("a run's requests at --max-rate take longer than --every %s: the runs fall behind, and never catch up", dur(s.Every))
			it.Fix = "raise --max-rate, or --every, or give fewer rules to replay (--rule)"
		}
	} else {
		it.Why = fmt.Sprintf("without --max-rate, how long a run takes is the API's to say: %d requests at once at most (--parallel)", s.Parallel)
	}
	if s.LedgerURLFile != "" {
		it.Why = strings.TrimPrefix(it.Why+"; with several replicas, each asks at --max-rate of its own (docs/design/replicas.md)", "; ")
	}
	p.add(it)
}

// backfill counts a first start's backfill: a day at a time from
// --backfill before now, the first with no settle, a guard query before
// each day.
func (p *run) backfill(ctx context.Context, rs []rules.Rule) {
	s := p.c.Serve
	now := p.now.Truncate(s.Step)
	cursor, settle := now.Add(-s.Backfill), time.Duration(0)
	b := &Backfill{By: map[string]int{}}
	var catchUp []map[string]int // the pieces more than a day behind
	rest := map[string]int{}     // the requests of the others, by API
	var mostBy map[string]int
	for cursor.Before(now) {
		from := cursor.Add(-settle).Truncate(s.Step)
		to := cursor.Add(24 * time.Hour)
		if to.After(now) {
			to = now
		}
		by := p.piece(ctx, rs, from, to)
		if ctx.Err() != nil {
			p.unworked("estimate.backfill")
			if s.CatchUpHours != nil {
				p.unworked("estimate.nights")
			}
			return
		}
		impact, guard := p.asides(from, to, false)
		by["prometheus"] += impact + guard
		b.Impact += impact
		b.Guard += guard
		for k, v := range by {
			b.By[k] += v
		}
		if t := sum(by); t > b.Most {
			b.Most, mostBy = t, by
		}
		if now.Sub(cursor) > serve.CatchUpAfter {
			catchUp = append(catchUp, by)
		} else {
			for k, v := range by {
				rest[k] += v
			}
		}
		b.Pieces++
		cursor, settle = to, s.Settle
	}
	// And of named signals, once: where the data each reads begins, and
	// the look at what each reads after the backfill's round.
	if len(s.Signals) > 0 {
		once := signalsOnce(s)
		for _, d := range s.Signals {
			once += seenLooks(d, s.Options.Match)
		}
		b.By["prometheus"] += once
		b.Impact += once
	}
	b.Requests = sum(b.By)
	p.r.Estimates.Backfill = b
	r := p.c.MaxRate
	if s.CatchUpRate > 0 && (r == 0 || s.CatchUpRate < r) {
		r = s.CatchUpRate
	}
	it := Item{ID: "estimate.backfill", State: OK, What: fmt.Sprintf("%s requests over %s, %s, the most %s%s",
		num(b.Requests), dur(s.Backfill), plural(b.Pieces, "piece")+" of a day at most", num(b.Most), byAPI(b.By))}
	if r <= 0 {
		it.Why = fmt.Sprintf("without --max-rate or --catch-up-rate, how long it takes is the API's to say: %d requests at once at most (--parallel)", s.Parallel)
		p.add(it)
		p.nights(nil, 0)
		return
	}
	// A catch-up's pieces at the lower of the rates; the rest, less than a
	// day behind, at --max-rate, or, without it, no sooner than at once.
	total := time.Duration(0)
	for _, by := range catchUp {
		total += floor(by, r)
	}
	if p.c.MaxRate > 0 {
		total += floor(rest, p.c.MaxRate)
	}
	b.Rate, b.Seconds = r, total.Seconds()
	day := floor(mostBy, r)
	it.What += fmt.Sprintf(": at least %s at %s a second", dur(total), rate(r))
	if day > s.Timeout {
		hours := 24.0
		for time.Duration(float64(day)*hours/24) > s.Timeout && hours > 1 {
			hours /= 2
		}
		b.PieceHours = hours
		it.Why = fmt.Sprintf("the most a day asks takes %s at that rate, past --timeout %s: serve halves the piece and goes on, in pieces of %s",
			dur(day), dur(s.Timeout), dur(time.Duration(hours*float64(time.Hour))))
	} else {
		it.Why = fmt.Sprintf("each day's in %s at most, within --timeout %s", dur(day), dur(s.Timeout))
	}
	p.add(it)
	p.nights(catchUp, r)
}

// nights counts the nights of --catch-up-hours the backfill takes, at
// rate: a day of it begins only while the hours last, and one they end in
// is the last of that night, from the first run in the hours on. Outside
// them the catch-up waits, its cursor where it was, and the day that
// passes is a day more to catch up (docs/design/quota.md, 4): one of the
// catch-up's days, on average. A night that holds no more than a day of
// it never catches up.
func (p *run) nights(catchUp []map[string]int, at float64) {
	w := p.c.Serve.CatchUpHours
	if w == nil {
		return
	}
	if at <= 0 {
		p.add(Item{ID: "estimate.nights", State: Unknown, What: "not worked out: without --max-rate or --catch-up-rate, how long a day takes is the API's to say"})
		return
	}
	n := &Nights{Hours: w.String(), PerNight: int(w.Len().Seconds() * at)}
	p.r.Estimates.Nights = n
	if len(catchUp) == 0 {
		p.add(Item{ID: "estimate.nights", State: OK, What: "none: the backfill is less than a day behind, and runs at any hour"})
		return
	}
	var left []time.Duration
	var all time.Duration
	for _, by := range catchUp {
		t := floor(by, at)
		left, all = append(left, t), all+t
	}
	day := all / time.Duration(len(left))
	if day >= w.Len() {
		n.Nights = -1
		p.add(Item{ID: "estimate.nights", State: Problem, What: fmt.Sprintf("never: a night of %s at %s a second holds %s requests, no more than a day of the catch-up asks",
			w, rate(at), num(n.PerNight)),
			Why: "the catch-up waits outside its hours, and each day it waits is a day more to catch up: the ledger falls further behind",
			Fix: "give --catch-up-hours longer, or --catch-up-rate more, or a shorter --backfill"})
		return
	}
	const most = 3650
	for len(left) > 0 && n.Nights < most {
		n.Nights++
		for t := time.Duration(0); len(left) > 0 && t < w.Len(); left = left[1:] {
			t += left[0]
		}
		if len(left) > 0 {
			left = append(left, day) // the day that passed
		}
	}
	p.add(Item{ID: "estimate.nights", State: OK, What: fmt.Sprintf("%s at least, in %s at %s a second, %s of the backfill's requests a night",
		plural(n.Nights, "night"), w, rate(at), num(n.PerNight)),
		Why: "each day the catch-up waits outside its hours is a day more to catch up, counted; each 429 with --yield-on-429, or each time the guard holds the runs, makes it longer; nothing newer is recorded meanwhile, so the pages show the ledger days behind until it is done"})
}

// beside counts what serve asks a day beside the runs.
func (p *run) beside(rd *read) {
	s := p.c.Serve
	by := map[string]int{}
	watch := int(24 * time.Hour / max(s.Watch, time.Second))
	switch {
	case s.Loaded:
		by["rules"] = watch
	case s.Grafana != "":
		by["rules"] = watch
	case s.Ruler != "":
		by["rules"] = watch
	}
	if s.LokiLoaded {
		by["loki_rules"] = 2 * watch
	}
	if len(s.Alarms) > 0 {
		every := s.AlarmsEvery
		if every <= 0 {
			every = 10 * time.Minute
		}
		by["alarms"] = len(s.Alarms) * int(24*time.Hour/every)
	}
	if rd != nil && s.CheckReplay && len(replayable(rd.all)) > 0 {
		n := 1
		if slices.ContainsFunc(rd.all, func(r rules.Rule) bool { return r.Graph != nil }) {
			n = 2
		}
		by["check_replay"] = 3 * n
	}
	if s.CheckContent && s.ScrapeInterval == 0 && p.historyBase() != "" {
		by["scrape_intervals"] = 2
	}
	if s.CheckContent && s.Grafana != "" {
		by["grafana_notifications"] = 3 * 24
	}
	if s.CheckContent && source.AlertmanagerURL(s.AlertmanagerConfig) {
		by["alertmanager_config"] = 24
	}
	if rd != nil && len(rd.dataSources) > 0 {
		by["grafana_data_sources"] = 24 * (len(rd.dataSources) + rd.unread)
	}
	if c := s.OnCall; c != nil && c.Every > 0 {
		feeds := 0
		for _, sch := range c.Schedules {
			if sch.File == "" {
				feeds++
			}
		}
		if feeds > 0 {
			by["on_call"] = feeds * int(24*time.Hour/c.Every)
		}
	}
	if len(by) == 0 {
		return
	}
	b := &Beside{Day: sum(by), By: by}
	p.r.Estimates.Beside = b
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(by)) {
		parts = append(parts, fmt.Sprintf("%s %s", besideSaid[k], num(by[k])))
	}
	if h := p.r.Estimates.Hourly; h != nil && p.c.MaxRate > 0 {
		h.Share = float64(h.Day+b.Day) / (86400 * p.c.MaxRate)
	}
	p.add(Item{ID: "estimate.beside", State: OK, What: fmt.Sprintf("%s requests a day beside the runs: %s", num(b.Day), strings.Join(parts, ", "))})
	p.shareSaid()
}

// besideSaid says what each of Beside's keys counts.
var besideSaid = map[string]string{
	"rules":                 "the rules read every --watch",
	"loki_rules":            "--loki's rules read every --watch",
	"alarms":                "the alarms read every --alarms-every",
	"check_replay":          "the replay check",
	"scrape_intervals":      "the scrape intervals",
	"grafana_notifications": "Grafana's templates",
	"alertmanager_config":   "the Alertmanager's configuration, each hour",
	"grafana_data_sources":  "Grafana's data sources, each hour",
	"on_call":               "the on-call feeds",
}

// shareSaid adds, to the hourly runs' item, the share of --max-rate the
// runs and what is asked beside them use once caught up.
func (p *run) shareSaid() {
	h := p.r.Estimates.Hourly
	if h == nil || h.Share == 0 {
		return
	}
	for i := range p.r.Items {
		if p.r.Items[i].ID == "estimate.hourly" {
			p.r.Items[i].What += fmt.Sprintf("; once caught up, with what is asked beside them, %s of --max-rate", percent(h.Share))
		}
	}
}

// memory works out what a run holds at the peak: of what fired over the
// last day, the reads of the --parallel largest rules at once, at
// prom.SampleBytes a sample, a read held to --max-samples; with nothing
// read of what fired, the caps' bound.
func (p *run) memory(rs []rules.Rule) {
	s := p.c.Serve
	most := p.c.MaxSamples
	if most <= 0 {
		most = prom.DefaultMaxSamples
	}
	m := &Memory{}
	it := Item{ID: "estimate.memory", State: OK}
	var over []string
	if len(p.alerts) > 0 && len(rs) > 0 {
		m.From = "alerts"
		every := map[string]time.Duration{}
		for _, r := range rs {
			every[r.AlertName()] = max(every[r.AlertName()], r.Interval)
		}
		var reads []int64
		for name, n := range p.alerts {
			iv, ok := every[name]
			if !ok {
				continue // fired, but by no rule replayed
			}
			samples := n
			if iv > s.Step {
				samples *= float64(iv) / float64(s.Step)
			}
			if samples > float64(most) {
				over = append(over, name)
				samples = float64(most)
			}
			reads = append(reads, int64(math.Ceil(samples))*prom.SampleBytes)
		}
		slices.Sort(reads)
		slices.Reverse(reads)
		for i := 0; i < len(reads) && i < s.Parallel; i++ {
			m.PeakBytes += reads[i]
		}
		m.PeakBytes += cacheBytes
		it.What = fmt.Sprintf("about %s at the peak: the %d largest reads of what fired in the last day at once, beside 64 MB of caches", bytesOf(uint64(m.PeakBytes)), min(s.Parallel, len(reads)))
		if len(reads) == 0 {
			it.What = fmt.Sprintf("about %s at the peak: no rule replayed fired in the last day, so a day's read of each holds little, beside 64 MB of caches", bytesOf(uint64(m.PeakBytes)))
		}
	} else {
		m.From = "caps"
		m.PeakBytes = int64(s.Parallel)*int64(most)*prom.SampleBytes + cacheBytes
		// A bound, not an estimate: no request or limit is advised from it,
		// as it is the most a run can take, not what one takes.
		it.What = fmt.Sprintf("no firing read: at most %s, the caps' bound (%d x %s x %d bytes, beside 64 MB of caches), which --parallel and --max-samples set: the most a run can take, not what one takes, which is worked out from a day of the history's alerts when it has any",
			bytesOf(uint64(m.PeakBytes)), s.Parallel, num(most), prom.SampleBytes)
	}
	const mib = 64 << 20
	m.RequestBytes = (m.PeakBytes + mib - 1) / mib * mib
	// Go's collector lets the heap grow to about twice what is live before
	// it collects (GOGC=100).
	m.LimitBytes = 2 * m.RequestBytes
	if m.From == "alerts" {
		it.What += fmt.Sprintf(": a request of %s, a limit of %s", bytesOf(uint64(m.RequestBytes)), bytesOf(uint64(m.LimitBytes)))
	}
	if len(over) > 0 {
		it.Why = fmt.Sprintf("a day of %s would pass --max-samples %s: serve won't replay it, and says why", names(over, 5), num(most))
	}
	// The backtest lane's reads, beside the runs': of a pull request's
	// rules, which nothing read here says the size of, so at their bound.
	if m.BacktestBytes = p.laneMemory(most); m.BacktestBytes > 0 {
		it.What += fmt.Sprintf("; beside them, the backtest lane's %d reads at once, of a pull request's rules, hold %s at most (%d x %s x %d bytes)",
			p.laneParallel(), bytesOf(uint64(m.BacktestBytes)), p.laneParallel(), num(most), prom.SampleBytes)
	}
	if limit, ok := cgroupLimit(p.c.CgroupMemory); ok {
		m.CgroupBytes = limit
		switch {
		case limit >= m.PeakBytes && limit < m.PeakBytes+m.BacktestBytes:
			// What a pull request's rules read is a bound here, not an
			// estimate: past the limit is unknown, never a problem.
			it.State = Unknown
			it.Why = strings.TrimPrefix(it.Why+fmt.Sprintf("; this process runs under a memory limit of %s: the runs' peak fits, and with the backtest lane's reads at their bound it doesn't; what a pull request's rules read can't be told here", bytesOf(uint64(limit))), "; ")
			it.Fix = fmt.Sprintf("to be held under the limit whatever a backtest reads, give the pod %s more, or lower --parallel or --max-samples", bytesOf(uint64(m.PeakBytes+m.BacktestBytes-limit)))
		case limit >= m.PeakBytes:
		case m.From == "caps":
			// With nothing read of what fired, the bound is what the caps
			// allow, not what a run holds: under it is unknown, never a
			// problem, as history.alerts is.
			it.State = Unknown
			it.Why = fmt.Sprintf("this process runs under a memory limit of %s, under the caps' bound: with no firing read, what a run holds can't be told, and the bound is reached only when %d reads at once each hold --max-samples", bytesOf(uint64(limit)), s.Parallel)
			it.Fix = fmt.Sprintf("to be held under the limit whatever a run reads, give the pod a request of %s and a limit of %s, or lower --parallel or --max-samples", bytesOf(uint64(m.RequestBytes)), bytesOf(uint64(m.LimitBytes)))
		default:
			it.State = Problem
			it.Why = strings.TrimPrefix(it.Why+fmt.Sprintf("; this process runs under a memory limit of %s, under the estimate", bytesOf(uint64(limit))), "; ")
			it.Fix = "give the pod the request and the limit above, or lower --parallel or --max-samples"
		}
	}
	p.r.Estimates.Memory = m
	p.add(it)
}

// cgroupLimit is the memory limit the process runs under, as the cgroup
// says (memory.max, or v1's memory.limit_in_bytes): ok false for none.
func cgroupLimit(path string) (int64, bool) {
	paths := []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"}
	if path != "" {
		paths = []string{path}
	}
	for _, f := range paths {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		v := strings.TrimSpace(string(b))
		n, err := strconv.ParseInt(v, 10, 64)
		if v == "max" || err != nil || n <= 0 || n >= 1<<60 {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// byAPI says requests by API, when there is more than one.
func byAPI(by map[string]int) string {
	if len(by) < 2 {
		for k := range by {
			if k != "prometheus" {
				return " to " + k
			}
		}
		return ""
	}
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(by)) {
		parts = append(parts, k+" "+num(by[k]))
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// num is a whole number as a person reads it: 11,900.
func num(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}

// dur is a duration as a person reads it: 6h37m, 13m, 40s.
func dur(d time.Duration) string {
	switch {
	case d >= 48*time.Hour && d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d >= time.Hour:
		d = d.Round(time.Minute)
		if d%time.Hour == 0 {
			return fmt.Sprintf("%dh", d/time.Hour)
		}
		return fmt.Sprintf("%dh%02dm", d/time.Hour, d%time.Hour/time.Minute)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", d.Round(time.Minute)/time.Minute)
	}
	return fmt.Sprintf("%ds", d.Round(time.Second)/time.Second)
}

// rate is requests a second, as given.
func rate(r float64) string { return strconv.FormatFloat(r, 'f', -1, 64) }

// percent is a share as a whole percentage, one under one percent as such.
func percent(x float64) string {
	if x > 0 && x < 0.01 {
		return "under 1%"
	}
	return fmt.Sprintf("%.0f%%", x*100)
}

// bytesOf is a size as a person reads it, in decimal units: 5.1 GB.
func bytesOf(n uint64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.0f MB", float64(n)/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.0f kB", float64(n)/1e3)
	}
	return fmt.Sprintf("%d bytes", n)
}
