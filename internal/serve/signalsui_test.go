package serve

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/verdict"
)

// Named impact signals on the Settings page (docs/design/signals.md, step
// 3): the list, the rule entries, the try, reading one again, a row each
// in the sources panel.

// adminPost posts a form as who, from this site.
func adminPost(t *testing.T, s *Server, path string, form url.Values, who string) (int, string) {
	t.Helper()
	return askAs(t, s.Handler(), http.MethodPost, path, form, who, "same-origin")
}

// signalPost is a signal's form: was, name, expr, for, labels to match on
// ("" follows the match in effect; "-" none), record.
func signalPost(was, name, expr, dur, match string, record bool) url.Values {
	v := url.Values{"was": {was}, "name": {name}, "expr": {expr}, "for": {dur}, "match-mode": {"effect"}}
	if match != "" {
		v.Set("match-mode", "labels")
		if match != "-" {
			v.Set("match", match)
		}
	}
	if record {
		v.Set("record", "1")
	}
	return v
}

// inEffect are the named signals in effect, by name.
func inEffect(t *testing.T, s *Server) signalsNow {
	t.Helper()
	n, _, err := s.signalsNowOf(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// signalRow is the ledger's row of a signal.
func signalRow(t *testing.T, s *Server, name string) (ledger.SignalRow, bool) {
	t.Helper()
	rows, err := s.l.SignalRows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(rows, func(r ledger.SignalRow) bool { return r.Name == name })
	if i < 0 {
		return ledger.SignalRow{}, false
	}
	return rows[i], true
}

// An admin adds a signal, changes one, takes one away and gives each back
// to the start; the UI's is kept over the command line's of the same name,
// the ledger keeps them as it keeps the command line's (a bit each, none
// shared), who set each and when, every change noted with its before and
// after; settings.yaml gives them as the chart's impact.
func TestSignalsSetInUI(t *testing.T) {
	ctx := context.Background()
	h := shopHistory()
	s := signalServer(t, h, func(c *Config) { c.Editors = []string{"bo@example.com"} })
	if code, _ := adminPost(t, s, "/settings/signals", signalPost("", "latency", "latency_p99 > 1", "10m", "service", true), "bo@example.com"); code != http.StatusForbidden {
		t.Errorf("an editor added one: %d", code)
	}
	if code, _ := askAs(t, s.Handler(), http.MethodPost, "/settings/signals", signalPost("", "latency", "latency_p99 > 1", "10m", "service", true), "ana@example.com", "cross-site"); code != http.StatusForbidden {
		t.Errorf("another site added one: %d", code)
	}
	if n := inEffect(t, s); len(n.Defs) != 2 {
		t.Fatalf("refused, kept: %+v", n.Defs)
	}
	code, body := adminPost(t, s, "/settings/signals", signalPost("", "latency", "latency_p99 > 1", "10m", "service", true), "ana@example.com")
	if code != http.StatusSeeOther {
		t.Fatalf("added: %d %s", code, body)
	}
	n := inEffect(t, s)
	d, ok := n.def("latency")
	if !ok || d.For != 10*time.Minute || !slices.Equal(d.Match, []string{"service"}) || d.SetBy != "ana@example.com" {
		t.Fatalf("in effect: %+v", n.Defs)
	}
	bits := map[int]string{}
	for _, name := range []string{"errors", "probe", "latency"} {
		r, ok := signalRow(t, s, name)
		if !ok {
			t.Fatalf("no row for %s", name)
		}
		if other, taken := bits[r.Bit]; taken {
			t.Errorf("%s and %s share bit %d", name, other, r.Bit)
		}
		bits[r.Bit] = name
		if name == "latency" && r.SetBy != "ana@example.com" {
			t.Errorf("latency set by %q", r.SetBy)
		}
	}
	// It was read over the last four weeks before it was saved.
	h.mu.Lock()
	asked := slices.Contains(h.asked, "latency_p99 > 1")
	h.mu.Unlock()
	if !asked {
		t.Error("saved without reading it")
	}
	// The probe's match changed here: the UI's over the command line's.
	if code, body := adminPost(t, s, "/settings/signals", signalPost("probe", "probe", "probe_success == 0", "2m", "service, team", true), "ana@example.com"); code != http.StatusSeeOther {
		t.Fatalf("changed: %d %s", code, body)
	}
	if d, _ := inEffect(t, s).def("probe"); !slices.Equal(d.Match, []string{"service", "team"}) {
		t.Errorf("the probe in effect: %+v", d)
	}
	// The command line's errors taken away: off, its row gone.
	if code, body := adminPost(t, s, "/settings/signals", url.Values{"was": {"errors"}, "remove": {"1"}}, "ana@example.com"); code != http.StatusSeeOther {
		t.Fatalf("removed: %d %s", code, body)
	}
	if _, ok := inEffect(t, s).def("errors"); ok {
		t.Error("errors still in effect")
	}
	if _, ok := signalRow(t, s, "errors"); ok {
		t.Error("errors still kept")
	}
	_, page := askAs(t, s.Handler(), http.MethodGet, "/settings", nil, "ana@example.com", "same-origin")
	for _, want := range []string{"not started with: added here", "changed here", "taken away here", `id="signal-errors"`, "Give errors back"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page doesn't say %q", want)
		}
	}
	// settings.yaml: the chart's impact, the signals in effect.
	_, y, _ := get(t, s.Handler(), "/settings.yaml")
	var doc struct {
		Impact struct {
			Signals []struct {
				Name, Expr, For string
				Match           []string
			}
		}
	}
	if err := yaml.Unmarshal([]byte(y), &doc); err != nil || len(doc.Impact.Signals) != 2 || doc.Impact.Signals[0].Name != "probe" ||
		!slices.Equal(doc.Impact.Signals[0].Match, []string{"service", "team"}) || doc.Impact.Signals[1].Name != "latency" {
		t.Errorf("settings.yaml: %v\n%s", err, y)
	}
	// Back to the start, each: the command line's again.
	for _, name := range []string{"errors", "probe", "latency"} {
		if code, body := adminPost(t, s, "/settings/signals", url.Values{"was": {name}, "reset": {"1"}}, "ana@example.com"); code != http.StatusSeeOther {
			t.Fatalf("%s back to the start: %d %s", name, code, body)
		}
	}
	n = inEffect(t, s)
	if len(n.Defs) != 2 || len(n.UI) != 0 || !sameSignal(n.Defs[0], s.cfg.Signals[0]) || !sameSignal(n.Defs[1], s.cfg.Signals[1]) {
		t.Errorf("back to the start: %+v", n)
	}
	if _, ok := signalRow(t, s, "latency"); ok {
		t.Error("latency still kept")
	}
	// The activity log: each change, with before and after.
	cs, err := s.l.Changes(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	var whats []string
	for _, c := range cs {
		whats = append(whats, c.What)
		if c.What == "setting impact signal probe" && c.Before != "" && c.After != "" && !strings.Contains(c.After, "team") && !strings.Contains(c.Before, "team") {
			t.Errorf("the probe's change: %+v", c)
		}
	}
	for _, want := range []string{"setting impact signal latency", "setting impact signal probe", "setting impact signal errors"} {
		if !slices.Contains(whats, want) {
			t.Errorf("the activity log hasn't %q: %v", want, whats)
		}
	}
}

// What the Settings page takes of a signal and of the rule entries is
// bounded as the design says, and nothing refused is saved.
func TestSignalBounds(t *testing.T) {
	s := signalServer(t, shopHistory(), nil)
	long := strings.Repeat("a", 4097)
	many := strings.TrimSuffix(strings.Repeat("l,", 17), ",")
	for i := 0; i < 17; i++ {
		many = strings.Replace(many, "l,", "l"+string(rune('a'+i))+"_,", 1)
	}
	for _, c := range []struct {
		name string
		form url.Values
		want string
	}{
		{"a name not of the alphabet", signalPost("", "Latency", "x > 1", "5m", "", true), "a signal&#39;s name is"},
		{"a name too long", signalPost("", strings.Repeat("a", 33), "x > 1", "5m", "", true), "a signal&#39;s name is"},
		{"no expression", signalPost("", "latency", "", "5m", "", true), "give the signal an expression"},
		{"an expression too long", signalPost("", "latency", long, "5m", "", true), "characters at most"},
		{"not PromQL", signalPost("", "latency", "sum(rate(x[5m])", "5m", "", true), "doesn&#39;t read as PromQL"},
		{"Prometheus refuses it", signalPost("", "latency", "nope(up)", "5m", "", true), "unknown function"},
		{"a for under the step", signalPost("", "latency", "x > 1", "10s", "", true), "want from 1m to 24h"},
		{"a for over a day", signalPost("", "latency", "x > 1", "25h", "", true), "want from 1m to 24h"},
		{"17 labels", signalPost("", "latency", "x > 1", "5m", many, true), "16 at most"},
		{"not a label's name", signalPost("", "latency", "x > 1", "5m", "service,a-b", true), "isn&#39;t a label&#39;s name"},
		{"a label twice", signalPost("", "latency", "x > 1", "5m", "service,service", true), "twice"},
		{"a name in effect", signalPost("", "probe", "x > 1", "5m", "", true), "in effect already"},
		{"renamed onto one in effect", signalPost("probe", "errors", "probe_success == 0", "2m", "service", true), "in effect already"},
		{"none such", signalPost("ghost", "ghost", "x > 1", "5m", "", true), "no signal named"},
	} {
		code, body := adminPost(t, s, "/settings/signals", c.form, "ana@example.com")
		if code != http.StatusBadRequest || !strings.Contains(body, c.want) {
			t.Errorf("%s: %d, want %q\n%s", c.name, code, c.want, between(body, "<main", "</main>"))
		}
	}
	// Eight at most: six more fit (set as the page sets them, past the
	// reads an admin may ask for in an hour), the ninth is refused.
	for i := 0; i < 6; i++ {
		name := "s" + string(rune('a'+i))
		u := uiSignal{Expr: name + " > 1", For: "5m", Record: true}
		if err := s.l.SetSettings(context.Background(), map[string]string{signalKey + name: u.text()}, "ana@example.com"); err != nil {
			t.Fatal(err)
		}
	}
	if n := inEffect(t, s); len(n.Defs) != 8 || len(n.Aside) != 0 {
		t.Fatalf("eight: %+v", n)
	}
	if code, body := adminPost(t, s, "/settings/signals", signalPost("", "ninth", "ninth > 1", "5m", "", true), "ana@example.com"); code != http.StatusBadRequest || !strings.Contains(body, "8 named signals at most") {
		t.Errorf("a ninth: %d", code)
	}
	// The rule entries.
	var many65, labels17, signals9 strings.Builder
	for i := 0; i < 65; i++ {
		many65.WriteString("- {rules: ^A$, signals: [probe]}\n")
	}
	labels17.WriteString("- signals: [probe]\n  labels:\n")
	for i := 0; i < 17; i++ {
		labels17.WriteString("    l" + string(rune('a'+i)) + ": x\n")
	}
	signals9.WriteString("- {rules: ^A$, signals: [errors, probe, sa, sb, sc, sd, se, sf, errors]}\n")
	for _, c := range []struct {
		name, text, want string
	}{
		{"65 entries", many65.String(), "65 rule entries: 64 at most"},
		{"17 labels", labels17.String(), "17 labels: 16 at most"},
		{"9 signals", signals9.String(), "9 signals: 8 at most"},
		{"a pattern too long", "- {rules: " + strings.Repeat("a", 201) + ", signals: [probe]}\n", "200 bytes at most"},
		{"a pattern that doesn't compile", "- {rules: \"(\", signals: [probe]}\n", "rule pattern"},
		{"a signal not in effect", "- {rules: ^A$, signals: [ghost]}\n", "no signal named"},
		{"no rule named", "- {signals: [probe]}\n", "names no rule"},
		{"no signals", "- {rules: ^A$}\n", "no signals"},
		{"an unknown key", "- {rules: ^A$, signals: [probe], kind: x}\n", "don&#39;t read as a YAML list"},
		{"a match label not a label's", "- {rules: ^A$, signals: [probe], match: [a-b]}\n", "isn&#39;t a label&#39;s name"},
		{"too long", strings.Repeat("#", maxEntriesText+1), "KB at most"},
	} {
		code, body := adminPost(t, s, "/settings/impact-rules", url.Values{"rules": {c.text}}, "ana@example.com")
		if code != http.StatusBadRequest || !strings.Contains(body, c.want) {
			t.Errorf("%s: %d, want %q\n%s", c.name, code, c.want, between(body, "<main", "</main>"))
		}
	}
	set, err := s.l.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := set[rulesKey]; ok {
		t.Errorf("refused entries kept: %+v", set[rulesKey])
	}
}

// The rule entries saved here take the place of the start's, as a whole,
// and back to the start gives the start's back; each change judges every
// firing again, as the command line's does.
func TestSignalEntriesInUI(t *testing.T) {
	ctx := context.Background()
	s := signalServer(t, shopHistory(), func(c *Config) {
		c.ImpactRules = []verdict.RuleEntry{{Rules: "^Nothing$", Signals: []string{"errors"}}}
	})
	text := "- rules: ^Errors$\n  signals: [probe]\n  match: []\n"
	if code, body := adminPost(t, s, "/settings/impact-rules", url.Values{"rules": {text}}, "ana@example.com"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	es, err := s.l.RuleEntries(ctx)
	if err != nil || len(es) != 1 || es[0].Rules != "^Errors$" || es[0].Match == nil || len(es[0].Match) != 0 {
		t.Fatalf("kept: %+v %v", es, err)
	}
	o, err := s.options(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if e := o.Signals.Entry("Errors"); e == nil || !slices.Equal(e.Signals, []string{"probe"}) {
		t.Errorf("judged by: %+v", e)
	}
	_, page := askAs(t, s.Handler(), http.MethodGet, "/settings", nil, "ana@example.com", "same-origin")
	if !strings.Contains(page, "Set here in place of the 1 serve was started with") {
		t.Errorf("the page doesn't say the entries are the UI's")
	}
	if code, _ := adminPost(t, s, "/settings/impact-rules", url.Values{"reset": {"1"}}, "ana@example.com"); code != http.StatusSeeOther {
		t.Fatal(code)
	}
	if es, _ := s.l.RuleEntries(ctx); len(es) != 1 || es[0].Rules != "^Nothing$" {
		t.Errorf("back to the start: %+v", es)
	}
	// Saved empty: no entries, in place of the start's.
	if code, _ := adminPost(t, s, "/settings/impact-rules", url.Values{"rules": {""}}, "ana@example.com"); code != http.StatusSeeOther {
		t.Fatal(code)
	}
	if es, _ := s.l.RuleEntries(ctx); len(es) != 0 {
		t.Errorf("none: %+v", es)
	}
}

// A signal renamed in place is the same criterion: nothing is read, its
// windows are its new name's, keys, decisions, tasks and comments with
// them, and the rule entries that name it name it by its new name.
func TestSignalRenamedInUI(t *testing.T) {
	ctx := context.Background()
	h := signalWindowsHistory()
	s := signalServer(t, h, func(c *Config) {
		c.ImpactRules = []verdict.RuleEntry{{Rules: "^Errors$", Signals: []string{"probe"}}}
	})
	ws, err := s.l.ImpactWindows(ctx, now.Add(-30*24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	var key string
	for _, w := range ws {
		if w.Signal == "probe" {
			key = w.ID
		}
	}
	if key == "" {
		t.Fatalf("no probe window: %+v", ws)
	}
	o, _ := s.options(ctx)
	if _, err := s.l.Decide(ctx, key, "dismissed", "", "ana@example.com", o); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutComment(ctx, ledger.Comment{On: "incident", Key: key, Body: "the prober broke"}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	was, _ := signalRow(t, s, "probe")
	h.mu.Lock()
	h.asked = nil
	h.mu.Unlock()
	if code, body := adminPost(t, s, "/settings/signals", signalPost("probe", "synthetic", "probe_success == 0", "2m", "service", true), "ana@example.com"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	h.mu.Lock()
	asked := slices.Clone(h.asked)
	h.mu.Unlock()
	if slices.Contains(asked, "probe_success == 0") {
		t.Errorf("renamed, read: %q", asked)
	}
	now, ok := signalRow(t, s, "synthetic")
	if !ok || now.Bit != was.Bit {
		t.Fatalf("renamed: %+v, was %+v", now, was)
	}
	renamed := strings.Replace(key, "impact-probe-", "impact-synthetic-", 1)
	in, err := s.l.IncidentByID(ctx, renamed)
	if err != nil || in.Signal != "synthetic" || in.State != "dismissed" {
		t.Errorf("the window: %+v %v", in, err)
	}
	if cs, err := s.l.Comments(ctx, "incident", renamed); err != nil || len(cs) != 1 {
		t.Errorf("its comment: %+v %v", cs, err)
	}
	n := inEffect(t, s)
	if n.EntriesUI == nil || len(n.Entries) != 1 || !slices.Equal(n.Entries[0].Signals, []string{"synthetic"}) {
		t.Errorf("entries: %+v", n.Entries)
	}
	if _, ok := n.def("probe"); ok {
		t.Error("the probe still in effect")
	}
	// Renamed with another expression: taken away and another added, its
	// windows kept with its old name.
	if code, body := adminPost(t, s, "/settings/signals", signalPost("synthetic", "probe2", "probe_success < 1", "2m", "service", true), "ana@example.com"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	if in, err := s.l.IncidentByID(ctx, renamed); err != nil || in.Signal != "synthetic" {
		t.Errorf("kept with its name: %+v %v", in, err)
	}
	if _, ok := signalRow(t, s, "synthetic"); ok {
		t.Error("synthetic still kept")
	}
	// Renamed onto a name whose windows are kept: refused.
	if code, body := adminPost(t, s, "/settings/signals", signalPost("errors", "synthetic", "http_requests_total > 1e9", "5m", "", true), "ana@example.com"); code != http.StatusBadRequest || !strings.Contains(body, "since taken away") {
		t.Errorf("onto kept windows: %d", code)
	}
}

// With the impact signal with no name in effect, the first named signal
// saved makes it the signal impact, beside the one added: its windows and
// their keys kept, its record as it was; the command line's --impact
// stands aside, and back to the start of both gives it back.
func TestFirstNamedSignalNamesUnnamed(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, false, func(c *Config) { c.Editors = nil })
	ws, err := s.l.ImpactWindows(ctx, now.Add(-30*24*time.Hour), now)
	if err != nil || len(ws) == 0 {
		t.Fatalf("windows: %+v %v", ws, err)
	}
	keys := map[string]bool{}
	for _, w := range ws {
		keys[w.ID] = true
	}
	kept, _ := s.l.ImpactKept(ctx)
	_, page := askAs(t, s.Handler(), http.MethodGet, "/settings", nil, "ana@example.com", "same-origin")
	if !strings.Contains(page, "Add a named impact signal") || !strings.Contains(page, "becomes the signal impact") {
		t.Error("the page offers no named signal")
	}
	if _, page := askAs(t, s.Handler(), http.MethodGet, "/settings", nil, "viewer@example.com", "same-origin"); strings.Contains(page, "Add a named impact signal") {
		t.Error("a viewer is offered a named signal")
	}
	if code, body := adminPost(t, s, "/settings/signals", signalPost("", "impact", "impact > 1", "5m", "", true), "ana@example.com"); code != http.StatusBadRequest || !strings.Contains(body, "give this one another name") {
		t.Errorf("named impact beside it: %d", code)
	}
	// Tried first: the one with no name, named, its windows as they are.
	q := signalPost("", "probe", "impact > 1", "5m", "service", true)
	q.Set("signal-try", "1")
	if code, body := askAs(t, s.Handler(), http.MethodGet, "/settings?"+q.Encode(), nil, "ana@example.com", "same-origin"); code != http.StatusOK ||
		!strings.Contains(body, "the impact signal with no name, named") || !strings.Contains(body, "added: read over the four weeks") {
		t.Errorf("tried: %d %s", code, between(body, `id="signals-tried"`, "</section>"))
	}
	if inEffect(t, s).named() {
		t.Fatal("the try saved")
	}
	if code, body := adminPost(t, s, "/settings/signals", signalPost("", "probe", "impact > 1", "5m", "service", true), "ana@example.com"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	n := inEffect(t, s)
	if len(n.Defs) != 2 || n.Defs[0].Name != "impact" || n.Defs[0].Expr != s.cfg.Impact || n.Defs[1].Name != "probe" {
		t.Fatalf("in effect: %+v", n.Defs)
	}
	if c, _ := s.criterion(ctx); c.Expr != "" {
		t.Errorf("the one with no name still read: %+v", c)
	}
	ws, err = s.l.ImpactWindows(ctx, now.Add(-30*24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range ws {
		if keys[w.ID] && w.Signal != "impact" {
			t.Errorf("window %s of %q", w.ID, w.Signal)
		}
	}
	r, ok := signalRow(t, s, "impact")
	p, _ := signalRow(t, s, "probe")
	if !ok || r.Kept.Criterion != kept.Criterion || r.Bit == p.Bit {
		t.Errorf("its row: %+v, the probe's bit %d", r, p.Bit)
	}
	// The runs go on: its windows keep their keys.
	runDue(s, 1)
	s.jobs(ctx, true)
	ws, _ = s.l.ImpactWindows(ctx, now.Add(-30*24*time.Hour), now)
	for k := range keys {
		if !slices.ContainsFunc(ws, func(w ledger.Incident) bool { return w.ID == k }) {
			t.Errorf("after a run, %s is gone: %+v", k, ws)
		}
	}
	// Both back to the start, the one it was made last: the one with no
	// name again.
	for _, name := range []string{"probe", "impact"} {
		if code, body := adminPost(t, s, "/settings/signals", url.Values{"was": {name}, "reset": {"1"}}, "ana@example.com"); code != http.StatusSeeOther {
			t.Fatalf("%s: %d %s", name, code, body)
		}
	}
	if c, _ := s.criterion(ctx); c.Expr != s.cfg.Impact || inEffect(t, s).named() {
		t.Errorf("back to the start: %+v", c)
	}
	// The last named one taken away, of its criterion, gives the one with
	// no name back its windows, keys and all, its record standing: nothing
	// read again.
	ws, _ = s.l.ImpactWindows(ctx, now.Add(-30*24*time.Hour), now.Add(2*time.Hour))
	for k := range keys {
		if i := slices.IndexFunc(ws, func(w ledger.Incident) bool { return w.ID == k }); i < 0 || ws[i].Signal != "" {
			t.Errorf("%s isn't the one with no name's again", k)
		}
	}
	if back, _ := s.l.ImpactKept(ctx); back.Criterion != kept.Criterion || !back.Since.Equal(kept.Since) {
		t.Errorf("its record: %+v, was %+v", back, kept)
	}
	if plan, _, _ := s.againPlan(ctx, criterion{s.cfg.Impact, s.cfg.ImpactFor}); plan == againRead {
		t.Error("read again, given back")
	}
}

// A signal serve was started with, given back to the start while the one
// with no name is in effect (the UI took every named one away, and set
// one with no name), makes that one a named one as the first named one
// added does: named signals and one with no name never go together.
func TestStartedSignalGivenBackNamesUnnamed(t *testing.T) {
	s := signalServer(t, shopHistory(), nil)
	for _, name := range []string{"errors", "probe"} {
		if code, body := adminPost(t, s, "/settings/signals", url.Values{"was": {name}, "remove": {"1"}}, "ana@example.com"); code != http.StatusSeeOther {
			t.Fatalf("%s: %d %s", name, code, body)
		}
	}
	if code, body := adminPost(t, s, "/settings/impact", url.Values{"impact": {"impact > 1"}}, "ana@example.com"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	if c, _ := s.criterion(context.Background()); c.Expr != "impact > 1" {
		t.Fatalf("the one with no name: %+v", c)
	}
	if code, body := adminPost(t, s, "/settings/signals", url.Values{"was": {"probe"}, "reset": {"1"}}, "ana@example.com"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	n := inEffect(t, s)
	if _, ok := n.def("impact"); !ok || len(n.Defs) != 2 {
		t.Errorf("in effect: %+v", n.Defs)
	}
	if c, _ := s.criterion(context.Background()); c.Expr != "" {
		t.Errorf("the one with no name beside named ones: %+v", c)
	}
}

// Saving the rule entries judges every firing again: among the reads and
// saves an admin may ask for in an hour.
func TestSignalEntriesSavesCounted(t *testing.T) {
	s := signalServer(t, shopHistory(), nil)
	for i := 0; i < impactTriesAnHour; i++ {
		text := "- {rules: ^Errors" + strings.Repeat("x", i) + "$, signals: [probe]}\n"
		if code, body := adminPost(t, s, "/settings/impact-rules", url.Values{"rules": {text}}, "ana@example.com"); code != http.StatusSeeOther {
			t.Fatalf("%d: %d %s", i, code, body)
		}
	}
	if code, body := adminPost(t, s, "/settings/impact-rules", url.Values{"rules": {"- {rules: ^Last$, signals: [probe]}\n"}}, "ana@example.com"); code != http.StatusBadRequest || !strings.Contains(body, "as many incident criteria") {
		t.Errorf("past the saves of an hour: %d", code)
	}
}

// A try shows no more than a save would keep: past the windows a read
// again keeps in a week, it says so.
func TestSignalsTryHeldToWhatIsKept(t *testing.T) {
	h := shopHistory()
	at := time.Date(2026, 11, 2, 5, 0, 0, 0, time.UTC)
	var many []prom.Series
	for i := 0; i < 5; i++ {
		many = append(many, minutes(map[string]string{"service": fmt.Sprint("s", i)}, at.Add(time.Duration(i)*time.Hour), at.Add(time.Duration(i)*time.Hour+10*time.Minute)))
	}
	h.windows = map[string][]prom.Series{"pods_down > 0": many}
	s := signalServer(t, h, nil)
	defer func(was int) { maxSignalWeek = was }(maxSignalWeek)
	maxSignalWeek = 1
	q := signalPost("", "pods", "pods_down > 0", "5m", "", true)
	q.Set("signal-try", "1")
	_, body := askAs(t, s.Handler(), http.MethodGet, "/settings?"+q.Encode(), nil, "ana@example.com", "same-origin")
	if !strings.Contains(between(body, `id="signals-tried"`, "</section>"), "finds more than 1 windows a week") {
		t.Errorf("the try: %s", between(body, `id="signals-tried"`, "</section>"))
	}
}

// The try of a named signal reads the signal it changes alone, shows its
// windows apart, and the verdict in effect and with it; it saves nothing.
// A try of the entries reads nothing. It is an admin's, from this site.
func TestSignalsTry(t *testing.T) {
	ctx := context.Background()
	h := signalWindowsHistory()
	at := time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC)
	h.windows["latency_p99 > 1"] = []prom.Series{minutes(map[string]string{"service": "search"}, at, at.Add(15*time.Minute))}
	s := signalServer(t, h, func(c *Config) { c.Editors = []string{"bo@example.com"} })
	q := signalPost("", "latency", "latency_p99 > 1", "10m", "service", true)
	q.Set("signal-try", "1")
	path := "/settings?" + q.Encode()
	if code, _ := askAs(t, s.Handler(), http.MethodGet, path, nil, "bo@example.com", "same-origin"); code != http.StatusForbidden {
		t.Errorf("an editor tried: %d", code)
	}
	if code, _ := askAs(t, s.Handler(), http.MethodGet, path, nil, "ana@example.com", "cross-site"); code == http.StatusOK {
		t.Errorf("another site tried: %d", code)
	}
	h.mu.Lock()
	h.asked = nil
	h.mu.Unlock()
	code, body := askAs(t, s.Handler(), http.MethodGet, path, nil, "ana@example.com", "same-origin")
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	tried := between(body, `id="signals-tried"`, "</section>")
	for _, want := range []string{"Impact signals, in effect and with it", "added: read over the four weeks", "1 windows found; 1 of them would count", "The verdict, in effect and with it", "Nothing was saved"} {
		if !strings.Contains(tried, want) {
			t.Errorf("the try doesn't say %q:\n%s", want, tried)
		}
	}
	h.mu.Lock()
	asked := slices.Clone(h.asked)
	h.mu.Unlock()
	for _, q := range asked {
		if q == "http_requests_total > 1e9" || q == "probe_success == 0" {
			t.Errorf("read a signal the try doesn't change: %q", q)
		}
	}
	if !slices.Contains(asked, "latency_p99 > 1") {
		t.Errorf("didn't read it: %q", asked)
	}
	if n := inEffect(t, s); len(n.Defs) != 2 {
		t.Errorf("saved: %+v", n.Defs)
	}
	// The entries tried: nothing read.
	h.mu.Lock()
	h.asked = nil
	h.mu.Unlock()
	rq := url.Values{"rules-try": {"1"}, "rules": {"- {rules: ^Errors$, signals: [probe]}\n"}}
	code, body = askAs(t, s.Handler(), http.MethodGet, "/settings?"+rq.Encode(), nil, "ana@example.com", "same-origin")
	if code != http.StatusOK || !strings.Contains(body, "The verdict, in effect and with it") {
		t.Fatalf("entries tried: %d %s", code, between(body, `id="signals-tried"`, "</section>"))
	}
	h.mu.Lock()
	n := len(h.asked)
	h.mu.Unlock()
	if n != 0 {
		t.Errorf("the entries tried read %d queries", n)
	}
	if es, _ := s.l.RuleEntries(ctx); len(es) != 0 {
		t.Errorf("saved: %+v", es)
	}
	// Tried, what is in effect: nothing would change.
	q = signalPost("probe", "probe", "probe_success == 0", "2m", "service", true)
	q.Set("signal-try", "1")
	_, body = askAs(t, s.Handler(), http.MethodGet, "/settings?"+q.Encode(), nil, "ana@example.com", "same-origin")
	if !strings.Contains(body, "It is what is in effect: nothing would change.") {
		t.Errorf("the same: %s", between(body, `id="signals-tried"`, "</section>"))
	}
	// Each try is one of the reads an admin may ask for in an hour, the
	// entries' too, which read nothing.
	for i := 0; i < impactTriesAnHour; i++ {
		askAs(t, s.Handler(), http.MethodGet, "/settings?"+rq.Encode()+"&n="+string(rune('a'+i)), nil, "ana@example.com", "same-origin")
	}
	if code, body := askAs(t, s.Handler(), http.MethodGet, "/settings?"+rq.Encode()+"&n=last", nil, "ana@example.com", "same-origin"); code != http.StatusBadRequest || !strings.Contains(body, "as many incident criteria") {
		t.Errorf("past the tries of an hour: %d", code)
	}
}

// An admin reads a signal again alone: within the reads an admin may ask
// for in an hour, noted in the activity log; its windows in place of its
// own from then on.
func TestSignalReadAgainInUI(t *testing.T) {
	ctx := context.Background()
	h := signalWindowsHistory()
	s := signalServer(t, h, func(c *Config) { c.Editors = []string{"bo@example.com"} })
	if code, _ := adminPost(t, s, "/settings/signals/again", url.Values{"name": {"probe"}}, "bo@example.com"); code != http.StatusForbidden {
		t.Errorf("an editor: %d", code)
	}
	h.mu.Lock()
	h.asked = nil
	h.mu.Unlock()
	if code, body := adminPost(t, s, "/settings/signals/again", url.Values{"name": {"probe"}}, "ana@example.com"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	h.mu.Lock()
	asked := slices.Clone(h.asked)
	h.mu.Unlock()
	if !slices.Contains(asked, "probe_success == 0") || slices.Contains(asked, "http_requests_total > 1e9") {
		t.Errorf("read: %q", asked)
	}
	cs, err := s.l.Changes(ctx, 10)
	if err != nil || len(cs) == 0 || cs[0].What != "setting impact signal probe read again" || cs[0].By != "ana@example.com" {
		t.Errorf("activity: %+v %v", cs, err)
	}
	if code, body := adminPost(t, s, "/settings/signals/again", url.Values{"name": {"ghost"}}, "ana@example.com"); code != http.StatusBadRequest || !strings.Contains(body, "no signal named") {
		t.Errorf("none such: %d", code)
	}
	// Counted among the reads an admin may ask for in an hour.
	for i := 0; i < impactTriesAnHour; i++ {
		adminPost(t, s, "/settings/signals/again", url.Values{"name": {"probe"}}, "ana@example.com")
	}
	if code, body := adminPost(t, s, "/settings/signals/again", url.Values{"name": {"probe"}}, "ana@example.com"); code != http.StatusBadRequest || !strings.Contains(body, "as many incident criteria") {
		t.Errorf("past the reads of an hour: %d", code)
	}
}

// The Incident sources panel gives a row each: when it was last read, its
// windows in four weeks, failing with Prometheus's error, where the data
// of each selector begins, and from when it keeps the record.
func TestSignalSources(t *testing.T) {
	h := signalWindowsHistory()
	s := signalServer(t, h, nil)
	h.mu.Lock()
	h.fail = map[string]bool{"probe_success == 0": true}
	h.mu.Unlock()
	runDue(s, 1)
	s.jobs(context.Background(), true)
	_, body := askAs(t, s.Handler(), http.MethodGet, "/settings", nil, "ana@example.com", "same-origin")
	panel := between(body, `id="incident-sources"`, "</table>")
	for _, want := range []string{"<code><bdi>errors</bdi></code>", "<code><bdi>probe</bdi></code>", "query timed out in expression evaluation", "It keeps the record from", "data since"} {
		if !strings.Contains(panel, want) {
			t.Errorf("the panel doesn't say %q:\n%s", want, panel)
		}
	}
	if strings.Contains(panel, "Off: set it under Incident criteria.") {
		t.Error("the panel says the impact signal is off")
	}
}

// A waiting proposal is held when a signal's match changes on the Settings
// page, as when the command line changes it (staleImpact).
func TestSignalChangeHoldsProposals(t *testing.T) {
	ctx := context.Background()
	s := signalServer(t, signalWindowsHistory(), nil)
	key, err := s.impactHeld(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id := impactProposed(t, s, key)
	if w := staleOf(t, s, id); w != nil {
		t.Fatalf("stale before: %+v", w)
	}
	if code, body := adminPost(t, s, "/settings/signals", signalPost("probe", "probe", "probe_success == 0", "2m", "service, instance", true), "ana@example.com"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	if w := staleOf(t, s, id); w == nil || w.Format != staleImpact {
		t.Errorf("after: %+v", w)
	}
}

// A deployment with no named signal: the Settings page's form for the one
// with no name is as it was, and settings.yaml says nothing of signals.
func TestUnnamedSettingsUnchanged(t *testing.T) {
	s := criteriaServer(t, false, nil)
	_, body := askAs(t, s.Handler(), http.MethodGet, "/settings", nil, "ana@example.com", "same-origin")
	if !strings.Contains(body, `<form method="post" action="/settings/impact" class="criteria">`) || strings.Contains(body, `id="impact-signals"`) {
		t.Error("the form of the one with no name")
	}
	_, y, _ := get(t, s.Handler(), "/settings.yaml")
	if strings.Contains(y, "impact") {
		t.Errorf("settings.yaml: %s", y)
	}
}

// A try of a signal that follows the match in effect counts what it reads
// by those labels: a name in the match in effect that isn't a label's
// never reaches a query (source.SeenFrom), on the Settings page as in the
// runs.
func TestSignalsTryMatchNeverInPromQL(t *testing.T) {
	h := shopHistory()
	s := signalServer(t, h, nil)
	inj := "a)\t(up)\tor\tvector(1)\tor\tcount\tby\t(b"
	if code, body := askAs(t, s.Handler(), http.MethodPost, "/settings", url.Values{"match": {inj}}, "ana@example.com", "same-origin"); code != http.StatusSeeOther {
		t.Fatalf("%d %.200s", code, body)
	}
	h.mu.Lock()
	h.asked = nil
	h.mu.Unlock()
	// A new match for the probe, the match in effect: what it reads is
	// counted by it.
	q := signalPost("probe", "probe", "probe_success == 0", "2m", "", true)
	q.Set("signal-try", "1")
	if code, body := askAs(t, s.Handler(), http.MethodGet, "/settings?"+q.Encode(), nil, "ana@example.com", "same-origin"); code != http.StatusOK || !strings.Contains(body, "a new match") {
		t.Fatalf("%d %s", code, between(body, `id="signals-tried"`, "</section>"))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, q := range h.asked {
		if strings.Contains(q, "vector(1)") || strings.Contains(q, "(up)") {
			t.Errorf("asked %q", q)
		}
	}
}

// A named signal the UI kept, and the rule entries, are no judging
// setting --lead and the others are: reading the settings in effect takes
// none of them for one that doesn't parse.
func TestSignalSettingsNotJudgingSettings(t *testing.T) {
	s := signalServer(t, shopHistory(), nil)
	if code, body := adminPost(t, s, "/settings/impact-rules", url.Values{"rules": {"- {rules: ^Errors$, signals: [probe]}\n"}}, "ana@example.com"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	if code, _ := adminPost(t, s, "/settings/signals", url.Values{"was": {"errors"}, "remove": {"1"}}, "ana@example.com"); code != http.StatusSeeOther {
		t.Fatal(code)
	}
	var logged strings.Builder
	s.log = slog.New(slog.NewTextHandler(&logged, nil))
	if _, err := s.options(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logged.String(), "doesn't parse") {
		t.Errorf("logged: %s", logged.String())
	}
}

// The form posts the expression back whatever was changed: the one serve
// was started with, which the UI's checks aren't for (a range of three
// days), is no change, and a change of --impact-for alone is saved; an
// expression an admin changed is checked.
func TestImpactStartedExprNotChecked(t *testing.T) {
	ctx := context.Background()
	started := "max_over_time(impact[3d]) > 1"
	s := criteriaServer(t, false, func(c *Config) { c.Impact = started })
	if code, body := askAs(t, s.Handler(), http.MethodPost, "/settings/impact", url.Values{"impact": {started + "\r\n"}, "impact-for": {"10m"}}, "ana@example.com", "same-origin"); code != http.StatusSeeOther {
		t.Fatalf("for alone: %d %s", code, between(body, "<main", "</main>"))
	}
	set, err := s.l.Settings(ctx)
	if _, kept := set["impact"]; err != nil || kept || set["impact-for"].Value != "10m" {
		t.Errorf("settings: %+v %v", set, err)
	}
	if code, body := askAs(t, s.Handler(), http.MethodPost, "/settings/impact", url.Values{"impact": {"max_over_time(impact[4d]) > 1"}}, "ana@example.com", "same-origin"); code != http.StatusBadRequest || !strings.Contains(body, "reads a range of 4d") {
		t.Errorf("changed: %d", code)
	}
}

// The same of a named signal's form: its expression as serve was started
// with it, posted back unchanged, isn't refused; changed, it is checked.
func TestSignalStartedExprNotChecked(t *testing.T) {
	started := "max_over_time(probe_success[3d]) == 0"
	s := signalServer(t, shopHistory(), func(c *Config) { c.Signals[1].Expr = started })
	if code, body := adminPost(t, s, "/settings/signals", signalPost("probe", "probe", started, "3m", "service", true), "ana@example.com"); code != http.StatusSeeOther {
		t.Fatalf("for alone: %d %s", code, between(body, "<main", "</main>"))
	}
	if d, _ := inEffect(t, s).def("probe"); d.For != 3*time.Minute || d.Expr != started {
		t.Errorf("in effect: %+v", d)
	}
	if code, body := adminPost(t, s, "/settings/signals", signalPost("probe", "probe", "max_over_time(probe_success[4d]) == 0", "3m", "service", true), "ana@example.com"); code != http.StatusBadRequest || !strings.Contains(body, "reads a range of 4d") {
		t.Errorf("changed: %d", code)
	}
}
