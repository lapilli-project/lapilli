package serve

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"lapilli.dev/internal/rules"
)

// Following a history of the rules (docs/design/history.md): with
// --rules-history and no --rules, the directory is the rules. Its newest
// version is the rules in effect, and each version added to it later, as
// recipes/versions/follow.sh adds the commits it fetches, is recorded from
// its own time, not from when it was seen: the hours since then, already
// replayed with the version before, are replayed again.
//
// A version that can't be read (a commit whose rendering holds no rules,
// none the selector keeps, or a file that doesn't parse) is passed over,
// once, saying so, and the version before it stays in effect until one that
// reads: following never stops on one commit.

// following reports whether the rules are the --rules-history directory's.
func (s *Server) following() bool {
	return s.cfg.RulesHistory != "" && s.cfg.RulesPath == ""
}

// pickHistory is how a version in the history is read: its rules alone,
// those --rules-select keeps.
func (s *Server) pickHistory() rules.Picking {
	return rules.Picking{Select: s.cfg.RulesSelect, Trim: true}
}

// historyVersion is a version of the history read: when it took effect,
// where it is, and its rules with the extras beside them.
type historyVersion struct {
	at    time.Time
	path  string
	rules []rules.Rule
	docs  []rules.Doc
}

// unreadable are the versions said to be unreadable, so each is said once.
var unreadable = struct {
	sync.Mutex
	m map[string]bool
}{m: map[string]bool{}}

// readVersion reads a version of the history, with the extras read beside
// the rules (a cloud's alarms, a Loki's rules); ok is false for one that
// can't be read, said once.
func (s *Server) readVersion(h rules.Then, xrs []rules.Rule, xdocs []rules.Doc, picked *rules.Picked) (historyVersion, bool) {
	rs, docs, p, err := rules.ReadPicked(h.Path, s.pickHistory())
	picked.Add(p)
	if err == nil && len(s.matching(rs)) == 0 {
		err = errors.New("no alert rules to replay")
	}
	if err != nil {
		unreadable.Lock()
		said := unreadable.m[s.cfg.Ledger+"\x00"+h.Path]
		unreadable.m[s.cfg.Ledger+"\x00"+h.Path] = true
		unreadable.Unlock()
		if !said {
			s.log.Warn("--rules-history: a version that can't be read is passed over; the one before it stays in effect",
				"dir", h.Path, "err", err, "passed over", p.String())
		}
		return historyVersion{}, false
	}
	return historyVersion{at: h.At, path: h.Path, rules: append(rs, xrs...), docs: append(docs, xdocs...)}, true
}

// extrasNow reads the extras for versions of the history; what can't be
// read now is logged, and the versions are read without it.
func (s *Server) extrasNow(ctx context.Context) ([]rules.Rule, []rules.Doc) {
	rs, docs, err := s.extras(ctx)
	if err != nil {
		s.log.Warn("--rules-history: the rules read beside the history (--alarms, --loki-rules) couldn't be read: its versions are read without them", "err", err)
		return nil, nil
	}
	return rs, docs
}

// newest reads the history's newest version from before now that reads:
// the rules in effect, when following.
func (s *Server) newest() ([]rules.Rule, []rules.Doc, error) {
	hs, err := rules.History(s.cfg.RulesHistory)
	if err != nil {
		return nil, nil, fmt.Errorf("--rules-history: %w", err)
	}
	now := s.now()
	for i := len(hs) - 1; i >= 0; i-- {
		if hs[i].At.After(now) {
			continue
		}
		var picked rules.Picked
		if v, ok := s.readVersion(hs[i], nil, nil, &picked); ok {
			s.sayPicked("--rules-history", picked)
			return v.rules, v.docs, nil
		}
	}
	return nil, nil, fmt.Errorf("--rules-history: %s holds no version from before now that reads", s.cfg.RulesHistory)
}

// follow records the versions added to the history since the last it
// followed, each from its time, and has the hours since each replayed
// again. A ledger with no version yet takes the history as a first start
// does, its backfill replaying each day with that day's rules.
func (s *Server) follow(ctx context.Context) error {
	latest, ok, err := s.l.Latest(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return s.history(ctx)
	}
	hs, err := rules.History(s.cfg.RulesHistory)
	if errors.Is(err, rules.ErrNoVersions) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("--rules-history: %w", err)
	}
	// After the last version followed; before any, after the ledger's
	// latest, or for one from before the ledger began, after when the runs
	// began. Not after the latest when there is a last followed: a version
	// recorded since for the extras beside it (alarms, Loki) is no later a
	// commit of the history.
	var after time.Time
	if v, err := s.l.Meta(ctx, "followed"); err != nil {
		return err
	} else if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		after = time.Unix(n, 0)
	} else if after = latest.Since; after.IsZero() {
		if after, err = s.l.RunsFrom(ctx); err != nil {
			return err
		}
	}
	now := s.now()
	var xrs []rules.Rule
	var xdocs []rules.Doc
	read := false
	for _, h := range hs {
		if !h.At.After(after) || h.At.After(now) {
			continue
		}
		if !read {
			xrs, xdocs = s.extrasNow(ctx)
			read = true
		}
		var picked rules.Picked
		if v, ok := s.readVersion(h, xrs, xdocs, &picked); ok {
			if err := s.record(ctx, v, now.Add(-s.cfg.Backfill)); err != nil {
				return err
			}
		}
		if err := s.l.SetMeta(ctx, "followed", strconv.FormatInt(h.At.Unix(), 10)); err != nil {
			return err
		}
	}
	return nil
}

// record keeps a version followed, when its rules decide anything
// differently from the latest's, and has the hours from its time replayed
// again, back to floor (--backfill's start) at most: noted before the
// version is kept, so no version is kept without it.
func (s *Server) record(ctx context.Context, v historyVersion, floor time.Time) error {
	latest, ok, err := s.l.Latest(ctx)
	if err != nil {
		return err
	}
	if ok && latest.Digest == digestOf(s.matching(v.rules)) {
		return nil
	}
	at := v.at
	if at.Before(floor) {
		s.log.Warn("--rules-history: a version from before --backfill; the hours since --backfill's start are replayed again", "since", v.at)
		at = floor
	}
	if err := s.l.Again(ctx, at); err != nil {
		return err
	}
	id, _, err := s.version(ctx, v.rules, v.docs, v.at)
	if err != nil {
		return fmt.Errorf("--rules-history: %s: %w", v.path, err)
	}
	if id == 0 {
		return nil
	}
	s.log.Info("rules changed: a new version, from the history", "version", id, "since", v.at, "dir", v.path)
	if err := s.seen(ctx, id); err != nil {
		s.fail("changing the rules", err)
	}
	s.poke() // to review it; the next run replays the hours since
	return nil
}
