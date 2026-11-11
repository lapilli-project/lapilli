package serve

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
)

// Tasks kept in step with a tracker (docs/design/trackers.md): on the
// incidents port, a job holding the tasks token reads tasks and sets their
// link and status, and does nothing else.

// trackerPage is how many tasks a tracker job is given at a time, at most.
const trackerPage = 200

// trackerBody is how much a tracker job's request may send: a comment of
// 10,000 characters, as JSON.
const trackerBody = 64 << 10

// tasksToken is the tasks token, as the incidents port takes it: none of the
// others' files may hold the same.
func (s *Server) tasksToken() token {
	return token{s.cfg.TasksToken, ledger.TaskTracker, "--tasks-token-file",
		[]string{s.cfg.IncidentsToken, s.cfg.FiringsToken, s.cfg.NotifyToken, s.cfg.ChangesToken}, &s.tasksWarned, trackerBody, s.tasksRate, s.tasksBusy}
}

// trackerPaths adds what the tasks token may do to the incidents port.
func (s *Server) trackerPaths(mux *http.ServeMux) {
	if s.cfg.TasksToken == "" {
		return
	}
	tk := s.tasksToken()
	mux.HandleFunc("GET /api/v1/tasks", s.withToken(tk, s.trackerTasks))
	mux.HandleFunc("PATCH /api/v1/tasks/{id}", s.withToken(tk, s.trackerPatch))
	mux.HandleFunc("GET /api/v1/tasks/{id}/comments", s.withToken(tk, s.trackerComments))
	mux.HandleFunc("POST /api/v1/tasks/{id}/comments", s.withToken(tk, s.trackerComment))
}

// trackerComments answers GET /api/v1/tasks/{id}/comments for a tracker
// job: the task's comments, people's and those copied in from its issue
// (origin tracker), the earliest first (docs/design/inputs.md).
func (s *Server) trackerComments(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	if _, err := s.l.TaskByID(r.Context(), id); err != nil {
		s.oops(w, r, err)
		return
	}
	cs, err := s.l.Comments(r.Context(), "task", strconv.FormatInt(id, 10))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if cs == nil {
		cs = []ledger.Comment{}
	}
	writeJSON(w, struct {
		Comments []ledger.Comment `json:"comments"`
	}{cs})
}

// trackerComment answers POST /api/v1/tasks/{id}/comments for a tracker
// job: a comment from the task's issue, with its text, its author and when
// as the tracker says, and the tracker's id for it, kept once. Nothing else
// is taken.
func (s *Server) trackerComment(w http.ResponseWriter, r *http.Request) {
	var c struct {
		Text   string    `json:"text"`
		Author string    `json:"author"`
		Ref    string    `json:"ref"`
		At     time.Time `json:"at"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		if mb := (*http.MaxBytesError)(nil); errors.As(err, &mb) {
			http.Error(w, fmt.Sprintf("a tracker job sends %d KB at most", trackerBody>>10), http.StatusRequestEntityTooLarge)
			return
		}
		s.oops(w, r, bad("want JSON with text, author, ref and at: %v", err))
		return
	}
	kept, err := s.l.CopyComment(r.Context(), pathID(r), c.Text, c.Author, c.Ref, c.At)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if kept {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
	}
	writeJSON(w, map[string]bool{"kept": kept})
}

// trackerTask is a task as a tracker job is given it: what an issue is made
// from, who set its status and when, the team's name, and where its page is
// when --external-url says.
type trackerTask struct {
	ledger.TrackerTask
	Team string `json:"team,omitempty"`
	URL  string `json:"url,omitempty"`
}

// trackerTasks answers GET /api/v1/tasks for a tracker job: linked=true or
// false (false unless given), status as a list (open,doing unless given),
// changed after updated_after (an RFC 3339 time) when given, the tasks by
// id after after, limit at a time; next is where to go on from, when there
// may be more.
func (s *Server) trackerTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tq := ledger.TrackerQuery{Status: []string{"open", "doing"}, Limit: trackerPage}
	switch q.Get("linked") {
	case "", "false":
	case "true":
		tq.Linked = true
	default:
		s.oops(w, r, bad("linked is true or false"))
		return
	}
	if v := q.Get("status"); v != "" {
		tq.Status = nil
		for _, st := range strings.Split(v, ",") {
			if !slices.Contains(ledger.TaskStatuses, st) {
				s.oops(w, r, bad("status %q: want some of open, doing, done, dropped, comma-separated", st))
				return
			}
			tq.Status = append(tq.Status, st)
		}
	}
	if v := q.Get("updated_after"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			s.oops(w, r, bad("%s %q: want an RFC 3339 time, like 2026-11-02T00:00:00Z", "updated_after", v))
			return
		}
		tq.UpdatedAfter = t
	}
	if v := q.Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			s.oops(w, r, bad("after is a task's id"))
			return
		}
		tq.After = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > trackerPage {
			s.oops(w, r, bad("limit is 1 to %d", trackerPage))
			return
		}
		tq.Limit = n
	}
	ts, err := s.l.TrackerTasks(r.Context(), tq)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	o, err := s.owners(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	out := struct {
		Tasks []trackerTask `json:"tasks"`
		Next  int64         `json:"next,omitempty"`
	}{Tasks: []trackerTask{}}
	for _, t := range ts {
		tt := trackerTask{TrackerTask: t}
		for _, team := range o.teams {
			if team.ID == t.TeamID {
				tt.Team = team.Name
			}
		}
		if s.cfg.ExternalURL != "" {
			tt.URL = fmt.Sprintf("%s/tasks/%d", strings.TrimRight(s.cfg.ExternalURL, "/"), t.ID)
		}
		out.Tasks = append(out.Tasks, tt)
	}
	if len(ts) == tq.Limit {
		out.Next = ts[len(ts)-1].ID
	}
	writeJSON(w, out)
}

// trackerPatch answers PATCH /api/v1/tasks/{id} for a tracker job: link and
// status, and nothing else.
func (s *Server) trackerPatch(w http.ResponseWriter, r *http.Request) {
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		if mb := (*http.MaxBytesError)(nil); errors.As(err, &mb) {
			http.Error(w, fmt.Sprintf("a tracker job sends %d KB at most", trackerBody>>10), http.StatusRequestEntityTooLarge)
			return
		}
		s.oops(w, r, bad("want JSON: %v", err))
		return
	}
	var link, status *string
	for k, v := range raw {
		var x string
		if err := json.Unmarshal(v, &x); err != nil {
			s.oops(w, r, bad("%s is a string", k))
			return
		}
		switch k {
		case "link":
			link = &x
		case "status":
			status = &x
		default:
			s.oops(w, r, bad("a tracker job sets a task's link and status, not %s", k))
			return
		}
	}
	if link == nil && status == nil {
		s.oops(w, r, bad("give link, status or both"))
		return
	}
	id, err := s.l.TrackTask(r.Context(), pathID(r), link, status, s.user(r))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	got, err := s.l.TaskByID(r.Context(), id)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	writeJSON(w, got)
}
