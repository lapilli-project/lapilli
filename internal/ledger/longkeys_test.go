package ledger

import (
	"context"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// wide is n characters of 4 bytes each that don't compress: as long, in
// bytes, as a text of n characters can be.
func wide(seed uint64, n int) string {
	r := rand.New(rand.NewPCG(seed, 7))
	var b strings.Builder
	for range n {
		b.WriteRune(rune(0x20000 + r.IntN(0xA6DF))) // CJK Ext. B
	}
	return b.String()
}

// Whatever SQLite keeps of any length, a rule file's or a sender's, the
// ledger keeps in Postgres too, longer than a btree holds (some 2,700
// bytes), and finds again: a rule's name as a rule file has it, as a
// note, a task and a comment take it when the rule is renamed, and as a
// proposal names it; a check of it; a group key and an alarm's id as a
// notification says them; a recorded rule's name and severity, and its
// alarm's id; an incident's id in a file.
func TestLongKeys(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	step := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	long := wide(1, 900) // 3,600 bytes
	record(t, l, t0, t0.Add(time.Hour), []firing.Firing{{Rule: long, Labels: map[string]string{"service": "a"}, Start: t0.Add(10 * time.Minute)}})
	var id int64
	step("the rule", l.r.QueryRowContext(ctx, "SELECT id FROM rules WHERE name = ?", long).Scan(&id))

	// A note, a task and a comment on a rule, renamed to the long name.
	step("note", l.PutNote(ctx, Note{Rule: "short", Note: "n"}, "ana"))
	_, err := l.PutTask(ctx, Task{Title: "t", Rule: "short"}, "ana")
	step("task", err)
	record(t, l, t0, t0.Add(2*time.Hour), []firing.Firing{{Rule: "short", Labels: map[string]string{"service": "a"}, Start: t0.Add(70 * time.Minute)}})
	_, err = l.PutComment(ctx, Comment{On: "rule", Key: "short", Body: "c"}, "ana")
	step("comment", err)
	var short int64
	step("the short rule", l.r.QueryRowContext(ctx, "SELECT id FROM rules WHERE name = 'short'").Scan(&short))
	other := wide(2, 900)
	step("rename", l.write(ctx, func(t *txn) error { return l.rename(t, short, "short", other) }))
	notes, err := l.Notes(ctx)
	if err != nil || notes[other].Note != "n" {
		t.Errorf("the note, renamed: %v %v", notes[other], err)
	}
	if cs, err := l.Comments(ctx, "rule", other); err != nil || len(cs) != 1 {
		t.Errorf("the comment, renamed: %v %v", cs, err)
	}
	if ts, err := l.Tasks(ctx, TaskQuery{}); err != nil || len(ts) != 1 || ts[0].Rule != other {
		t.Errorf("the task, renamed: %v %v", ts, err)
	}
	// A proposal for it, and the one unfinished a rule may have.
	_, err = l.Propose(ctx, Proposal{Rule: long, Kind: KindDisable, Path: PathNone, MadeBy: "ana"})
	step("proposal", err)
	if _, err := l.Propose(ctx, Proposal{Rule: long, Kind: KindDisable, Path: PathNone, MadeBy: "ana"}); err == nil {
		t.Error("a second unfinished proposal for the rule")
	}

	// A check of it, twice: the second keeps the day's again.
	_, seq, _ := l.AgainFrom(ctx)
	check := &verdict.SpanCheck{Rules: map[string]*verdict.SpanRule{long: {Replayed: 1, Real: 2,
		Misses: []verdict.Miss{{Way: "real", Firing: firing.Firing{Rule: long, Start: t0.Add(time.Hour), Labels: map[string]string{"service": "a"}}}}}},
		Elsewhere: map[string]int{long: 3}}
	for range 2 {
		step("check", l.PutCheck(ctx, t0, check, nil, t0.Add(24*time.Hour), seq))
	}
	if cs, err := l.Checks(ctx, t0, t0.Add(24*time.Hour)); err != nil || cs[long].Replayed != 1 {
		t.Errorf("checks: %v %v", cs[long], err)
	}
	if ms, err := l.CheckMisses(ctx, long); err != nil || len(ms) == 0 {
		t.Errorf("misses: %v %v", ms, err)
	}
	if e, err := l.CheckElsewhere(ctx, t0, t0.Add(24*time.Hour)); err != nil || e[long] != 3 {
		t.Errorf("elsewhere: %v %v", e, err)
	}

	// A notification's group key of 1,000 characters, twice; an alarm's
	// id of 800 in its labels.
	group, alarm := wide(3, maxGroup), wide(4, 800)
	for i := range 2 {
		at := t0.Add(time.Duration(i+1) * 10 * time.Minute)
		_, err := l.Notify(ctx, Notification{At: at, Source: "alertmanager", Receiver: wide(5, maxReceiver), Group: group,
			Alerts: []Alert{{Rule: "Errors", Labels: map[string]string{"alarm_id": alarm}, Start: t0}}})
		step("notification", err)
	}
	if ns, err := l.Notifications(ctx, t0, t0.Add(time.Hour)); err != nil || len(ns) != 2 || ns[0].Group != group {
		t.Errorf("notifications: %d %v", len(ns), err)
	}

	// A recorded rule of the longest name and a long severity, and an
	// alarm's id as long as its labels can hold.
	sent, sev, id2 := wide(6, maxRuleName), wide(7, 900), wide(8, 900)
	_, err = l.PutFirings(ctx, "cw", t0, t0.Add(24*time.Hour), []firing.Firing{
		{Rule: sent, Labels: map[string]string{"severity": sev}, Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)},
		{Rule: "cpu", Labels: map[string]string{AlarmID: id2}, Start: t0.Add(time.Hour)},
	}, FiringsCopier, opts)
	step("recorded firings", err)
	if on, err := l.FiringAlarm(ctx, "cw", id2, false); err != nil || !on {
		t.Errorf("the alarm of the long id: %v %v", on, err)
	}
	// A Google policy's id, found by its project named either way.
	_, err = l.PutFirings(ctx, "gcp", t0, t0.Add(24*time.Hour), []firing.Firing{
		{Rule: "policy", Labels: map[string]string{AlarmID: "projects/" + wide(9, 600) + "/alertPolicies/9"}, Start: t0.Add(time.Hour)},
	}, FiringsCopier, opts)
	step("a policy's firings", err)
	if on, err := l.FiringAlarm(ctx, "gcp", "projects/123/alertPolicies/9", false); err != nil || !on {
		t.Errorf("the policy by its project's number: %v %v", on, err)
	}

	// An incident's id in a file.
	key := wide(10, 900)
	_, err = l.SyncFile(ctx, []incident.Incident{{ID: key, Title: "down", Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)}}, opts)
	step("incidents", err)
	_, err = l.SyncFile(ctx, []incident.Incident{{ID: key, Title: "down, again", Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)}}, opts)
	step("incidents again", err)
	if ins, err := l.Incidents(ctx, t0, t0.Add(24*time.Hour), true); err != nil || len(ins) != 1 || ins[0].Title != "down, again" {
		t.Errorf("incidents: %v %v", ins, err)
	}
}
