package serve

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"lapilli.dev/internal/ledger"
)

// hostile is what a rule's name, a task's title or a label's value can
// hold, from outside: Slack's mentions and links, a code block's end, a
// line of its own, and JSON's quotes and braces.
const hostile = "<!channel> <@U123|ana> <https://attacker.example|click> & ``` \"}],\"x\":{\"\nlapilli: forged\u2028line"

// A destination's template: its fields filled, each value from outside
// escaped for where it goes, so that none mentions anyone, links anywhere,
// ends a code block, starts a line, or breaks the JSON; the template's own
// text is the admin's, and mentions and links as written.
func TestTemplateEscapes(t *testing.T) {
	parallel(t)
	tp, err := parseTemplate("ops", "<!here> *{{.Event}}* ```{{.Title}}``` <{{.Link}}|open> {{ .Labels }}")
	if err != nil {
		t.Fatal(err)
	}
	f := noticeFields{Event: "task.created", Title: hostile, Link: "https://lapilli.example.com/tasks/1?a=b&c=d", Labels: "team=" + hostile}
	slack := tp.render(f, slackEscape)
	for _, bad := range []string{"<!channel>", "<@U123", "<https://attacker", "\n", "\u2028", "``` \""} {
		if strings.Contains(slack, bad) {
			t.Errorf("Slack's text holds %q: %s", bad, slack)
		}
	}
	for _, want := range []string{"<!here> *task.created* ```", "&lt;!channel&gt; &lt;@U123|ana&gt; &lt;https://attacker.example|click&gt; &amp; '''",
		"<https://lapilli.example.com/tasks/1?a=b&amp;c=d|open>", " lapilli: forged line"} {
		if !strings.Contains(slack, want) {
			t.Errorf("Slack's text lacks %q: %s", want, slack)
		}
	}
	if strings.Count(slack, "<") != 2 || strings.Count(slack, ">") != 2 {
		t.Errorf("Slack's text has markup of its own beyond the template's: %s", slack)
	}
	// JSON: the text is a string the receiver reads back as it was, but
	// for its line breaks; no key is added.
	text := tp.render(f, jsonEscape)
	b, err := json.Marshal(map[string]any{"event": "task.created", "text": text})
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("not JSON: %s", b)
	}
	if len(back) != 2 || back["text"] != text || !strings.Contains(text, `"}],"x":{"`) || strings.ContainsAny(text, "\n\u2028") {
		t.Errorf("JSON's text: %q from %s", back["text"], b)
	}
}

// Each backquote of a value is a quote: fields side by side, each with a
// backquote or two, make no code block between them, and the template's
// own backquotes stay. Unicode's format characters (zero-width ones, bidi
// overrides and isolates) are left out of every value, Slack's and JSON's,
// so that none hides a word or shows a title's words in another order.
func TestTemplateBackquotesAndFormat(t *testing.T) {
	parallel(t)
	tp, err := parseTemplate("ops", "{{.Title}}{{.Rule}}{{.Team}} `{{.ID}}`")
	if err != nil {
		t.Fatal(err)
	}
	if got := tp.render(noticeFields{Title: "a`", Rule: "`", Team: "`b", ID: "c`d"}, slackEscape); got != "a'''b `c'd`" {
		t.Errorf("backquotes: %q", got)
	}
	one, _ := parseTemplate("ops", "{{.Title}}|{{.Rule}}")
	f := noticeFields{Title: "refunds\u202egnirotinom\u202c slow\u200b\u2066x\u2069\ufeff", Rule: "Pay\u200dErrors\u00ad"}
	for name, esc := range map[string]func(string) string{"slack": slackEscape, "json": jsonEscape} {
		got := one.render(f, esc)
		if got != "refundsgnirotinom slowx|PayErrors" {
			t.Errorf("%s: %q", name, got)
		}
		for _, r := range got {
			if unicode.Is(unicode.Cf, r) {
				t.Errorf("%s holds %U", name, r)
			}
		}
	}
}

// What a template gives is bounded, whatever the values: each field's to
// maxFieldValue bytes, the whole to maxTemplated, cut at a letter's end;
// and the template itself to maxTemplate.
func TestTemplateBounded(t *testing.T) {
	parallel(t)
	tp, err := parseTemplate("ops", strings.Repeat("{{.Title}}", 199))
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("가", 10_000) // three bytes a letter
	out := tp.render(noticeFields{Title: long}, slackEscape)
	if len(out) > maxTemplated || !utf8.ValidString(out) || !strings.HasSuffix(out, "…") {
		t.Errorf("%d bytes, valid %v", len(out), utf8.ValidString(out))
	}
	one, _ := parseTemplate("ops", "{{.Title}}")
	if got := one.render(noticeFields{Title: long}, slackEscape); len(got) > maxFieldValue || !utf8.ValidString(got) {
		t.Errorf("a field: %d bytes", len(got))
	}
	if _, err := parseTemplate("ops", strings.Repeat("x", maxTemplate+1)); err == nil {
		t.Error("a template past maxTemplate taken")
	}
}

// A route's destination is the row's, never what the notice tells: a
// "to" or a "url" in it, or a label naming another destination or a URL,
// posts nowhere else. A named webhook's posts are signed with its own key;
// a Slack-style destination's, in its template's words, and a JSON one's
// text a JSON string whatever its values; a name the flags no longer give
// posts nothing.
func TestRoutedSendByRow(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, hs, _ := routedServer(t, `notify:
  routes:
    - {kinds: [task.created], to: [payments-hook, ops]}
  default: [slack]
  templates:
    ops: "<!here> {{.Event}}: {{.Title}} {{.Link}}"
    payments-hook: "{{.Title}}"
`, nil)
	s.wall = func() time.Time { return time.Unix(1_800_000_000, 0) }
	if _, err := s.l.PutTask(ctx, ledger.Task{Title: hostile, Kind: "tune", Body: `{"to": "checkout-hook", "url": "https://attacker.example/x"}`}, "ana"); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"route:payments-hook", "route:ops"} {
		if err := s.sendDue(ctx, d, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	pay := hs.payments.all()
	if len(pay) != 1 || !pay[0].holds(hs.paymentsKey) || pay[0].event() != "task.created" {
		t.Fatalf("payments-hook: %+v", pay)
	}
	// Its template's text a JSON string, the title in it as it was but on
	// one line, and no key the title tried to add.
	var body map[string]any
	if err := json.Unmarshal(pay[0].body, &body); err != nil {
		t.Fatalf("payments-hook's body isn't JSON: %v\n%s", err, pay[0].body)
	}
	if body["url"] != "https://lapilli.example.com/tasks/1" || body["id"] != "lapilli-1" || body["event"] != "task.created" ||
		body["text"] != oneLine(hostile) || body["x"] != nil || body["task"] == nil {
		t.Errorf("payments-hook's body: %s", pay[0].body)
	}
	ops := hs.ops.all()
	if len(ops) != 1 {
		t.Fatalf("ops: %d posts", len(ops))
	}
	var msg struct {
		Text   string `json:"text"`
		Mrkdwn bool   `json:"mrkdwn"`
	}
	json.Unmarshal(ops[0].body, &msg)
	if !msg.Mrkdwn || !strings.HasPrefix(msg.Text, "<!here> task.created: &lt;!channel&gt;") || strings.Contains(msg.Text, "<!channel>") ||
		!strings.HasSuffix(msg.Text, " https://lapilli.example.com/tasks/1") {
		t.Errorf("ops: %s", ops[0].body)
	}
	for name, h := range map[string]*signedHook{"checkout-hook": hs.checkout, "webhook": hs.webhook, "slack": hs.slack, "team:payments": hs.teamPay} {
		if n := len(h.all()); n != 0 {
			t.Errorf("%s was posted %d times", name, n)
		}
	}
	// A row for a destination the flags no longer give fails, posting
	// nothing; so does one whose name is a label's or a URL.
	for _, dest := range []string{"route:gone", "route:https://attacker.example/x"} {
		if err := s.send(ctx, ledger.Notice{ID: 9, Dest: dest, Event: "task.created", Body: []byte(`{"event": "task.created", "to": "ops"}`)}); err == nil ||
			!strings.Contains(err.Error(), "no destination called") {
			t.Errorf("%s: %v", dest, err)
		}
	}
	if n := len(hs.ops.all()); n != 1 {
		t.Errorf("ops posted %d times", n)
	}
}

// A --webhook-dest whose key can't be used fails, posting nothing, and
// says the flag that gave the key, not --notify-webhook-secret-file.
func TestWebhookDestKeyNamed(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	short := filepath.Join(t.TempDir(), "short")
	os.WriteFile(short, []byte("too short"), 0o600)
	s, hs, _ := routedServer(t, "notify:\n  default: [payments-hook]\n", func(c *Config) {
		c.WebhookDestSecrets["payments-hook"] = short
	})
	if _, err := s.l.PutTask(ctx, ledger.Task{Title: "t", Kind: "tune"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if err := s.sendDue(ctx, "route:payments-hook", time.Now()); err != nil {
		t.Fatal(err)
	}
	why, err := s.l.LastError(ctx, "route:payments-hook")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(why, "--webhook-dest-secret-file payments-hook: want a key of 32 bytes at least") || strings.Contains(why, "too short") {
		t.Errorf("said: %q", why)
	}
	if n := len(hs.payments.all()); n != 0 {
		t.Errorf("posted %d times unsigned", n)
	}
}

// A source down or back is told by its name: a template's {{.Title}}.
func TestSourceNoticeTitled(t *testing.T) {
	parallel(t)
	s, _, _ := routedServer(t, "teams: []\n", nil)
	for _, kind := range []string{"source.down", "source.back"} {
		about := []byte(`{"event": "` + kind + `", "source_read": {"id": "prometheus", "kind": "prometheus", "name": "prometheus-main", "state": "error"}}`)
		f := s.fieldsOf(context.Background(), ledger.Notice{Event: kind, Body: about}, notice{Event: kind}, "")
		if f.Title != "prometheus-main" || f.ID != "prometheus" {
			t.Errorf("%s: title %q, id %q", kind, f.Title, f.ID)
		}
	}
}

// The webhook and Slack with templates of their own: what happens in their
// words, the digest as it was, its text the digest's.
func TestTemplatedWebhookAndSlack(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, hs, _ := routedServer(t, `notify:
  templates:
    webhook: "{{.Event}} by {{.By}}"
    slack: "*{{.Title}}*"
`, nil)
	if _, err := s.l.PutTask(ctx, ledger.Task{Title: "<b>", Kind: "tune"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if err := s.l.TellOnce(ctx, "digest-told", "1", "digest.ready", []ledger.Told{{Dest: "webhook", About: map[string]any{"text": "the week"}}}); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"webhook", "slack"} {
		if err := s.sendDue(ctx, d, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	hook := hs.webhook.all()
	if len(hook) != 2 {
		t.Fatalf("webhook: %d posts", len(hook))
	}
	texts := map[string]string{}
	for _, p := range hook {
		var b struct{ Event, Text string }
		json.Unmarshal(p.body, &b)
		texts[b.Event] = b.Text
	}
	if texts["task.created"] != "task.created by ana" || texts["digest.ready"] != "the week" {
		t.Errorf("webhook's texts: %q", texts)
	}
	slack := hs.slack.all()
	var msg struct{ Text string }
	if len(slack) == 1 {
		json.Unmarshal(slack[0].body, &msg)
	}
	if len(slack) != 1 || msg.Text != "*&lt;b&gt;*" {
		t.Errorf("slack: %d posts, %q", len(slack), msg.Text)
	}
}
