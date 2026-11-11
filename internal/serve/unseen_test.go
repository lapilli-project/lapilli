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

// A Grafana rule that fires on a query error says, on its page, that its
// replay can't show those firings: in English and in Korean.
func TestRuleReplayedInPart(t *testing.T) {
	s, rules := server(t, nil)
	os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte(`apiVersion: 1
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
`), 0o644)
	ctx := context.Background()
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	s.ready.Store(true)
	h := s.Handler()
	for lang, want := range map[string]string{
		"en": "Replayed in part: it fires when a query fails, too",
		"ko": "일부만 백테스트했어요: 쿼리가 실패해도 울리는 규칙인데",
	} {
		r := httptest.NewRequest(http.MethodGet, "/rules/Errors", nil)
		r.Header.Set("Accept-Language", lang)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: %d, want %q in\n%s", lang, w.Code, want, w.Body.String())
		}
	}
}
