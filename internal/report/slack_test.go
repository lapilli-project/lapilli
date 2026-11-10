package report

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// A one-line message carries names from outside, a rule's or a task's: it
// is posted as written, with no formatting, no links made and no preview.
func TestSlackMessagePlain(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &got); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	file := filepath.Join(t.TempDir(), "hook")
	if err := os.WriteFile(file, []byte(srv.URL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SlackMessage(context.Background(), file, `ana made task #1, "Tune <https://evil.example|click> *URGENT* <!channel>"`); err != nil {
		t.Fatal(err)
	}
	if got["mrkdwn"] != false || got["unfurl_links"] != false || got["unfurl_media"] != false {
		t.Errorf("formatting and previews should be off: %v", got)
	}
	if want := `ana made task #1, "Tune &lt;https://evil.example|click&gt; *URGENT* &lt;!channel&gt;"`; got["text"] != want {
		t.Errorf("text = %q, want %q", got["text"], want)
	}
}
