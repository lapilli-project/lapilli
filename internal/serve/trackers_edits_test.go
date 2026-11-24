package serve

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// Edits followed, both ways, with every tracker (docs/design/trackers.md):
// a comment people wrote in Lapilli and changed after it was posted has
// its post changed, once; a comment from the tracker changed there after
// it was copied is taken again, once, as the tracker's; neither when
// nothing changed, nor without COMMENTS. The job's post changed in the
// tracker by someone isn't changed back by a Lapilli comment older than
// that, nor copied.
func TestTrackerRecipesCommentEdits(t *testing.T) {
	parallel(t)
	needTools(t)
	for name := range trackerAPIs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := newTrackerRig(t, name)
			f := g.f
			ctx := context.Background()
			g.mustRun(t) // tasks 1 and 2 get issues 1 and 2
			mine, err := g.s.l.PutComment(ctx, ledger.Comment{On: "task", Key: "1", Body: "Raised it to 15m."}, "ana@example.com")
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			theirs := f.comment(f.issue(1), "someone", "Looks right.")
			f.mu.Unlock()
			g.mustRun(t, "COMMENTS=true")
			// Again: nothing changed, nothing carried either way.
			out := g.mustRun(t, "COMMENTS=true")
			if f.edited() != 0 || !strings.Contains(out, "carried 0 edits to issues, took 0 edits from them") {
				t.Fatalf("edits with nothing changed: %d\n%s", f.edited(), out)
			}
			copied := func() ledger.Comment {
				t.Helper()
				cs, _ := g.s.l.Comments(ctx, "task", "1")
				for _, c := range cs {
					if c.Origin == ledger.OriginTracker {
						return c
					}
				}
				t.Fatalf("nothing copied: %+v", cs)
				return ledger.Comment{}
			}
			was := copied()

			// Changed in Lapilli, a second after it was posted.
			time.Sleep(1100 * time.Millisecond)
			if _, err := g.s.l.PutComment(ctx, ledger.Comment{ID: mine, Body: "Raised it to 30m, not 15m."}, "ana@example.com"); err != nil {
				t.Fatal(err)
			}
			// Changed in the tracker, by whoever wrote it.
			f.mu.Lock()
			f.reword(f.issue(1), theirs, "Looks right, but watch Fridays.")
			f.mu.Unlock()
			out = g.mustRun(t, "COMMENTS=true")
			f.mu.Lock()
			var post *fakeComment
			for _, c := range f.issue(1).comments {
				if c.author == trBot {
					post = c
				}
			}
			edits := f.edits
			f.mu.Unlock()
			if edits != 1 || post == nil || !strings.Contains(post.body, "> Raised it to 30m, not 15m.") ||
				!strings.HasSuffix(strings.TrimSpace(post.body), fmt.Sprintf("Kept in step with Lapilli: lapilli-comment-%d", mine)) {
				t.Errorf("the post after the edit (%d edits): %+v\n%s", edits, post, out)
			}
			now := copied()
			if now.ID != was.ID || now.Body != "Looks right, but watch Fridays." || now.By != ledger.TaskTracker || now.Author != "someone" || !now.Updated.After(was.Updated) {
				t.Errorf("the copy after the edit: %+v, was %+v\n%s", now, was, out)
			}
			if !strings.Contains(out, "carried 1 edits to issues, took 1 edits from them") {
				t.Errorf("said:\n%s", out)
			}
			// Again: each was taken once.
			out = g.mustRun(t, "COMMENTS=true")
			if f.edited() != 1 || !strings.Contains(out, "carried 0 edits to issues, took 0 edits from them") {
				t.Errorf("again: %d edits\n%s", f.edited(), out)
			}
			// The job's post changed in the tracker by someone: not changed
			// back by the Lapilli comment, older than that; not copied.
			f.mu.Lock()
			f.reword(f.issue(1), post, post.body+"\n(fixed a typo)")
			f.mu.Unlock()
			g.mustRun(t, "COMMENTS=true")
			if cs, _ := g.s.l.Comments(ctx, "task", "1"); f.edited() != 1 || len(cs) != 2 {
				t.Errorf("the job's post changed in the tracker: %d edits, %d comments", f.edited(), len(cs))
			}
			// Without COMMENTS, nothing.
			time.Sleep(1100 * time.Millisecond)
			g.s.l.PutComment(ctx, ledger.Comment{ID: mine, Body: "Raised it to 45m."}, "ana@example.com")
			g.mustRun(t)
			if f.edited() != 1 {
				t.Errorf("edited without COMMENTS")
			}
		})
	}
}

// An edit in the tracker whose text doesn't show as written is taken as
// what does, and once: Lapilli keeps its time, so it isn't sent each run.
func TestTrackerRecipesCommentEditsShown(t *testing.T) {
	parallel(t)
	needTools(t)
	g := newTrackerRig(t, "github")
	ctx := context.Background()
	g.mustRun(t)
	g.f.mu.Lock()
	c := g.f.comment(g.f.issue(1), "mallory", "fine")
	g.f.mu.Unlock()
	g.mustRun(t, "COMMENTS=true")
	g.f.mu.Lock()
	g.f.reword(g.f.issue(1), c, "pay\u202eyap")
	g.f.mu.Unlock()
	for run, want := range []string{"took 1 edits", "took 0 edits"} {
		if out := g.mustRun(t, "COMMENTS=true"); !strings.Contains(out, want) {
			t.Errorf("run %d said:\n%s", run+1, out)
		}
	}
	cs, _ := g.s.l.Comments(ctx, "task", "1")
	if len(cs) != 1 || cs[0].Body != "pay\uFFFDyap" {
		t.Errorf("copied: %+v", cs)
	}
}

// What the tasks token may do with an edit, as an attacker holding it
// would try: change a comment copied in from the tracker, by a later edit
// alone, its text kept as what shows and its time no later than now; not a
// person's comment, not another task's, not with posted beside it, not
// with an older or the same time, nothing past the bounds; nothing told.
func TestTrackerCommentEdited(t *testing.T) {
	parallel(t)
	s := trackerServer(t, func(c *Config) { c.NotifyWebhook = tokenFile(t, "https://hooks.example.com/lapilli") })
	h := s.IncidentsHandler()
	ctx := context.Background()
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	if _, err := s.l.CopyComment(ctx, 1, "Looks right.", "someone", "c-1", at); err != nil {
		t.Fatal(err)
	}
	person, err := s.l.PutComment(ctx, ledger.Comment{On: "task", Key: "1", Body: "Mine."}, "ana@example.com")
	if err != nil {
		t.Fatal(err)
	}
	cs, _ := s.l.Comments(ctx, "task", "1")
	copied := cs[0].ID
	pending, _ := s.l.Unsent(ctx)
	path := func(task, c int64) string { return fmt.Sprintf("/api/v1/tasks/%d/comments/%d", task, c) }
	later := at.Add(time.Minute).UTC().Format(time.RFC3339)
	for name, c := range map[string]struct {
		path, tok, body string
		code            int
	}{
		"no token":              {path(1, copied), "", `{"text":"x","edited":"` + later + `"}`, http.StatusUnauthorized},
		"another token":         {path(1, copied), trIncTok, `{"text":"x","edited":"` + later + `"}`, http.StatusUnauthorized},
		"a person's comment":    {path(1, person), trTasksTok, `{"text":"x","edited":"` + later + `"}`, http.StatusBadRequest},
		"another task's":        {path(2, copied), trTasksTok, `{"text":"x","edited":"` + later + `"}`, http.StatusNotFound},
		"posted beside it":      {path(1, copied), trTasksTok, `{"posted":"c-1","text":"x","edited":"` + later + `"}`, http.StatusBadRequest},
		"no time":               {path(1, copied), trTasksTok, `{"text":"x"}`, http.StatusBadRequest},
		"another field":         {path(1, copied), trTasksTok, `{"text":"x","edited":"` + later + `","author":"ana"}`, http.StatusBadRequest},
		"no text":               {path(1, copied), trTasksTok, `{"text":" ","edited":"` + later + `"}`, http.StatusBadRequest},
		"text too long":         {path(1, copied), trTasksTok, `{"text":"` + strings.Repeat("x", 10001) + `","edited":"` + later + `"}`, http.StatusBadRequest},
		"a body too large":      {path(1, copied), trTasksTok, `{"text":"` + strings.Repeat("x", 70000) + `","edited":"` + later + `"}`, http.StatusRequestEntityTooLarge},
		"a time before 1970":    {path(1, copied), trTasksTok, `{"text":"x","edited":"1969-12-31T00:00:00Z"}`, http.StatusBadRequest},
		"a time that isn't one": {path(1, copied), trTasksTok, `{"text":"x","edited":"yesterday"}`, http.StatusBadRequest},
	} {
		if code, body := tracker(t, h, "PATCH", c.path, c.tok, c.body); code != c.code && !(c.code == http.StatusUnauthorized && code == http.StatusForbidden) {
			t.Errorf("%s: %d %s, want %d", name, code, body, c.code)
		}
	}
	// The same time, or an older one: nothing changes.
	for _, when := range []time.Time{at, at.Add(-time.Minute)} {
		if code, body := tracker(t, h, "PATCH", path(1, copied), trTasksTok, `{"text":"older","edited":"`+when.UTC().Format(time.RFC3339)+`"}`); code != http.StatusOK || !strings.Contains(body, `"changed": false`) {
			t.Errorf("an edit at %s: %d %s", when, code, body)
		}
	}
	// A later one: kept, as what shows; then the same again changes nothing.
	edit := `{"text":"Looks right, \u202ebut","edited":"` + later + `"}`
	if code, body := tracker(t, h, "PATCH", path(1, copied), trTasksTok, edit); code != http.StatusOK || !strings.Contains(body, `"changed": true`) || !strings.Contains(body, `"as_sent": false`) {
		t.Fatalf("a later edit: %d %s", code, body)
	}
	if code, body := tracker(t, h, "PATCH", path(1, copied), trTasksTok, edit); code != http.StatusOK || !strings.Contains(body, `"changed": false`) {
		t.Errorf("the same again: %d %s", code, body)
	}
	// A later edit of the text kept keeps its time alone, noted as no
	// change: a tracker may move a comment's time for a reaction.
	before, _ := s.l.ChangesSince(ctx, time.Unix(0, 0), 1000)
	same := `{"text":"Looks right, \u202ebut","edited":"` + at.Add(2*time.Minute).UTC().Format(time.RFC3339) + `"}`
	if code, body := tracker(t, h, "PATCH", path(1, copied), trTasksTok, same); code != http.StatusOK || !strings.Contains(body, `"changed": false`) {
		t.Errorf("a later edit of the same text: %d %s", code, body)
	}
	if after, _ := s.l.ChangesSince(ctx, time.Unix(0, 0), 1000); len(after) != len(before) {
		t.Errorf("a later edit of the same text noted: %d changes, were %d", len(after), len(before))
	}
	c, _ := s.l.CommentByID(ctx, copied)
	if c.Body != "Looks right, \uFFFDbut" || c.Author != "someone" || c.Origin != ledger.OriginTracker || c.By != ledger.TaskTracker || !c.Updated.Equal(at.Add(2*time.Minute)) {
		t.Errorf("after the edit: %+v", c)
	}
	// A time ahead of now is now.
	if code, _ := tracker(t, h, "PATCH", path(1, copied), trTasksTok, `{"text":"ahead","edited":"2099-01-01T00:00:00Z"}`); code != http.StatusOK {
		t.Errorf("a time ahead: %d", code)
	}
	if c, _ := s.l.CommentByID(ctx, copied); c.Body != "ahead" || c.Updated.After(time.Now()) {
		t.Errorf("a time ahead kept as %+v", c)
	}
	if now, _ := s.l.Unsent(ctx); now != pending {
		t.Errorf("told: %d pending, were %d", now, pending)
	}
	// The person's comment is as it was.
	if p, _ := s.l.CommentByID(ctx, person); p.Body != "Mine." {
		t.Errorf("a person's comment changed: %+v", p)
	}
}

// A recipe of one's own written before edits were followed, its comments
// with no edited time and no t_edit (github.sh as it was): its comments
// are still kept in step both ways, once, and no edit is sent either way.
func TestTrackerRecipeOwnWithoutEdits(t *testing.T) {
	parallel(t)
	needTools(t)
	g := newTrackerRig(t, "github")
	ctx := context.Background()
	src, err := os.ReadFile(trackerRecipes + "github.sh")
	if err != nil {
		t.Fatal(err)
	}
	old := regexp.MustCompile(`(?s)\n# t_edit:.*?\n}\n`).ReplaceAllString(string(src), "\n")
	old = strings.Replace(old, ` edited: (.updated_at // .created_at),`, "", 1)
	if old == string(src) || strings.Contains(old, "t_edit") || strings.Contains(old, "edited") {
		t.Fatal("github.sh isn't as this test takes it apart")
	}
	dir := t.TempDir()
	lib, err := os.ReadFile(trackerRecipes + "lib.sh")
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{"own.sh": []byte(old), "lib.sh": lib} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	jq, _ := os.ReadFile(trackerRecipes + "github.jq")
	os.WriteFile(filepath.Join(dir, "github.jq"), jq, 0o644)
	run := func() string {
		t.Helper()
		cmd := exec.Command("sh", filepath.Join(dir, "own.sh"))
		cmd.Env = append(slices.Clone(g.env), "COMMENTS=true")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return string(out)
	}
	run()
	mine, _ := g.s.l.PutComment(ctx, ledger.Comment{On: "task", Key: "1", Body: "Raised it."}, "ana@example.com")
	g.f.mu.Lock()
	theirs := g.f.comment(g.f.issue(1), "someone", "Fine.")
	g.f.mu.Unlock()
	if out := run(); !strings.Contains(out, "posted 1 comments to issues, copied 1 comments to tasks") {
		t.Fatalf("said:\n%s", out)
	}
	time.Sleep(1100 * time.Millisecond)
	g.s.l.PutComment(ctx, ledger.Comment{ID: mine, Body: "Raised it more."}, "ana@example.com")
	g.f.mu.Lock()
	g.f.reword(g.f.issue(1), theirs, "Fine, but.")
	g.f.mu.Unlock()
	if out := run(); g.f.edited() != 0 || !strings.Contains(out, "carried 0 edits to issues, took 0 edits from them") {
		t.Errorf("edits followed without edited times: %d\n%s", g.f.edited(), out)
	}
	// With edited times but no t_edit: the tracker's edits are taken, and
	// none is sent to it.
	withTimes := regexp.MustCompile(`(?s)\n# t_edit:.*?\n}\n`).ReplaceAllString(string(src), "\n")
	if err := os.WriteFile(filepath.Join(dir, "own.sh"), []byte(withTimes), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	g.f.mu.Lock()
	g.f.reword(g.f.issue(1), theirs, "Fine, but watch Fridays.")
	g.f.mu.Unlock()
	if out := run(); g.f.edited() != 0 || !strings.Contains(out, "carried 0 edits to issues, took 1 edits from them") {
		t.Errorf("edited times, no t_edit: %d\n%s", g.f.edited(), out)
	}
}

// A save in Lapilli that changes nothing moves the comment's time, and
// edits nothing in the tracker, nor tells its watchers, though the post
// reads back otherwise than it was sent (every tracker): Jira Data
// Center's in a block, GitLab's line starting with / escaped (a name the
// sign-in proxy gives, with COMMENT_NAMES), Azure Boards' read as HTML.
func TestTrackerRecipesCommentEditsUnchanged(t *testing.T) {
	parallel(t)
	needTools(t)
	for name := range trackerAPIs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := newTrackerRig(t, name)
			ctx := context.Background()
			g.mustRun(t)
			const who, body = "/cy@example.com", "Raised it: errors < 5 && latency > 2s, at <b>night</b> & noon."
			mine, err := g.s.l.PutComment(ctx, ledger.Comment{On: "task", Key: "1", Body: body}, who)
			if err != nil {
				t.Fatal(err)
			}
			g.mustRun(t, "COMMENTS=true", "COMMENT_NAMES=true")
			if g.f.posts() != 1 {
				t.Fatalf("posted %d", g.f.posts())
			}
			time.Sleep(1100 * time.Millisecond)
			if _, err := g.s.l.PutComment(ctx, ledger.Comment{ID: mine, Body: body}, who); err != nil {
				t.Fatal(err)
			}
			if c, _ := g.s.l.CommentByID(ctx, mine); !c.Updated.After(c.Created) {
				t.Fatalf("the save didn't move the comment's time: %+v", c)
			}
			out := g.mustRun(t, "COMMENTS=true", "COMMENT_NAMES=true")
			if g.f.edited() != 0 || !strings.Contains(out, "carried 0 edits to issues") {
				t.Errorf("a save that changed nothing edited the post %d times\n%s", g.f.edited(), out)
			}
		})
	}
}

// A tracker that refuses to change a comment holds up nothing else: the
// refusal is said, and passed over, and the tracker's own edit is still
// taken, run after run.
func TestTrackerRecipesCommentEditRefused(t *testing.T) {
	parallel(t)
	needTools(t)
	g := newTrackerRig(t, "github")
	ctx := context.Background()
	g.mustRun(t)
	mine, _ := g.s.l.PutComment(ctx, ledger.Comment{On: "task", Key: "1", Body: "Raised it."}, "ana@example.com")
	g.f.mu.Lock()
	theirs := g.f.comment(g.f.issue(1), "someone", "Fine.")
	g.f.mu.Unlock()
	g.mustRun(t, "COMMENTS=true")
	time.Sleep(1100 * time.Millisecond)
	g.s.l.PutComment(ctx, ledger.Comment{ID: mine, Body: "Raised it more."}, "ana@example.com")
	g.f.with(func() {
		g.f.noEdit = true
		g.f.reword(g.f.issue(1), theirs, "Fine, but watch Fridays.")
	})
	out, err := g.run("COMMENTS=true")
	if err != nil || !strings.Contains(out, "refused the edit of comment") || !strings.Contains(out, "took 1 edits from them, passed over 1 the tracker refused") {
		t.Fatalf("%v\n%s", err, out)
	}
	cs, _ := g.s.l.Comments(ctx, "task", "1")
	if !slices.ContainsFunc(cs, func(c ledger.Comment) bool { return c.Body == "Fine, but watch Fridays." }) {
		t.Errorf("the tracker's edit wasn't taken: %+v", cs)
	}
}

// Jira Data Center: with COMMENT_NAMES, who wrote a comment, as the
// sign-in proxy names them, is in the block with the comment, so a name
// holding wiki markup mentions no one and shows no image.
func TestTrackerRecipeJiraDCNameInBlock(t *testing.T) {
	parallel(t)
	needTools(t)
	g := newTrackerRig(t, "jira-dc")
	g.mustRun(t)
	const who = "[~admin] !https://evil.example/i.png!"
	if _, err := g.s.l.PutComment(context.Background(), ledger.Comment{On: "task", Key: "1", Body: "Raised it."}, who); err != nil {
		t.Fatal(err)
	}
	g.mustRun(t, "COMMENTS=true", "COMMENT_NAMES=true")
	g.f.mu.Lock()
	cs := g.f.issue(1).comments
	g.f.mu.Unlock()
	if len(cs) != 1 {
		t.Fatalf("posted %d", len(cs))
	}
	lines := strings.Split(cs[0].body, "\n")
	end := slices.Index(lines[1:], "{noformat}") + 1
	if lines[0] != "{noformat}" || end < 2 || lines[1] != who+" wrote in Lapilli:" || slices.Contains(lines[end:], who+" wrote in Lapilli:") {
		t.Errorf("the name isn't in the block:\n%s", cs[0].body)
	}
}
