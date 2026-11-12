package serve

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"lapilli.dev/internal/rules"
)

// Following a history of the rules (docs/design/history.md): with
// --rules-history and no --rules, the directory is the rules. Its newest
// version is the rules in effect, and each version added to it later, as
// recipes/versions/follow.sh adds the commits it fetches, is recorded from
// its own time, not from when it was seen: the hours since then, already
// replayed with the version before, are replayed again.

// following reports whether the rules are the --rules-history directory's.
func (s *Server) following() bool {
	return s.cfg.RulesHistory != "" && s.cfg.RulesPath == ""
}

// pickHistory is how a version in the history is read: its rules alone,
// those --rules-select keeps.
func (s *Server) pickHistory() rules.Picking {
	return rules.Picking{Select: s.cfg.RulesSelect, Trim: true}
}

// newest reads the history's newest version from before now: the rules in
// effect, when following.
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
		rs, docs, picked, err := rules.ReadPicked(hs[i].Path, s.pickHistory())
		s.sayPicked("--rules-history", picked)
		if err != nil {
			return nil, nil, fmt.Errorf("--rules-history: %w", err)
		}
		return rs, docs, nil
	}
	return nil, nil, fmt.Errorf("--rules-history: %s holds no version from before now", s.cfg.RulesHistory)
}

// follow records the versions added to the history since the last it
// recorded, each from its time, and has the hours since the earliest
// replayed again. A ledger with no version yet takes the history as a first
// start does, its backfill replaying each day with that day's rules.
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
	// After the last version read from the history, or, before any, after
	// the ledger's latest; one from before the ledger began is followed
	// from when the runs began.
	after := latest.Since
	if v, err := s.l.Meta(ctx, "followed"); err != nil {
		return err
	} else if n, err := strconv.ParseInt(v, 10, 64); err == nil && time.Unix(n, 0).After(after) {
		after = time.Unix(n, 0)
	}
	if after.IsZero() {
		if after, err = s.l.RunsFrom(ctx); err != nil {
			return err
		}
	}
	now := s.now()
	var earliest time.Time
	for _, h := range hs {
		if !h.At.After(after) || h.At.After(now) {
			continue
		}
		rs, docs, _, err := rules.ReadPicked(h.Path, s.pickHistory())
		if err == nil && len(s.matching(rs)) == 0 {
			err = errors.New("no alert rules to replay")
		}
		if err != nil {
			return fmt.Errorf("--rules-history: %s: %w", h.Path, err)
		}
		id, _, err := s.version(ctx, rs, docs, h.At)
		if err != nil {
			return fmt.Errorf("--rules-history: %s: %w", h.Path, err)
		}
		if err := s.l.SetMeta(ctx, "followed", strconv.FormatInt(h.At.Unix(), 10)); err != nil {
			return err
		}
		if id == 0 {
			continue
		}
		s.log.Info("rules changed: a new version, from the history", "version", id, "since", h.At, "dir", h.Path)
		if err := s.seen(ctx, id); err != nil {
			s.fail("changing the rules", err)
		}
		if earliest.IsZero() {
			earliest = h.At
		}
	}
	if earliest.IsZero() {
		return nil
	}
	// The hours since replayed again, back to --backfill at most.
	if floor := now.Add(-s.cfg.Backfill); earliest.Before(floor) {
		s.log.Warn("--rules-history: a version from before --backfill; the hours since --backfill's start are replayed again", "since", earliest)
		earliest = floor
	}
	if err := s.l.Again(ctx, earliest); err != nil {
		return err
	}
	s.poke() // to review it, and replay
	return nil
}
