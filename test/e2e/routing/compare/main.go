// Command compare is the check of test/e2e/routing.sh: the notifications
// lapilli's model worked out from an Alertmanager's configuration, set beside
// those a real Alertmanager sent to the webhook keeper, receiver and
// integration by receiver and integration. It prints every notification of
// either side in a table, the differences marked, and exits 1 on any.
//
// A notification agrees when the other side has the same one on the same
// integration, in order: at a time within -tolerance of it, with the same
// status (firing, or resolved when it tells of nothing firing) and the same
// alerts, each as firing or resolved. Which of the alerts a mute kept out of
// the notifications is read from that: an alert missing from one side's
// notification is the other's difference.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"lapilli.dev/internal/content"
	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/routing"
	"lapilli.dev/internal/verdict"
)

// note is one notification, from either side.
type note struct {
	at     time.Time
	key    string // RECEIVER/INDEX
	status string // firing, or resolved
	alerts string // the alerts and how each stood, sorted
}

type report struct {
	Firings []struct {
		Rule          string `json:"rule"`
		Notifications *struct {
			Muted []struct {
				Kind string `json:"kind"`
				By   string `json:"by"`
			} `json:"muted"`
		} `json:"notifications"`
	} `json:"firings"`
	Notifications *struct {
		WorkedOut bool `json:"worked_out"`
		Notified  int  `json:"notified"`
		Paged     int  `json:"paged"`
		Receivers []struct {
			Name   string `json:"name"`
			Paging bool   `json:"paging"`
		} `json:"receivers"`
	} `json:"notifications"`
}

func main() {
	var (
		verdictFile = flag.String("verdict", "", "the report of `lapilli verdict --json`, with --alertmanager-config")
		configFile  = flag.String("config", "", "the Alertmanager's configuration")
		silences    = flag.String("silences", "", "the Alertmanager's silences, as GET /api/v2/silences lists them")
		keeperFile  = flag.String("keeper", "", "the keeper's notifications")
		instance    = flag.String("instance", "", "which Alertmanager's notifications the keeper kept to take")
		paging      = flag.String("paging", "", "--paging-receivers as the report was given it")
		tolerance   = flag.Duration("tolerance", 15*time.Second, "how far a notification may be from its time")
		origin      = flag.Int64("origin", 0, "Unix seconds the times in the table are given from (0 for none)")
		to          = flag.String("to", "", "the end of the period, RFC 3339")
		minimum     = flag.Int("min", 15, "the fewest notifications a real Alertmanager must have sent for the check to mean something")
		muteKinds   = flag.String("mutes", "inhibition,silence,mute_time_interval", "the kinds of mute the report must have found, comma separated")
	)
	flag.Parse()
	if err := run(*verdictFile, *configFile, *silences, *keeperFile, *instance, *paging, *to, *muteKinds, *tolerance, *origin, *minimum); err != nil {
		fmt.Fprintln(os.Stderr, "compare:", err)
		os.Exit(2)
	}
}

func run(verdictFile, configFile, silencesFile, keeperFile, instance, paging, to, muteKinds string, tol time.Duration, origin int64, minimum int) error {
	read := func(p string) ([]byte, error) { return os.ReadFile(p) }
	vb, err := read(verdictFile)
	if err != nil {
		return err
	}
	var rep report
	if err := json.Unmarshal(vb, &rep); err != nil {
		return fmt.Errorf("%s: %w", verdictFile, err)
	}
	var fs struct {
		Firings []firing.Firing `json:"firings"`
	}
	if err := json.Unmarshal(vb, &fs); err != nil {
		return err
	}
	until, err := time.Parse(time.RFC3339, to)
	if err != nil {
		return fmt.Errorf("-to: %w", err)
	}
	cb, err := read(configFile)
	if err != nil {
		return err
	}
	cfg, err := content.ReadAlertmanagerRouting(cb)
	if err != nil {
		return fmt.Errorf("%s: %w", configFile, err)
	}
	sb, err := read(silencesFile)
	if err != nil {
		return err
	}
	sil, err := routing.ReadSilences(sb)
	if err != nil {
		return fmt.Errorf("%s: %w", silencesFile, err)
	}
	pr, err := regexp.Compile(paging)
	if err != nil {
		return fmt.Errorf("-paging: %w", err)
	}
	opts := routing.Options{Silences: sil, Until: until}
	if paging != "" {
		opts.PagingReceivers = pr
	}
	judged := make([]verdict.Judged, len(fs.Firings))
	for i, f := range fs.Firings {
		judged[i] = verdict.Judged{Firing: f}
	}
	alerts := verdict.NotificationAlerts(judged, nil)
	res, err := cfg.Run(alerts, opts)
	if err != nil {
		return err
	}

	// What the model worked out.
	var model []note
	modelFiring, modelPaged := 0, 0
	for _, n := range res.Notifications {
		var parts []string
		for _, i := range n.Firing {
			parts = append(parts, alerts[i].Labels["alertname"]+":firing")
		}
		for _, i := range n.Resolved {
			parts = append(parts, alerts[i].Labels["alertname"]+":resolved")
		}
		if len(parts) == 0 {
			continue
		}
		status := "resolved"
		if len(n.Firing) > 0 {
			status = "firing"
			modelFiring++
			if n.Page {
				modelPaged++
			}
		}
		sort.Strings(parts)
		model = append(model, note{n.At, fmt.Sprintf("%s/%d", n.Receiver, n.Index), status, strings.Join(parts, " ")})
	}

	// What the real Alertmanager sent.
	kb, err := read(keeperFile)
	if err != nil {
		return err
	}
	var kept []struct {
		At          time.Time `json:"at"`
		Instance    string    `json:"instance"`
		Receiver    string    `json:"receiver"`
		Integration int       `json:"integration"`
		Body        struct {
			Status string `json:"status"`
			Alerts []struct {
				Status string            `json:"status"`
				Labels map[string]string `json:"labels"`
			} `json:"alerts"`
		} `json:"body"`
	}
	if err := json.Unmarshal(kb, &kept); err != nil {
		return fmt.Errorf("%s: %w", keeperFile, err)
	}
	var real []note
	realFiring, realPaged := 0, 0
	for _, k := range kept {
		if k.Instance != instance {
			continue
		}
		var parts []string
		status := "resolved"
		for _, a := range k.Body.Alerts {
			parts = append(parts, a.Labels["alertname"]+":"+a.Status)
			if a.Status == "firing" {
				status = "firing"
			}
		}
		if status == "firing" {
			realFiring++
			if pr.MatchString(k.Receiver) && paging != "" {
				realPaged++
			}
		}
		sort.Strings(parts)
		real = append(real, note{k.At, fmt.Sprintf("%s/%d", k.Receiver, k.Integration), status, strings.Join(parts, " ")})
	}

	// Side by side, integration by integration, in order of time.
	byKey := func(ns []note) map[string][]note {
		m := map[string][]note{}
		for _, n := range ns {
			m[n.key] = append(m[n.key], n)
		}
		for _, l := range m {
			sort.SliceStable(l, func(i, j int) bool { return l[i].at.Before(l[j].at) })
		}
		return m
	}
	mm, rm := byKey(model), byKey(real)
	keys := map[string]bool{}
	for k := range mm {
		keys[k] = true
	}
	for k := range rm {
		keys[k] = true
	}
	rel := func(t time.Time) string {
		if t.IsZero() {
			return "-"
		}
		if origin == 0 {
			return t.UTC().Format("15:04:05")
		}
		return fmt.Sprintf("T+%ds", t.Unix()-origin)
	}
	fmt.Printf("%-18s %-2s %-8s %-8s %-6s %-9s %s\n", "INTEGRATION", "#", "MODEL", "REAL", "DELTA", "STATUS", "ALERTS (model | real, where they differ)")
	diffs, maxDelta := 0, time.Duration(0)
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range keys {
			if !yield(k) {
				return
			}
		}
	}) {
		a, b := mm[k], rm[k]
		for i := 0; i < max(len(a), len(b)); i++ {
			var m, r *note
			if i < len(a) {
				m = &a[i]
			}
			if i < len(b) {
				r = &b[i]
			}
			mark, status, alertsText, delta := "", "", "", "-"
			switch {
			case m == nil:
				mark, status, alertsText = "DIFF: only real", r.status, r.alerts
			case r == nil:
				mark, status, alertsText = "DIFF: only model", m.status, m.alerts
			default:
				status, alertsText = m.status, m.alerts
				d := r.at.Sub(m.at)
				delta = d.Round(time.Second).String()
				maxDelta = max(maxDelta, d.Abs())
				switch {
				case d.Abs() > tol:
					mark = fmt.Sprintf("DIFF: %s apart, more than %s", d.Abs().Round(time.Second), tol)
				case m.status != r.status:
					mark, status = "DIFF: status", m.status+" | "+r.status
				case m.alerts != r.alerts:
					mark, alertsText = "DIFF: alerts", m.alerts+" | "+r.alerts
				}
			}
			var mt, rt time.Time
			if m != nil {
				mt = m.at
			}
			if r != nil {
				rt = r.at
			}
			if mark != "" {
				diffs++
			}
			fmt.Printf("%-18s %-2d %-8s %-8s %-6s %-9s %s %s\n", k, i+1, rel(mt), rel(rt), delta, status, alertsText, mark)
		}
	}
	fmt.Printf("\nmodel: %d notifications (%d of them firing, %d to paging receivers); real: %d (%d firing, %d to paging receivers); largest time difference %s, tolerance %s\n",
		len(model), modelFiring, modelPaged, len(real), realFiring, realPaged, maxDelta.Round(time.Second), tol)

	// What the report itself says, which the model's own run above must agree with.
	fail := func(format string, a ...any) {
		diffs++
		fmt.Printf("DIFF: "+format+"\n", a...)
	}
	if len(real) < minimum {
		fail("the real Alertmanager sent %d notifications, fewer than the %d that make the check mean something", len(real), minimum)
	}
	if rep.Notifications == nil || !rep.Notifications.WorkedOut {
		fail("the report worked out no notifications")
	} else {
		if rep.Notifications.Notified != realFiring {
			fail("the report's notifications: %d worked out (with firing alerts), %d really sent", rep.Notifications.Notified, realFiring)
		}
		if rep.Notifications.Paged != realPaged {
			fail("the report's pages: %d worked out, %d really sent to receivers matching %q", rep.Notifications.Paged, realPaged, paging)
		}
		if rep.Notifications.Notified != modelFiring || rep.Notifications.Paged != modelPaged {
			fail("the report (%d notified, %d paged) isn't what the model gives these firings (%d, %d)", rep.Notifications.Notified, rep.Notifications.Paged, modelFiring, modelPaged)
		}
	}
	// The mutes the scenario is made to have must have been found, or the
	// agreement above says little about them.
	found := map[string]bool{}
	for _, f := range rep.Firings {
		if f.Notifications == nil {
			continue
		}
		for _, m := range f.Notifications.Muted {
			found[m.Kind] = true
		}
	}
	for _, kind := range strings.Split(muteKinds, ",") {
		if !found[kind] {
			fail("no firing was muted by %s in the report", kind)
		}
	}
	if diffs > 0 {
		fmt.Printf("\n%d differences\n", diffs)
		os.Exit(1)
	}
	fmt.Println("\nno differences")
	return nil
}
