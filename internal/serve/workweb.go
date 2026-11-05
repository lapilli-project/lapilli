package serve

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
)

// The pages and API for what people keep: teams, notes on rules, quiet
// windows, tasks and comments.

func (s *Server) workRoutes(mux *http.ServeMux, page func(string, func(*http.Request) (any, error)) http.HandlerFunc) {
	mux.HandleFunc("GET /teams", page("teams", s.teamsPage))
	mux.HandleFunc("GET /windows", page("windows", s.windowsPage))
	mux.HandleFunc("GET /windows/{id}", page("window", s.windowPage))
	mux.HandleFunc("GET /tasks", page("tasks", s.tasksPage))
	mux.HandleFunc("GET /tasks/{id}", page("task", s.taskPage))

	mux.HandleFunc("POST /teams", s.admin(s.postTeam))
	mux.HandleFunc("POST /teams/{id}/delete", s.admin(s.deleteTeam))
	mux.HandleFunc("POST /notes", s.write(s.postNote))
	mux.HandleFunc("POST /windows", s.write(s.postWindow))
	mux.HandleFunc("POST /windows/{id}/delete", s.write(s.deleteWindow))
	mux.HandleFunc("POST /tasks", s.write(s.postTask))
	mux.HandleFunc("POST /tasks/{id}/delete", s.write(s.deleteTask))
	mux.HandleFunc("POST /comments", s.write(s.postComment))
	mux.HandleFunc("POST /comments/{id}/delete", s.write(s.deleteComment))

	mux.HandleFunc("GET /api/v1/teams", s.cached(s.apiTeams))
	mux.HandleFunc("GET /api/v1/teams/{id}", s.cached(s.apiTeam))
	mux.HandleFunc("GET /api/v1/windows/{id}", s.cached(s.apiWindow))
	mux.HandleFunc("GET /api/v1/comments/{id}", s.cached(s.apiComment))
	mux.HandleFunc("POST /api/v1/teams", s.admin(s.apiPutTeam))
	mux.HandleFunc("PUT /api/v1/teams/{id}", s.admin(s.apiPutTeam))
	mux.HandleFunc("DELETE /api/v1/teams/{id}", s.admin(s.apiDelete(func(r *http.Request, id int64) error { return s.l.DeleteTeam(r.Context(), id, s.user(r)) })))
	mux.HandleFunc("GET /api/v1/notes", s.cached(s.apiNotes))
	mux.HandleFunc("PUT /api/v1/notes", s.write(s.apiPutNote))
	mux.HandleFunc("GET /api/v1/windows", s.cached(s.apiWindows))
	mux.HandleFunc("POST /api/v1/windows", s.write(s.apiPutWindow))
	mux.HandleFunc("PUT /api/v1/windows/{id}", s.write(s.apiPutWindow))
	mux.HandleFunc("DELETE /api/v1/windows/{id}", s.write(s.apiDelete(func(r *http.Request, id int64) error {
		o, err := s.options(r.Context())
		if err != nil {
			return err
		}
		defer s.poke() // to carry on judging what is left
		return s.l.DeleteWindow(r.Context(), id, s.user(r), o)
	})))
	mux.HandleFunc("GET /api/v1/tasks", s.cached(s.apiTasks))
	mux.HandleFunc("GET /api/v1/tasks/{id}", s.cached(s.apiTask))
	mux.HandleFunc("POST /api/v1/tasks", s.write(s.apiPutTask))
	mux.HandleFunc("PUT /api/v1/tasks/{id}", s.write(s.apiPutTask))
	mux.HandleFunc("PATCH /api/v1/tasks/{id}", s.write(s.apiPatchTask))
	mux.HandleFunc("DELETE /api/v1/tasks/{id}", s.write(s.apiDelete(func(r *http.Request, id int64) error { return s.l.DeleteTask(r.Context(), id, s.user(r)) })))
	mux.HandleFunc("GET /api/v1/comments", s.cached(s.apiComments))
	mux.HandleFunc("POST /api/v1/comments", s.write(s.apiPutComment))
	mux.HandleFunc("PUT /api/v1/comments/{id}", s.write(s.apiPutComment))
	mux.HandleFunc("DELETE /api/v1/comments/{id}", s.write(s.apiDelete(func(r *http.Request, id int64) error { return s.l.DeleteComment(r.Context(), id, s.user(r)) })))
}

// The views.

type teamLine struct {
	ledger.Team
	Labels string
	Rules  int // rules it owns
	Open   int // its open tasks
}

type teamsView struct {
	Lines []teamLine
	Edit  *teamLine
}

func (s *Server) teamsPage(r *http.Request) (any, error) {
	ctx := r.Context()
	o, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	open, err := s.l.OpenTasks(ctx)
	if err != nil {
		return nil, err
	}
	var v teamsView
	for _, t := range o.teams {
		l := teamLine{Team: t, Labels: labelText(t.Labels)}
		for _, id := range o.rule {
			if id == t.ID {
				l.Rules++
			}
		}
		l.Open = open[t.ID]
		v.Lines = append(v.Lines, l)
		if r.URL.Query().Get("edit") == strconv.FormatInt(t.ID, 10) {
			v.Edit = &v.Lines[len(v.Lines)-1]
		}
	}
	return v, nil
}

type windowsView struct {
	Lines []ledger.Window
	Now   time.Time
}

func (s *Server) windowsPage(r *http.Request) (any, error) {
	now := s.now()
	ws, err := s.l.Windows(r.Context(), now.Add(-4*7*24*time.Hour), now.Add(366*24*time.Hour))
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(ws)-1; i < j; i, j = i+1, j-1 {
		ws[i], ws[j] = ws[j], ws[i]
	}
	return windowsView{ws, now}, nil
}

type windowView struct {
	ledger.Window
	Labels    string
	UntilText string
	Excused   []firingLine
}

func (s *Server) windowPage(r *http.Request) (any, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return nil, ledger.ErrNotFound
	}
	w, err := s.l.WindowByID(r.Context(), id)
	if err != nil {
		return nil, err
	}
	v := windowView{Window: w, Labels: labelText(w.Labels)}
	if !w.Until.IsZero() {
		v.UntilText = w.Until.In(s.cfg.Loc).Format("2006-01-02")
	}
	// Those it excused, the latest first: of a repeating window, over each
	// repeat so far.
	to := w.End
	if w.Every != "" {
		to = s.now()
		if !w.Until.IsZero() && w.Until.In(s.cfg.Loc).AddDate(0, 0, 8).Before(to) {
			to = w.Until.In(s.cfg.Loc).AddDate(0, 0, 8)
		}
	}
	fs, err := s.l.Firings(r.Context(), ledger.FiringQuery{From: w.Start, To: to.Add(time.Second), Excused: true, Limit: 500})
	if err != nil {
		return nil, err
	}
	for _, f := range fs {
		if w.Excuses(f.Firing) { // and not only another window overlapping it
			v.Excused = append(v.Excused, s.firingLine(f))
		}
	}
	return v, nil
}

type taskLine struct {
	ledger.Task
	Team    string
	DueText string // the day it is due, in the UI's zone
	Overdue bool
}

// taskForm is the form to make or change a task.
type taskForm struct {
	Task            ledger.Task
	Due             string
	Teams           []ledger.Team
	Kinds, Statuses []string
}

func (s *Server) taskForm(t ledger.Task, teams []ledger.Team) taskForm {
	f := taskForm{Task: t, Teams: teams, Kinds: ledger.Kinds, Statuses: ledger.TaskStatuses}
	if !t.Due.IsZero() {
		f.Due = t.Due.In(s.cfg.Loc).Format("2006-01-02")
	}
	return f
}

type tasksView struct {
	Status string // open (open and doing), done, dropped, all
	Team   teamFilter
	Lines  []taskLine
	Form   taskForm // to make one
}

func (s *Server) taskLines(o *owners, ts []ledger.Task) []taskLine {
	var out []taskLine
	today := s.l.Midnight(s.l.Day(s.now()))
	for _, t := range ts {
		l := taskLine{Task: t, Team: o.byID[t.TeamID].Name, Overdue: !t.Due.IsZero() && t.Due.Before(today) && (t.Status == "open" || t.Status == "doing")}
		if !t.Due.IsZero() {
			l.DueText = t.Due.In(s.cfg.Loc).Format("2006-01-02")
		}
		out = append(out, l)
	}
	return out
}

func (s *Server) tasksPage(r *http.Request) (any, error) {
	ctx := r.Context()
	o, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	q := r.URL.Query()
	v := tasksView{Status: q.Get("status")}
	tq := ledger.TaskQuery{Limit: 300}
	switch v.Status {
	case "done", "dropped":
		tq.Status = []string{v.Status}
	case "all":
	default:
		v.Status = "open"
		tq.Status = []string{"open", "doing"}
	}
	var team *ledger.Team
	v.Team, team = s.teamFilter(o, q.Get("team"))
	if team != nil {
		tq.TeamID = team.ID
	}
	ts, err := s.l.Tasks(ctx, tq)
	if err != nil {
		return nil, err
	}
	v.Lines = s.taskLines(o, ts)
	nt := ledger.Task{Rule: q.Get("rule"), Incident: q.Get("incident"), Kind: q.Get("kind"), Title: q.Get("title")}
	if team != nil {
		nt.TeamID = team.ID
	} else if nt.Rule != "" {
		nt.TeamID = o.rule[nt.Rule]
	}
	v.Form = s.taskForm(nt, o.teams)
	return v, nil
}

type taskView struct {
	taskLine
	Form     taskForm
	Comments commentsView
}

func (s *Server) taskPage(r *http.Request) (any, error) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return nil, ledger.ErrNotFound
	}
	t, err := s.l.TaskByID(ctx, id)
	if err != nil {
		return nil, err
	}
	o, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	v := taskView{taskLine: s.taskLines(o, []ledger.Task{t})[0], Form: s.taskForm(t, o.teams)}
	v.Comments, err = s.commentsView(r, "task", fmt.Sprint(id))
	return v, err
}

// commentsView is what was said about something, and a form to say more.
type commentsView struct {
	On, Key, Back string
	List          []ledger.Comment
	Me            string
	ReadOnly      bool
	Loc           *time.Location
}

func (s *Server) commentsView(r *http.Request, on, key string) (commentsView, error) {
	cs, err := s.l.Comments(r.Context(), on, key)
	return commentsView{On: on, Key: key, Back: r.URL.RequestURI(), List: cs, Me: s.user(r), ReadOnly: s.cfg.ReadOnly, Loc: s.cfg.Loc}, err
}

// The forms.

func formInt(r *http.Request, name string) int64 {
	n, _ := strconv.ParseInt(r.PostFormValue(name), 10, 64)
	return n
}

// written answers a form: back where it came from, or to the error.
func (s *Server) written(w http.ResponseWriter, r *http.Request, err error, to string) {
	if err != nil {
		s.oops(w, r, err)
		return
	}
	back(w, r, safeBack(to, "/"))
}

func (s *Server) postTeam(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	labels, err := parseLabels(r.PostFormValue("labels"))
	if err == nil {
		_, err = s.l.PutTeam(r.Context(), ledger.Team{ID: formInt(r, "id"), Name: r.PostFormValue("name"),
			Rules: strings.TrimSpace(r.PostFormValue("rules")), Labels: labels, Note: strings.TrimSpace(r.PostFormValue("note"))}, s.user(r))
	}
	s.written(w, r, err, "/teams")
}

func (s *Server) deleteTeam(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	s.written(w, r, s.l.DeleteTeam(r.Context(), id, s.user(r)), "/teams")
}

func (s *Server) postNote(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	rule := r.PostFormValue("rule")
	err := s.l.PutNote(r.Context(), ledger.Note{Rule: rule, TeamID: formInt(r, "team"), Status: r.PostFormValue("status"),
		Runbook: strings.TrimSpace(r.PostFormValue("runbook")), Note: strings.TrimSpace(r.PostFormValue("note"))}, s.user(r))
	s.written(w, r, err, "/rules/"+url.PathEscape(rule))
}

func (s *Server) postWindow(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	start, err := s.parseTime(r.PostFormValue("start"))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	end, err := s.parseTime(r.PostFormValue("end"))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	labels, err := parseLabels(r.PostFormValue("labels"))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	o, err := s.options(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	var until time.Time
	if v := r.PostFormValue("until"); v != "" {
		if until, err = time.ParseInLocation("2006-01-02", v, s.cfg.Loc); err != nil {
			s.oops(w, r, bad("until %q: want a date like 2026-12-31", v))
			return
		}
	}
	id, err := s.l.PutWindow(r.Context(), ledger.Window{ID: formInt(r, "id"), Title: r.PostFormValue("title"), Start: start, End: end,
		Every: r.PostFormValue("every"), Until: until, Rules: strings.TrimSpace(r.PostFormValue("rules")), Labels: labels}, s.user(r), o)
	s.poke() // to carry on judging what is left
	s.written(w, r, err, fmt.Sprintf("/windows/%d", id))
}

func (s *Server) deleteWindow(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	o, err := s.options(r.Context())
	if err == nil {
		err = s.l.DeleteWindow(r.Context(), id, s.user(r), o)
		s.poke()
	}
	s.written(w, r, err, "/windows")
}

func (s *Server) postTask(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	t := ledger.Task{ID: formInt(r, "id"), Title: r.PostFormValue("title"), Kind: r.PostFormValue("kind"), Rule: strings.TrimSpace(r.PostFormValue("rule")),
		Incident: strings.TrimSpace(r.PostFormValue("incident")), TeamID: formInt(r, "team"), Assignee: strings.TrimSpace(r.PostFormValue("assignee")),
		Status: r.PostFormValue("status"), Body: strings.TrimSpace(r.PostFormValue("body")), Link: strings.TrimSpace(r.PostFormValue("link"))}
	if v := r.PostFormValue("due"); v != "" {
		d, err := time.ParseInLocation("2006-01-02", v, s.cfg.Loc)
		if err != nil {
			s.oops(w, r, bad("due %q: want a date like 2026-11-20", v))
			return
		}
		t.Due = d
	}
	// A new task about a rule is its team's, unless one is named.
	if t.ID == 0 && t.TeamID == 0 && t.Rule != "" {
		o, err := s.owners(r.Context())
		if err != nil {
			s.oops(w, r, err)
			return
		}
		t.TeamID = o.rule[t.Rule]
	}
	var id int64
	var err error
	if only := r.PostFormValue("only"); only == "status" && t.ID != 0 {
		// A status change alone keeps the rest as it is when written.
		patch, _ := json.Marshal(map[string]string{"status": t.Status})
		id, err = s.l.PatchTask(r.Context(), t.ID, patch, s.user(r))
	} else {
		id, err = s.l.PutTask(r.Context(), t, s.user(r))
	}
	to := r.PostFormValue("back")
	if to == "" {
		to = fmt.Sprintf("/tasks/%d", id)
	}
	s.written(w, r, err, to)
}

func (s *Server) deleteTask(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	s.written(w, r, s.l.DeleteTask(r.Context(), id, s.user(r)), "/tasks")
}

func (s *Server) postComment(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	_, err := s.l.PutComment(r.Context(), ledger.Comment{ID: formInt(r, "id"), On: r.PostFormValue("on"), Key: r.PostFormValue("key"),
		Body: r.PostFormValue("body")}, s.user(r))
	s.written(w, r, err, r.PostFormValue("back"))
}

func (s *Server) deleteComment(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	s.written(w, r, s.l.DeleteComment(r.Context(), id, s.user(r)), r.PostFormValue("back"))
}

// The API.

func readJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return bad("want JSON: %v", err)
	}
	return nil
}

// pathID is the ID in the path; 0, which is no row, when it isn't one.
func pathID(r *http.Request) int64 {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 0 {
		return 0
	}
	return id
}

// putID is the ID a PUT changes: ErrNotFound for none, as PUT doesn't
// make one.
func putID(r *http.Request) (int64, error) {
	if id := pathID(r); id > 0 {
		return id, nil
	}
	return 0, ledger.ErrNotFound
}

// answer writes what was made or changed: 201 with where it is, for a POST.
func answer(w http.ResponseWriter, r *http.Request, where string, v any) {
	if r.Method == http.MethodPost {
		w.Header().Set("Location", where)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
	}
	writeJSON(w, v)
}

func (s *Server) apiDelete(del func(*http.Request, int64) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := del(r, pathID(r)); err != nil {
			s.oops(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func orNoneOf[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

func (s *Server) apiTeams(w http.ResponseWriter, r *http.Request) {
	ts, err := s.l.Teams(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	writeJSON(w, orNoneOf(ts))
}

func (s *Server) apiTeam(w http.ResponseWriter, r *http.Request) {
	ts, err := s.l.Teams(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	for _, t := range ts {
		if t.ID == pathID(r) {
			writeJSON(w, t)
			return
		}
	}
	s.oops(w, r, ledger.ErrNotFound)
}

func (s *Server) apiWindow(w http.ResponseWriter, r *http.Request) {
	x, err := s.l.WindowByID(r.Context(), pathID(r))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	writeJSON(w, x)
}

func (s *Server) apiComment(w http.ResponseWriter, r *http.Request) {
	c, err := s.l.CommentByID(r.Context(), pathID(r))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	writeJSON(w, c)
}

func (s *Server) apiPutTeam(w http.ResponseWriter, r *http.Request) {
	var t ledger.Team
	if err := readJSON(r, &t); err != nil {
		s.oops(w, r, err)
		return
	}
	t.ID = 0
	if r.Method == http.MethodPut {
		var err error
		if t.ID, err = putID(r); err != nil {
			s.oops(w, r, err)
			return
		}
	}
	id, err := s.l.PutTeam(r.Context(), t, s.user(r))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	t.ID = id
	answer(w, r, fmt.Sprintf("/api/v1/teams/%d", id), t)
}

func (s *Server) apiNotes(w http.ResponseWriter, r *http.Request) {
	ns, err := s.l.Notes(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	writeJSON(w, ns)
}

func (s *Server) apiPutNote(w http.ResponseWriter, r *http.Request) {
	var n ledger.Note
	if err := readJSON(r, &n); err != nil {
		s.oops(w, r, err)
		return
	}
	if err := s.l.PutNote(r.Context(), n, s.user(r)); err != nil {
		s.oops(w, r, err)
		return
	}
	writeJSON(w, n)
}

func (s *Server) apiWindows(w http.ResponseWriter, r *http.Request) {
	from, to, err := s.period(r, 400*24*time.Hour)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	ws, err := s.l.Windows(r.Context(), from, to)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	writeJSON(w, orNoneOf(ws))
}

func (s *Server) apiPutWindow(w http.ResponseWriter, r *http.Request) {
	var x ledger.Window
	if err := readJSON(r, &x); err != nil {
		s.oops(w, r, err)
		return
	}
	x.ID = 0
	if r.Method == http.MethodPut {
		var err error
		if x.ID, err = putID(r); err != nil {
			s.oops(w, r, err)
			return
		}
	}
	o, err := s.options(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	id, err := s.l.PutWindow(r.Context(), x, s.user(r), o)
	s.poke()
	if err != nil {
		s.oops(w, r, err)
		return
	}
	got, err := s.l.WindowByID(r.Context(), id)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	answer(w, r, fmt.Sprintf("/api/v1/windows/%d", id), got)
}

func (s *Server) apiTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tq := ledger.TaskQuery{Rule: q.Get("rule"), Incident: q.Get("incident"), Limit: 500}
	if v := q.Get("status"); v != "" {
		tq.Status = strings.Split(v, ",")
	}
	if v := q.Get("team"); v != "" {
		o, err := s.owners(r.Context())
		if err != nil {
			s.oops(w, r, err)
			return
		}
		t, ok := o.team(v)
		if !ok {
			s.oops(w, r, bad("no team called %q", v))
			return
		}
		tq.TeamID = t.ID
	}
	ts, err := s.l.Tasks(r.Context(), tq)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	writeJSON(w, orNoneOf(ts))
}

func (s *Server) apiTask(w http.ResponseWriter, r *http.Request) {
	t, err := s.l.TaskByID(r.Context(), pathID(r))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	writeJSON(w, t)
}

func (s *Server) apiPutTask(w http.ResponseWriter, r *http.Request) {
	var t ledger.Task
	if err := readJSON(r, &t); err != nil {
		s.oops(w, r, err)
		return
	}
	t.ID = 0
	if r.Method == http.MethodPut {
		var err error
		if t.ID, err = putID(r); err != nil {
			s.oops(w, r, err)
			return
		}
	}
	id, err := s.l.PutTask(r.Context(), t, s.user(r))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	got, err := s.l.TaskByID(r.Context(), id)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	answer(w, r, fmt.Sprintf("/api/v1/tasks/%d", id), got)
}

// apiPatchTask changes only the fields given: a tracker puts an issue's
// link back with {"link": ...}, leaving what was changed meanwhile.
func (s *Server) apiPatchTask(w http.ResponseWriter, r *http.Request) {
	var patch json.RawMessage
	if err := readJSON(r, &patch); err != nil {
		s.oops(w, r, err)
		return
	}
	id, err := s.l.PatchTask(r.Context(), pathID(r), patch, s.user(r))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	got, err := s.l.TaskByID(r.Context(), id)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	answer(w, r, fmt.Sprintf("/api/v1/tasks/%d", id), got)
}

func (s *Server) apiComments(w http.ResponseWriter, r *http.Request) {
	cs, err := s.l.Comments(r.Context(), r.URL.Query().Get("on"), r.URL.Query().Get("key"))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	writeJSON(w, orNoneOf(cs))
}

func (s *Server) apiPutComment(w http.ResponseWriter, r *http.Request) {
	var c ledger.Comment
	if err := readJSON(r, &c); err != nil {
		s.oops(w, r, err)
		return
	}
	c.ID = 0
	if r.Method == http.MethodPut {
		var err error
		if c.ID, err = putID(r); err != nil {
			s.oops(w, r, err)
			return
		}
	}
	id, err := s.l.PutComment(r.Context(), c, s.user(r))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	c.ID = id
	answer(w, r, fmt.Sprintf("/api/v1/comments/%d", id), c)
}

// errCode is the HTTP status for an error from the ledger's writes.
func errCode(err error) (int, bool) {
	var inv ledger.ErrInvalid
	switch {
	case errors.As(err, &inv):
		return http.StatusBadRequest, true
	case errors.Is(err, ledger.ErrNotAuthor):
		return http.StatusForbidden, true
	case errors.Is(err, errBusy):
		return http.StatusServiceUnavailable, true
	}
	return 0, false
}
