package serve

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"lapilli.dev/internal/ledger"
)

// TestRefiredWeeksInOrder: the rules list's refires say how many of the
// whole weeks had one, as "1 of 3", in every language; Korean and Japanese
// put the whole first, and said "0 of 3" as "3 of 0".
func TestRefiredWeeksInOrder(t *testing.T) {
	for lang, want := range map[string]string{
		"en": "re-fired: 11% of firings, 1 of 3 whole weeks",
		"ko": "재발화: 발화의 11%, 온전한 3주 중 1주",
		"ja": "再発火: 発火の 11%、まるごとの 3 週のうち 1 週",
		"es": "redisparos: 11% de los disparos, 1 de 3 semanas completas",
	} {
		if got := tr(lang, "re-fired: %s of firings, %d of %d whole weeks", "11%", 1, 3); got != want {
			t.Errorf("%s: %q, want %q", lang, got, want)
		}
	}
}

// TestCaughtNotNoise: in Korean, 잡음 alone reads as noise, the opposite of
// what a column of what caught something, or of the rule that caught an
// incident, means; those say 정탐 (of firings) and 탐지 (of incidents).
func TestCaughtNotNoise(t *testing.T) {
	for _, key := range []string{"Caught", "caught", "○ Caught", "● Nothing", "✓ Caught", "✓ caught", "○ Caught now", "○ Would catch",
		"caught it, %s", "caught it, %s, and noisy that week", "caught by %s", "%s caught it first, noisy that week"} {
		if got := tr("ko", key); strings.Contains(got, "잡음") {
			t.Errorf("%q is %q", key, got)
		}
	}
	if got := tr("ko", "○ Caught") + " " + tr("ko", "● Nothing"); got != "○\u00a0정탐 ●\u00a0오탐" {
		t.Errorf("the stones' columns: %q, as the overview names them", got)
	}
}

// TestProposalsNoneOpen: with proposals, none of them open, the list says
// so and links to the closed ones, with nothing to clear; with none at
// all, no filters are offered.
func TestProposalsNoneOpen(t *testing.T) {
	s, _ := changesServer(t, nil)
	if v := proposalsFor(t, s, ""); v.Total != 0 || v.Empty == nil || v.Empty.Title != "No proposals yet." {
		t.Fatalf("none: %+v", v.Empty)
	}
	if _, body, _ := ui(t, s.Handler(), http.MethodGet, "/proposals", cy, "", nil); strings.Contains(body, `class="facets pick"`) {
		t.Error("filters offered with no proposal to pick")
	}
	ctx := context.Background()
	id, err := s.l.Propose(ctx, ledger.Proposal{Rule: "Old", Kind: ledger.KindFor, Path: ledger.PathGit, MadeBy: bo})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.Move(ctx, id, []string{ledger.StateProposed}, ledger.StateDropped, cy, ledger.Step{}); err != nil {
		t.Fatal(err)
	}
	v := proposalsFor(t, s, "")
	if v.Total != 1 || v.Shown != 0 || v.Empty == nil || v.Empty.Title != "No proposal is waiting for approval or under way." ||
		len(v.Empty.Links) != 1 || !strings.Contains(v.Empty.Links[0].Href, "state=all") {
		t.Fatalf("none open: %d of %d, %+v", v.Shown, v.Total, v.Empty)
	}
	_, body, _ := ui(t, s.Handler(), http.MethodGet, "/proposals", cy, "", nil)
	if !strings.Contains(body, `class="facets pick"`) || strings.Count(body, ">Clear filters<") != 1 {
		t.Error("with one closed: the filters, with their one Clear filters")
	}
	for _, q := range []string{"state=refused", "state=nosuch"} {
		if v := proposalsFor(t, s, q); v.Empty == nil || v.Empty.Title != "No proposal matches these filters." {
			t.Errorf("%s: %+v", q, v.Empty)
		}
	}
}

// TestMatchInEffectSaid: a named signal's choice to follow the match in
// effect says what that is.
func TestMatchInEffectSaid(t *testing.T) {
	for match, want := range map[string]string{"service": "<code><bdi>service</bdi></code>", "": "no label"} {
		s := signalServer(t, shopHistory(), func(c *Config) {
			c.Options.Match = nil
			if match != "" {
				c.Options.Match = []string{match}
			}
		})
		said := `> the match in effect <span class="quiet">(` + want + `)</span></label>`
		code, body := askAs(t, s.Handler(), http.MethodGet, "/settings", nil, "ana@example.com", "same-origin")
		if code != http.StatusOK || !strings.Contains(body, said) {
			t.Errorf("--match %q: %d, the match in effect isn't said as %s", match, code, want)
		}
		// A form tried comes back as typed, saying it still.
		q := signalPost("", "pods", "pods_down > 0", "5m", "", true)
		q.Set("signal-try", "1")
		code, body = askAs(t, s.Handler(), http.MethodGet, "/settings?"+q.Encode(), nil, "ana@example.com", "same-origin")
		if add := between(body, `id="add-signal"`, "</details>"); code != http.StatusOK || !strings.Contains(add, "pods_down") || !strings.Contains(add, said) {
			t.Errorf("--match %q, tried: %d, the form kept doesn't say the match in effect as %s", match, code, want)
		}
	}
}
