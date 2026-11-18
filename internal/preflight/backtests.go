package preflight

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/serve"
	"lapilli.dev/internal/signin"
)

// The backtest lane (docs/design/backtests.md), with --backtest-listen: its
// trust file as serve read it, its issuers' discovery documents and keys,
// asked as a token's first check asks them, and, in the estimates, its
// share of --max-rate, taken out of what the runs have while it runs, and
// what a backtest can ask in --backtest-timeout.

// mostIssuers is how many issuers are asked, at most: a trust file holds a
// hundred entries at most, and a preflight asks a few dozen requests.
const mostIssuers = 20

// issuer is an issuer of the trust file's, and the keys' URL its entries
// give (jwks_url, "" for the discovery document's): one signin.CI in
// serve, which every entry naming it shares (newBacktestPort).
type issuer struct {
	url, jwks string
	entries   []string
}

// discovery is where an issuer's discovery document is, as signin.CI
// asks for it.
func (x issuer) discovery() string {
	return strings.TrimSuffix(x.url, "/") + "/.well-known/openid-configuration"
}

// issuers are the trust file's issuers, as serve asks them, in the order
// the file names them.
func (p *run) issuers() []issuer {
	var out []issuer
	at := map[[2]string]int{}
	for _, t := range p.c.Serve.Trust {
		if t.Issuer == "" {
			continue
		}
		k := [2]string{t.Issuer, t.JWKSURL}
		i, ok := at[k]
		if !ok {
			i = len(out)
			at[k] = i
			out = append(out, issuer{url: t.Issuer, jwks: t.JWKSURL})
		}
		out[i].entries = append(out[i].entries, t.Name)
	}
	return out
}

// backtesting reports whether the flags take pull request backtests, and
// serve reads a trust file for them.
func (p *run) backtesting() bool { return p.c.Serve.BacktestListen != "" }

// allowIssuers allows, for each issuer of the trust file serve read,
// exactly its discovery document, and the keys at the jwks_url its
// entries give: GETs of those URLs, nothing under them. The keys a
// discovery document names are allowed once it is read (backtestIssuers),
// held to the issuer's own origin. Each is counted as backtest, before any
// wider read that holds it.
func (p *run) allowIssuers() {
	if !p.backtesting() || p.c.TrustErr != nil {
		return
	}
	for i, x := range p.issuers() {
		if i == mostIssuers {
			break
		}
		p.allowURLAt("backtest", x.discovery(), true)
		if x.jwks != "" {
			p.allowURLAt("backtest", x.jwks, true)
		}
	}
}

// hideTrust has what is printed redact the values --backtest-trust pins
// and the audience its tokens are for: a repository's id, sub, an owner's
// id, the backtest port's own URL. Lapilli's words name the claims, never
// their values; what serve.ReadTrust said in refusing the file may hold
// them, so they are hidden in what was said, as a short name a server
// says is. They are read from the file as YAML, whatever serve made of
// it, so that those of a file it refuses are hidden too. The event a job
// runs for (event_name, pipeline_source) is one of a few words, which the
// refusal names, and isn't hidden.
func (p *run) hideTrust() {
	if !p.backtesting() || p.c.TrustFile == "" {
		return
	}
	f, err := os.Open(p.c.TrustFile)
	if err != nil {
		return
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return
	}
	var doc yaml.Node
	if yaml.Unmarshal(b, &doc) != nil {
		return
	}
	hide := func(what string, n *yaml.Node) {
		var vs []*yaml.Node
		switch n.Kind {
		case yaml.ScalarNode:
			vs = []*yaml.Node{n}
		case yaml.SequenceNode:
			vs = n.Content
		}
		for _, v := range vs {
			if v.Kind == yaml.ScalarNode && v.Value != "" {
				// As written, and as ReadTrust quotes it (%q).
				for _, as := range []string{v.Value, strings.Trim(strconv.Quote(v.Value), `"`)} {
					p.hideSaid("--backtest-trust's "+what, as)
					p.trustHidden = append(p.trustHidden, [2]string{"--backtest-trust's " + what, as})
				}
			}
		}
	}
	for _, entry := range yamlEntries(&doc) {
		for i := 0; i+1 < len(entry.Content); i += 2 {
			k, v := entry.Content[i], entry.Content[i+1]
			switch {
			case k.Value == "audience":
				hide("audience", v)
			case k.Value == "claims" && v.Kind == yaml.MappingNode:
				for j := 0; j+1 < len(v.Content); j += 2 {
					if name := v.Content[j].Value; name != "event_name" && name != "pipeline_source" {
						hide(name, v.Content[j+1])
					}
				}
			}
		}
	}
}

// yamlEntries are the mappings under a trust file's trust:, as the YAML
// reads, whatever else it holds.
func yamlEntries(doc *yaml.Node) []*yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
		doc = doc.Content[0]
	}
	if doc.Kind != yaml.MappingNode {
		return nil
	}
	var out []*yaml.Node
	for i := 0; i+1 < len(doc.Content); i += 2 {
		if doc.Content[i].Value != "trust" || doc.Content[i+1].Kind != yaml.SequenceNode {
			continue
		}
		for _, e := range doc.Content[i+1].Content {
			if e.Kind == yaml.MappingNode {
				out = append(out, e)
			}
		}
	}
	return out
}

// trustSaidMost is how much of why serve refuses a trust file is said, in
// bytes: ReadTrust's own words, the file's path and an entry's name among
// them, and what YAML says of each line it can't read.
const trustSaidMost = 600

// trustSaid is why serve.ReadTrust refused the trust file, as serve says
// it, on one line: its own words, not a server's, so not cut as one is,
// once what the file pins is taken out of it (hideTrust), which a value
// it can't take is said by.
func (p *run) trustSaid(err error) string {
	why := strings.Join(strings.Fields(err.Error()), " ")
	if p.c.Scrubber != nil {
		why = p.c.Scrubber(p.trustHidden...)(why)
	}
	return clip(why, trustSaidMost)
}

// backtest says what serve does with the backtest port's flags and trust
// file as it starts, and asks each issuer the file names for its
// discovery document and keys.
func (p *run) backtest(ctx context.Context) {
	if !p.backtesting() {
		return
	}
	p.backtestTrust()
	p.backtestIssuers(ctx)
}

// backtestTrust is what serve does at start with --backtest-trust, as
// serve.ReadTrust read it in reading the flags (cmd/lapilli's
// readServeAs), and with the backtest port's flags (serve.BacktestRefused):
// it starts, or stops, saying why. The entries are said by their names,
// their CI and the claims they pin, never what each claim is pinned to.
func (p *run) backtestTrust() {
	s := p.c.Serve
	if err := p.c.TrustErr; err != nil {
		p.add(Item{ID: "backtest.trust", State: Problem, What: "serve doesn't start: it refuses --backtest-trust", Why: p.trustSaid(err),
			Fix: "serve stops at start on a trust file it can't use, as one half read could let in whom it shouldn't: " +
				"each entry as docs/design/backtests.md, \"Who may ask\", says"})
		return
	}
	if err := serve.BacktestRefused(s, p.historyBase() != ""); err != nil {
		p.add(Item{ID: "backtest.trust", State: Problem, What: "serve doesn't start: it refuses the backtest port", Why: err.Error(),
			Fix: "serve stops at start with these flags"})
		return
	}
	var each, unread []string
	for _, t := range s.Trust {
		if t.TokenFile != "" {
			if p.c.Offline {
				each = append(each, t.Name+": a plain token, its file not read")
				unread = append(unread, t.Name)
			} else {
				each = append(each, t.Name+": a plain token, its file read, long enough and no other token's")
			}
			continue
		}
		ci := t.CI
		if ci == "" {
			ci = "generic"
		}
		var pins []string
		for _, name := range slices.Sorted(maps.Keys(t.Claims)) {
			if pin := t.Claims[name]; pin.List {
				pins = append(pins, fmt.Sprintf("%s (any of %d)", name, len(pin.Values)))
			} else {
				pins = append(pins, name)
			}
		}
		each = append(each, fmt.Sprintf("%s: %s, pins %s", t.Name, ci, and(pins)))
	}
	it := Item{ID: "backtest.trust", State: OK, What: fmt.Sprintf("serve takes its %s: %s", plural(len(s.Trust), "entry"), strings.Join(each, "; "))}
	if len(unread) > 0 {
		it.State = Unknown
		it.Why = "--offline: the plain tokens' files (" + strings.Join(unread, ", ") + ") are where serve runs; " +
			"serve stops at start unless each holds a token of 32 characters at least, which no other token file holds"
	}
	p.add(it)
}

// backtestIssuers asks each issuer of the trust file serve read for its
// discovery document and its keys, once, as a token's first check asks
// them (signin.CI): its issuer to the byte, its keys at its own origin or
// at the entry's jwks_url, no redirect, 1 MB and 100 keys, ten seconds,
// within the preflight's time. Each is ok when a key it holds verifies a
// token of an algorithm its discovery document names; a problem when it
// can't be read, or none does.
func (p *run) backtestIssuers(ctx context.Context) {
	if p.c.TrustErr != nil {
		p.add(Item{ID: "backtest.issuers", State: Unknown, What: "not asked: serve refuses --backtest-trust (backtest.trust)"})
		return
	}
	iss := p.issuers()
	if len(iss) == 0 {
		return
	}
	if p.c.Offline {
		p.offline("backtest.issuers", plural(len(iss), "issuer")+" not asked for their discovery documents and keys")
		return
	}
	var ok, bad, unasked []string
	for i, x := range iss {
		of := " (" + strings.Join(x.entries, ", ") + ")"
		if i >= mostIssuers || ctx.Err() != nil {
			unasked = append(unasked, x.url+of)
			continue
		}
		n, algs, err := p.issuerKeys(ctx, x)
		switch {
		case err != nil && ctx.Err() != nil:
			// The preflight's time, not the issuer's answer.
			unasked = append(unasked, x.url+of)
		case err != nil:
			bad = append(bad, x.url+of+": "+said(err))
		case n == 0:
			bad = append(bad, fmt.Sprintf("%s%s: no key its JWKS holds verifies a token of %s", x.url, of, strings.Join(algs, ", ")))
		default:
			ok = append(ok, fmt.Sprintf("%s%s, %s for %s", x.url, of, plural(n, "key"), strings.Join(algs, ", ")))
		}
	}
	it := Item{ID: "backtest.issuers", State: OK, What: "discovery and keys read: " + strings.Join(ok, "; ")}
	if len(ok) == 0 {
		it.What = "no issuer's keys read"
	}
	switch {
	case len(bad) > 0:
		it.State, it.Why = Problem, strings.Join(bad, "; ")
		it.Fix = "serve starts, and refuses these entries' tokens (401) until the issuer answers as a CI's should: check issuer and jwks_url, and that the pod reaches it"
	case len(unasked) > 0:
		it.State = Unknown
	}
	if len(unasked) > 0 {
		it.Why = strings.TrimPrefix(it.Why+fmt.Sprintf("; not asked, past %d issuers or --preflight-timeout: %s", mostIssuers, strings.Join(unasked, "; ")), "; ")
	}
	p.add(it)
}

// issuerKeys reads an issuer's discovery document and keys: how many keys
// verify a token of the algorithms it names, and those. The keys a
// discovery document names are allowed only at the issuer's own origin, as
// signin.CI takes them, or where the entry's jwks_url says: a request to
// anywhere else is refused before it is sent.
func (p *run) issuerKeys(ctx context.Context, x issuer) (int, []string, error) {
	each, cancel := context.WithTimeout(ctx, eachTime)
	defer cancel()
	ci := &signin.CI{Issuer: x.url, JWKS: x.jwks, Client: signin.Client(nil), Now: p.c.Now}
	keys, err := ci.DiscoverKeys(each)
	if err != nil {
		return 0, nil, err
	}
	if at := keys.URL(); keysAllowed(x, at) {
		p.allowURLAt("backtest", at, true)
	}
	n, err := keys.Usable(each)
	return n, keys.Algs(), err
}

// keysAllowed reports whether the keys an issuer's discovery document
// names, at, are allowed: where its entries give no jwks_url, at the
// issuer's own origin, https or, as signin takes an issuer's URL, http to
// this machine or a name inside a cluster. Keys at a jwks_url are allowed
// as it is given (allowIssuers), and none elsewhere.
func keysAllowed(x issuer, at string) bool {
	return x.jwks == "" && signin.SafeURL(at) && signin.SameOrigin(at, x.url)
}

// laneShare is --backtest-share, as serve takes it.
func (p *run) laneShare() float64 {
	if f := p.c.Serve.BacktestShare; f > 0 {
		return f
	}
	return 0.25
}

// laneRate is the lane's requests a second, as serve paces it: its share
// of --max-rate's pace (prom.Pace.Share); 0 without --max-rate.
func (p *run) laneRate() float64 {
	return prom.NewPace(p.c.MaxRate).Share(p.laneShare()).Rate()
}

// laneParallel is how many requests the lane asks at once: a quarter of
// --parallel, one at least (newBacktestPort).
func (p *run) laneParallel() int {
	parallel := p.c.Serve.Parallel
	if parallel <= 0 {
		parallel = 8
	}
	return max(1, parallel/4)
}

// laneTimeout is --backtest-timeout, as serve takes it.
func (p *run) laneTimeout() time.Duration {
	if d := p.c.Serve.BacktestTimeout; d > 0 {
		return d
	}
	return 15 * time.Minute
}

// The bounds a backtest is held to, as the port holds them: 1 to 14 days,
// 7 when the job gives none (the recipe's since); and the rules that
// differ the estimate counts, ten as in docs/design/backtests.md's "What
// it costs".
const (
	laneDays, laneDefaultDays = 14, 7
	laneRules                 = 10
)

// whileBacktest adds, to the hourly runs' item, how long their requests
// take while the lane runs a backtest the whole hour: --max-rate less the
// share kept for the lane (prom.Pace.Reserve), for the requests to
// --prometheus, which the lane asks too; the other APIs' at their rate.
// A run that then takes longer than --every is unknown, not a problem:
// whether backtests run back to back, hour after hour, is the pull
// requests' to say.
func (p *run) whileBacktest() {
	h := p.r.Estimates.Hourly
	if !p.backtesting() || h == nil || p.c.MaxRate <= 0 {
		return
	}
	s := p.c.Serve
	share := p.laneShare()
	runs := p.c.MaxRate * (1 - share)
	var took, longest time.Duration
	for _, by := range p.runsBy {
		t := time.Duration(0)
		for k, n := range by {
			r := p.c.MaxRate
			if k == "prometheus" {
				r = runs
			}
			t = max(t, time.Duration(float64(n)/r*float64(time.Second)))
		}
		took += t
		longest = max(longest, t)
	}
	if len(p.runsBy) == 0 {
		return
	}
	w := &WhileBacktest{Rate: runs, Seconds: longest.Seconds()}
	if b := p.r.Estimates.Beside; b != nil || h.Day > 0 {
		day := h.Day
		if b != nil {
			day += b.Day
		}
		w.Share = float64(day) / (86400 * runs)
	}
	h.WhileBacktest = w
	it := itemByID(p, "estimate.hourly")
	if it == nil {
		return
	}
	it.What += fmt.Sprintf("; while a backtest runs the whole hour, the runs have %s a second, --backtest-share %s of it kept for the lane: a run takes %s at least, the most %s",
		rate(runs), rate(share), dur(took/time.Duration(len(p.runsBy))), dur(longest))
	if w.Share > 0 {
		it.What += fmt.Sprintf(", %s of that once caught up", percent(w.Share))
	}
	if it.State == OK && longest > s.Every {
		it.State = Unknown
		it.Why = strings.TrimPrefix(it.Why+fmt.Sprintf("; while backtests run back to back, a run's requests take longer than --every %s: the runs fall behind for as long as they do, and whether they will is the pull requests' to say",
			dur(s.Every)), "; ")
		it.Fix = "raise --max-rate, or lower --backtest-share, or give fewer rules to replay (--rule)"
	}
}

// and is a list of a few words as a sentence has it: a, b and c.
func and(ws []string) string {
	if len(ws) < 2 {
		return strings.Join(ws, "")
	}
	return strings.Join(ws[:len(ws)-1], ", ") + " and " + ws[len(ws)-1]
}

// itemByID is the item of an id the run added, nil for none.
func itemByID(p *run, id string) *Item {
	for i := range p.r.Items {
		if p.r.Items[i].ID == id {
			return &p.r.Items[i]
		}
	}
	return nil
}

// changedRules are the rules the estimate's backtest changes: ten of those
// read that the port takes, in Prometheus's format and replayed, each of
// an expression of its own, so that none shares a read with another, in
// the order read.
func changedRules(rs []rules.Rule) []rules.Rule {
	var out []rules.Rule
	seen := map[string]bool{}
	for _, r := range rs {
		if len(out) == laneRules {
			break
		}
		if r.Graph != nil || r.Logs != "" || r.Unsupported != "" || seen[r.Expr] {
			continue
		}
		seen[r.Expr] = true
		out = append(out, r)
	}
	return out
}

// backtests works out what a pull request's backtest can ask within
// --backtest-timeout at the lane's rate, with the planner serve's port
// counts one with before it is queued (fits: replay.PlanEach, the rules
// before and after it side by side, over days ending at now less
// --settle, on --step): ten of the rules read changed, each rule's
// expression (the two sides' reads apart) or its for alone (the two
// sides sharing their reads, as RunEach shares them), over the recipe's
// seven days, and the most days of each that fit.
func (p *run) backtests(ctx context.Context, rd *read) {
	if !p.backtesting() {
		return
	}
	s := p.c.Serve
	limit, laneAt := p.laneTimeout(), p.laneRate()
	b := &Backtests{Share: p.laneShare(), Rate: laneAt, Parallel: p.laneParallel(), TimeoutSeconds: limit.Seconds(), Days: laneDefaultDays}
	took := func(n int) time.Duration { return time.Duration(float64(n) / laneAt * float64(time.Second)) }
	fits := func(n int) bool { return laneAt <= 0 || took(n) <= limit }
	if laneAt > 0 {
		for b.Fit = int(laneAt * limit.Seconds()); b.Fit > 0 && !fits(b.Fit); b.Fit-- {
		}
		for fits(b.Fit + 1) {
			b.Fit++
		}
	}
	var rs []rules.Rule
	if rd != nil {
		rs = changedRules(rd.all)
	}
	b.Rules = len(rs)
	h := p.planned()
	plan := func(sets [][]replay.Version, days int) int {
		to := p.now.Add(-s.Settle).Truncate(s.Step)
		return replay.PlanEach(h, sets, to.Add(-time.Duration(days)*24*time.Hour), to, s.Step).Requests
	}
	changed := func(days int) int {
		// Each side alone: the two share no expression, so no read.
		return 2 * plan([][]replay.Version{{{Rules: rs}}}, days)
	}
	shared := func(days int) int {
		return plan([][]replay.Version{{{Rules: rs}}, {{Rules: rs}}}, days)
	}
	most := func(of func(int) int) int {
		d := laneDays
		for ; d >= 1 && ctx.Err() == nil; d-- {
			if fits(of(d)) {
				break
			}
		}
		return d
	}
	if len(rs) > 0 {
		b.Changed, b.Shared = changed(b.Days), shared(b.Days)
		b.ChangedDays, b.SharedDays = most(changed), most(shared)
	}
	if ctx.Err() != nil {
		p.unworked("estimate.backtests")
		return
	}
	p.r.Estimates.Backtests = b
	it := Item{ID: "estimate.backtests", State: OK}
	lane := fmt.Sprintf("the lane asks %d at once", b.Parallel)
	if laneAt > 0 {
		it.What = fmt.Sprintf("a backtest asks %s at most within --backtest-timeout %s, at the lane's %s a second (--backtest-share %s of --max-rate %s; %s)",
			plural(b.Fit, "request"), dur(limit), rate(laneAt), rate(b.Share), rate(p.c.MaxRate), lane)
	} else {
		it.What = fmt.Sprintf("without --max-rate, %s and keeps to no rate: how long a backtest takes is the API's to say, and none is refused for its time", lane)
		it.Why = fmt.Sprintf("one that doesn't end within --backtest-timeout %s ends failed", dur(limit))
	}
	if len(rs) == 0 {
		it.What += "; no rule read is one the port takes (Prometheus's format, replayed), so none is counted"
		p.add(it)
		return
	}
	of := "ten of these rules"
	switch {
	case len(rs) == 1:
		of = "one of these rules"
	case len(rs) < laneRules:
		of = fmt.Sprintf("%d of these rules", len(rs))
	}
	daysFit := func(d int) string {
		switch {
		case laneAt <= 0:
			return ""
		case d == laneDays:
			return fmt.Sprintf(": all %d days a backtest may ask fit", laneDays)
		case d < 1:
			return ": not a day fits"
		}
		if d == 1 {
			return ": a day fits"
		}
		return fmt.Sprintf(": %d days fit", d)
	}
	it.What += fmt.Sprintf("; %s changed ask %s over %d days, the recipe's default%s; with only their for changed, the two sides' reads shared, %s%s",
		of, num(b.Changed), b.Days, daysFit(b.ChangedDays), num(b.Shared), daysFit(b.SharedDays))
	if laneAt > 0 && b.SharedDays < 1 {
		it.State = Problem
		it.Why = "the port refuses every backtest before it is queued: not one rule changed over a day fits within --backtest-timeout at the lane's rate"
		it.Fix = "raise --max-rate or --backtest-share, or give --backtest-timeout longer"
	}
	p.add(it)
}

// laneMemory is what the lane's reads hold at once at most, beside the
// runs': a read of a pull request's rules, as many as it asks at once,
// each held to --max-samples.
func (p *run) laneMemory(most int) int64 {
	if !p.backtesting() {
		return 0
	}
	return int64(p.laneParallel()) * int64(most) * prom.SampleBytes
}
