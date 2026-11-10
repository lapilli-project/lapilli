package serve

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
)

// Firings recorded elsewhere, copied in through the incidents port with the
// firings token (docs/design/recorded.md).

// maxFiringsBody is how much one write of firings may send.
const maxFiringsBody = 16 << 20

// replaying reports whether there are rules to replay: lapilli serve can
// run on firings copied in alone, with no rules and no Prometheus.
func (s *Server) replaying() bool {
	return !s.cfg.Alerts && (s.cfg.RulesPath != "" || s.cfg.Loaded || s.cfg.Grafana != "")
}

// running reports whether there are runs to make: rules to replay, the
// ALERTS series, or --impact.
func (s *Server) running() bool {
	return s.replaying() || s.cfg.Alerts || s.cfg.Impact != ""
}

// written is what a write of firings did, as the API answers it.
type written struct {
	Source string    `json:"source"`
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	ledger.Recorded
	Changed bool `json:"changed"`
}

// apiPutFirings takes a source's firings that start in [from, to), one a
// line as --firings reads them, in place of those the ledger has.
func (s *Server) apiPutFirings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	when := func(name string) (time.Time, error) {
		v := q.Get(name)
		if v == "" {
			return time.Time{}, bad("say which period the firings are of: from and to, as RFC 3339 times")
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, bad("%s %q: want an RFC 3339 time, like 2026-11-02T00:00:00Z", name, v)
		}
		return t, nil
	}
	from, err := when("from")
	if err != nil {
		s.oops(w, r, err)
		return
	}
	to, err := when("to")
	if err != nil {
		s.oops(w, r, err)
		return
	}
	source := r.PathValue("source")
	if !ledger.ValidSource(source) {
		s.oops(w, r, bad("a source is lowercase letters, digits and dashes, 40 at most, such as cloudwatch-prod"))
		return
	}
	// Read whole first: cut off at the limit, the last line would read as
	// a broken one.
	body, err := io.ReadAll(r.Body)
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		json.NewEncoder(w).Encode(map[string]string{"error": "a write is 16 MB at most: send a shorter period"})
		return
	case err != nil:
		s.oops(w, r, err)
		return
	}
	fs, err := firing.ReadJSONL(bytes.NewReader(body))
	if err != nil {
		s.oops(w, r, bad("want firings, one JSON object a line, each with rule and start: %v", err))
		return
	}
	o, err := s.options(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	got, err := s.l.PutFirings(r.Context(), source, from, to, fs, s.user(r), o)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if got.Changed() {
		s.log.Info("firings copied in", "source", source, "from", from, "to", to, "firings", got.Firings, "added", got.Added, "removed", got.Removed)
	}
	writeJSON(w, written{source, from.UTC(), to.UTC(), got, got.Changed()})
}

// apiSources lists the sources firings are recorded from.
func (s *Server) apiSources(w http.ResponseWriter, r *http.Request) {
	srcs, err := s.l.Sources(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if srcs == nil {
		srcs = []ledger.Source{}
	}
	writeJSON(w, srcs)
}
