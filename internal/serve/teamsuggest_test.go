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
	"time"

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

// suggested are the teams suggested, each as key=value, its rules and why
// it isn't made.
func suggested(sg teamSuggestions) string {
	var out []string
	for _, l := range sg.Lines {
		out = append(out, fmt.Sprintf("%s=%s %d %s", l.Key, l.Value, l.Rules, l.Why))
	}
	return strings.Join(out, "; ")
}

// withoutFrag is a Location as a browser asks for it, without its fragment.
func withoutFrag(loc string) string { return strings.SplitN(loc, "#", 2)[0] }

// madeTo is where "Make these teams" went: the made teams' names, as the
// ids it names are, and the page it went to.
func madeTo(t *testing.T, s *Server, loc string) []string {
	t.Helper()
	u, err := url.Parse(loc)
	if err != nil || u.Path != "/teams" || u.Fragment != "suggested" {
		t.Fatalf("making them went to %q", loc)
	}
	teams, err := s.l.Teams(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, v := range u.Query()["made"] {
		for _, tm := range teams {
			if itoa(tm.ID) == v {
				names = append(names, tm.Name)
			}
		}
	}
	return names
}

// TestTeamsSuggested: with no team, the Teams page suggests the teams the
// rules' labels name, each with the rules it would own; an admin makes them
// with one button, each as the team form would, and the result names the
// teams made, once; a value that isn't a plain name is left to make by
// hand, said so; and a rule's page says its label is why it is its team's.
func TestTeamsSuggested(t *testing.T) {
	ctx := context.Background()
	s := labelledServer(t, labelledRules, nil)
	h := s.Handler()
	o, err := s.owners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sg := s.suggestTeams(o)
	if got := suggested(sg); got != "team=payments 3 ; team=a/b 1 hand; owner=cart 1 ; team=search 1 " || sg.Make != 3 || sg.Long != 0 || sg.More != 0 {
		t.Fatalf("suggested %q (make %d, long %d, more %d)", got, sg.Make, sg.Long, sg.More)
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
	// Nothing judged yet, the list has no table, and no Team column to say
	// anything of.
	empty, _ := server(t, nil)
	if _, body, _ := get(t, empty.Handler(), "/rules"); !strings.Contains(body, "Nothing judged yet") || strings.Contains(body, "Team column") {
		t.Errorf("/rules before the first run speaks of its Team column:\n%s", body)
	}

	_, body, _ = get(t, h, "/teams")
	for _, x := range []string{`id="suggested"`, "Teams these rules&#39; labels suggest", "<bdi>payments</bdi>", "<code><bdi>owner=cart</bdi></code>",
		`<form method="post" action="/teams/suggested"><button>Make these teams</button></form>`,
		"<bdi>a/b</bdi><br><span class=\"quiet\">Make it by hand, with the form below: its value isn&#39;t a plain lowercase name"} {
		if !strings.Contains(body, x) {
			t.Errorf("/teams lacks %q", x)
		}
	}

	// From another site, refused, nothing made.
	if code, _, _ := post(t, h, "/teams/suggested", url.Values{}, "cross-site"); code != http.StatusForbidden {
		t.Errorf("a cross-site POST: %d, want 403", code)
	}
	if teams, _ := s.l.Teams(ctx); len(teams) != 0 {
		t.Fatalf("a refused POST made %d teams", len(teams))
	}
	code, _, hd := post(t, h, "/teams/suggested", url.Values{}, "same-origin")
	if made := madeTo(t, s, hd.Get("Location")); code != http.StatusSeeOther || strings.Join(made, " ") != "payments cart search" {
		t.Fatalf("making them: %d, made %v", code, made)
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
	_, body, _ = get(t, h, withoutFrag(hd.Get("Location")))
	if strings.Count(body, "3 teams made from the rules&#39; labels: <bdi>payments, cart, search</bdi>.") != 1 || strings.Contains(body, "<bdi>payments</bdi><br>") {
		t.Errorf("/teams after making them doesn't name them once, or still suggests them:\n%s", body)
	}
	// A link can't say more was made than was, by the reader.
	for _, q := range []string{"made=50&skipped=0", "made=999", "made=" + itoa(teams[0].ID) + "&made=x", "made=-1"} {
		if _, body, _ := get(t, h, "/teams?"+q); strings.Contains(body, "made from the rules") {
			t.Errorf("/teams?%s says teams were made", q)
		}
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
	if _, _, hd := post(t, h, "/teams/suggested", url.Values{}, "same-origin"); hd.Get("Location") != "/teams?made=0#suggested" {
		t.Errorf("making them again went to %q", hd.Get("Location"))
	}
	if _, body, _ := get(t, h, "/teams?made=0"); !strings.Contains(body, "No team made from the rules&#39; labels: each one left says why.") {
		t.Error("/teams?made=0 doesn't say none was made")
	}
}

// TestTeamsSuggestedAsOwnersWould: only the rules no team owns, by a note,
// a name pattern or labels, are counted for a team their labels suggest; a
// value whose name a team has already, in any case, isn't made, and is said
// what to do as that team owns; one whose label is on rules another team
// owns isn't made, as it would take them; one that isn't a plain lowercase
// name (another script's letter, a capital) is left to make by hand; a
// value longer than a team's name is counted and left out; and
// --team-labels names the labels read. Each rule's page says how its team
// was found, as owners found it.
func TestTeamsSuggestedAsOwnersWould(t *testing.T) {
	ctx := context.Background()
	s := labelledServer(t, labelledRules+`      - {alert: Charge, expr: charge > 1, labels: {team: payments, service: checkout}}
      - {alert: Web1, expr: web1 > 1, labels: {team: Web}}
      - {alert: Pay2, expr: pay2 > 1, labels: {team: "pаyments"}}
      - {alert: Shop1, expr: shop1 > 1, labels: {team: shop}}
      - {alert: Long1, expr: long1 > 1, labels: {team: `+strings.Repeat("l", 65)+`}}
`, nil)
	h := s.Handler()
	pay, err := s.l.PutTeam(ctx, ledger.Team{Name: "pay", Rules: "^PayE"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	for _, tm := range []ledger.Team{{Name: "search", Rules: "^Nothing"}, {Name: "carts", Labels: incident.Labels{"owner": {"cart"}}},
		{Name: "zeta", Labels: incident.Labels{"service": {"checkout"}}}, {Name: "web", Rules: "^Nothing"},
		{Name: "SHOP", Labels: incident.Labels{"env": {"prod"}}}} {
		if _, err := s.l.PutTeam(ctx, tm, "ana"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.l.PutNote(ctx, ledger.Note{Rule: "PayLatency", TeamID: pay}, "ana"); err != nil {
		t.Fatal(err)
	}
	o, err := s.owners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sg := s.suggestTeams(o)
	// payments: Both alone (PayErrors is pay's by its pattern, PayLatency by
	// its note, Charge zeta's by its labels), and payments would take Charge
	// from zeta; search, Web (web's, in another case) and shop (SHOP's) are
	// teams' names; pаyments and a/b aren't plain names; carts owns cart.
	if got := suggested(sg); got != "team=Web 1 taken; team=a/b 1 hand; team=payments 1 takes; team=pаyments 1 hand; team=search 1 taken; team=shop 1 taken" ||
		sg.Make != 0 || sg.Long != 1 {
		t.Errorf("suggested %q (make %d, long %d)", got, sg.Make, sg.Long)
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
	for _, x := range []string{
		"Not made: a team called <bdi>search</bdi> is there already. Give it the label <bdi>team=search</bdi> to own these rules.",
		"Not made: a team called <bdi>SHOP</bdi> is there already, and owns by other labels; a team owns the rules with all of its labels",
		"Not made: rules another team owns now have this label, and a team made of it would take them.",
		"1 rule&#39;s label has a value longer than a team&#39;s name may be (64 bytes)"} {
		if !strings.Contains(body, x) {
			t.Errorf("/teams doesn't say %q", x)
		}
	}
	if !strings.Contains(body, `href="/teams?edit=`) || strings.Contains(body, "Make these teams") {
		t.Errorf("/teams offers to make what it says it doesn't, or no way to change a team:\n%s", body)
	}
	if _, _, hd := post(t, h, "/teams/suggested", url.Values{}, "same-origin"); hd.Get("Location") != "/teams?made=0#suggested" {
		t.Errorf("making them went to %q", hd.Get("Location"))
	}
	if teams, _ := s.l.Teams(ctx); len(teams) != 6 {
		t.Errorf("%d teams, after making none", len(teams))
	}

	// --team-labels: only the labels it names are read.
	s = labelledServer(t, labelledRules, func(c *Config) { c.TeamLabels = []string{"owner"} })
	o, _ = s.owners(ctx)
	if got := suggested(s.suggestTeams(o)); got != "owner=cart 1 ; owner=payments 1 " {
		t.Errorf("--team-labels owner: suggested %q", got)
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
	code, _, hd := postAs(t, h, "/teams/suggested", url.Values{}, "ana@example.com", "same-origin")
	if code != http.StatusSeeOther {
		t.Errorf("an admin making them: %d", code)
	}
	// The result names the teams made to the one who made them alone.
	if _, body := getAs(t, h, withoutFrag(hd.Get("Location")), "ana@example.com"); !strings.Contains(body, "3 teams made from the rules") {
		t.Error("the result doesn't name the teams made to their maker")
	}
	if _, body := getAs(t, h, withoutFrag(hd.Get("Location")), "bo@example.com"); strings.Contains(body, "made from the rules") {
		t.Error("the result names teams another made")
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
	if _, _, hd := post(t, s.Handler(), "/teams/suggested", url.Values{}, "same-origin"); len(madeTo(t, s, hd.Get("Location"))) != maxSuggested {
		t.Errorf("making them went to %q", hd.Get("Location"))
	}
	o, _ = s.owners(ctx)
	ids := url.Values{}
	for _, tm := range o.teams {
		ids.Add("made", itoa(tm.ID))
	}
	if m := teamsMadeOf(ids, o, "anonymous"); m == nil || len(m.Names) != maxSuggested {
		t.Errorf("the %d made: %+v", maxSuggested, m)
	}
	ids.Add("made", ids["made"][0])
	if m := teamsMadeOf(ids, o, "anonymous"); m != nil {
		t.Errorf("past %d made, said %+v", maxSuggested, m)
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
	b.WriteString(labelledRules + `      - {alert: Charge, expr: charge > 1, labels: {team: payments, service: checkout}}
      - {alert: Web1, expr: web1 > 1, labels: {team: Web}}
      - {alert: Shop1, expr: shop1 > 1, labels: {team: shop}}
      - {alert: Long1, expr: long1 > 1, labels: {team: ` + strings.Repeat("l", 65) + `}}
      - {alert: Long2, expr: long2 > 1, labels: {team: ` + strings.Repeat("m", 65) + `}}
`)
	for i := range maxSuggested + 1 {
		fmt.Fprintf(&b, "      - {alert: R%02d, expr: r%02d > 1, labels: {squad: s%02d}}\n", i, i, i)
	}
	s := labelledServer(t, b.String(), func(c *Config) {
		c.UserHeader, c.Admins, c.Editors = "X-Forwarded-Email", []string{"ana@example.com"}, []string{"*"}
	})
	h := s.Handler()
	inEachLanguage(t, h, "ana@example.com", "/teams", "/rules", "/rules/Plain", "/teams?made=0")
	inEachLanguage(t, h, "bo@example.com", "/teams")
	for _, tm := range []ledger.Team{{Name: "search", Rules: "^Nothing"}, {Name: "pay", Rules: "^PayE"}, {Name: "carts", Labels: incident.Labels{"owner": {"cart"}}},
		{Name: "zeta", Labels: incident.Labels{"service": {"checkout"}}}, {Name: "web", Rules: "^Nothing"}, {Name: "SHOP", Labels: incident.Labels{"env": {"prod"}}}} {
		if _, err := s.l.PutTeam(ctx, tm, "ana@example.com"); err != nil {
			t.Fatal(err)
		}
	}
	gone, err := s.l.PutTeam(ctx, ledger.Team{Name: "gone", Rules: "^Gone"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.l.PutNote(ctx, ledger.Note{Rule: "Both", TeamID: gone}, "ana"); err != nil {
		t.Fatal(err)
	}
	inEachLanguage(t, h, "ana@example.com", "/teams", "/rules/PayErrors", "/rules/CartSlow", "/rules/Both", "/rules/Plain", "/teams?made="+itoa(gone-1))
	inEachLanguage(t, h, "bo@example.com", "/teams")
	_, _, hd := postAs(t, h, "/teams/suggested", url.Values{}, "ana@example.com", "same-origin")
	if len(madeTo(t, s, hd.Get("Location"))) < 2 {
		t.Fatalf("making them went to %q", hd.Get("Location"))
	}
	inEachLanguage(t, h, "ana@example.com", withoutFrag(hd.Get("Location")))
	if err := s.l.DeleteTeam(ctx, gone, "ana"); err != nil {
		t.Fatal(err)
	}
	inEachLanguage(t, h, "ana@example.com", "/rules/Both")
}

// TestTeamsMadeRecentlyAndFresh: what "Make these teams" did is said for
// the teams the reader made, or changed, in the last few minutes alone, as
// a team keeps who changed it last and when, not who made it; and making
// them reads the teams there now, not the owners kept for the ledger's
// generation, a team of a name in another case made meanwhile passed over.
func TestTeamsMadeRecentlyAndFresh(t *testing.T) {
	ctx := context.Background()
	s := labelledServer(t, labelledRules, nil)
	h := s.Handler()
	// The owners as they were, kept, and a team made meanwhile in another
	// case, as another replica, or request, would.
	o, err := s.owners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "Payments", Rules: "^Nothing"}, "bo"); err != nil {
		t.Fatal(err)
	}
	s.own.Lock()
	s.own.gen, s.own.o = s.l.Gen(), o
	s.own.Unlock()
	_, _, hd := post(t, h, "/teams/suggested", url.Values{}, "same-origin")
	made := madeTo(t, s, hd.Get("Location"))
	if strings.Join(made, " ") != "cart search" {
		t.Errorf("made %v, want cart and search, and no payments beside Payments", made)
	}
	loc := withoutFrag(hd.Get("Location"))
	if _, body, _ := get(t, h, loc); !strings.Contains(body, "2 teams made from the rules&#39; labels: <bdi>cart, search</bdi>.") {
		t.Errorf("the result isn't said:\n%s", body)
	}
	// Some time on, the same link says nothing.
	was := madeWithin
	madeWithin = -time.Second
	defer func() { madeWithin = was }()
	if _, body, _ := get(t, h, loc+"&x=1"); strings.Contains(body, "made from the rules") {
		t.Error("an old link says the reader made teams")
	}
}
