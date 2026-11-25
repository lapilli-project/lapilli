package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"lapilli.dev/internal/content"
	"lapilli.dev/internal/routing"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
	"lapilli.dev/internal/verdict"
)

// Who each firing reached, in lapilli verdict (docs/design/routing.md): the
// alert manager's configuration, read once (--alertmanager-config, or the
// policies and contact points of --grafana), the model worked out over the
// firings of the period, and --compare's two sides.

// notifies reports whether the run works out the notifications: it is given
// the alert manager's configuration, or a Grafana whose policies say where
// its alerts go, and --notifications=false didn't turn it off.
func (f *verdictFlags) notifies() bool {
	return !f.noNotifications && !f.checkContent && !f.checkReplay && (f.amConfig != "" || f.grafana != "")
}

// validNotifications checks the flags of the notifications against the rest.
func (f *verdictFlags) validNotifications() error {
	given := ""
	switch {
	case len(f.externalLabels) > 0:
		given = "--external-labels"
	case f.silences != "":
		given = "--silences"
	case f.pagingReceivers != nil:
		given = "--paging-receivers"
	}
	switch {
	case (f.checkContent || f.checkReplay && f.checkReplaySet) && (given != "" || f.notificationsSet):
		what := given
		if what == "" {
			what = "--notifications"
		}
		return fmt.Errorf("%s works out who a firing reached, which the content check and the replay check don't: leave it out, or the check", what)
	case f.checkContent:
		return nil
	case f.notificationsSet && !f.noNotifications && f.amConfig == "" && f.grafana == "":
		return errors.New("--notifications works out who a firing reached from the alert manager's configuration: give --alertmanager-config, or --grafana")
	case given != "" && f.amConfig == "" && f.grafana == "":
		return fmt.Errorf("%s goes with --alertmanager-config, or --grafana, whose policies say where an alert goes", given)
	}
	return nil
}

// notifier is the routing configuration of a run, read, with what the model
// is given beside it.
type notifier struct {
	cfg *routing.Config
	run routing.Options
}

// options are what a side's report is worked out with.
func (n *notifier) options(known []rules.Rule, hours *verdict.Hours) verdict.NotificationOptions {
	return verdict.NotificationOptions{Config: n.cfg, Run: n.run, Rules: known, Hours: hours}
}

// workOut works out the notifications of a report, saying on stderr why it
// couldn't; nil is no notifier.
func (n *notifier) workOut(rep *verdict.Report, known []rules.Rule, hours *verdict.Hours, stderr io.Writer) {
	if n == nil || rep == nil {
		return
	}
	if err := rep.WorkOutNotifications(n.options(known, hours)); err != nil {
		fmt.Fprintf(stderr, "lapilli verdict: the notifications weren't worked out: %s\n", verdict.Clean(err.Error()))
	}
}

// routingOf reads the routing configuration, the silences and the rest the
// notifications are worked out with, once; nil when the run works out none.
// A configuration --alertmanager-config names that can't be read is an
// error; Grafana's policies, read because --grafana was given, say so on
// stderr and the run goes on without them.
func (f *verdictFlags) routingOf(ctx context.Context, stderr io.Writer) (*notifier, error) {
	if !f.notifies() {
		return nil, nil
	}
	n := &notifier{run: routing.Options{ExternalLabels: f.externalLabels, PagingReceivers: f.pagingReceivers}}
	if f.silences != "" {
		b, err := readSilencesFile(f.silences)
		if err != nil {
			return nil, fmt.Errorf("--silences %s: %w", verdict.Clean(f.silences), err)
		}
		if n.run.Silences, err = routing.ReadSilences(b); err != nil {
			return nil, fmt.Errorf("--silences %s: %w", verdict.Clean(f.silences), err)
		}
	}
	if f.amConfig != "" {
		notes, err := f.alertmanagerTemplates(ctx, stderr)
		if err != nil {
			return nil, err
		}
		if n.cfg = notes.Routing(); n.cfg == nil {
			return nil, fmt.Errorf("--alertmanager-config: the routing can't be read: %v", verdict.Clean(fmt.Sprint(notes.RoutingErr())))
		}
		return n, nil
	}
	_, cps, ps, err := source.GrafanaNotifications(ctx, f.grafana, f.grafanaTk, "lapilli/"+buildVersion())
	var mute []byte
	if err == nil {
		mute, err = source.GrafanaMuteTimings(ctx, f.grafana, f.grafanaTk, "lapilli/"+buildVersion())
	}
	if err == nil {
		n.cfg, err = content.ReadGrafanaRouting(cps, ps, mute)
	}
	if err != nil {
		fmt.Fprintf(stderr, "lapilli verdict: Grafana's notification policies weren't read, so who each firing reached isn't worked out (--notifications=false to leave it out): %s\n", verdict.Clean(err.Error()))
		return nil, nil
	}
	return n, nil
}

// readSilencesFile reads --silences, at most as many bytes as the model
// takes.
func readSilencesFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	b, err := io.ReadAll(io.LimitReader(file, routing.MaxSilenceBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > routing.MaxSilenceBytes {
		return nil, fmt.Errorf("more than %d MB", routing.MaxSilenceBytes>>20)
	}
	return b, nil
}
