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
	Msg    string
	Alerts []Alert
}

// Alert is an alert in a notification.
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
	return a.Rule + "\x00" + string(b) + "\x00" + strconv.FormatInt(a.Start.Unix(), 10), nil
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
	}
	for i, a := range n.Alerts {
		k := i + 1
		switch {
		case a.Rule == "" || !sane(a.Start):
			return fmt.Errorf("%w: alert %d needs a rule and when it started", ErrBadNotification, k)
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
	digest, err := n.digest()
	if err != nil {
		return false, err
	}
	kept := false
	// Aside: one already kept, as from another replica, changes no page, and
	// throws away nothing read before.
	err = l.writeAside(ctx, func(t *txn) error {
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
		err := t.QueryRowContext(t.ctx, "SELECT 1 FROM notifications WHERE digest = ? AND at >= ? AND at <= ? LIMIT 1",
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
		if err := t.QueryRowContext(t.ctx, `INSERT INTO notifications (at, source, receiver, grp, reason, resolved, digest, msg)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`, at, n.Source, n.Receiver, n.Group, reason, n.Resolved(), digest, n.Msg).Scan(&id); err != nil {
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
			if _, err := st.ExecContext(t.ctx, id, a.Rule, string(b), a.Start.Unix(), !a.Resolved); err != nil {
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
	rows, err := l.r.QueryContext(ctx, `SELECT n.id, n.at, n.source, n.receiver, n.grp, n.reason, n.msg, d.rule, d.labels, d.start_at, d.firing
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
		if err := rows.Scan(&n.ID, &at, &n.Source, &n.Receiver, &n.Group, &n.Reason, &n.Msg, &a.Rule, &text, &start, &firing); err != nil {
			return nil, err
		}
		if a.Labels = labels[text]; a.Labels == nil {
			if err := json.Unmarshal([]byte(text), &a.Labels); err != nil {
				return nil, err
			}
			labels[text] = a.Labels
		}
		a.Start, a.Resolved = unix(start), !firing
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
	out := map[string]time.Time{}
	err := l.eachRow(ctx, func(r scanner) error {
		var rule, src string
		var start int64
		if err := r.Scan(&rule, &src, &start); err != nil {
			return err
		}
		at := unix(max(start, first[src]))
		if was, ok := out[rule]; !ok || at.Before(was) {
			out[rule] = at
		}
		return nil
	}, "SELECT d.rule, n.source, min(d.start_at) FROM notified d JOIN notifications n ON n.id = d.notification_id GROUP BY d.rule, n.source")
	return out, err
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
}

func (l *Ledger) tier(ctx context.Context, within Within) (*tier, error) {
	t := &tier{l: l, ctx: ctx, within: within, ids: map[string][]int64{}, near: map[string][]tied{}, memo: map[string]*tied{}}
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
		if len(t.ids[a.Rule]) > 0 {
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

// among reports whether every label of a is in b, with the same value.
func among(a, b map[string]string) bool {
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
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
	var all []Alert
	for _, n := range ns {
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
		for _, f := range rfs {
			rows, err := l.r.QueryContext(ctx, "SELECT notification_id, labels, start_at, firing FROM notified WHERE rule = ? AND start_at >= ? AND start_at <= ?",
				rule, f.Start.Unix()-w, f.Start.Unix()+w)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var c cand
				var text string
				var start int64
				var firing bool
				if err := rows.Scan(&c.n, &text, &start, &firing); err != nil {
					rows.Close()
					return nil, err
				}
				if err := json.Unmarshal([]byte(text), &c.a.Labels); err != nil {
					rows.Close()
					return nil, err
				}
				c.a.Rule, c.a.Start, c.a.Resolved = rule, unix(start), !firing
				if among(f.Labels, c.a.Labels) {
					cands = append(cands, c)
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return nil, err
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
