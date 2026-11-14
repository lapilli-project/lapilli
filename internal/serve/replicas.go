package serve

// lapilli serve as several replicas on one Postgres ledger
// (docs/design/replicas.md): one leads, and runs what must run once; every
// replica serves the pages, the API, MCP, sign-in and the intake ports,
// and writes what people and jobs send it.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"lapilli.dev/internal/ledger"
)

// lead runs what must run once among the replicas, while this one leads,
// until ctx is done (it no longer leads, or serve stops): the runs and the
// backfill, the jobs (judging again, the incidents and deploys files, the
// check of the replay, the digest, near misses, proposals, changes carried
// out, reports, pruning), the rules watched and their versions, the
// outbox, and the on-call feeds. Each loop stops with ctx; lead returns
// once they all have. A SQLite ledger's process, or a Postgres one's with
// a single replica, leads from its start. failed is told of what makes
// serve stop: the rules' history that can't be added.
func (s *Server) lead(ctx context.Context, failed chan<- error) {
	if s.l.Shared() {
		p, _ := s.l.Leader()
		s.log.Info("leading: this replica runs what is run once", "replica", s.l.ReplicaID(), "term", p.Term)
		defer s.log.Info("no longer leading", "replica", s.l.ReplicaID())
		// The rules' history and the on-call turns, as a process does at
		// start, by the one that leads.
		if err := s.history(ctx); err != nil {
			if ctx.Err() == nil {
				failed <- err
			}
			return
		}
		if err := s.onCallKept(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("reading the on-call turns kept", "err", err)
		}
	}
	// The rules are read before the first run, which replays them.
	raw := ""
	if s.replaying() {
		if err := s.look(ctx, &raw); err != nil {
			s.fail("reading the rules", err)
		}
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.work(ctx)
	}()
	if s.replaying() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.watch(ctx, raw)
		}()
	}
	if len(s.sendsTo()) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.notify(ctx)
		}()
	}
	if s.cfg.OnCall != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.readOnCall(ctx)
		}()
	}
	wg.Wait()
}

// still says whether the leader's work may go on with ctx: not done, and
// this replica still leads by its lease's clock. Looked at before each of
// the jobs, so one that no longer leads starts none of them.
func still(ctx context.Context) bool { return ctx.Err() == nil && ledger.Leads(ctx) }

// keepUp keeps what a follower shows that the leader's loops keep in
// theirs, until ctx is done: what the rules say, and Grafana's data
// sources and a Loki's places they are replayed with, read as the leader
// reads them (no version is recorded); and the on-call rotas, from the
// turns the leader keeps in the ledger. Nothing for a replica that leads.
func (s *Server) keepUp(ctx context.Context) {
	raw := ""
	var rota time.Time
	every := s.cfg.Watch
	if every <= 0 {
		every = time.Minute
	}
	for {
		if !s.l.Leading() {
			if s.replaying() {
				if err := s.peek(ctx, &raw); err != nil && ctx.Err() == nil {
					s.log.Warn("reading the rules, for what they say", "err", err)
				}
			}
			if s.cfg.OnCall != nil && time.Since(rota) >= min(s.cfg.OnCall.Every, time.Minute) {
				if err := s.onCallKept(ctx); err != nil && ctx.Err() == nil {
					s.log.Warn("reading the on-call turns kept", "err", err)
				}
				rota = time.Now()
			}
		} else {
			// What the leader read; read afresh when this one follows again.
			raw = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// peek reads the rules as look does, for what pages show of them alone:
// no version is recorded, nothing is written.
func (s *Server) peek(ctx context.Context, raw *string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	rs, docs, err := s.readRules(ctx)
	if err != nil {
		return err
	}
	s.askScrape(ctx)
	s.askNotify(ctx)
	if sum := docsSum(docs); sum != *raw {
		s.said(s.matching(rs))
		*raw = sum
	}
	return nil
}

// sharedStatus is what the leader says of what it is doing, for the
// followers' pages: what the status banner, the runs page and the pages'
// versions are made of.
type sharedStatus struct {
	Doing      string                `json:"doing,omitempty"`
	Args       []string              `json:"args,omitempty"`
	ErrWhat    string                `json:"err_what,omitempty"`
	Err        string                `json:"err,omitempty"`
	Judged     float64               `json:"judged,omitempty"`
	Took       time.Duration         `json:"took,omitempty"`
	Runs       map[string]int        `json:"runs,omitempty"`
	CheckEmpty []string              `json:"check_empty,omitempty"`
	CheckDay   time.Time             `json:"check_day,omitzero"`
	OnCall     map[string]onCallRead `json:"on_call,omitempty"`
}

// ownStatus is this replica's status, as it would say it.
func (s *Server) ownStatus() sharedStatus {
	s.status.Lock()
	st := sharedStatus{Doing: s.status.doing, ErrWhat: s.status.errWhat, Err: s.status.err, Judged: s.status.judged, Took: s.status.took,
		Runs: maps.Clone(s.status.runs), CheckEmpty: slices.Clone(s.status.checkEmpty), CheckDay: s.status.checkDay}
	for _, a := range s.status.args {
		st.Args = append(st.Args, fmt.Sprint(a))
	}
	s.status.Unlock()
	if s.cfg.OnCall != nil {
		st.OnCall = map[string]onCallRead{}
		for _, v := range s.onCallView() {
			st.OnCall[v.Name] = v
		}
	}
	return st
}

// publishedStatus is what this replica says of itself while it leads.
func (s *Server) publishedStatus() []byte {
	b, _ := json.Marshal(s.ownStatus())
	return b
}

// shownStatus is the status pages show: this replica's own while it leads,
// with no replicas, or with no other known to lead; else the leader's, as
// it last said, a few seconds late at most.
func (s *Server) shownStatus() sharedStatus {
	if !s.follows() {
		return s.ownStatus()
	}
	p, _ := s.l.Leader()
	if len(p.Status) == 0 {
		return sharedStatus{}
	}
	s.leaderSaid.Lock()
	defer s.leaderSaid.Unlock()
	if string(p.Status) != s.leaderSaid.text {
		var st sharedStatus
		if json.Unmarshal(p.Status, &st) == nil {
			s.leaderSaid.text, s.leaderSaid.st = string(p.Status), st
		}
	}
	return s.leaderSaid.st
}

// follows says another replica leads, as last read: this one shows what
// that one says. One that knows of none, as before the first heartbeat is
// read, shows its own.
func (s *Server) follows() bool {
	if s.l.Leading() {
		return false
	}
	p, ok := s.l.Leader()
	return ok && !p.Self
}

// args are a status's arguments as tr takes them.
func (st sharedStatus) args() []any {
	out := make([]any, len(st.Args))
	for i, a := range st.Args {
		out[i] = a
	}
	return out
}

// posted is the context what was sent is noted with, once sent: with
// replicas, noted even when the lease ended meanwhile, or serve stops, as
// a notice sent is, so that the leader that follows doesn't send it again.
func (s *Server) posted(ctx context.Context) context.Context {
	if !s.l.Shared() {
		return ctx
	}
	return ledger.WithoutLease(context.WithoutCancel(ctx))
}

// leadership says which replica leads, for /readyz.
func (s *Server) leadership() string {
	p, ok := s.l.Leader()
	switch {
	case !ok:
		return fmt.Sprintf("replica %s follows; no replica leads now (%d live)", s.l.ReplicaID(), s.l.Replicas())
	case p.Self:
		return fmt.Sprintf("replica %s leads, term %d, since %s (%d live)", s.l.ReplicaID(), p.Term, p.Since.UTC().Format(time.RFC3339), s.l.Replicas())
	}
	return fmt.Sprintf("replica %s follows %s (%s), term %d, since %s (%d live)", s.l.ReplicaID(), p.ID, p.Version, p.Term,
		p.Since.UTC().Format(time.RFC3339), s.l.Replicas())
}

// askRejudge asks the leader, this replica or another, to judge every
// firing again, and to look at its jobs now.
func (s *Server) askRejudge(ctx context.Context) {
	s.rejudge.Store(true)
	if err := s.l.AskRejudge(ctx); err != nil {
		s.log.Error("asking the replica that leads to judge again", "err", err)
	}
	s.poke()
}

// rejudgeAsked says whether another replica asked the leader to judge
// again since it last did: the count asked (Rejudges) past the one kept
// once judged.
func (s *Server) rejudgeAsked(ctx context.Context) (int64, bool) {
	if !s.l.Shared() {
		return 0, false
	}
	if err := s.l.Refresh(ctx); err != nil {
		return 0, false
	}
	n := s.l.Rejudges()
	done, err := s.l.Meta(ctx, "rejudged")
	if err != nil {
		return n, false
	}
	if done == "" {
		done = "0" // none asked, none done
	}
	return n, done != fmt.Sprint(n)
}

// shareLimits has the rate limits that guard the ledger's writes taken
// from buckets every replica shares, so that several replicas let through
// what one would: the tokens' writes, the notifications port's sources,
// the changes token's. What guards a process's own memory and time (one
// heavy request at a time, one write a token, the events' bytes) stays
// each replica's.
func (s *Server) shareLimits() {
	if !s.l.Shared() {
		return
	}
	for name, lim := range map[string]*limiter{"incidents-token": s.tokenRate, "firings-token": s.firingsRate, "tasks-token": s.tasksRate,
		"changes-token": s.changes.rate} {
		lim.share(s, name)
	}
	for src, lim := range s.port.rates {
		lim.share(s, "notify:"+src)
	}
}

// joinable refuses to join replicas already running with a sign-in of
// lapilli's own and no --session-key-file: each replica would sign its
// sessions with a key of its own, and a person signed in on one would not
// be on the next.
func (s *Server) joinable(ctx context.Context) error {
	if !s.l.Shared() || s.in == nil || s.in.oidc == nil || s.cfg.SessionKey != "" {
		return nil
	}
	n, err := s.l.Others(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("--session-key-file: %d other replicas of lapilli serve share this ledger, and each signs its sessions with a key of its own without one: give them all the same key", n)
	}
	s.log.Warn("sessions are signed with a key made for this process: a second replica sharing the ledger is refused until both have --session-key-file")
	return nil
}
