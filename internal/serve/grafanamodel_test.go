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
// half days, a rule left out for longer than one; and, of a history that
// says each change's rule, rules of a judged rule's title held at rounds,
// and others made, firing and deleted between them.
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

// modelFiring is a firing of a judged rule, as Grafana's history has it.
type modelFiring struct {
	title, service string
	s, e           time.Duration
	open           bool
}

// The kinds of schedule.
const (
	modelUntold = iota // a history that says no rule's id
	modelLong          // the same, a rule left out for longer than a day
	modelTold          // a history that says each change's rule
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

// modelRun runs the schedule of seed, and says how it differs from the
// model, "" for not at all.
func modelRun(t *testing.T, seed int64, loud bool) string {
	rng := rand.New(rand.NewSource(seed))
	kind := int(seed % 3)
	settle := []time.Duration{time.Hour, time.Hour, 3 * time.Hour, 20 * time.Minute}[rng.Intn(4)]
	titles := []string{"Orders slow", "Stock low"}
	if kind == modelLong || rng.Intn(10) < 6 {
		// Over days, a third rule never shared, so that no day is a gap
		// for every rule: a first day after a gap is read back a day.
		titles = append(titles, "Cart full")
	}
	uids := map[string]string{"Orders slow": ordersUID, "Stock low": stockUID, "Cart full": "cart"}

	// The rounds.
	var rounds []time.Duration
	at, until, steps := time.Duration(0), 20*time.Hour, []time.Duration{20, 30, 40, 60, 60, 90, 120, 180, 240}
	n := 6 + rng.Intn(8)
	if kind == modelLong {
		until, steps, n = 60*time.Hour, []time.Duration{60, 120, 180, 240, 240}, 40
	}
	for len(rounds) < n {
		at += steps[rng.Intn(len(steps))] * time.Minute
		if at > until {
			break
		}
		rounds = append(rounds, at)
	}
	end := rounds[len(rounds)-1]

	// What the judged rules fired, under each title, for each service.
	var truth []modelFiring
	g := &shopGrafana{ids: kind == modelTold}
	g.fired("Zed", "zed", -80*time.Hour, -79*time.Hour) // the history reaches back past the backfill
	for _, title := range titles {
		for _, svc := range []string{"a", "b", "c"} {
			tm := -300*time.Minute + time.Duration(rng.Intn(40))*10*time.Minute + 3*time.Minute
			for tm < end {
				dur := time.Duration([]int{4, 14, 24, 44, 64, 94, 124, 184, 304, 604}[rng.Intn(10)]) * time.Minute
				f := modelFiring{title: title, service: svc, s: tm, e: tm + dur, open: rng.Intn(8) == 0}
				truth = append(truth, f)
				if f.open {
					g.firedBy(uids[title], title, svc, f.s, 0)
					break
				}
				g.firedBy(uids[title], title, svc, f.s, f.e)
				tm = f.e + time.Duration([]int{6, 16, 36, 66, 126, 246, 486}[rng.Intn(7)])*time.Minute
			}
		}
	}
	// Each title's beat, which keeps a change of it in every read: a title
	// shared is then one whose changes the history doesn't say the rule of.
	for _, title := range titles {
		for at := -300 * time.Minute; at < end; at += 15 * time.Minute {
			g.firedBy(uids[title], title, "beat", at+time.Minute, at+3*time.Minute)
		}
	}
	// Of a history that says each change's rule: between two rounds, now
	// and then, a rule of Orders slow's title made in another folder fires
	// for its services, the labels of its alerts, and is deleted before the
	// round after; no read of the rules sees it.
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
	// of (holes), the rounds each was judged in, and the versions of the
	// rules, each from its round on, with the titles shared then.
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
	say("seed %d: kind %d, settle %s, rules %q", seed, kind, settle, titles)
	for _, f := range truth {
		say("  fired %s/%s %s %s open=%v", f.title, f.service, f.s, f.e, f.open)
	}

	expect := func(T time.Duration) []string {
		var want []string
		for _, f := range truth {
			if f.s >= T {
				continue
			}
			cover := src.minus(holes[f.title])
			start := f.s
			if !cover.has(f.s) {
				// Written from where the copies cover its rule again, if on
				// there.
				g1, ok := cover.next(f.s)
				if !ok || !f.open && f.e <= g1 {
					continue
				}
				start = g1
			}
			e := "on"
			if !f.open && slices.ContainsFunc(judged[f.title], func(j time.Duration) bool { return j >= f.e }) {
				e = f.e.String()
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
		if first {
			p = a
		}
		// A title is shared, of a history that says no rule's id, where a
		// version of the rules in effect across what the round writes,
		// from the settle before it, has it twice.
		shared := map[string]bool{}
		for i, v := range versions {
			to := time.Duration(1 << 62)
			if i+1 < len(versions) {
				to = versions[i+1].since
			}
			for title, twice := range v.shared {
				if twice && kind != modelTold && v.since <= T && to > p {
					shared[title] = true
				}
			}
		}
		say("  round at %s: restart %v, held twice %v, shared %v, copies [%s, %s)", T, restart, dup, shared, a, T)
		sh.round(T)
		for title := range shared {
			holes[title] = holes[title].add([2]time.Duration{a, T})
		}
		if len(shared) == len(titles) {
			past = T
		} else {
			src = src.add([2]time.Duration{a, T})
			covEnd = T
			for _, title := range titles {
				if !shared[title] {
					judged[title] = append(judged[title], T)
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
// hold beside the model, round by round, over schedules of fixed seeds: 9
// with -short, 45 without.
func TestServeGrafanaJudgedModel(t *testing.T) {
	if s := os.Getenv("LAPILLI_MODEL_SEED"); s != "" {
		seed, _ := strconv.ParseInt(s, 10, 64)
		if bad := modelRun(t, seed, true); bad != "" {
			t.Error(bad)
		}
		return
	}
	n := 45
	if testing.Short() {
		n = 9
	}
	if s := os.Getenv("LAPILLI_MODEL_SEEDS"); s != "" {
		n, _ = strconv.Atoi(s)
	}
	fails := 0
	for seed := int64(1); seed <= int64(n); seed++ {
		if bad := modelRun(t, seed, false); bad != "" {
			if fails++; fails <= 3 {
				t.Error(bad)
			}
		}
	}
	if fails > 0 {
		t.Errorf("%d of %d schedules differ from the model", fails, n)
	}
}
