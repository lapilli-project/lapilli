package ledger

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Why a notification was sent.
const (
	ReasonFirst    = "first"    // the group's first, or its first since all its alerts resolved
	ReasonNew      = "new"      // alerts the one before it didn't have
	ReasonResolved = "resolved" // alerts that resolved
	ReasonRepeat   = "repeat"   // the same alerts again, nothing new and nothing resolved
	ReasonUnknown  = "unknown"
)

// The stones of a notification, from the firings its alerts tell of.
const (
	PageWhite   = "white"   // one of them caught something
	PageBlack   = "black"   // each caught nothing
	PageExcused = "excused" // each was excused by a quiet window
	PageUntied  = "untied"  // one of its alerts tells of no firing the ledger has
)

// MaxAlerts is the most alerts one notification may carry.
const MaxAlerts = 1000

// Bounds on the names and labels a notification carries: they come from
// outside, and are shown on pages, in /metrics and to agents.
const (
	// maxReceiver is how long a receiver's name is, in characters; a rule's
	// is maxRuleName, as a firing copied in has it.
	maxReceiver = 200
	// maxGroup is how long a group key is kept, in characters: Alertmanager's
	// names the route's matchers and the group's labels, which can run
	// longer; such a key is kept as its hash, which tells groups apart as
	// well.
	maxGroup = 1000
	// maxMsg is how long a message's id is.
	maxMsg = 256
	// maxAlertLabels is how many labels an alert carries at most; together
	// they are maxLabelBytes at most.
	maxAlertLabels = 64
)

// ErrBadNotification is a notification the ledger won't keep: without
// alerts, with too many, or with an alert without its rule or start.
var ErrBadNotification = errors.New("not a notification the ledger keeps")

// sameWithin is how close together two notifications alike come when they
// are one sent twice: an Alertmanager in high availability sends from each
// replica rather than risk not sending.
const sameWithin = time.Minute

// Notification is one notification as it was sent.
type Notification struct {
	ID int64
	At time.Time
	// Source is alertmanager, grafana, sns, azure or google.
	Source, Receiver string
	// Group is the group it was sent for: the group key, or what stands for
	// one where the source gives none.
	Group string
	// Reason is why it was sent, as one of the Reason values; "" or unknown
	// when the source doesn't say, for the ledger to work out.
	Reason string
	// Msg is an id the source gives each message, if it gives one: one
	// kept already is the same message, sent again.
	Msg string
	// Copied is the source the alerts' firings are copied in as, when the
	// notification's URL names one (?source=): an alert then ties to that
	// source's rule that has the name its alarm sent, and its severity,
	// whatever name the ledger gives the rule. "" for none.
	Copied string
	// Shape is the kind of message it came as, where a path takes more
	// than one: cloudwatch or oci; "" for an alert manager's. A message
	// that doesn't say its alert's start takes it only from one of the same
	// shape.
	Shape  string
	Alerts []Alert
}

// Alert is an alert in a notification. A zero Start is one the message
// didn't say, as a cloud's message of an alarm resolving or repeating
// doesn't: it is taken from the last notification of the same group that
// told of the alert, and with none kept, it stays unknown and the alert ties
// to no firing.
type Alert struct {
	Rule     string
	Labels   map[string]string
	Start    time.Time
	Resolved bool
}

// Resolved reports whether every alert in it was.
func (n Notification) Resolved() bool {
	for _, a := range n.Alerts {
		if !a.Resolved {
			return false
		}
	}
	return len(n.Alerts) > 0
}

// key tells an alert apart from the others: its rule, labels and start.
func (a Alert) key() (string, error) {
	labels := a.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	b, err := json.Marshal(labels) // keys sorted
	if err != nil {
		return "", err
	}
	var start int64 // 0: not known, as the ledger keeps it
	if !a.Start.IsZero() {
		start = a.Start.Unix()
	}
	return a.Rule + "\x00" + string(b) + "\x00" + strconv.FormatInt(start, 10), nil
}

// digest is what two notifications alike share: where they were sent, for
// which group, and their alerts and how each stood.
func (n Notification) digest() (string, error) {
	lines := make([]string, 0, len(n.Alerts))
	for _, a := range n.Alerts {
		k, err := a.key()
		if err != nil {
			return "", err
		}
		lines = append(lines, k+"\x00"+strconv.FormatBool(a.Resolved))
	}
	slices.Sort(lines)
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00", n.Source, n.Receiver, n.Group)
	for _, l := range lines {
		h.Write([]byte(l))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// groupKey is a group key as it is kept: as sent, or its hash when it is
// long or has a character that doesn't show. A group key is only ever
// compared, never shown, and a 400 would make the alert manager drop the
// notification for good.
func groupKey(k string) string {
	if plain(k) && utf8.RuneCountInString(k) <= maxGroup {
		return k
	}
	h := sha256.Sum256([]byte(k))
	return "sha256:" + hex.EncodeToString(h[:])
}

// check checks a notification's names and labels before it is kept.
func (n Notification) check() error {
	switch {
	case !sane(n.At):
		return fmt.Errorf("%w: it needs when it came", ErrBadNotification)
	case !plain(n.Receiver) || utf8.RuneCountInString(n.Receiver) > maxReceiver:
		return fmt.Errorf("%w: a receiver's name is %d characters at most, with none that don't show", ErrBadNotification, maxReceiver)
	case !plain(n.Group) || utf8.RuneCountInString(n.Group) > maxGroup:
		return fmt.Errorf("%w: a group key is %d characters at most, with none that don't show", ErrBadNotification, maxGroup)
	case !plain(n.Msg) || len(n.Msg) > maxMsg:
		return fmt.Errorf("%w: a message's id is %d bytes at most", ErrBadNotification, maxMsg)
	case n.Copied != "" && !ValidSource(n.Copied):
		return fmt.Errorf("%w: a source is lowercase letters, digits and dashes, 40 at most, such as cloudwatch-prod", ErrBadNotification)
	}
	for i, a := range n.Alerts {
		k := i + 1
		switch {
		case a.Rule == "" || !a.Start.IsZero() && !sane(a.Start):
			return fmt.Errorf("%w: alert %d needs a rule, and a start that is a time", ErrBadNotification, k)
		case !plain(a.Rule) || utf8.RuneCountInString(a.Rule) > maxRuleName:
			return fmt.Errorf("%w: alert %d: a rule's name is %d characters at most, with none that don't show", ErrBadNotification, k, maxRuleName)
		case len(a.Labels) > maxAlertLabels:
			return fmt.Errorf("%w: alert %d has more than %d labels", ErrBadNotification, k, maxAlertLabels)
		}
		size := 0
		for name, v := range a.Labels {
			if name == "" || !plain(name) || !utf8.ValidString(v) {
				return fmt.Errorf("%w: alert %d: a label needs a name that shows, and labels are UTF-8", ErrBadNotification, k)
			}
			size += len(name) + len(v) + 6
		}
		if size > maxLabelBytes {
			return fmt.Errorf("%w: alert %d: its labels are 4 KB at most in all", ErrBadNotification, k)
		}
	}
	return nil
}

// AlertsSince is how many alerts the notifications kept since a time
// carried, by their source.
func (l *Ledger) AlertsSince(ctx context.Context, since time.Time) (map[string]int, error) {
	rows, err := l.r.QueryContext(ctx, `SELECT n.source, count(*) FROM notified d JOIN notifications n ON n.id = d.notification_id
		WHERE n.at >= ? GROUP BY n.source`, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var source string
		var n int
		if err := rows.Scan(&source, &n); err != nil {
			return nil, err
		}
		out[source] = n
	}
	return out, rows.Err()
}

// Notify keeps a notification. It reports false, and keeps nothing, for one
// kept already: the same message sent again, or the same notification from
// another replica within a minute. The reason for one whose source doesn't
// say is worked out from the one before it in its group. The stones don't
// change: a notification is counted against the firing it tells of, when
// read.
func (l *Ledger) Notify(ctx context.Context, n Notification) (bool, error) {
	if len(n.Alerts) == 0 {
		return false, fmt.Errorf("%w: it has no alerts", ErrBadNotification)
	}
	if len(n.Alerts) > MaxAlerts {
		return false, fmt.Errorf("%w: %d alerts in one notification; %d at most", ErrBadNotification, len(n.Alerts), MaxAlerts)
	}
	n.Group = groupKey(n.Group)
	if err := n.check(); err != nil {
		return false, err
	}
	n.Alerts = slices.Clone(n.Alerts)
	kept := false
	// Aside: one already kept, as from another replica, changes no page, and
	// throws away nothing read before.
	err := l.writeAside(ctx, func(t *txn) error {
		if err := l.fill(t, &n); err != nil {
			return err
		}
		digest, err := n.digest()
		if err != nil {
			return err
		}
		at := n.At.Unix()
		var x int
		if n.Msg != "" {
			err := t.QueryRowContext(t.ctx, "SELECT 1 FROM notifications WHERE msg = ? AND msg != '' LIMIT 1", n.Msg).Scan(&x)
			if err == nil {
				return nil
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		err = t.QueryRowContext(t.ctx, "SELECT 1 FROM notifications WHERE digest = ? AND at >= ? AND at <= ? LIMIT 1",
			digest, at-int64(sameWithin/time.Second), at+int64(sameWithin/time.Second)).Scan(&x)
		if err == nil {
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		reason := n.Reason
		if reason == "" || reason == ReasonUnknown {
			if reason, err = l.reason(t, n); err != nil {
				return err
			}
		}
		var id int64
		if err := t.QueryRowContext(t.ctx, `INSERT INTO notifications (at, source, receiver, grp, reason, resolved, digest, msg, copied, shape)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`, at, n.Source, n.Receiver, n.Group, reason, n.Resolved(), digest, n.Msg, n.Copied, n.Shape).Scan(&id); err != nil {
			return err
		}
		st, err := t.PrepareContext(t.ctx, "INSERT INTO notified (notification_id, rule, labels, start_at, firing) VALUES (?, ?, ?, ?, ?)")
		if err != nil {
			return err
		}
		defer st.Close()
		for _, a := range n.Alerts {
			labels := a.Labels
			if labels == nil {
				labels = map[string]string{}
			}
			b, err := json.Marshal(labels)
			if err != nil {
				return err
			}
			var start int64 // 0: not known
			if !a.Start.IsZero() {
				start = a.Start.Unix()
			}
			if _, err := st.ExecContext(t.ctx, id, a.Rule, string(b), start, !a.Resolved); err != nil {
				return err
			}
		}
		kept = true
		return nil
	})
	if kept {
		l.gen.Add(1)
	}
	return kept, err
}

// qStarts reads the starts of the alerts a group's last notifications told
// of: through notifications_group, then notified_of, never every alert of
// a rule.
const qStarts = `SELECT d.labels, d.start_at FROM notified d WHERE d.notification_id IN
	(SELECT id FROM notifications WHERE source = ? AND receiver = ? AND grp = ? AND at <= ? AND shape = ?
		ORDER BY at DESC, id DESC LIMIT ?)
	AND d.start_at > 0 ORDER BY d.notification_id DESC`

// startsFrom is how many of a group's last notifications an alert whose
// message didn't say its start looks for it in: however long ago they came,
// as an alarm can stay on for months and its OK say nothing of its start.
const startsFrom = 50

// fill gives each alert the message didn't say the start of the start of
// the last alert with its labels that a notification of the same group,
// receiver and shape told of, in the last startsFrom. It is one query for
// the notification, bounded by startsFrom, whatever its alerts: it runs
// while the ledger is written, which nothing else may wait long on. An
// alert's rule isn't asked: a cloud's message may title its alarm afresh
// each time, and its labels name it.
func (l *Ledger) fill(t *txn, n *Notification) error {
	if !slices.ContainsFunc(n.Alerts, func(a Alert) bool { return a.Start.IsZero() }) {
		return nil
	}
	at := n.At.Unix()
	rows, err := t.QueryContext(t.ctx, qStarts, n.Source, n.Receiver, n.Group, at, n.Shape, startsFrom)
	if err != nil {
		return err
	}
	defer rows.Close()
	last := map[string]int64{} // by labels, the latest
	for rows.Next() {
		var text string
		var start int64
		if err := rows.Scan(&text, &start); err != nil {
			return err
		}
		if _, ok := last[text]; !ok {
			last[text] = start
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range n.Alerts {
		a := &n.Alerts[i]
		if !a.Start.IsZero() {
			continue
		}
		labels, err := json.Marshal(noLabelsIfNil(a.Labels))
		if err != nil {
			return err
		}
		if start := last[string(labels)]; start > 0 {
			a.Start = unix(start)
		}
	}
	return nil
}

func noLabelsIfNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// reason works out why a notification was sent, from the last one sent to
// the same receiver for the same group: alerts resolved, alerts that one
// didn't have, the first after all resolved, or the same again.
func (l *Ledger) reason(t *txn, n Notification) (string, error) {
	for _, a := range n.Alerts {
		if a.Resolved {
			return ReasonResolved, nil
		}
	}
	var prev int64
	var resolved bool
	err := t.QueryRowContext(t.ctx, `SELECT id, resolved FROM notifications WHERE source = ? AND receiver = ? AND grp = ? AND at <= ?
		ORDER BY at DESC, id DESC LIMIT 1`, n.Source, n.Receiver, n.Group, n.At.Unix()).Scan(&prev, &resolved)
	if errors.Is(err, sql.ErrNoRows) || err == nil && resolved {
		return ReasonFirst, nil
	}
	if err != nil {
		return "", err
	}
	rows, err := t.QueryContext(t.ctx, "SELECT rule, labels, start_at FROM notified WHERE notification_id = ? AND firing = 1", prev)
	if err != nil {
		return "", err
	}
	had := map[string]bool{}
	for rows.Next() {
		var rule, labels string
		var start int64
		if err := rows.Scan(&rule, &labels, &start); err != nil {
			rows.Close()
			return "", err
		}
		had[rule+"\x00"+labels+"\x00"+strconv.FormatInt(start, 10)] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	for _, a := range n.Alerts {
		k, err := a.key()
		if err != nil {
			return "", err
		}
		if !had[k] {
			return ReasonNew, nil
		}
	}
	return ReasonRepeat, nil
}

// Notifications are the notifications that came in [from, to), the
// earliest first, with their alerts.
func (l *Ledger) Notifications(ctx context.Context, from, to time.Time) ([]Notification, error) {
	rows, err := l.r.QueryContext(ctx, `SELECT n.id, n.at, n.source, n.receiver, n.grp, n.reason, n.msg, n.copied, n.shape, d.rule, d.labels, d.start_at, d.firing
		FROM notifications n JOIN notified d ON d.notification_id = n.id
		WHERE n.at >= ? AND n.at < ? ORDER BY n.at, n.id`, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Notification
	labels := map[string]map[string]string{} // labels alike are read once
	for rows.Next() {
		var n Notification
		var a Alert
		var at, start int64
		var text string
		var firing bool
		if err := rows.Scan(&n.ID, &at, &n.Source, &n.Receiver, &n.Group, &n.Reason, &n.Msg, &n.Copied, &n.Shape, &a.Rule, &text, &start, &firing); err != nil {
			return nil, err
		}
		if a.Labels = labels[text]; a.Labels == nil {
			if err := json.Unmarshal([]byte(text), &a.Labels); err != nil {
				return nil, err
			}
			labels[text] = a.Labels
		}
		if start > 0 {
			a.Start = unix(start)
		}
		a.Resolved = !firing
		if len(out) == 0 || out[len(out)-1].ID != n.ID {
			n.At = unix(at)
			out = append(out, n)
		}
		last := &out[len(out)-1]
		last.Alerts = append(last.Alerts, a)
	}
	return out, rows.Err()
}

// NotifiedSince is, by rule, the start of the earliest alert of it any
// notification kept told of, or when the first notification came if that
// was later: from then on, a firing of the rule that no notification tells
// of paged no one, as far as the ledger can see. An alert on before the port
// was connected, told of resolving, doesn't make the firings before it look
// unpaged.
func (l *Ledger) NotifiedSince(ctx context.Context) (map[string]time.Time, error) {
	// Each source counts from when its own first notification came: one
	// connected later doesn't make the weeks before it look unpaged.
	first := map[string]int64{}
	if err := l.eachRow(ctx, func(r scanner) error {
		var src string
		var at int64
		if err := r.Scan(&src, &at); err != nil {
			return err
		}
		first[src] = at
		return nil
	}, "SELECT source, min(at) FROM notifications GROUP BY source"); err != nil {
		return nil, err
	}
	rec, err := l.recordedNames(ctx)
	if err != nil {
		return nil, err
	}
	type earliest struct {
		src, copied string
		a           Alert
		start       int64
	}
	var es []earliest
	var ids []string
	err = l.eachRow(ctx, func(r scanner) error {
		var e earliest
		var sev, alarm sql.NullString
		if err := r.Scan(&e.a.Rule, &sev, &alarm, &e.src, &e.copied, &e.start); err != nil {
			return err
		}
		e.a.Labels = map[string]string{"severity": sev.String}
		if alarm.String != "" {
			e.a.Labels["alarm_id"] = alarm.String
			if e.copied != "" && !rec.asked[alarm.String] {
				rec.asked[alarm.String] = true
				ids = append(ids, alarm.String)
			}
		}
		es = append(es, e)
		return nil
	}, `SELECT d.rule, json_extract(d.labels, '$.severity'), json_extract(d.labels, '$.alarm_id'), n.source, n.copied, min(d.start_at)
		FROM notified d JOIN notifications n ON n.id = d.notification_id WHERE d.start_at > 0 GROUP BY d.rule, 2, 3, n.source, n.copied`)
	if err != nil {
		return nil, err
	}
	if err := rec.alarmIDs(ctx, ids); err != nil {
		return nil, err
	}
	out := map[string]time.Time{}
	for _, e := range es {
		rule := rec.of(e.copied, e.a)
		at := unix(max(e.start, first[e.src]))
		if was, ok := out[rule]; !ok || at.Before(was) {
			out[rule] = at
		}
	}
	return out, nil
}

// recordedNames are the rules copied in, by their source, the name their
// source sent and their severity: an alert of a notification that names
// the source its firings are copied in as is of the rule it names there.
type recordedNames struct {
	byKey  map[[3]string]string
	bySent map[[2]string][]string
	// sent is, by a rule's name in the ledger, its source and the name
	// that source sent.
	sent map[string][2]string
	// alarms are, by source and alarm id, the name the source sent the
	// alarm's rule by: an alert that names its alarm by id is of that rule,
	// whatever title its message gave it. Read for the ids asked, by
	// alarms(), when a notification is read: one that came before its
	// alarm's firing was copied in ties once it is.
	alarms map[[2]string]string
	asked  map[string]bool
	l      *Ledger
}

func (l *Ledger) recordedNames(ctx context.Context) (*recordedNames, error) {
	r := &recordedNames{byKey: map[[3]string]string{}, bySent: map[[2]string][]string{}, sent: map[string][2]string{},
		alarms: map[[2]string]string{}, asked: map[string]bool{}, l: l}
	err := l.eachRow(ctx, func(row scanner) error {
		var name, source, sent, severity string
		if err := row.Scan(&name, &source, &sent, &severity); err != nil {
			return err
		}
		r.byKey[[3]string{source, sent, severity}] = name
		r.bySent[[2]string{source, sent}] = append(r.bySent[[2]string{source, sent}], name)
		r.sent[name] = [2]string{source, sent}
		return nil
	}, "SELECT name, source, sent, severity FROM rules WHERE source != ''")
	return r, err
}

// alarmsOf reads the rules of the alarms the alerts of notifications naming
// a source name by id, those not read already: a few hundred at a time,
// through series_alarm.
func (r *recordedNames) alarmsOf(ctx context.Context, ns []Notification) error {
	var ids []string
	for _, n := range ns {
		if n.Copied == "" {
			continue
		}
		for _, a := range n.Alerts {
			if id := a.Labels["alarm_id"]; id != "" && !r.asked[id] {
				r.asked[id] = true
				ids = append(ids, id)
			}
		}
	}
	return r.alarmIDs(ctx, ids)
}

func (r *recordedNames) alarmIDs(ctx context.Context, ids []string) error {
	for len(ids) > 0 {
		part := ids[:min(len(ids), 500)]
		ids = ids[len(part):]
		b, err := json.Marshal(part)
		if err != nil {
			return err
		}
		if err := r.l.eachRow(ctx, func(row scanner) error {
			var source, id, sent string
			if err := row.Scan(&source, &id, &sent); err != nil {
				return err
			}
			r.alarms[[2]string{source, id}] = sent
			return nil
		}, `SELECT DISTINCT r.source, json_extract(s.labels, '$.alarm_id'), r.sent FROM series s JOIN rules r ON r.id = s.rule_id
			WHERE json_extract(s.labels, '$.alarm_id') IS NOT NULL AND json_extract(s.labels, '$.alarm_id') IN (SELECT value FROM json_each(?))
			AND r.source != ''`, string(b)); err != nil {
			return err
		}
	}
	return nil
}

// of is the rule an alert is of: copied in from the source named, the rule
// of that source with the alarm the alert names by id, or with the alert's
// name and severity, or the one rule of it with that name; else the rule
// with the alert's name.
func (r *recordedNames) of(copied string, a Alert) string {
	if copied == "" {
		return a.Rule
	}
	if id := a.Labels["alarm_id"]; id != "" {
		if sent, ok := r.alarms[[2]string{copied, id}]; ok {
			a.Rule = sent
		}
	}
	if name, ok := r.byKey[[3]string{copied, a.Rule, a.Labels["severity"]}]; ok {
		return name
	}
	if names := r.bySent[[2]string{copied, a.Rule}]; len(names) == 1 {
		return names[0]
	}
	return a.Rule
}

// FirstNotification is when the earliest notification kept came; zero for
// none.
func (l *Ledger) FirstNotification(ctx context.Context) (time.Time, error) {
	var at sql.NullInt64
	err := l.r.QueryRowContext(ctx, "SELECT min(at) FROM notifications").Scan(&at)
	return fromNull(at), err
}

// Page is a notification as it bears on one rule: there is one for each
// rule a notification carries alerts of.
type Page struct {
	ID int64 // the notification's
	At time.Time
	// Rule is the rule; Source, Receiver and Reason, the notification's.
	Rule, Source, Receiver, Reason string
	// Resolved is set when it told only of the rule's alerts resolving.
	Resolved bool
	// Firings are the ledger's firings the rule's alerts in it tell of.
	Firings []int64
	// Stone is one of the Page values.
	Stone string
}

// Within is how far apart an alert's start and its firing's may be: two
// evaluations of the rule, as its group interval, or the replay's step,
// give them.
type Within func(rule string) time.Duration

// tied is a firing an alert may tell of.
type tied struct {
	id, start      int64
	labels         map[string]string
	white, excused bool
}

// tier ties alerts to firings, reading each rule's firings near the alerts'
// starts once.
type tier struct {
	l      *Ledger
	ctx    context.Context
	within Within
	// ids are the rule an alert's rule name is, by name. A recorded rule
	// named apart isn't found by the name its source sent: what the pages
	// count a notification against is the alert's rule's name.
	ids map[string][]int64
	// near are each rule name's firings near the alerts, by start.
	near map[string][]tied
	memo map[string]*tied
	// rec names the rule an alert of a notification naming its source is of.
	rec *recordedNames
}

func (l *Ledger) tier(ctx context.Context, within Within) (*tier, error) {
	t := &tier{l: l, ctx: ctx, within: within, ids: map[string][]int64{}, near: map[string][]tied{}, memo: map[string]*tied{}}
	var err error
	if t.rec, err = l.recordedNames(ctx); err != nil {
		return nil, err
	}
	rows, err := l.r.QueryContext(ctx, "SELECT id, name FROM rules")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		t.ids[name] = append(t.ids[name], id)
	}
	return t, rows.Err()
}

// load reads the firings of each rule that start near the alerts' starts:
// a span about each start, those that overlap read as one.
func (t *tier) load(alerts []Alert) error {
	starts := map[string][]int64{}
	for _, a := range alerts {
		if len(t.ids[a.Rule]) > 0 && !a.Start.IsZero() {
			starts[a.Rule] = append(starts[a.Rule], a.Start.Unix())
		}
	}
	for rule, ss := range starts {
		slices.Sort(ss)
		w := int64(t.within(rule) / time.Second)
		var spans [][2]int64
		for _, s := range ss {
			if n := len(spans); n > 0 && s-w <= spans[n-1][1] {
				spans[n-1][1] = s + w
			} else {
				spans = append(spans, [2]int64{s - w, s + w})
			}
		}
		ids, _ := json.Marshal(t.ids[rule])
		for _, sp := range spans {
			rows, err := t.l.r.QueryContext(t.ctx, `SELECT f.id, f.start_at, f.white, f.excused, s.id, s.labels FROM firings f
				JOIN series s ON s.id = f.series_id WHERE f.rule_id IN (SELECT value FROM json_each(?)) AND f.start_at >= ? AND f.start_at <= ?
				ORDER BY f.start_at, f.id`, string(ids), sp[0], sp[1])
			if err != nil {
				return err
			}
			for rows.Next() {
				var f tied
				var sid int64
				var text string
				if err := rows.Scan(&f.id, &f.start, &f.white, &f.excused, &sid, &text); err != nil {
					rows.Close()
					return err
				}
				if f.labels, err = t.l.labels(sid, text); err != nil {
					rows.Close()
					return err
				}
				t.near[rule] = append(t.near[rule], f)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}
	}
	return nil
}

// tie is the firing an alert tells of: its rule's firing whose labels are
// all among the alert's (which also carries labels the replay doesn't, such
// as Prometheus's external labels), started within two evaluations of the
// alert, the nearest; nil for none.
func (t *tier) tie(a Alert) (*tied, error) {
	k, err := a.key()
	if err != nil {
		return nil, err
	}
	if f, ok := t.memo[k]; ok {
		return f, nil
	}
	if a.Start.IsZero() {
		return nil, nil // when it started isn't known: of no firing
	}
	var best *tied
	w := int64(t.within(a.Rule) / time.Second)
	s := a.Start.Unix()
	// The rule's firings are in order of start, in spans that don't
	// overlap: those within w of the alert are found, not all looked at.
	near := t.near[a.Rule]
	from, _ := slices.BinarySearchFunc(near, s-w, func(f tied, at int64) int { return cmp.Compare(f.start, at) })
	for i := from; i < len(near) && near[i].start <= s+w; i++ {
		f := &near[i]
		d := abs(f.start - s)
		if !among(f.labels, a.Labels) {
			continue
		}
		if best == nil || d < abs(best.start-s) || d == abs(best.start-s) && f.id < best.id {
			best = f
		}
	}
	t.memo[k] = best
	return best, nil
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// among reports whether every label of a is in b, with the same value. An
// Azure resource's name and its group's are compared without their case:
// Azure writes them either way, and those kept before 0.21 weren't
// lower-cased. The alarm's own ID, which each firings recipe labels its
// firings with, is asked only of an alert that carries one: Azure's
// notifications don't, and a notification kept before v0.24 didn't; it is
// compared without its case, as Azure's ids are.
func among(a, b map[string]string) bool {
	for k, v := range a {
		w, ok := b[k]
		switch {
		case !ok && k == AlarmID:
		case !ok:
			return false
		case k == "resource" || k == "resource_group" || k == AlarmID:
			if ASCIILower(w) != ASCIILower(v) {
				return false
			}
		case w != v:
			return false
		}
	}
	return true
}

// ASCIILower lower-cases the ASCII letters of s and leaves the others, as
// jq's ascii_downcase does: the recipes' firings and the notifications'
// alerts are lower-cased alike.
func ASCIILower(s string) string {
	return strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}

// Pages are the notifications that came in [from, to), the earliest first:
// one for each rule each carries alerts of, tied to the firings those
// alerts tell of.
func (l *Ledger) Pages(ctx context.Context, from, to time.Time, within Within) ([]Page, error) {
	ns, err := l.Notifications(ctx, from, to)
	if err != nil {
		return nil, err
	}
	t, err := l.tier(ctx, within)
	if err != nil {
		return nil, err
	}
	if err := t.rec.alarmsOf(ctx, ns); err != nil {
		return nil, err
	}
	var all []Alert
	for _, n := range ns {
		for i := range n.Alerts {
			n.Alerts[i].Rule = t.rec.of(n.Copied, n.Alerts[i])
		}
		all = append(all, n.Alerts...)
	}
	if err := t.load(all); err != nil {
		return nil, err
	}
	var out []Page
	for _, n := range ns {
		by := map[string]*Page{}
		var order []string
		for _, a := range n.Alerts {
			p := by[a.Rule]
			if p == nil {
				p = &Page{ID: n.ID, At: n.At, Rule: a.Rule, Source: n.Source, Receiver: n.Receiver, Reason: n.Reason, Resolved: true}
				by[a.Rule] = p
				order = append(order, a.Rule)
			}
			p.Resolved = p.Resolved && a.Resolved
			f, err := t.tie(a)
			if err != nil {
				return nil, err
			}
			switch {
			case f == nil:
				p.Stone = worse(p.Stone, PageUntied)
			case f.white:
				p.Stone = worse(p.Stone, PageWhite)
			case f.excused:
				p.Stone = worse(p.Stone, PageExcused)
			default:
				p.Stone = worse(p.Stone, PageBlack)
			}
			if f != nil && !slices.Contains(p.Firings, f.id) {
				p.Firings = append(p.Firings, f.id)
			}
		}
		for _, rule := range order {
			out = append(out, *by[rule])
		}
	}
	return out, nil
}

// worse is the stone of a notification from two of its alerts': one that
// caught something makes it white; else one that tells of no firing makes
// it untied, since what it was for can't be said; else one that caught
// nothing makes it black.
func worse(a, b string) string {
	rank := func(s string) int {
		switch s {
		case PageWhite:
			return 4
		case PageUntied:
			return 3
		case PageBlack:
			return 2
		case PageExcused:
			return 1
		}
		return 0
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// NotifiedOf is, for each firing given, how many notifications told of it;
// those no notification told of are left out.
func (l *Ledger) NotifiedOf(ctx context.Context, fs []Firing, within Within) (map[int64]int, error) {
	out := map[int64]int{}
	if len(fs) == 0 {
		return out, nil
	}
	t, err := l.tier(ctx, within)
	if err != nil {
		return nil, err
	}
	type cand struct {
		n int64
		a Alert
	}
	var cands []cand
	seen := map[[2]int64]bool{} // a notification and a firing, counted once
	byRule := map[string][]Firing{}
	for _, f := range fs {
		byRule[f.Rule] = append(byRule[f.Rule], f)
	}
	for rule, rfs := range byRule {
		w := int64(within(rule) / time.Second)
		// A rule copied in is told of by the name its source sent, too.
		names := []string{rule}
		if s, ok := t.rec.sent[rule]; ok && s[1] != rule {
			names = append(names, s[1])
		}
		for _, f := range rfs {
			scan := func(query string, args ...any) error {
				rows, err := l.r.QueryContext(ctx, query, args...)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var c cand
					var text, copied string
					var start int64
					var firing bool
					if err := rows.Scan(&c.n, &c.a.Rule, &text, &start, &firing, &copied); err != nil {
						return err
					}
					if err := json.Unmarshal([]byte(text), &c.a.Labels); err != nil {
						return err
					}
					c.a.Start, c.a.Resolved = unix(start), !firing
					if c.a.Rule = t.rec.of(copied, c.a); c.a.Rule == rule && among(f.Labels, c.a.Labels) {
						cands = append(cands, c)
					}
				}
				return rows.Err()
			}
			for _, name := range names {
				if err := scan(`SELECT d.notification_id, d.rule, d.labels, d.start_at, d.firing, n.copied FROM notified d
					JOIN notifications n ON n.id = d.notification_id WHERE d.rule = ? AND d.start_at >= ? AND d.start_at <= ?`,
					name, f.Start.Unix()-w, f.Start.Unix()+w); err != nil {
					return nil, err
				}
			}
			// A rule copied in whose firings name their alarm by id is told of
			// by alerts that name it so, whatever title their message gave.
			if _, copied := t.rec.sent[rule]; copied && f.Labels["alarm_id"] != "" {
				id := f.Labels["alarm_id"]
				if !t.rec.asked[id] {
					t.rec.asked[id] = true
					if err := t.rec.alarmIDs(ctx, []string{id}); err != nil {
						return nil, err
					}
				}
				if err := scan(`SELECT d.notification_id, d.rule, d.labels, d.start_at, d.firing, n.copied FROM notified d
					JOIN notifications n ON n.id = d.notification_id WHERE json_extract(d.labels, '$.alarm_id') IS NOT NULL
					AND json_extract(d.labels, '$.alarm_id') = ? AND d.start_at >= ? AND d.start_at <= ? AND d.rule NOT IN (SELECT value FROM json_each(?))`,
					id, f.Start.Unix()-w, f.Start.Unix()+w, jsonList(names)); err != nil {
					return nil, err
				}
			}
		}
	}
	all := make([]Alert, len(cands))
	for i, c := range cands {
		all[i] = c.a
	}
	if err := t.load(all); err != nil {
		return nil, err
	}
	want := map[int64]bool{}
	for _, f := range fs {
		want[f.ID] = true
	}
	for _, c := range cands {
		f, err := t.tie(c.a)
		if err != nil {
			return nil, err
		}
		if f == nil || !want[f.id] || seen[[2]int64{c.n, f.id}] {
			continue
		}
		seen[[2]int64{c.n, f.id}] = true
		out[f.id]++
	}
	return out, nil
}

// Receivers names the receivers in a count by receiver, the most first.
func Receivers(by map[string]int) string {
	names := make([]string, 0, len(by))
	for r := range by {
		names = append(names, r)
	}
	slices.SortFunc(names, func(a, b string) int {
		if by[a] != by[b] {
			return by[b] - by[a]
		}
		return strings.Compare(a, b)
	})
	parts := make([]string, len(names))
	for i, r := range names {
		name := r
		if name == "" {
			name = "–"
		}
		parts[i] = fmt.Sprintf("%s %d", name, by[r])
	}
	return strings.Join(parts, ", ")
}

// jsonList is a list as JSON, for json_each.
func jsonList(xs []string) string {
	b, _ := json.Marshal(xs)
	return string(b)
}
