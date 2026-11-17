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
	"maps"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// Marked windows (docs/design/marked.md): spans of time of a named kind (a
// sale, a launch, a campaign) that mark the firings and incidents that
// came during them and change nothing else. A firing judged that started
// during an occurrence of a window and agrees with it is marked with the
// window's kind (firings.marked, a bit a kind), and counted by day and week
// and kind (marked_days, marked_weeks), so every count can be read inside
// each kind and outside it. A quiet window excuses; a marked window never
// does.

// Where a marked window comes from: the marked file (--marked), --config,
// the UI and the API, or a PromQL expression (--mark). Each owns its own:
// the file and the expressions replace theirs as they are read again, and
// the UI changes none of theirs.
const (
	OriginFile   = "file"
	OriginConfig = "config"
	OriginUI     = "ui"
	OriginExpr   = "expr"
)

// Marked is a marked window: from Start to End, of Kind, repeating as a
// quiet window does (Every, On, Until, read by the same code), each repeat
// an occurrence of its own. Labels are matched as a deploy's are: a firing
// agrees on every label both have, one at least; a window that names none
// marks everything. A team's window (TeamID) marks its team's rules alone;
// one whose team was removed (its id negated), nothing.
type Marked struct {
	ID     int64     `json:"id"`
	Kind   string    `json:"kind"`
	Origin string    `json:"origin"`
	Title  string    `json:"title"`
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	Every  string    `json:"every,omitempty"`
	On     string    `json:"on,omitempty"`
	Until  time.Time `json:"until,omitzero"`
	TeamID int64     `json:"team_id,omitempty"`
	// Labels name what it is of: a launch of {service: search}.
	Labels  map[string]string `json:"labels,omitempty"`
	Updated time.Time         `json:"updated"`
	By      string            `json:"by,omitempty"`
	// Declared says --config declares it: the UI and the API don't change
	// or delete it.
	Declared  bool       `json:"declared,omitempty"`
	GivenBack bool       `json:"-"`
	kind      int64      // its kind's id, 1 to MaxKinds
	when      Window     // its times, as a quiet window's: when it repeats
	own       *ownership // who owns what, for a team's window
}

// Bounds on marked windows (docs/design/marked.md, "Cost and bounds"). A
// kind is a name of MaxKindName bytes at most, MaxKinds of them in the
// ledger (deploy, the sixteenth, is firings.deploy). Of the windows of a
// kind, the occurrences meeting any one week are MarkedKindWeek at most,
// and of every window, MarkedWeek. A window lasts maxWindow at most, as a
// quiet window, and starts no later than MarkedAhead after it is kept.
const (
	MaxKinds        = 15
	MaxKindName     = 32
	MarkedKindWeek  = 64
	MarkedWeek      = 256
	MarkedAhead     = 366 * 24 * time.Hour
	MaxMarkedFile   = 2000
	MaxMarkedConfig = 500
	MaxMarkedUI     = 500
	// MaxTeamMarked is how many of a team's its editors may keep.
	MaxTeamMarked = 20
	// MarkedUIWeek is the share of MarkedWeek the windows made in the UI
	// may take, MarkedUIKindWeek their share of each kind's
	// MarkedKindWeek, MarkedUIKinds their share of MaxKinds, and
	// MarkedTeamWeek a team's windows' share of MarkedWeek: the rest is the
	// marked file's, --config's and the expressions', so that what a
	// team's editors keep, or every editor's, never refuses them.
	MarkedUIWeek     = MarkedWeek / 2
	MarkedUIKindWeek = MarkedKindWeek / 2
	MarkedUIKinds    = MaxKinds / 2
	MarkedTeamWeek   = MarkedWeek / 8
	// MaxMarkedExpr is how many windows an expression keeps from a read;
	// MaxMarkExprs, how many expressions there are at most.
	MaxMarkedExpr = 1000
	MaxMarkExprs  = 8
)

// ReservedKinds are names no kind may have: deploy is the deploys', and
// week is each: week's in goals (docs/design/goals.md).
var ReservedKinds = []string{"deploy", "week"}

var kindName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// CheckKind refuses a kind's name a window can't have: [a-z][a-z0-9-],
// MaxKindName bytes at most, and neither of ReservedKinds.
func CheckKind(name string) error {
	switch {
	case len(name) > MaxKindName || !kindName.MatchString(name):
		return invalid("a kind of marked window is a name of up to %d bytes, a letter then letters, digits and -: sale, launch, campaign", MaxKindName)
	case slices.Contains(ReservedKinds, name):
		return invalid("%s isn't a kind of marked window: deploy is the deploys', and week is a goal's", name)
	}
	return nil
}

// Occurrence is one occurrence of a marked window: once for a window that
// doesn't repeat, each repeat for one that does.
type Occurrence struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// OccurrenceKey is what an occurrence is known by: its window's id and its
// start, in UTC (7@2026-11-06T11:00Z), which notices and tasks are keyed
// by.
func OccurrenceKey(id int64, start time.Time) string {
	u := start.UTC()
	f := "2006-01-02T15:04Z"
	if u.Second() != 0 {
		f = "2006-01-02T15:04:05Z"
	}
	return strconv.FormatInt(id, 10) + "@" + u.Format(f)
}

// resolve keeps the window's times as a quiet window holds them, in loc,
// for when it repeats.
func (m *Marked) resolve(loc *time.Location) {
	m.when = Window{Start: m.Start, End: m.End, Every: m.Every, On: m.On, Until: m.Until, loc: loc}
	m.when.resolve()
}

// Occurrences are the occurrences of the window that meet [from, to], the
// earliest first, most at most (over says there are more): the repeats a
// quiet window's schedule makes, the same code (Window.Covers) saying when.
func (m Marked) Occurrences(from, to time.Time, most int) (out []Occurrence, over bool) {
	long := m.End.Sub(m.Start)
	starts, over := m.when.occurrences(from, to, most)
	for _, s := range starts {
		out = append(out, Occurrence{s, s.Add(long)})
	}
	return out, over
}

// Covers reports whether a time falls in one of its occurrences.
func (m Marked) Covers(t time.Time) bool { return m.when.Covers(t) }

// Through is when the last of its occurrences up to now ends.
func (m Marked) Through(now time.Time) time.Time { return m.when.Through(now) }

// span is the firing starts it can mark, up to now.
func (m Marked) span(now time.Time) [2]int64 { return m.when.span(now) }

// occurrences are the starts of the repeats of a window that meet [from,
// to]: start at or before to, end at or after from, as Covers says each
// one's; the earliest first, most at most, over when there are more.
func (w Window) occurrences(from, to time.Time, most int) ([]time.Time, bool) {
	long := w.End.Sub(w.Start)
	if w.Start.After(to) {
		return nil, false
	}
	if w.Every == "" {
		if w.End.Before(from) {
			return nil, false
		}
		return []time.Time{w.Start}, most < 1
	}
	loc := w.where()
	var out []time.Time
	if w.scheduled() {
		w.resolve()
		if w.sch == nil {
			return nil, false
		}
		floor := from.Add(-long)
		for d := w.sch.Last(w.lastAt(to), loc); !d.IsZero() && !d.Before(w.Start) && !d.Before(floor); d = w.sch.Prev(d, loc) {
			if len(out) == most {
				slices.Reverse(out)
				return out, true
			}
			out = append(out, d)
		}
		slices.Reverse(out)
		return out, false
	}
	s, days := w.Start.In(loc), w.days()
	k := int((civil(from.Add(-long).In(loc))-civil(s))/int64(days)) - 1
	for n := max(k, 0); ; n++ {
		at := s.AddDate(0, 0, n*days)
		if at.After(to) || !w.Until.IsZero() && civil(at) > civil(w.Until.In(loc)) {
			break
		}
		if at.Add(long).Before(from) {
			continue
		}
		if len(out) == most {
			return out, true
		}
		out = append(out, at)
	}
	return out, false
}

// marks reports whether the window marks a firing in one of its
// occurrences: it agrees with the window's labels, as a deploy's are
// agreed with, and is of the window's team's rules, for a team's.
func (m *Marked) marks(f firing.Firing) bool {
	if !deployOf(m.Labels, f.Labels) {
		return false
	}
	switch {
	case m.TeamID < 0:
		return false
	case m.TeamID > 0 && (m.own == nil || m.own.of(f.Rule, f.Labels) != m.TeamID):
		return false
	}
	return true
}

// Agrees reports whether an incident's labels agree with the window's, as
// a firing's do: on every label both have, one at least, any of an
// incident's values agreeing; a window that names none, any incident's.
func (m Marked) Agrees(labels incident.Labels) bool {
	if len(m.Labels) == 0 {
		return true
	}
	shared := false
	for k, v := range m.Labels {
		vs, ok := labels[k]
		if !ok {
			continue
		}
		if !slices.Contains(vs, v) {
			return false
		}
		shared = true
	}
	return shared
}

// markLooked, when set, is told how many occurrences each firing was set
// beside: for TestScale, which holds it.
var markLooked func(n int)

// markedMost is how many occurrences of one window a span works out at
// most: past it, each firing asks Covers.
const markedMost = dueMost

// marking says, for the firings judged over a span [from, to], the kinds
// each is marked with, a bit each. The occurrences meeting the span are
// worked out once, and kept by the weeks they meet (week, a day's Monday
// as the ledger counts them), sorted by start: a firing is set beside
// those of its week that started within the longest of them before its
// start, found by a search, as deploys are (during). So it is set beside
// MarkedWeek at most, and, for windows of hours, a few.
func marking(ms []Marked, from, to time.Time, week func(int64) int64) func(firing.Firing) int64 {
	type occ struct {
		start, end int64
		m          int
	}
	type weekOccs struct {
		os      []occ
		longest int64
	}
	by := map[int64]*weekOccs{}
	var slow []int // windows of more occurrences than markedMost in the span
	w1, w2 := week(from.Unix()), week(to.Unix())
	for i := range ms {
		m := &ms[i]
		if m.TeamID < 0 || m.kind < 1 || m.kind > MaxKinds {
			continue
		}
		starts, over := m.when.occurrences(from, to, markedMost)
		if over {
			slow = append(slow, i)
			continue
		}
		long := m.End.Unix() - m.Start.Unix()
		for _, s := range starts {
			o := occ{s.Unix(), s.Unix() + long, i}
			for w := max(week(o.start), w1); w <= min(week(o.end), w2); w += 7 {
				x := by[w]
				if x == nil {
					x = &weekOccs{}
					by[w] = x
				}
				x.os = append(x.os, o)
				x.longest = max(x.longest, long)
			}
		}
	}
	if len(by) == 0 && len(slow) == 0 {
		return func(firing.Firing) int64 { return 0 }
	}
	for _, x := range by {
		slices.SortFunc(x.os, func(a, b occ) int { return cmp.Or(cmp.Compare(a.start, b.start), cmp.Compare(a.m, b.m)) })
	}
	return func(f firing.Firing) int64 {
		at := f.Start.Unix()
		var bits int64
		looked := 0
		if x := by[week(at)]; x != nil {
			n := sort.Search(len(x.os), func(i int) bool { return x.os[i].start > at })
			for i := n - 1; i >= 0 && x.os[i].start >= at-x.longest; i-- {
				looked++
				o := x.os[i]
				m := &ms[o.m]
				if bit := int64(1) << m.kind; at <= o.end && bits&bit == 0 && m.marks(f) {
					bits |= bit
				}
			}
		}
		for _, i := range slow {
			looked++
			m := &ms[i]
			if bit := int64(1) << m.kind; bits&bit == 0 && m.marks(f) && m.when.Covers(f.Start) {
				bits |= bit
			}
		}
		if markLooked != nil {
			markLooked(looked)
		}
		return bits
	}
}

// markedColumns are what a marked window is read with, from marked m
// joined to its kind k.
const markedColumns = `m.id, k.id, k.name, m.origin, m.title, m.start_at, m.end_at, m.every, m.repeat_on, m.until_at, m.team_id, m.labels,
	m.declared, m.given_back, m.updated, m.updated_by`

// markedRows are the marked windows a where clause on m and k picks, each
// with its times read and, for a team's, who owns what.
func (l *Ledger) markedRows(ctx context.Context, q queryer, where string, args ...any) ([]Marked, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+markedColumns+" FROM marked m JOIN marked_kinds k ON k.id = m.kind_id "+where, args...)
	if err != nil {
		return nil, err
	}
	var out []Marked
	teams := false
	for rows.Next() {
		var m Marked
		var start, end, at int64
		var until sql.NullInt64
		var labels string
		if err := rows.Scan(&m.ID, &m.kind, &m.Kind, &m.Origin, &m.Title, &start, &end, &m.Every, &m.On, &until, &m.TeamID, &labels,
			&m.Declared, &m.GivenBack, &at, &m.By); err != nil {
			rows.Close()
			return nil, err
		}
		m.Start, m.End, m.Until, m.Updated = unix(start), unix(end), fromNull(until), unix(at)
		if err := json.Unmarshal([]byte(labels), &m.Labels); err != nil {
			rows.Close()
			return nil, err
		}
		if len(m.Labels) == 0 {
			m.Labels = nil
		}
		m.resolve(l.loc)
		teams = teams || m.TeamID > 0
		out = append(out, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil || !teams {
		return out, err
	}
	own, err := l.ownership(ctx, q)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].own = own
	}
	return out, nil
}

// markedMeeting are the marked windows with an occurrence that may meet
// [lo, hi], the earliest first: as windowsMeeting finds quiet windows.
func (l *Ledger) markedMeeting(ctx context.Context, q queryer, lo, hi int64) ([]Marked, error) {
	return l.markedRows(ctx, q, `WHERE (m.every = '' AND m.start_at <= ? AND m.start_at >= ? AND m.end_at >= ?)
		OR (m.every != '' AND m.start_at <= ? AND (m.until_at IS NULL OR m.until_at >= ?)) ORDER BY m.start_at, m.id`,
		hi, lo-int64(maxWindow/time.Second), lo, hi, lo-int64((maxWindow+48*time.Hour)/time.Second))
}

// MarkedMeeting are the marked windows with an occurrence that may meet
// [from, to], the earliest first.
func (l *Ledger) MarkedMeeting(ctx context.Context, from, to time.Time) ([]Marked, error) {
	return l.markedMeeting(ctx, l.r, from.Unix(), to.Unix())
}

// MarkedByID is one marked window.
func (l *Ledger) MarkedByID(ctx context.Context, id int64) (Marked, error) {
	ms, err := l.markedRows(ctx, l.r, "WHERE m.id = ?", id)
	if err != nil {
		return Marked{}, err
	}
	if len(ms) == 0 {
		return Marked{}, ErrNotFound
	}
	return ms[0], nil
}

// AllMarked are every marked window, by id.
func (l *Ledger) AllMarked(ctx context.Context) ([]Marked, error) {
	return l.markedRows(ctx, l.r, "ORDER BY m.id")
}

// MarkedKinds are the names of the kinds the ledger keeps, in order.
func (l *Ledger) MarkedKinds(ctx context.Context) ([]string, error) {
	ks, err := l.kinds(ctx, l.r)
	if err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Values(ks)), nil
}

// kinds are the kinds the ledger keeps, by id.
func (l *Ledger) kinds(ctx context.Context, q queryer) (map[int64]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT id, name FROM marked_kinds")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// KindsOf are the names of the kinds bits marks, in order.
func KindsOf(bits int64, kinds map[int64]string) []string {
	if bits == 0 {
		return nil
	}
	var out []string
	for id := int64(1); id <= MaxKinds; id++ {
		if bits&(int64(1)<<id) != 0 {
			if name, ok := kinds[id]; ok {
				out = append(out, name)
			}
		}
	}
	slices.Sort(out)
	return out
}

// kindID is the id of the kind of that name, made if there is none: the
// lowest of 1 to MaxKinds not taken.
func (l *Ledger) kindID(t *txn, name string) (int64, error) {
	if err := CheckKind(name); err != nil {
		return 0, err
	}
	have, err := l.kinds(t.ctx, t)
	if err != nil {
		return 0, err
	}
	for id, n := range have {
		if n == name {
			return id, nil
		}
	}
	for id := int64(1); id <= MaxKinds; id++ {
		if _, taken := have[id]; taken {
			continue
		}
		if _, err := t.ExecContext(t.ctx, "INSERT INTO marked_kinds (id, name) VALUES (?, ?)", id, name); err != nil {
			return 0, err
		}
		t.marking = 1
		return id, nil
	}
	return 0, invalid("the ledger keeps %d kinds of marked window at most: use one there is, or delete the windows of one no longer needed", MaxKinds)
}

// letGoKinds lets go of the kinds no window or expression names, once
// nothing is left to judge again: the firings they marked are marked no
// more, so their ids can be given to others.
func (l *Ledger) letGoKinds(t *txn) error {
	res, err := t.ExecContext(t.ctx, `DELETE FROM marked_kinds WHERE id NOT IN (SELECT kind_id FROM marked)
		AND 'marked-expr.' || name NOT IN (SELECT key FROM meta WHERE key > 'marked-expr.' AND key < 'marked-expr/')`)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		t.marking = 0
		_, err = t.ExecContext(t.ctx, "DELETE FROM marked_days WHERE kind_id NOT IN (SELECT id FROM marked_kinds)")
		if err == nil {
			_, err = t.ExecContext(t.ctx, "DELETE FROM marked_weeks WHERE kind_id NOT IN (SELECT id FROM marked_kinds)")
		}
		return err
	}
	return nil
}

// marks says whether the ledger keeps any kind, and so any mark, read once
// a write.
func (t *txn) marks() (bool, error) {
	if t.marking == 0 {
		var n int
		if err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM (SELECT 1 FROM marked_kinds LIMIT 1) AS one").Scan(&n); err != nil {
			return false, err
		}
		t.marking = 2
		if n > 0 {
			t.marking = 1
		}
	}
	return t.marking == 1, nil
}

// checkMarked checks a marked window as it is kept at at, and puts it as
// it is kept: its title trimmed, its schedule read, as checkWindow does a
// quiet window's.
func (l *Ledger) checkMarked(m *Marked, at time.Time) error {
	if err := CheckKind(m.Kind); err != nil {
		return err
	}
	m.Start, m.End, m.Until = m.Start.Truncate(time.Second), m.End.Truncate(time.Second), m.Until.Truncate(time.Second)
	if !slices.Contains([]string{OriginFile, OriginConfig, OriginUI, OriginExpr}, m.Origin) {
		m.Origin = OriginUI
	}
	m.Title = strings.TrimSpace(m.Title)
	switch {
	case m.Title == "":
		return invalid("a marked window needs a title")
	case !plain(m.Title) || Hidden(m.Title):
		return invalid("a marked window's title is one line, with no character that doesn't show")
	case utf8.RuneCountInString(m.Title) > 200:
		return invalid("a title is %d characters at most", 200)
	case m.Start.IsZero() || m.End.IsZero() || !m.End.After(m.Start):
		return invalid("a marked window needs a start, and an end after it")
	case m.End.Sub(m.Start) > maxWindow:
		return invalid("a marked window lasts 31 days at most: write a longer one as two")
	case m.Start.Year() < 1970 || m.End.Year() > 9999:
		return invalid("a marked window's times are from 1970 to 9999")
	case m.Origin != OriginExpr && m.Start.After(at.Add(MarkedAhead)):
		return invalid("a marked window starts no later than a year ahead")
	case len(m.Labels) > 16:
		return invalid("a marked window names 16 labels at most")
	case labelBytes(m.Labels) > 4096:
		return invalid("a marked window's labels are 4 KB at most")
	case m.TeamID < 0:
		return invalid("no such team")
	}
	for k, v := range m.Labels {
		if k == "" || v == "" || !utf8.ValidString(k) || !utf8.ValidString(v) || len(k) > 128 || len(v) > 256 {
			return invalid("a marked window's labels each have a name of up to 128 bytes and a value of up to 256")
		}
	}
	// Its repeats are a quiet window's: the same schedule, read by the
	// same code.
	w := Window{Start: m.Start, End: m.End, Every: m.Every, On: strings.TrimSpace(m.On), Until: m.Until}
	switch {
	case w.Every != "" && !slices.Contains(Everies, w.Every):
		return invalid("a marked window repeats every day, week or month, or by a cron expression")
	case w.Every == "day" && w.End.Sub(w.Start) >= 24*time.Hour, w.Every == "week" && w.End.Sub(w.Start) >= 7*24*time.Hour:
		return invalid("a repeating marked window is shorter than how often it repeats")
	case w.Every != "" && !w.Until.IsZero() && w.Until.Before(w.Start.Truncate(24*time.Hour)):
		return invalid("a marked window repeats until a day after it starts")
	case len(w.On) > 100:
		return invalid("a marked window's schedule is 100 characters at most")
	}
	if w.Every == "" {
		w.Until = time.Time{}
	}
	if !w.scheduled() {
		w.On = ""
	} else {
		sch, err := w.schedule(l.loc)
		if err != nil {
			return err
		}
		if !sch.Last(w.Start, l.loc).Equal(w.Start) {
			return invalid("a marked window that repeats starts at a time its schedule falls due: the next is %s", sch.Next(w.Start, l.loc).Format("2006-01-02 15:04"))
		}
		if gap := sch.Gap(w.Start, l.loc); gap > 0 && w.End.Sub(w.Start) >= gap {
			return invalid("a repeating marked window is shorter than how often it repeats")
		}
		if w.Every == "month" && w.On == "" {
			w.On = strconv.Itoa(w.Start.In(l.loc).Day())
		}
		w.On = strings.ToLower(strings.Join(strings.Fields(w.On), " "))
	}
	m.On, m.Until = w.On, w.Until
	m.resolve(l.loc)
	return nil
}

// markedLoad is how many occurrences of marked windows meet each week, of
// each kind and in all, to hold them to MarkedKindWeek and MarkedWeek: a
// window that doesn't repeat counts in each week it meets; one that
// repeats, the most of its occurrences any week meets, in every week from
// its first to its last (for good, with no until).
type markedLoad struct {
	l       *Ledger
	once    map[int64]map[string]int // by week's Monday, by kind
	repeats []repeatLoad
}

type repeatLoad struct {
	kind     string
	from, to int64 // the Mondays of its first week and its last
	most     int
}

func (l *Ledger) newLoad() *markedLoad { return &markedLoad{l: l, once: map[int64]map[string]int{}} }

// weeks are the Mondays of the weeks [a, b] meets.
func (ld *markedLoad) weeks(a, b time.Time) (int64, int64) {
	return Week(ld.l.day(a.Unix())), Week(ld.l.day(b.Unix()))
}

// add counts a window's occurrences.
func (ld *markedLoad) add(m Marked) {
	if m.Every == "" {
		w1, w2 := ld.weeks(m.Start, m.End)
		for w := w1; w <= w2; w += 7 {
			if ld.once[w] == nil {
				ld.once[w] = map[string]int{}
			}
			ld.once[w][m.Kind]++
		}
		return
	}
	from, _ := ld.weeks(m.Start, m.Start)
	to := int64(math.MaxInt64)
	if !m.Until.IsZero() {
		_, to = ld.weeks(m.Start, m.Until.AddDate(0, 0, 32))
	}
	ld.repeats = append(ld.repeats, repeatLoad{m.Kind, from, to, ld.most(m)})
}

// most is the most of a repeating window's occurrences that meet any one
// week, MarkedKindWeek and one at most: over the weeks its schedule takes
// to come round, three for every day or week, a year's for a month or a
// cron expression.
func (ld *markedLoad) most(m Marked) int {
	if m.when.loc == nil {
		m.resolve(ld.l.loc)
	}
	n := 3
	if m.when.scheduled() {
		n = 54
	}
	first, _ := ld.weeks(m.Start, m.Start)
	most := 0
	for w := first; w < first+int64(7*n); w += 7 {
		got, over := m.Occurrences(ld.l.Midnight(w), ld.l.Midnight(w+7).Add(-time.Second), MarkedKindWeek)
		if over {
			return MarkedKindWeek + 1
		}
		most = max(most, len(got))
	}
	return most
}

// at is the load of a week: in all, and the most of any kind, and which.
func (ld *markedLoad) at(w int64) (all int, kinds map[string]int) {
	kinds = maps.Clone(ld.once[w])
	if kinds == nil {
		kinds = map[string]int{}
	}
	for _, r := range ld.repeats {
		if r.from <= w && w <= r.to {
			kinds[r.kind] += r.most
		}
	}
	for _, n := range kinds {
		all += n
	}
	return all, kinds
}

// fits reports whether one more occurrence of kind fits in each of the
// weeks [a, b] meets.
func (ld *markedLoad) fits(kind string, a, b time.Time) bool {
	w1, w2 := ld.weeks(a, b)
	for w := w1; w <= w2; w += 7 {
		all, kinds := ld.at(w)
		if all+1 > MarkedWeek || kinds[kind]+1 > MarkedKindWeek {
			return false
		}
	}
	return true
}

// rises are the weeks where the load may rise, in order: a week with an
// occurrence of one that doesn't repeat, or where one that repeats
// starts. The most a load holds is in one of them.
func (ld *markedLoad) rises() []int64 {
	var at []int64
	for w := range ld.once {
		at = append(at, w)
	}
	for _, r := range ld.repeats {
		at = append(at, r.from)
	}
	slices.Sort(at)
	return slices.Compact(at)
}

// check refuses a load past the bounds, saying the first week past them.
func (ld *markedLoad) check() error {
	for _, w := range ld.rises() {
		all, kinds := ld.at(w)
		for _, k := range slices.Sorted(maps.Keys(kinds)) {
			if kinds[k] > MarkedKindWeek {
				return invalid("the windows of kind %s would have %d occurrences in the week of %s; a week meets %d of a kind at most",
					k, kinds[k], ld.l.Midnight(w).Format("2006-01-02"), MarkedKindWeek)
			}
		}
		if all > MarkedWeek {
			return invalid("the marked windows would have %d occurrences in the week of %s; a week meets %d at most",
				all, ld.l.Midnight(w).Format("2006-01-02"), MarkedWeek)
		}
	}
	return nil
}

// fitsAll refuses windows whose occurrences, with the others kept, are
// past the bounds, or that name more kinds than the ledger keeps: those
// kept that keep says stay, and those given.
func (l *Ledger) fitsAll(t *txn, keep func(Marked) bool, add []Marked) error {
	have, err := l.markedRows(t.ctx, t, "ORDER BY m.id")
	if err != nil {
		return err
	}
	ld := l.newLoad()
	for _, m := range have {
		if keep(m) {
			ld.add(m)
		}
	}
	// The kinds the ledger keeps: of windows, of expressions, and those
	// not let go yet.
	held, err := l.kinds(t.ctx, t)
	if err != nil {
		return err
	}
	kinds := map[string]bool{}
	for _, k := range held {
		kinds[k] = true
	}
	for _, m := range add {
		ld.add(m)
		kinds[m.Kind] = true
	}
	if len(kinds) > MaxKinds {
		return invalid("the ledger keeps %d kinds of marked window at most: use one there is, or delete the windows of one no longer needed", MaxKinds)
	}
	return ld.check()
}

// shareFits refuses a window made in the UI, m in place of the window id
// (0 for none), past the UI's shares: of the occurrences a week meets
// (MarkedUIWeek), of those of m's kind (MarkedUIKindWeek), and of the
// kinds (MarkedUIKinds); or, of a team's, past its team's share of a
// week's (MarkedTeamWeek).
func (l *Ledger) shareFits(t *txn, id int64, m Marked) error {
	have, err := l.markedRows(t.ctx, t, "WHERE m.origin = 'ui' ORDER BY m.id")
	if err != nil {
		return err
	}
	// The shares' loads as they are, and with m in place of the window id.
	was, ui, wasTeam, team := l.newLoad(), l.newLoad(), l.newLoad(), l.newLoad()
	for _, h := range have {
		ours := m.TeamID > 0 && h.TeamID == m.TeamID
		was.add(h)
		if ours {
			wasTeam.add(h)
		}
		if h.ID == id {
			continue
		}
		ui.add(h)
		if ours {
			team.add(h)
		}
	}
	ui.add(m)
	team.add(m)
	// Past a share, as windows --config gave back can be, or those kept
	// before there were shares, a write is refused in a week it makes
	// busier, so that one that adds nothing, a new title or a shorter
	// window, is let be.
	if w, n := ui.busier(was, "", MarkedUIWeek); n > 0 {
		return invalid("the marked windows made in the UI would have %d occurrences in the week of %s; they have %d a week at most, the rest kept for the marked file, --config and the expressions",
			n, l.Midnight(w).Format("2006-01-02"), MarkedUIWeek)
	}
	if w, n := ui.busier(was, m.Kind, MarkedUIKindWeek); n > 0 {
		return invalid("the marked windows of kind %s made in the UI would have %d occurrences in the week of %s; they have %d of a kind a week at most, the rest kept for the marked file, --config and the expressions",
			m.Kind, n, l.Midnight(w).Format("2006-01-02"), MarkedUIKindWeek)
	}
	if w, n := team.busier(wasTeam, "", MarkedTeamWeek); m.TeamID > 0 && n > 0 {
		return invalid("a team's marked windows would have %d occurrences in the week of %s; a team's have %d a week at most", n, l.Midnight(w).Format("2006-01-02"), MarkedTeamWeek)
	}
	// Past their share of the kinds, as windows --config gave back can
	// take them, a write that takes no more than they take already, such
	// as a new title, is let be.
	now, after, err := l.uiKinds(t, have, id, m)
	if err != nil {
		return err
	}
	if after > MarkedUIKinds && after > now {
		return invalid("the marked windows made in the UI would take %d kinds, counting those no window names any more until their firings are judged again; they take %d at most, the rest kept for the marked file, --config and the expressions: use a kind there is",
			after, MarkedUIKinds)
	}
	return nil
}

// uiKinds are how many kinds the windows made in the UI, ui, take, now and
// with m in place of the window id (0 for none): the kinds they name, and
// those no window or expression names that aren't let go yet
// (letGoKinds). Those count among the UI's, whoever let go of them, so
// that windows made and deleted, or changed from kind to kind, faster than
// their kinds are let go take none of the kinds left to the rest.
func (l *Ledger) uiKinds(t *txn, ui []Marked, id int64, m Marked) (now, after int, err error) {
	held, err := l.kinds(t.ctx, t)
	if err != nil {
		return 0, 0, err
	}
	// Of each kind a window names, how many do, and the least id: whether
	// one but the window id does.
	type naming struct{ n, least int64 }
	named := map[string]naming{}
	rows, err := t.QueryContext(t.ctx, "SELECT k.name, count(*), min(m.id) FROM marked m JOIN marked_kinds k ON k.id = m.kind_id GROUP BY k.name")
	if err != nil {
		return 0, 0, err
	}
	for rows.Next() {
		var k string
		var x naming
		if err := rows.Scan(&k, &x.n, &x.least); err != nil {
			rows.Close()
			return 0, 0, err
		}
		named[k] = x
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, 0, err
	}
	exprs := map[string]bool{}
	rows, err = t.QueryContext(t.ctx, "SELECT key FROM meta WHERE key > 'marked-expr.' AND key < 'marked-expr/'")
	if err != nil {
		return 0, 0, err
	}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return 0, 0, err
		}
		exprs[strings.TrimPrefix(key, "marked-expr.")] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, 0, err
	}
	was, will := map[string]bool{}, map[string]bool{m.Kind: true}
	for _, h := range ui {
		was[h.Kind] = true
		if h.ID != id {
			will[h.Kind] = true
		}
	}
	for _, k := range held {
		x, ok := named[k]
		switch {
		case exprs[k]:
		case !ok:
			was[k], will[k] = true, true
		case x.n == 1 && x.least == id:
			will[k] = true // the window id alone names it, which m takes the place of
		}
	}
	return len(was), len(will), nil
}

// busier is the first week where ld meets more than share occurrences, of
// kind or, "", of every kind, and more than before does, and how many; 0
// for none. Each week either load changes in is looked at (turns), so
// none between them can be.
func (ld *markedLoad) busier(before *markedLoad, kind string, share int) (week int64, n int) {
	weeks := append(ld.turns(), before.turns()...)
	slices.Sort(weeks)
	for _, w := range slices.Compact(weeks) {
		if now := ld.of(w, kind); now > share && now > before.of(w, kind) {
			return w, now
		}
	}
	return 0, 0
}

// turns are the weeks where the load may change: where it rises (rises),
// and the week after an occurrence of one that doesn't repeat, or after
// the last of one that repeats until a day.
func (ld *markedLoad) turns() []int64 {
	at := ld.rises()
	for w := range ld.once {
		at = append(at, w+7)
	}
	for _, r := range ld.repeats {
		if r.to != math.MaxInt64 {
			at = append(at, r.to+7)
		}
	}
	return at
}

// of is how many occurrences meet the week w, of kind or, "", of every
// kind.
func (ld *markedLoad) of(w int64, kind string) int {
	all, kinds := ld.at(w)
	if kind != "" {
		return kinds[kind]
	}
	return all
}

// insertMarked keeps a window in a write.
func (l *Ledger) insertMarked(t *txn, m Marked, by string) (int64, error) {
	kind, err := l.kindID(t, m.Kind)
	if err != nil {
		return 0, err
	}
	var id int64
	err = t.QueryRowContext(t.ctx, `INSERT INTO marked (kind_id, origin, title, start_at, end_at, every, repeat_on, until_at, team_id, labels, declared, updated, updated_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		kind, m.Origin, m.Title, m.Start.Unix(), m.End.Unix(), m.Every, m.On, nullable(m.Until), m.TeamID, t.d.kept(deployLabelsText(m.Labels)),
		m.Declared, time.Now().Unix(), by).Scan(&id)
	return id, err
}

// updateMarked changes a window in a write.
func (l *Ledger) updateMarked(t *txn, id int64, m Marked, by string) error {
	kind, err := l.kindID(t, m.Kind)
	if err != nil {
		return err
	}
	_, err = t.ExecContext(t.ctx, `UPDATE marked SET kind_id = ?, title = ?, start_at = ?, end_at = ?, every = ?, repeat_on = ?, until_at = ?, team_id = ?,
		labels = ?, declared = ?, given_back = 0, updated = ?, updated_by = ? WHERE id = ?`,
		kind, m.Title, m.Start.Unix(), m.End.Unix(), m.Every, m.On, nullable(m.Until), m.TeamID, t.d.kept(deployLabelsText(m.Labels)),
		m.Declared, time.Now().Unix(), by, id)
	return err
}

// sameMarks reports whether two windows mark the same firings.
func sameMarks(a, b Marked) bool {
	return a.Kind == b.Kind && a.Start.Equal(b.Start) && a.End.Equal(b.End) && a.Every == b.Every && a.On == b.On && a.Until.Equal(b.Until) &&
		a.TeamID == b.TeamID && deployLabelsText(a.Labels) == deployLabelsText(b.Labels)
}

// ErrNotTheUIs is a marked window the UI didn't make, changed in it: the
// marked file's, or an expression's, are theirs to change.
var ErrNotTheUIs = errors.New("this marked window is the marked file's or an expression's: change it there")

// markedLeft refuses a new window made in the UI past what the UI keeps,
// for a team's editors MaxTeamMarked of their team's.
func markedLeft(t *txn, team int64) error {
	var own, all int
	if err := t.QueryRowContext(t.ctx, "SELECT count(*) FILTER (WHERE team_id = ?), count(*) FROM marked WHERE origin = 'ui'", team).Scan(&own, &all); err != nil {
		return err
	}
	_, limited := scopeOf(t.ctx)
	switch {
	case all >= MaxMarkedUI:
		return invalid("the ledger keeps %d marked windows made in the UI at most: delete those no longer needed", MaxMarkedUI)
	case limited && own >= MaxTeamMarked:
		return invalid("a team's editors keep %d of its marked windows at most; an editor or an admin can keep more", MaxTeamMarked)
	}
	return nil
}

// PutMarked makes a marked window in the UI or the API, or changes the one
// with m's ID, which the UI made, marks again the firings it meets, before
// and after, and returns its ID.
func (l *Ledger) PutMarked(ctx context.Context, m Marked, by string, o verdict.Options) (int64, error) {
	m.Origin, m.Declared = OriginUI, false
	if err := l.checkMarked(&m, time.Now()); err != nil {
		return 0, err
	}
	id := m.ID
	err := l.write(ctx, func(t *txn) error {
		if err := inScope(t, m.TeamID); err != nil {
			return err
		}
		if m.TeamID != 0 {
			var x int
			if err := t.QueryRowContext(t.ctx, "SELECT 1 FROM teams WHERE id = ?", m.TeamID).Scan(&x); err != nil {
				return invalid("no such team")
			}
		}
		now := time.Now()
		spans := [][2]int64{m.span(now)}
		if id == 0 {
			if err := markedLeft(t, m.TeamID); err != nil {
				return err
			}
		} else {
			was, err := l.markedRows(t.ctx, t, "WHERE m.id = ?", id)
			if err != nil {
				return err
			}
			if len(was) == 0 {
				return ErrNotFound
			}
			switch w := was[0]; {
			case w.Declared:
				return ErrDeclared
			case w.Origin != OriginUI:
				return ErrNotTheUIs
			}
			if err := inScope(t, was[0].TeamID); err != nil {
				return err
			}
			if _, limited := scopeOf(t.ctx); limited && was[0].TeamID != m.TeamID {
				var own int
				if err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM marked WHERE origin = 'ui' AND team_id = ?", m.TeamID).Scan(&own); err != nil {
					return err
				}
				if own >= MaxTeamMarked {
					return invalid("a team's editors keep %d of its marked windows at most; an editor or an admin can keep more", MaxTeamMarked)
				}
			}
			spans = append(spans, was[0].span(now))
			if sameMarks(was[0], m) {
				spans = nil // a new title: no mark changes
			}
		}
		if err := l.fitsAll(t, func(h Marked) bool { return h.ID != id }, []Marked{m}); err != nil {
			return err
		}
		if err := l.shareFits(t, id, m); err != nil {
			return err
		}
		// A team's editors use the kinds there are: the ledger's fifteen
		// are everyone's.
		if _, limited := scopeOf(t.ctx); limited {
			held, err := l.kinds(t.ctx, t)
			if err != nil {
				return err
			}
			if !slices.Contains(slices.Collect(maps.Values(held)), m.Kind) {
				return invalid("a team's editors use the kinds of marked window there are; an editor or an admin can make %s", m.Kind)
			}
		}
		if id == 0 {
			var err error
			if id, err = l.insertMarked(t, m, by); err != nil {
				return err
			}
		} else if err := l.updateMarked(t, id, m, by); err != nil {
			return err
		}
		m.ID = id
		kept, err := l.markedRows(t.ctx, t, "WHERE m.id = ?", id)
		if err != nil || len(kept) == 0 {
			return errors.Join(err, ErrNotFound)
		}
		if err := l.tell(t, "marked.kept", by, map[string]any{"marked": kept[0]}); err != nil {
			return err
		}
		if err := change(t, by, "marked window "+m.Title+" kept", nil, kept[0]); err != nil {
			return err
		}
		return pend(t, spans)
	})
	if err != nil {
		return 0, err
	}
	l.judgeSoon(ctx, o)
	return id, nil
}

// DeleteMarked deletes a marked window the UI made, and marks again the
// firings it marked.
func (l *Ledger) DeleteMarked(ctx context.Context, id int64, by string, o verdict.Options) error {
	err := l.write(ctx, func(t *txn) error {
		was, err := l.markedRows(t.ctx, t, "WHERE m.id = ?", id)
		if err != nil {
			return err
		}
		if len(was) == 0 {
			return ErrNotFound
		}
		switch w := was[0]; {
		case w.Declared:
			return ErrDeclared
		case w.Origin != OriginUI:
			return ErrNotTheUIs
		}
		if err := inScope(t, was[0].TeamID); err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, "DELETE FROM marked WHERE id = ?", id); err != nil {
			return err
		}
		if err := change(t, by, "marked window "+was[0].Title+" deleted", nil, nil); err != nil {
			return err
		}
		return pend(t, [][2]int64{was[0].span(time.Now())})
	})
	if err != nil {
		return err
	}
	l.judgeSoon(ctx, o)
	return nil
}

// MarkedRead is a read of the marked file: when, and a digest of what it
// held, which an unchanged file is known by.
type MarkedRead struct {
	At  time.Time
	Sum string
}

// markedKey is what tells two windows of one origin apart: one given twice
// is kept once.
func markedKey(m Marked) string {
	return strings.Join([]string{m.Kind, m.Title, strconv.FormatInt(m.Start.Unix(), 10), strconv.FormatInt(m.End.Unix(), 10), m.Every, m.On,
		strconv.FormatInt(m.Until.Unix(), 10), strconv.FormatInt(m.TeamID, 10), deployLabelsText(m.Labels)}, "\x00")
}

// MarkedUnchanged reports whether a read of the marked file is the last
// one kept, by its digest: what it holds needn't be read.
func (l *Ledger) MarkedUnchanged(ctx context.Context, read MarkedRead) (bool, error) {
	if read.Sum == "" {
		return false, nil
	}
	was, err := l.meta(ctx, l.r, "marked-read")
	return was == read.Sum, err
}

// SyncMarked makes the ledger's windows from the marked file those given,
// which the file owns, and marks again the firings they meet, before and
// after: the spans are judged a week at a time, the first two seconds'
// worth before it returns. A read whose Sum is the last read's changes
// nothing. A window that can't be kept, or a file whose windows are past
// the bounds, refuses the file whole: the ledger keeps what it had.
func (l *Ledger) SyncMarked(ctx context.Context, ms []Marked, o verdict.Options, read MarkedRead) (changed bool, err error) {
	if len(ms) > MaxMarkedFile {
		return false, invalid("the marked file holds %d windows at most", MaxMarkedFile)
	}
	if same, err := l.MarkedUnchanged(ctx, read); err != nil || same {
		return false, err
	}
	if read.At.IsZero() {
		read.At = time.Now()
	}
	want := map[string]Marked{}
	var errs []error
	for i, m := range ms {
		m.Origin, m.Declared, m.ID = OriginFile, false, 0
		if err := l.checkMarked(&m, read.At); err != nil {
			errs = append(errs, invalidAt(i+1, err))
			continue
		}
		want[markedKey(m)] = m
	}
	if err := errors.Join(errs...); err != nil {
		return false, err
	}
	err = l.write(ctx, func(t *txn) error {
		for _, m := range want {
			if m.TeamID == 0 {
				continue
			}
			var x int
			if err := t.QueryRowContext(t.ctx, "SELECT 1 FROM teams WHERE id = ?", m.TeamID).Scan(&x); err != nil {
				return fmt.Errorf("%s: %w", m.Title, invalid("no such team"))
			}
		}
		have, err := l.markedRows(t.ctx, t, "WHERE m.origin = 'file' ORDER BY m.id")
		if err != nil {
			return err
		}
		if err := l.fitsAll(t, func(h Marked) bool { return h.Origin != OriginFile }, slices.Collect(maps.Values(want))); err != nil {
			return err
		}
		now := time.Now()
		var spans [][2]int64
		kept := map[string]bool{}
		for _, h := range have {
			if k := markedKey(h); want[k].Kind != "" && !kept[k] {
				kept[k] = true
				continue
			}
			if _, err := t.ExecContext(t.ctx, "DELETE FROM marked WHERE id = ?", h.ID); err != nil {
				return err
			}
			spans = append(spans, h.span(now))
		}
		for _, k := range slices.Sorted(maps.Keys(want)) {
			if kept[k] {
				continue
			}
			m := want[k]
			if _, err := l.insertMarked(t, m, "marked file"); err != nil {
				return err
			}
			spans = append(spans, m.span(now))
		}
		if read.Sum != "" {
			if err := setMeta(t.ctx, t, "marked-read", read.Sum); err != nil {
				return err
			}
		}
		changed = len(spans) > 0
		return pend(t, spans)
	})
	if err != nil || !changed {
		return changed, err
	}
	l.judgeSoon(ctx, o)
	return true, nil
}

// invalidAt is what is wrong with the nth window of a file: a file's
// errors are said where it is read, the run history and the log.
func invalidAt(n int, err error) error { return fmt.Errorf("window %d: %w", n, err) }

// HasMarked reports whether the ledger keeps any marked window.
func (l *Ledger) HasMarked(ctx context.Context) (bool, error) {
	var n int
	err := l.r.QueryRowContext(ctx, "SELECT count(*) FROM (SELECT 1 FROM marked LIMIT 1) AS one").Scan(&n)
	return n > 0, err
}

// MarkRead is what an expression (--mark KIND=PROMQL) found over a read:
// its windows from Since to To, read with Key (a hash of the expression
// and its --mark-match), a step apart. Again says it was read again over
// the weeks before, its key new: the windows it found before are all
// replaced.
type MarkRead struct {
	Kind      string
	Key       string
	Since, To time.Time
	Step      time.Duration
	Again     bool
	Windows   []Marked
}

// MarkKey is the key an expression's windows are kept with: a hash of the
// expression and the labels --mark-match keeps.
func MarkKey(expr string, match []string) string {
	h := sha256.New()
	h.Write([]byte(expr))
	for _, m := range match {
		h.Write([]byte{0})
		h.Write([]byte(m))
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// MarkKeyOf is the key an expression's windows of a kind were kept with;
// "" for none.
func (l *Ledger) MarkKeyOf(ctx context.Context, kind string) (string, error) {
	return l.meta(ctx, l.r, "marked-expr."+kind)
}

// keepMarksSpans, when set, is told the spans a KeepMarks pends to judge
// again: for tests of how much a read costs.
var keepMarksSpans func([][2]int64)

// MarksSkipped are the kinds whose reads KeepMarks didn't keep: one read
// with another key than its windows were kept with, not read again over
// the weeks before (MarkRead.Again), or one whose kind can't be kept. The
// others are kept all the same.
type MarksSkipped struct {
	Kinds   []string
	Dropped int
}

// KeepMarks keeps what the expressions found, each in place of the windows
// it found before from its Since on, or all of them when read again
// (Again), with its key. A read with another key than the windows were
// kept with is skipped until a read again keeps the key: its weeks before
// are read first. A window found before that was on at Since goes on in
// one found now on from it (within two steps, each found one going on
// from one at most), and is let be otherwise. A window past the bounds is
// dropped for this read, the latest first; one whose kind the ledger
// can't keep skips its read. The firings the windows meet, before and
// after, are marked again: of one going on, those from its end before to
// its end now.
func (l *Ledger) KeepMarks(ctx context.Context, reads []MarkRead, o verdict.Options) (dropped int, err error) {
	x, err := l.KeepMarksSaid(ctx, reads, o)
	return x.Dropped, err
}

// KeepMarksSaid is KeepMarks, saying what it skipped.
func (l *Ledger) KeepMarksSaid(ctx context.Context, reads []MarkRead, o verdict.Options) (MarksSkipped, error) {
	var any bool
	var said MarksSkipped
	err := l.write(ctx, func(t *txn) error {
		said, any = MarksSkipped{}, false
		now := time.Now()
		var spans [][2]int64
		for _, rd := range reads {
			if CheckKind(rd.Kind) != nil {
				said.Kinds = append(said.Kinds, rd.Kind)
				continue
			}
			was, err := l.meta(t.ctx, t, "marked-expr."+rd.Kind)
			if err != nil {
				return err
			}
			have, err := l.markedRows(t.ctx, t, "WHERE m.origin = 'expr' AND k.name = ? ORDER BY m.start_at, m.id", rd.Kind)
			if err != nil {
				return err
			}
			// A first read of a kind none was found of starts its windows;
			// a read with another key waits for the weeks before to be
			// read with it.
			again := rd.Again || was == "" && len(have) == 0
			if !again && was != rd.Key {
				said.Kinds = append(said.Kinds, rd.Kind)
				continue
			}
			if _, err := l.kindID(t, rd.Kind); err != nil {
				if invalidErr(err) {
					said.Kinds = append(said.Kinds, rd.Kind)
					continue
				}
				return err
			}
			gap := 2 * rd.Step
			ws := slices.Clone(rd.Windows)
			slices.SortFunc(ws, func(a, b Marked) int { return a.Start.Compare(b.Start) })
			replaced := map[int64]bool{}
			used := make([]bool, len(ws))
			// goesOn are, by the index of the window found now, the end of
			// the one found before it goes on from; from, by that one's id,
			// the index.
			goesOn := map[int]time.Time{}
			from := map[int64]int{}
			for _, h := range have {
				switch {
				case again || !h.Start.Before(rd.Since):
					replaced[h.ID] = true
				case h.End.Before(rd.Since):
				default:
					// On at Since: one found now on from it goes on from
					// it, within the bounds a window has; each found now
					// goes on from one at most.
					for i, w := range ws {
						if !used[i] && !w.Start.After(rd.Since.Add(gap)) && w.End.Sub(h.Start) <= maxWindow && deployLabelsText(w.Labels) == deployLabelsText(h.Labels) {
							ws[i].Start, replaced[h.ID], used[i] = h.Start, true, true
							goesOn[i], from[h.ID] = h.End, i
							break
						}
					}
				}
			}
			all, err := l.markedRows(t.ctx, t, "ORDER BY m.id")
			if err != nil {
				return err
			}
			ld := l.newLoad()
			for _, m := range all {
				if !replaced[m.ID] {
					ld.add(m)
				}
			}
			var keep []Marked
			var keepAt []int
			for i, w := range ws {
				w.Kind, w.Origin, w.Every, w.On, w.Until, w.TeamID, w.Declared = rd.Kind, OriginExpr, "", "", time.Time{}, 0, false
				if strings.TrimSpace(w.Title) == "" {
					w.Title = rd.Kind
				}
				if len(keep) == MaxMarkedExpr || l.checkMarked(&w, now) != nil || !ld.fits(rd.Kind, w.Start, w.End) {
					said.Dropped++
					continue
				}
				ld.add(w)
				keep = append(keep, w)
				keepAt = append(keepAt, i)
			}
			kept := map[int]bool{}
			for _, i := range keepAt {
				kept[i] = true
			}
			for _, h := range have {
				i, goes := from[h.ID]
				if !replaced[h.ID] || goes && !kept[i] {
					continue // what would go on from it was dropped: kept as it was
				}
				if _, err := t.ExecContext(t.ctx, "DELETE FROM marked WHERE id = ?", h.ID); err != nil {
					return err
				}
				// One going on in a window kept is judged again from where
				// it ended to where it ends now, below.
				if !goes {
					spans = append(spans, h.span(now))
				}
			}
			for k, w := range keep {
				if _, err := l.insertMarked(t, w, "expression"); err != nil {
					return err
				}
				if end, ok := goesOn[keepAt[k]]; ok {
					a, b := end, w.End
					if b.Before(a) {
						a, b = b, a
					}
					spans = append(spans, [2]int64{a.Unix(), b.Unix()})
					continue
				}
				spans = append(spans, w.span(now))
			}
			if was != rd.Key {
				if err := setMeta(t.ctx, t, "marked-expr."+rd.Kind, rd.Key); err != nil {
					return err
				}
			}
			any = any || len(spans) > 0 || was != rd.Key
		}
		if keepMarksSpans != nil {
			keepMarksSpans(spans)
		}
		if !any {
			return errUnchanged
		}
		return pend(t, spans)
	})
	if errors.Is(err, errUnchanged) {
		return said, nil
	}
	if err != nil {
		return MarksSkipped{}, err
	}
	l.judgeSoon(ctx, o)
	return said, nil
}

// ForgetMarks forgets the keys of the expressions of kinds not in keep: no
// longer given, their windows are kept as they were found, and a kind
// nothing names any more is let go.
func (l *Ledger) ForgetMarks(ctx context.Context, keep []string) error {
	return l.writeAside(ctx, func(t *txn) error {
		rows, err := t.QueryContext(t.ctx, "SELECT key FROM meta WHERE key > 'marked-expr.' AND key < 'marked-expr/'")
		if err != nil {
			return err
		}
		var gone []string
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				rows.Close()
				return err
			}
			if !slices.Contains(keep, strings.TrimPrefix(k, "marked-expr.")) {
				gone = append(gone, k)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, k := range gone {
			if _, err := t.ExecContext(t.ctx, "DELETE FROM meta WHERE key = ?", k); err != nil {
				return err
			}
		}
		return nil
	})
}

// MarkedTally is a rule's firings marked with a kind over a day or a
// week: those that caught something (White), and those that re-fired.
type MarkedTally struct {
	At      int64
	Rule    int64
	Kind    string
	Firings int
	White   int
	Refired int
	// Signals are the signals the firings' alerts are tied to, of a tally
	// by them (MarkedWeeksBy): 0 of one summed over them.
	Signals int64
}

// MarkedDays are the firings marked with each kind by day and rule, from
// day to day.
func (l *Ledger) MarkedDays(ctx context.Context, from, to int64) ([]MarkedTally, error) {
	return l.markedTallies(ctx, `SELECT d.day, d.rule_id, k.name, sum(d.firings), sum(d.white), sum(d.refired), 0 FROM marked_days d JOIN marked_kinds k ON k.id = d.kind_id
		WHERE d.day >= ? AND d.day <= ? GROUP BY d.day, d.rule_id, k.name ORDER BY d.day, d.rule_id, k.name`, from, to)
}

// MarkedDaysBy are MarkedDays, apart by the signals the firings' alerts
// are tied to: what a firing is judged by.
func (l *Ledger) MarkedDaysBy(ctx context.Context, from, to int64) ([]MarkedTally, error) {
	return l.markedTallies(ctx, `SELECT d.day, d.rule_id, k.name, d.firings, d.white, d.refired, d.signals FROM marked_days d JOIN marked_kinds k ON k.id = d.kind_id
		WHERE d.day >= ? AND d.day <= ? ORDER BY d.day, d.rule_id, k.name, d.signals`, from, to)
}

// MarkedWeeksBy are MarkedWeeks, apart by the signals the firings' alerts
// are tied to.
func (l *Ledger) MarkedWeeksBy(ctx context.Context, from, to int64) ([]MarkedTally, error) {
	return l.markedTallies(ctx, `SELECT d.week, d.rule_id, k.name, d.firings, d.white, d.refired, d.signals FROM marked_weeks d JOIN marked_kinds k ON k.id = d.kind_id
		WHERE d.week >= ? AND d.week <= ? ORDER BY d.week, d.rule_id, k.name, d.signals`, from, to)
}

// MarkedWeeks are the firings marked with each kind by week and rule, from
// the week starting on one Monday to that starting on another.
func (l *Ledger) MarkedWeeks(ctx context.Context, from, to int64) ([]MarkedTally, error) {
	return l.markedTallies(ctx, `SELECT d.week, d.rule_id, k.name, sum(d.firings), sum(d.white), sum(d.refired), 0 FROM marked_weeks d JOIN marked_kinds k ON k.id = d.kind_id
		WHERE d.week >= ? AND d.week <= ? GROUP BY d.week, d.rule_id, k.name ORDER BY d.week, d.rule_id, k.name`, from, to)
}

// RuleMarked are a rule's firings marked with each kind, by day, from day
// to day.
func (l *Ledger) RuleMarked(ctx context.Context, rule string, from, to int64) ([]MarkedTally, error) {
	return l.markedTallies(ctx, `SELECT d.day, d.rule_id, k.name, sum(d.firings), sum(d.white), sum(d.refired), 0 FROM marked_days d JOIN marked_kinds k ON k.id = d.kind_id
		WHERE d.rule_id = (SELECT id FROM rules WHERE name = ?) AND d.day >= ? AND d.day <= ? GROUP BY d.day, d.rule_id, k.name ORDER BY d.day, k.name`, rule, from, to)
}

func (l *Ledger) markedTallies(ctx context.Context, q string, args ...any) ([]MarkedTally, error) {
	rows, err := l.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MarkedTally
	for rows.Next() {
		var x MarkedTally
		if err := rows.Scan(&x.At, &x.Rule, &x.Kind, &x.Firings, &x.White, &x.Refired, &x.Signals); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// kindBit is the bit of the kind named by a parameter, NULL for none: a
// firing's marked, and'ed with it, is NULL then, and matches nothing.
const kindBit = "SELECT CAST(1 AS BIGINT) << CAST(k.id AS INTEGER) FROM marked_kinds k WHERE k.name = ?"

// markedOn is the condition that a firing f is marked with the kind k:
// its bit, 1 << k.id, set, which SQLite and Postgres both read.
const markedOn = "(f.marked & (CAST(1 AS BIGINT) << CAST(k.id AS INTEGER))) != 0"

// tallyMarked counts each rule's firings marked with each kind again, for
// the days [d1, d2] and the weeks they fall in, in the tally that counts
// days and weeks. With no kind kept, none is marked: nothing is read.
func (l *Ledger) tallyMarked(t *txn, d1, d2, w1, w2 int64) error {
	if _, err := t.ExecContext(t.ctx, "DELETE FROM marked_days WHERE day >= ? AND day <= ?", d1, d2); err != nil {
		return err
	}
	if _, err := t.ExecContext(t.ctx, "DELETE FROM marked_weeks WHERE week >= ? AND week <= ?", w1, w2); err != nil {
		return err
	}
	if ok, err := t.marks(); err != nil || !ok {
		return err
	}
	if _, err := t.ExecContext(t.ctx, `INSERT INTO marked_days (day, rule_id, kind_id, signals, firings, white, refired)
		SELECT f.day, f.rule_id, k.id, s.signals, count(*), sum(f.white), sum(f.refired) FROM firings f JOIN marked_kinds k ON `+markedOn+`
		JOIN series s ON s.id = f.series_id
		WHERE f.start_at >= ? AND f.start_at < ? AND f.marked != 0 GROUP BY f.day, f.rule_id, k.id, s.signals`, l.midnight(d1), l.midnight(d2+1)); err != nil {
		return err
	}
	_, err := t.ExecContext(t.ctx, `INSERT INTO marked_weeks (week, rule_id, kind_id, signals, firings, white, refired)
		SELECT day - (day + 3) % 7, rule_id, kind_id, signals, sum(firings), sum(white), sum(refired) FROM marked_days
		WHERE day >= ? AND day <= ? GROUP BY 1, 2, 3, 4`, w1, w2+6)
	return err
}

// MarkedText is a window's labels as a person reads them.
func MarkedText(m Marked) string { return DeployText(Deploy{Labels: m.Labels}) }
