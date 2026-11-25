package serve

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// A token writes its own source's pages and no other's, so one source can't
// write over another's; two sources holding the same token take none.
func TestResponsesTokenPerSource(t *testing.T) {
	parallel(t)
	pdTok, opsTok := "a-pagerduty-token-long-enough", "an-ops-token-long-enough-too"
	pdFile, opsFile := tokenFile(t, pdTok), tokenFile(t, opsTok)
	s, _ := server(t, func(c *Config) {
		c.IncidentsListen, c.ResponsesTokens = "127.0.0.1:0", map[string]string{"pd": pdFile, "ops": opsFile}
	})
	port := s.IncidentsHandler()
	send := func(source, tok string) int {
		r := httptest.NewRequest("PUT", "/api/v1/responses/"+source+respWeek, strings.NewReader(`{"responses": []}`))
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		port.ServeHTTP(w, r)
		return w.Code
	}
	for _, c := range []struct {
		source, tok string
		want        int
	}{
		{"pd", pdTok, http.StatusOK},
		{"ops", opsTok, http.StatusOK},
		{"pd", opsTok, http.StatusUnauthorized},
		{"ops", pdTok, http.StatusUnauthorized},
		{"other", pdTok, http.StatusUnauthorized},
	} {
		if got := send(c.source, c.tok); got != c.want {
			t.Errorf("%s with %s's token: %d, want %d", c.source, c.tok, got, c.want)
		}
	}
	// The same token in both files: neither source is written.
	os.WriteFile(opsFile, []byte(pdTok+"\n"), 0o600)
	if got := send("pd", pdTok); got != http.StatusUnauthorized {
		t.Errorf("a token two sources hold: %d", got)
	}
	if got := send("ops", pdTok); got != http.StatusUnauthorized {
		t.Errorf("a token two sources hold, the other's: %d", got)
	}
}
