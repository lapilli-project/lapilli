// Package source reads what lapilli judges from where it lives: the
// history in a Prometheus-compatible API, and alert rules in files, in that
// API or in a Grafana. Both lapilli verdict and lapilli serve read through
// it.
package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/cloud"
	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// Prometheus is where the history is, and how to ask for it.
type Prometheus struct {
	URL string
	// GCPProject stands for Google Cloud Managed Service for Prometheus's
	// URL for the project, with Auth google.
	GCPProject string
	// Auth is the cloud whose credentials to send: aws, azure or google.
	Auth, AWSRegion string
	// TokenFile holds a bearer token to send with each query; it is read
	// again for each one, so a rotated token is picked up.
	TokenFile string
	// Tenant, when set, is sent as X-Scope-OrgID: a Mimir's or a Cortex's
	// tenant, for one with tenants and no gateway to set it.
	Tenant string
}

// History is a client for the API.
func (p Prometheus) History(ctx context.Context, agent string) (*prom.Client, error) {
	base, auth := p.URL, p.Auth
	if p.GCPProject != "" {
		base = "https://monitoring.googleapis.com/v1/projects/" + url.PathEscape(p.GCPProject) + "/location/global/prometheus"
		auth = "google"
	}
	var opts []prom.Option
	if auth != "" {
		o, err := cloud.Auth(ctx, auth, base, p.AWSRegion)
		if err != nil {
			return nil, err
		}
		opts = append(opts, o)
	}
	if p.Tenant != "" {
		opts = append(opts, prom.WithHeader("X-Scope-OrgID", p.Tenant))
	}
	if p.TokenFile != "" {
		file := p.TokenFile
		opts = append(opts, prom.WithToken(func(context.Context) (string, error) {
			b, err := os.ReadFile(file)
			return strings.TrimSpace(string(b)), err
		}))
	}
	return prom.New(base, agent, opts...), nil
}

// Loaded asks the history's API for the alert rules it has loaded, and
// returns them and the answer they were read from.
func Loaded(ctx context.Context, h replay.History) ([]rules.Rule, rules.Doc, error) {
	api, ok := h.(interface {
		Rules(context.Context) ([]byte, error)
	})
	if !ok {
		return nil, rules.Doc{}, errors.New("--loaded-rules: this history has no rules API")
	}
	b, err := api.Rules(ctx)
	if err != nil {
		return nil, rules.Doc{}, fmt.Errorf("--loaded-rules: %w", err)
	}
	rs, err := rules.FromAPI(b)
	if err != nil {
		return nil, rules.Doc{}, fmt.Errorf("--loaded-rules: %w", err)
	}
	return rs, rules.Doc{Source: "api", API: true, Data: b}, nil
}

// client doesn't follow redirects, so a token goes nowhere it wasn't sent.
var client = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Grafana asks a Grafana for its alert rules in the provisioning format
// lapilli reads from files, and returns them and the export they were read
// from. The token is read from a file and never shown, and redirects aren't
// followed, so it goes nowhere else.
func Grafana(ctx context.Context, base, tokenFile, agent string, warn io.Writer) ([]rules.Rule, rules.Doc, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/api/v1/provisioning/alert-rules/export?format=yaml", nil)
	if err != nil {
		return nil, rules.Doc{}, fmt.Errorf("--grafana: %w", err)
	}
	req.Header.Set("User-Agent", agent)
	if tokenFile != "" && strings.HasPrefix(base, "http://") && warn != nil {
		fmt.Fprintln(warn, "lapilli: --grafana is plain http: the token goes unencrypted")
	}
	if tokenFile != "" {
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			return nil, rules.Doc{}, fmt.Errorf("--grafana-token-file: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(b)))
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, rules.Doc{}, fmt.Errorf("--grafana: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, rules.Doc{}, fmt.Errorf("--grafana: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, rules.Doc{}, fmt.Errorf("--grafana: HTTP %d: %s", resp.StatusCode, verdict.Clean(msg))
	}
	doc := rules.Doc{Source: base, Data: body}
	rs, err := rules.FromDocs([]rules.Doc{doc})
	if err != nil {
		return nil, rules.Doc{}, fmt.Errorf("--grafana: %w", err)
	}
	return rs, doc, nil
}

// Split sorts the rules in place (was) and those proposed (is) into the
// rules alike in both, those only in place, and those only proposed.
func Split(was, is []rules.Rule) (same, before, after []rules.Rule) {
	left := map[string]int{}
	for _, r := range was {
		left[r.Key()]++
	}
	for _, r := range is {
		if k := r.Key(); left[k] > 0 {
			left[k]--
			same = append(same, r)
		} else {
			after = append(after, r)
		}
	}
	for _, r := range was {
		if k := r.Key(); left[k] > 0 {
			left[k]--
			before = append(before, r)
		}
	}
	return same, before, after
}

// TellApart names apart rules that share a name in one set, by their
// groups (a ruler's under their namespaces) or the labels they differ in:
// "HighErrorRate [severity=page]", "ScrapeSlow [payments/shop]". The
// name is worked out over every set the same way, so a rule keeps it from
// one set to the next. Rules alike in both are left to be counted together.
//
// A name made so can be a rule's own: an alert written "Errors [shop/api]"
// beside the Errors in shop/api, told apart. It is named apart again, until
// no two rules in a set share a name but those alike in group and labels;
// the name made first is kept, and the rule whose own name it was named
// apart from it.
func TellApart(sets ...[]rules.Rule) {
	for pass := 0; pass < 8; pass++ {
		if !tellApart(sets, pass > 0) {
			return
		}
	}
}

// tellApart names apart once the rules that share a name, and says whether
// it named any. again is a pass after the first: of the rules sharing a
// name, those that have it of their own are named apart, when others were
// given it.
func tellApart(sets [][]rules.Rule, again bool) bool {
	twice := map[string]bool{}
	for _, rs := range sets {
		seen := map[string]bool{}
		for _, r := range rs {
			twice[r.Name] = twice[r.Name] || seen[r.Name]
			seen[r.Name] = true
		}
	}
	byName := map[string][]*rules.Rule{}
	for _, rs := range sets {
		for i := range rs {
			if twice[rs[i].Name] {
				byName[rs[i].Name] = append(byName[rs[i].Name], &rs[i])
			}
		}
	}
	// A ruler's rules are in a group in a namespace: ScrapeSlow
	// [payments/shop]; a cloud's alarms under their source: HighCPU
	// [cloudwatch-prod]. Each is counted by the pair, and written with a /
	// in either escaped, so namespace a/b's group c and namespace a's group
	// b/c are two: a%2Fb/c and a/b%2Fc.
	group := func(r *rules.Rule) string {
		if r.Namespace != "" {
			return r.Namespace + "\x00" + r.Group
		}
		return r.Group
	}
	part := func(r *rules.Rule) string {
		switch {
		case r.Namespace != "" && r.Cloud != "" && r.Group == "":
			return nameEscape.Replace(r.Namespace)
		case r.Namespace != "":
			return nameEscape.Replace(r.Namespace) + "/" + nameEscape.Replace(r.Group)
		}
		return r.Group
	}
	changed := false
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		rs := byName[name]
		pick := rs
		if again {
			var own []*rules.Rule
			for _, r := range rs {
				if r.Alert == "" {
					own = append(own, r)
				}
			}
			if len(own) > 0 && len(own) < len(rs) {
				pick = own
			}
		}
		groups, keys := map[string]bool{}, map[string]bool{}
		for _, r := range rs {
			groups[group(r)] = true
			for k := range r.Labels {
				keys[k] = true
			}
		}
		var differ []string
		for k := range keys {
			v0, ok0 := rs[0].Labels[k]
			for _, r := range rs[1:] {
				if v, ok := r.Labels[k]; v != v0 || ok != ok0 {
					differ = append(differ, k)
					break
				}
			}
		}
		slices.Sort(differ)
		for _, r := range pick {
			var parts []string
			if len(groups) > 1 {
				parts = append(parts, part(r))
			}
			for _, k := range differ {
				if v, ok := r.Labels[k]; ok {
					parts = append(parts, k+"="+v)
				}
			}
			if len(parts) > 0 {
				r.Alert = r.AlertName()
				r.Name = name + " [" + strings.Join(parts, ", ") + "]"
				changed = true
			}
		}
	}
	return changed
}

// nameEscape writes a ruler's namespace or group in a rule's name, so
// that no two pairs read alike: a / in either, and what would end or split
// the brackets, escaped as a URL's path escapes them.
var nameEscape = strings.NewReplacer("%", "%25", "/", "%2F", ",", "%2C", "]", "%5D")

// longerFors are the "for"s a noisy rule is replayed with, the shortest
// first.
var longerFors = []time.Duration{5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour}

// Suggest replays each noisy rule in rep with longer "for"s and keeps the
// shortest that would have caught every incident the rule caught, firing at
// most half as often. A name shared by several rules has no one "for" to
// change, and is passed over. excuse, when not nil, says which firings that
// caught nothing a quiet window excuses, as the ledger does.
func Suggest(ctx context.Context, h replay.History, rs []rules.Rule, rep *verdict.Report, incs []incident.Incident, o verdict.Options, step time.Duration,
	excuse func(firing.Firing) bool) []verdict.Suggestion {
	var out []verdict.Suggestion
	o.SomeRules = true // each is judged alone
	// caughtBy is when the rule caught each incident it caught.
	caughtBy := func(r *verdict.Report, rule string) map[string]time.Duration {
		at := map[string]time.Duration{}
		for _, in := range r.Incidents {
			for _, c := range in.CaughtBy {
				if c.Rule == rule {
					at[in.ID] = c.Delay
				}
			}
		}
		return at
	}
	for _, rr := range rep.Rules {
		if !rr.Noisy || !rr.Replayed {
			continue
		}
		i := slices.IndexFunc(rs, func(r rules.Rule) bool { return r.Name == rr.Rule })
		if i < 0 || slices.ContainsFunc(rs[i+1:], func(r rules.Rule) bool { return r.Name == rr.Rule }) {
			continue
		}
		var fors []time.Duration
		for _, d := range longerFors {
			if d > rs[i].For {
				fors = append(fors, d)
			}
		}
		alts, err := replay.WithFor(ctx, h, rs[i], fors, o.From, o.To, step)
		if err != nil {
			continue
		}
		caught := caughtBy(rep, rr.Rule)
		for _, d := range fors {
			alt := verdict.Judge(nil, alts[d], incs, o)
			after := verdict.RuleResult{Rule: rr.Rule}
			if j := slices.IndexFunc(alt.Rules, func(r verdict.RuleResult) bool { return r.Rule == rr.Rule }); j >= 0 {
				after = alt.Rules[j]
			}
			Excuse(&after, alt.Firings, excuse)
			still := caughtBy(alt, rr.Rule)
			kept, later := true, time.Duration(0)
			for id, was := range caught {
				now, ok := still[id]
				kept = kept && ok
				later = max(later, now-was)
			}
			if kept && 2*after.Firings <= rr.Firings {
				out = append(out, verdict.Suggestion{Rule: rr.Rule, For: rs[i].For, NewFor: d, Before: rr, After: after,
					Incidents: len(caught), Later: later})
				break
			}
		}
	}
	return out
}
