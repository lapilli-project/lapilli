package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A rule on a cloud's own monitoring says, on its page, that its replay
// reads its metrics as filled in since, in English and in Korean; a Grafana
// rule isn't marked for its query errors, as nearly every one fires on
// them.
func TestRuleReplayedInPart(t *testing.T) {
	page := func(file, body, rule string) map[string]string {
		t.Helper()
		s, rules := server(t, nil)
		os.Remove(filepath.Join(rules, "shop.yaml"))
		os.WriteFile(filepath.Join(rules, file), []byte(body), 0o644)
		ctx := context.Background()
		raw := ""
		if err := s.look(ctx, &raw); err != nil {
			t.Fatal(err)
		}
		s.jobs(ctx, true)
		s.ready.Store(true)
		out := map[string]string{}
		for _, lang := range []string{"en", "ko"} {
			r := httptest.NewRequest(http.MethodGet, "/rules/"+rule, nil)
			r.Header.Set("Accept-Language", lang)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != 200 {
				t.Fatalf("%s %s: %d", rule, lang, w.Code)
			}
			out[lang] = w.Body.String()
		}
		return out
	}
	cloud := page("alarms.json", `{"MetricAlarms": [{"AlarmName": "Errors", "EvaluationInterval": 60,
	  "EvaluationCriteria": {"PromQLCriteria": {"Query": "errors > 5", "PendingPeriod": 120}}}]}`, "Errors")
	for lang, want := range map[string]string{
		"en": "Replayed in part: it reads a cloud",
		"ko": "일부만 백테스트했어요: 클라우드 자체 메트릭을 읽는 규칙이에요",
	} {
		if !strings.Contains(cloud[lang], want) {
			t.Errorf("%s: want %q in\n%s", lang, want, cloud[lang])
		}
	}
	grafana := page("shop.yaml", `apiVersion: 1
groups:
  - orgId: 1
    name: shop
    folder: f
    interval: 1m
    rules:
      - uid: e1
        title: Errors
        condition: B
        noDataState: OK
        execErrState: Error
        data:
          - {refId: A, datasourceUid: prom, model: {expr: errors > 5, instant: true}}
          - refId: B
            datasourceUid: __expr__
            model: {type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [0]}}]}
`, "Errors")
	if strings.Contains(grafana["en"], "Replayed in part") {
		t.Errorf("a Grafana rule marked for its errors:\n%s", grafana["en"])
	}
}
