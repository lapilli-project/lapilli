package serve

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
)

// What a says change writes is worked out where the rule is kept each time
// the alert fires: a text that parses and calls no query, but repeats,
// sets a variable doubled again and again, or applies a function that
// grows what it is given, can stall Prometheus or Grafana, taking the
// alerts down. Such a text is refused, whoever proposes it.
func TestSaysTemplateCantGrow(t *testing.T) {
	bomb := strings.Repeat("{{range $labels}}", 22) + "x" + strings.Repeat("{{end}}", 22)
	for text, want := range map[string]string{
		bomb:                                     "repeats",
		`{{range 1000000000000}}x{{end}}`:        "repeats",
		`{{define "x"}}a{{end}}{{template "x"}}`: "repeats",
		`{{block "x" .}}a{{end}}`:                "repeats",
		`{{$a := "xx"}}{{$a = printf "%s%s" $a $a}}{{$a}}`:                    "sets a variable",
		`{{with $x := $labels.job}}{{$x}}{{end}}`:                             "sets a variable",
		`{{ $labels.job | js | js | js }}`:                                    "applies js",
		`{{ reReplaceAll "a" "aaaa" (reReplaceAll "a" "aaaa" $labels.job) }}`: "applies reReplaceAll",
		`{{ $labels.job | toUpper | reReplaceAll "A" "AAAA" }}`:               "applies reReplaceAll",
		`{{ $labels.job | html | graphLink }}`:                                "applies graphLink",
		`{{ call $labels.f }}`:                                                "calls call",
		`{{ printf "%0999d" 1 }}`:                                             "printf",
		`{{ printf "%.999f" $value }}`:                                        "printf",
		`{{ printf "%*d" 9999 1 }}`:                                           "printf",
		`{{ printf "%[1]*d" 9999 1 }}`:                                        "printf",
		`{{ printf $labels.format 1 }}`:                                       "printf",
		`{{ "%0999d" | printf }}`:                                             "printf",
	} {
		err := templateOK(text, rules.TemplatePrometheus)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%.60s: %v, want %q", text, err, want)
		}
	}
	for _, text := range []string{
		`{{ $labels.instance }} at {{ printf "%.2f" $value }} ({{ $value | humanizePercentage }})`,
		`{{ if gt $value 0.9 }}high{{ else if gt $value 0.5 }}raised{{ else }}low{{ end }} on {{ with $labels.job }}{{ . }}{{ end }}`,
		`100%% of {{ printf "%5.1f%%" $value }} {{ index $labels "pod" | toUpper | title }}`,
		`{{ humanizeDuration $value }} since {{ $externalURL }}, {{ len $labels }} labels {{ print $labels }}`,
		"No template at all, 100% plain.",
		`{{ reReplaceAll "(.*):.*" "$1" $labels.instance }} {{ $labels.instance | reReplaceAll ":.*" "" | toUpper }}`,
		`{{ graphLink "up == 0" }} {{ $labels.job | html }}`,
	} {
		if err := templateOK(text, rules.TemplatePrometheus); err != nil {
			t.Errorf("%s: %v", text, err)
		}
	}
	if err := templateOK(`{{ $values.A.Value }} on {{ index $labels "pod" }}`, rules.TemplateGrafana); err != nil {
		t.Errorf("Grafana's: %v", err)
	}

	// Through the form, as the reviewer's 571-byte text came.
	s, _ := changesServer(t, nil)
	h := s.Handler()
	if _, code, body := proposeSays(t, h, url.Values{"annotation": {"description"}, "text": {bomb}}); code != http.StatusBadRequest || !strings.Contains(body, "repeats") {
		t.Errorf("the bomb proposed: %d %s", code, body)
	}
}

// A proposal the ledger would refuse (the rule has one unfinished, or the
// person made as many as anyone may in the hour) is refused before the
// rules are read again and the backtest runs: refused again and again, it
// asks Prometheus nothing.
func TestProposalRefusedBeforeBacktest(t *testing.T) {
	ctx := context.Background()
	s, _ := changesServer(t, nil)
	h := s.Handler()
	hist := s.h.(*history)
	asked := func() int { hist.Lock(); defer hist.Unlock(); return hist.asked }
	first := proposeFor(t, h, "15m") // the rule now has an unfinished proposal
	before := asked()
	for i := range 5 {
		code, body, _ := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin",
			url.Values{"rule": {"Errors"}, "kind": {"for"}, "for": {fmt.Sprintf("%dm", 20+i)}})
		if code == http.StatusSeeOther || !strings.Contains(body, "one at a time") {
			t.Fatalf("%d %s", code, zzMsg(body))
		}
	}
	if _, err := s.l.Move(ctx, first, []string{ledger.StateProposed}, ledger.StateDropped, bo, ledger.Step{}); err != nil {
		t.Fatal(err)
	}
	// As many as anyone may in an hour, by eve, for other rules.
	for i := range ledger.ProposalsAnHour {
		if _, err := s.l.Propose(ctx, ledger.Proposal{Rule: fmt.Sprintf("Other%d", i), Kind: ledger.KindFor, Path: ledger.PathNone,
			MadeBy: "Eve@example.com", Before: "x", BeforeHash: "x", After: "y", Diff: "d"}); err != nil {
			t.Fatal(err)
		}
	}
	code, body, _ := ui(t, h, http.MethodPost, "/proposals", "eve@example.com", "same-origin",
		url.Values{"rule": {"Errors"}, "kind": {"says"}, "annotation": {"summary"}, "text": {"Errors high"}})
	if code == http.StatusSeeOther || !strings.Contains(body, "as many proposals") {
		t.Fatalf("past the hour's: %d %s", code, zzMsg(body))
	}
	if n := asked() - before; n != 0 {
		t.Errorf("refused proposals asked Prometheus %d times", n)
	}
}

// The names Lapilli and its tokens note changes under are refused from
// the sign-in proxy in any case, as the ledger folds names: a person named
// "Lapilli" isn't taken for Lapilli, past the hour's proposals.
func TestReservedNamesFolded(t *testing.T) {
	s, _ := changesServer(t, nil)
	for _, who := range []string{"Lapilli", "LAPILLI", "Tasks-Token", "Changes-Token", ledger.Proposer} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("X-Forwarded-Email", who)
		if u := s.user(r); u != "anonymous" {
			t.Errorf("%q taken as %q", who, u)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Forwarded-Email", "Lapilli Fan")
	if u := s.user(r); u != "Lapilli Fan" {
		t.Errorf("a name holding one: %q", u)
	}
}
