package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"lapilli.dev/internal/content"
	"lapilli.dev/internal/firing"
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
// its alerts go, and --notifications-worked-out=false didn't turn it off.
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
	case f.amConfigBefore != "" && (f.amConfig == "" || len(f.compare) == 0):
		return errors.New("--alertmanager-config-before is the configuration before the change: it goes with --alertmanager-config and --compare")
	case f.amConfigBefore != "" && f.noNotifications:
		return errors.New("--alertmanager-config-before works out who a firing reached, which --notifications-worked-out=false turns off")
	case (f.checkContent || f.checkReplay && f.checkReplaySet) && (given != "" || f.notificationsSet):
		what := given
		if what == "" {
			what = "--notifications-worked-out"
		}
		return fmt.Errorf("%s works out who a firing reached, which the content check and the replay check don't: leave it out, or the check", what)
	case f.checkContent:
		return nil
	case f.notificationsSet && !f.noNotifications && f.amConfig == "" && f.grafana == "":
		return errors.New("--notifications-worked-out works out who a firing reached from the alert manager's configuration: give --alertmanager-config, or --grafana")
	case given != "" && f.amConfig == "" && f.grafana == "":
		return fmt.Errorf("%s goes with --alertmanager-config, or --grafana, whose policies say where an alert goes", given)
	}
	return nil
}

// servesNotifications reports whether lapilli serve works out the
// notifications: it is given the alert manager's configuration, or a
// Grafana whose policies say where its alerts go, and --notifications-worked-out=false
// didn't turn it off.
func (f *verdictFlags) servesNotifications() bool {
	return !f.noNotifications && (f.amConfig != "" || f.grafana != "")
}

// validServeNotifications checks the flags of the notifications of lapilli
// serve, which works them out for each run's hours: they need the alert
// manager's configuration, or Grafana's policies, to work from.
func (f *verdictFlags) validServeNotifications() error {
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
	case f.notificationsSet && !f.noNotifications && f.amConfig == "" && f.grafana == "":
		return errors.New("--notifications-worked-out works out who a firing reached from the alert manager's configuration: give --alertmanager-config, or --grafana")
	case given != "" && f.amConfig == "" && f.grafana == "":
		return fmt.Errorf("%s goes with --alertmanager-config, or --grafana, whose policies say where an alert goes", given)
	case given != "" && f.noNotifications:
		return fmt.Errorf("%s works out who a firing reached, which --notifications-worked-out=false turns off", given)
	}
	return nil
}

// notifier is the routing configuration of a run, read, with what the model
// is given beside it.
type notifier struct {
	cfg *routing.Config
	run routing.Options
	// sent asks for each firing's notifications to be listed (--json).
	sent bool
	// also are the firings of rules left alone by --changed-only that the
	// model takes part (an inhibition they cause, a group they share) and
	// counts none of, with their rules.
	also      []firing.Firing
	alsoRules []rules.Rule
}

// options are what a side's report is worked out with.
func (n *notifier) options(known []rules.Rule, hours *verdict.Hours) verdict.NotificationOptions {
	return verdict.NotificationOptions{Config: n.cfg, Run: n.run, Rules: known, Hours: hours, Sent: n.sent, Also: n.also, AlsoRules: n.alsoRules}
}

// before is the notifier of --compare's before side: with
// --alertmanager-config-before, its own configuration; else this one.
func (n *notifier) before(cfg *routing.Config) *notifier {
	if n == nil || cfg == nil {
		return n
	}
	b := *n
	b.cfg = cfg
	return &b
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

// routingBefore reads the configuration --alertmanager-config-before names,
// for --compare's before side; nil when it names none.
func (f *verdictFlags) routingBefore(ctx context.Context, stderr io.Writer) (*routing.Config, error) {
	if f.amConfigBefore == "" || !f.notifies() {
		return nil, nil
	}
	fb := *f
	fb.amConfig = f.amConfigBefore
	notes, err := fb.alertmanagerTemplates(ctx, stderr)
	if err != nil {
		return nil, fmt.Errorf("--alertmanager-config-before: %s", strings.TrimPrefix(err.Error(), "--alertmanager-config: "))
	}
	cfg := notes.Routing()
	if cfg == nil {
		return nil, fmt.Errorf("--alertmanager-config-before: the routing can't be read: %v", verdict.Clean(fmt.Sprint(notes.RoutingErr())))
	}
	return cfg, nil
}

// routingOf reads the routing configuration, the silences and the rest the
// notifications are worked out with, once; nil when the run works out none.
// A configuration --alertmanager-config names that can't be read is said on
// stderr, and the run goes on without the routing, as it does for Grafana's
// policies, read because --grafana was given. (With the content check on, the
// notifications aren't worked out, and the configuration is read there, where
// a failure stays an error.)
func (f *verdictFlags) routingOf(ctx context.Context, stderr io.Writer) (*notifier, error) {
	if !f.notifies() {
		return nil, nil
	}
	n := &notifier{run: routing.Options{ExternalLabels: f.externalLabels, PagingReceivers: f.pagingReceivers}, sent: f.json}
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
		// Read for who a firing reached alone (the content check, which
		// reads it too, is off here): a configuration that can't be read
		// says so, as Grafana's policies do, and the run goes on without
		// the routing, as a release that carried it unread before did.
		notes, err := f.alertmanagerTemplates(ctx, stderr)
		if err == nil {
			if n.cfg = notes.Routing(); n.cfg == nil {
				err = fmt.Errorf("--alertmanager-config: the routing can't be read: %v", verdict.Clean(fmt.Sprint(notes.RoutingErr())))
			}
		}
		if err != nil {
			fmt.Fprintf(stderr, "lapilli verdict: the alert manager's configuration wasn't read, so who each firing reached isn't worked out (--notifications-worked-out=false to leave it out): %s\n", verdict.Clean(err.Error()))
			return nil, nil
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
		fmt.Fprintf(stderr, "lapilli verdict: Grafana's notification policies weren't read, so who each firing reached isn't worked out (--notifications-worked-out=false to leave it out): %s\n", verdict.Clean(err.Error()))
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

// leftAlone are the rules --changed-only leaves alone that the work-out
// still needs, replayed beside the changed ones and counted in nothing: the
// rules whose labels could match the source of an inhibition rule (a label
// the rule doesn't say itself may come with its series, so only the labels it
// does say rule one out), and, where a route groups by something other than
// alertname, so that rules share a group, all of them. The rest of the rules
// left alone change nothing about the changed rules' notifications.
func (n *notifier) leftAlone(alike []rules.Rule) []rules.Rule {
	if n == nil || len(alike) == 0 {
		return nil
	}
	for _, r := range n.cfg.Routes() {
		if !r.GroupByAll && !slices.Contains(r.GroupBy, "alertname") {
			return alike
		}
	}
	var out []rules.Rule
	for _, r := range alike {
		labels := map[string]string{"alertname": r.AlertName()}
		maps.Copy(labels, n.run.ExternalLabels)
		maps.Copy(labels, r.AlertLabels())
		if r.Folder != "" {
			labels["grafana_folder"] = r.Folder
		}
		for _, ih := range n.cfg.InhibitRules() {
			could := true
			for _, m := range ih.SourceMatchers {
				if _, says := labels[m.Name]; says && !m.Matches(labels) {
					could = false
					break
				}
			}
			if could {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

// withAlso is the notifier that also takes part the firings of rules left
// alone, counted in nothing.
func (n *notifier) withAlso(fired []firing.Firing, rs []rules.Rule) *notifier {
	if n == nil || len(fired) == 0 {
		return n
	}
	b := *n
	b.also, b.alsoRules = fired, rs
	return &b
}

// unionRules are the rules of a and b, a rule in both once.
func unionRules(a, b []rules.Rule) []rules.Rule {
	out := slices.Clone(a)
	for _, r := range b {
		if !slices.ContainsFunc(out, func(x rules.Rule) bool { return x.Key() == r.Key() }) {
			out = append(out, r)
		}
	}
	return out
}

// ofRules are the firings, and the replayed rules, of the rules in keep,
// from those a replay of a union of rules gave.
func ofRules(fired []firing.Firing, ran []rules.Rule, keep []rules.Rule) ([]firing.Firing, []rules.Rule) {
	names := map[string]bool{}
	for _, r := range keep {
		names[r.Name] = true
	}
	var fs []firing.Firing
	for _, f := range fired {
		if names[f.Rule] {
			fs = append(fs, f)
		}
	}
	var rs []rules.Rule
	for _, r := range ran {
		if names[r.Name] {
			rs = append(rs, r)
		}
	}
	return fs, rs
}
