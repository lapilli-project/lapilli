package serve

import (
	"context"
	"slices"
	"sort"
	"sync"
	"time"

	"lapilli.dev/internal/verdict"
)

// On-call schedules (docs/design/inputs.md): each feed --on-call names is
// read every so often, its turns kept in the ledger past what the feed
// holds, and the schedule's rota, the turns kept and the people's hours,
// set for shifts written on-call=NAME.

// onCallState is how each schedule was last read.
type onCallState struct {
	mu sync.Mutex
	by map[string]*onCallRead
}

// onCallRead is a schedule's last read, as the runs page shows it.
type onCallRead struct {
	Name string
	// At is when it was last read, well or not; OK when it was last read
	// well, Err why the last read failed.
	At, OK time.Time
	Err    string
	// Events the feed held, Notes what in it was passed over.
	Events int
	Notes  []string
	// Turns are how many are kept, From and To their span; Unknown the
	// people they name who aren't in the file, the most turns first.
	Turns    int
	From, To time.Time
	Unknown  []string
}

// onCallKept sets each schedule's rota from the turns the ledger keeps,
// before its feed is read again.
func (s *Server) onCallKept(ctx context.Context) error {
	if s.cfg.OnCall == nil {
		return nil
	}
	for _, sc := range s.cfg.OnCall.Schedules {
		if err := s.setRota(ctx, sc.Name); err != nil {
			return err
		}
	}
	return nil
}

// setRota sets a schedule's rota from the turns kept.
func (s *Server) setRota(ctx context.Context, name string) error {
	turns, err := s.l.Turns(ctx, name)
	if err != nil {
		return err
	}
	r := verdict.NewRota(turns, s.cfg.OnCall.HoursOf())
	verdict.OnCallNamed(name).Set(r)
	st := s.onCallRead(name)
	s.onCall.mu.Lock()
	defer s.onCall.mu.Unlock()
	st.Turns, st.From, st.To, st.Unknown = len(turns), time.Time{}, time.Time{}, nil
	if len(turns) > 0 {
		st.From, st.To = turns[0].From, turns[len(turns)-1].To
	}
	unknown := map[string]int{}
	for _, t := range turns {
		if !r.Known(t.Who) {
			unknown[t.Who]++
		}
	}
	for who := range unknown {
		st.Unknown = append(st.Unknown, who)
	}
	sort.Slice(st.Unknown, func(i, j int) bool {
		a, b := st.Unknown[i], st.Unknown[j]
		return unknown[a] > unknown[b] || unknown[a] == unknown[b] && a < b
	})
	st.Unknown = st.Unknown[:min(len(st.Unknown), 20)]
	return nil
}

func (s *Server) onCallRead(name string) *onCallRead {
	s.onCall.mu.Lock()
	defer s.onCall.mu.Unlock()
	if s.onCall.by == nil {
		s.onCall.by = map[string]*onCallRead{}
	}
	st := s.onCall.by[name]
	if st == nil {
		st = &onCallRead{Name: name}
		s.onCall.by[name] = st
	}
	return st
}

// readOnCall reads each feed now, and again every so often, until ctx is
// done.
func (s *Server) readOnCall(ctx context.Context) {
	for {
		s.readFeeds(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.cfg.OnCall.Every):
		}
	}
}

// readFeeds reads each schedule's feed once, one at a time, and keeps its
// turns: a feed that fails changes nothing kept.
func (s *Server) readFeeds(ctx context.Context) {
	for _, sc := range s.cfg.OnCall.Schedules {
		if ctx.Err() != nil {
			return
		}
		f, err := s.cfg.OnCall.Fetch(ctx, sc, s.cfg.OnCallClient, "lapilli/"+s.cfg.Version, s.now())
		changed := false
		if err == nil {
			changed, err = s.l.PutTurns(ctx, sc.Name, f.Turns)
		}
		st := s.onCallRead(sc.Name)
		s.onCall.mu.Lock()
		st.At = s.now()
		if err != nil {
			st.Err = err.Error()
		} else {
			st.Err, st.OK, st.Events, st.Notes = "", st.At, f.Events, slices.Clone(f.Notes)
		}
		s.onCall.mu.Unlock()
		if err != nil {
			// Never the URL: errors name the host only.
			s.log.Warn("reading an on-call schedule's feed; its turns kept stay", "schedule", sc.Name, "err", err)
			continue
		}
		if changed || verdict.OnCallNamed(sc.Name).Rota() == nil {
			if err := s.setRota(ctx, sc.Name); err != nil {
				s.log.Error("setting an on-call schedule's turns", "schedule", sc.Name, "err", err)
			}
		}
	}
}

// onCallView is each schedule's last read, by name.
func (s *Server) onCallView() []onCallRead {
	if s.cfg.OnCall == nil {
		return nil
	}
	var out []onCallRead
	for _, sc := range s.cfg.OnCall.Schedules {
		st := s.onCallRead(sc.Name)
		s.onCall.mu.Lock()
		v := *st
		v.Notes, v.Unknown = slices.Clone(st.Notes), slices.Clone(st.Unknown)
		s.onCall.mu.Unlock()
		out = append(out, v)
	}
	return out
}

// checkShifts refuses shifts naming a schedule --on-call doesn't.
func (s *Server) checkShifts(shifts string) error {
	name, ok := verdict.OnCallShifts(shifts)
	if !ok {
		return nil
	}
	if s.cfg.OnCall == nil || s.cfg.OnCall.Schedule(name) == nil {
		return bad("no schedule %q in --on-call: on-call=NAME names one of its schedules", name)
	}
	return nil
}
