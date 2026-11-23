package serve

import (
	"context"
	"fmt"
	"math/rand"
	"net/http/httptest"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// A model of what the copies of Grafana's history hold after each round,
// set beside what they hold, over made-up schedules of rounds, each of a
// fixed seed: rules of a judged rule's title coming and going, restarts,
// --settle, of a history that says no rule's id; the same over two and a
// half days, a rule left out for longer than one; the same over ten days,
// a rule left out for longer than a week; and, of a history that says
// each change's rule, rules of a judged rule's title held at rounds,
// others made, firing and deleted between them, and one in a folder the
// token doesn't read. Now and then a rule's change to a state Grafana has
// no name for, or a firing of labels no write holds, leaves it out too;
// and, without the beat, a read may hold no change of a title.
//
// LAPILLI_MODEL_SEEDS=N runs N schedules; LAPILLI_MODEL_SEED=K runs
// schedule K alone, saying each round.

// modelSet is a set of periods, as time past now, apart and in order.
type modelSet [][2]time.Duration

func (a modelSet) add(x [2]time.Duration) modelSet {
	if x[1] <= x[0] {
		return a
	}
	out := append(slices.Clone(a), x)
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	m := modelSet{out[0]}
	for _, s := range out[1:] {
		if last := &m[len(m)-1]; s[0] <= last[1] {
			last[1] = max(last[1], s[1])
		} else {
			m = append(m, s)
		}
	}
	return m
}

func (a modelSet) minus(b modelSet) modelSet {
	var out modelSet
	for _, s := range a {
		lo := s[0]
		for _, o := range b {
			if o[1] <= lo {
				continue
			}
			if o[0] >= s[1] {
				break
			}
			if o[0] > lo {
				out = append(out, [2]time.Duration{lo, o[0]})
			}
			lo = o[1]
		}
		if lo < s[1] {
			out = append(out, [2]time.Duration{lo, s[1]})
		}
	}
	return out
}

func (a modelSet) has(t time.Duration) bool {
	return slices.ContainsFunc(a, func(s [2]time.Duration) bool { return t >= s[0] && t < s[1] })
}

// next is where a covers again after t, which it doesn't cover.
func (a modelSet) next(t time.Duration) (time.Duration, bool) {
	for _, s := range a {
		if s[0] > t {
			return s[0], true
		}
	}
	return 0, false
}

func (a modelSet) String() string {
	var out []string
	for _, s := range a {
		out = append(out, s[0].String()+"→"+s[1].String())
	}
	return strings.Join(out, " ")
}

// modelFiring is a firing of a judged rule, as Grafana's history has it;
// unheld, one of labels no write holds.
type modelFiring struct {
	title, service string
	s, e           time.Duration
	open, unheld   bool
}

// modelReturn is a rule judged again from a round's start a, past a week
// after it was left out from h0: read back from rs alone, to T.
type modelReturn struct {
	h0, a, rs, T time.Duration
}

// The kinds of schedule.
const (
	modelUntold = iota // a history that says no rule's id
	modelLong          // the same, a rule left out for longer than a day
	modelTold          // a history that says each change's rule
	modelWeek          // the same as modelUntold, a rule left out for longer than a week
)

const (
	modelDay      = 24 * time.Hour
	modelWeekLong = 7 * modelDay
)

func modelSpansOf(t *testing.T, sh *shop, rule string) modelSet {
	t.Helper()
	spans, err := sh.s.l.SpansOf(context.Background(), ledger.GrafanaSource, rule)
	if err != nil {
		t.Fatal(err)
	}
	var out modelSet
	for _, sp := range spans {
		out = append(out, [2]time.Duration{sp[0].Sub(now), sp[1].Sub(now)})
	}
	return out
}

// modelIn reports whether one of ts lies in [from, to].
func modelIn(ts []time.Duration, from, to time.Duration) bool {
	return slices.ContainsFunc(ts, func(x time.Duration) bool { return x >= from && x <= to })
}

// modelRun runs the schedule of seed, and says how it differs from the
// model, "" for not at all.
func modelRun(t *testing.T, seed int64, loud bool) string {
	rng := rand.New(rand.NewSource(seed))
	kind := int(seed % 4)
	settle := []time.Duration{time.Hour, time.Hour, 3 * time.Hour, 20 * time.Minute}[rng.Intn(4)]
	titles := []string{"Orders slow", "Stock low"}
	if kind == modelLong || kind == modelWeek || rng.Intn(10) < 6 {
		// Over days, a third rule never shared, so that no day is a gap
		// for every rule: a first day after a gap is read back a day.
		titles = append(titles, "Cart full")
	}
	// Without the beat, a read may hold no change of a title.
	beating := kind == modelLong || kind == modelWeek || rng.Intn(10) < 6
	// A fourth rule, on since before the first round, that the history
	// can't say all of now and then: a change to a state Grafana has no
	// name for, and firings no write holds.
	disk := kind == modelLong || rng.Intn(10) < 5
	if disk {
		titles = append(titles, "Disk full")
	}
	uids := map[string]string{"Orders slow": ordersUID, "Stock low": stockUID, "Cart full": "cart", "Disk full": "disk"}

	// The rounds.
	var rounds []time.Duration
	at, until, steps := time.Duration(0), 20*time.Hour, []time.Duration{20, 30, 40, 60, 60, 90, 120, 180, 240}
	n := 6 + rng.Intn(8)
	// weekEnd is when Orders slow stops being held twice, of modelWeek.
	weekEnd := modelWeekLong + 30*time.Hour + time.Duration(seed%5)*time.Hour
	switch kind {
	case modelLong:
		until, steps, n = 60*time.Hour, []time.Duration{60, 120, 180, 240, 240}, 40
	case modelWeek:
		until, steps, n = weekEnd+60*time.Hour, []time.Duration{240, 360, 480}, 60
	}
	for len(rounds) < n {
		at += steps[rng.Intn(len(steps))] * time.Minute
		if at > until {
			break
		}
		rounds = append(rounds, at)
	}
	end := rounds[len(rounds)-1]

	// What the judged rules fired, under each title, for each service, and
	// the changes of each title the history holds.
	var truth []modelFiring
	changes, bad := map[string][]time.Duration{}, map[string][]time.Duration{}
	unheld := map[string][]time.Duration{}
	g := &shopGrafana{ids: kind == modelTold}
	g.fired("Zed", "zed", -80*time.Hour, -79*time.Hour) // the history reaches back past the backfill
	fire := func(f modelFiring) {
		truth = append(truth, f)
		extra := 0
		if f.unheld {
			extra = 40
			unheld[f.title] = append(unheld[f.title], f.s)
		}
		g.note(shopNote{uid: uids[f.title], title: f.title, prev: "Normal", next: "Alerting", service: f.service, at: now.Add(f.s), extra: extra})
		changes[f.title] = append(changes[f.title], f.s)
		if !f.open {
			g.note(shopNote{uid: uids[f.title], title: f.title, prev: "Alerting", next: "Normal", service: f.service, at: now.Add(f.e), extra: extra})
			changes[f.title] = append(changes[f.title], f.e)
		}
	}
	random := func(title, svc string, from, to time.Duration) {
		tm := from + time.Duration(rng.Intn(40))*10*time.Minute + 3*time.Minute
		for tm < to {
			dur := time.Duration([]int{4, 14, 24, 44, 64, 94, 124, 184, 304, 604}[rng.Intn(10)]) * time.Minute
			f := modelFiring{title: title, service: svc, s: tm, e: tm + dur, open: rng.Intn(8) == 0}
			fire(f)
			if f.open {
				break
			}
			tm = f.e + time.Duration([]int{6, 16, 36, 66, 126, 246, 486}[rng.Intn(7)])*time.Minute
		}
	}
	for _, title := range titles {
		switch {
		case title == "Disk full":
			fire(modelFiring{title: title, service: "a", s: -247 * time.Minute, open: true})
		case title == "Orders slow" && kind == modelWeek:
			// Left out from the first round on, for longer than a week: on
			// since before it, open and ended before what is read back of
			// the week, ended where it was left out; ended within that week,
			// ended there; begun within it, more than a week before and on
			// through it, never seen; begun within it after, written from
			// where it is judged again.
			for _, f := range []modelFiring{
				{service: "a", s: -197 * time.Minute, open: true},
				{service: "b", s: -147 * time.Minute, e: 20*time.Hour + 7*time.Minute},
				{service: "c", s: -97 * time.Minute, e: weekEnd - modelWeekLong + 30*time.Hour + 7*time.Minute},
				{service: "d", s: 10*time.Hour + 3*time.Minute, open: true},
				{service: "e", s: weekEnd - 2*modelDay + 3*time.Minute, open: true},
				{service: "f", s: 12*time.Hour + 3*time.Minute, e: weekEnd - 3*modelDay + 7*time.Minute},
				{service: "g", s: 14*time.Hour + 3*time.Minute, e: weekEnd + 2*modelDay + 7*time.Minute},
			} {
				f.title = title
				fire(f)
			}
			for _, svc := range []string{"x", "y"} {
				random(title, svc, weekEnd+26*time.Hour, end)
			}
		default:
			for _, svc := range []string{"a", "b", "c"} {
				random(title, svc, -300*time.Minute, end)
			}
		}
	}
	// Each title's beat, which keeps a change of it in every read: a title
	// shared is then one whose changes the history doesn't say the rule of.
	if beating {
		for _, title := range titles {
			for at := -300 * time.Minute; at < end; at += 15 * time.Minute {
				g.firedBy(uids[title], title, "beat", at+time.Minute, at+3*time.Minute)
				changes[title] = append(changes[title], at+time.Minute, at+3*time.Minute)
			}
		}
	}
	// Between two rounds, now and then: a change of a rule to a state
	// Grafana has no name for, and a firing of labels no write holds, of
	// Disk full and, firing, of Stock low.
	prev := time.Duration(0)
	for _, T := range rounds {
		if gap := int((T - prev) / (10 * time.Minute)); gap >= 2 {
			at := func() time.Duration { return prev + time.Duration(rng.Intn(gap-1))*10*time.Minute + 5*time.Minute }
			if disk && rng.Intn(10) < 2 {
				u := at()
				g.note(shopNote{uid: uids["Disk full"], title: "Disk full", prev: "Normal", next: "Firing", service: "odd", at: now.Add(u)})
				bad["Disk full"] = append(bad["Disk full"], u)
				changes["Disk full"] = append(changes["Disk full"], u)
			}
			if disk && rng.Intn(10) < 2 {
				u := at()
				fire(modelFiring{title: "Disk full", service: "wide", s: u, e: u + 4*time.Minute, unheld: true})
			}
			if rng.Intn(20) == 0 {
				u := at()
				fire(modelFiring{title: "Stock low", service: "wide", s: u, e: u + 4*time.Minute, unheld: true})
			}
		}
		prev = T
	}
	// Of a history that says each change's rule: between two rounds, now
	// and then, a rule of Orders slow's title made in another folder fires
	// for its services, the labels of its alerts, and is deleted before the
	// round after; no read of the rules sees it. And a rule of the title in
	// a folder the token doesn't read fires now and then throughout.
	if kind == modelTold {
		prev := time.Duration(0)
		for i, T := range rounds {
			if T-prev > 10*time.Minute && rng.Intn(10) < 4 {
				svc := []string{"a", "b", "c"}[rng.Intn(3)]
				s := prev + time.Duration(1+rng.Intn(int((T-prev)/time.Minute)-5))*time.Minute
				e := s + time.Duration(1+rng.Intn(int((T-s)/time.Minute)-1))*time.Minute
				if rng.Intn(4) == 0 {
					e = 0 // on still once it is deleted
				}
				g.firedBy(fmt.Sprintf("made-%d", i), "Orders slow", svc, s, e)
			}
			prev = T
		}
		g.hidden = []shopRule{{"hidden", "Orders slow", "Team B", false}}
		for range 2 + rng.Intn(4) {
			s := time.Duration(rng.Intn(int(end/time.Minute)))*time.Minute + 30*time.Second
			g.firedBy("hidden", "Orders slow", []string{"a", "b", "c"}[rng.Intn(3)], s, s+time.Duration(5+rng.Intn(60))*time.Minute)
		}
	}

	// The rules held at each round: a second rule of a title, paused, in
	// another folder, now and then.
	hold := func(dup map[string]bool) {
		var rs []shopRule
		for _, title := range titles {
			rs = append(rs, shopRule{uids[title], title, "Ops", false})
		}
		for _, title := range titles {
			if dup[title] {
				rs = append(rs, shopRule{"dup-" + uids[title], title, "Elsewhere", true})
			}
		}
		g.hold(rs...)
	}
	dup := map[string]bool{}
	hold(dup)

	ts := httptest.NewServer(g)
	t.Cleanup(ts.Close)
	sh := &shop{t: t, g: g, url: ts.URL, tok: tokenFile(t, "glsa_viewer")}
	cfg := func(was *Server) func(*Config) {
		return func(c *Config) {
			sh.config(was)(c)
			c.Settle = settle
		}
	}
	sh.s, _ = server(t, cfg(nil))
	defer func() { sh.s.Close() }()
	sh.round(0)

	// The model: what the copies cover (src), what each rule was left out
	// of (holes), the rounds each was judged in, the rules judged again past
	// a week, and the versions of the rules, each from its round on, with
	// the titles shared then.
	src := modelSpansOf(t, sh, "no such rule")
	if len(src) == 0 || src[len(src)-1][1] != 0 {
		return fmt.Sprintf("seed %d: after the first round the copies cover %s", seed, src)
	}
	type version struct {
		since  time.Duration
		shared map[string]bool
	}
	versions := []version{{0, map[string]bool{}}}
	covEnd, past := time.Duration(0), time.Duration(0)
	holes := map[string]modelSet{}
	judged := map[string][]time.Duration{}
	returns := map[string][]modelReturn{}
	for _, title := range titles {
		judged[title] = []time.Duration{0}
	}
	var log []string
	say := func(format string, a ...any) {
		log = append(log, fmt.Sprintf(format, a...))
		if loud {
			t.Logf(format, a...)
		}
	}
	say("seed %d: kind %d, settle %s, rules %q, beat %v", seed, kind, settle, titles, beating)
	for _, f := range truth {
		say("  fired %s/%s %s %s open=%v unheld=%v", f.title, f.service, f.s, f.e, f.open, f.unheld)
	}
	for title, us := range bad {
		say("  %s: changes to a state Grafana has no name for at %v", title, us)
	}

	expect := func(T time.Duration) []string {
		var want []string
		for _, f := range truth {
			if f.unheld || f.s >= T {
				continue
			}
			cover := src.minus(holes[f.title])
			start := f.s
			if !cover.has(f.s) {
				// Written from where the copies cover its rule again, if on
				// there; not, where that is past a week after its rule was
				// left out, and it began more than a week before and was on
				// through the week read back.
				g1, ok := cover.next(f.s)
				if !ok || !f.open && f.e <= g1 {
					continue
				}
				if slices.ContainsFunc(returns[f.title], func(r modelReturn) bool { return r.a == g1 && f.s < r.rs && (f.open || f.e > r.T) }) {
					continue
				}
				start = g1
			}
			e := "on"
			if !f.open && slices.ContainsFunc(judged[f.title], func(j time.Duration) bool { return j >= f.e }) {
				e = f.e.String()
			}
			// On where its rule was left out, judged again past a week, and
			// changing state in none of the week read back: ended where its
			// rule was left out.
			for _, r := range returns[f.title] {
				if start < r.h0 && (f.open || f.e > r.h0) && (f.open || f.e < r.rs || f.e > r.T) {
					e = r.h0.String()
				}
			}
			want = append(want, fmt.Sprintf("%s/%s %s %s", f.title, f.service, start, e))
		}
		slices.Sort(want)
		return want
	}
	check := func(T time.Duration) string {
		want := expect(T)
		got := slices.DeleteFunc(sh.copied(), func(s string) bool { return strings.HasPrefix(s, "Zed/") })
		var missing, extra, bad []string
		for _, w := range want {
			if !slices.Contains(got, w) {
				missing = append(missing, w)
			}
		}
		for _, x := range got {
			if !slices.Contains(want, x) {
				extra = append(extra, x)
			}
		}
		if len(missing)+len(extra) > 0 {
			bad = append(bad, fmt.Sprintf("at %s: missing %q, extra %q", T, missing, extra))
		}
		for _, title := range titles {
			if g, w := modelSpansOf(t, sh, title).String(), src.minus(holes[title]).String(); g != w {
				bad = append(bad, fmt.Sprintf("at %s: %s's copies cover %s, want %s", T, title, g, w))
			}
		}
		return strings.Join(bad, "\n")
	}
	if bad := check(0); bad != "" {
		return fmt.Sprintf("seed %d (before any rule is left out): %s\n%s", seed, bad, strings.Join(log, "\n"))
	}

	for _, T := range rounds {
		for _, title := range []string{"Orders slow", "Stock low"} {
			switch {
			case kind == modelLong && title == "Orders slow" && T < 26*time.Hour+time.Duration(seed%5)*time.Hour:
				// Left out for longer than a day from the first round on.
				dup[title] = true
			case kind == modelWeek && title == "Orders slow":
				// Left out for longer than a week from the first round on.
				dup[title] = T < weekEnd
			case rng.Intn(100) < 35:
				dup[title] = !dup[title]
			}
		}
		restart := rng.Intn(100) < 15
		if restart {
			was := sh.s
			if err := was.Close(); err != nil {
				t.Fatal(err)
			}
			sh.s, _ = server(t, cfg(was))
			past = 0
		}
		hold(dup)
		versions = append(versions, version{T, map[string]bool{"Orders slow": dup["Orders slow"], "Stock low": dup["Stock low"]}})
		a, first := covEnd, false
		if past > covEnd {
			a, first = past, true
		}
		p := a - settle
		read := p - time.Millisecond
		if first {
			p, read = a, a-modelDay
		}
		// A title is shared, of a history that says no rule's id, where a
		// version of the rules in effect across what the round writes,
		// from the settle before it, has it twice: its rule is left out
		// where a read holds a change of the title.
		twice := map[string]bool{}
		for i, v := range versions {
			to := time.Duration(1 << 62)
			if i+1 < len(versions) {
				to = versions[i+1].since
			}
			for title, two := range v.shared {
				if two && kind != modelTold && v.since <= T && to > p {
					twice[title] = true
				}
			}
		}
		shared := func(title string, from time.Duration) bool { return twice[title] && modelIn(changes[title], from, T) }
		unsure := func(title string, from time.Duration) bool { return modelIn(bad[title], from, T) }
		// The day's read, then the read back from where the rules judged
		// again were left out, a day before a at least and a week at most;
		// a rule that read finds unsure, and the day's didn't, read from a
		// day before a alone.
		leftDay := map[string]bool{}
		far := read
		pastFrom := map[string]time.Duration{}
		for _, title := range titles {
			leftDay[title] = shared(title, read) || unsure(title, read)
		}
		for _, title := range titles {
			i := slices.IndexFunc(holes[title], func(h [2]time.Duration) bool { return h[0] < a && h[1] >= a })
			if i < 0 || leftDay[title] {
				continue
			}
			h0 := holes[title][i][0]
			from := min(a-modelDay, h0-time.Millisecond)
			if from < a-modelWeekLong {
				from, pastFrom[title] = a-modelWeekLong, h0
			}
			far = min(far, from)
		}
		left := map[string]bool{}
		for _, title := range titles {
			left[title] = leftDay[title]
			if far < read {
				sh, un := shared(title, far), unsure(title, far)
				if near := min(a-modelDay, read); un && !leftDay[title] && near > far {
					sh, un = shared(title, near), unsure(title, near)
				}
				left[title] = sh || un
			}
			if modelIn(unheld[title], p, T) {
				left[title] = true
			}
		}
		var out []string
		for _, title := range titles {
			if left[title] {
				out = append(out, title)
			}
		}
		say("  round at %s: restart %v, held twice %v, shared %v, left out %q, copies [%s, %s), read back from %s", T, restart, dup, twice, out, a, T, far)
		sh.round(T)
		for _, title := range out {
			holes[title] = holes[title].add([2]time.Duration{a, T})
		}
		if len(out) == len(titles) {
			past = T
		} else {
			src = src.add([2]time.Duration{a, T})
			covEnd = T
			for _, title := range titles {
				if left[title] {
					continue
				}
				judged[title] = append(judged[title], T)
				if h0, ok := pastFrom[title]; ok {
					returns[title] = append(returns[title], modelReturn{h0, a, a - modelWeekLong, T})
				}
			}
		}
		if bad := check(T); bad != "" {
			return fmt.Sprintf("seed %d: %s\n%s\n  ledger: %q", seed, bad, strings.Join(log, "\n"), sh.copied())
		}
	}
	return ""
}

// TestServeGrafanaJudgedModel sets what the copies of Grafana's history
// hold beside the model, round by round, over schedules of fixed seeds: 48
// and those of modelShort, or modelShort alone with -short.
func TestServeGrafanaJudgedModel(t *testing.T) {
	if s := os.Getenv("LAPILLI_MODEL_SEED"); s != "" {
		seed, _ := strconv.ParseInt(s, 10, 64)
		if bad := modelRun(t, seed, true); bad != "" {
			t.Error(bad)
		}
		return
	}
	var seeds []int64
	n := 48
	if s := os.Getenv("LAPILLI_MODEL_SEEDS"); s != "" {
		n, _ = strconv.Atoi(s)
	}
	for seed := int64(1); seed <= int64(n); seed++ {
		seeds = append(seeds, seed)
	}
	switch {
	case testing.Short():
		seeds = modelShort
	case os.Getenv("LAPILLI_MODEL_SEEDS") == "":
		for _, seed := range modelShort {
			if !slices.Contains(seeds, seed) {
				seeds = append(seeds, seed)
			}
		}
	}
	fails := 0
	for _, seed := range seeds {
		if bad := modelRun(t, seed, false); bad != "" {
			if fails++; fails <= 3 {
				t.Error(bad)
			}
		}
	}
	if fails > 0 {
		t.Errorf("%d of %d schedules differ from the model", fails, len(seeds))
	}
}

// modelShort are the schedules run with -short: of each kind, with and
// without the beat, and among them one each of those that differ from the
// model where the code is as it was before a fix of 0.69.2's (seed: fix):
// 26, a rule of a judged rule's title in a folder the token doesn't read
// taken for it, and a change taken for a judged rule by its title alone,
// of a history that says each change's rule; 7, a firing no write holds
// leaving its rule out for as long as a read back holds it, a rule judged
// again by a day's read ending its alerts where it was left out, and no
// read back from where a rule was left out; 97, a firing written where its
// rule is judged again and ended before the next round reads taken away by
// that round.
var modelShort = []int64{1, 2, 4, 7, 8, 12, 26, 97, 172}
