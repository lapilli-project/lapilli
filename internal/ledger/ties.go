package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"lapilli.dev/internal/rules"
)

// An alarm and its history, one rule (docs/design/alarms.md): a cloud's
// alarm Lapilli doesn't replay, read through --alarms SOURCE=URL, is judged
// by what really fired, its history copied in under SOURCE. The recorded
// rule of that history is the alarm's: named as the alarm is judged, not
// apart from it. Nothing in the ledger's schema changes: rules.source and
// rules.sent say it, as for every recorded rule.

// AlarmID is the label a recipe gives each firing with its alarm's own ID:
// what ties the history to the alarm, whatever it is named.
const AlarmID = "alarm_id"

// A Tie is an alarm and the rule its history is copied in as.
type Tie struct {
	// Source is the --alarms source the alarm is read under, and its
	// history copied in under.
	Source string `json:"source"`
	// Rule is the name the alarm is judged under.
	Rule string `json:"rule"`
	// ID is the alarm's own, and Name the name its recipe gives it.
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Untied is why an alarm read through --alarms has no history tied to it,
// by the name it is judged under.
type Untied map[string]string

// Why an alarm has no history tied to it.
const (
	UntiedShared   = "another alarm read under its source has its name, and its history doesn't say which of the two it is"
	UntiedPolicy   = "its policy has several conditions, and a policy's alerts don't say which of them opened them"
	UntiedTwo      = "more than one rule of its source's history is tied to it, so which is its isn't known"
	UntiedNone     = "no history is copied in for it: copy its source's in with recipes/firings"
	UntiedReplayed = "" // a PromQL alarm: judged by its replay, its history apart
)

// TieKey is what an alarm read through --alarms is tied by: its ID, and
// the name its recipe gives it (Google's policy's display name, every
// other cloud's alarm's own). ok is false for an alarm Lapilli replays,
// judged by its replay, and for every rule that isn't a cloud's alarm read
// through --alarms.
func TieKey(r rules.Rule) (id, name string, ok bool) {
	if r.Cloud == "" || r.Namespace == "" || r.Expr != "" {
		return "", "", false
	}
	name = r.Name
	if r.Cloud == "google" {
		name = r.Group // the policy's display name
	}
	return r.Source, name, true
}

// TiesOf are the ties a version's rules make, and why each alarm that is
// read through --alarms and isn't replayed has none: a name two alarms
// share ties neither by name, an ID two rules share (a policy's
// conditions) ties none.
func TiesOf(rs []rules.Rule, names []string) ([]Tie, Untied) {
	type key struct{ source, v string }
	ids, byName := map[key]int{}, map[key]int{}
	var cand []Tie
	for i, r := range rs {
		id, name, ok := TieKey(r)
		if !ok || i >= len(names) || names[i] == "" {
			continue
		}
		cand = append(cand, Tie{Source: r.Namespace, Rule: names[i], ID: id, Name: name})
		ids[key{r.Namespace, id}]++
		byName[key{r.Namespace, strings.ToLower(name)}]++
	}
	var out []Tie
	untied := Untied{}
	for _, c := range cand {
		if ids[key{c.Source, c.ID}] > 1 {
			untied[c.Rule] = UntiedPolicy
			continue
		}
		if byName[key{c.Source, strings.ToLower(c.Name)}] > 1 {
			c.Name = "" // tied by its ID alone, where its history gives one
		}
		out = append(out, c)
	}
	return out, untied
}

// ties are the latest version's, worked out once a version.
type ties struct {
	sync.Mutex
	version int64
	list    []Tie
	untied  Untied
}

// reader reads the ledger: in a write, or apart from one.
type reader interface {
	querier
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// latestTies are the ties of the latest version.
func (l *Ledger) latestTies(ctx context.Context, q reader) ([]Tie, Untied, error) {
	var id int64
	var docText, names string
	err := q.QueryRowContext(ctx, "SELECT id, docs, names FROM versions ORDER BY since DESC, id DESC LIMIT 1").Scan(&id, &docText, &names)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	l.ties.Lock()
	defer l.ties.Unlock()
	if l.ties.version == id {
		return l.ties.list, l.ties.untied, nil
	}
	var refs []docRef
	var ns []string
	if err := json.Unmarshal([]byte(docText), &refs); err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal([]byte(names), &ns); err != nil {
		return nil, nil, err
	}
	// Only a version that reads alarms through --alarms makes ties.
	alarms := false
	for _, r := range refs {
		alarms = alarms || r.Namespace != ""
	}
	var list []Tie
	untied := Untied{}
	if alarms {
		docs, err := l.docsIn(ctx, q, refs)
		if err != nil {
			return nil, nil, err
		}
		rs, err := rules.FromDocs(docs)
		if err != nil || len(rs) != len(ns) {
			return nil, nil, nil // read afresh by serve, which names it again
		}
		list, untied = TiesOf(rs, ns)
	}
	l.ties.version, l.ties.list, l.ties.untied = id, list, untied
	return list, untied, nil
}

// heldBy are a source's recorded rules, each with the one alarm ID its
// alerts carry, "" for none; many says they carry more than one.
type heldBy struct {
	id                int64
	name, sent, alarm string
	many              bool
	series            int
}

// recordedOf are the recorded rules of the sources ties are of.
func (l *Ledger) recordedOf(ctx context.Context, q reader, sources map[string]bool) (map[string][]heldBy, error) {
	out := map[string][]heldBy{}
	for src := range sources {
		rows, err := q.QueryContext(ctx, `SELECT r.id, r.name, r.sent, count(s.id),
				count(DISTINCT json_extract(s.labels, '$.`+AlarmID+`')), ifnull(min(json_extract(s.labels, '$.`+AlarmID+`')), '')
			FROM rules r LEFT JOIN series s ON s.rule_id = r.id WHERE r.source = ? AND r.source != '' GROUP BY r.id`, src)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var h heldBy
			var ids int
			if err := rows.Scan(&h.id, &h.name, &h.sent, &h.series, &ids, &h.alarm); err != nil {
				rows.Close()
				return nil, err
			}
			h.many = ids > 1
			out[src] = append(out[src], h)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// tied are, for ties, the recorded rule each is: by the alarm's ID where
// the history labels its alerts with one, by name where it gives none.
// Two rules tied to one alarm tie neither. It gives the recorded rule's id
// by the name the alarm is judged under, and why each tie that isn't made
// isn't.
func (l *Ledger) tied(ctx context.Context, q reader, list []Tie) (map[string]heldBy, Untied, error) {
	if len(list) == 0 {
		return nil, nil, nil
	}
	sources := map[string]bool{}
	for _, x := range list {
		sources[x.Source] = true
	}
	held, err := l.recordedOf(ctx, q, sources)
	if err != nil {
		return nil, nil, err
	}
	byID, byName := map[string]string{}, map[string]string{}
	for _, x := range list {
		byID[x.Source+"\x00"+x.ID] = x.Rule
		if x.Name != "" {
			byName[x.Source+"\x00"+x.Name] = x.Rule
		}
	}
	found := map[string][]heldBy{}
	for src, hs := range held {
		for _, h := range hs {
			var rule string
			switch {
			case h.many:
				continue // the alerts of two alarms
			case h.alarm != "":
				rule = byID[src+"\x00"+h.alarm]
				if rule == "" {
					// Azure's ids are compared in any letter case.
					for k, v := range byID {
						if strings.EqualFold(k, src+"\x00"+h.alarm) {
							rule = v
						}
					}
				}
			default:
				rule = byName[src+"\x00"+h.sent]
			}
			if rule != "" {
				found[rule] = append(found[rule], h)
			}
		}
	}
	out, untied := map[string]heldBy{}, Untied{}
	for _, x := range list {
		hs := found[x.Rule]
		if len(hs) > 1 && x.Name != "" {
			// An alarm renamed: the history under its name now is its.
			if named := slices.DeleteFunc(slices.Clone(hs), func(h heldBy) bool { return h.sent != x.Name }); len(named) == 1 {
				hs = named
			}
		}
		switch {
		case len(hs) == 1:
			out[x.Rule] = hs[0]
		case len(hs) > 1:
			untied[x.Rule] = UntiedTwo
		case x.Name == "":
			untied[x.Rule] = UntiedShared
		default:
			untied[x.Rule] = UntiedNone
		}
	}
	return out, untied, nil
}

// tie names each recorded rule tied to an alarm as the alarm is judged,
// and gives the names so held, which yield leaves. An empty rule of that
// name, made for the alarm by a run, goes; a recorded rule holding it is
// named apart first.
func (l *Ledger) tie(t *txn, list []Tie) (map[string]bool, error) {
	found, _, err := l.tied(t.ctx, t, list)
	if err != nil || len(found) == 0 {
		return nil, err
	}
	keep := map[string]bool{}
	claimed := map[string]bool{}
	for rule := range found {
		claimed[rule] = true
	}
	for rule, h := range found {
		keep[rule] = true
		if h.name == rule {
			continue
		}
		var other int64
		var source string
		var series int
		err := t.QueryRowContext(t.ctx, "SELECT r.id, r.source, (SELECT count(*) FROM series WHERE rule_id = r.id) FROM rules r WHERE r.name = ?", rule).
			Scan(&other, &source, &series)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return nil, err
		case source == "" && series == 0:
			// What a run kept of the alarm, with nothing judged under it.
			if _, err := t.ExecContext(t.ctx, "DELETE FROM rules WHERE id = ?", other); err != nil {
				return nil, err
			}
		case source == "":
			delete(keep, rule) // a rule that fired under the name: not tied
			continue
		default:
			var sent, sev string
			if err := t.QueryRowContext(t.ctx, "SELECT sent, severity FROM rules WHERE id = ?", other).Scan(&sent, &sev); err != nil {
				return nil, err
			}
			name, err := l.freeName(t, source, recordedKey{sent, sev}, claimed)
			if err != nil {
				return nil, err
			}
			if err := l.rename(t, other, rule, name); err != nil {
				return nil, err
			}
		}
		if err := l.rename(t, h.id, h.name, rule); err != nil {
			return nil, err
		}
	}
	return keep, nil
}

// rename names a rule anew, and what people kept about it under its old
// name goes with it.
func (l *Ledger) rename(t *txn, id int64, was, name string) error {
	if _, err := t.ExecContext(t.ctx, "UPDATE rules SET name = ? WHERE id = ?", name, id); err != nil {
		return err
	}
	for _, q := range []string{
		"UPDATE OR IGNORE notes SET rule = ? WHERE rule = ?",
		"UPDATE tasks SET rule = ? WHERE rule = ?",
		"UPDATE comments SET on_key = ? WHERE on_kind = 'rule' AND on_key = ?",
	} {
		if _, err := t.ExecContext(t.ctx, q, name, was); err != nil {
			return err
		}
	}
	if t.gone == nil {
		t.gone = map[string]bool{}
	}
	t.gone[was] = true
	delete(t.rules, was)
	t.rules[name] = id
	t.all, t.recorded = true, true
	return nil
}

// TieState is what the ledger has of an alarm and its history: the
// source's rule tied to it, and when its source's history begins.
type TieState struct {
	Source string
	// Since is when the source's earliest firing began: an alarm's record
	// covers the weeks since.
	Since int64
	// Why is why an alarm has no history tied to it, when it hasn't.
	Why string
}

// Ties are, by the name each alarm read through --alarms and not replayed
// is judged under, its history tied to it, or why none is.
func (l *Ledger) Ties(ctx context.Context) (map[string]TieState, error) {
	out := map[string]TieState{}
	list, untied, err := l.latestTies(ctx, l.r)
	if err != nil || (len(list) == 0 && len(untied) == 0) {
		return out, err
	}
	found, why, err := l.tied(ctx, l.r, list)
	if err != nil {
		return nil, err
	}
	since := map[string]int64{}
	for rule, h := range found {
		if h.name != rule {
			why[rule] = UntiedNone // not yet named as the alarm: the next copy or version names it
			continue
		}
		src := ""
		for _, x := range list {
			if x.Rule == rule {
				src = x.Source
			}
		}
		if _, ok := since[src]; !ok {
			var at sql.NullInt64
			if err := l.r.QueryRowContext(ctx, `SELECT min(f.start_at) FROM firings f JOIN rules r ON r.id = f.rule_id
				WHERE r.source = ? AND r.source != ''`, src).Scan(&at); err != nil {
				return nil, err
			}
			since[src] = at.Int64
		}
		out[rule] = TieState{Source: src, Since: since[src]}
	}
	for rule, w := range untied {
		out[rule] = TieState{Why: w}
	}
	for rule, w := range why {
		if _, ok := out[rule]; !ok {
			out[rule] = TieState{Why: w}
		}
	}
	return out, nil
}

// docsIn reads a version's documents.
func (l *Ledger) docsIn(ctx context.Context, q querier, refs []docRef) ([]rules.Doc, error) {
	out := make([]rules.Doc, 0, len(refs))
	for _, r := range refs {
		var z []byte
		if err := q.QueryRowContext(ctx, "SELECT data FROM docs WHERE hash = ?", r.Hash).Scan(&z); err != nil {
			return nil, fmt.Errorf("the document of %s: %w", r.Source, err)
		}
		var data []byte
		if err := ungz(z, &data); err != nil {
			return nil, err
		}
		out = append(out, rules.Doc{Source: r.Source, API: r.API, Data: data, Namespace: r.Namespace})
	}
	return out, nil
}
