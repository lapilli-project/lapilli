package serve

import (
	"context"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
)

// An alarm and its history, one rule (docs/design/alarms.md): what a rule
// Lapilli doesn't replay has for a record, which retiring rests on.

// replayed reports whether a rule is replayed, or would be but for being
// off: a cloud's alarm is when its condition is PromQL, and Lapilli can say
// what it would catch; any other rule turned off is taken to have been.
func replayed(r rules.Rule) bool {
	if r.Cloud != "" {
		return r.Expr != ""
	}
	return r.Unsupported == "" || isOff(r)
}

// recordOK says whether a rule has a record of twelve weeks to retire it
// by: one replayed, or one turned off, has; an alarm Lapilli doesn't replay
// has its history's, copied in and tied to it, once the copies cover
// twelve weeks without a gap (Coverage) up to now (within --source-quiet,
// as a source that stopped is told of), and no history of it held apart
// at another severity fired within them; any other has none, and why says
// why, in words the page translates. since is when the copies begin, or,
// for one that stopped, end, or when the history held apart last fired.
func (s *Server) recordOK(ctx context.Context, name string, r rules.Rule, ties map[string]ledger.TieState) (ok bool, why string, since time.Time, err error) {
	if r.Unsupported == "" || isOff(r) {
		return true, "", since, nil
	}
	if ties == nil {
		if ties, err = s.l.Ties(ctx); err != nil {
			return false, "", since, err
		}
	}
	st, tied := ties[name]
	switch {
	case !tied || st.Source == "":
		return false, noRecord, since, nil
	case st.Since == 0:
		return false, noRecord, since, nil
	}
	quiet := s.cfg.SourceQuiet
	if quiet <= 0 {
		quiet = sourceQuiet
	}
	if to := time.Unix(st.To, 0); s.now().Sub(to) > quiet {
		return false, staleRecord, to, nil
	}
	if apart := time.Unix(st.Apart, 0); st.Apart != 0 && s.now().Sub(apart) < weeks*weekLen {
		return false, apartRecord, apart, nil
	}
	since = time.Unix(st.Since, 0)
	if s.now().Sub(since) < weeks*weekLen {
		return false, shortRecord, since, nil
	}
	return true, "", since, nil
}

// Why a rule Lapilli doesn't replay has no record to retire it by.
const (
	noRecord    = "it isn't replayed and no history is tied to it: what it would catch is unknown, so retiring it isn't proposed"
	shortRecord = "its history is copied in without a gap for less than twelve weeks: retiring it rests on twelve"
	staleRecord = "its history isn't copied in up to now: retiring it rests on twelve weeks up to now"
	apartRecord = "it fired at another severity within twelve weeks, a history held apart from its own: retiring it rests on twelve weeks of one"
)
