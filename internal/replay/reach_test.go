package replay

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lapilli-project/lapilli/internal/casefile"
)

// A witness that does not end is stopped, and has reached nothing, whatever it printed first; one
// that prints without end is read no further than a witness is worth; and a session whose tools are
// not there fails as a session, not as an item.
func TestAWitnessThatDoesNotEndOrDoesNotStopPrinting(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	item := func(command string) *casefile.Case {
		return &casefile.Case{Evidence: []casefile.Evidence{{Pattern: "conn-table active=40/40", Store: casefile.StoreKubernetes, Command: command}}}
	}
	session := func(c *casefile.Case) *Session {
		return &Session{Case: c, Workdir: dir, Env: map[string]string{"PATH": bin + string(os.PathListSeparator) + os.Getenv("PATH")}}
	}
	// No tool: the session cannot ask anything.
	if _, err := session(item("kubectl get pods")).Reach(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "running kubectl get pods") {
		t.Errorf("a session with no kubectl in it: %v", err)
	}
	os.WriteFile(filepath.Join(bin, "kubectl"), []byte(`#!/bin/sh
case "$1" in
  logs) echo "conn-table active=40/40"; exec sleep 30 ;;
  get) echo "conn-table active=40/40" ;;
  top) while :; do echo "0123456789012345678901234567890123456789012345678901234567890123456789"; done ;;
  describe) i=0; while [ $i -lt 40 ]; do echo "line $i of what it printed, which is long enough to be cut"; i=$((i+1)); done; exit 4 ;;
esac
`), 0o755)
	was := reachTimeout
	reachTimeout = 2 * time.Second
	defer func() { reachTimeout = was }()
	began := time.Now()
	got, err := session(item("kubectl logs cache -f")).Reach(context.Background(), nil)
	if err != nil || len(got) != 1 || got[0].Found || !strings.Contains(got[0].Said, "conn-table active=40/40") || !strings.Contains(got[0].Said, "(it had not ended after 2s, and was stopped)") || time.Since(began) > 20*time.Second {
		t.Errorf("a witness that follows a log: %+v (%v), after %v", got, err, time.Since(began))
	}
	// The same item, by a witness that ends: reached, with nothing more to say.
	if got, err := session(item("kubectl get pods")).Reach(context.Background(), nil); err != nil || !got[0].Found || got[0].Said != "" || got[0].By != "kubectl get pods" {
		t.Errorf("a witness that prints its item and ends: %+v (%v)", got, err)
	}
	// What is shown of a witness that did not reach is the end of what it printed, from the beginning of a line, and how it ended.
	if got, err := session(item("kubectl describe pod cache")).Reach(context.Background(), nil); err != nil || got[0].Found ||
		!strings.HasPrefix(got[0].Said, "… line ") || !strings.Contains(got[0].Said, "line 39 of what it printed") || strings.Contains(got[0].Said, "line 3 of") || !strings.HasSuffix(got[0].Said, "(exit status 4)") {
		t.Errorf("a witness that printed forty lines of something else: %+v (%v)", got, err)
	}
	// And one that says nothing at all is said to have said nothing.
	if got, _ := session(item("kubectl version")).Reach(context.Background(), nil); got[0].Found || got[0].Said != "(it printed nothing)" {
		t.Errorf("a witness that prints nothing: %+v", got)
	}
	for text, want := range map[string]string{"short": "short", "  padded\n": "padded", "one\ntwo\nthree": "… three", "nolinebreakhere": "… akhere"} {
		if got := lastOf(text, 6); got != want {
			t.Errorf("the end of %q, in six bytes: %q, want %q", text, got, want)
		}
	}
	// A writer that keeps the first of what it is given, and says there was more.
	kept := &most{left: 10}
	kept.Write([]byte("0123456"))
	kept.Write([]byte("789abc"))
	kept.Write([]byte("def"))
	if kept.String() != "0123456789" || !kept.over {
		t.Errorf("ten bytes kept of sixteen: %q, more: %v", kept.String(), kept.over)
	}
	exact := &most{left: 4}
	if exact.Write([]byte("0123")); exact.String() != "0123" || exact.over {
		t.Errorf("four bytes kept of four: %q, more: %v", exact.String(), exact.over)
	}
}
