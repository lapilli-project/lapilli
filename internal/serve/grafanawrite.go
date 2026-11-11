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
// changed; and read once more, to say what it is now.
func (s *Server) grafanaApply(ctx context.Context, p ledger.Proposal, step ledger.Step) (ledger.Step, error) {
	ev := proposalEvidence(p)
	uid := ""
	if ev.Edit != nil {
		uid = ev.Edit.UID
	}
	if !grafanaUID.MatchString(uid) {
		return step, refusal{"the proposal names no Grafana rule"}
	}
	path := "/api/v1/provisioning/alert-rules/" + url.PathEscape(uid)
	raw, rule, gone, err := s.grafanaRule(ctx, uid)
	if err != nil {
		return step, err
	}
	hash := hashOf(p, rule)
	if p.Kind == ledger.KindUndo {
		return s.grafanaUndo(ctx, p, step, uid, raw, hash, gone)
	}
	switch {
	case gone:
		return step, refusal{"the rule is gone from Grafana"}
	case hash != p.BeforeHash:
		return step, refusal{"the rule changed in Grafana since it was proposed: propose it again"}
	}
	if err := provenanceRefusal(raw); err != nil {
		return step, err
	}
	backup, err := json.Marshal(raw)
	if err != nil {
		return step, err
	}
	step.Backup = string(backup)
	var code int
	var body []byte
	switch p.Kind {
	case ledger.KindDelete:
		code, body, err = s.grafanaCall(ctx, http.MethodDelete, path, nil)
	case ledger.KindFor, ledger.KindDisable, ledger.KindSays:
		w := writable(raw)
		switch p.Kind {
		case ledger.KindFor:
			w["for"] = verdict.Duration(ev.For)
		case ledger.KindDisable:
			w["isPaused"] = true
		default:
			// One annotation, set or taken out: Grafana's own (a dashboard's
			// and a panel's uid) and the rest stay as they are.
			if ev.Edit == nil || !slices.Contains(saysKeys, ev.Edit.Annotation) {
				return step, refusal{"the proposal names no annotation Lapilli changes"}
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
			return step, err
		}
		code, body, err = s.grafanaCall(ctx, http.MethodPut, path, b)
	default:
		return step, refusal{"Grafana's thresholds are changed by a person, in Grafana"}
	}
	if err != nil {
		return step, err
	}
	if code/100 != 2 {
		return step, grafanaFailed(code, body)
	}
	if p.Kind != ledger.KindDelete {
		_, now, _, err := s.grafanaRule(ctx, uid)
		if err != nil {
			return step, err
		}
		step.AfterHash = hashOf(p, now)
	}
	return step, nil
}

// grafanaUndo writes back the rule an applied proposal changed, as it was
// kept: made again, when it was deleted. The rule must be as the change
// left it.
func (s *Server) grafanaUndo(ctx context.Context, p ledger.Proposal, step ledger.Step, uid string, raw map[string]any, hash string, gone bool) (ledger.Step, error) {
	var was map[string]any
	if err := json.Unmarshal([]byte(p.Backup), &was); err != nil || was == nil {
		return step, refusal{"the rule as it was isn't kept, so it can't be written back"}
	}
	if u, _ := was["uid"].(string); u != uid {
		return step, refusal{"the rule as it was kept is another rule"}
	}
	body, err := json.Marshal(writable(was))
	if err != nil {
		return step, err
	}
	var code int
	var out []byte
	if p.BeforeHash == "" { // it was deleted
		if !gone {
			return step, refusal{"a rule with its uid is in Grafana again"}
		}
		code, out, err = s.grafanaCall(ctx, http.MethodPost, "/api/v1/provisioning/alert-rules", body)
	} else {
		switch {
		case gone:
			return step, refusal{"the rule is gone from Grafana"}
		case hash != p.BeforeHash:
			return step, refusal{"the rule changed in Grafana since the change: undo it in Grafana"}
		}
		if err := provenanceRefusal(raw); err != nil {
			return step, err
		}
		code, out, err = s.grafanaCall(ctx, http.MethodPut, "/api/v1/provisioning/alert-rules/"+url.PathEscape(uid), body)
	}
	if err != nil {
		return step, err
	}
	if code/100 != 2 {
		return step, grafanaFailed(code, out)
	}
	_, now, _, err := s.grafanaRule(ctx, uid)
	if err != nil {
		return step, err
	}
	step.AfterHash = hashOf(p, now)
	return step, nil
}
