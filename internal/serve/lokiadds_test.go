package serve

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
)

// A new rule added to Loki: a Loki ruler's group through its API, and a
// Loki rule file by the git path, held to its expectations, backtested in
// LogQL, as the other rules' are (docs/design/expectations.md, step 5).

// lokiAdded is a rule to add that lokiLogs's one hour of the checkout's
// errors fires: LogQL, in a text.
const lokiAdded = "alert: ErrorsLow\nexpr: 'sum by (service) (count_over_time({app=\"checkout\"} |= \"error\" [5m])) > 5'\nfor: 2m\nlabels:\n  severity: ticket\n"

// A Loki ruler: its groups are places only in a namespace --changes-in
// names as loki:NAMESPACE; the rule is written into a group, or a new
// group, through Loki's API with the write token and the tenant, in LogQL
// (its backtest asked Loki), read back; undone by deleting it, the rest of
// the group as Loki holds it. The Mimir with a namespace of the same name
// isn't written to, nor is its group a place of the Loki's.
func TestAddInLokiRuler(t *testing.T) {
	ctx := context.Background()
	s, m, lk := lokiServer(t, []string{"loki:shop"}, nil)
	h := s.Handler()
	for _, o := range mustPlaces(t, s) {
		if o.Path == ledger.PathLoki && (o.Namespace != "shop" || o.Loki) {
			t.Errorf("a Loki place: %+v", o)
		}
	}
	id, code, body := proposeAdding(t, h, bo, url.Values{"place": {placeKeyOf(t, s, addPlace{Path: ledger.PathLoki, Namespace: "shop", Group: "api"})},
		"rule": {lokiAdded}, "expectations": {heldTo(60)}})
	if code != http.StatusSeeOther {
		t.Fatalf("proposing: %d\n%s", code, zzMsg(body))
	}
	p := state(t, s, id)
	ev := proposalEvidence(p)
	if p.Path != ledger.PathLoki || !strings.Contains(p.Diff, "+        - alert: \"ErrorsLow\"") || ev.Add == nil || len(ev.Add.Items) != 1 ||
		ev.Add.Items[0].State != "met" || addHeldWhy(ev) != "" || ev.Backtest == nil || ev.Backtest.After.Firings != 1 || ev.Backtest.Unfinished {
		t.Fatalf("the proposal: path %s, %+v, %+v\n%s", p.Path, ev.Add, ev.Backtest, p.Diff)
	}
	if _, page, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), ana, "", nil); !strings.Contains(page, "Loki&#39;s ruler&#39;s API") ||
		!strings.Contains(page, "It names no one") {
		t.Error("its page doesn't say it goes through Loki's ruler's API, and names no one")
	}
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	g := lk.group("shop", "api")
	if p = state(t, s, id); p.State != ledger.StateApplied || !strings.Contains(g, "ErrorsLow") || !strings.Contains(g, "LogSlow") ||
		!slices.Equal(lk.written(), []string{"POST shop/api"}) || len(m.written()) != 0 {
		t.Fatalf("added: %s %q, written %v, the Mimir %v\n%s", p.State, p.Reason, lk.written(), m.written(), g)
	}
	for _, r := range lk.requests() {
		if !strings.HasSuffix(r, " acme") {
			t.Errorf("a request without the tenant: %s", r)
		}
	}
	if u := undoNow(t, s, id, 1); u.Path != ledger.PathLoki || u.State != ledger.StateApplied {
		t.Fatalf("the undo: %s %s %q", u.Path, u.State, u.Reason)
	}
	if g = lk.group("shop", "api"); strings.Contains(g, "ErrorsLow") || !strings.Contains(g, "LogSlow") || state(t, s, id).State != ledger.StateUndone {
		t.Fatalf("undone, the group:\n%s", g)
	}
	// A new group, made for it, whole in the namespace.
	id, code, body = proposeAdding(t, h, bo, url.Values{"place": {placeKeyOf(t, s, addPlace{Path: ledger.PathLoki, Namespace: "shop", NewGroup: true})},
		"group": {"slow"}, "interval": {"30s"}, "rule": {lokiAdded}, "expectations": {heldTo(60)}})
	if code != http.StatusSeeOther {
		t.Fatalf("proposing a new group: %d\n%s", code, zzMsg(body))
	}
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	if g = lk.group("shop", "slow"); state(t, s, id).State != ledger.StateApplied || !strings.Contains(g, "ErrorsLow") || !strings.Contains(g, "interval: 30s") {
		t.Fatalf("a group made: %s\n%s", state(t, s, id).State, g)
	}
	// What breaks an expectation isn't approved, in Loki as anywhere.
	id, _, _ = proposeAdding(t, h, bo, url.Values{"place": {placeKeyOf(t, s, addPlace{Path: ledger.PathLoki, Namespace: "shop", Group: "api"})},
		"rule": {strings.Replace(lokiAdded, "ErrorsLow", "ErrorsLoud", 1)}, "expectations": {strings.Replace(heldTo(1), "ErrorsLow", "ErrorsLoud", 1)}})
	if p := state(t, s, id); addHeldWhy(proposalEvidence(p)) != addHeldBroken {
		t.Errorf("a broken expectation: %q", addHeldWhy(proposalEvidence(p)))
	}
}

// A Loki ruler's namespace --changes-in doesn't name as loki:NAMESPACE is
// no place, though a Mimir's of that name is: shop names the Mimir's.
func TestAddLokiRulerNotNamed(t *testing.T) {
	parallel(t)
	s, _, _ := lokiServer(t, nil, nil)
	for _, o := range mustPlaces(t, s) {
		if o.Path == ledger.PathLoki {
			t.Errorf("a Loki ruler's group is a place with no loki:NAMESPACE: %+v", o)
		}
	}
	if _, _, err := placeFor(mustPlaces(t, s), addPlace{Path: ledger.PathLoki, Namespace: "shop", Group: "api"}); !sameErr(err, errNoPlace) {
		t.Errorf("a namespace --changes-in doesn't name as Loki's: %v", err)
	}
}

// A Loki rule file, by the git path: its group a place beside, not the
// same as, a Prometheus file's; the rule written in LogQL, handed to a
// recipe by the hash of the file as Lapilli read it, read back from the
// file; undone by the file as it was. A new group fits; a new file isn't a
// place, as a Prometheus file's isn't; a keep_firing_for, which Loki drops
// from a file, is refused.
func TestAddInLokiFile(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, file := lokiFilesServer(t)
	h, port := s.Handler(), s.IncidentsHandler()
	var sawNew bool
	for _, o := range mustPlaces(t, s) {
		if o.Path == ledger.PathGit && (!o.Loki || o.File != "logs.yaml") {
			t.Errorf("a place that isn't the Loki file's: %+v", o)
		}
		sawNew = sawNew || o.NewGroup
	}
	if !sawNew {
		t.Error("no new group offered in a Loki rule file")
	}
	place := addPlace{Path: ledger.PathGit, File: "logs.yaml", Group: "logs", Loki: true}
	if place.Loki && placeKey(place) == placeKey(addPlace{Path: ledger.PathGit, File: "logs.yaml", Group: "logs"}) {
		t.Fatal("a Loki file's place is the Prometheus file's of that name")
	}
	if _, _, err := placeFor(mustPlaces(t, s), addPlace{Path: ledger.PathGit, File: "logs.yaml", Group: "logs"}); !sameErr(err, errNoPlace) {
		t.Errorf("the same path as a Prometheus file: %v", err)
	}
	if _, _, err := placeFor(mustPlaces(t, s), addPlace{Path: ledger.PathGit, File: "new.yaml", NewGroup: true, Group: "g", Loki: true}); !sameErr(err, errNoPlace) {
		t.Errorf("a new file: %v", err)
	}
	keep := lokiAdded + "keep_firing_for: 5m\n"
	if _, code, body := proposeAdding(t, h, bo, url.Values{"place": {placeKeyOf(t, s, place)}, "rule": {keep}, "expectations": {heldTo(60)}}); code != http.StatusBadRequest ||
		!strings.Contains(body, "keep_firing_for") {
		t.Errorf("a keep_firing_for in a Loki file: %d %s", code, zzMsg(body))
	}
	id, code, body := proposeAdding(t, h, bo, url.Values{"place": {placeKeyOf(t, s, place)}, "rule": {lokiAdded}, "expectations": {heldTo(60)}})
	if code != http.StatusSeeOther {
		t.Fatalf("proposing: %d\n%s", code, zzMsg(body))
	}
	p := state(t, s, id)
	ev := proposalEvidence(p)
	if p.Path != ledger.PathGit || p.DocHash != rules.Hash([]byte(lokiFile)) || !strings.HasPrefix(p.Document, "# Loki rules of checkout\n") ||
		!strings.Contains(p.Document, "keep_firing_for: 5m") || !strings.Contains(p.Document, "ErrorsLow") || addHeldWhy(ev) != "" ||
		ev.Backtest == nil || ev.Backtest.After.Firings != 1 {
		t.Fatalf("the proposal: %s %q %+v\n%s", p.Path, p.Reason, ev.Backtest, p.Diff)
	}
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	if p = state(t, s, id); p.State != ledger.StateReady || p.Backup != lokiFile {
		t.Fatalf("checked again: %s %q", p.State, p.Reason)
	}
	if got := handed(t, port); len(got) != 1 || got[0].Document != p.Document || got[0].DocHash != p.DocHash || got[0].Kind != ledger.KindAdd {
		t.Fatalf("handed out: %+v", got)
	}
	for _, st := range []string{"opened", "merged"} {
		in := map[string]string{"state": st}
		if st == "opened" {
			in["link"] = "https://git.example/logs/pull/2"
		}
		if code, body := recipe(t, port, http.MethodPatch, "/api/v1/changes/"+itoa(id), changesToken, in); code != 200 {
			t.Fatalf("%s: %d %s", st, code, body)
		}
	}
	os.WriteFile(file, []byte(p.Document), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	if p = state(t, s, id); p.State != ledger.StateApplied {
		t.Fatalf("read back: %s %q", p.State, p.Reason)
	}
	key := keyFor(t, h, id, ana, "undo")
	_, _, hd := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/undo", ana, "same-origin", url.Values{"key": {key}, "read": {"1"}})
	s.jobs(ctx, false)
	if u := state(t, s, locID(hd)); u.State != ledger.StateReady || u.Document != lokiFile {
		t.Fatalf("the undo: %s %q", u.State, u.Reason)
	}
}

// A Loki rule file is a place only while Loki's rules in effect are its
// files: with --loki-loaded-rules they are read from its API and the files
// aren't what Loki runs, so none is offered (as gitPlace's lock with
// --grafana and --ruler, which replace --rules). --grafana and --ruler
// don't touch Loki's files: they stay a place beside them.
func TestLokiFilePlaceLocks(t *testing.T) {
	parallel(t)
	s, _ := lokiFilesServer(t)
	rs, docs, err := s.readRules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var r rules.Rule
	var d rules.Doc
	for _, x := range rs {
		if x.Name == "CheckoutLogErrors" {
			r, d = x, docOf(x, docs)
		}
	}
	if !s.lokiFilePlace(r, d) {
		t.Fatal("a Loki rule file isn't a place")
	}
	s.cfg.LokiLoaded = true
	if s.lokiFilePlace(r, d) {
		t.Error("a Loki rule file is a place with --loki-loaded-rules")
	}
	s.cfg.LokiLoaded = false
	s.cfg.Grafana = "http://grafana.example"
	if !s.lokiFilePlace(r, d) {
		t.Error("--grafana takes a Loki rule file's place away")
	}
}

// With --ruler, a Loki rule file is still a place, and the Mimir's group
// beside it.
func TestLokiFilePlaceBesideRuler(t *testing.T) {
	parallel(t)
	m := mimirRuler(t, "/prometheus/config/v1/rules")
	dir := t.TempDir()
	os.WriteFile(dir+"/logs.yaml", []byte(lokiFile), 0o644)
	s := mimirServer(t, m, func(c *Config) { c.Loki, c.LokiRules = "http://loki:3100", dir })
	var file, ruler bool
	for _, o := range mustPlaces(t, s) {
		file = file || o.Path == ledger.PathGit && o.Loki
		ruler = ruler || o.Path == ledger.PathRuler
	}
	if !file || !ruler {
		t.Errorf("a Loki file place %v, a ruler place %v", file, ruler)
	}
}
