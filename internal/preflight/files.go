package preflight

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/oncall"
	"lapilli.dev/internal/serve"
)

// files reads the files serve reads beside the rules, each as serve reads
// it, and each on-call feed once.
func (p *run) files(ctx context.Context) {
	s := p.c.Serve
	var read, bad, unasked []string
	check := func(flag string, err error) {
		if err != nil {
			bad = append(bad, flag+": "+said(err))
			return
		}
		read = append(read, flag)
	}
	if s.Incidents != "" {
		_, err := incident.Load(s.Incidents)
		check("--incidents", err)
	}
	if s.Deploys != "" {
		_, _, err := serve.LoadDeploys(s.Deploys)
		check("--deploys", err)
	}
	if s.Config != "" {
		_, _, err := serve.ReadConfig(s.Config, s.Loc)
		check("--config", err)
	}
	if c := s.OnCall; c != nil {
		for _, sch := range c.Schedules {
			flag := "--on-call " + sch.Name
			if sch.File == "" {
				if p.c.Offline {
					unasked = append(unasked, flag)
					continue
				}
				if raw, err := feedURL(sch); err != nil {
					check(flag, err)
					continue
				} else {
					p.allowURL("on-call", raw)
				}
			}
			each, cancel := context.WithTimeout(ctx, eachTime)
			_, err := c.Fetch(each, sch, oncall.Client(), p.agent(), p.now)
			cancel()
			check(flag, err)
		}
	}
	if len(read)+len(bad)+len(unasked) == 0 {
		return
	}
	it := Item{ID: "inputs.files", State: OK, What: strings.Join(read, ", ") + " read, as serve reads them"}
	if len(read) == 0 {
		it.What = "nothing read"
	}
	switch {
	case len(bad) > 0:
		it.State, it.Why, it.Fix = Problem, strings.Join(bad, "; "), "serve refuses what it can't read: a run says so, and keeps what it read last"
	case len(unasked) > 0:
		it.State, it.Why = Unknown, "--offline: "+strings.Join(unasked, ", ")+" not fetched"
	}
	p.add(it)
}

// feedURL is the URL a schedule's feed is fetched from, read from its
// file each time, as serve reads it; never shown.
func feedURL(s *oncall.Schedule) (string, error) {
	raw := s.URL
	if s.URLFile != "" {
		b, err := os.ReadFile(s.URLFile)
		if err != nil {
			return "", fmt.Errorf("reading the file of the feed's URL: %w", err)
		}
		raw = strings.TrimSpace(string(b))
	}
	u, err := oncall.FeedURL(raw)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// secret is a file serve reads a secret from.
type secret struct {
	flag, path string
	// least is how long the secret must be; webhook says it holds a URL.
	least   int
	webhook bool
}

// secrets checks each token, key and webhook file: there, readable, long
// enough, each token its own, a webhook an https URL. A secret is said by
// its file's path and its length, never its text.
func (p *run) secrets() {
	s := p.c.Serve
	var all []secret
	add := func(flag, path string, least int, webhook bool) {
		if path != "" {
			all = append(all, secret{flag, path, least, webhook})
		}
	}
	for _, x := range []struct{ flag, path string }{{"--incidents-token-file", s.IncidentsToken}, {"--firings-token-file", s.FiringsToken},
		{"--tasks-token-file", s.TasksToken}, {"--changes-token-file", s.ChangesToken}, {"--notify-token-file", s.NotifyToken},
		{"--notify-grafana-secret-file", s.NotifyGrafanaSecret}, {"--grafana-token-file", s.GrafanaToken}, {"--loki-token-file", s.LokiToken},
		{"--ruler-token-file", s.RulerToken}, {"--changes-grafana-token-file", s.ChangesGrafanaToken}, {"--changes-ruler-token-file", s.ChangesRulerToken},
		{"--bearer-token-file", p.c.History.TokenFile}} {
		least := 16
		if x.flag == "--grafana-token-file" || x.flag == "--loki-token-file" || x.flag == "--ruler-token-file" || x.flag == "--bearer-token-file" {
			least = 1 // tokens others make, of whatever length
		}
		add(x.flag, x.path, least, false)
	}
	add("--session-key-file", s.SessionKey, 32, false)
	if s.OIDC != nil {
		add("--oidc-client-secret-file", s.OIDC.SecretFile, 1, false)
	}
	add("--slack-webhook-file", s.Slack, 0, true)
	add("--notify-webhook-file", s.NotifyWebhook, 0, true)
	for _, team := range slices.Sorted(maps.Keys(s.TeamDigests)) {
		add("--team-digest "+team, s.TeamDigests[team], 0, true)
	}
	for _, name := range slices.Sorted(maps.Keys(s.ReportDests)) {
		add("--report-dest "+name, s.ReportDests[name], 0, true)
	}
	if len(all) == 0 {
		return
	}
	var said, bad []string
	held := map[string]string{} // a token's text to the flag first holding it
	for _, x := range all {
		b, err := os.ReadFile(x.path)
		text := strings.TrimSpace(string(b))
		switch {
		case err != nil:
			bad = append(bad, fmt.Sprintf("%s %s: can't be read", x.flag, x.path))
			continue
		case x.webhook:
			u, err := url.Parse(text)
			if err != nil || u.Scheme != "https" || u.Host == "" || strings.ContainsAny(text, " \n") {
				bad = append(bad, fmt.Sprintf("%s %s: doesn't hold an https URL", x.flag, x.path))
				continue
			}
			said = append(said, fmt.Sprintf("%s %s: an https URL", x.flag, x.path))
			continue
		case len(text) < max(x.least, 1):
			bad = append(bad, fmt.Sprintf("%s %s: %d characters, under %d", x.flag, x.path, len(text), max(x.least, 1)))
			continue
		}
		if was, ok := held[text]; ok && x.least >= 16 {
			bad = append(bad, fmt.Sprintf("%s %s holds the same token as %s: give each its own", x.flag, x.path, was))
			continue
		}
		if x.least >= 16 {
			held[text] = x.flag
		}
		said = append(said, fmt.Sprintf("%s %s: %d characters", x.flag, x.path, len(text)))
	}
	it := Item{ID: "secrets.files", State: OK, What: plural(len(all), "secret file") + " read: " + strings.Join(said, "; ")}
	if len(bad) > 0 {
		it.State, it.Why = Problem, strings.Join(bad, "; ")
		it.Fix = "serve refuses a token it can't read, or one another holds; a webhook that holds no URL posts nothing"
	}
	if len(s.Slack)+len(s.NotifyWebhook)+len(s.TeamDigests)+len(s.ReportDests) > 0 {
		it.Why = strings.TrimPrefix(it.Why+"; whether a webhook takes a post isn't asked: knowing it would post", "; ")
	}
	p.add(it)
}
