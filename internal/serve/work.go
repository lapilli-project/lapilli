package serve

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/report"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
	"lapilli.dev/internal/sse"
	"lapilli.dev/internal/verdict"
)

// work does the ledger's jobs one at a time, so that Prometheus is asked
// for one thing at a time: runs, reviews of changes to the rules, the
// weekly digest, judging again after the settings change, pruning. It is
// the leader's (lead): a change on another replica wakes it through the
// ledger (Woken).
func (s *Server) work(ctx context.Context) {
	tick := time.NewTicker(s.cfg.Every)
	defer tick.Stop()
	next := true // a run is due
	for {
		s.jobs(ctx, next)
		next = false
		s.ready.Store(true)
		s.status.set("")
		s.idle()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			next = true
		case <-s.wake:
		case <-s.l.Woken():
		}
	}
}

// judgeAll judges every firing again: a variable, so that a test can
// count how often.
var judgeAll = (*ledger.Ledger).JudgeAll

// linkedMeta is the key in meta of the --linked-only and --match-strict
// the ledger's stones were last all given by: linkedText's.
const linkedMeta = "linked-only"

// judgedBy is the version of what the ledger's stones are judged by, ahead
// of the flags in linkedText: a release that changes what a stone says
// (schema 032's loose catches) raises it, and a ledger last judged under
// the one before judges every firing again once at its first start, as a
// change of a flag does.
const judgedBy = "j2"

// linkedText is judgedBy, then --linked-only as "1" and --match-strict as
// "m", in that order, as meta keeps them: a ledger before they were judged
// has "", and one judged before judgedBy has the flags alone.
func linkedText(o verdict.Options) string {
	v := judgedBy
	if o.LinkedOnly {
		v += "1"
	}
	if o.MatchStrict {
		v += "m"
	}
	return v
}

// linkedSaid is linkedText's --linked-only as the log says it.
func linkedSaid(v string) string {
	return onOff(strings.Contains(strings.TrimPrefix(v, judgedBy), "1"))
}

// strictSaid is linkedText's --match-strict as the log says it.
func strictSaid(v string) string {
	return onOff(strings.Contains(strings.TrimPrefix(v, judgedBy), "m"))
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// linkedAgain reads what the ledger's stones were last all given by
// (linkedMeta), and says whether this replica, leading, judges every
// firing again with its own --linked-only and --match-strict: when the ledger's is the other
// value and still the one this replica knew, as it found it at start or
// as it last agreed with its flag. One another replica wrote since is a
// change newer than this replica's flag, as in a rolling update: leading,
// this replica judges what it runs with its own, and leaves the rest as
// judged, so that replicas that differ judge everything again once for
// each change of value, not at each change of leader.
func (s *Server) linkedAgain(ctx context.Context, own string) (was string, again bool, err error) {
	if was, err = s.l.Meta(ctx, linkedMeta); err != nil {
		return "", false, err
	}
	s.linkedKnown.Lock()
	defer s.linkedKnown.Unlock()
	if !s.linkedKnown.read || was == own {
		s.linkedKnown.read, s.linkedKnown.v = true, was
	}
	return was, was != own && was == s.linkedKnown.v, nil
}

// logLinked says in the log, at start and on becoming the leader, when
// this replica's --linked-only isn't what the ledger was last judged by:
// both values, and whether it judges every firing again (linkedAgain).
func (s *Server) logLinked(ctx context.Context) {
	own := linkedText(s.cfg.Options)
	was, again, err := s.linkedAgain(ctx, own)
	switch {
	case err != nil || was == own:
	case again:
		s.log.Info("--linked-only or --match-strict isn't what the ledger was last judged by: every firing is judged again with this replica's, once, as it leads",
			"linked_only", linkedSaid(own), "ledger", linkedSaid(was), "match_strict", strictSaid(own), "ledger_match_strict", strictSaid(was))
	default:
		s.log.Warn("--linked-only or --match-strict isn't what the ledger was last judged by, which another replica judged every firing with since this one started: leading, this replica judges what it runs with its own and nothing again; give the replicas the same, and start this one again once they have it, which judges every firing again if the ledger's is still the other",
			"linked_only", linkedSaid(own), "ledger", linkedSaid(was), "match_strict", strictSaid(own), "ledger_match_strict", strictSaid(was))
	}
}

// jobs does each job in turn, each only while this replica still leads
// (still): one whose lease ended starts none of them.
func (s *Server) jobs(ctx context.Context, run bool) {
	if !s.still(ctx) {
		return
	}
	// Pull request backtests ended a day ago go, the port on or not.
	if err := s.l.PruneBacktests(ctx, s.now()); err != nil && ctx.Err() == nil {
		s.log.Warn("letting go of the pull request backtests kept", "err", err)
	}
	// The named impact signals and the rule entries serve was started
	// with, kept: a change to which there are, or what they match, pends
	// every firing to be judged again, in the ledger (JudgePending, below).
	pended, err := s.syncSignals(ctx)
	if err != nil {
		s.fail("keeping the impact signals", err)
	}
	// Taken before the settings are read, so a change saved in between is
	// judged with: here, or, asked on another replica, in the ledger.
	rejudge := s.rejudge.Swap(false)
	asked, other := s.rejudgeAsked(ctx)
	rejudge = rejudge || other
	o, err := s.options(ctx)
	if err != nil {
		s.rejudge.Store(rejudge || s.rejudge.Load())
		s.fail("settings", err)
		return
	}
	// --linked-only other than the ledger was last judged with, as after
	// a start with it given or taken away: every firing judged again
	// with it, and the ledger told so once that is done
	// (docs/design/links.md); not to undo another replica's newer change
	// (linkedAgain).
	linked := linkedText(o)
	relinked := false
	if _, again, err := s.linkedAgain(ctx, linked); err != nil {
		s.fail("settings", err)
	} else if again {
		rejudge, relinked = true, true
	}
	judged := func() {
		if !relinked {
			return
		}
		if err := s.l.SetMeta(ctx, linkedMeta, linked); err != nil {
			s.fail("judging again", err)
			return
		}
		s.linkedKnown.Lock()
		s.linkedKnown.read, s.linkedKnown.v = true, linked
		s.linkedKnown.Unlock()
	}
	// Once, on the upgrade to this release: the windows of signals not in
	// effect kept from before, which rules with an entry may have caught,
	// pended to be judged again with the rest (docs/design/judging.md,
	// item 5).
	if n, err := s.l.PendNotInEffect(ctx, o); err != nil {
		s.fail("judging again the windows of impact signals not in effect", err)
	} else if n > 0 {
		s.log.Info("windows of impact signals not in effect judged again, once: they keep no rule's record, and only rules with no rule entry catch them", "windows", n)
	}
	// Spans a quiet window left to judge again, should its write have been
	// cut short.
	if !s.still(ctx) {
		s.rejudge.Store(rejudge || s.rejudge.Load())
		return
	}
	if err := s.l.JudgePending(ctx, o); errors.Is(err, ledger.ErrSignalsMoved) {
		// The signals changed since the options were read: judged by the
		// next round, with them read anew.
		pended = false
		s.poke()
	} else if err != nil {
		s.fail("judging again", err)
		pended = false
	}
	// The episodes of the weeks not counted yet, after an upgrade or a
	// change of zone, a week a write from the newest back
	// (docs/design/judging.md, item 2): no stone changes.
	if n, err := s.l.CountEpisodes(ctx, o, func() bool { return s.still(ctx) }); err != nil {
		s.fail("counting the episodes", err)
	} else if n > 0 {
		s.log.Info("episodes counted, from the newest week back", "weeks", n)
	}
	switch {
	case rejudge && pended && s.still(ctx):
		// The signals' change pended every firing, judged just now with
		// these settings: what was asked (a match changed, with named
		// signals) is done, once.
		judged()
		if s.l.Shared() {
			if err := s.l.SetMeta(ctx, "rejudged", fmt.Sprint(asked)); err != nil {
				s.fail("judging again", err)
			}
		}
	case rejudge && s.still(ctx):
		s.status.set("judging every firing again with the new settings")
		err := judgeAll(s.l, ctx, o, func(done float64) {
			s.status.Lock()
			s.status.judged = done
			s.status.Unlock()
		})
		s.status.Lock()
		s.status.judged = 0
		s.status.Unlock()
		if err != nil {
			s.rejudge.Store(true)
			s.fail("judging again", err)
		} else if judged(); s.l.Shared() {
			// What was asked is done: the next leader doesn't do it again.
			if err := s.l.SetMeta(ctx, "rejudged", fmt.Sprint(asked)); err != nil {
				s.fail("judging again", err)
			}
		}
	case rejudge:
		s.rejudge.Store(true)
	}
	if !s.still(ctx) {
		return
	}
	// The criterion in effect, read again over the last four weeks once
	// it is new; the settings read again after, for the record it keeps.
	if again, err := s.impactAgain(ctx, o); err != nil {
		s.fail("reading the impact signal again", err)
	} else if again {
		if o, err = s.options(ctx); err != nil {
			s.fail("settings", err)
			return
		}
	}
	if !s.still(ctx) {
		return
	}
	if s.cfg.Incidents != "" {
		err := s.syncIncidents(ctx, o)
		s.readFile(err)
		if err != nil {
			s.fail("incidents", err)
		} else {
			s.status.Lock()
			if s.status.errWhat == "incidents" {
				s.status.err, s.status.errWhat = "", ""
			}
			s.status.Unlock()
		}
	}
	if s.cfg.Deploys != "" {
		began := time.Now()
		err := s.syncDeploys(ctx, o)
		s.noteRead("deploys-file", began, -1, err)
		if err != nil {
			s.fail("deploys", err)
		} else {
			s.status.Lock()
			if s.status.errWhat == "deploys" {
				s.status.err, s.status.errWhat = "", ""
			}
			s.status.Unlock()
		}
	} else if kept, err := s.l.HasDeploys(ctx); err == nil && kept {
		// Kept from when --deploys was given: they stay as they were
		// marked, and the status says so until they are taken away.
		if s.deploysUnread.CompareAndSwap(false, true) {
			s.log.Warn(errDeploysUnread.Error())
		}
		s.status.Lock()
		s.status.errWhat, s.status.err = "deploys", errDeploysUnread.Error()
		s.status.Unlock()
	}
	// The marked file, as the deploys file; the expressions (--mark) read
	// again over the last four weeks once their key is new.
	if s.cfg.Marked != "" {
		began := time.Now()
		err := s.syncMarked(ctx, o)
		s.noteRead("marked-file", began, -1, err)
		if err != nil {
			s.fail("marked windows", err)
		} else {
			s.status.Lock()
			if s.status.errWhat == "marked windows" {
				s.status.err, s.status.errWhat = "", ""
			}
			s.status.Unlock()
		}
	}
	// The expressions no longer given let go, every one with no --mark.
	if err := s.forgetMarks(ctx); err != nil {
		s.fail("reading the marked windows' expressions again", err)
	}
	if len(s.cfg.Marks) > 0 && s.still(ctx) {
		if err := s.marksAgain(ctx, o); err != nil {
			s.fail("reading the marked windows' expressions again", err)
		}
	}
	if !s.still(ctx) {
		return
	}
	// With no rules to replay, there is nothing to run for but --impact:
	// what is copied in is judged as it comes.
	if run && s.running(ctx) {
		// The rules judged from Grafana's history, kept before the run,
		// which ties each to what is copied in for it.
		if s.replaying() {
			if err := s.noteFromHistory(ctx); err != nil {
				s.fail("copying in Grafana's alert state history", err)
			}
		}
		s.runs(ctx, o)
		// Where the data of the criterion the runs read begins, once they
		// read it: the record never starts before it.
		if err := s.impactData(ctx); err != nil {
			s.fail("looking for where the impact signal's data begins", err)
		}
		// What each named signal reads, by its match labels: which
		// alerts it could have seen.
		if err := s.signalsSeen(ctx, o); err != nil {
			s.fail("reading what the impact signals read", err)
		}
		// The firings of the alerts it ties otherwise counted again.
		if o.Signals.Named() {
			if err := s.judgePending(ctx, o); err != nil {
				s.fail("judging again", err)
			}
		}
		if err := s.keepBegan(ctx); err != nil {
			s.fail("a run", err)
		}
		// Not while the runs wait, or yielded to the prometheus API: it
		// asks the same API (docs/design/quota.md).
		if s.checking() && s.still(ctx) && !s.runsWait() {
			if err := s.checkReplay(ctx); err != nil {
				s.fail("setting the replay beside what really fired", err)
			}
		}
		// What Grafana's history says the rules Lapilli can't replay
		// fired, up to where the runs got.
		if s.still(ctx) {
			if err := s.copyFromHistory(ctx, o); err != nil {
				s.fail("copying in Grafana's alert state history", err)
			}
		}
	}
	if !s.still(ctx) {
		return
	}
	s.tellDue(ctx)
	// While the runs wait (--guard, --catch-up-hours, a 429 yielded to),
	// what reads the history waits with them, for a later round: reviews
	// of changes, the digest and what follows it, backtests again, and
	// changes carried out (docs/design/quota.md).
	waiting := s.runsWait()
	if s.replaying() && s.still(ctx) && !waiting {
		if err := s.reviewChanges(ctx, o); err != nil {
			s.fail("reviewing a change to the rules", err)
		}
	}
	if !s.still(ctx) {
		return
	}
	// The rules held to what is expected of them (docs/design/
	// expectations.md), before the digest, which counts what this finds.
	if err := s.expectAfter(ctx); err != nil {
		s.fail("checking the expectations", err)
	}
	if !s.still(ctx) {
		return
	}
	var fresh bool
	if !waiting {
		if fresh, err = s.digest(ctx, o); err != nil {
			s.fail("the weekly digest", err)
		}
	}
	if s.cfg.Seed != nil && !s.seeded.Load() && !waiting && s.still(ctx) {
		s.seeded.Store(s.cfg.Seed(ctx, s))
	}
	// After a digest worked out afresh, the next round: the digest holds the
	// runs up for --timeout and firedOnWithin at most, and the two together
	// not for twice --timeout.
	if s.replaying() && !fresh && s.still(ctx) && !waiting {
		if err := s.nearLater(ctx, o); err != nil {
			s.fail("near misses", err)
		}
		if s.still(ctx) {
			if err := s.proposeWeekly(ctx); err != nil {
				s.fail("proposing changes to the rules", err)
			}
		}
	}
	// Those that waited too long closed before any is backtested again, so
	// that no backtest is spent on one about to close. One someone
	// approved isn't closed: --approvals lowered, carryOut approves it.
	if s.still(ctx) {
		if err := s.expireProposals(ctx); err != nil {
			s.fail("closing the proposals that waited too long", err)
		}
	}
	// On a run alone: a write that pokes the work doesn't make Lapilli
	// replay twelve weeks again.
	if run && s.replaying() && s.still(ctx) && !waiting {
		if err := s.backtestStale(ctx); err != nil {
			s.fail("backtesting stale proposals again", err)
		}
	}
	if !s.still(ctx) {
		return
	}
	if !waiting {
		if err := s.carryOut(ctx); err != nil {
			s.fail("changing the rules", err)
		}
	}
	if !s.still(ctx) {
		return
	}
	if err := s.reports(ctx, o); err != nil {
		s.fail("a scheduled report", err)
	}
	if !s.still(ctx) {
		return
	}
	// The goals, once the round has written what it does: the last whole
	// week, what is told of it and the tasks it makes (docs/design/goals.md).
	if err := s.goalsAfter(ctx); err != nil {
		s.fail("working the goals out", err)
	}
	if !s.still(ctx) {
		return
	}
	if err := s.prune(ctx); err != nil {
		s.fail("pruning", err)
	}
}

// errDeploysUnread is a ledger keeping deploys with no --deploys to read.
var errDeploysUnread = errors.New("the ledger keeps deploys, but --deploys isn't given: their firings stay marked as they were. To take them away, give --deploys a file holding deploys: [] once")

func (s *Server) fail(what string, err error) {
	// Stopped, or no longer the leader's to do: lead says so once.
	if errors.Is(err, context.Canceled) || errors.Is(err, ledger.ErrNotLeading) {
		return
	}
	s.log.Error(what, "err", err)
	s.status.Lock()
	s.status.errWhat, s.status.err = what, err.Error()
	s.status.Unlock()
}

func (s *Server) syncIncidents(ctx context.Context, o verdict.Options) error {
	incs, err := incident.Load(s.cfg.Incidents)
	if err != nil {
		return err
	}
	changed, err := s.l.SyncFile(ctx, incs, o)
	s.incidentsRead.Store(true)
	var taken *ledger.TakenError
	if errors.As(err, &taken) {
		s.passedOver.Store(int64(len(taken.IDs)))
	} else if err == nil {
		s.passedOver.Store(0)
	}
	if changed {
		s.log.Info("incidents file read again", "incidents", len(incs))
	}
	return err
}

// runs replays from where the last run got to, less the hours to settle,
// up to now: a day at a time while catching up, and in smaller pieces when
// a day doesn't finish within the timeout.
func (s *Server) runs(ctx context.Context, o verdict.Options) {
	cursor, err := s.l.Cursor(ctx)
	if err != nil {
		s.fail("the ledger", err)
		return
	}
	if s.replaying() {
		// Nothing to replay until the rules have been read: the cursor
		// waits, so no hour is passed over.
		if _, ok, err := s.l.Latest(ctx); err != nil || !ok {
			if err == nil {
				err = errors.New("the rules haven't been read yet; runs wait for them")
			}
			s.fail("a run", err)
			return
		}
	}
	now := s.now().Truncate(s.cfg.Step)
	settle := s.cfg.Settle
	if cursor.IsZero() {
		cursor, settle = now.Add(-s.cfg.Backfill), 0
	}
	// Where the ledger's firings are up to: replaying again from further
	// back isn't a catch-up (docs/design/quota.md).
	ahead := cursor
	// A version of the rules recorded since the hours it was in effect were
	// replayed: replayed again from there.
	again, seq, err := s.l.AgainFrom(ctx)
	if err != nil {
		s.fail("the ledger", err)
		return
	}
	if !again.IsZero() && again.Before(cursor) {
		s.log.Info("replaying again from when a version of the rules recorded since took effect", "from", again)
		cursor = again
	}
	piece := 24 * time.Hour
	var looked time.Time // where the run was last looked at to wait
	s.holding.Store(false)
	s.heldFor.Store("")
	if s.yield != nil {
		s.yield.Begin(s.now())
	}
	for cursor.Before(now) && ctx.Err() == nil {
		s.stepped()
		from := cursor.Add(-settle).Truncate(s.cfg.Step)
		to := cursor.Add(piece)
		if to.After(now) {
			to = now
		}
		// Not so far that a rule a 429 left missed would be given up: at
		// cursor, the piece replays what was missed, and goes no further.
		if end, err := s.missedEnd(ctx, to); err != nil {
			s.fail("the ledger", err)
			return
		} else if to = end; !to.After(cursor) {
			to = cursor
		}
		stay := to.Equal(cursor)
		kind := "hourly"
		if to.Before(now) || settle == 0 {
			kind = "backfill"
		}
		// Before each run, and between the days of a catch-up, not again
		// for one tried over less: one that waits is kept, and the next
		// run tries again.
		if !cursor.Equal(looked) {
			looked = cursor
			if w := s.waits(ctx, cursor, ahead, now); w.why != "" || w.refused != nil {
				s.hold(ctx, kind, from, now, w)
				return
			}
		}
		s.status.set("replaying %s → %s", from.In(s.cfg.Loc).Format("2006-01-02 15:04"), to.In(s.cfg.Loc).Format("2006-01-02 15:04"))
		began := s.now()
		asking := s.asking(ctx, catchingUp(cursor, ahead, now))
		// The marked windows' expressions, as the impact signal: over the
		// run's period and the hour before, each with time of its own,
		// kept before the run is, so that it marks its own firings.
		if len(s.cfg.Marks) > 0 {
			if err := s.keepMarks(asking, s.readMarks(asking, s.cfg.Marks, from.Add(-time.Hour), to, false), o); err != nil {
				s.fail("keeping the marked windows' expressions", err)
			}
		}
		found, unfinished, err := s.find(asking, from, to)
		switch {
		case err != nil && prom.IsYielded(err):
			// The --impact or ALERTS query yielded: the run waits, as it
			// failed for nothing of its own.
			s.hold(ctx, kind, from, now, yieldedWait(err))
			return
		case unfinished && to.Sub(cursor) > s.cfg.Every:
			// Too much at once for the time given: half as much.
			piece = to.Sub(cursor) / 2
			s.log.Warn("a run didn't finish within --timeout; replaying less at once", "hours", piece.Hours())
			continue
		case err != nil:
			s.count(kind, "failed")
			s.fail("a run", err)
			s.l.AddRun(context.WithoutCancel(ctx), ledger.Run{Kind: kind, Started: began, Finished: s.now(), From: from, To: to, Error: err.Error()})
			return
		}
		found.Run = ledger.Run{Kind: kind, Started: began, Finished: s.now(), From: from, To: to, Failed: found.Run.Failed}
		if err := s.l.Record(ctx, found, o); err != nil {
			s.count(kind, "failed")
			s.fail("recording a run", err)
			return
		}
		s.count(kind, "ok")
		s.status.Lock()
		s.status.took = s.now().Sub(began)
		// What is wrong with the incidents or deploys file is for reading
		// it to clear.
		if s.status.errWhat != "incidents" && s.status.errWhat != "deploys" && s.status.errWhat != "marked windows" {
			s.status.err, s.status.errWhat = "", ""
		}
		s.status.Unlock()
		s.log.Info("run", "kind", kind, "from", from, "to", to, "firings", len(found.Firings), "failed", len(found.Run.Failed), "took", s.now().Sub(began).Round(time.Millisecond))
		if !again.IsZero() {
			// Unless a version was recorded since the run began: the next
			// run replays from there.
			if err := s.l.AgainDone(ctx, seq); err != nil {
				s.fail("the ledger", err)
			}
			again = time.Time{}
		}
		cursor, settle = to, s.cfg.Settle
		if !unfinished && piece < 24*time.Hour {
			piece *= 2
		}
		if ys := s.yielded(); len(ys) > 0 {
			s.log.Warn("the runs yielded to a history API that answered HTTP 429: they stop here, and the rules not replayed are replayed by a later run", "apis", strings.Join(ys, ","))
			return
		}
		if stay {
			s.log.Warn("the runs go no further until the rules a 429 left missed are replayed: one missed longer than a week would be given up", "to", to)
			return
		}
	}
}

func (s *Server) count(kind, result string) {
	s.status.Lock()
	s.status.runs[kind+" "+result]++
	s.status.Unlock()
}

// margins are how far before its period a rule's replay starts, so that an
// alert pending then is seen: its "for", keep_firing_for and group
// interval, rounded up to one of these, so that rules alike share their
// requests.
var margins = []time.Duration{15 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour, 7 * 24 * time.Hour}

// Margin is how far before its period a run's replay of r starts: its
// "for", keep_firing_for and group interval, rounded up to 15m, 1h, 6h,
// 1d or 7d.
func Margin(r rules.Rule, step time.Duration) time.Duration { return margin(r, step) }

func margin(r rules.Rule, step time.Duration) time.Duration {
	need := r.For + r.KeepFiringFor + r.Interval + step
	for _, m := range margins {
		if need <= m {
			return m
		}
	}
	return need
}

// find replays [from, to] and asks for the impact windows in it. It reports
// whether time ran out, for the run to be tried over less.
func (s *Server) find(parent context.Context, from, to time.Time) (ledger.Found, bool, error) {
	f := ledger.Found{From: from, To: to, Since: map[string]time.Time{}, Fresh: map[string]time.Time{},
		Keep: map[string]bool{}, Missed: map[string]time.Time{}}
	failed := map[string]string{}
	f.Run.Failed = failed
	unfinished := false
	// A query on its own that runs out of time: the run is tried over
	// less, or not recorded.
	slow := func(ctx context.Context, err error) (ledger.Found, bool, error) {
		if ctx.Err() == context.DeadlineExceeded {
			return f, true, errSlow
		}
		return f, false, err
	}
	// Impact first, with time of its own: one query, which a replay that
	// runs out of time shouldn't take along.
	n, set, err := s.signalsNowOf(parent)
	if err != nil {
		return f, false, err
	}
	c := s.criterionOf(set)
	if n.named() {
		if err := s.signalsRead(parent, from, to, &f, n); err != nil {
			return f, false, err
		}
	}
	if c.Expr != "" {
		ctx, cancel := context.WithTimeout(parent, s.cfg.Timeout)
		defer cancel()
		f.ImpactOn, f.ImpactSince, f.ImpactCriterion, f.ImpactAssumed = true, from.Add(-time.Hour), c.key(), s.assumedKey()
		series, err := s.h.QueryRange(ctx, c.Expr, f.ImpactSince, to, s.cfg.Step)
		s.readImpact(err, c.key())
		if err != nil {
			return slow(ctx, fmt.Errorf("--impact: %w", err))
		}
		if f.Impact, err = incident.FromImpact(ctx, series, s.cfg.Step, c.For); err != nil {
			return slow(ctx, fmt.Errorf("--impact: %w", err))
		}
	}
	ctx, cancel := context.WithTimeout(parent, s.cfg.Timeout)
	defer cancel()
	if s.cfg.Alerts {
		since := from.Add(-time.Hour)
		series, err := s.h.QueryRange(ctx, `ALERTS{alertstate="firing"}`, since, to, s.cfg.Step)
		if err != nil {
			return slow(ctx, fmt.Errorf("reading ALERTS: %w", err))
		}
		for _, x := range firing.FromAlerts(series, s.cfg.Step, to) {
			if s.cfg.RuleRE != nil && !s.cfg.RuleRE.MatchString(x.Rule) {
				continue
			}
			f.Firings = append(f.Firings, x)
			f.Since[x.Rule] = since
		}
		var names []string
		for _, x := range f.Firings {
			if !slices.Contains(names, x.Rule) {
				names = append(names, x.Rule)
				f.Rules = append(f.Rules, ledger.Rule{Name: x.Rule, Severity: x.Labels["severity"]})
			}
		}
		return f, false, nil
	}
	if !s.replaying() {
		return f, false, nil // --impact alone: no rules, whatever the ledger kept
	}

	// Each rule's replay starts early enough to see the pending time of
	// the first firing it records: from its From, or where a run before
	// missed it, less its margin. Rules that start alike are replayed
	// together and share their requests.
	missed, err := s.l.Missed(ctx)
	if err != nil {
		return f, false, err
	}
	versions, err := s.versions(ctx, from, to)
	if err != nil {
		return f, false, err
	}
	need, fresh := map[string]time.Duration{}, map[string]time.Duration{}
	for _, v := range versions {
		for _, r := range v.Rules {
			need[r.Name] = max(need[r.Name], margin(r, s.cfg.Step))
			fresh[r.Name] = max(fresh[r.Name], r.For+r.Interval+s.cfg.Step)
		}
	}
	groups := map[time.Time]map[string]bool{}
	for name, m := range need {
		start := from
		if at, ok := missed[name]; ok && at.Before(from) {
			start = at
			f.Missed[name] = at
		}
		since := start.Add(-m).Truncate(s.cfg.Step)
		if groups[since] == nil {
			groups[since] = map[string]bool{}
		}
		groups[since][name] = true
	}
	meta := map[string]*ledger.Rule{}
	sevs := severitiesBy{}
	var order []string
	// The latest first: the rules the run is for, then those catching up,
	// so one rule far behind doesn't keep the rest from their turn; with
	// --yield-on-429, the earliest first (replayOrder).
	for _, since := range slices.SortedFunc(maps.Keys(groups), s.replayOrder) {
		vs, err := s.versions(ctx, since, to)
		if err != nil && ctx.Err() == nil {
			return f, false, err
		}
		if ctx.Err() != nil {
			// Out of time before their turn: kept as they were, and
			// replayed from here by the next run.
			unfinished = true
			for name := range groups[since] {
				f.Keep[name] = true
				failed[name] = replay.Unfinished
			}
			continue
		}
		var set []replay.Version
		for _, v := range vs {
			rv := replay.Version{From: v.Since}
			if !v.Since.After(since) {
				rv.From = time.Time{}
			}
			for _, r := range v.Rules {
				if groups[since][r.Name] {
					rv.Rules = append(rv.Rules, r)
				}
			}
			set = append(set, rv)
		}
		ran, fs := replay.Run(ctx, s.h, set, since, to, s.cfg.Step, s.cfg.Parallel)
		fs, over := withinSeries(fs)
		for _, r := range ran {
			if over[r.Name] {
				r.Unsupported = fmt.Sprintf("query failed: the API answered for more than %d alerts at once, told apart by their labels, which no rule fires: the rule is left as it was", maxRuleSeries)
			}
			f.Since[r.Name] = since
			f.Fresh[r.Name] = since.Add(fresh[r.Name])
			x := meta[r.Name]
			if x == nil {
				x = &ledger.Rule{Name: r.Name, Group: r.Group}
				meta[r.Name] = x
				order = append(order, r.Name)
			}
			x.Severity = sevs.add(r.Name, r.Labels["severity"])
			if r.Unsupported != "" && x.Unsupported == "" {
				// Kept without how often a request was asked, which
				// differs from run to run; the run's record says it.
				x.Unsupported = prom.Steady(r.Unsupported)
			}
			switch {
			case r.Unsupported == replay.Unfinished:
				unfinished = true
				f.Keep[r.Name] = true
				failed[r.Name] = r.Unsupported
			case rules.Unanswered(r.Unsupported):
				f.Keep[r.Name] = true
				failed[r.Name] = r.Unsupported
			}
		}
		f.Firings = append(f.Firings, fs...)
	}
	slices.Sort(order)
	for _, name := range order {
		f.Rules = append(f.Rules, *meta[name])
	}
	return f, unfinished, nil
}

// maxRuleSeries is the most alerts, told apart by their labels, one rule's
// replay in a run is kept with: what a replay finds is an API's answer,
// and each label set is kept in the ledger for good. Past it, the rule is
// left as it was, as one whose query failed. Replaced in tests.
var maxRuleSeries = 20000

// withinSeries are the firings of rules of maxRuleSeries label sets at
// most, and the rules past it.
func withinSeries(fs []firing.Firing) ([]firing.Firing, map[string]bool) {
	sets := map[string]map[uint64]bool{}
	over := map[string]bool{}
	for _, x := range fs {
		if over[x.Rule] {
			continue
		}
		if sets[x.Rule] == nil {
			sets[x.Rule] = map[uint64]bool{}
		}
		sets[x.Rule][labelsHash(seriesSeed, x.Labels)] = true
		if len(sets[x.Rule]) > maxRuleSeries {
			over[x.Rule] = true
			delete(sets, x.Rule)
		}
	}
	if len(over) == 0 {
		return fs, nil
	}
	return slices.DeleteFunc(fs, func(x firing.Firing) bool { return over[x.Rule] }), over
}

// seriesSeed is the seed of labelsHash, as the process starts: what label
// sets hash alike can't be known before, and two made to can't be told.
var seriesSeed = maphash.MakeSeed()

// labelsHash is a label set's hash under seed, the same for the same set:
// each name and value written after its length, so that no value,
// whatever it holds, reads as the labels after it. Seeded, as an API's
// answer is outside input: a hostile one making label sets that hash
// alike (2^32 tries a pair of FNV-64a's) would be counted as fewer
// alerts than it is.
func labelsHash(seed maphash.Seed, m map[string]string) uint64 {
	var h maphash.Hash
	h.SetSeed(seed)
	var n [binary.MaxVarintLen64]byte
	field := func(s string) {
		h.Write(n[:binary.PutUvarint(n[:], uint64(len(s)))])
		h.WriteString(s)
	}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		field(k)
		field(m[k])
	}
	return h.Sum64()
}

// errSlow is a query on its own that didn't finish within --timeout.
var errSlow = errors.New("a query didn't finish within --timeout")

// version is a version of the rules, read again from its documents.
type version struct {
	ledger.Version
	Rules []rules.Rule
}

// parsed keeps the versions read lately: a version doesn't change. By the
// ledger (its path, or the file naming its Postgres database: every one of
// those has a version 1), the version's id and its digest.
var parsed = struct {
	sync.Mutex
	m map[string][]rules.Rule
}{m: map[string][]rules.Rule{}}

// versions are the versions of the rules in effect over [from, to], each
// rule under the name it is judged under.
func (s *Server) versions(ctx context.Context, from, to time.Time) ([]version, error) {
	vs, err := s.l.Versions(ctx, from, to, false)
	if err != nil {
		return nil, err
	}
	out := make([]version, 0, len(vs))
	for _, v := range vs {
		rs, err := s.parse(ctx, v)
		if err != nil {
			return nil, err
		}
		out = append(out, version{v, rs})
	}
	return out, nil
}

func (s *Server) parse(ctx context.Context, v ledger.Version) ([]rules.Rule, error) {
	as, scrapes := s.replayedAs(), s.scrapes()
	k := fmt.Sprint(s.cfg.Ledger, "\x00", s.cfg.LedgerURLFile, "\x00", v.ID, "\x00", v.Digest, "\x00", as.DataSources, "\x00", scrapes.String(), "\x00", as.Kinds, "\x00", as.Places, as.NoLoki, as.NoGrafanaLoki,
		"\x00", as.Listing.Tag(), "\x00", as.LokiListing.Tag())
	parsed.Lock()
	rs, ok := parsed.m[k]
	parsed.Unlock()
	if ok {
		return rs, nil
	}
	all, names, err := s.named(ctx, v.ID)
	if err != nil {
		return nil, err
	}
	for j, r := range all {
		if names[j] != "" {
			r.Name = names[j]
			rs = append(rs, r)
		}
	}
	as.Apply(rs)
	s.sayUnplaced(as.Unplaced)
	parsed.Lock()
	if len(parsed.m) >= 128 {
		for k := range parsed.m { // one out, any one: most are in use
			delete(parsed.m, k)
			break
		}
	}
	parsed.m[k] = rs
	parsed.Unlock()
	return rs, nil
}

// replayedAs is how rules are replayed: --late, and --scrape-interval, or
// else Prometheus's own intervals, for Grafana's reduces; and the scrape
// intervals --grafana's data sources say they have.
func (s *Server) replayedAs() rules.Replaying {
	s.dataSources.Lock()
	ds, kinds := s.dataSources.ds, s.dataSources.kinds
	s.dataSources.Unlock()
	s.places.Lock()
	places := s.places.m
	s.places.Unlock()
	p := rules.Replaying{Late: s.cfg.Late, Scrape: s.cfg.ScrapeInterval, DataSources: ds, Kinds: kinds, Places: places,
		NoLoki: s.cfg.Loki == "", NoGrafanaLoki: s.cfg.Loki == "" && s.cfg.Grafana == ""}
	s.listing.Lock()
	p.Listing, p.LokiListing, p.Unplaced = s.listing.prom, s.listing.loki, s.listing.unplaced
	s.listing.Unlock()
	if sc := s.scrapes(); s.cfg.ScrapeInterval == 0 && sc.Global > 0 {
		p.ScrapeOf = sc.Shortest
	}
	return p
}

// readDataSources asks --grafana for each data source the rules' queries
// name: its own scrape interval, which $__rate_interval is put in with,
// and its type, which says whether its queries are PromQL or LogQL; and
// keeps what it says. What can't be read is logged: its $__rate_interval
// is put in as --scrape-interval says, or as Grafana's 15s, and its
// queries taken as PromQL, unless they say otherwise.
func (s *Server) readDataSources(ctx context.Context, rs []rules.Rule) {
	var uids []string
	for _, r := range rs {
		for _, u := range r.GrafanaUIDs() {
			if !slices.Contains(uids, u) {
				uids = append(uids, u)
			}
		}
	}
	if len(uids) == 0 {
		return
	}
	slices.Sort(uids)
	s.dataSources.Lock()
	fresh := slices.Equal(s.dataSources.uids, uids) && s.now().Sub(s.dataSources.at) < time.Hour
	s.dataSources.Unlock()
	if fresh {
		return
	}
	ds, kinds, missed, err := source.GrafanaDataSources(ctx, s.cfg.Grafana, s.cfg.GrafanaToken, "lapilli/"+s.cfg.Version, uids)
	if err != nil {
		s.log.Warn("Grafana's data sources weren't read: their $__rate_interval and types are taken as before", "err", err)
		return
	}
	if len(missed) > 0 {
		s.log.Warn("Grafana's data sources weren't read: their $__rate_interval is put in with --scrape-interval, or Grafana's 15s, and their queries taken as PromQL", "data_sources", strings.Join(missed, ", "))
	}
	s.dataSources.Lock()
	s.dataSources.ds, s.dataSources.kinds, s.dataSources.uids, s.dataSources.at = ds, kinds, uids, s.now()
	s.dataSources.Unlock()
}

// namedReads counts the versions named reads, each a parse of every rule
// in it, for tests.
var namedReads atomic.Int64

// named reads a version's rules again, as they were read, and the name
// each is judged under ("" for one --rule leaves out). Should a newer
// lapilli read the documents into other rules than the names were given
// to, the rules are named afresh.
func (s *Server) named(ctx context.Context, id int64) ([]rules.Rule, []string, error) {
	return s.namedRec(ctx, id, nil)
}

// namedRec is named, adding to rec, when not nil, the version's recording
// rules.
func (s *Server) namedRec(ctx context.Context, id int64, rec *[]rules.Recording) ([]rules.Rule, []string, error) {
	all, names, _, err := s.namedDocs(ctx, id, rec)
	return all, names, err
}

// namedDocs is namedRec, with the version's documents the rules were read
// from.
func (s *Server) namedDocs(ctx context.Context, id int64, rec *[]rules.Recording) ([]rules.Rule, []string, []rules.Doc, error) {
	namedReads.Add(1)
	full, err := s.l.VersionByID(ctx, id)
	if err != nil {
		return nil, nil, nil, err
	}
	all, err := rules.FromDocsRecorded(context.Background(), full.Docs, rec)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(all) == len(full.Names) {
		return all, full.Names, full.Docs, nil
	}
	s.log.Warn("a version of the rules reads differently now; naming its rules afresh", "version", id)
	names := make([]string, len(all))
	var keep []rules.Rule
	var at []int
	for i, r := range all {
		if s.cfg.RuleRE == nil || s.cfg.RuleRE.MatchString(r.Name) {
			keep = append(keep, r)
			at = append(at, i)
		}
	}
	source.TellApart(keep)
	for j, i := range at {
		names[i] = keep[j].Name
	}
	return all, names, full.Docs, nil
}

// watch reads the rules every --watch and records a new version when
// anything that decides when they fire has changed. raw is what the
// documents read last hashed to.
func (s *Server) watch(ctx context.Context, raw string) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.cfg.Watch):
		}
		if err := s.look(ctx, &raw); err != nil {
			s.fail("reading the rules", err)
		}
		// A source of rules down, or back, told of as it is read, not
		// at the next run.
		if err := s.tellSources(ctx); err != nil {
			s.fail("notifying", err)
		}
	}
}

// identity is what a rule keeps its name by from one version to the next.
func identity(r rules.Rule) string {
	id := fmt.Sprintf("%q %q %q %q", r.Group, r.Name, sse.LabelKey(r.Labels), r.UID)
	if r.Namespace != "" {
		id += fmt.Sprintf(" %q", r.Namespace)
	}
	// A Loki ruler's rule is another than a Mimir rule of the same
	// namespace, group, name and labels; every other rule's identity is
	// as it was.
	if r.Logs != "" && r.Graph == nil {
		id += fmt.Sprintf(" logs %q", r.Logs)
	}
	return id
}

func (s *Server) look(ctx context.Context, raw *string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	// Following a history, the versions added to it are recorded from their
	// own times first: the rules read now are the newest's.
	if s.following() {
		if err := s.follow(ctx); err != nil {
			return err
		}
	}
	rs, docs, err := s.readRules(ctx)
	if err != nil {
		return err
	}
	s.askScrape(ctx)
	s.askNotify(ctx)
	s.askAlertmanager(ctx)
	s.askJobs(ctx, s.matching(rs))
	sum := docsSum(docs)
	if sum == *raw {
		return nil
	}
	s.said(s.matching(rs))
	if err := s.seenSays(ctx, rs); err != nil {
		s.fail("changing the rules", err)
	}
	id, had, err := s.version(ctx, rs, docs, s.now().Truncate(time.Second))
	if err != nil {
		return err
	}
	*raw = sum
	if id == 0 {
		return nil
	}
	s.log.Info("rules changed: a new version", "version", id)
	if err := s.seen(ctx, id); err != nil {
		s.fail("changing the rules", err)
	}
	if had {
		s.poke() // to review it
	}
	return nil
}

// docsSum is what documents of rules hash to, to tell whether they changed.
func docsSum(docs []rules.Doc) string {
	h := sha256.New()
	for _, d := range docs {
		fmt.Fprintf(h, "%s\x00%v\x00%s\x00%s\x00%d\x00", d.Source, d.API, d.Namespace, d.Logs, len(d.Data))
		h.Write(d.Data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// version records rules as a version in effect from since, the first a
// ledger has from before it began, unless they decide when alerts fire as
// the latest version's do. A rule keeps the name it had in the version
// before; a new one is named apart from the rules alongside it and those
// before. It gives the version's id, 0 for none recorded, and whether the
// ledger had a version before.
func (s *Server) version(ctx context.Context, rs []rules.Rule, docs []rules.Doc, since time.Time) (int64, bool, error) {
	var keep []rules.Rule
	var at []int
	for i, r := range rs {
		if s.cfg.RuleRE == nil || s.cfg.RuleRE.MatchString(r.Name) {
			keep = append(keep, r)
			at = append(at, i)
		}
	}
	if len(keep) == 0 {
		return 0, false, errors.New("no alert rules to replay")
	}
	digest := digestOf(keep)
	latest, ok, err := s.l.Latest(ctx)
	if err != nil {
		return 0, false, err
	}
	if ok && latest.Digest == digest && s.wholeAsLatest(ctx, latest, docs) {
		return 0, true, nil
	}
	had := map[string]string{}
	var was []rules.Rule
	if ok {
		all, names, err := s.named(ctx, latest.ID)
		if err != nil {
			return 0, true, err
		}
		for i, r := range all {
			if names[i] != "" {
				had[identity(r)] = names[i]
				was = append(was, r)
			}
		}
	}
	plain := slices.Clone(keep)
	source.TellApart(keep, was)
	names := make([]string, len(rs))
	for j, i := range at {
		names[i] = keep[j].Name
		if n, ok := had[identity(plain[j])]; ok {
			names[i] = n
		}
	}
	if !ok {
		since = time.Time{} // the rules as they stood before the ledger began
	}
	id, err := s.l.AddVersion(ctx, ledger.Version{Since: since, Digest: digest, Docs: docs, Names: names})
	return id, ok, err
}

// history adds the versions --rules-history holds to a ledger that has no
// version yet, before its first run: the one in effect when the backfill
// starts, from before the ledger began, and each later one from its time.
// A ledger with versions of its own keeps them; the directory's are passed
// over, and the log says so. A version that can't be read (no rules, none
// --rules-select keeps, a file that doesn't parse) is passed over, saying
// so, and the one before it stays in effect: one bad commit doesn't hold
// the start up, and a pod restarted doesn't loop on it.
func (s *Server) history(ctx context.Context) error {
	if s.cfg.RulesHistory == "" {
		return nil
	}
	// A ledger with versions of its own needs no history, and isn't held
	// up by one that can't be read now: a restart while git can't be
	// reached starts as it would without.
	if _, ok, err := s.l.Latest(ctx); err != nil {
		return err
	} else if ok && s.following() {
		return nil // what was added since, the watcher records
	} else if ok {
		s.log.Info("--rules-history: the ledger has versions of the rules of its own, which it keeps; the directory is passed over",
			"dir", s.cfg.RulesHistory)
		return nil
	}
	hs, err := historyVersions(s.cfg.RulesHistory)
	if errors.Is(err, rules.ErrNoVersions) {
		s.log.Warn("--rules-history: the directory holds no versions, as when they couldn't be fetched: the backfill replays today's rules",
			"dir", s.cfg.RulesHistory)
		return nil
	}
	if err != nil && s.following() {
		// Read again every --watch, which says so until it is put right.
		s.log.Error("--rules-history", "err", err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("--rules-history: %w", err)
	}
	now := s.now()
	// A version from after now is a commit dated ahead of the clock, which
	// its author may set as they like: passed over, not believed.
	if future := slices.DeleteFunc(slices.Clone(hs), func(h rules.Then) bool { return !h.At.After(now) }); len(future) > 0 {
		s.log.Warn("--rules-history: versions from after now are passed over", "dir", s.cfg.RulesHistory, "first", future[0].Path, "versions", len(future))
		hs = slices.DeleteFunc(hs, func(h rules.Then) bool { return h.At.After(now) })
		if len(hs) == 0 {
			return nil
		}
	}
	start := now.Add(-s.cfg.Backfill)
	first := 0
	for i, h := range hs {
		if !h.At.After(start) {
			first = i
		}
	}
	xrs, xdocs := s.extrasNow(ctx)
	// Every version read before any is kept, so a start cut short leaves
	// the ledger as it was. The one in effect when the backfill starts is
	// the latest that reads at or before then.
	var reads []historyVersion
	var picked rules.Picked
	for i := first; i >= 0 && len(reads) == 0; i-- {
		if v, ok := s.readVersion(hs[i], xrs, xdocs, &picked); ok {
			reads = append(reads, v)
		}
	}
	for _, h := range hs[first+1:] {
		if v, ok := s.readVersion(h, xrs, xdocs, &picked); ok {
			reads = append(reads, v)
		}
	}
	if len(reads) == 0 {
		s.log.Warn("--rules-history: no version reads: the backfill replays today's rules", "dir", s.cfg.RulesHistory)
		return nil
	}
	if reads[0].at.After(start) {
		s.log.Warn("--rules-history: no version is from before the backfill starts; the earliest is taken as the rules from before then",
			"backfill", start, "earliest", reads[0].at)
	}
	added := 0
	for _, v := range reads {
		id, _, err := s.version(ctx, v.rules, v.docs, v.at)
		if err != nil {
			return fmt.Errorf("--rules-history: %s: %w", v.path, err)
		}
		if id != 0 {
			added++
		}
	}
	// Followed from the newest, read or not: one that doesn't read isn't
	// read again each --watch.
	if err := s.l.SetMeta(ctx, "followed", strconv.FormatInt(hs[len(hs)-1].At.Unix(), 10)); err != nil {
		return err
	}
	s.log.Info("--rules-history: versions of the rules from before the ledger began", "dir", s.cfg.RulesHistory, "versions", added)
	if said := picked.String(); said != "" {
		s.log.Info("--rules-history: the versions held "+said+": passed over", "versions", len(hs[first:]))
	}
	// The rules read now should be the newest version's; if they aren't, they
	// are taken as changed now, and the directory may be stale.
	if rs, _, err := s.readRules(ctx); err == nil {
		if latest, ok, err := s.l.Latest(ctx); err == nil && ok && latest.Digest != digestOf(s.matching(rs)) {
			s.log.Warn("--rules-history: the rules read now differ from the newest version in the directory; they are taken as changed now",
				"newest", reads[len(reads)-1].path)
		}
	}
	return nil
}

// sayPicked logs what reading the rule files passed over, when it isn't
// what was said last: they are read every --watch.
func (s *Server) sayPicked(what string, p rules.Picked) {
	said := p.String()
	if was, _ := s.picked.Load().(string); said == was {
		return
	}
	s.picked.Store(said)
	if said != "" {
		s.log.Info(what + ": the rule files hold " + said + ": passed over")
	}
}

// matching are the rules --rule-re keeps.
func (s *Server) matching(rs []rules.Rule) []rules.Rule {
	var keep []rules.Rule
	for _, r := range rs {
		if s.cfg.RuleRE == nil || s.cfg.RuleRE.MatchString(r.Name) {
			keep = append(keep, r)
		}
	}
	return keep
}

// poke asks the worker to look at its jobs now, if it isn't already asked:
// this replica's, and the leader's through the ledger, wherever it runs.
func (s *Server) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
	if s.l.Shared() && !s.l.Leading() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.l.Wake(ctx); err != nil {
			s.log.Warn("waking the replica that leads; it looks at its jobs at its next round", "err", err)
		}
	}
}

// digestOf is the same for two sets of rules that fire alike.
func digestOf(rs []rules.Rule) string { return sumOf(rs, rules.Rule.Key) }

// sumOf is what rules hash to by key, in any order.
func sumOf(rs []rules.Rule, key func(rules.Rule) string) string {
	keys := make([]string, len(rs))
	for i, r := range rs {
		keys[i] = key(r)
	}
	slices.Sort(keys)
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return hex.EncodeToString(sum[:])
}

// wholeAsLatest reports whether documents of rules whose digest is the
// latest version's hold its rules as a whole too. A version's digest is of
// its rules' keys, as it was before Lapilli read a group's labels and
// limit, which a key leaves out (rules.Rule.Whole): both versions'
// documents are read again to tell, by this Lapilli and alike, so a ledger
// written by one before reads as the rules do, and a change to a group's
// labels or limit alone is a new version. Documents that can't be read
// again are taken as changed.
func (s *Server) wholeAsLatest(ctx context.Context, latest ledger.Version, docs []rules.Doc) bool {
	full, err := s.l.VersionByID(ctx, latest.ID)
	if err == nil {
		var was, is []rules.Rule
		if was, err = rules.FromDocs(full.Docs); err == nil {
			if is, err = rules.FromDocs(docs); err == nil {
				return sumOf(s.matching(was), rules.Rule.Whole) == sumOf(s.matching(is), rules.Rule.Whole)
			}
		}
	}
	s.log.Warn("the rules' documents can't be read again; taken as changed", "version", latest.ID, "err", err)
	return false
}

// rulesReadTime is how long reading the rules may take, wherever they are
// kept, whoever reads them: the minute look gives, and the same for a
// proposal's fresh read, so that a Grafana or a ruler that takes the
// request and never answers can't hold the work loop. Replaced in tests.
var rulesReadTime = time.Minute

func (s *Server) readRules(ctx context.Context) ([]rules.Rule, []rules.Doc, error) {
	ctx, cancel := context.WithTimeout(ctx, rulesReadTime)
	defer cancel()
	agent := "lapilli/" + s.cfg.Version
	var rs []rules.Rule
	var docs []rules.Doc
	var err error
	// How the read went, for the Sources page and its notices
	// (sourcelist.go): the rules where they are kept, then each source
	// read beside them.
	began, primary := time.Now(), s.cfg.Loaded || s.cfg.Grafana != "" || s.ruler != nil || s.following() || s.cfg.RulesPath != ""
	switch {
	case s.cfg.Loaded:
		var d rules.Doc
		rs, d, err = source.Loaded(ctx, s.h)
		docs = []rules.Doc{d}
	case s.cfg.Grafana != "":
		var d rules.Doc
		rs, d, err = source.Grafana(ctx, s.cfg.Grafana, s.cfg.GrafanaToken, agent, nil)
		docs = []rules.Doc{d}
		if err == nil {
			s.readDataSources(ctx, rs)
		}
	case s.ruler != nil:
		rs, docs, err = s.ruler.Read(ctx)
	case s.following():
		rs, docs, err = s.newest(ctx)
	case s.cfg.RulesPath != "":
		var picked rules.Picked
		rs, docs, picked, err = rules.ReadPicked(s.cfg.RulesPath, rules.Picking{Select: s.cfg.RulesSelect})
		s.sayPicked("--rules", picked)
	}
	if primary {
		s.noteRead("rules", began, len(rs), err)
	}
	s.readListings(ctx)
	// What is read beside them is read, and noted, whatever the rules'
	// read came to: one left unread because another failed would age into
	// quiet on the Sources page, and be told down.
	r, d, xerr := s.extras(ctx)
	if err == nil {
		err = xerr
	}
	if err != nil {
		return nil, nil, err
	}
	return append(rs, r...), append(docs, d...), nil
}

// readListings asks the history's rules API, and --loki's, for the groups
// they list, when rules are read from rule files, to place those groups
// where the servers run the groups of their names (rules.Listing). Each is
// asked once a read, as a read lapilli can do without (not waited for, nor
// held to the API's pushing back): when it can't be read, the groups are
// placed by what was last read, or on the steps' grid, and it is said once.
func (s *Server) readListings(ctx context.Context) {
	agent := "lapilli/" + s.cfg.Version
	ownRules := !s.cfg.Loaded && s.cfg.Grafana == "" && s.ruler == nil && !s.following()
	if ownRules && s.cfg.RulesPath != "" && s.h != nil {
		l, err := source.Listed(ctx, s.h, true)
		s.setListing(&s.listing.prom, &s.listing.promBad, "the history's rules API", l, err)
	}
	if s.cfg.LokiRules != "" && !s.cfg.LokiLoaded && s.cfg.Loki != "" {
		l, err := source.LokiListed(ctx, source.Loki{URL: s.cfg.Loki, TokenFile: s.cfg.LokiToken, Tenant: s.cfg.LokiTenant, Limit: s.cfg.LokiLimit}, agent, true)
		s.setListing(&s.listing.loki, &s.listing.lokiBad, "--loki's rules API", l, err)
	}
}

func (s *Server) setListing(at **rules.Listing, bad *bool, of string, l *rules.Listing, err error) {
	s.listing.Lock()
	defer s.listing.Unlock()
	switch {
	case errors.Is(err, source.ErrNoRulesAPI):
	case err != nil:
		if !*bad {
			s.log.Warn(of+" couldn't be read: the groups read from rule files are placed as last read, or on the steps' grid", "err", err)
		}
		*bad = true
	default:
		*bad = false
		if l.Tag() != (*at).Tag() {
			// Another listing places differently: what was collected of
			// the last is of its own.
			s.listing.unplaced = &rules.Unplaced{}
		}
		*at = l
	}
	if s.listing.unplaced == nil {
		s.listing.unplaced = &rules.Unplaced{}
	}
}

// sayUnplaced logs, when it changes, which groups read from rule files
// couldn't be placed by the rules API, and why.
func (s *Server) sayUnplaced(u *rules.Unplaced) {
	said := strings.Join(u.Said("the history's rules API"), "; ")
	s.listing.Lock()
	changed := said != s.listing.said
	s.listing.said = said
	s.listing.Unlock()
	if changed && said != "" {
		s.log.Info("groups read from rule files, placed by the rules API", "said", said)
	}
}

// extras are the rules read beside the rules: a cloud's own alarms, and a
// Loki ruler's. Each is read, and its read noted, whichever of the others
// failed, and a read that fails anywhere fails the whole, so the version
// stays as it was. One the time given ran out before is held (noteHeld):
// not read, and not aging meanwhile.
func (s *Server) extras(ctx context.Context) ([]rules.Rule, []rules.Doc, error) {
	agent := "lapilli/" + s.cfg.Version
	var rs []rules.Rule
	var docs []rules.Doc
	var failed error
	// read reports whether the source id is read: false, held, once the
	// time is out.
	read := func(id string) bool {
		if ctx.Err() != nil {
			s.noteHeld(id)
			failed = cmp.Or(failed, ctx.Err())
			return false
		}
		return true
	}
	for _, a := range s.alarms {
		if !read("alarms/" + a.Source) {
			continue
		}
		began := time.Now()
		d, err := a.Docs(ctx, s.alarmsEvery())
		var r []rules.Rule
		if err == nil {
			r, err = rules.FromDocs(d)
		}
		s.noteRead("alarms/"+a.Source, began, len(r), err)
		if err != nil {
			failed = cmp.Or(failed, err)
			continue
		}
		rs, docs = append(rs, r...), append(docs, d...)
	}
	// A Loki's ruler's rules, beside them, the same way.
	l := source.Loki{URL: s.cfg.Loki, TokenFile: s.cfg.LokiToken, Tenant: s.cfg.LokiTenant, Limit: s.cfg.LokiLimit}
	began := time.Now()
	switch {
	case (s.cfg.LokiLoaded || s.cfg.LokiRules != "") && !read("loki"):
		// Held: the time ran out on another.
	case s.cfg.LokiLoaded:
		r, d, err := source.LokiRules(ctx, l, agent, func(what string) { s.log.Warn(what) })
		s.noteRead("loki", began, len(r), err)
		if err != nil {
			return nil, nil, err
		}
		// Where its ruler runs each group is read between the replays'
		// queries, which a Loki pushing back holds: not waited for, and
		// kept as read before when it can't be, so the rules, and those of
		// every other source, are read all the same.
		places, err := source.LokiPlacesOnce(ctx, l, agent)
		s.places.Lock()
		if err != nil {
			if !s.places.failing {
				s.log.Warn("where --loki's ruler runs its groups couldn't be read: kept as last read", "err", err)
			}
			places, s.places.failing = s.places.m, true
		} else {
			s.places.m, s.places.failing = places, false
		}
		s.places.Unlock()
		source.Placed(r, places)
		rs, docs = append(rs, r...), append(docs, d...)
	case s.cfg.LokiRules != "":
		r, d, notes, err := source.LokiFiles(s.cfg.LokiRules)
		s.noteRead("loki", began, len(r), err)
		if err != nil {
			return nil, nil, err
		}
		// Said once, not each minute the files are read again.
		s.places.Lock()
		if said := strings.Join(notes, "\n"); said != s.places.notes {
			s.places.notes = said
			for _, n := range notes {
				s.log.Warn(n)
			}
		}
		s.places.Unlock()
		rs, docs = append(rs, r...), append(docs, d...)
	}
	if failed != nil {
		return nil, nil, failed
	}
	return rs, docs, nil
}

// reviewChanges reviews each version of the rules recorded since the last
// one reviewed: kept in the ledger, so a restart doesn't drop one.
func (s *Server) reviewChanges(ctx context.Context, o verdict.Options) error {
	done, err := s.l.Meta(ctx, "reviewed")
	if err != nil {
		return err
	}
	last, _ := strconv.ParseInt(done, 10, 64)
	vs, err := s.l.Versions(ctx, time.Unix(0, 0), s.now(), false)
	if err != nil {
		return err
	}
	for i, v := range vs {
		if v.ID <= last {
			continue
		}
		if i > 0 && last > 0 {
			if err := s.reviewChange(ctx, vs[i-1], v, o); err != nil {
				return err
			}
		}
		if err := s.l.SetMeta(ctx, "reviewed", fmt.Sprint(v.ID)); err != nil {
			return err
		}
		last = v.ID
	}
	return nil
}

// reviewChange replays the rules a version changed, as they were and as
// they are, over the week before it, and keeps the comparison.
func (s *Server) reviewChange(ctx context.Context, before, after ledger.Version, o verdict.Options) error {
	was, err := s.parse(ctx, before)
	if err != nil {
		return err
	}
	is, err := s.parse(ctx, after)
	if err != nil {
		return err
	}
	_, gone, came := source.Split(was, is)
	if len(gone)+len(came) == 0 {
		return nil
	}
	s.status.set("reviewing a change to the rules")
	at := after.Since
	to := at.Truncate(s.cfg.Step)
	from := to.Add(-7 * 24 * time.Hour)
	cmp, err := s.replayChange(ctx, gone, came, len(was)-len(gone), from, to, s.cfg.Timeout, o)
	if err != nil {
		return err
	}
	if err := s.l.AddResult(ctx, "review", at, from, to, cmp); err != nil {
		return err
	}
	s.log.Info("change reviewed", "version", after.ID, "rules", len(cmp.Rules))
	return s.l.AddRun(ctx, ledger.Run{Kind: "review", Started: s.now(), Finished: s.now(), From: from, To: to})
}

// replayChange replays the rules a change took away and those it brought
// over [from, to), within timeout, and sets them side by side, judged
// against the ledger's incidents; same is how many rules it left alike.
func (s *Server) replayChange(ctx context.Context, gone, came []rules.Rule, same int, from, to time.Time, timeout time.Duration, o verdict.Options) (*verdict.Comparison, error) {
	cmp, err := s.replayCompared(ctx, s.h, s.cfg.Parallel, gone, came, same, from, to, timeout, o)
	if err != nil {
		return nil, err
	}
	cmp.Before.Firings, cmp.After.Firings = nil, nil
	return cmp, nil
}

// replayCompared is replayChange against h, parallel rules at once, its
// firings kept: a pull request's backtest's, in the lane (backtestResult),
// counts them.
func (s *Server) replayCompared(ctx context.Context, h replay.History, parallel int, gone, came []rules.Rule, same int, from, to time.Time, timeout time.Duration, o verdict.Options) (*verdict.Comparison, error) {
	rctx, cancel := context.WithTimeout(s.askedFor(ctx), timeout)
	defer cancel()
	ran, fired := replay.RunEach(rctx, h, [][]replay.Version{{{Rules: gone}}, {{Rules: came}}}, from, to, s.cfg.Step, parallel)
	incs, err := s.l.Incidents(ctx, from, to, false)
	if err != nil {
		return nil, err
	}
	plain, err := s.recordAround(ctx, from, to, o) // the record kept, as the ledger's
	if err != nil {
		return nil, err
	}
	for _, in := range incs {
		plain = append(plain, in.Incident)
	}
	o.From, o.To, o.SomeRules = from, to, true
	// People's verdicts, on both sides alike (docs/design/judging.md,
	// "Through judging again"): a change mustn't look better on the
	// firings people called coincidences.
	if o.Verdicts, err = s.l.ReplayVerdicts(ctx, from, to); err != nil {
		return nil, err
	}
	var changed []string
	for _, r := range slices.Concat(gone, came) {
		changed = append(changed, r.Name)
	}
	cmp := verdict.Compare(verdict.Judge(ran[0], fired[0], plain, o), verdict.Judge(ran[1], fired[1], plain, o), changed, same)
	cmp.ChangedOnly = true
	cmp.LabelRules(gone, came)
	return cmp, nil
}

// wholeAfter is how far past a span's end the run cursor must be before
// what is worked out once the span is whole is worked out of it: the
// digest, goal.missed, the goals' tasks and their /metrics, for a week and
// for an occurrence of each: KIND (docs/design/judging.md, "--settle, a
// signal's for, a rule's for"). It is the most, over each named signal and
// the rules judged against it, of the lead and the signal's "for" (of the
// signal with no name, the lead in effect and --impact-for; of incidents
// written down alone, the lead), and --settle after that: every window a
// firing of the span can catch has been found, and a run has read again
// the samples that came late, so that no stone of the span turns after it
// was told. set are the settings in effect, for the signal with no name.
func (s *Server) wholeAfter(o verdict.Options, set map[string]ledger.Setting) time.Duration {
	reach, named := o.Reach()
	switch c := s.criterionOf(set); {
	case named:
	case c.Expr != "":
		reach = o.Lead + c.For
	default:
		reach = o.Lead
	}
	return reach + s.cfg.Settle
}

// wholeAfterNow is wholeAfter of the settings as they are.
func (s *Server) wholeAfterNow(ctx context.Context, o verdict.Options) (time.Duration, error) {
	set, err := s.l.Settings(ctx)
	if err != nil {
		return 0, err
	}
	return s.wholeAfter(o, set), nil
}

// digest judges the week before, suggests longer "for"s for its noisy
// rules, and keeps them, once a week at --digest, or as soon as it can when
// that time was missed; then tells of it and posts it. fresh says it was
// worked out in this call.
func (s *Server) digest(ctx context.Context, o verdict.Options) (fresh bool, err error) {
	// When it falls due, and where it goes: the flags', or the ledger's
	// once it was set in the UI.
	in, err := s.digestNow(ctx)
	if err != nil {
		return false, err
	}
	week, from, to := s.digestWeek(in.When)
	cursor, err := s.l.Cursor(ctx)
	if err != nil {
		return false, err
	}
	// Past the week's end by what a firing of it can catch after it, and
	// --settle after that (wholeAfter).
	wait, err := s.wholeAfterNow(ctx, o)
	if !s.running(ctx) && err == nil {
		// No runs: the ledger has what was copied in, up to where each
		// source that isn't quiet has written. One gone quiet doesn't hold
		// the digest up: the digest says it is quiet. Nothing is read
		// again, and no signal: copied firings are judged against
		// incidents written down, by --lead.
		cursor, err = s.copiedTo(ctx)
		wait = o.Lead
	}
	if err != nil || cursor.Before(to.Add(wait)) {
		return false, err // the week isn't all in the ledger yet
	}
	done, err := s.l.Meta(ctx, "digest")
	if err != nil {
		return false, err
	}
	// Its day moved back across a week's start: the digest never goes
	// back to a week older than the last worked out.
	if last, err := strconv.ParseInt(done, 10, 64); err == nil && week < last {
		return false, nil
	}
	fresh = done != fmt.Sprint(week)
	if fresh {
		s.status.set("the weekly digest")
		began := s.now()
		// The incidents as they are before the work: one written down
		// meanwhile is looked at next time.
		incs, err := s.l.Incidents(ctx, from, to, false)
		if err != nil {
			return true, err
		}
		seen := map[string]int64{}
		for _, in := range incs {
			seen[in.ID] = in.Updated.Unix()
		}
		rep, err := s.l.Report(ctx, from, to, o, false)
		if err != nil {
			return true, err
		}
		var sugg []verdict.Suggestion
		var near []verdict.NearMiss
		var unrec []verdict.Unrecorded
		// The suggestions, the near misses and what firings with no record
		// were share one --timeout, so the digest's replays hold the runs up
		// no longer than a run may take.
		rctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
		defer cancel()
		if s.replaying() {
			in, err := s.replayInput(ctx, rep, from, to, o)
			if err != nil {
				return true, err
			}
			if len(in.versions) == 1 {
				sugg, _ = source.Suggest(rctx, s.h, in.versions[0].Rules, rep, in.incs, in.o, s.cfg.Step, s.cfg.Parallel, in.excuse)
			}
			var done bool
			near, done, err = source.NearMisses(rctx, s.h, in.versions, rep, in.incs, in.o, s.cfg.Step, s.cfg.Parallel, in.excuse)
			if !done {
				s.log.Warn("the digest's near misses weren't all looked for", "err", err)
			}
			if err != nil {
				// What was kept for the week stays, with what was found for
				// the incidents it had nothing for; the week is worked out
				// again, as incidents written down since are.
				seen = nil
				if prev, ok, err := s.l.ForPeriod(ctx, "near_misses", from); err != nil {
					return true, err
				} else if ok {
					var kept []verdict.NearMiss
					if !s.decodeKept(prev, &kept) {
						kept = nil
					}
					near, _ = addNew(kept, near, rep)
				}
			} else {
				seen = seenNow(seen, rep, nil)
			}
			// With no record, no rule was noisy: what the firings of those
			// that fired as often were, in the time that is left; and of a
			// team's rules, by its own record, when the ledger's is kept.
			if unrec, err = s.unrecordedTeams(ctx, rctx, in, rep, o, from, to); err != nil {
				s.log.Warn("the digest's firings with no record to judge them by weren't all looked at", "err", err)
			}
		}
		if err := s.l.AddResult(ctx, "suggestions", s.now(), from, to, orNone(sugg)); err != nil {
			return true, err
		}
		if err := s.l.ReplaceResult(ctx, "unrecorded", s.now(), from, to, orNoneOf(unrec)); err != nil {
			return true, err
		}
		// The rules that fired mostly on what another team owns: one read,
		// with a time of its own, which the replays above can't have used
		// up, and short (firedOnWithin), so that the digest holds the runs
		// up for --timeout and that at most. What runs out of it isn't kept,
		// and says so.
		fctx, fcancel := context.WithTimeout(ctx, min(s.cfg.Timeout, firedOnWithin))
		on, unread, err := s.firedOnOf(fctx, from, to)
		late := fctx.Err() != nil
		fcancel()
		switch {
		case err != nil && late:
			s.log.Warn("the digest's firings by team weren't counted in its --timeout", "err", err)
		case err != nil:
			return true, err
		default:
			if unread > 0 {
				s.log.Warn("the digest counted the firings by team of the alerts that fired most alone", "alerts read", firedOnRead, "firings not read", unread)
			}
			if err := s.l.ReplaceResult(ctx, "fired_on", s.now(), from, to, orNoneOf(on)); err != nil {
				return true, err
			}
		}
		if err := s.keepNear(ctx, from, to, cursor, seen, orNoneOf(near)); err != nil {
			return true, err
		}
		if err := s.l.AddRun(ctx, ledger.Run{Kind: "digest", Started: began, Finished: s.now(), From: from, To: to}); err != nil {
			return true, err
		}
		if err := s.l.SetMeta(ctx, "digest", fmt.Sprint(week)); err != nil {
			return true, err
		}
	}
	// Off, the week's work is done, for the proposals and the incidents'
	// pages, and nothing is sent: the week is passed over, so that turned
	// on again it goes from the next, as a report does.
	if !in.On {
		return fresh, s.passDigest(ctx, fmt.Sprint(week))
	}
	return fresh, errors.Join(s.tellDigest(ctx, in, fmt.Sprint(week), fresh, from, to, o), s.postDigest(ctx, in, fmt.Sprint(week), from, to, o))
}

// replayInput is what a replay over a period's rules needs besides them: the
// rules as they stood, the period's incidents, the options for it, and
// which firings its quiet windows excuse.
type replayInput struct {
	versions []replay.Version
	incs     []incident.Incident
	o        verdict.Options
	excuse   func(firing.Firing) bool
}

func (s *Server) replayInput(ctx context.Context, rep *verdict.Report, from, to time.Time, o verdict.Options) (replayInput, error) {
	vs, err := s.versions(ctx, from, to)
	if err != nil {
		return replayInput{}, err
	}
	in := replayInput{o: o}
	for _, v := range vs {
		in.versions = append(in.versions, replay.Version{From: v.Since, Rules: v.Rules})
	}
	// Those that keep the period's record from before it, so that a replay
	// judged against them has the record the ledger's report has.
	if in.incs, err = s.recordAround(ctx, from, to, o); err != nil {
		return replayInput{}, err
	}
	for _, x := range rep.Incidents {
		in.incs = append(in.incs, x.Incident)
	}
	in.o.From, in.o.To = from, to
	// People's verdicts, which a replay with another "for" or number
	// finds its firings again by (docs/design/judging.md, "Through judging
	// again").
	if in.o.Verdicts, err = s.l.ReplayVerdicts(ctx, from, to); err != nil {
		return replayInput{}, err
	}
	// What a replay with another "for" or number fires is excused by the
	// quiet windows as the ledger's firings are.
	quiet, err := s.l.Windows(ctx, from, to)
	if err != nil {
		return replayInput{}, err
	}
	in.excuse = ledger.Excusing(quiet, from, to)
	return in, nil
}

// nearSeen is how far a week's near misses were worked out: with each of
// its counted incidents as last written then (when, in Unix seconds), and
// the ledger's firings up to Cursor. Updated, without Incidents, is a
// digest's from before that was kept: when it was made. Failed is what a
// round couldn't work out, for a question failing or time running out: a
// fingerprint of the incidents, when (by the server's clock), and how many
// rounds in a row; they are tried again after an hour, then two, four, up to
// a day, or as soon as they change.
type nearSeen struct {
	Incidents map[string]int64 `json:"incidents,omitempty"`
	Updated   int64            `json:"updated,omitempty"`
	Cursor    int64            `json:"cursor"`
	Failed    string           `json:"failed,omitempty"`
	At        int64            `json:"at,omitempty"`
	Tries     int              `json:"tries,omitempty"`
}

// incidentsPrint tells one set of incidents, each as last written, and ids
// taken away, from another.
func incidentsPrint(incs []ledger.Incident, gone []string) string {
	h := sha256.New()
	for _, in := range incs {
		fmt.Fprintf(h, "%q %d\n", in.ID, in.Updated.Unix())
	}
	for _, id := range gone {
		fmt.Fprintf(h, "%q gone\n", id)
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func (s *Server) nearSeen(ctx context.Context, w int64) (nearSeen, bool, error) {
	var x nearSeen
	v, err := s.l.Meta(ctx, "near:"+fmt.Sprint(w))
	if err != nil || v == "" {
		return x, false, err
	}
	return x, true, json.Unmarshal([]byte(v), &x)
}

func (s *Server) setNearSeen(ctx context.Context, w int64, x nearSeen) error {
	b, err := json.Marshal(x)
	if err != nil {
		return err
	}
	return s.l.SetMeta(ctx, "near:"+fmt.Sprint(w), string(b))
}

// keepNear keeps a week's near misses, in place of those kept for it
// before, and how far they were worked out: with the incidents seen, each
// as it was last written before the work began, and the ledger's firings up
// to cursor.
func (s *Server) keepNear(ctx context.Context, from, to, cursor time.Time, seen map[string]int64, near []verdict.NearMiss) error {
	if err := s.l.ReplaceResult(ctx, "near_misses", s.now(), from, to, near); err != nil {
		return err
	}
	if seen == nil {
		seen = map[string]int64{}
	}
	return s.setNearSeen(ctx, ledger.Week(s.l.Day(from)), nearSeen{Incidents: seen, Cursor: cursor.Unix()})
}

// addNew is kept, and of near, the near misses of the incidents kept has
// none for, in rep's order of incidents; added says whether there were any.
func addNew(kept, near []verdict.NearMiss, rep *verdict.Report) (out []verdict.NearMiss, added bool) {
	had := map[string]bool{}
	for _, nm := range kept {
		had[nm.Incident] = true
	}
	out = slices.Clone(kept)
	for _, nm := range near {
		if !had[nm.Incident] {
			out, added = append(out, nm), true
		}
	}
	return byIncident(out, rep), added
}

// byIncident orders near misses as rep orders their incidents.
func byIncident(nms []verdict.NearMiss, rep *verdict.Report) []verdict.NearMiss {
	order := map[string]int{}
	for i, ir := range rep.Incidents {
		order[ir.ID] = i
	}
	slices.SortStableFunc(nms, func(a, b verdict.NearMiss) int { return order[a.Incident] - order[b.Incident] })
	return nms
}

// seenNow is what was seen of a week's incidents once rep's are looked at:
// those NearMisses passed over, past the twenty it looks at, as they were
// seen before (not at all, without before), so they are looked at next
// time.
func seenNow(seen map[string]int64, rep *verdict.Report, before map[string]int64) map[string]int64 {
	looked := source.LookedAt(rep)
	for _, ir := range rep.Incidents {
		if (ir.Missed() || ir.OnlyNoisy()) && !looked[ir.ID] {
			if at, ok := before[ir.ID]; ok {
				seen[ir.ID] = at
			} else {
				delete(seen, ir.ID)
			}
		}
	}
	return seen
}

// nearLater looks again, for each week of those kept, at the incidents
// written down, changed or taken away since its near misses were last worked
// out, and at those whose end, and --settle after, the ledger's firings have
// reached since: for a week the digest is past, and for the week under way.
// What it finds for them takes the place of what was kept for them; the
// week's other incidents keep theirs. The latest week first, all within one
// --timeout; a week left is looked at next time.
func (s *Server) nearLater(ctx context.Context, o verdict.Options) error {
	cursor, err := s.l.Cursor(ctx)
	if err != nil || cursor.IsZero() {
		return err
	}
	now := ledger.Week(s.l.Day(cursor.Add(-time.Second)))
	first := now - 7*(weeks-1)
	incs, err := s.l.Incidents(ctx, s.l.Midnight(first), s.l.Midnight(now+7), false)
	if err != nil {
		return err
	}
	byWeek := map[int64][]ledger.Incident{}
	for _, in := range incs {
		w := ledger.Week(s.l.Day(in.Start))
		byWeek[w] = append(byWeek[w], in)
	}
	o.Hours = nil // no hours to count for this
	var rctx context.Context
	for w := now; w >= first; w -= 7 {
		x, ok, err := s.nearSeen(ctx, w)
		if err != nil {
			return err
		}
		prev, worked, err := s.l.ForPeriod(ctx, "near_misses", s.l.Midnight(w))
		if err != nil {
			return err
		}
		switch {
		case ok:
		case worked:
			x.Updated, x.Cursor = prev.At.Unix(), prev.To.Unix() // a digest's from before
		case w != now:
			continue // before near misses were looked for
		}
		// What to look at again: what changed, and what the firings have
		// reached the end of.
		var look []ledger.Incident
		here := map[string]bool{}
		for _, in := range byWeek[w] {
			here[in.ID] = true
			at, had := x.Incidents[in.ID]
			end := in.EndOr(o.UnknownLen).Add(s.cfg.Settle).Unix()
			if x.Incidents == nil && in.Updated.Unix() > x.Updated || x.Incidents != nil && (!had || at != in.Updated.Unix()) ||
				x.Cursor < end && end <= cursor.Unix() {
				look = append(look, in)
			}
		}
		var gone []string
		for _, id := range slices.Sorted(maps.Keys(x.Incidents)) {
			if !here[id] {
				gone = append(gone, id)
			}
		}
		var kept []verdict.NearMiss
		if worked {
			if !s.decodeKept(prev, &kept) {
				kept = nil
			}
		}
		if x.Incidents == nil {
			// Kept from before what was seen was: the incidents gone are
			// those near misses were kept for.
			for _, nm := range kept {
				if !here[nm.Incident] && !slices.Contains(gone, nm.Incident) {
					gone = append(gone, nm.Incident)
				}
			}
			slices.Sort(gone)
		}
		if len(look) == 0 && len(gone) == 0 && (ok || worked) {
			continue
		}
		fp := incidentsPrint(look, gone)
		if x.Failed == fp && s.now().Unix() < x.At+int64(min(time.Hour<<min(x.Tries-1, 5), 24*time.Hour)/time.Second) {
			continue // failed lately, and nothing has changed
		}
		if rctx == nil {
			s.status.set("near misses of incidents written down since")
			var cancel context.CancelFunc
			rctx, cancel = context.WithTimeout(ctx, s.cfg.Timeout)
			defer cancel()
		}
		if rctx.Err() != nil {
			break // time's up: the rest next time
		}
		from, to := s.l.Midnight(w), s.l.Midnight(w+7)
		if to.After(cursor) {
			to = cursor
		}
		rep, err := s.l.Report(ctx, from, to, o, false)
		if err != nil {
			return err
		}
		in, err := s.replayInput(ctx, rep, from, to, o)
		if err != nil {
			return err
		}
		// Only the incidents to look at again; the cost is judged against
		// them all.
		again := map[string]bool{}
		for _, l := range look {
			again[l.ID] = true
		}
		some := *rep
		some.Incidents = slices.DeleteFunc(slices.Clone(rep.Incidents), func(ir verdict.IncidentResult) bool { return !again[ir.ID] })
		near, _, err := source.NearMisses(rctx, s.h, in.versions, &some, in.incs, in.o, s.cfg.Step, s.cfg.Parallel, in.excuse)
		if err != nil {
			// Tried again later. What was kept stays: what was found may be
			// short of it. An incident that had nothing kept gets what was
			// found for it meanwhile.
			tries := 1
			if x.Failed == fp {
				tries = x.Tries + 1
			}
			x.Failed, x.At, x.Tries = fp, s.now().Unix(), tries
			s.log.Warn("near misses of incidents written down since weren't worked out", "week", from.Format(time.DateOnly), "err", err)
			if more, added := addNew(kept, near, rep); added {
				if err := s.l.ReplaceResult(ctx, "near_misses", s.now(), from, to, more); err != nil {
					return err
				}
			}
			if err := s.setNearSeen(ctx, w, x); err != nil {
				return err
			}
			continue
		}
		// The week's other incidents keep what was kept for them.
		drop := maps.Clone(again)
		for _, id := range gone {
			drop[id] = true
		}
		kept = slices.DeleteFunc(kept, func(nm verdict.NearMiss) bool { return drop[nm.Incident] })
		kept = byIncident(append(kept, near...), rep)
		seen := map[string]int64{}
		for _, in := range byWeek[w] {
			seen[in.ID] = in.Updated.Unix()
		}
		if err := s.keepNear(ctx, from, to, cursor, seenNow(seen, &some, x.Incidents), orNoneOf(kept)); err != nil {
			return err
		}
		s.log.Info("near misses worked out again", "week", from.Format(time.DateOnly), "incidents", len(look), "near misses", len(near))
	}
	return nil
}

// laterWeek reports whether week a is later than b, both the digest's
// weeks as kept; any week is later than none.
func laterWeek(a, b string) bool {
	x, errA := strconv.ParseInt(a, 10, 64)
	y, errB := strconv.ParseInt(b, 10, 64)
	return errA == nil && (errB != nil || x > y)
}

// passDigest passes a week of the digest over, to every destination, as if
// it had been sent: the digest was off when it fell due.
func (s *Server) passDigest(ctx context.Context, week string) error {
	if sent, err := s.l.Meta(ctx, "digest-sent"); err != nil || sent == week {
		return err
	}
	return errors.Join(s.l.SetMeta(ctx, "digest-sent", week), s.l.TellOnce(ctx, "digest-told", week, "digest.ready", nil))
}

// postDigest posts the week's report to Slack, trying again each hour until
// it goes.
func (s *Server) postDigest(ctx context.Context, in digestIn, week string, from, to time.Time, o verdict.Options) error {
	if s.cfg.Slack == "" || !in.goes("slack") {
		return nil
	}
	if sent, err := s.l.Meta(ctx, "digest-sent"); err != nil || sent == week {
		return err
	}
	if s.now().Sub(s.slackTried) < time.Hour {
		return nil
	}
	s.slackTried = s.now()
	rep, err := s.digestReport(ctx, from, to, o)
	if err != nil {
		return err
	}
	spare, err := s.spareRules(ctx)
	if err != nil {
		return err
	}
	text, err := s.digestText(ctx, rep, spare, from, to, nil)
	if err != nil {
		return err
	}
	// Posted only while this process still holds the ledger: digest-sent
	// was read through a reader, and a process that took the ledger over
	// posts it too.
	if err := s.l.Hold(ctx); err != nil {
		return err
	}
	// Claimed, among the replicas: two leaders for a moment post it once.
	// Whoever claims it posts it, within the minute its claim outlasts.
	claim := "digest-post:" + week
	if ok, err := s.l.ClaimOnce(ctx, claim, digestClaim); err != nil || !ok {
		if err == nil {
			// Another replica, the leader before this one, holds the claim:
			// tried again once it is over, not an hour from now.
			s.slackTried = time.Time{}
			time.AfterFunc(digestClaim+time.Second, s.poke)
		}
		return err
	}
	if sent, err := s.l.Meta(ctx, "digest-sent"); err != nil || sent == week {
		return err
	}
	sctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := report.Slack(sctx, s.cfg.Slack, text); err != nil {
		s.l.AddRun(ctx, ledger.Run{Kind: "digest", Started: s.now(), Finished: s.now(), From: from, To: to, Error: err.Error()})
		pctx, pcancel := s.posted(ctx)
		defer pcancel()
		s.l.Unclaim(pctx, claim)
		return err
	}
	pctx, pcancel := s.posted(ctx)
	defer pcancel()
	// digest-sent is set for a week passed over too; digest-posted, for a
	// week posted alone, which the pages say.
	return errors.Join(s.l.SetMeta(pctx, "digest-sent", week), s.l.SetMeta(pctx, "digest-posted", week))
}

// digestClaim is how long a replica holds the digest's post for itself.
const digestClaim = 2 * time.Minute

// digestReport is the week's report, with the suggestions worked out for it.
func (s *Server) digestReport(ctx context.Context, from, to time.Time, o verdict.Options) (*verdict.Report, error) {
	return s.digestReportFor(ctx, from, to, o, nil)
}

// digestReportFor is digestReport, judged by a team's record when team
// isn't nil: by the incidents it owns. Its suggestions, while the week has a
// record; what its firings were with no record, while it has none.
func (s *Server) digestReportFor(ctx context.Context, from, to time.Time, o verdict.Options, team *ledger.Team) (*verdict.Report, error) {
	rep, err := s.ledgerReportFor(ctx, from, to, o, false, team)
	if err != nil {
		return nil, err
	}
	rs, err := s.l.Results(ctx, "suggestions", 1)
	if err != nil {
		return nil, err
	}
	if len(rs) > 0 && rs[0].From.Equal(from) && !rep.NoRecord && !s.decodeKept(rs[0], &rep.Suggestions) {
		rep.Suggestions = nil
	}
	// The week's near misses, as last worked out: by the digest, or since,
	// for an incident written down later.
	r, ok, err := s.l.ForPeriod(ctx, "near_misses", from)
	if err != nil {
		return nil, err
	}
	if ok && !s.decodeKept(r, &rep.NearMisses) {
		rep.NearMisses = nil
	}
	// Only for an incident nothing caught, or only noisy rules did, still:
	// judged again with other settings, it may have been caught since.
	open := map[string]bool{}
	for _, ir := range rep.Incidents {
		open[ir.ID] = ir.Missed() || ir.OnlyNoisy()
	}
	rep.NearMisses = slices.DeleteFunc(rep.NearMisses, func(nm verdict.NearMiss) bool { return !open[nm.Incident] })
	// What the firings with no record were, while the week still has none,
	// of everyone's or the team's.
	if rep.NoRecord {
		if r, ok, err := s.l.ForPeriod(ctx, "unrecorded", from); err != nil {
			return nil, err
		} else if ok && s.decodeKept(r, &rep.Unrecorded) {
			scope := ""
			if team != nil {
				scope = team.Name
			}
			rep.Unrecorded = slices.DeleteFunc(rep.Unrecorded, func(u verdict.Unrecorded) bool { return u.Scope != "" && u.Scope != scope })
		} else {
			rep.Unrecorded = nil
		}
	}
	return rep, nil
}

// spareRules are the rules spare over the twelve weeks kept: a week alone
// rarely has the incidents to tell.
func (s *Server) spareRules(ctx context.Context) ([]string, error) { return s.spareOver(ctx, weeks) }

// spareOver are the rules spare over the last n weeks of the twelve kept,
// as a rule's page over them says (ruleLines).
func (s *Server) spareOver(ctx context.Context, n int) ([]string, error) {
	t, err := s.tallies(ctx, weeks)
	if err != nil {
		return nil, err
	}
	credit, err := s.credit(ctx, t, n)
	if err != nil {
		return nil, err
	}
	var out []string
	for name, r := range credit {
		if r.Spare {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out, nil
}

// digestMax is how long a digest's text is, at most, in what is told of it.
const digestMax = 256 << 10

// digestText is the week's report, and how things stand: for everyone, or
// for one team.
func (s *Server) digestText(ctx context.Context, rep *verdict.Report, spare []string, from, to time.Time, team *ledger.Team) (string, error) {
	sent, err := s.sent(ctx, rep, from, to)
	if err != nil {
		return "", err
	}
	return s.digestTextSent(ctx, rep, spare, from, to, team, sent)
}

// digestTextSent is digestText, with what the notifications came to as
// sent worked it out: a digest's text and what is told of it read it once.
func (s *Server) digestTextSent(ctx context.Context, rep *verdict.Report, spare []string, from, to time.Time, team *ledger.Team, sent *sentLine) (string, error) {
	shown := *rep
	shown.Rules = slices.Clone(rep.Rules)
	for i := range shown.Rules {
		shown.Rules[i].Spare = false // said over twelve weeks, below
	}
	var report bytes.Buffer
	if err := verdict.Text(&report, &shown, s.cfg.Loc); err != nil {
		return "", err
	}
	var id int64
	var outs string
	if team != nil {
		id = team.ID
	} else {
		var err error
		if outs, err = s.outcomesLine(ctx); err != nil {
			return "", err
		}
	}
	tasks, err := s.tasksLine(ctx, from, to, id)
	if err != nil {
		return "", err
	}
	// Firings copied in, for everyone's digest; notifications sent, for the
	// rules in it.
	var copiedLine, checkedLine string
	if team == nil {
		cs, err := s.copied(ctx, rep)
		if err != nil {
			return "", err
		}
		copiedLine = s.sourcesLine(cs)
		// Only a week checked to its end.
		if s.checking() {
			done, err := s.l.CheckedTo(ctx)
			if err != nil {
				return "", err
			}
			if !done.Before(to) {
				if checkedLine, err = s.checkLine(ctx, from, to); err != nil {
					return "", err
				}
			}
		}
	}
	// Re-fires and firings during deploys (docs/design/noc.md).
	rf, err := s.refiresOf(ctx, rep, from, to)
	if err != nil {
		return "", err
	}
	// The week's goals, of whom the digest is for (docs/design/goals.md).
	goals, _, err := s.goalsLine(ctx, from, team)
	if err != nil {
		return "", err
	}
	// The expectations of its rules, as last checked (docs/design/
	// expectations.md).
	ed, err := s.expectDigestOf(ctx, ledger.Week(s.l.Day(from)), team)
	if err != nil {
		return "", err
	}
	// The marked windows on in the week (docs/design/marked.md).
	mk, err := s.markedOfReport(ctx, rep, from, to, team, s.cfg.Loc)
	if err != nil {
		return "", err
	}
	// Of a team's, the rules of others that fired mostly on what it owns.
	var onLine string
	if team != nil {
		on, err := s.firedOnFor(ctx, from)
		if err != nil {
			return "", err
		}
		if xs := firedOnLines(on, from, team.Name); len(xs) > 0 {
			items := make([]string, 0, len(xs))
			for _, x := range xs[:min(len(xs), 20)] {
				items = append(items, tr(s.cfg.Lang, "%s (%d of %d firings)", verdict.Clean(x.Rule), x.Firings, x.Of))
			}
			onLine = tr(s.cfg.Lang, "Rules of other teams, or of no team, that fired mostly on what %s owns: %s.", verdict.Clean(team.Name), strings.Join(items, "; "))
			if n := len(xs) - len(items); n > 0 {
				onLine += " " + tr(s.cfg.Lang, "And %d more.", n)
			}
		}
	}
	line := ""
	if len(spare) > 0 {
		names := make([]string, 0, len(spare))
		for _, name := range spare[:min(len(spare), 20)] {
			names = append(names, verdict.Clean(name))
		}
		line = tr(s.cfg.Lang, "Over twelve weeks, another rule caught every incident these caught, as soon, at the same severity or above: %s.", strings.Join(names, ", "))
		if n := len(spare) - len(names); n > 0 {
			line += " " + tr(s.cfg.Lang, "And %d more.", n)
		}
	}
	// These go after the report's first lines, not at its end: on a large
	// install, what is too long for a chat is cut from the end.
	head, rest, _ := strings.Cut(report.String(), "\n\n")
	var b strings.Builder
	b.WriteString(head + "\n")
	for _, x := range []string{goals, ed.text(s.cfg.Lang), onLine, episodesLine(s.cfg.Lang, rep), peopleLine(s.cfg.Lang, rep), rf.refireLine(s.cfg.Lang), rf.deployLine(s.cfg.Lang), mk.text(), line, sent.text(s.cfg.Lang), copiedLine, checkedLine, tasks, outs} {
		if x != "" {
			b.WriteString("\n" + x + "\n")
		}
	}
	if rest != "" {
		b.WriteString("\n" + rest)
	}
	return strings.ToValidUTF8(b.String()[:min(b.Len(), digestMax)], ""), nil
}

// digestAbout is what is told of a digest: its numbers, for a receiver to
// use as it likes, and the report as text, for one to pass on to a chat:
// as written, and escaped for Slack.
//
// episodesLine is what the digest says of the noisy rules' episodes
// (docs/design/judging.md, item 2): each one's firings in its episodes, and
// those caught, the most firings that caught nothing first; and of a rule
// noisy by firings and not by episodes, that it isn't. "" with none, or
// with the week's episodes not counted yet.
func episodesLine(l string, rep *verdict.Report) string {
	if rep.EpisodesNotCounted {
		return ""
	}
	var noisy []verdict.RuleResult
	for _, r := range rep.Rules {
		if r.Noisy && r.Episodes.Total > 0 {
			noisy = append(noisy, r)
		}
	}
	if len(noisy) == 0 {
		return ""
	}
	slices.SortStableFunc(noisy, func(a, b verdict.RuleResult) int {
		return cmp.Or(cmp.Compare(b.Black-b.Unjudged, a.Black-a.Unjudged), strings.Compare(a.Rule, b.Rule))
	})
	items := make([]string, 0, 10)
	for _, r := range noisy[:min(len(noisy), 10)] {
		if r.Episodes.Noisy {
			items = append(items, tr(l, "%s, %d firings in %d episodes, %d caught", verdict.Clean(r.Rule), r.Firings, r.Episodes.Total, r.Episodes.White))
		} else {
			items = append(items, tr(l, "%s, %d firings in %d episodes, %d caught: noisy by firings, not by episodes", verdict.Clean(r.Rule), r.Firings, r.Episodes.Total, r.Episodes.White))
		}
	}
	line := tr(l, "The noisy rules' episodes, one problem each however many alerts: %s.", strings.Join(items, "; "))
	if n := len(noisy) - len(items); n > 0 {
		line += " " + tr(l, "And %d more.", n)
	}
	return line
}

// peopleLine is what the digest says of people's verdicts on the week's
// firings (docs/design/judging.md, item 3): how many they said were
// unrelated, and of how many they answered whether they needed action,
// how many did. Never their notes. "" when nothing was said.
func peopleLine(l string, rep *verdict.Report) string {
	var unrelated, yes, no, firings int
	for _, r := range rep.Rules {
		unrelated += r.Unrelated
		yes += r.ActionYes
		no += r.ActionNo
		firings += r.Firings
	}
	switch {
	case unrelated == 0 && yes+no == 0:
		return ""
	case yes+no == 0:
		return tr(l, "People said %d firings were unrelated to what they caught.", unrelated)
	}
	return tr(l, "People said %d firings were unrelated, and answered %d of %d firings: %d needed action.", unrelated, yes+no, firings, yes)
}

func digestAbout(rep *verdict.Report, text, team string, spare []string) map[string]any {
	firings, white := 0, 0
	noisy, silent := []string{}, 0
	for _, r := range rep.Rules {
		firings += r.Firings
		white += r.White
		if r.Noisy {
			noisy = append(noisy, r.Rule)
		}
		if r.Firings+r.Excused+r.Incidents == 0 && r.Unsupported == "" {
			silent++
		}
	}
	missed, onlyNoisy := 0, 0
	for _, in := range rep.Incidents {
		if in.Missed() {
			missed++
		}
		if in.OnlyNoisy() {
			onlyNoisy++
		}
	}
	type suggestion struct {
		Rule   string `json:"rule"`
		For    string `json:"for"`
		NewFor string `json:"new_for"`
		Fired  int    `json:"fired"`
		Would  int    `json:"would_fire"`
	}
	suggestions := []suggestion{}
	for _, x := range rep.Suggestions {
		suggestions = append(suggestions, suggestion{x.Rule, verdict.Duration(x.For), verdict.Duration(x.NewFor), x.Before.Firings, x.After.Firings})
	}
	if spare == nil {
		spare = []string{}
	}
	about := map[string]any{"from": rep.From.UTC(), "to": rep.To.UTC(), "firings": firings, "caught": white, "nothing": firings - white,
		"never_fired": silent, "incidents": len(rep.Incidents), "missed": missed, "only_noisy": onlyNoisy,
		"noisy_rules": noisy, "spare_rules": spare, "suggestions": suggestions,
		// With no record, no rule is noisy: what the firings of those that
		// fired as often were.
		"no_record": rep.NoRecord, "unrecorded": orNoneOf(rep.Unrecorded),
		// text is the report as written, from rule names and incident titles
		// others wrote: escape it for where it goes. slack_text is it as the
		// Slack digest posts it.
		"text": text, "slack_text": report.SlackText(text)}
	if team != "" {
		about["team"] = team
	}
	return about
}

// teamReport is a report's part for one team: its rules, counted by its
// hours, and the incidents it owns.
func teamReport(rep *verdict.Report, o *owners, t ledger.Team, server *verdict.Hours) *verdict.Report {
	out := *rep
	out.Firings = nil
	out.Rules = slices.DeleteFunc(slices.Clone(rep.Rules), func(r verdict.RuleResult) bool { return o.rule[r.Rule] != t.ID })
	if rep.Hours != nil {
		// Its rules were counted by its hours: say which.
		out.Hours = o.schedules(server).used(nil, &t)
	}
	out.Suggestions = slices.DeleteFunc(slices.Clone(rep.Suggestions), func(x verdict.Suggestion) bool { return o.rule[x.Rule] != t.ID })
	// Its rules' firings with no record; and of another's, its alerts on
	// what the team owns alone, no third team's labels with them.
	out.Unrecorded = nil
	for _, u := range rep.Unrecorded {
		if o.rule[u.Rule] != t.ID {
			u.Top = slices.DeleteFunc(slices.Clone(u.Top), func(a verdict.AlertCount) bool { return a.Team != t.Name })
			if len(u.Top) == 0 {
				continue
			}
		}
		out.Unrecorded = append(out.Unrecorded, u)
	}
	out.Incidents = slices.DeleteFunc(slices.Clone(rep.Incidents), func(in verdict.IncidentResult) bool {
		return !o.ownsIncident(t, ledger.Incident{Incident: in.Incident, CaughtBy: in.CaughtBy})
	})
	// The rules that nearly caught the team's incidents, whoever's they are:
	// as another team's rule catching one counts.
	out.NearMisses = slices.DeleteFunc(slices.Clone(rep.NearMisses), func(nm verdict.NearMiss) bool {
		return !slices.ContainsFunc(out.Incidents, func(in verdict.IncidentResult) bool { return in.ID == nm.Incident })
	})
	return &out
}

// tellDigest tells of the week's digest, once: to the webhook, for everyone
// and for each team, to each team that has a webhook of its own, and, as
// text, to each destination --report-dest names that it goes to. A team to
// which nothing happened hears nothing. It goes through the outbox, so a
// receiver that is down holds up nothing and is tried again.
func (s *Server) tellDigest(ctx context.Context, in digestIn, week string, fresh bool, from, to time.Time, o verdict.Options) error {
	webhook := s.cfg.NotifyWebhook != "" && in.goes("webhook")
	teams := map[string]bool{}
	for name := range s.cfg.TeamDigests {
		teams[name] = in.goes("team:" + name)
	}
	var named []string
	for _, name := range slices.Sorted(maps.Keys(s.cfg.ReportDests)) {
		if in.goes(name) {
			named = append(named, name)
		}
	}
	if !webhook && !slices.Contains(slices.Collect(maps.Values(teams)), true) && len(named) == 0 {
		return nil
	}
	// What TellOnce keeps: read here, so a week told isn't worked out
	// again each round only for TellOnce to find it told.
	told0, err := s.l.Told(ctx, "digest-told")
	if err != nil {
		return err
	}
	// 0.24 and before marked a week passed over as old news in a key of
	// its own: the later of the two is the week last told.
	if old, err := s.l.Meta(ctx, "digest-told"); err != nil {
		return err
	} else if laterWeek(old, told0) {
		told0 = old
	}
	if told0 == week {
		return nil
	}
	// A webhook added, or an upgrade, on a Friday: Monday's digest is old
	// news. Not when the digest was set in the UI after it fell due: a week
	// due and sent nowhere goes once, where it is set to go.
	if told0 == "" && !fresh && (in.Set == nil || !in.Set.Updated.After(in.When.Last(s.now(), s.cfg.Loc))) {
		return s.l.TellOnce(ctx, "digest-told", week, "digest.ready", nil)
	}
	rep, err := s.digestReport(ctx, from, to, o)
	if err != nil {
		return err
	}
	spare, err := s.spareRules(ctx)
	if err != nil {
		return err
	}
	own, err := s.owners(ctx)
	if err != nil {
		return err
	}
	on, err := s.firedOnFor(ctx, from)
	if err != nil {
		return err
	}
	for name := range s.cfg.TeamDigests {
		if _, ok := own.team(name); !ok {
			s.log.Warn("--team-digest names a team the ledger doesn't have", "team", name)
		}
	}
	var told []ledger.Told
	if webhook || len(named) > 0 {
		// What the notifications came to, and the firings that paged no
		// one: read once, for the text and what is told of it.
		sent, err := s.sent(ctx, rep, from, to)
		if err != nil {
			return err
		}
		text, err := s.digestTextSent(ctx, rep, spare, from, to, nil, sent)
		if err != nil {
			return err
		}
		if webhook {
			about := digestAbout(rep, text, "", spare)
			if err := s.aboutMore(ctx, about, rep, from, to, nil, sent); err != nil {
				return err
			}
			if err := s.aboutGoals(ctx, about, from, nil); err != nil {
				return err
			}
			if err := s.aboutExpect(ctx, about, from, nil); err != nil {
				return err
			}
			told = append(told, ledger.Told{Dest: "webhook", About: about})
		}
		for _, name := range named {
			told = append(told, ledger.Told{Dest: reportDest, About: map[string]any{"to": name, "report": "weekly digest", "text": text}})
		}
	}
	for _, t := range own.teams {
		own1 := teams[t.Name]
		if !webhook && !own1 {
			continue
		}
		// Judged by the incidents it owns.
		judged, err := s.digestReportFor(ctx, from, to, o, &t)
		if err != nil {
			return err
		}
		part := teamReport(judged, own, t, o.Hours)
		fired := slices.ContainsFunc(part.Rules, func(r verdict.RuleResult) bool { return r.Firings+r.Excused > 0 })
		if !fired && len(part.Incidents) == 0 && len(firedOnLines(on, from, t.Name)) == 0 {
			continue // nothing happened to it, nor fired on what it owns
		}
		mine := slices.DeleteFunc(slices.Clone(spare), func(name string) bool { return own.rule[name] != t.ID })
		sent, err := s.sent(ctx, part, from, to)
		if err != nil {
			return err
		}
		text, err := s.digestTextSent(ctx, part, mine, from, to, &t, sent)
		if err != nil {
			return err
		}
		about := digestAbout(part, text, t.Name, mine)
		if err := s.aboutMore(ctx, about, part, from, to, &t, sent); err != nil {
			return err
		}
		if err := s.aboutGoals(ctx, about, from, &t); err != nil {
			return err
		}
		if err := s.aboutExpect(ctx, about, from, &t); err != nil {
			return err
		}
		if webhook {
			told = append(told, ledger.Told{Dest: "webhook", About: about})
		}
		if own1 {
			told = append(told, ledger.Told{Dest: teamDest, About: about})
		}
	}
	return s.l.TellOnce(ctx, "digest-told", week, "digest.ready", told)
}

func orNone(s []verdict.Suggestion) []verdict.Suggestion {
	if s == nil {
		return []verdict.Suggestion{}
	}
	return s
}

// prune drops, once a day, what is older than --keep.
func (s *Server) prune(ctx context.Context) error {
	if s.cfg.Keep <= 0 {
		return nil
	}
	today := fmt.Sprint(s.l.Day(s.now()))
	if done, err := s.l.Meta(ctx, "pruned"); err != nil || done == today {
		return err
	}
	s.status.set("pruning")
	if err := s.l.PruneKeeping(ctx, s.now().Add(-s.cfg.Keep), s.activityBefore()); err != nil {
		return err
	}
	return s.l.SetMeta(ctx, "pruned", today)
}

// tasksLine says, for the digest, how the follow-ups stand, of a team's or
// of everyone's: open, overdue, and done in the week.
func (s *Server) tasksLine(ctx context.Context, from, to time.Time, team int64) (string, error) {
	open, late, done, err := s.l.TeamTaskCounts(ctx, from, to, team)
	if err != nil || open+done == 0 {
		return "", err
	}
	return tr(s.cfg.Lang, "Tasks: %d open, %d overdue, %d done in the week.", open, late, done), nil
}

// tellDue tells what is told once as the leader comes round to it: tasks
// gone overdue, sources quiet, down or back. With nowhere to tell it
// (toldTo), nothing is looked at, so that none of it is marked told with
// no notice written.
func (s *Server) tellDue(ctx context.Context) {
	if len(s.toldTo()) == 0 {
		return
	}
	if _, err := s.l.Overdue(ctx, s.l.Midnight(s.l.Day(s.now()))); err != nil {
		s.fail("notifying", err)
	}
	if err := s.tellQuiet(ctx); err != nil {
		s.fail("notifying", err)
	}
	if err := s.tellSources(ctx); err != nil {
		s.fail("notifying", err)
	}
}
