// Package preflight says what lapilli serve will meet before it is
// deployed (docs/design/preflight.md): whether it reads the rules, reaches
// the history and signs in to it, what its tokens and files hold, the
// ledger it would open, and how many requests, how long, how many nights
// and how much memory its runs will take. It writes nothing and replays
// nothing: every request it sends goes through an allowlist that refuses
// the rest before they are sent (allow.go), the ledger is read through a
// probe that opens it read-only (ledger.Probe), and no expression a person
// wrote is evaluated.
package preflight

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"lapilli.dev/internal/egress"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/serve"
	"lapilli.dev/internal/source"
	"lapilli.dev/internal/verdict"
)

// State is what an item says: ok, a problem, or unknown.
type State string

const (
	OK      State = "ok"
	Problem State = "problem"
	Unknown State = "unknown"
)

// Item is one thing checked, by an id that stays the same between
// releases (docs/numbers.md).
type Item struct {
	ID    string `json:"id"`
	State State  `json:"state"`
	What  string `json:"what"`
	Why   string `json:"why,omitempty"`
	Fix   string `json:"fix,omitempty"`
}

// Report is what a preflight found.
type Report struct {
	Version   int       `json:"version"`
	Lapilli   string    `json:"lapilli"`
	Items     []Item    `json:"items"`
	Estimates Estimates `json:"estimates"`
	LeftOut   []string  `json:"left_out"`
	Asked     Asked     `json:"asked"`
}

// Asked is every request the preflight sent, by the API asked; Refused,
// those the allowlist refused before they were sent, which is none unless
// lapilli has a bug.
type Asked struct {
	Requests int            `json:"requests"`
	By       map[string]int `json:"by"`
	Refused  int            `json:"refused"`
}

// Config is what serve's flags say, as serve read them, and the
// preflight's own.
type Config struct {
	Serve serve.Config
	// History is --prometheus or --gcp-project, as serve asks it; zero
	// without either. Loki is --loki's, as serve asks it.
	History source.Prometheus
	Loki    source.Loki
	// MaxRate is --max-rate, 0 for none; MaxSamples --max-samples, 0 for
	// the default.
	MaxRate    float64
	MaxSamples int
	// Offline asks nothing: what needs a request is unknown.
	Offline bool
	// Timeout is the whole preflight's (--preflight-timeout).
	Timeout time.Duration
	// Version is lapilli's; Now the time it is.
	Version string
	Now     func() time.Time
	// Scrubber is what takes out of what is printed what is redacted: the
	// URLs given, their hosts and users, addresses, tenants, a Google Cloud
	// project (as a pull request's comment, cmd/lapilli forComment); and
	// more, each a flag and a value, as the run learns them (a sign-in's
	// keys, an on-call feed, the ledger's server).
	Scrubber func(more ...[2]string) func(string) string
	// CgroupMemory is where the memory limit of the process is read, by
	// tests; "" for the cgroup's own files.
	CgroupMemory string
	// TrustFile is --backtest-trust; TrustErr why serve.ReadTrust refused
	// it as serve read the flags, which serve stops at, nil when it took
	// it (Serve.Trust its entries).
	TrustFile string
	TrustErr  error
}

// eachTime is how long one read is waited for.
const eachTime = 10 * time.Second

// Run checks what serve's flags say and reports it. It sets the
// allowlist as the process's guard (egress) while it runs: every request
// any client of lapilli sends meanwhile goes through it.
func Run(ctx context.Context, c Config) *Report {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Scrubber == nil {
		c.Scrubber = func(...[2]string) func(string) string { return func(s string) string { return s } }
	}
	if c.Timeout <= 0 {
		c.Timeout = 2 * time.Minute
	}
	al := newAllowlist(c.Now)
	release := egress.Guard(al.check)
	defer release()
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	p := &run{c: c, al: al, r: &Report{Version: 1, Lapilli: c.Version}, now: c.Now().UTC()}
	p.allowAll()
	p.hideScopes()
	p.hideIssuers()
	read := p.rules(ctx)
	p.expressions()
	p.history(ctx, read)
	p.grafana(ctx)
	p.changes(ctx)
	p.files(ctx)
	p.secrets()
	p.ledger(ctx)
	p.signin(ctx)
	p.backtest(ctx)
	p.access()
	p.estimate(ctx, read)
	p.r.Asked = al.asked()
	p.redact()
	return p.r
}

// run is one preflight under way.
type run struct {
	c   Config
	al  *allowlist
	r   *Report
	now time.Time
	// alerts are, by alert name, the samples each fired over the last
	// day, as history.alerts read them; nil when not read.
	alerts map[string]float64
	// h is the history's client, as serve asks it, or why it couldn't be
	// made (historyClient); ds the scrape intervals Grafana's data sources
	// say they have.
	h    *prom.Client
	hErr error
	ds   rules.DataSources
	// hidden are what the run learned is to be redacted, by flag;
	// hiddenSaid what is to be redacted of what a server said only.
	hidden, hiddenSaid [][2]string
	// docs are the documents the rules were read from, by where
	// (ruler, alarms:SOURCE): the alarms and the namespaces there are.
	docs map[string][]rules.Doc
	// runsBy are the requests of each of a day's hourly runs, by API, as
	// the estimate counted them: timed again at what the runs have while
	// the backtest lane runs (whileBacktest).
	runsBy []map[string]int
}

// hide has what is printed redact value, as of flag.
func (p *run) hide(flag, value string) {
	if value != "" {
		p.hidden = append(p.hidden, [2]string{flag, value})
	}
}

// hideSaid has what a server said redact value, as of flag, and nothing
// else: a name a server's answer repeats, as Postgres' refusal does the
// user's and the database's, which may well be a word of Lapilli's own
// (lapilli, postgres) that the rest of the report says.
func (p *run) hideSaid(flag, value string) {
	if value != "" {
		p.hiddenSaid = append(p.hiddenSaid, [2]string{flag, value})
	}
}

func (p *run) add(it Item) { p.r.Items = append(p.r.Items, it) }

// offline is the item of something that needs a request, with --offline.
func (p *run) offline(id, what string) {
	p.add(Item{ID: id, State: Unknown, What: what, Why: "--offline: nothing is asked"})
}

// redact readies every text of the report to be printed, here and only
// here, so that a check that puts a text from outside into an item
// without readying it (a rule's reason, a name, a note) can't print it as
// it came: each is shown as it reads first (shown), then what isn't to be
// printed is taken out of the whole text, then what a server said is cut,
// so that nothing to be redacted is cut in two before it is looked for.
// The text and the JSON are both written from what it leaves.
func (p *run) redact() {
	scrub, scrubSaid := p.c.Scrubber(p.hidden...), p.c.Scrubber(p.hiddenSaid...)
	done := func(s string) string { return cutSaid(inSaid(scrub(shown(s)), scrubSaid)) }
	for i := range p.r.Items {
		it := &p.r.Items[i]
		it.What, it.Why, it.Fix = done(it.What), done(it.Why), done(it.Fix)
	}
	for i := range p.r.LeftOut {
		p.r.LeftOut[i] = done(p.r.LeftOut[i])
	}
}

// What a server said is marked in a text until it is redacted, then cut
// (cutSaid). The marks hold a text drawn at random as the process starts,
// which nothing from outside can know: a mark a rule file or a server's
// answer holds is no mark, and is shown as text (shown), as any control
// character is.
var saidFrom, saidTo = marks()

// saidMost is how much of what a server said is printed, in bytes.
const saidMost = 200

func marks() (string, string) {
	n := rand.Text()
	return "\x00" + n + "\x01", "\x00" + n + "\x02"
}

// shown has every part of s but the marks shown as it reads
// (verdict.Clean): what turns text around or acts on a terminal, an
// escape sequence, a direction mark, a NUL, is written as text. What a
// server said was shown so as it was marked (said), and is the same shown
// again.
func shown(s string) string {
	from := strings.Split(s, saidFrom)
	for i, part := range from {
		to := strings.Split(part, saidTo)
		for j := range to {
			to[j] = verdict.Clean(to[j])
		}
		from[i] = strings.Join(to, saidTo)
	}
	return strings.Join(from, saidFrom)
}

// said is what an error, or a server's answer in it, says: what turns
// text around or acts on a terminal shown as text (verdict.Clean), on one
// line; once redacted, saidMost bytes of it at most.
func said(err error) string {
	if err == nil {
		return ""
	}
	return saidText(err.Error())
}

// saidText is a text a server gave, a name it answered with, as said has
// it: shown as it reads, on one line, and redacted as what a server said
// is (hideSaid).
func saidText(s string) string {
	return saidFrom + verdict.Clean(strings.ReplaceAll(strings.TrimSpace(s), "\n", "; ")) + saidTo
}

// inSaid is s with f applied to each part of it said marks, and to
// nothing else; the marks are kept.
func inSaid(s string, f func(string) string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, saidFrom)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i+len(saidFrom)])
		s = s[i+len(saidFrom):]
		j := strings.Index(s, saidTo)
		if j < 0 {
			j = len(s)
		}
		b.WriteString(f(s[:j]))
		s = s[j:]
	}
}

// cutSaid cuts each part of s said marks to saidMost bytes, and takes the
// marks out.
func cutSaid(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, saidFrom)
		if i < 0 {
			b.WriteString(strings.ReplaceAll(s, saidTo, ""))
			return b.String()
		}
		b.WriteString(strings.ReplaceAll(s[:i], saidTo, ""))
		s = s[i+len(saidFrom):]
		j := strings.Index(s, saidTo)
		if j < 0 {
			j = len(s)
		}
		b.WriteString(clip(strings.ReplaceAll(s[:j], saidFrom, ""), saidMost))
		s = s[min(j+len(saidTo), len(s)):]
	}
}

// clip cuts s to n bytes at most, on a character's edge.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// Problems and unknowns are counted in a report.
func (r *Report) Count() (ok, problems, unknown int) {
	for _, it := range r.Items {
		switch it.State {
		case OK:
			ok++
		case Problem:
			problems++
		default:
			unknown++
		}
	}
	return
}

// Exit is the exit status a report gives: 1 with a problem, or with
// failUnknown an unknown; 0 otherwise.
func (r *Report) Exit(failUnknown bool) int {
	_, problems, unknown := r.Count()
	if problems > 0 || failUnknown && unknown > 0 {
		return 1
	}
	return 0
}

// names lists a few names, shown as they read (verdict.Clean), and how
// many more.
func names(ns []string, most int) string {
	ns = slices.Clone(ns)
	for i := range ns {
		ns[i] = verdict.Clean(ns[i])
	}
	return listed(ns, most)
}

// listed lists a few texts already shown as they read, and how many more.
func listed(ns []string, most int) string {
	ns = slices.Clone(ns)
	slices.Sort(ns)
	ns = slices.Compact(ns)
	if len(ns) <= most {
		return strings.Join(ns, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(ns[:most], ", "), len(ns)-most)
}

// plural is n with its noun, the noun with an s for any n but one.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	if strings.HasSuffix(noun, "y") && !strings.HasSuffix(noun, "ey") {
		return fmt.Sprintf("%d %sies", n, strings.TrimSuffix(noun, "y"))
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// errOffline is what a read with --offline would have been.
var errOffline = errors.New("--offline: nothing is asked")

// replayable are the rules a run replays: those with no reason they
// can't be.
func replayable(rs []rules.Rule) []rules.Rule {
	var out []rules.Rule
	for _, r := range rs {
		if r.Unsupported == "" {
			out = append(out, r)
		}
	}
	return out
}
