package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// Changing a rule kept in Grafana, through its provisioning API: only to
// --grafana, with --changes-grafana-token-file's token, following no
// redirect. A rule another tool keeps (provisioned from files, or through
// the API by a tool that keeps it so) is refused: the change would be
// undone by it, or fight it.

// grafanaUID is what a Grafana rule's uid is: it goes in a path.
var grafanaUID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

// grafanaClient follows no redirect: a write goes where it was told, or
// nowhere.
var grafanaClient = &http.Client{Timeout: 30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// grafanaCall asks Grafana's provisioning API.
func (s *Server) grafanaCall(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	if s.cfg.ChangesGrafanaToken == "" {
		return 0, nil, errors.New("Grafana's rules are changed with the token in --changes-grafana-token-file, which isn't given")
	}
	b, err := os.ReadFile(s.cfg.ChangesGrafanaToken)
	if err != nil {
		return 0, nil, fmt.Errorf("--changes-grafana-token-file: %w", err)
	}
	var in io.Reader
	if body != nil {
		in = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(s.cfg.Grafana, "/")+path, in)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(b)))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "lapilli/"+s.cfg.Version)
	if method != http.MethodGet {
		// The rule stays editable in Grafana's UI: Lapilli doesn't keep it.
		req.Header.Set("X-Disable-Provenance", "true")
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := grafanaClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("Grafana: %w", err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("Grafana: %w", err)
	}
	if resp.StatusCode/100 == 3 {
		return resp.StatusCode, nil, fmt.Errorf("Grafana answered %s %s with a redirect, which isn't followed", method, path)
	}
	return resp.StatusCode, out, nil
}

// grafanaFailed is an answer from Grafana that isn't what was asked for.
func grafanaFailed(code int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return fmt.Errorf("Grafana: HTTP %d: %s", code, verdict.Clean(msg))
}

// grafanaRule reads a rule from Grafana: as its API gives it, and as its
// provisioning file reads, to hash. gone is a rule Grafana hasn't.
func (s *Server) grafanaRule(ctx context.Context, uid string) (raw map[string]any, rule rules.Rule, gone bool, err error) {
	path := "/api/v1/provisioning/alert-rules/" + url.PathEscape(uid)
	code, body, err := s.grafanaCall(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, rule, false, err
	}
	if code == http.StatusNotFound {
		return nil, rule, true, nil
	}
	if code != http.StatusOK {
		return nil, rule, false, grafanaFailed(code, body)
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, rule, false, fmt.Errorf("Grafana: the rule %s: %w", uid, err)
	}
	if got, _ := raw["uid"].(string); got != uid {
		return nil, rule, false, fmt.Errorf("Grafana answered for the rule %s with another", uid)
	}
	code, body, err = s.grafanaCall(ctx, http.MethodGet, path+"/export?format=yaml", nil)
	if err != nil {
		return nil, rule, false, err
	}
	if code != http.StatusOK {
		return nil, rule, false, grafanaFailed(code, body)
	}
	rs, err := rules.Parse(body)
	if err != nil {
		return nil, rule, false, fmt.Errorf("Grafana: the rule %s: %w", uid, err)
	}
	for _, r := range rs {
		if r.UID == uid {
			return raw, r, false, nil
		}
	}
	return nil, rule, false, fmt.Errorf("Grafana's export of the rule %s holds no rule of that uid", uid)
}

// provenanceRefusal is why a rule kept by another tool isn't changed.
func provenanceRefusal(raw map[string]any) error {
	switch p, _ := raw["provenance"].(string); p {
	case "":
		return nil
	case "file":
		return refusal{"Grafana keeps this rule from provisioning files: change it where the files are kept"}
	default:
		return refusal{fmt.Sprintf("another tool keeps this rule in Grafana (provenance %q): change it there", p)}
	}
}

// writable is a rule as Grafana gave it, ready to be written back: without
// what Grafana sets itself.
func writable(raw map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range raw {
		switch k {
		case "id", "updated", "provenance":
		default:
			out[k] = v
		}
	}
	return out
}

// grafanaApply applies an approved proposal in Grafana: the rule read
// again and compared with the one proposed against, kept as it was, then
// changed by grafanaWrite. A refusal says why it can't be.
func (s *Server) grafanaApply(ctx context.Context, p ledger.Proposal, step ledger.Step) error {
	ev := proposalEvidence(p)
	uid := ""
	if ev.Edit != nil {
		uid = ev.Edit.UID
	}
	if !grafanaUID.MatchString(uid) {
		return refusal{"the proposal names no Grafana rule"}
	}
	path := "/api/v1/provisioning/alert-rules/" + url.PathEscape(uid)
	raw, rule, gone, err := s.grafanaRule(ctx, uid)
	if err != nil {
		return err
	}
	hash := hashOf(p, rule)
	if p.Kind == ledger.KindUndo {
		return s.grafanaUndo(ctx, p, step, uid, raw, hash, gone)
	}
	switch {
	case gone:
		return refusal{"the rule is gone from Grafana"}
	case hash != p.BeforeHash:
		return refusal{"the rule changed in Grafana since it was proposed: propose it again"}
	}
	if err := provenanceRefusal(raw); err != nil {
		return err
	}
	backup, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	step.Backup = string(backup)
	switch p.Kind {
	case ledger.KindDelete:
		return s.grafanaWrite(ctx, p, step, uid, http.MethodDelete, path, nil)
	case ledger.KindFor, ledger.KindDisable, ledger.KindSays:
		w := writable(raw)
		switch p.Kind {
		case ledger.KindFor:
			w["for"] = verdict.Duration(ev.For)
		case ledger.KindDisable:
			w["isPaused"] = true
		default:
			// One annotation, set or taken out: Grafana's own (a dashboard's
			// and a panel's uid) and the rest stay as they are. The text is
			// checked again as it was when proposed.
			if ev.Edit == nil || !slices.Contains(saysKeys, ev.Edit.Annotation) {
				return refusal{"the proposal names no annotation Lapilli changes"}
			}
			if !ev.Edit.Unset {
				if err := saidOK(ev.Edit.Annotation, ev.Edit.Value, rules.TemplateGrafana); err != nil {
					return refusal{err.Error()}
				}
			}
			a, _ := w["annotations"].(map[string]any)
			if a == nil {
				a = map[string]any{}
			}
			if ev.Edit.Unset {
				delete(a, ev.Edit.Annotation)
			} else {
				a[ev.Edit.Annotation] = ev.Edit.Value
			}
			w["annotations"] = a
		}
		b, err := json.Marshal(w)
		if err != nil {
			return err
		}
		return s.grafanaWrite(ctx, p, step, uid, http.MethodPut, path, b)
	}
	return refusal{"Grafana's thresholds are changed by a person, in Grafana"}
}

// grafanaUndo writes back the rule an applied proposal changed, as it was
// kept: made again, when it was deleted. The rule must be as the change
// left it.
func (s *Server) grafanaUndo(ctx context.Context, p ledger.Proposal, step ledger.Step, uid string, raw map[string]any, hash string, gone bool) error {
	var was map[string]any
	if err := json.Unmarshal([]byte(p.Backup), &was); err != nil || was == nil {
		return refusal{"the rule as it was isn't kept, so it can't be written back"}
	}
	if u, _ := was["uid"].(string); u != uid {
		return refusal{"the rule as it was kept is another rule"}
	}
	body, err := json.Marshal(writable(was))
	if err != nil {
		return err
	}
	if p.BeforeHash == "" { // it was deleted
		if !gone {
			return refusal{"a rule with its uid is in Grafana again"}
		}
		return s.grafanaWrite(ctx, p, step, uid, http.MethodPost, "/api/v1/provisioning/alert-rules", body)
	}
	switch {
	case gone:
		return refusal{"the rule is gone from Grafana"}
	case hash != p.BeforeHash:
		return refusal{"the rule changed in Grafana since the change: undo it in Grafana"}
	}
	if err := provenanceRefusal(raw); err != nil {
		return err
	}
	return s.grafanaWrite(ctx, p, step, uid, http.MethodPut, "/api/v1/provisioning/alert-rules/"+url.PathEscape(uid), body)
}

// grafanaWrite sends a change to Grafana. The pause is looked at once more
// just before; then the proposal is kept as applying, with the rule as it
// was and counted against the limits, before anything is sent, so that
// whatever Grafana answers, or doesn't, the change is neither lost nor sent
// twice. What became of it is read back from Grafana, not taken from its
// answer: a delete can be done and still answer 504.
func (s *Server) grafanaWrite(ctx context.Context, p ledger.Proposal, step ledger.Step, uid, method, path string, body []byte) error {
	if s.changesPaused() {
		return errPaused
	}
	step.Applied = true
	p, err := s.l.Move(ctx, p.ID, []string{ledger.StateApproved}, ledger.StateApplying, ledger.Proposer, step)
	if err != nil {
		return err
	}
	code, out, err := s.grafanaCall(ctx, method, path, body)
	var sent error
	switch {
	case err != nil:
		sent = err
	case code/100 != 2:
		sent = grafanaFailed(code, out)
	}
	return s.grafanaSettle(ctx, p, uid, sent)
}

// settleApplying reads back the changes sent to Grafana and not yet read
// back, and says what became of each.
func (s *Server) settleApplying(ctx context.Context) error {
	ps, err := s.l.Proposals(ctx, ledger.ProposalQuery{States: []string{ledger.StateApplying}, Limit: 100})
	if err != nil {
		return err
	}
	for _, p := range ps {
		ev := proposalEvidence(p)
		uid := ""
		if ev.Edit != nil {
			uid = ev.Edit.UID
		}
		if err := s.grafanaSettle(ctx, p, uid, nil); err != nil {
			return err
		}
	}
	return nil
}

// notReadBack is why a change sent to Grafana is still applying.
const notReadBack = "sent to Grafana, but not read back yet: it is read again each round, and never sent again"

// grafanaSettle reads back the rule a change was sent to, and says what
// became of the change: applied, when the rule is no longer as it was;
// failed, when it is; still applying, with why, when it can't be read. sent
// is Grafana's answer to the change, when it was an error.
func (s *Server) grafanaSettle(ctx context.Context, p ledger.Proposal, uid string, sent error) error {
	var now rules.Rule
	gone := false
	err := errors.New("the proposal names no Grafana rule")
	if grafanaUID.MatchString(uid) {
		_, now, gone, err = s.grafanaRule(ctx, uid)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		why := notReadBack
		if sent != nil {
			why += "; Grafana answered: " + sent.Error()
		}
		s.log.Warn("a change sent to Grafana isn't read back yet", "proposal", p.ID, "err", err)
		if p.Reason == "" || sent != nil {
			if _, err := s.l.Move(ctx, p.ID, []string{ledger.StateApplying}, ledger.StateApplying, ledger.Proposer, ledger.Step{Reason: clip(why, 500)}); err != nil && !errors.Is(err, ledger.ErrMoved) {
				return err
			}
		}
		return nil
	}
	deleting := p.Kind == ledger.KindDelete
	var changed bool
	switch {
	case p.BeforeHash == "": // an undo of a deletion: made again
		changed = !gone
	case deleting:
		changed = gone
	case gone:
		_, err := s.l.Move(ctx, p.ID, []string{ledger.StateApplying}, ledger.StateFailed, ledger.Proposer, ledger.Step{Reason: "the rule is gone from Grafana"})
		if errors.Is(err, ledger.ErrMoved) {
			return nil
		}
		return err
	default:
		changed = hashOf(p, now) != p.BeforeHash
	}
	if !changed {
		why := "Grafana answered that it took the change, but the rule reads back as it was"
		if sent != nil {
			why = sent.Error()
		}
		_, err := s.l.Move(ctx, p.ID, []string{ledger.StateApplying}, ledger.StateFailed, ledger.Proposer, ledger.Step{Reason: clip(why, 500)})
		if errors.Is(err, ledger.ErrMoved) {
			return nil
		}
		return err
	}
	step := ledger.Step{}
	if !gone {
		step.AfterHash = hashOf(p, now)
	}
	switch {
	case sent != nil:
		step.Reason = clip("Grafana answered with an error, but the rule reads back changed: "+sent.Error(), 500)
	case p.Reason != "":
		step.Reason = "read back applied, after it couldn't be at first"
	}
	if _, err := s.l.Move(ctx, p.ID, []string{ledger.StateApplying}, ledger.StateApplied, ledger.Proposer, step); err != nil {
		if errors.Is(err, ledger.ErrMoved) {
			return nil
		}
		return err
	}
	if p.Kind == ledger.KindUndo {
		if _, err := s.l.Move(ctx, p.Follows, []string{ledger.StateApplied}, ledger.StateUndone, ledger.Proposer, ledger.Step{}); err != nil && !errors.Is(err, ledger.ErrMoved) {
			return err
		}
	}
	return nil
}
