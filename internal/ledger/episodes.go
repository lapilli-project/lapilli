package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strconv"
	"time"

	"lapilli.dev/internal/verdict"
)

// Episodes beside firings (docs/design/judging.md, item 2): each firing
// keeps the id of the first firing of its episode (firings.episode), and
// each first firing whether any firing of its episode is white
// (firings.episode_white). The tallies count the first firings, by the key
// each row has (day or week, rule, signals, and a kind for the marked
// ones), so that an episode is counted in its first firing's row and
// judged by that row's record, as each firing is by its own. No stone
// changes.
//
// An episode never crosses midnight, so the tally, which counts whole
// days, walks the firings of the days it counts before it counts them
// (walkEpisodes), and nothing outside them can change.

// firstFirings counts, of the firings f a tally reads, those that start
// an episode: an excused firing's episode is 0, never its own id.
const firstFirings = "sum(CASE WHEN f.episode = f.id THEN 1 ELSE 0 END)"

// metaEpisodesFrom is the Monday from which on every week's episodes are
// counted: on an upgrade to schema 025, and a change of zone, the week
// after the newest firing's, walked back a week a write (CountEpisodes).
// Missing, every week's are.
const metaEpisodesFrom = "episodes_from"

// episodesWalked, when set, is told how many firings each walk read: for
// tests of how far a write reaches.
var episodesWalked func(firings int)

// walkEpisodes gives each firing of the days [lo, hi] falls in its episode,
// by the match in effect and --refire of o, as verdict.Judge does: by rule
// and match values, in order of their start, parted at midnight. Only
// what changed is written.
func (l *Ledger) walkEpisodes(t *txn, lo, hi int64, o verdict.Options) error {
	d1, d2 := l.day(lo), l.day(hi)
	rows, err := t.QueryContext(t.ctx, `SELECT f.id, f.series_id, f.rule_id, f.start_at, f.end_at, f.day, f.white, f.excused, f.episode, f.episode_white, s.labels
		FROM firings f JOIN series s ON s.id = f.series_id
		WHERE f.start_at >= ? AND f.start_at < ? ORDER BY f.rule_id, f.start_at, f.id`, l.midnight(d1), l.midnight(d2+1))
	if err != nil {
		return err
	}
	type row struct {
		id, series, rule, day     int64
		start, end                time.Time
		white, excused            bool
		episode, was, whiteWas    int64
		labels                    map[string]string
		first                     int // the index of its episode's first firing
		episodeWhite, walkedWhite bool
	}
	var fs []row
	for rows.Next() {
		var r row
		var start int64
		var end sql.NullInt64
		var text string
		if err := rows.Scan(&r.id, &r.series, &r.rule, &start, &end, &r.day, &r.white, &r.excused, &r.was, &r.whiteWas, &text); err != nil {
			rows.Close()
			return err
		}
		if r.labels, err = l.labels(r.series, text); err != nil {
			rows.Close()
			return err
		}
		r.start, r.end = unix(start), fromNull(end)
		fs = append(fs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if episodesWalked != nil {
		episodesWalked(len(fs))
	}
	type walk struct {
		verdict.EpisodeWalk
		first int
	}
	var walks map[string]*walk
	for i := range fs {
		r := &fs[i]
		if i == 0 || r.rule != fs[i-1].rule {
			walks = map[string]*walk{} // a rule's firings are read together
		}
		if r.excused {
			r.first = -1 // in no episode
			continue
		}
		k := verdict.EpisodeKey("", r.labels, o.Match)
		w := walks[k]
		if w == nil {
			w = &walk{}
			walks[k] = w
		}
		if w.Starts(r.day, r.start, r.end, o.RefireWithin) {
			w.first = i
		}
		r.first = w.first
		if r.white {
			fs[w.first].walkedWhite = true
		}
	}
	up, err := t.updating("firings", []string{"episode", "episode_white"})
	if err != nil {
		return err
	}
	defer up.close()
	for i, r := range fs {
		var episode, white int64
		if r.first >= 0 {
			episode = fs[r.first].id
			if r.first == i && r.walkedWhite {
				white = 1
			}
		}
		if episode == r.was && white == r.whiteWas {
			continue
		}
		if err := up.add(episode, white, r.id); err != nil {
			return err
		}
	}
	return up.close()
}

// EpisodesFrom is the Monday from which on the weeks' episodes are
// counted; ok false when every week's are. A week before it says its
// episodes aren't counted yet.
func (l *Ledger) EpisodesFrom(ctx context.Context) (week int64, ok bool, err error) {
	return l.episodesFrom(ctx, l.r)
}

func (l *Ledger) episodesFrom(ctx context.Context, q querier) (int64, bool, error) {
	v, err := l.meta(ctx, q, metaEpisodesFrom)
	if err != nil || v == "" {
		return 0, false, err
	}
	w, err := strconv.ParseInt(v, 10, 64)
	return w, err == nil, err
}

// EpisodesCounted reports whether the episodes of [from, to) are counted:
// every week it meets is from EpisodesFrom on.
func (l *Ledger) EpisodesCounted(ctx context.Context, from time.Time) (bool, error) {
	w, ok, err := l.EpisodesFrom(ctx)
	if err != nil || !ok {
		return err == nil, err
	}
	return Week(l.Day(from)) >= w, nil
}

// newestBefore is the latest start of a firing before a time, which the
// walk of CountEpisodes reads in the ledger's one write, a week a write:
// along firings_start, never a scan of every firing.
const newestBefore = "SELECT max(start_at) FROM firings WHERE start_at < ?"

// CountEpisodes counts the episodes of the weeks not counted yet (after an
// upgrade to schema 025, or a change of zone), a week a write, from the
// newest back, so that the weeks people read first are counted first,
// while still says so: a walk of its own, not pending, which JudgePending
// takes from the oldest. Each write reads where the walk got to again, so
// that two replicas walking at once walk each week once. It gives how
// many weeks it counted. No stone changes.
func (l *Ledger) CountEpisodes(ctx context.Context, o verdict.Options, still func() bool) (int, error) {
	n := 0
	for {
		if _, ok, err := l.EpisodesFrom(ctx); err != nil || !ok {
			return n, err
		}
		if still != nil && !still() {
			return n, nil
		}
		walked := false
		err := l.write(ctx, func(t *txn) error {
			walked = false
			from, ok, err := l.episodesFrom(t.ctx, t)
			if err != nil || !ok {
				return err
			}
			// The next week back with a firing: those with none have
			// nothing to count. By its start, along firings_start, not
			// by its day, which no index has: the day of the latest
			// start before the week's midnight.
			var newest sql.NullInt64
			if err := t.QueryRowContext(t.ctx, newestBefore, l.midnight(from)).Scan(&newest); err != nil {
				return err
			}
			if !newest.Valid {
				// Every week walked: none is said not counted.
				_, err := t.ExecContext(t.ctx, "DELETE FROM meta WHERE key = ?", metaEpisodesFrom)
				return err
			}
			w := Week(l.day(newest.Int64))
			lo, hi := l.midnight(w), l.midnight(w+7)-1
			if err := l.walkEpisodes(t, lo, hi, o); err != nil {
				return err
			}
			if err := l.tally(t, lo, hi); err != nil {
				return err
			}
			walked = true
			return setMeta(t.ctx, t, metaEpisodesFrom, strconv.FormatInt(w, 10))
		})
		if err != nil {
			return n, err
		}
		if walked {
			n++
		}
	}
}

// uncountEpisodes takes back every firing's episode, in a change of zone,
// whose days changed: the weeks are counted again by CountEpisodes, from
// the week after the newest firing's, a week a write. One write over
// every firing kept would hold the ledger's one writer as long as all of
// them take.
func (l *Ledger) uncountEpisodes(t *txn) error {
	if _, err := t.ExecContext(t.ctx, "UPDATE firings SET episode = 0, episode_white = 0 WHERE episode <> 0 OR episode_white <> 0"); err != nil {
		return err
	}
	var newest sql.NullInt64
	if err := t.QueryRowContext(t.ctx, "SELECT max(start_at) FROM firings").Scan(&newest); err != nil {
		return err
	}
	if !newest.Valid {
		_, err := t.ExecContext(t.ctx, "DELETE FROM meta WHERE key = ?", metaEpisodesFrom)
		return err
	}
	return setMeta(t.ctx, t, metaEpisodesFrom, strconv.FormatInt(Week(l.day(newest.Int64))+7, 10))
}

// Episode is an episode as a firing's line says it: when its first firing
// started, and how many firings it held.
type Episode struct {
	Start   time.Time
	Firings int
}

// Episodes are the episodes the firings given are of, by the id of their
// first firing (Firing.Episode): what a list of firings says of each.
// Read by the firings' rules over the days they fell on, as an episode is
// one rule's within a day, along the index of a rule's firings.
func (l *Ledger) Episodes(ctx context.Context, fs []Firing) (map[int64]Episode, error) {
	out := map[int64]Episode{}
	var ids []int64
	var names []string
	d1, d2 := int64(0), int64(0)
	for _, f := range fs {
		if f.Episode == 0 {
			continue
		}
		d := l.day(f.Start.Unix())
		if len(ids) == 0 || d < d1 {
			d1 = d
		}
		if len(ids) == 0 || d > d2 {
			d2 = d
		}
		ids = append(ids, f.Episode)
		names = append(names, f.Rule)
	}
	if len(ids) == 0 {
		return out, nil
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	slices.Sort(names)
	rules, err := json.Marshal(slices.Compact(names))
	if err != nil {
		return nil, err
	}
	for len(ids) > 0 {
		part := ids[:min(len(ids), pgRows)]
		ids = ids[len(part):]
		b, err := json.Marshal(part)
		if err != nil {
			return nil, err
		}
		err = l.eachRow(ctx, func(sc scanner) error {
			var id, start int64
			var n int
			if err := sc.Scan(&id, &start, &n); err != nil {
				return err
			}
			out[id] = Episode{Start: unix(start), Firings: n}
			return nil
		}, `SELECT episode, min(start_at), count(*) FROM firings WHERE rule_id IN (SELECT id FROM rules WHERE name IN (`+l.d.texts("?1")+`))
			AND start_at >= ?2 AND start_at < ?3 AND episode IN (`+l.d.ints("?4")+`) GROUP BY episode`, string(rules), l.midnight(d1), l.midnight(d2+1), string(b))
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
