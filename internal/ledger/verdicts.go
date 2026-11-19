package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/verdict"
)

// A person's verdict on one firing (docs/design/judging.md, item 3): that
// it is unrelated to the incidents it caught, which turns that firing
// alone black, and whether it needed someone to act, which changes no
// stone. Kept by the firing's alert and start (verdicts), not its id: a
// run gives its firings new ids, and SQLite gives a gone row's id again.
// Judging reads them, and never writes them but to say they moved.

// The bounds on what people say of firings.
const (
	// VerdictsAnHour is how many verdict writes one person makes in an
	// hour, of every kind: a form's post, a bulk post, an API call.
	VerdictsAnHour = 30
	// VerdictAge is how far back an editor, and a team's editor, says
	// something of a firing (and presses "This was real"): older ones are
	// an admin's.
	VerdictAge = 14 * 24 * time.Hour
	// MaxBulkVerdicts is how many firings a bulk post or call answers.
	MaxBulkVerdicts = 100
	// MaxVerdictNote is a note's most, in characters, on one line.
	MaxVerdictNote = 500
)

// Errors from saying something of a firing.
var (
	// ErrFiringChanged is a form about a firing whose id is another's, or
	// no one's, by now: a run gave its firings again since the page was
	// read.
	ErrFiringChanged = errors.New("this firing changed since the page was read: look again")
	// ErrSaidUnrelated is "This was real" on a firing a person said is
	// unrelated: the one would undo the other.
	ErrSaidUnrelated = errors.New("a person said this firing was unrelated: undo that first")
	// ErrCaughtNothing is "unrelated" on a firing that caught nothing.
	ErrCaughtNothing = errors.New("this firing caught nothing: there is nothing for it to be unrelated to")
	// ErrRealFiring is "unrelated" on a firing "This was real" wrote an
	// incident from.
	ErrRealFiring = errors.New("This was real wrote an incident from this firing: undo that first")
	// ErrTooOld is a verdict, or "This was real", on a firing that started
	// more than VerdictAge ago, by anyone but an admin.
	ErrTooOld = errors.New("only an admin changes what is said of a firing that started more than 14 days ago")
	// ErrUnrelatedRole is "unrelated" set, changed or cleared by a team's
	// editor, whose write leaves it as it is.
	ErrUnrelatedRole = errors.New("only an editor of every team, or an admin, says a firing is unrelated")
	// ErrTooManyVerdicts is a verdict write past VerdictsAnHour.
	ErrTooManyVerdicts = errors.New("you have said as much of firings in the last hour as anyone may: try again later")
	// ErrNoVerdict is a verdict undone that isn't there.
	ErrNoVerdict error = notFound("nothing is said of this firing")
	// ErrNoFiring is a firing that isn't there.
	ErrNoFiring error = notFound("no such firing")
)

// notFound is a thing not there, as ErrNotFound is.
type notFound string

func (e notFound) Error() string        { return string(e) }
func (e notFound) Is(target error) bool { return target == ErrNotFound }

// TooManyVerdicts is ErrTooManyVerdicts, with how long until the oldest
// write of the hour leaves it.
type TooManyVerdicts struct{ Retry time.Duration }

func (e TooManyVerdicts) Error() string { return ErrTooManyVerdicts.Error() }
func (e TooManyVerdicts) Unwrap() error { return ErrTooManyVerdicts }

// UnrelatedTo is an incident a firing was said to be unrelated to, as it
// was when said.
type UnrelatedTo struct {
	Key   string    `json:"key"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end,omitzero"`
}

// Verdict is a person's verdict on a firing, as the ledger keeps it.
type Verdict struct {
	ID int64
	// Rule, Labels, Series and Start are the alert and start it is kept
	// by; End is its firing's end when it was said, zero for one on.
	Rule   string
	Labels map[string]string
	Series int64
	Start  time.Time
	End    time.Time
	// Unrelated are the incidents it was said to be unrelated to, each as
	// it was then; with who said so, when, and why.
	Unrelated     []UnrelatedTo
	UnrelatedBy   string
	UnrelatedAt   time.Time
	UnrelatedNote string
	// Action is verdict.ActionYes, verdict.ActionNo or "".
	Action     string
	ActionBy   string
	ActionAt   time.Time
	ActionNote string
	// Moved is why it is to be looked at again (MovedText), "" for none.
	Moved string
	// Firing is the id of the firing it is on now, 0 for none.
	Firing int64
}

// Keys are the keys of the incidents it was said to be unrelated to.
func (v Verdict) Keys() []string {
	out := make([]string, len(v.Unrelated))
	for i, u := range v.Unrelated {
		out[i] = u.Key
	}
	return out
}

// empty reports whether it says nothing: no part set.
func (v Verdict) empty() bool { return len(v.Unrelated) == 0 && v.Action == "" }

// FiringKey picks one firing: by its id, with the series and start of its
// alert the page read it with (a form), which must still be its own; or by
// its rule, labels and start (the API), as a report gives a firing.
type FiringKey struct {
	ID     int64
	Series int64
	Rule   string
	Labels map[string]string
	Start  time.Time
}

// VerdictChange is what a write says of a firing: each part given, the
// rest left as it is.
type VerdictChange struct {
	Unrelated     *bool
	UnrelatedNote *string
	Action        *string
	ActionNote    *string
}

// VerdictBy is who writes a verdict, and when: an admin's reaches any
// firing kept, an editor's the last VerdictAge's. A team's editor is told
// by the write's context (WithTeams): their write leaves "unrelated" as it
// is, and reaches their team's rules alone.
type VerdictBy struct {
	By    string
	Admin bool
	Now   time.Time
}

func (w VerdictBy) now() time.Time {
	if w.Now.IsZero() {
		return time.Now()
	}
	return w.Now
}

// Moved, as a verdict says why it is to be looked at again.
const (
	movedStart   = "start"   // its firing starts at another time now
	movedAlert   = "alert"   // its alert's firing is gone: it found another alert's
	movedGone    = "gone"    // its firing is gone, and none was found
	movedHeld    = "held"    // another verdict holds the firing it found: "held SERIES START"
	movedSpan    = "span"    // an incident's span changed: "span KEY"
	movedCatches = "catches" // it catches an incident it wasn't said to be unrelated to: "catches KEY"
)

// MovedText says why a verdict moved, for a person, in English: what a
// page translates.
func MovedText(moved string) (format string, args []any) {
	kind, rest, _ := strings.Cut(moved, " ")
	switch kind {
	case movedStart:
		return "its firing starts at another time now", nil
	case movedAlert:
		return "its alert's firing is gone: it is on another alert's of the same rule and labels matched on", nil
	case movedGone:
		return "its firing is gone: it is on no firing", nil
	case movedHeld:
		return "another verdict holds the firing it found", nil
	case movedSpan:
		return "%s has another start or end than when this was said", []any{rest}
	case movedCatches:
		return "it catches %s now, which it wasn't said to be unrelated to", []any{rest}
	}
	return "", nil
}

// checkNote refuses a note past MaxVerdictNote, or of more than one line.
func checkNote(s *string) error {
	if s == nil {
		return nil
	}
	if strings.ContainsAny(*s, "\r\n  ") {
		return invalid("a verdict's note is one line")
	}
	return checkText("a verdict's note is %d characters at most", *s, MaxVerdictNote)
}

func checkChange(c VerdictChange) error {
	if c.Action != nil && !slices.Contains([]string{"", verdict.ActionYes, verdict.ActionNo}, *c.Action) {
		return invalid("needed action is yes, no, or none")
	}
	if c.Unrelated == nil && c.UnrelatedNote == nil && c.Action == nil && c.ActionNote == nil {
		return invalid("say something of the firing: unrelated, needed action, or a note")
	}
	return errors.Join(checkNote(c.UnrelatedNote), checkNote(c.ActionNote))
}

// SetVerdict says something of one firing, part by part: each part given
// replaces what is kept of it, with who and when; the rest stays. It is
// one write: the firing is read again in it, refused when it changed under
// a form (ErrFiringChanged), is another team's for a team's editor
// (ErrNotYourTeam), or started more than VerdictAge ago for one who isn't
// an admin (ErrTooOld). A team's editor's write leaves "unrelated" as it
// is stored: given otherwise, it is refused (ErrUnrelatedRole).
// "Unrelated" keeps the incidents the firing catches now as Lapilli judges
// it, 16 at most; refused on one that catches none (ErrCaughtNothing), or
// that "This was real" wrote one from (ErrRealFiring). Past VerdictsAnHour
// writes of the writer's in the hour, nothing is written
// (TooManyVerdicts). A write that says what is kept already writes
// nothing, and isn't counted. The firing's span is judged again.
func (l *Ledger) SetVerdict(ctx context.Context, k FiringKey, c VerdictChange, w VerdictBy, o verdict.Options) (Verdict, error) {
	if err := checkChange(c); err != nil {
		return Verdict{}, err
	}
	var out Verdict
	err := l.write(ctx, func(t *txn) error {
		if err := verdictsLeft(t, w); err != nil {
			return err
		}
		f, err := l.firingFor(t, k)
		if err != nil {
			return err
		}
		if err := l.mayVerdict(t, f, w); err != nil {
			return err
		}
		was, err := l.verdictAt(t, f.Series, f.Start)
		if err != nil {
			return err
		}
		now, err := l.changeVerdict(t, f, was, c, w, o)
		if err != nil {
			return err
		}
		out = now
		return l.keepVerdict(t, f, was, now, w.By, "verdict on "+firingName(f), o)
	})
	if errors.Is(err, errNothing) {
		err = nil
	}
	return out, err
}

// firingName is a firing as the changes name it: its rule, labels and
// start.
func firingName(f Firing) string {
	b, _ := json.Marshal(orEmptyMap(f.Labels))
	return fmt.Sprintf("%s %s at %s", f.Rule, b, f.Start.UTC().Format(time.RFC3339))
}

// changeVerdict is was with c said of f by w, as a person's write gives
// it, part by part.
func (l *Ledger) changeVerdict(t *txn, f Firing, was Verdict, c VerdictChange, w VerdictBy, o verdict.Options) (Verdict, error) {
	_, limited := scopeOf(t.ctx)
	now := was
	now.Series, now.Start, now.End, now.Rule, now.Labels = f.Series, f.Start, f.End, f.Rule, f.Labels
	at := w.now().UTC().Truncate(time.Second)
	if limited {
		// A team's editor leaves "unrelated" as it is: neither set, nor
		// cleared, nor its note changed.
		if c.Unrelated != nil && *c.Unrelated != (len(was.Unrelated) > 0) || c.UnrelatedNote != nil && *c.UnrelatedNote != was.UnrelatedNote {
			return now, ErrUnrelatedRole
		}
	} else if c.Unrelated != nil {
		if *c.Unrelated {
			caught, err := l.caughtNow(t, f, o)
			if err != nil {
				return now, err
			}
			switch real, err := realFrom(t, f); {
			case err != nil:
				return now, err
			case len(caught) == 0:
				return now, ErrCaughtNothing
			case real:
				return now, ErrRealFiring
			case len(caught) > verdict.MaxUnrelated:
				return now, invalid("this firing caught %d incidents: one that caught more than %d can't be said unrelated", len(caught), verdict.MaxUnrelated)
			}
			now.Unrelated, now.UnrelatedBy, now.UnrelatedAt = caught, w.By, at
		} else {
			now.Unrelated, now.UnrelatedBy, now.UnrelatedAt, now.UnrelatedNote = nil, "", time.Time{}, ""
		}
		// Said again, or undone: not to be looked at again.
		now.Moved = ""
	}
	if c.UnrelatedNote != nil && !limited {
		now.UnrelatedNote = *c.UnrelatedNote
	}
	if len(now.Unrelated) == 0 {
		now.UnrelatedNote = ""
	}
	if c.Action != nil && *c.Action != was.Action {
		now.Action, now.ActionBy, now.ActionAt = *c.Action, w.By, at
	}
	if c.ActionNote != nil {
		now.ActionNote = *c.ActionNote
	}
	if now.Action == "" {
		now.ActionBy, now.ActionAt, now.ActionNote = "", time.Time{}, ""
	}
	if len(now.Unrelated) == 0 && now.Moved != "" && !strings.HasPrefix(now.Moved, movedHeld) {
		now.Moved = "" // nothing it said is about what moved
	}
	return now, nil
}

// sameVerdict reports whether a and b say the same: a write of b over a
// writes nothing.
func sameVerdict(a, b Verdict) bool {
	return slices.EqualFunc(a.Unrelated, b.Unrelated, func(x, y UnrelatedTo) bool { return x.Key == y.Key && x.Start.Equal(y.Start) && x.End.Equal(y.End) }) &&
		a.UnrelatedNote == b.UnrelatedNote && a.Action == b.Action && a.ActionNote == b.ActionNote && a.Moved == b.Moved &&
		a.Series == b.Series && a.Start.Equal(b.Start)
}

// keepVerdict writes now in place of was (none when was.ID is 0), notes the
// change as what, by by, and judges f's span again. A verdict that says
// nothing is taken away. Writing "unrelated", or undoing it, takes away the
// verdicts another held of f (movedHeld): what a person says of f now is
// what it is.
func (l *Ledger) keepVerdict(t *txn, f Firing, was, now Verdict, by, what string, o verdict.Options) error {
	if was.ID != 0 && sameVerdict(was, now) {
		return errNothing
	}
	if was.ID == 0 && now.empty() {
		return errNothing
	}
	if err := writeVerdict(t, was, now); err != nil {
		return err
	}
	if !slices.EqualFunc(was.Unrelated, now.Unrelated, func(x, y UnrelatedTo) bool { return x.Key == y.Key }) || was.UnrelatedBy != now.UnrelatedBy {
		if _, err := t.ExecContext(t.ctx, "DELETE FROM verdicts WHERE moved = ?", heldAt(f.Series, f.Start)); err != nil {
			return err
		}
	}
	if err := change(t, by, what, verdictShown(was), verdictShown(now)); err != nil {
		return err
	}
	return l.judge(t, f.Start.Unix(), f.Start.Unix(), o)
}

// writeVerdict writes now in place of was, or takes it away when it says
// nothing.
func writeVerdict(t *txn, was, now Verdict) error {
	if now.empty() {
		if was.ID == 0 {
			return nil
		}
		_, err := t.ExecContext(t.ctx, "DELETE FROM verdicts WHERE id = ?", was.ID)
		return err
	}
	u, err := json.Marshal(orNoUnrelated(now.Unrelated))
	if err != nil {
		return err
	}
	args := []any{now.Series, now.Start.Unix(), nullable(now.End), string(u), now.UnrelatedBy, unixOr0(now.UnrelatedAt), now.UnrelatedNote,
		now.Action, now.ActionBy, unixOr0(now.ActionAt), now.ActionNote, now.Moved}
	if was.ID != 0 {
		_, err = t.ExecContext(t.ctx, `UPDATE verdicts SET series_id = ?, start_at = ?, end_at = ?, unrelated = ?, unrelated_by = ?, unrelated_at = ?,
			unrelated_note = ?, action = ?, action_by = ?, action_at = ?, action_note = ?, moved = ? WHERE id = ?`, append(args, was.ID)...)
		return err
	}
	_, err = t.ExecContext(t.ctx, `INSERT INTO verdicts (series_id, start_at, end_at, unrelated, unrelated_by, unrelated_at, unrelated_note,
		action, action_by, action_at, action_note, moved) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, args...)
	return err
}

func orNoUnrelated(us []UnrelatedTo) []UnrelatedTo {
	if us == nil {
		return []UnrelatedTo{}
	}
	return us
}

// verdictShown is a verdict as the changes keep it: nil for none.
func verdictShown(v Verdict) any {
	if v.empty() {
		return nil
	}
	out := map[string]any{}
	if len(v.Unrelated) > 0 {
		out["unrelated"], out["unrelated_by"] = v.Keys(), v.UnrelatedBy
		if v.UnrelatedNote != "" {
			out["unrelated_note"] = v.UnrelatedNote
		}
	}
	if v.Action != "" {
		out["action"], out["action_by"] = v.Action, v.ActionBy
		if v.ActionNote != "" {
			out["action_note"] = v.ActionNote
		}
	}
	return out
}

// verdictsLeft refuses a verdict write past VerdictsAnHour of the writer's
// in the hour before now: one change noted a write, as each is (a bulk
// post or call is one), by the name it is noted under, on every replica
// alike.
func verdictsLeft(t *txn, w VerdictBy) error {
	// By the clock the changes are noted by (change).
	now := time.Now()
	rows, err := t.QueryContext(t.ctx, "SELECT by, at FROM changes WHERE at > ? AND what LIKE 'verdict%'", now.Add(-time.Hour).Unix())
	if err != nil {
		return err
	}
	defer rows.Close()
	who := FoldName(w.By)
	var n int
	var oldest int64
	for rows.Next() {
		var by string
		var at int64
		if err := rows.Scan(&by, &at); err != nil {
			return err
		}
		if FoldName(by) == who {
			if n == 0 || at < oldest {
				oldest = at
			}
			n++
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if n >= VerdictsAnHour {
		return TooManyVerdicts{Retry: max(time.Second, time.Duration(oldest+3600-now.Unix())*time.Second)}
	}
	return nil
}

// firingFor reads, in t, the firing k picks.
func (l *Ledger) firingFor(t *txn, k FiringKey) (Firing, error) {
	if k.ID != 0 {
		f, err := l.firingIn(t, k.ID)
		if errors.Is(err, ErrNotFound) {
			return f, ErrFiringChanged
		}
		if err != nil {
			return f, err
		}
		// The form's id is another alert's firing, or another start's,
		// by now: refused, never written about.
		if f.Series != k.Series || !f.Start.Equal(k.Start.Truncate(time.Second)) {
			return f, ErrFiringChanged
		}
		return f, nil
	}
	series, ok, err := l.seriesOf(t, k.Rule, k.Labels)
	if err != nil || !ok {
		if err == nil {
			err = ErrNoFiring
		}
		return Firing{}, err
	}
	var id int64
	err = t.QueryRowContext(t.ctx, "SELECT id FROM firings WHERE series_id = ? AND start_at = ? ORDER BY id LIMIT 1", series, k.Start.Unix()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Firing{}, ErrNoFiring
	}
	if err != nil {
		return Firing{}, err
	}
	return l.firingIn(t, id)
}

// seriesOf is the id of a rule's alert with those labels, as kept, when the
// ledger has it: never made.
func (l *Ledger) seriesOf(t *txn, rule string, labels map[string]string) (int64, bool, error) {
	var rid int64
	err := t.QueryRowContext(t.ctx, "SELECT id FROM rules WHERE name = ?", rule).Scan(&rid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	k, err := keyOf(t.d, rid, labels)
	if err != nil {
		return 0, false, err
	}
	var id int64
	err = t.QueryRowContext(t.ctx, t.d.pick("SELECT id FROM series WHERE rule_id = ? AND labels = ?",
		"SELECT id FROM series WHERE rule_id = ?1 AND md5(labels) = md5(?2) AND labels = ?2"), rid, k.labels).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

// mayVerdict refuses w a verdict on f: another team's rule for a team's
// editor; one that started more than VerdictAge ago for one who isn't an
// admin.
func (l *Ledger) mayVerdict(t *txn, f Firing, w VerdictBy) error {
	if err := l.ruleScope(t, f.Rule, f.Labels); err != nil {
		return err
	}
	if !w.Admin && f.Start.Before(w.now().Add(-VerdictAge)) {
		return ErrTooOld
	}
	return nil
}

// ruleScope refuses, in a write limited to some teams, a rule's alert no
// team of theirs owns.
func (l *Ledger) ruleScope(t *txn, rule string, labels map[string]string) error {
	if _, limited := scopeOf(t.ctx); !limited {
		return nil
	}
	own, err := l.ownership(t.ctx, t)
	if err != nil {
		return err
	}
	return inScope(t, own.of(rule, labels))
}

// verdictAt is the verdict kept by an alert and a start, none (ID 0) when
// there is none.
func (l *Ledger) verdictAt(t *txn, series int64, start time.Time) (Verdict, error) {
	vs, err := l.readVerdicts(t.ctx, t, "WHERE v.series_id = ? AND v.start_at = ?", series, start.Unix())
	if err != nil || len(vs) == 0 {
		return Verdict{}, err
	}
	return vs[0], nil
}

// verdictCols are what readVerdicts reads, in order.
const verdictCols = `v.id, v.series_id, s.labels, r.name, v.start_at, v.end_at, v.unrelated, v.unrelated_by, v.unrelated_at, v.unrelated_note,
	v.action, v.action_by, v.action_at, v.action_note, v.moved,
	coalesce((SELECT min(f.id) FROM firings f WHERE f.series_id = v.series_id AND f.start_at = v.start_at), 0)`

// readVerdicts reads the verdicts where says, in order of their start.
func (l *Ledger) readVerdicts(ctx context.Context, q queryer, where string, args ...any) ([]Verdict, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+verdictCols+` FROM verdicts v JOIN series s ON s.id = v.series_id JOIN rules r ON r.id = s.rule_id
		`+where+` ORDER BY v.start_at, v.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Verdict
	for rows.Next() {
		var v Verdict
		var labels, unrelated string
		var start, uat, aat int64
		var end sql.NullInt64
		if err := rows.Scan(&v.ID, &v.Series, &labels, &v.Rule, &start, &end, &unrelated, &v.UnrelatedBy, &uat, &v.UnrelatedNote,
			&v.Action, &v.ActionBy, &aat, &v.ActionNote, &v.Moved, &v.Firing); err != nil {
			return nil, err
		}
		if v.Labels, err = l.labels(v.Series, labels); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(unrelated), &v.Unrelated); err != nil {
			return nil, err
		}
		if len(v.Unrelated) == 0 {
			v.Unrelated = nil
		}
		v.Start, v.End = unix(start), fromNull(end)
		if uat > 0 {
			v.UnrelatedAt = unix(uat)
		}
		if aat > 0 {
			v.ActionAt = unix(aat)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// caughtNow are the incidents f catches now as Lapilli judges it, people's
// verdicts aside, each with its span: what "unrelated" keeps.
func (l *Ledger) caughtNow(t *txn, f Firing, o verdict.Options) ([]UnrelatedTo, error) {
	at := f.Start.Unix()
	incs, err := l.counting(t.ctx, t, at, at, o)
	if err != nil {
		return nil, err
	}
	var out []UnrelatedTo
	for _, in := range incs {
		if verdict.Catches(f.Firing, in.Incident, o) {
			out = append(out, UnrelatedTo{Key: in.ID, Start: in.Start.UTC(), End: in.End.UTC()})
		}
	}
	return out, nil
}

// realFrom reports whether "This was real" wrote an incident from f.
func realFrom(t *txn, f Firing) (bool, error) {
	var n int
	err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM incidents WHERE real_of = ?", realKey(f.Series, f.Start)).Scan(&n)
	return n > 0, err
}

// realKey is an alert and a start as incidents.real_of keeps them.
func realKey(series int64, start time.Time) string {
	return strconv.FormatInt(series, 10) + " " + strconv.FormatInt(start.Unix(), 10)
}

// heldAt is what a verdict another holds the firing of its find says
// (movedHeld): the series and start of that firing.
func heldAt(series int64, start time.Time) string {
	return movedHeld + " " + realKey(series, start)
}

// SetVerdicts answers "needed action" of many firings at once, as the bulk
// form and PUT /api/v1/verdicts/bulk give them, MaxBulkVerdicts at most:
// all or nothing, one write, counted once against VerdictsAnHour and
// noted as one change. A firing not found, another team's for a team's
// editor, or older than VerdictAge for one who isn't an admin refuses it
// all, saying which. "Unrelated" isn't taken in bulk: a person looks at
// each.
func (l *Ledger) SetVerdicts(ctx context.Context, ks []FiringKey, action string, note *string, w VerdictBy, o verdict.Options) (int, error) {
	switch {
	case len(ks) == 0:
		return 0, invalid("pick a firing")
	case len(ks) > MaxBulkVerdicts:
		return 0, invalid("at most %d firings at a time: pick fewer", MaxBulkVerdicts)
	}
	c := VerdictChange{Action: &action, ActionNote: note}
	if err := checkChange(c); err != nil {
		return 0, err
	}
	var changed int
	err := l.write(ctx, func(t *txn) error {
		changed = 0
		if err := verdictsLeft(t, w); err != nil {
			return err
		}
		var spans [][2]int64
		var said []map[string]any
		rules := map[string]bool{}
		seen := map[[2]int64]bool{}
		for i, k := range ks {
			f, err := l.firingFor(t, k)
			if err == nil {
				err = l.mayVerdict(t, f, w)
			}
			if err != nil {
				return fmt.Errorf("firing %d of %d: %w", i+1, len(ks), err)
			}
			if seen[[2]int64{f.Series, f.Start.Unix()}] {
				continue
			}
			seen[[2]int64{f.Series, f.Start.Unix()}] = true
			was, err := l.verdictAt(t, f.Series, f.Start)
			if err != nil {
				return err
			}
			now, err := l.changeVerdict(t, f, was, c, w, o)
			if err != nil {
				return err
			}
			if (was.ID != 0 && sameVerdict(was, now)) || (was.ID == 0 && now.empty()) {
				continue
			}
			if err := writeVerdict(t, was, now); err != nil {
				return err
			}
			changed++
			rules[f.Rule] = true
			spans = append(spans, [2]int64{f.Start.Unix(), f.Start.Unix()})
			said = append(said, map[string]any{"rule": f.Rule, "labels": orEmptyMap(f.Labels), "start": f.Start.UTC()})
		}
		if changed == 0 {
			return errNothing
		}
		answer := action
		if answer == "" {
			answer = "none"
		}
		what := fmt.Sprintf("verdicts: needed action %s, on %d firings of %d rules", answer, changed, len(rules))
		if err := change(t, w.By, what, nil, map[string]any{"action": action, "firings": said}); err != nil {
			return err
		}
		return l.judgeSpans(t, spans, o)
	})
	if errors.Is(err, errNothing) {
		err = nil
	}
	return changed, err
}

// DeleteVerdict undoes what is said of the firing k picks, part by part:
// "unrelated", "action", or both when part is "". A team's editor undoes
// "needed action" alone: "unrelated" is refused them (ErrUnrelatedRole),
// and both undoes theirs. It is found by the verdict's alert and start, a
// firing there or not (one moved onto no firing is undone so too), and
// counted against VerdictsAnHour.
func (l *Ledger) DeleteVerdict(ctx context.Context, k FiringKey, part string, w VerdictBy, o verdict.Options) error {
	if !slices.Contains([]string{"", "unrelated", "action"}, part) {
		return invalid("part is unrelated, action, or both when left out")
	}
	err := l.write(ctx, func(t *txn) error {
		if err := verdictsLeft(t, w); err != nil {
			return err
		}
		var series int64
		var start time.Time
		if k.ID != 0 {
			f, err := l.firingFor(t, k)
			if err != nil {
				return err
			}
			series, start = f.Series, f.Start
		} else {
			s, ok, err := l.seriesOf(t, k.Rule, k.Labels)
			if err != nil {
				return err
			}
			if !ok {
				return ErrNoVerdict
			}
			series, start = s, k.Start.Truncate(time.Second)
		}
		was, err := l.verdictAt(t, series, start)
		if err != nil {
			return err
		}
		if was.ID == 0 {
			return ErrNoVerdict
		}
		f := Firing{Firing: firing.Firing{Rule: was.Rule, Labels: was.Labels, Start: was.Start, End: was.End}, Series: was.Series}
		if err := l.mayVerdict(t, f, w); err != nil {
			return err
		}
		_, limited := scopeOf(t.ctx)
		if limited && part == "" {
			part = "action"
		}
		var c VerdictChange
		no, none := false, ""
		if part != "action" {
			c.Unrelated = &no
		}
		if part != "unrelated" {
			c.Action = &none
		}
		now, err := l.changeVerdict(t, f, was, c, w, o)
		if err != nil {
			return err
		}
		if sameVerdict(was, now) {
			return ErrNoVerdict
		}
		return l.keepVerdict(t, f, was, now, w.By, "verdict on "+firingName(f)+" undone", o)
	})
	return err
}

// VerdictsIn are the verdicts kept by firings that started in [from, to),
// each with its firing, if any: of the rules in says, all when nil.
func (l *Ledger) VerdictsIn(ctx context.Context, from, to time.Time, in func(rule string) bool) ([]Verdict, error) {
	vs, err := l.readVerdicts(ctx, l.r, "WHERE v.start_at >= ? AND v.start_at < ?", from.Unix(), to.Unix())
	if err != nil || in == nil {
		return vs, err
	}
	return slices.DeleteFunc(vs, func(v Verdict) bool { return !in(v.Rule) }), nil
}

// VerdictsOf are the verdicts kept by the firings fs, by firing id.
func (l *Ledger) VerdictsOf(ctx context.Context, fs []Firing) (map[int64]Verdict, error) {
	out := map[int64]Verdict{}
	if len(fs) == 0 {
		return out, nil
	}
	lo, hi := fs[0].Start.Unix(), fs[0].Start.Unix()
	for _, f := range fs {
		lo, hi = min(lo, f.Start.Unix()), max(hi, f.Start.Unix())
	}
	vs, err := l.readVerdicts(ctx, l.r, "WHERE v.start_at >= ? AND v.start_at <= ?", lo, hi)
	if err != nil {
		return nil, err
	}
	by := map[[2]int64]Verdict{}
	for _, v := range vs {
		by[[2]int64{v.Series, v.Start.Unix()}] = v
	}
	for _, f := range fs {
		if v, ok := by[[2]int64{f.Series, f.Start.Unix()}]; ok {
			out[f.ID] = v
		}
	}
	return out, nil
}

// MovedVerdicts are the verdicts to be looked at again (Verdict.Moved), the
// latest first, most at most; and how many of them started before since.
func (l *Ledger) MovedVerdicts(ctx context.Context, most int, since time.Time) ([]Verdict, int, error) {
	vs, err := l.readVerdicts(ctx, l.r, "WHERE v.moved != ''")
	if err != nil {
		return nil, 0, err
	}
	old := 0
	for _, v := range vs {
		if v.Start.Before(since) {
			old++
		}
	}
	slices.Reverse(vs)
	if len(vs) > most {
		vs = vs[:most]
	}
	return vs, old, nil
}

// VerdictsSince counts the verdicts said, or undone, in [from, to) of
// firings that started in [wFrom, wTo): what moved a week's goals since
// they were told (docs/design/judging.md, "Who, when, and the trail").
func (l *Ledger) VerdictsSince(ctx context.Context, since time.Time, wFrom, wTo time.Time) (int, error) {
	var n int
	err := l.r.QueryRowContext(ctx, `SELECT count(*) FROM verdicts WHERE start_at >= ? AND start_at < ? AND (unrelated_at >= ? OR action_at >= ?)`,
		wFrom.Unix(), wTo.Unix(), since.Unix(), since.Unix()).Scan(&n)
	return n, err
}

// SaidSince reports whether a person said a firing unrelated, of the rules
// in says, that started in [from, to), since a time: what makes a
// proposal's backtest stale, as an incident written there does.
func (l *Ledger) SaidSince(ctx context.Context, from, to, since time.Time, in func(rule string) bool) (bool, error) {
	vs, err := l.readVerdicts(ctx, l.r, "WHERE v.start_at >= ? AND v.start_at < ? AND v.unrelated_at > ?", from.Unix(), to.Unix(), since.Unix())
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(vs, func(v Verdict) bool { return in == nil || in(v.Rule) }), nil
}

// ReplayVerdicts are the verdicts kept by firings that started in [from,
// to), as a replay judged in memory takes them (verdict.Options.Verdicts):
// a verdict to be looked at again (moved) among them, as it judges in the
// ledger, but one another holds the firing of (movedHeld), which a replay
// places again itself.
func (l *Ledger) ReplayVerdicts(ctx context.Context, from, to time.Time) ([]verdict.Verdict, error) {
	vs, err := l.readVerdicts(ctx, l.r, "WHERE v.start_at >= ? AND v.start_at < ?", from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	out := make([]verdict.Verdict, 0, len(vs))
	for _, v := range vs {
		out = append(out, ReplayVerdict(v))
	}
	return out, nil
}

// ReplayVerdict is v as judging takes it.
func ReplayVerdict(v Verdict) verdict.Verdict {
	return verdict.Verdict{Rule: v.Rule, Labels: v.Labels, Start: v.Start, End: v.End, Unrelated: v.Keys(), Action: v.Action,
		At: latest(v.UnrelatedAt, v.ActionAt)}
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// keyMove is an incident's key, and its span, moved: from to key, from
// [lo, hi] the span its firings are found in; spanChanged says its span
// isn't the one verdicts were said of any more.
type keyMove struct {
	from, to    string
	lo, hi      int64
	spanChanged bool
}

// moveVerdictKeys moves, in the write that moves incidents' keys, the
// verdicts naming each and the firings' kept unrelated with them, so that
// a person's word follows the incident (docs/design/judging.md, "Through
// judging again"); a verdict whose incident's span changed is marked to be
// looked at again.
func (l *Ledger) moveVerdictKeys(t *txn, moves []keyMove) error {
	for _, m := range moves {
		if m.from == m.to && !m.spanChanged {
			continue
		}
		vs, err := l.readVerdicts(t.ctx, t, "WHERE v.start_at >= ? AND v.start_at <= ? AND v.unrelated != '[]'", m.lo, m.hi)
		if err != nil {
			return err
		}
		named := 0
		for _, v := range vs {
			i := slices.IndexFunc(v.Unrelated, func(u UnrelatedTo) bool { return u.Key == m.from })
			if i < 0 {
				continue
			}
			named++
			v.Unrelated[i].Key = m.to
			moved := v.Moved
			if m.spanChanged && moved == "" {
				moved = movedSpan + " " + m.to
			}
			b, err := json.Marshal(v.Unrelated)
			if err != nil {
				return err
			}
			if _, err := t.ExecContext(t.ctx, "UPDATE verdicts SET unrelated = ?, moved = ? WHERE id = ?", string(b), moved, v.ID); err != nil {
				return err
			}
		}
		// A firing keeps an incident unrelated only by a verdict naming
		// it: with none, none does.
		if m.from == m.to || named == 0 {
			continue
		}
		rows, err := t.QueryContext(t.ctx, "SELECT id, unrelated FROM firings WHERE start_at >= ? AND start_at <= ? AND unrelated != ''", m.lo, m.hi)
		if err != nil {
			return err
		}
		type kept struct {
			id   int64
			keys []string
		}
		var ks []kept
		for rows.Next() {
			var k kept
			var text string
			if err := rows.Scan(&k.id, &text); err != nil {
				rows.Close()
				return err
			}
			if err := json.Unmarshal([]byte(text), &k.keys); err != nil {
				rows.Close()
				return err
			}
			if i := slices.Index(k.keys, m.from); i >= 0 {
				k.keys[i] = m.to
				slices.Sort(k.keys)
				ks = append(ks, k)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, k := range ks {
			b, _ := json.Marshal(k.keys)
			if _, err := t.ExecContext(t.ctx, "UPDATE firings SET unrelated = ? WHERE id = ?", string(b), k.id); err != nil {
				return err
			}
		}
	}
	return nil
}

// countVerdictsNaming counts the verdicts whose firings started in [lo,
// hi] that name key: what keeps a window no longer there from being
// passed over (remapImpact).
func (l *Ledger) countVerdictsNaming(t *txn, key string, lo, hi int64) (int, error) {
	vs, err := l.readVerdicts(t.ctx, t, "WHERE v.start_at >= ? AND v.start_at <= ? AND v.unrelated != '[]'", lo, hi)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, v := range vs {
		if slices.Contains(v.Keys(), key) {
			n++
		}
	}
	return n, nil
}

// verdictReach is how far around a span the verdicts that may find a
// firing in it again are looked for: a day.
const verdictReach = 86400

// moveVerdicts finds again the firing of each verdict kept by a firing that
// started in [lo, hi] that isn't there any more: a run gave its firings
// again, starting one a step later, or merged two (docs/design/judging.md,
// "Through judging again"). Found (verdict.Place), the verdict is kept by
// it, marked moved; found but held by another verdict, it is marked so,
// and adds its "unrelated" to the firing (stones); not found, marked gone.
// It returns the span widened to the firings found, which are judged with
// the rest.
func (l *Ledger) moveVerdicts(t *txn, lo, hi int64, o verdict.Options) (int64, int64, error) {
	vs, err := l.readVerdicts(t.ctx, t, `WHERE v.start_at >= ? AND v.start_at <= ?
		AND NOT EXISTS (SELECT 1 FROM firings f WHERE f.series_id = v.series_id AND f.start_at = v.start_at)`, lo, hi)
	if err != nil || len(vs) == 0 {
		return lo, hi, err
	}
	reach := o.VerdictReach(nil)
	for _, v := range vs {
		x := ReplayVerdict(v)
		until := v.End
		if until.IsZero() {
			until = v.Start.Add(reach(v.Rule) + verdictReach*time.Second)
		}
		cands, err := l.firingsNear(t, v.Rule, v.Start.Add(-verdictReach*time.Second), until)
		if err != nil {
			return lo, hi, err
		}
		fs := make([]firing.Firing, len(cands))
		for i, c := range cands {
			fs[i] = c.Firing
		}
		at, _ := verdict.Place([]verdict.Verdict{x}, fs, o.Match, reach)
		moved := movedGone
		if i := at[0]; i >= 0 {
			to := cands[i]
			var other int
			if err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM verdicts WHERE series_id = ? AND start_at = ?", to.Series, to.Start.Unix()).Scan(&other); err != nil {
				return lo, hi, err
			}
			switch {
			case other > 0:
				moved = heldAt(to.Series, to.Start)
			default:
				moved = movedStart
				if to.Series != v.Series {
					moved = movedAlert
				}
				if _, err := t.ExecContext(t.ctx, "UPDATE verdicts SET series_id = ?, start_at = ?, moved = ? WHERE id = ?",
					to.Series, to.Start.Unix(), moved, v.ID); err != nil {
					return lo, hi, err
				}
				lo, hi = min(lo, to.Start.Unix()), max(hi, to.Start.Unix())
				continue
			}
			lo, hi = min(lo, to.Start.Unix()), max(hi, to.Start.Unix())
		}
		if moved != v.Moved {
			if _, err := t.ExecContext(t.ctx, "UPDATE verdicts SET moved = ? WHERE id = ?", moved, v.ID); err != nil {
				return lo, hi, err
			}
		}
	}
	return lo, hi, nil
}

// firingsNear are a rule's firings that started in [from, to], in
// firing.Sort's order.
func (l *Ledger) firingsNear(t *txn, rule string, from, to time.Time) ([]Firing, error) {
	rows, err := t.QueryContext(t.ctx, `SELECT f.id, s.id, s.labels, f.start_at, f.end_at FROM firings f JOIN series s ON s.id = f.series_id
		WHERE f.rule_id = (SELECT id FROM rules WHERE name = ?) AND f.start_at >= ? AND f.start_at <= ?`, rule, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Firing
	for rows.Next() {
		var f Firing
		var start int64
		var end sql.NullInt64
		var labels string
		if err := rows.Scan(&f.ID, &f.Series, &labels, &start, &end); err != nil {
			return nil, err
		}
		if f.Labels, err = l.labels(f.Series, labels); err != nil {
			return nil, err
		}
		f.Rule, f.Start, f.End = rule, unix(start), fromNull(end)
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// As firing.Sort has them: by start, then labels, so that one order
	// is found whatever the rows came in.
	slices.SortStableFunc(out, func(a, b Firing) int {
		if c := a.Start.Compare(b.Start); c != 0 {
			return c
		}
		return strings.Compare(firingLabelsText(a.Labels), firingLabelsText(b.Labels))
	})
	return out, nil
}

func firingLabelsText(m map[string]string) string {
	b, _ := json.Marshal(orEmptyMap(m))
	return string(b)
}

// saidOf is what stones reads of the verdicts of the firings it judges:
// by alert and start, the verdict kept there, and those another holds the
// firing of (movedHeld), which add their "unrelated" to it.
type saidOf struct {
	at   map[[2]int64]Verdict
	held map[[2]int64][]Verdict
}

// verdictsNear reads, for stones over [lo, hi], the verdicts kept there and
// those held around it.
func (l *Ledger) verdictsNear(t *txn, lo, hi int64) (saidOf, error) {
	s := saidOf{at: map[[2]int64]Verdict{}, held: map[[2]int64][]Verdict{}}
	vs, err := l.readVerdicts(t.ctx, t, "WHERE v.start_at >= ? AND v.start_at <= ?", lo-verdictReach, hi+verdictReach)
	if err != nil {
		return s, err
	}
	for _, v := range vs {
		if rest, ok := strings.CutPrefix(v.Moved, movedHeld+" "); ok {
			a, b, _ := strings.Cut(rest, " ")
			series, err1 := strconv.ParseInt(a, 10, 64)
			start, err2 := strconv.ParseInt(b, 10, 64)
			if err1 == nil && err2 == nil {
				k := [2]int64{series, start}
				s.held[k] = append(s.held[k], v)
			}
			continue
		}
		if at := v.Start.Unix(); at >= lo && at <= hi {
			s.at[[2]int64{v.Series, at}] = v
		}
	}
	return s, nil
}

// The bits stones keeps in firings.person.
const (
	personUnrelated  = 1  // white as Lapilli judged it, black by a person
	personYes        = 2  // a person said it needed action
	personNo         = 4  // that it didn't
	personRealWhite  = 8  // white only by incidents "This was real" wrote
	personRealQuiet  = 16 // of those, in a quiet window that matches it
	personActionBits = personYes | personNo
)

// personOf is what a firing's verdicts make of it, against the incidents
// it catches as Lapilli judges it (caught): white, its person bits but
// those of a quiet window, the incidents it is said unrelated to of those
// (sorted), and why the verdict kept by it is to be looked at again now,
// "" for no news.
func personOf(caught []counted, v Verdict, held []Verdict) (white bool, bits int64, unrelated []string, moved string) {
	said := map[string]UnrelatedTo{}
	for _, x := range append([]Verdict{v}, held...) {
		for _, u := range x.Unrelated {
			if _, ok := said[u.Key]; !ok {
				said[u.Key] = u
			}
		}
	}
	realOnly := true
	for _, in := range caught {
		if u, ok := said[in.ID]; ok {
			unrelated = append(unrelated, in.ID)
			if moved == "" && slices.ContainsFunc(v.Unrelated, func(x UnrelatedTo) bool { return x.Key == in.ID }) &&
				(!u.Start.Equal(in.Start) || !u.End.Equal(in.End)) {
				moved = movedSpan + " " + in.ID
			}
			continue
		}
		white = true
		realOnly = realOnly && in.Real
		if len(v.Unrelated) > 0 && moved == "" {
			moved = movedCatches + " " + in.ID
		}
	}
	slices.Sort(unrelated)
	switch {
	case !white && len(unrelated) > 0:
		bits |= personUnrelated
	case white && realOnly:
		bits |= personRealWhite
	}
	switch v.Action {
	case verdict.ActionYes:
		bits |= personYes
	case verdict.ActionNo:
		bits |= personNo
	}
	return white, bits, unrelated, moved
}

// unrelatedText is a firing's kept unrelated, as firings.unrelated has it.
func unrelatedText(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	b, _ := json.Marshal(keys)
	return string(b)
}

// unrelatedKeys reads firings.unrelated.
func unrelatedKeys(text string) []string {
	if text == "" {
		return nil
	}
	var out []string
	json.Unmarshal([]byte(text), &out)
	return out
}

// RulesOfSeries are the names of the rules of the alerts ids, by id: what
// "This was real" wrote an incident from (Incident.RealSeries) is of.
func (l *Ledger) RulesOfSeries(ctx context.Context, ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if len(ids) == 0 {
		return out, nil
	}
	b, _ := json.Marshal(ids)
	rows, err := l.r.QueryContext(ctx, `SELECT s.id, r.name FROM series s JOIN rules r ON r.id = s.rule_id WHERE s.id IN (`+l.d.ints("?")+`)`, string(b))
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
		out[id] = name
	}
	return out, rows.Err()
}
