package serve

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// A person's verdict on one firing (docs/design/judging.md, item 3): that
// it is unrelated to what it caught, an editor's of every team or an
// admin's, which turns that firing alone black; and whether it needed
// someone to act, a team's editor's too, which changes no stone. The forms
// and the API go part by part: a team's editor's write leaves "unrelated"
// as it is stored, and the ledger refuses one that would change it. Each
// write is counted, thirty an hour a person, and is about the last 14
// days' firings but for an admin's.

func (s *Server) verdictRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /firings/{id}/verdict", s.teamWrite(s.postVerdict))
	mux.HandleFunc("POST /firings/verdict", s.teamWrite(s.bulkVerdict))
	mux.HandleFunc("GET /api/v1/verdicts", s.apiVerdicts)
	mux.HandleFunc("PUT /api/v1/verdicts", s.teamWrite(s.apiPutVerdict))
	mux.HandleFunc("PUT /api/v1/verdicts/bulk", s.teamWrite(s.apiPutVerdicts))
	mux.HandleFunc("DELETE /api/v1/verdicts", s.teamWrite(s.apiDeleteVerdict))
}

// postVerdict is a firing's verdict form: unrelated (yes, no, or left out
// to leave it), its note, action (yes, no, none, or left out), its note;
// undo (unrelated, action or both) to undo instead. The firing's series and
// start are held to its own (ledger.ErrFiringChanged).
func (s *Server) postVerdict(w http.ResponseWriter, r *http.Request) {
	k, err := formFiring(r, r.PathValue("id"))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	o, err := s.options(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if undo := r.PostFormValue("undo"); undo != "" {
		part := map[string]string{"unrelated": "unrelated", "action": "action", "both": ""}
		p, ok := part[undo]
		if !ok {
			s.oops(w, r, bad("undo is unrelated, action or both"))
			return
		}
		if err := s.l.DeleteVerdict(r.Context(), k, p, s.verdictBy(r), o); err != nil {
			s.oops(w, r, err)
			return
		}
		back(w, r, s.verdictBack(r, k))
		return
	}
	c, err := verdictForm(r)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if _, err := s.l.SetVerdict(r.Context(), k, c, s.verdictBy(r), o); err != nil {
		s.oops(w, r, err)
		return
	}
	back(w, r, s.verdictBack(r, k))
}

// verdictForm reads what a verdict form says: each part given, the rest
// left out.
func verdictForm(r *http.Request) (ledger.VerdictChange, error) {
	var c ledger.VerdictChange
	if _, ok := r.PostForm["unrelated"]; ok {
		switch r.PostFormValue("unrelated") {
		case "yes":
			c.Unrelated = valPtr(true)
		case "no":
			c.Unrelated = valPtr(false)
		default:
			return c, bad("unrelated is yes or no")
		}
	}
	if v, ok := r.PostForm["unrelated_note"]; ok {
		c.UnrelatedNote = valPtr(strings.TrimSpace(v[0]))
	}
	if _, ok := r.PostForm["action"]; ok {
		a := r.PostFormValue("action")
		if a == "none" {
			a = ""
		}
		c.Action = &a
	}
	if v, ok := r.PostForm["action_note"]; ok {
		c.ActionNote = valPtr(strings.TrimSpace(v[0]))
	}
	return c, nil
}

func valPtr[T any](v T) *T { return &v }

// verdictBack is where a verdict form goes back to: back, when it is a
// page of this server's, else the firing's day.
func (s *Server) verdictBack(r *http.Request, k ledger.FiringKey) string {
	return safeBack(r.PostFormValue("back"), "/firings?day="+k.Start.In(s.cfg.Loc).Format("2006-01-02"))
}

// bulkVerdict is "needed action" answered of the firings picked, 100 at
// most, as "These were real" takes them: one write, all or nothing,
// counted once.
func (s *Server) bulkVerdict(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	vs, ok := picked(r.PostForm["firing"])
	if !ok {
		s.oops(w, r, bad("at most %d firings at a time: pick fewer", maxBulk))
		return
	}
	var ks []ledger.FiringKey
	for _, v := range vs {
		k, err := firingRef(v)
		if err != nil {
			s.oops(w, r, err)
			return
		}
		ks = append(ks, k)
	}
	if len(ks) == 0 {
		back(w, r, bulkBack(r.PostFormValue("back"), "/firings", "action", bulkResult{None: true}))
		return
	}
	action := r.PostFormValue("action")
	if action == "none" {
		action = ""
	}
	var note *string
	if v, ok := r.PostForm["action_note"]; ok && strings.TrimSpace(v[0]) != "" {
		note = valPtr(strings.TrimSpace(v[0]))
	}
	o, err := s.options(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	n, err := s.l.SetVerdicts(r.Context(), ks, action, note, s.verdictBy(r), o)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	back(w, r, bulkBack(r.PostFormValue("back"), "/firings", "action", bulkResult{Done: n, Already: len(ks) - n}))
}

// apiFiring is a firing as the API picks it: by its alert and start, as a
// report gives it.
type apiFiring struct {
	Rule   string            `json:"rule"`
	Labels map[string]string `json:"labels"`
	Start  time.Time         `json:"start"`
}

func (f apiFiring) firingKey() (ledger.FiringKey, error) {
	if f.Rule == "" || f.Start.IsZero() {
		return ledger.FiringKey{}, bad("a firing is its rule, labels and start, as a report gives them")
	}
	return ledger.FiringKey{Rule: f.Rule, Labels: f.Labels, Start: f.Start}, nil
}

// apiVerdictIn is PUT /api/v1/verdicts' body: the firing, and each part
// given; a part left out is left as it is.
type apiVerdictIn struct {
	apiFiring
	Unrelated     *bool   `json:"unrelated"`
	UnrelatedNote *string `json:"unrelated_note"`
	Action        *string `json:"action"`
	ActionNote    *string `json:"action_note"`
}

// apiVerdictPart is one part of a verdict, as the API gives it.
type apiVerdictPart struct {
	By   string    `json:"by"`
	At   time.Time `json:"at"`
	Note string    `json:"note,omitempty"`
}

// apiUnrelated is the incidents a firing was said to be unrelated to, each
// as it was then.
type apiUnrelated struct {
	apiVerdictPart
	Incidents []ledger.UnrelatedTo `json:"incidents"`
}

type apiAction struct {
	apiVerdictPart
	Answer string `json:"answer"`
}

// apiVerdict is a verdict as the API gives it: its firing (its end when it
// was said), each part with who said it, when and their note, and whether
// it moved, why, and whether it is on a firing now.
type apiVerdict struct {
	apiFiring
	End       *time.Time    `json:"end"`
	Unrelated *apiUnrelated `json:"unrelated"`
	Action    *apiAction    `json:"action"`
	Moved     string        `json:"moved,omitempty"`
	OnFiring  bool          `json:"on_firing"`
}

func verdictOut(v ledger.Verdict) apiVerdict {
	out := apiVerdict{apiFiring: apiFiring{Rule: v.Rule, Labels: v.Labels, Start: v.Start.UTC()}, OnFiring: v.Firing != 0}
	if out.Labels == nil {
		out.Labels = map[string]string{}
	}
	if !v.End.IsZero() {
		end := v.End.UTC()
		out.End = &end
	}
	if len(v.Unrelated) > 0 {
		out.Unrelated = &apiUnrelated{apiVerdictPart{v.UnrelatedBy, v.UnrelatedAt.UTC(), v.UnrelatedNote}, v.Unrelated}
	}
	if v.Action != "" {
		out.Action = &apiAction{apiVerdictPart{v.ActionBy, v.ActionAt.UTC(), v.ActionNote}, v.Action}
	}
	if format, args := ledger.MovedText(v.Moved); format != "" {
		out.Moved = tr("en", format, args...)
	}
	return out
}

// apiPutVerdict is PUT /api/v1/verdicts: a firing's verdict, part by part.
func (s *Server) apiPutVerdict(w http.ResponseWriter, r *http.Request) {
	var in apiVerdictIn
	if err := readJSON(r, &in); err != nil {
		s.oops(w, r, err)
		return
	}
	k, err := in.firingKey()
	if err != nil {
		s.oops(w, r, err)
		return
	}
	o, err := s.options(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	v, err := s.l.SetVerdict(r.Context(), k, ledger.VerdictChange{Unrelated: in.Unrelated, UnrelatedNote: in.UnrelatedNote, Action: in.Action, ActionNote: in.ActionNote},
		s.verdictBy(r), o)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if v.ID == 0 {
		// Written as it was: what is kept.
		got, err := s.l.VerdictsIn(r.Context(), k.Start, k.Start.Add(time.Second), func(rule string) bool { return rule == k.Rule })
		if err != nil {
			s.oops(w, r, err)
			return
		}
		for _, x := range got {
			if sameLabels(x.Labels, k.Labels) {
				v = x
			}
		}
	}
	out := verdictOut(v)
	if v.ID == 0 {
		// Nothing said of it any more: the firing, with nothing.
		out = apiVerdict{apiFiring: in.apiFiring, OnFiring: true}
	}
	writeJSON(w, out)
}

func sameLabels(a, b map[string]string) bool {
	x, _ := json.Marshal(verdict.KeptLabels(a))
	y, _ := json.Marshal(verdict.KeptLabels(b))
	return string(x) == string(y)
}

// apiVerdictsIn is PUT /api/v1/verdicts/bulk's body.
type apiVerdictsIn struct {
	Firings    []apiFiring `json:"firings"`
	Action     *string     `json:"action"`
	ActionNote *string     `json:"action_note"`
}

// apiPutVerdicts is PUT /api/v1/verdicts/bulk: one answer to "needed
// action" of up to 100 firings, all or nothing, one write.
func (s *Server) apiPutVerdicts(w http.ResponseWriter, r *http.Request) {
	var in apiVerdictsIn
	if err := readJSON(r, &in); err != nil {
		s.oops(w, r, err)
		return
	}
	if in.Action == nil {
		s.oops(w, r, bad("give action: yes, no, or empty for none; unrelated isn't taken in bulk"))
		return
	}
	if len(in.Firings) > ledger.MaxBulkVerdicts {
		s.oops(w, r, bad("at most %d firings at a time: pick fewer", ledger.MaxBulkVerdicts))
		return
	}
	var ks []ledger.FiringKey
	for _, f := range in.Firings {
		k, err := f.firingKey()
		if err != nil {
			s.oops(w, r, err)
			return
		}
		ks = append(ks, k)
	}
	o, err := s.options(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	n, err := s.l.SetVerdicts(r.Context(), ks, *in.Action, in.ActionNote, s.verdictBy(r), o)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	writeJSON(w, map[string]int{"firings": len(ks), "changed": n})
}

// apiVerdictDel is DELETE /api/v1/verdicts' body: the firing, and the part
// undone (unrelated, action, or both when left out).
type apiVerdictDel struct {
	apiFiring
	Part string `json:"part"`
}

func (s *Server) apiDeleteVerdict(w http.ResponseWriter, r *http.Request) {
	var in apiVerdictDel
	if err := readJSON(r, &in); err != nil {
		s.oops(w, r, err)
		return
	}
	k, err := in.firingKey()
	if err != nil {
		s.oops(w, r, err)
		return
	}
	o, err := s.options(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if err := s.l.DeleteVerdict(r.Context(), k, in.Part, s.verdictBy(r), o); err != nil {
		s.oops(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// verdictsLongest is how long a period GET /api/v1/verdicts gives at most.
const verdictsLongest = 31 * 24 * time.Hour

// apiVerdicts is GET /api/v1/verdicts?from=&to=: the verdicts of the
// firings that started in the period, 31 days at most, the last seven when
// not given; a viewer's.
func (s *Server) apiVerdicts(w http.ResponseWriter, r *http.Request) {
	from, to, err := s.period(r, verdictsLongest)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	vs, err := s.l.VerdictsIn(r.Context(), from, to, nil)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	out := make([]apiVerdict, 0, len(vs))
	for _, v := range vs {
		out = append(out, verdictOut(v))
	}
	writeJSON(w, map[string]any{"from": from.UTC(), "to": to.UTC(), "verdicts": out})
}
