package serve

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
)

// labelledRules are rules that say whose they are in a label, as a real
// set often does: two teams' by team=, one by owner=, one by both, one
// whose value can't be a team's name, and one with none.
const labelledRules = `groups:
  - name: shop
    rules:
      - {alert: PayErrors, expr: errors > 5, labels: {team: payments, severity: page}}
      - {alert: PayLatency, expr: latency > 1, labels: {team: payments}}
      - {alert: SearchDown, expr: up == 0, labels: {team: search}}
      - {alert: CartSlow, expr: slow > 1, labels: {owner: cart}}
      - {alert: Both, expr: both > 1, labels: {team: payments, owner: payments}}
      - {alert: Odd, expr: odd > 1, labels: {team: "a/b"}}
      - {alert: Plain, expr: plain > 1}
`

// labelledServer is a server whose rules are labelledRules, read and
// replayed, with more besides.
func labelledServer(t *testing.T, rulesYAML string, change func(*Config)) *Server {
	t.Helper()
	s, rules := server(t, change)
	if err := os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte(rulesYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	return s
}

// TestTeamsSuggested: with no team, the Teams page suggests the teams the
// rules' labels name, each with the rules it would own; an admin makes them
// with one button, each as the team form would, and the result is said
// once; then nothing is suggested, and a rule's page says its label is why
// it is its team's.
func TestTeamsSuggested(t *testing.T) {
	ctx := context.Background()
	s := labelledServer(t, labelledRules, nil)
	h := s.Handler()
	o, err := s.owners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sg := s.suggestTeams(o)
	got := map[string]string{}
	for _, l := range sg.Lines {
		got[l.Value] = fmt.Sprintf("%s=%s %d", l.Key, l.Value, l.Rules)
	}
	want := map[string]string{"payments": "team=payments 3", "search": "team=search 1", "cart": "owner=cart 1"}
	if fmt.Sprint(got) != fmt.Sprint(want) || sg.Make != 3 || sg.Unnamed != 1 || sg.More != 0 {
		t.Fatalf("suggested %v (make %d, unnamed %d, more %d), want %v, 3, 1, 0", got, sg.Make, sg.Unnamed, sg.More, want)
	}
	if sg.Lines[0].Value != "payments" {
		t.Errorf("the team with the most rules isn't first: %+v", sg.Lines)
	}

	// The rules list, with no team, says why its Team column is empty, and
	// links to them; so does a rule's page no team owns.
	_, body, _ := get(t, h, "/rules")
	if !strings.Contains(body, "No team owns a rule yet, so the Team column is empty.") || !strings.Contains(body, `href="/teams#suggested">The rules&#39; labels suggest 3 teams</a>`) {
		t.Errorf("/rules doesn't say no team owns a rule, linking to the 3 suggested:\n%s", body)
	}
	_, body, _ = get(t, h, "/rules/Plain")
	if !strings.Contains(body, "No team owns it. No team&#39;s name pattern or labels match it.") || !strings.Contains(body, `href="/teams#suggested"`) {
		t.Errorf("a rule no team owns doesn't say why, linking to the teams suggested:\n%s", body)
	}

	_, body, _ = get(t, h, "/teams")
	for _, x := range []string{`id="suggested"`, "Teams these rules&#39; labels suggest", "<bdi>payments</bdi>", "<code><bdi>owner=cart</bdi></code>",
		`<form method="post" action="/teams/suggested"><button>Make these teams</button></form>`,
		"1 label value can&#39;t be a team&#39;s name"} {
		if !strings.Contains(body, x) {
			t.Errorf("/teams lacks %q", x)
		}
	}
	if strings.Contains(body, "a/b") {
		t.Error("/teams suggests a value that can't be a team's name")
	}

	// From another site, refused, nothing made.
	if code, _, _ := post(t, h, "/teams/suggested", url.Values{}, "cross-site"); code != http.StatusForbidden {
		t.Errorf("a cross-site POST: %d, want 403", code)
	}
	if teams, _ := s.l.Teams(ctx); len(teams) != 0 {
		t.Fatalf("a refused POST made %d teams", len(teams))
	}
	code, _, hd := post(t, h, "/teams/suggested", url.Values{}, "same-origin")
	if code != http.StatusSeeOther || hd.Get("Location") != "/teams?made=3&skipped=0#suggested" {
		t.Fatalf("making them: %d, to %q", code, hd.Get("Location"))
	}
	teams, err := s.l.Teams(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var made []string
	for _, tm := range teams {
		made = append(made, fmt.Sprintf("%s %s %q", tm.Name, labelText(tm.Labels), tm.Rules))
	}
	if fmt.Sprint(made) != `[cart owner=cart "" payments team=payments "" search team=search ""]` {
		t.Errorf("made %v", made)
	}
	_, body, _ = get(t, h, "/teams?made=3&skipped=0")
	if strings.Count(body, "3 teams made from the rules&#39; labels.") != 1 || strings.Contains(body, `id="suggested"`) {
		t.Errorf("/teams after making them doesn't say so once, or still suggests some:\n%s", body)
	}
	// Each rule is its team's, by the label, as the page says.
	o, _ = s.owners(ctx)
	for rule, team := range map[string]string{"PayErrors": "payments", "PayLatency": "payments", "Both": "payments", "SearchDown": "search", "CartSlow": "cart", "Odd": "", "Plain": ""} {
		if got := o.teamOf(rule); got != team {
			t.Errorf("%s is %q's, want %q's", rule, got, team)
		}
	}
	_, body, _ = get(t, h, "/rules/PayErrors")
	if !strings.Contains(body, "Its labels have <bdi>team=payments</bdi>, which <bdi>payments</bdi> owns by.") {
		t.Errorf("a rule's page doesn't say its label makes it its team's:\n%s", body)
	}
	_, body, _ = get(t, h, "/rules")
	if strings.Contains(body, "No team owns a rule yet") {
		t.Error("/rules says no team owns a rule, with teams")
	}
	// Asked again, nothing is left to make.
	if _, _, hd := post(t, h, "/teams/suggested", url.Values{}, "same-origin"); hd.Get("Location") != "/teams?made=0&skipped=0#suggested" {
		t.Errorf("making them again went to %q", hd.Get("Location"))
	}
}

// TestTeamsSuggestedAsOwnersWould: a rule a note or a name pattern gives a
// team isn't counted for a team its labels suggest, a value a team owns by
// isn't suggested, one whose name a team has already is said so and not
// made, and --team-labels names the labels read. Each rule's page says how
// its team was found, as owners found it.
func TestTeamsSuggestedAsOwnersWould(t *testing.T) {
	ctx := context.Background()
	s := labelledServer(t, labelledRules, nil)
	h := s.Handler()
	pay, err := s.l.PutTeam(ctx, ledger.Team{Name: "pay", Rules: "^PayE"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "search", Rules: "^Nothing"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "carts", Labels: incident.Labels{"owner": {"cart"}}}, "ana"); err != nil {
		t.Fatal(err)
	}
	if err := s.l.PutNote(ctx, ledger.Note{Rule: "PayLatency", TeamID: pay}, "ana"); err != nil {
		t.Fatal(err)
	}
	o, err := s.owners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sg := s.suggestTeams(o)
	var got []string
	for _, l := range sg.Lines {
		got = append(got, fmt.Sprintf("%s=%s %d %v", l.Key, l.Value, l.Rules, l.Taken != nil))
	}
	// payments: Both alone (PayErrors is pay's by its pattern, PayLatency by
	// its note); search's name is a team's; cart is carts'.
	if fmt.Sprint(got) != "[team=payments 1 false team=search 1 true]" || sg.Make != 1 {
		t.Errorf("suggested %v (make %d)", got, sg.Make)
	}
	w := o.walk()
	for name := range o.ruleLabels {
		if how := w.how(name); how.Team != o.teamOf(name) {
			t.Errorf("%s: found %+v, owners say %q", name, how, o.teamOf(name))
		}
	}
	for rule, said := range map[string]string{
		"PayErrors":  "Its name matches <bdi>pay</bdi>&#39;s pattern, <bdi>^PayE</bdi>.",
		"PayLatency": "Its note names the team.",
		"CartSlow":   "Its labels have <bdi>owner=cart</bdi>, which <bdi>carts</bdi> owns by.",
		"Plain":      "No team owns it. No team&#39;s name pattern or labels match it.",
	} {
		_, body, _ := get(t, h, "/rules/"+rule)
		if !strings.Contains(body, said) {
			t.Errorf("/rules/%s doesn't say %q", rule, said)
		}
		if strings.Contains(body, `href="/teams#suggested"`) {
			t.Errorf("/rules/%s links to the teams suggested, with teams", rule)
		}
	}
	_, body, _ := get(t, h, "/teams")
	if !strings.Contains(body, "A team called <bdi>search</bdi> is there already") || !strings.Contains(body, `href="/teams?edit=`) {
		t.Errorf("/teams doesn't say search is a team already, with a way to change it:\n%s", body)
	}
	if _, _, hd := post(t, h, "/teams/suggested", url.Values{}, "same-origin"); hd.Get("Location") != "/teams?made=1&skipped=1#suggested" {
		t.Errorf("making them went to %q", hd.Get("Location"))
	}
	_, body, _ = get(t, h, "/teams?made=1&skipped=1")
	if !strings.Contains(body, "1 team made from the rules&#39; labels. 1 passed over: a team of its name was there already.") {
		t.Errorf("/teams doesn't say what making them did:\n%s", body)
	}

	// --team-labels: only the labels it names are read.
	s = labelledServer(t, labelledRules, func(c *Config) { c.TeamLabels = []string{"owner"} })
	o, _ = s.owners(ctx)
	got = nil
	for _, l := range s.suggestTeams(o).Lines {
		got = append(got, l.Key+"="+l.Value)
	}
	if fmt.Sprint(got) != "[owner=cart owner=payments]" {
		t.Errorf("--team-labels owner: suggested %v", got)
	}
}

// TestTeamsSuggestedAdminsOnly: making the teams is an admin's, the server
// checks it, and anyone else sees the button disabled, with why.
func TestTeamsSuggestedAdminsOnly(t *testing.T) {
	ctx := context.Background()
	s := labelledServer(t, labelledRules, func(c *Config) {
		c.UserHeader, c.Admins, c.Editors = "X-Forwarded-Email", []string{"ana@example.com"}, []string{"*"}
	})
	h := s.Handler()
	_, body := getAs(t, h, "/teams", "bo@example.com")
	if !strings.Contains(body, `<button type="button" disabled aria-describedby="suggested-why">Make these teams</button> <span id="suggested-why" class="why">Admins only</span>`) {
		t.Errorf("an editor isn't shown the button disabled, with why:\n%s", body)
	}
	if code, _, _ := postAs(t, h, "/teams/suggested", url.Values{}, "bo@example.com", "same-origin"); code != http.StatusForbidden {
		t.Errorf("an editor making them: %d, want 403", code)
	}
	if teams, _ := s.l.Teams(ctx); len(teams) != 0 {
		t.Fatalf("an editor made %d teams", len(teams))
	}
	if code, _, _ := postAs(t, h, "/teams/suggested", url.Values{}, "ana@example.com", "same-origin"); code != http.StatusSeeOther {
		t.Errorf("an admin making them: %d", code)
	}
	if teams, _ := s.l.Teams(ctx); len(teams) != 3 {
		t.Errorf("an admin made %d teams, want 3", len(teams))
	}
}

// TestTeamsSuggestedBounded: past maxSuggested values, those with the most
// rules are suggested and made, the rest counted; a crafted result says no
// more than maxSuggested.
func TestTeamsSuggestedBounded(t *testing.T) {
	ctx := context.Background()
	var b strings.Builder
	b.WriteString("groups:\n  - name: many\n    rules:\n")
	for i := range maxSuggested + 10 {
		fmt.Fprintf(&b, "      - {alert: R%02d, expr: r%02d > 1, labels: {team: t%02d}}\n", i, i, i)
	}
	fmt.Fprintf(&b, "      - {alert: Extra, expr: extra > 1, labels: {team: t%02d}}\n", maxSuggested+5)
	s := labelledServer(t, b.String(), nil)
	o, err := s.owners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sg := s.suggestTeams(o)
	if len(sg.Lines) != maxSuggested || sg.More != 10 || sg.Make != maxSuggested || sg.Lines[0].Value != fmt.Sprintf("t%02d", maxSuggested+5) {
		t.Fatalf("%d suggested, %d more, %d made, first %q", len(sg.Lines), sg.More, sg.Make, sg.Lines[0].Value)
	}
	if _, _, hd := post(t, s.Handler(), "/teams/suggested", url.Values{}, "same-origin"); hd.Get("Location") != fmt.Sprintf("/teams?made=%d&skipped=0#suggested", maxSuggested) {
		t.Errorf("making them went to %q", hd.Get("Location"))
	}
	for _, q := range []string{"made=51&skipped=0", "made=40&skipped=11", "made=-1&skipped=0", "made=x"} {
		if v, _ := url.ParseQuery(q); teamsMadeOf(v) != nil {
			t.Errorf("%s: said", q)
		}
	}
}

// inEachLanguage asks for pages, as who (no one, for ""), in each language,
// and fails on a text one of them shows with no translation, as
// TestPagesInEachLanguage does for the pages it knows.
func inEachLanguage(t *testing.T, h http.Handler, who string, paths ...string) {
	t.Helper()
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	for _, n := range names {
		for _, p := range paths {
			r := httptest.NewRequest(http.MethodGet, p, nil)
			r.Header.Set("Accept-Language", n.Code)
			if who != "" {
				r.Header.Set("X-Forwarded-Email", who)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `<html lang="`+n.Code+`">`) {
				t.Errorf("%s %s: %d, not in %s", n.Code, p, w.Code, n.Code)
			}
		}
	}
	missing.Lock()
	defer missing.Unlock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		t.Errorf("%s has no translation for %q", lang, msg)
	}
}

// TestTeamsSuggestedInEachLanguage: what the teams suggested, making them
// and why a rule is a team's say is in every language.
func TestTeamsSuggestedInEachLanguage(t *testing.T) {
	ctx := context.Background()
	var b strings.Builder
	b.WriteString(labelledRules)
	for i := range maxSuggested + 1 {
		fmt.Fprintf(&b, "      - {alert: R%02d, expr: r%02d > 1, labels: {squad: s%02d}}\n", i, i, i)
	}
	s := labelledServer(t, b.String(), func(c *Config) {
		c.UserHeader, c.Admins, c.Editors = "X-Forwarded-Email", []string{"ana@example.com"}, []string{"*"}
	})
	h := s.Handler()
	inEachLanguage(t, h, "ana@example.com", "/teams", "/rules", "/rules/Plain", "/teams?made=1&skipped=1", "/teams?made=3&skipped=2")
	inEachLanguage(t, h, "bo@example.com", "/teams")
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "search", Rules: "^Nothing"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "pay", Rules: "^PayE"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "carts", Labels: incident.Labels{"owner": {"cart"}}}, "ana"); err != nil {
		t.Fatal(err)
	}
	gone, err := s.l.PutTeam(ctx, ledger.Team{Name: "gone", Rules: "^Gone"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.l.PutNote(ctx, ledger.Note{Rule: "Both", TeamID: gone}, "ana"); err != nil {
		t.Fatal(err)
	}
	inEachLanguage(t, h, "ana@example.com", "/teams", "/rules/PayErrors", "/rules/CartSlow", "/rules/Both", "/rules/Plain")
	if err := s.l.DeleteTeam(ctx, gone, "ana"); err != nil {
		t.Fatal(err)
	}
	inEachLanguage(t, h, "ana@example.com", "/rules/Both")
}
