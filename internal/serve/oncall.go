package serve

import (
	"context"
	"slices"
	"sort"
	"sync"
	"time"

	"lapilli.dev/internal/oncall"
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
	// Who each turn is, as the file names people now: one added later, by
	// a name the feed used, is found in the turns kept before.
	for i := range turns {
		if p := s.cfg.OnCall.Find(turns[i].Who); p != nil {
			turns[i].Who = p.Name
		}
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

// feedTime is how long one feed may take to read and parse: a feed is
// outside input, read one schedule after another, and one that holds a CPU
// mustn't hold the rest. Replaced in tests.
var feedTime = time.Minute

// readFeeds reads each schedule's feed once, one at a time, and keeps its
// turns: a feed that fails changes nothing kept.
func (s *Server) readFeeds(ctx context.Context) {
	for _, sc := range s.cfg.OnCall.Schedules {
		if ctx.Err() != nil {
			return
		}
		now := s.now()
		fctx, cancel := context.WithTimeout(ctx, feedTime)
		f, err := s.cfg.OnCall.Fetch(fctx, sc, s.cfg.OnCallClient, "lapilli/"+s.cfg.Version, now)
		cancel()
		changed := false
		if err == nil {
			// The turns kept of an event passed over stay as they were.
			from, turns := speaksFrom(f, now), clipTurns(f.Turns, now)
			if len(f.Held) > 0 && len(turns) > 0 {
				var kept []verdict.RotaTurn
				if kept, err = s.l.Turns(ctx, sc.Name); err == nil {
					turns = oncall.Hold(kept, turns, f.Held, from)
				}
			}
			if err == nil {
				changed, err = s.l.PutTurns(ctx, sc.Name, from, turns)
			}
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

// speaksFrom is from when a feed's turns replace those kept: its earliest
// event, a cancelled one too (a turn removed at its head is gone), and no
// earlier than the span it is read over, so an event from long ago can't
// take the turns kept before it away.
func speaksFrom(f *oncall.Feed, now time.Time) time.Time {
	from := f.From
	if len(f.Turns) > 0 && (from.IsZero() || f.Turns[0].From.Before(from)) {
		from = f.Turns[0].From
	}
	if floor := now.Add(-oncall.Back); from.Before(floor) {
		from = floor
	}
	return from
}

// clipTurns are the turns from the span a feed is read over on: one that
// began before it begins there.
func clipTurns(turns []verdict.RotaTurn, now time.Time) []verdict.RotaTurn {
	floor := now.Add(-oncall.Back)
	out := make([]verdict.RotaTurn, 0, len(turns))
	for _, t := range turns {
		if !t.To.After(floor) {
			continue
		}
		if t.From.Before(floor) {
			t.From = floor
		}
		out = append(out, t)
	}
	return out
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

// onCallShown is each schedule's last read as a status says it: the
// leader's, which reads the feeds, on a replica that follows; with its turns
// as this replica keeps them, read from the ledger.
func (s *Server) onCallShown(st sharedStatus) []onCallRead {
	own := s.onCallView()
	if !s.follows() || st.OnCall == nil {
		return own
	}
	for i, v := range own {
		if r, ok := st.OnCall[v.Name]; ok {
			own[i].At, own[i].OK, own[i].Err, own[i].Events, own[i].Notes = r.At, r.OK, r.Err, r.Events, r.Notes
		}
	}
	return own
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
