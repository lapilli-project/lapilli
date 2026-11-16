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
	// Scrub takes out of what is printed what is redacted: the URLs given,
	// their hosts and users, addresses, tenants, a Google Cloud project
	// (as a pull request's comment, cmd/lapilli forComment).
	Scrub func(string) string
	// CgroupMemory is where the memory limit of the process is read, by
	// tests; "" for the cgroup's own files.
	CgroupMemory string
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
	if c.Scrub == nil {
		c.Scrub = func(s string) string { return s }
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
	read := p.rules(ctx)
	p.expressions()
	p.history(ctx, read)
	p.grafana(ctx)
	p.changes(ctx)
	p.files(ctx)
	p.secrets()
	p.ledger(ctx)
	p.signin(ctx)
	p.access()
	p.estimate(read)
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
}

func (p *run) add(it Item) { p.r.Items = append(p.r.Items, it) }

// offline is the item of something that needs a request, with --offline.
func (p *run) offline(id, what string) {
	p.add(Item{ID: id, State: Unknown, What: what, Why: "--offline: nothing is asked"})
}

// redact takes what isn't to be printed out of every text.
func (p *run) redact() {
	for i := range p.r.Items {
		it := &p.r.Items[i]
		it.What, it.Why, it.Fix = p.c.Scrub(it.What), p.c.Scrub(it.Why), p.c.Scrub(it.Fix)
	}
	for i := range p.r.LeftOut {
		p.r.LeftOut[i] = p.c.Scrub(p.r.LeftOut[i])
	}
}

// said is what an error, or a server's answer in it, says, as printed:
// what turns text around or acts on a terminal shown as text
// (verdict.Clean), 200 bytes at most.
func said(err error) string {
	if err == nil {
		return ""
	}
	return clip(verdict.Clean(strings.ReplaceAll(strings.TrimSpace(err.Error()), "\n", "; ")), 200)
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

// names lists a few names, and how many more.
func names(ns []string, most int) string {
	ns = slices.Clone(ns)
	slices.Sort(ns)
	ns = slices.Compact(ns)
	for i := range ns {
		ns[i] = verdict.Clean(ns[i])
	}
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
