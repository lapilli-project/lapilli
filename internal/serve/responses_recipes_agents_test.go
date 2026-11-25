package serve

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pagerDutyAgent runs pagerduty.jq on one resolved incident whose resolve
// log entry has an agent of the type, and gives what the page came to.
func pagerDutyAgent(t *testing.T, agentType string) (resolvedBy string, resolved bool, unknown int) {
	t.Helper()
	dir := t.TempDir()
	var alerts struct {
		Alerts []json.RawMessage `json:"alerts"`
	}
	if err := json.Unmarshal(responseFixture(t, "pagerduty-alerts-1.json"), &alerts); err != nil {
		t.Fatal(err)
	}
	linked, _ := json.Marshal(map[string]any{"id": "Q1", "alerts": alerts.Alerts[:1]})
	logs, _ := json.Marshal(map[string]any{"id": "Q1", "log_entries": []map[string]any{
		{"type": "resolve_log_entry", "created_at": "2026-11-20T03:20:00Z", "agent": map[string]any{"id": "A", "type": agentType}},
	}})
	files := map[string]string{
		"incidents.json": `[{"id":"Q1","incident_number":1,"created_at":"2026-11-20T03:12:00Z","status":"resolved","urgency":"high"}]`,
		"linked.json":    string(linked),
		"logs.json":      string(logs),
	}
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("jq", "-c", "-L", responseRecipes, "-L", responseRecipes+"../incidents", "-f", responseRecipes+"pagerduty.jq",
		"--argjson", "from", "1795132800", "--argjson", "to", "1795219200", "--arg", "urgency", "high",
		"--slurpfile", "linked", filepath.Join(dir, "linked.json"), "--slurpfile", "logs", filepath.Join(dir, "logs.json"), filepath.Join(dir, "incidents.json"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var got struct {
		Responses []struct {
			Resolved   string `json:"resolved"`
			ResolvedBy string `json:"resolved_by"`
		} `json:"responses"`
		Said struct {
			Unknown int `json:"unknown"`
		} `json:"said"`
	}
	if err := json.Unmarshal(out, &got); err != nil || len(got.Responses) != 1 {
		t.Fatalf("%v: %s", err, out)
	}
	r := got.Responses[0]
	return r.ResolvedBy, r.Resolved != "", got.Said.Unknown
}

// Every type of agent PagerDuty's OpenAPI document gives a log entry: a user
// is a person; a service, or an integration by any of the concrete types its
// reference carries (events_api_v2_inbound_integration_reference for
// Alertmanager's events), is an alert; any other leaves the resolve out,
// said by a count.
func TestPagerDutyResponsesAgentTypes(t *testing.T) {
	parallel(t)
	needTools(t)
	person := []string{"user_reference"}
	alert := []string{
		"service_reference", "technical_service_reference", "integration_reference", "outbound_integration_reference", "inbound_integration_reference",
		"aws_cloudwatch_inbound_integration_reference", "cloudkick_inbound_integration_reference",
		"event_transformer_api_inbound_integration_reference", "generic_email_inbound_integration_reference",
		"generic_events_api_inbound_integration_reference", "keynote_inbound_integration_reference",
		"nagios_inbound_integration_reference", "pingdom_inbound_integration_reference",
		"sql_monitor_inbound_integration_reference", "events_api_v2_inbound_integration_reference",
	}
	other := []string{"bot_user_reference", "business_service_reference", "team_reference", ""}
	for _, c := range []struct {
		types   []string
		by      string
		unknown int
	}{{person, "person", 0}, {alert, "alert", 0}, {other, "", 1}} {
		for _, typ := range c.types {
			by, resolved, unknown := pagerDutyAgent(t, typ)
			if by != c.by || resolved != (c.by != "") || unknown != c.unknown {
				t.Errorf("agent %q: resolved_by %q (resolved %v, %d unknown), want %q (%d unknown)", typ, by, resolved, unknown, c.by, c.unknown)
			}
		}
	}
}

// A log whose next page has no cursor is said by a count: stderr carries
// counts only, never an alert's id.
func TestResponsesRecipesSayCountsNotAlertIDs(t *testing.T) {
	parallel(t)
	needTools(t)
	const cloud = "35273b54-3f06-40d2-880f-dd28cf8daafa"
	for _, c := range []struct {
		script, idFmt, alerts1, alerts2, logs1, logs2, logs3 string
		env                                                  func(url string) []string
		base                                                 string
	}{
		{"jsm.sh", "a1000000-0000-4000-8000-%012d", "jsm-alerts-1.json", "jsm-alerts-2.json", "jsm-logs-1a.json", "jsm-logs-2.json", "jsm-logs-3.json",
			func(u string) []string {
				return []string{"JSM_API=" + u, "JSM_CLOUD_ID=" + cloud, "JIRA_EMAIL=ana@example.com", "JIRA_TOKEN_FILE=" + tokenFile(t, "jsm-secret"), "JSM_QUERY=tag: prometheus"}
			}, "/jsm/ops/api/" + cloud + "/v1"},
		{"compass.sh", "c1000000-0000-4000-8000-%012d", "compass-alerts-1.json", "compass-alerts-2.json", "compass-logs-1a.json", "compass-logs-2.json", "compass-logs-3.json",
			func(u string) []string {
				return []string{"COMPASS_API=" + u, "COMPASS_CLOUD_ID=" + cloud, "COMPASS_EMAIL=ana@example.com", "COMPASS_TOKEN_FILE=" + tokenFile(t, "compass-secret")}
			}, ""},
	} {
		secret := strings.TrimSuffix(c.script, ".sh") + "-secret"
		stand := &opsStandIn{t: t,
			auth: func(r *http.Request) bool {
				u, p, ok := r.BasicAuth()
				return ok && u == "ana@example.com" && p == secret
			},
			alerts: func(offset string) []byte {
				if offset == "100" {
					return responseFixture(t, c.alerts2)
				}
				return responseFixture(t, c.alerts1)
			},
			logs: func(alert, after string) []byte {
				switch {
				case alert == fmt.Sprintf(c.idFmt, 1) && after == "":
					// Its next page, with no cursor in the link.
					b := responseFixture(t, c.logs1)
					return []byte(strings.Replace(string(b), "?after=", "?page=", 1))
				case alert == fmt.Sprintf(c.idFmt, 2) && after == "":
					return responseFixture(t, c.logs2)
				case alert == fmt.Sprintf(c.idFmt, 3) && after == "":
					return responseFixture(t, c.logs3)
				case alert == fmt.Sprintf(c.idFmt, 5) && after == "":
					return responseFixture(t, "jsm-logs-5.json")
				}
				return nil
			}}
		srv := httptest.NewServer(stand.handler(c.base))
		env := append(c.env(srv.URL), "SINCE="+responsesPeriod, "UNTIL="+responsesUntil)
		_, said := runResponses(t, c.script, env...)
		srv.Close()
		if !strings.Contains(said, "1 alerts whose log gives a next page without a cursor") {
			t.Errorf("%s: no count of it: %q", c.script, said)
		}
		for n := 1; n <= 7; n++ {
			if strings.Contains(said, fmt.Sprintf(c.idFmt, n)) {
				t.Errorf("%s: stderr has an alert id: %q", c.script, said)
			}
		}
	}
}
