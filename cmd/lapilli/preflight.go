package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"lapilli.dev/internal/preflight"
	"lapilli.dev/internal/verdict"
)

const preflightUsage = `lapilli preflight says what lapilli serve will meet before it is deployed,
given serve's own flags: whether it reads the rules and how many it can
replay, whether it reaches the history and signs in to it, what its tokens,
files and ledger hold, and how many requests, how long, how many nights and
how much memory its runs will take. It writes nothing, replays nothing, and
sends no request but the reads it makes, which an allowlist holds it to
(docs/design/preflight.md). What the clouds' credential chains do of their
own is outside it: Google's metadata server (and whether it is there, off
Google Cloud), the Azure CLI (which may write its token cache), OCI's
instance and resource principals and OKE's workload identity (its auth
service's POST among them), and the processes AWS's credential_process and
Google's executable credentials start.

Usage:
  lapilli preflight [serve's flags] [--json] [--fail-unknown] [--offline]

Every flag of lapilli serve (lapilli serve -h), read as serve reads it: one
serve refuses, it refuses, and exits 2. Its own:
  --json              the report as JSON: its items' ids and fields stay the
                      same between releases (docs/numbers.md)
  --fail-unknown      exit 1 for an item it can't say, as for a problem
  --offline           ask nothing: the items that need a request are
                      unknown; the flags, the rule files, the other files
                      and the estimates of requests and time are worked out.
                      The files of secrets and --ledger-url-file aren't
                      read. For CI, against what a pull request changes
  --preflight-timeout DUR
                      the whole of it (default 2m); --timeout stays serve's,
                      a run's, read for the estimates

Each item is ok, a problem or unknown, with why, and for a problem what to
change. It exits 0 with no problem, 1 with one (or, with --fail-unknown, an
unknown), 2 when the flags are wrong. Run it where serve runs, with serve's
service account and volume, for the items that need its credentials (the
history, Grafana's token, the clouds, the ledger): never in CI with the
production credentials. With --changes, what each write token or identity
may write is asked of an API that says so without writing: Grafana's list
of the token's actions, IAM's policy simulator (the role needs
iam:SimulatePrincipalPolicy on its own ARN, or the item is unknown), Cloud
Resource Manager's testIamPermissions, Azure's permissions of the identity,
and OCI's policies (the user needs to inspect groups, users, compartments
and policies in the tenancy; a policy with a condition is never a grant). What it prints is redacted as a pull request's
backtest comment is: the URLs given as their flag's name, their hosts and
users, addresses, tenants, a --gcp-project; a secret by its file's path and
length.
`

func runPreflight(args []string, stdout, stderr io.Writer) int {
	asJSON, failUnknown, offline := false, false, false
	timeout := 2 * time.Minute
	var rest []string
	refuse := func(err error) int {
		fmt.Fprintf(stderr, "lapilli preflight: %s\nRun \"lapilli preflight -h\" for the options.\n", verdict.Clean(err.Error()))
		return 2
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := strings.Cut(a, "=")
		switch name {
		case "-h", "--help":
			fmt.Fprint(stdout, preflightUsage)
			return 0
		case "--json", "--fail-unknown", "--offline":
			on := true
			if hasVal {
				b, err := strconv.ParseBool(val)
				if err != nil {
					return refuse(fmt.Errorf("%s=%s: want true or false", name, val))
				}
				on = b
			}
			switch name {
			case "--json":
				asJSON = on
			case "--fail-unknown":
				failUnknown = on
			default:
				offline = on
			}
			continue
		case "--preflight-timeout":
			if !hasVal {
				if i+1 >= len(args) {
					return refuse(fmt.Errorf("%s needs a value", name))
				}
				i++
				val = args[i]
			}
			d, err := parseDuration(val)
			if err != nil || d < time.Second {
				return refuse(fmt.Errorf("--preflight-timeout %s: want a duration like 2m", val))
			}
			timeout = d
			continue
		}
		rest = append(rest, a)
	}
	st, err := readServe(rest, offline)
	if err != nil {
		return refuse(err)
	}
	f := st.f
	c := preflight.Config{Serve: st.cfg, MaxRate: f.maxRate, MaxSamples: f.maxSamples, Offline: offline, Timeout: timeout,
		Version: buildVersion(), Now: now}
	if f.promURL != "" || f.gcpProj != "" {
		c.History = f.prometheus()
	}
	if f.loki != "" {
		c.Loki = f.lokiSource()
	}
	c.Scrubber = func(learned ...[2]string) func(string) string { return f.scrubber(append(redacted(st), learned...)...) }
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rep := preflight.Run(ctx, c)
	if asJSON {
		err = rep.JSON(stdout)
	} else {
		err = rep.Text(stdout)
	}
	if err != nil {
		fmt.Fprintf(stderr, "lapilli preflight: %v\n", err)
		return 1
	}
	return rep.Exit(failUnknown)
}

// redacted are the values, beside where the history is, that what a
// preflight prints never says, by flag: the other URLs serve is given.
func redacted(st serveSetup) [][2]string {
	f, c := st.f, st.cfg
	more := [][2]string{{"--check-grafana", f.checkGrafana}}
	for _, a := range f.alarms {
		more = append(more, [2]string{"--alarms " + a.source, a.url})
	}
	if c.OIDC != nil {
		more = append(more, [2]string{"--oidc-issuer", c.OIDC.Issuer})
	}
	if c.Proxy != nil {
		more = append(more, [2]string{"--proxy-jwt-jwks", c.Proxy.JWKS}, [2]string{"--proxy-jwt-issuer", c.Proxy.Issuer})
	}
	return more
}
