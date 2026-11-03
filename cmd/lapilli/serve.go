package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
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
                      "for"s for noisy rules, and post the report to
                      --slack-webhook-file (default mon@09:00, in --tz)
  --tz ZONE           the zone days and weeks are counted, and times shown,
                      in (default: local)
  --timeout DUR       give up a run after this long (default 50m)

The web UI and API:
  --listen ADDR       where to listen (default 127.0.0.1:8080). There is no
                      sign-in: keep it inside, or behind a proxy that signs
                      people in
  --user-header NAME  the header such a proxy names the person in, such as
                      X-Forwarded-Email; changes are recorded under it. Set it
                      only behind a proxy that sets it and strips it from
                      callers
  --read-only         take no changes through the UI or API

Pages: / (overview), /rules, /incidents, /firings, /settings, /runs.
API: /api/v1/report?from=&to=, /api/v1/incidents, /api/v1/rules,
/api/v1/runs, /api/v1/settings. Also /metrics, /healthz, /readyz.
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
		case "--timeout":
			timeoutSet = true
			return false, nil
		default:
			return false, nil
		}
		return true, err
	}
	f, err := parseFlags(args, takes)
	if err == nil && cfg.Keep > 0 && cfg.Keep < cfg.Backfill {
		err = fmt.Errorf("--keep %s is shorter than --backfill %s", time.Duration(cfg.Keep), time.Duration(cfg.Backfill))
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
	cfg.Loaded, cfg.Grafana, cfg.GrafanaToken, cfg.Alerts = f.loadedRules, f.grafana, f.grafanaTk, f.alerts
	cfg.RuleRE, cfg.Incidents, cfg.Impact, cfg.ImpactFor = f.ruleRE, f.incidents, f.impact, f.impactFor
	cfg.Options, cfg.Step, cfg.Loc, cfg.Slack = f.opts, f.step, f.loc, f.slackFile
	cfg.Timeout, cfg.Parallel = f.timeout, f.parallel

	log := slog.New(slog.NewTextHandler(stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	h, err := newHistory(ctx, f)
	if err != nil {
		fmt.Fprintf(stderr, "lapilli serve: %v\n", err)
		return 1
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
