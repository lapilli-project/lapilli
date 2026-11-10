package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"lapilli.dev/internal/serve"
)

const serveUsage = `lapilli serve keeps judging: every hour it replays the rules over the hours
since it last did, records the firings, the incidents and every version of
the rules in a ledger, and shows the stones in a web UI, a JSON API and
Prometheus metrics. Incidents can be written down in the UI, and judged
against at once.

Usage:
  lapilli serve [flags]

Where the firings come from (pick one), what went wrong, where the history
is, and how to judge: as for lapilli verdict (lapilli verdict -h), except:
  --rules PATH        one file or directory, read again every --watch: each
                      change is recorded as a new version, from when it was
                      seen, and replayed as such
  --rules-history DIR earlier versions of those rules, one directory each,
                      named by when it took effect (2026-10-14T16:40:31Z or
                      20261014T164031Z), so a first start's backfill replays
                      each day with that day's rules; a ledger with versions
                      of its own passes them over. recipes/versions/git.sh
                      writes one from git
  --incidents FILE    read again each run; optional, as incidents can be
                      written down in the UI
  --lead, --match, --refire, --noisy-firings, --noisy-precision
                      the defaults; the UI can change them

The ledger:
  --ledger DIR        where the ledger is kept (default lapilli-ledger)
  --every DUR         run this often (default 1h)
  --settle DUR        and replay the last DUR before the last run's end
                      again, for points that arrived late (default 1h)
  --backfill DUR      on first start, replay this far back (default 28d)
  --keep DUR          drop firings older than this (default 400d; 0 keeps
                      everything)
  --watch DUR         read the rules this often (default 1m)
  --digest DAY@HH:MM  once a week, judge the week before, work out longer
                      "for"s for noisy rules, and shorter ones that would
                      have caught the incidents nothing caught, and post the
                      report to --slack-webhook-file (default mon@09:00, in
                      --tz)
  --tz ZONE           the zone days and weeks are counted, and times shown,
                      in (default: local)
  --timeout DUR       give up a run after this long (default 50m)

The web UI and API:
  --listen ADDR       where to listen (default 127.0.0.1:8080). There is no
                      sign-in: keep it inside, or behind a proxy that signs
                      people in
  --metrics-listen ADDR
                      also serve /metrics, /healthz and /readyz here, alone,
                      so a NetworkPolicy can let Prometheus reach only them
  --incidents-listen ADDR
                      also take incidents here, and nothing else, from a job
                      that holds the token in --incidents-token-file: PUT or
                      DELETE /api/v1/incidents/ID, POST /api/v1/incidents.
                      A NetworkPolicy can let that job reach this port alone,
                      and not the one whose headers name people
  --incidents-token-file FILE
                      the file holding that token, 16 characters at least;
                      read for each request, so it can be rotated
  --firings-token-file FILE
                      also take firings recorded elsewhere, such as a
                      cloud's own alarm history, on --incidents-listen,
                      from a job that holds this token: PUT
                      /api/v1/firings/SOURCE?from=&to= with the firings
                      that start in [from, to), one JSON object a line as
                      --firings reads them (recipes/firings/put.sh sends a
                      recipe's), in place of those the ledger has from
                      SOURCE then. With it, --rules (or the like) and
                      --prometheus are optional: without them nothing is
                      replayed, and what is copied in is judged as it comes
  --source-quiet DUR  tell, once, of a source nothing has been copied in from
                      for this long, to --notify-webhook-file and, with
                      --notify-slack, Slack: the job copying it may have
                      stopped (default 26h; give a weekly job's 8d)
  --allowed-hosts LIST
                      host names to answer to, comma-separated, besides an
                      address, localhost, a name without dots or under
                      .svc, .cluster.local or .internal, and --external-url's:
                      the name a proxy passes on, if it is another. Any
                      other name is refused, against DNS rebinding; * for
                      any
  --user-header NAME  the header such a proxy names the person in, such as
                      X-Forwarded-Email; changes are recorded under it. Set it
                      only behind a proxy that sets it and strips it from
                      callers
  --read-only         take no changes through the UI or API
  --editors LIST      with this or --admins, only those named may change
                      anything, and the rest may only look: a comma-separated
                      list of people (as --user-header names them) and groups
                      (as --groups-header lists them), or * for everyone
                      signed in. Editors write incidents, tasks, comments,
                      quiet windows and notes on rules. user:NAME is a
                      person and group:NAME a group, alone: use them where
                      someone could pick a name that is a group's
  --admins LIST       as --editors, who may also change the settings and the
                      teams
  --groups-header NAME
                      the header such a proxy lists the person's groups in,
                      separated by commas, such as X-Forwarded-Groups. With
                      roles, nothing but the proxy should reach lapilli
                      serve, and it should strip both headers from callers

Telling of what happens (tasks made, assigned, done or overdue; incidents
written down; quiet windows kept; comments):
  --notify-webhook-file FILE
                      the file holding a URL to POST each, as JSON, to; for
                      a tracker, a chat or anything else that takes a
                      webhook. Sent apart from the writes, and tried again,
                      further apart, for about eight hours when it fails
  --notify-slack      also post each, as a line, to --slack-webhook-file
  --external-url URL  where people reach the UI, such as
                      https://lapilli.example.com; what is told links to it
  --lang en|ko        the language of what is posted to Slack (default en)
  --team-digest TEAM=FILE
                      also post each week's digest for one team, its own
                      rules and incidents, to the Slack-style incoming webhook
                      whose URL is in FILE (repeatable). TEAM is a team's name
                      in the ledger. --notify-webhook-file gets the digest as
                      digest.ready, for everyone and for each team

Notifications as they were sent, kept beside the firings: how many each rule
sent, how many repeats, how many woke people, how many for nothing:
  --notify-listen ADDR
                      take notifications here, and nothing else, each source
                      on a path of its own: POST /v1/alertmanager (a webhook
                      with the token as its bearer token), /v1/grafana (a
                      webhook contact point, with the token or an HMAC
                      signature), /v1/sns (Amazon SNS, for Amazon Managed
                      Service for Prometheus), /v1/azure (an action group's
                      webhook, common alert schema, the token as the URL's
                      password), /v1/google (a Cloud Monitoring webhook
                      channel, with basic auth). ?receiver=NAME names the
                      receiver where the notification doesn't
  --notify-token-file FILE
                      the file holding that token, 16 characters at least;
                      read for each request, so it can be rotated
  --notify-grafana-secret-file FILE
                      the file holding Grafana's HMAC secret, 16 characters
                      at least. Set Grafana's timestamp header to
                      X-Grafana-Alerting-Timestamp
  --notify-sns ARNS   the SNS topics taken, by ARN, comma-separated. A
                      subscription to one is confirmed by fetching its
                      SubscribeURL, from an SNS host only: the one request
                      the port makes

Agents, over MCP at /mcp:
  --mcp-compare editors|admins
                      who an agent must act for to replay a change to the
                      rules it proposes (the compare tool). Off by default:
                      it runs PromQL the agent writes, with lapilli's access
                      to Prometheus. It needs roles (--editors or
                      --admins). The tools that read are everyone's

Pages: / (overview), /rules, /incidents, /firings, /settings, /runs, /teams,
/windows, /tasks, /outcomes. API: /api/v1/report?from=&to=, /api/v1/incidents,
/api/v1/rules, /api/v1/runs, /api/v1/settings, /api/v1/teams, /api/v1/notes,
/api/v1/windows, /api/v1/tasks, /api/v1/comments, /api/v1/me,
/api/v1/outcomes. MCP: /mcp. Also /metrics, /healthz, /readyz.
`

func runServe(args []string, stdout, stderr io.Writer) int {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Fprint(stdout, serveUsage)
			return 0
		}
	}
	cfg := serve.Config{
		Ledger: "lapilli-ledger", Listen: "127.0.0.1:8080", Every: time.Hour, Settle: time.Hour,
		Backfill: 28 * 24 * time.Hour, Keep: 400 * 24 * time.Hour, Watch: time.Minute,
		Digest: serve.Weekly{Day: time.Monday, Hour: 9}, Version: buildVersion(),
	}
	timeoutSet := false
	takes := func(name, val string) (bool, error) {
		dur := func(min time.Duration) (time.Duration, error) {
			d, err := serve.ParseDuration(val)
			if err != nil || d < min {
				return 0, fmt.Errorf("%s %s: want a duration like 1h, 28d", name, val)
			}
			return d, nil
		}
		var err error
		switch name {
		case "--ledger":
			cfg.Ledger = val
		case "--listen":
			cfg.Listen = val
		case "--allowed-hosts":
			cfg.AllowedHosts = list(val)
		case "--metrics-listen":
			cfg.MetricsListen = val
		case "--incidents-listen":
			cfg.IncidentsListen = val
		case "--incidents-token-file", "--firings-token-file":
			b, err := os.ReadFile(val)
			if err != nil {
				return true, fmt.Errorf("%s: %v", name, err)
			}
			if len(strings.TrimSpace(string(b))) < 16 {
				return true, fmt.Errorf("%s: want a token of 16 characters at least", name)
			}
			if name == "--firings-token-file" {
				cfg.FiringsToken = val
			} else {
				cfg.IncidentsToken = val
			}
		case "--notify-listen":
			cfg.NotifyListen = val
		case "--notify-token-file", "--notify-grafana-secret-file":
			b, err := os.ReadFile(val)
			if err != nil {
				return true, fmt.Errorf("%s: %v", name, err)
			}
			if len(strings.TrimSpace(string(b))) < 16 {
				return true, fmt.Errorf("%s: want a secret of 16 characters at least", name)
			}
			if name == "--notify-token-file" {
				cfg.NotifyToken = val
			} else {
				cfg.NotifyGrafanaSecret = val
			}
		case "--notify-sns":
			for _, arn := range list(val) {
				if !snsTopic.MatchString(arn) {
					return true, fmt.Errorf("--notify-sns %s: want topic ARNs, like arn:aws:sns:eu-west-1:123456789012:alerts", arn)
				}
				cfg.NotifySNS = append(cfg.NotifySNS, arn)
			}
		case "--source-quiet":
			cfg.SourceQuiet, err = dur(time.Hour)
		case "--every":
			cfg.Every, err = dur(time.Minute)
		case "--settle":
			cfg.Settle, err = dur(0)
		case "--backfill":
			cfg.Backfill, err = dur(time.Hour)
		case "--keep":
			cfg.Keep, err = dur(0)
		case "--watch":
			cfg.Watch, err = dur(10 * time.Second)
		case "--digest":
			cfg.Digest, err = serve.ParseWeekly(val)
			if err != nil {
				err = fmt.Errorf("--digest %w", err)
			}
		case "--user-header":
			cfg.UserHeader = val
		case "--read-only":
			cfg.ReadOnly = true
		case "--editors":
			cfg.Editors = list(val)
		case "--admins":
			cfg.Admins = list(val)
		case "--groups-header":
			cfg.GroupsHeader = val
		case "--notify-webhook-file":
			if _, err := os.Stat(val); err != nil {
				return true, fmt.Errorf("--notify-webhook-file: %v", err)
			}
			cfg.NotifyWebhook = val
		case "--notify-slack":
			cfg.NotifySlack = true
		case "--external-url":
			if u, perr := url.Parse(val); perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return true, fmt.Errorf("--external-url %s: want where people reach the UI, like https://lapilli.example.com", val)
			}
			cfg.ExternalURL = val
		case "--lang":
			if val != "en" && val != "ko" {
				return true, fmt.Errorf("--lang %s: want en or ko", val)
			}
			cfg.Lang = val
		case "--team-digest":
			team, file, ok := strings.Cut(val, "=")
			if !ok || team == "" || file == "" {
				return true, fmt.Errorf("--team-digest %s: want a team and a file, like payments=/run/secrets/payments", val)
			}
			if _, err := os.Stat(file); err != nil {
				return true, fmt.Errorf("--team-digest: %v", err)
			}
			if cfg.TeamDigests == nil {
				cfg.TeamDigests = map[string]string{}
			}
			cfg.TeamDigests[team] = file
		case "--mcp-compare":
			if val != "editors" && val != "admins" {
				return true, fmt.Errorf("--mcp-compare %s: want editors or admins", val)
			}
			cfg.MCPCompare = val
		case "--timeout":
			timeoutSet = true
			return false, nil
		default:
			return false, nil
		}
		return true, err
	}
	f, err := parseFlags(args, takes)
	switch {
	case err != nil:
	case cfg.Keep > 0 && cfg.Keep < cfg.Backfill:
		err = fmt.Errorf("--keep %s is shorter than --backfill %s", time.Duration(cfg.Keep), time.Duration(cfg.Backfill))
	case (len(cfg.Editors) > 0 || len(cfg.Admins) > 0 || cfg.GroupsHeader != "") && cfg.UserHeader == "":
		err = fmt.Errorf("--editors, --admins and --groups-header need --user-header: who is who comes from the proxy that signs people in")
	case cfg.NotifySlack && f.slackFile == "":
		err = fmt.Errorf("--notify-slack needs --slack-webhook-file")
	case cfg.IncidentsListen != "" && cfg.IncidentsToken == "" && cfg.FiringsToken == "":
		err = fmt.Errorf("--incidents-listen takes what a token writes: add --incidents-token-file, --firings-token-file or both")
	case cfg.IncidentsListen == "" && (cfg.IncidentsToken != "" || cfg.FiringsToken != ""):
		err = fmt.Errorf("--incidents-token-file and --firings-token-file go with --incidents-listen, the port they are taken on")
	case cfg.IncidentsToken != "" && cfg.FiringsToken != "" && sameFile(cfg.IncidentsToken, cfg.FiringsToken):
		err = fmt.Errorf("--incidents-token-file and --firings-token-file hold the same token: give each its own, so a job that may copy one can't write the other")
	case cfg.NotifyToken != "" && (cfg.IncidentsToken != "" && sameFile(cfg.NotifyToken, cfg.IncidentsToken) ||
		cfg.FiringsToken != "" && sameFile(cfg.NotifyToken, cfg.FiringsToken)):
		err = fmt.Errorf("--notify-token-file holds the same token as --incidents-token-file or --firings-token-file: give each its own, since alert managers keep the notifications token in webhook URLs more people can read")
	case cfg.IncidentsListen != "" && cfg.ReadOnly:
		err = fmt.Errorf("--incidents-listen takes writes, and --read-only refuses them")
	case cfg.NotifyListen != "" && cfg.NotifyToken == "" && cfg.NotifyGrafanaSecret == "" && len(cfg.NotifySNS) == 0:
		err = fmt.Errorf("--notify-listen needs --notify-token-file, --notify-grafana-secret-file or --notify-sns: what lets a notification in")
	case cfg.NotifyListen == "" && (cfg.NotifyToken != "" || cfg.NotifyGrafanaSecret != "" || len(cfg.NotifySNS) > 0):
		err = fmt.Errorf("--notify-token-file, --notify-grafana-secret-file and --notify-sns need --notify-listen")
	case cfg.NotifyListen != "" && cfg.ReadOnly:
		err = fmt.Errorf("--notify-listen takes writes, and --read-only refuses them")
	case cfg.MCPCompare != "" && len(cfg.Editors)+len(cfg.Admins) == 0:
		err = fmt.Errorf("--mcp-compare needs --editors or --admins: without roles everyone is an admin, and anyone who reaches /mcp could run PromQL with lapilli's access")
	}
	if err != nil {
		fmt.Fprintf(stderr, "lapilli serve: %v\nRun \"lapilli serve -h\" for the options.\n", err)
		return 2
	}
	if !timeoutSet {
		f.timeout = 50 * time.Minute
	}
	cfg.RulesPath = ""
	if len(f.rules) == 1 {
		cfg.RulesPath = f.rules[0].path
	}
	cfg.RulesHistory = f.rulesHistory
	cfg.Loaded, cfg.Grafana, cfg.GrafanaToken, cfg.Alerts = f.loadedRules, f.grafana, f.grafanaTk, f.alerts
	cfg.RuleRE, cfg.Incidents, cfg.Impact, cfg.ImpactFor = f.ruleRE, f.incidents, f.impact, f.impactFor
	cfg.Options, cfg.Step, cfg.Loc, cfg.Slack = f.opts, f.step, f.loc, f.slackFile
	cfg.Timeout, cfg.Parallel = f.timeout, f.parallel

	log := slog.New(slog.NewTextHandler(stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// With firings copied in alone, there may be no history to read.
	var h history
	if f.promURL != "" || f.gcpProj != "" {
		if h, err = newHistory(ctx, f); err != nil {
			fmt.Fprintf(stderr, "lapilli serve: %v\n", err)
			return 1
		}
	}
	s, err := serve.New(ctx, cfg, h, log)
	if err != nil {
		fmt.Fprintf(stderr, "lapilli serve: %v\n", err)
		return 1
	}
	defer s.Close()
	if !strings.HasPrefix(cfg.Listen, "127.") && !strings.HasPrefix(cfg.Listen, "localhost:") && !strings.HasPrefix(cfg.Listen, "[::1]") && cfg.UserHeader == "" && !cfg.ReadOnly {
		log.Warn("listening beyond this machine with no sign-in: anyone who reaches it can write incidents and change settings; put it behind a proxy (--user-header) or use --read-only")
	}
	if err := s.Run(ctx); err != nil {
		fmt.Fprintf(stderr, "lapilli serve: %v\n", err)
		return 1
	}
	return 0
}

// sameFile reports whether two paths name one file, or files holding the
// same token.
func sameFile(a, b string) bool {
	x, err1 := os.ReadFile(a)
	y, err2 := os.ReadFile(b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	return strings.TrimSpace(string(x)) == strings.TrimSpace(string(y))
}

// snsTopic is an SNS topic's ARN, in any partition.
var snsTopic = regexp.MustCompile(`^arn:aws(-cn|-us-gov)?:sns:[a-z0-9-]+:[0-9]{12}:[A-Za-z0-9_-]{1,256}$`)

// list is a comma-separated list, without blanks.
func list(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}
