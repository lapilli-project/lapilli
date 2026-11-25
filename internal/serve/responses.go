package serve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/response"
	"lapilli.dev/internal/verdict"
)

// What people did when a rule fired (docs/design/responses.md), as serve
// takes it: the pages a paging tool sent, from --responses (read again each
// run) and PUT /api/v1/responses/SOURCE, and the changes made to the alerts'
// workloads, read with each run (--rollouts, rolloutsFor). The ledger works
// each firing's response and usefulness out from what it keeps.

// maxResponsesBody is how much one write of pages may send, as a write of
// firings (maxFiringsBody): 16 MiB, the file's bound too
// (response.MaxFile).
const maxResponsesBody = 16 << 20

// responsesOn reports whether what people did when a rule fired is read: a
// file, tokens that PUT pages, the history, or the deploys file.
func (s *Server) responsesOn() bool {
	return s.cfg.Responses != "" || len(s.cfg.ResponsesTokens) > 0 || s.cfg.Rollouts || s.cfg.Deploys != ""
}

// responseWrite is what PUT /api/v1/responses/SOURCE answers.
type responseWrite struct {
	Source  string    `json:"source"`
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Pages   int       `json:"pages"`
	Added   int       `json:"added"`
	Changed int       `json:"changed"`
	Removed int       `json:"removed"`
	// Differs says the write changed what the ledger keeps.
	Differs bool `json:"changed_ledger"`
}

// apiPutResponses takes what a paging tool says of the pages a source sent
// in [from, to): the whole {"responses": [...]} as --responses reads it, in
// place of the source's pages sent in that period, as PUT /api/v1/firings/
// SOURCE replaces a period's firings.
func (s *Server) apiPutResponses(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	when := func(name string) (time.Time, error) {
		v := q.Get(name)
		if v == "" {
			return time.Time{}, bad("say which period the pages were sent in: from and to, as RFC 3339 times")
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
	if !to.After(from) {
		s.oops(w, r, bad("to is after from"))
		return
	}
	source := r.PathValue("source")
	switch {
	case !ledger.ValidSource(source):
		s.oops(w, r, bad("a source is lowercase letters, digits and dashes, 40 at most, such as pagerduty-prod"))
		return
	case source == ledger.ResponsesFile:
		s.oops(w, r, bad("%s is the source --responses is read as: it owns its pages, name another", source))
		return
	}
	// JSON only, read a page at a time and held to the bounds as it goes
	// (response.ParseJSON), not the whole body parsed into a YAML tree.
	pages, err := response.ParseJSON(r.Body, maxResponsesBody)
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig) || errors.Is(err, response.ErrTooLarge):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		json.NewEncoder(w).Encode(map[string]string{"error": "a write is 16 MB at most: send a shorter period"})
		return
	case err != nil:
		s.oops(w, r, bad("want JSON, {\"responses\": [...]}, the pages as --responses reads them: %v", err))
		return
	}
	o, err := s.options(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	got, err := s.l.PutResponses(r.Context(), source, from, to, pages, o)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	if got.Differs() {
		s.log.Info("pages copied in", "source", source, "from", from, "to", to, "pages", got.Pages, "added", got.Added, "changed", got.Changed, "removed", got.Removed)
	}
	writeJSON(w, responseWrite{source, from.UTC(), to.UTC(), got.Pages, got.Added, got.Changed, got.Removed, got.Differs()})
}

// syncResponses reads --responses again, and keeps what it holds: when the
// file is as it was, nothing more is read or written.
func (s *Server) syncResponses(ctx context.Context, o verdict.Options) error {
	b, err := readResponsesFile(s.cfg.Responses)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	digest := hex.EncodeToString(sum[:])
	if same, err := s.l.ResponsesUnchanged(ctx, ledger.ResponsesFile, digest); err != nil || same {
		return err
	}
	pages, err := response.Parse(b, s.cfg.Responses)
	if err != nil {
		return err
	}
	got, err := s.l.SyncResponses(ctx, ledger.ResponsesFile, digest, pages, o)
	if err != nil {
		return err
	}
	if got.Differs() {
		s.log.Info("responses file read again", "pages", got.Pages, "added", got.Added, "changed", got.Changed, "removed", got.Removed)
	}
	return nil
}

// readResponsesFile reads a regular file, response.MaxFile bytes at most
// (more is refused when it is parsed).
func readResponsesFile(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New(path + ": not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, response.MaxFile+1))
}

// rolloutsFor reads, from the history, the changes made to the workloads of
// the namespaces the firings fs have, and which workloads the pods they name
// belong to, over [from, to] (response.ReadRollouts). Firings that weren't on
// then are passed over.
func (s *Server) rolloutsFor(parent context.Context, from, to time.Time, fs []firing.Firing) (*response.Rollouts, error) {
	var namespaces []string
	var pods []response.Pod
	seen := map[string]bool{}
	for _, x := range fs {
		if !x.Active(from, to) {
			continue
		}
		ns := x.Labels["namespace"]
		if ns == "" {
			continue
		}
		if !seen[ns] {
			seen[ns] = true
			namespaces = append(namespaces, ns)
		}
		if pod := x.Labels["pod"]; pod != "" {
			pods = append(pods, response.Pod{Namespace: ns, Name: pod})
		}
	}
	if len(namespaces) == 0 {
		return nil, nil
	}
	slices.Sort(namespaces)
	slices.SortFunc(pods, func(a, b response.Pod) int {
		if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	pods = slices.Compact(pods)
	ctx, cancel := context.WithTimeout(parent, s.cfg.Timeout)
	defer cancel()
	r, err := response.ReadRollouts(ctx, s.h, from, to, s.cfg.Step, namespaces, pods)
	if err != nil {
		return nil, fmt.Errorf("reading rollouts: %w", err)
	}
	return r, nil
}

// readRollouts is rolloutsFor, and a read that fails is kept as a span the
// next run reads once more (retryRollouts): until then the firings in it have
// no change known, and are neither called changed nor not useful for want of
// one.
func (s *Server) readRollouts(ctx context.Context, from, to time.Time, fs []firing.Firing) *response.Rollouts {
	r, err := s.rolloutsFor(ctx, from, to, fs)
	if err != nil {
		s.fail("rollouts", err)
		if err := s.l.RolloutFailed(context.WithoutCancel(ctx), from, to); err != nil {
			s.log.Warn("a span whose rollouts couldn't be read wasn't kept to be read again", "from", from, "to", to, "err", err)
		}
		return nil
	}
	return r
}

// retryRollouts reads once more the spans earlier runs couldn't read the
// changes of, with the firings the ledger has in each, and keeps what it
// finds (judging them again). A span that fails again is let go: it is tried
// once, as the history that failed twice is more than a hiccup.
func (s *Server) retryRollouts(ctx context.Context, o verdict.Options) {
	spans, err := s.l.RolloutRetries(ctx)
	if err != nil {
		s.log.Warn("the spans whose rollouts weren't read couldn't be listed", "err", err)
		return
	}
	for _, sp := range spans {
		from, to := sp[0], sp[1]
		fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: from, To: to})
		if err == nil {
			plain := make([]firing.Firing, len(fs))
			for i, f := range fs {
				plain[i] = f.Firing
			}
			var r *response.Rollouts
			if r, err = s.rolloutsFor(ctx, from, to, plain); err == nil {
				err = s.l.KeepRollouts(context.WithoutCancel(ctx), r, s.now(), o)
			}
		}
		if err != nil {
			s.log.Warn("rollouts read again for a span a run couldn't read them for failed too, and aren't tried again", "from", from, "to", to, "err", err)
		}
		if err := s.l.RolloutRetried(context.WithoutCancel(ctx), from); err != nil {
			s.log.Warn("a span whose rollouts were read again wasn't dropped", "from", from, "err", err)
		}
	}
}

// responseLabel is, by kind of response, what the pages call it, in English:
// the filter's choices, and the chip that takes the filter away.
var responseLabel = map[string]string{
	verdict.ResponseChanged: "Change made", verdict.ResponseAcknowledged: "Acknowledged", verdict.ResponseUnanswered: "Unanswered",
	verdict.ResponseRoutine: "Routine change", verdict.ResponseNone: "No response",
}

// responseAsked is the response a page is asked to pick firings by,
// ?response=changed, acknowledged, unanswered, routine or none; "" for any,
// and for what isn't one of them.
func responseAsked(q url.Values) string {
	if v := q.Get("response"); slices.Contains(ledger.ResponseKinds, v) {
		return v
	}
	return ""
}

// responsesShown reports whether what was done about the firings is shown by
// the pages: the ledger keeps pages or changes read from the history, or
// rules are found noisy by what was done and it keeps deploys
// (ledger.ResponseInputs.Shown).
func (s *Server) responsesShown(ctx context.Context, o verdict.Options) (bool, error) {
	in, err := s.l.ResponseInputs(ctx)
	return err == nil && in.Shown(o), err
}

// ResponsesFiles are the files of the tokens that write pages, in the order
// of their sources; none when no source has one. A token's file holds no
// other token's.
func (c Config) ResponsesFiles() []string {
	var out []string
	for _, source := range slices.Sorted(maps.Keys(c.ResponsesTokens)) {
		out = append(out, c.ResponsesTokens[source])
	}
	return out
}
