package ledger

import (
	"context"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
)

// A cloud's message of its alarm resolving, or repeating, doesn't say when
// the alarm began: the alert takes the start of the last alert of its rule
// and labels a notification of the same group told of, and with none, stays
// unknown and ties to no firing.
func TestNotifyStartFromGroup(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	s := t0.Add(2 * time.Hour)
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{{Rule: "Disk", Labels: map[string]string{"host": "a"}, Start: s, End: s.Add(time.Hour)}})
	disk := func(host string, start time.Time, resolved bool) Alert {
		return Alert{Rule: "Disk", Labels: map[string]string{"host": host}, Start: start, Resolved: resolved}
	}
	if !notify(t, l, Notification{At: s.Add(time.Minute), Source: "sns", Group: "arn:alarm:Disk", Alerts: []Alert{disk("b", s.Add(-time.Hour), false)}}) ||
		!notify(t, l, Notification{At: s.Add(2 * time.Minute), Source: "sns", Group: "arn:alarm:Disk", Alerts: []Alert{disk("a", s, false)}}) {
		t.Fatal("not kept")
	}
	// Resolved, with no start: host a's, not the group's last alert of
	// another host.
	if !notify(t, l, Notification{At: s.Add(time.Hour), Source: "sns", Group: "arn:alarm:Disk", Alerts: []Alert{disk("a", time.Time{}, true)}}) {
		t.Fatal("not kept")
	}
	// A group the ledger has no notification of.
	if !notify(t, l, Notification{At: s.Add(time.Hour), Source: "sns", Group: "arn:alarm:Other", Alerts: []Alert{disk("a", time.Time{}, true)}}) {
		t.Fatal("not kept")
	}
	ns, err := l.Notifications(ctx, t0, t0.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(ns) != 4 || !ns[2].Alerts[0].Start.Equal(s) || !ns[3].Alerts[0].Start.IsZero() {
		t.Fatalf("notifications: %+v", ns)
	}
	ps, err := l.Pages(ctx, t0, t0.Add(24*time.Hour), twoMinutes)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 4 || ps[2].Stone == PageUntied || len(ps[2].Firings) != 1 || ps[3].Stone != PageUntied {
		t.Errorf("pages: %+v", ps)
	}
}

// A notification that names the source its alarm's firings are copied in
// as ties to that source's rule with the name the alarm sent, and its
// severity, though the ledger names the rule apart; without the source, the
// alert's name ties as before, to the replayed rule of that name.
func TestNotifyTiesToItsSource(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	s := t0.Add(2 * time.Hour)
	labels := map[string]string{"severity": "Sev3", "resource": "db-1"}
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{{Rule: "disk-full", Labels: map[string]string{"resource": "db-1"}, Start: s.Add(time.Hour)}})
	putFirings(t, l, "azure-shop", t0, t0.Add(24*time.Hour), []firing.Firing{{Rule: "disk-full", Labels: labels, Start: s, End: s.Add(30 * time.Minute)}})
	sent := Alert{Rule: "disk-full", Labels: labels, Start: s}
	if !notify(t, l, Notification{At: s.Add(time.Minute), Source: "azure", Group: "a1", Copied: "azure-shop", Alerts: []Alert{sent}}) {
		t.Fatal("not kept")
	}
	if !notify(t, l, Notification{At: s.Add(time.Hour), Source: "azure", Group: "a2", Alerts: []Alert{{Rule: "disk-full", Labels: labels, Start: s.Add(time.Hour)}}}) {
		t.Fatal("not kept")
	}
	ps, err := l.Pages(ctx, t0, t0.Add(24*time.Hour), twoMinutes)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0].Rule != "disk-full [azure-shop]" || len(ps[0].Firings) != 1 || ps[1].Rule != "disk-full" || len(ps[1].Firings) != 1 {
		t.Fatalf("pages: %+v", ps)
	}
	since, err := l.NotifiedSince(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := since["disk-full [azure-shop]"]; !ok {
		t.Errorf("notified since: %v", since)
	}
	all, err := l.Firings(ctx, FiringQuery{From: t0, To: t0.Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	of, err := l.NotifiedOf(ctx, all, twoMinutes)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range all {
		if of[f.ID] != 1 {
			t.Errorf("%s at %s: told of by %d", f.Rule, f.Start, of[f.ID])
		}
	}
}

// An alert naming its alarm by id, as OCI's do, is of the rule its source's
// firings with that alarm_id are of, whatever title its message has.
func TestNotifyByAlarmID(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	s := t0.Add(2 * time.Hour)
	labels := map[string]string{"severity": "critical", "namespace": "oci_computeagent", "alarm_id": "ocid1.alarm.oc1..a"}
	putFirings(t, l, "oci-prod", t0, t0.Add(24*time.Hour), []firing.Firing{{Rule: "High CPU", Labels: labels, Start: s}})
	if !notify(t, l, Notification{At: s.Add(time.Minute), Source: "oci", Group: "k", Copied: "oci-prod",
		Alerts: []Alert{{Rule: "critical alarm at 15:34", Labels: labels, Start: s}}}) {
		t.Fatal("not kept")
	}
	ps, err := l.Pages(ctx, t0, t0.Add(24*time.Hour), twoMinutes)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].Rule != "High CPU" || len(ps[0].Firings) != 1 {
		t.Errorf("pages: %+v", ps)
	}
}
