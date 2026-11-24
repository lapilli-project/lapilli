package serve

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// /api/v1/report?redact=true: the report with every name an alias, its
// numbers as they were; the rules', the team's, the services' and the
// incidents' names nowhere in it.
func TestReportRedacted(t *testing.T) {
	parallel(t)
	s := nocServer(t)
	h := s.Handler()
	span := "from=" + nocBase.Add(-15*24*time.Hour).UTC().Format(time.RFC3339) + "&to=" + now.Add(time.Hour).UTC().Format(time.RFC3339)
	_, plain, _ := get(t, h, "/api/v1/report?"+span)
	code, body, _ := get(t, h, "/api/v1/report?redact=true&"+span)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	for _, name := range []string{`"Flap"`, `"Noise"`, `"DiskFull"`, `"search"`, `"cart"`, `"INC-1"`, `"cloud-test"`, `"shop"`} {
		if !strings.Contains(plain, name) {
			t.Fatalf("the report unredacted lacks %s: a test of nothing", name)
		}
		if strings.Contains(body, name) {
			t.Errorf("redacted: %s is there", name)
		}
	}
	var a, b verdict.Report
	if err := json.Unmarshal([]byte(plain), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(body), &b); err != nil {
		t.Fatal(err)
	}
	// Each rule's numbers, sorted: rules that tie are in order of their
	// alias once redacted.
	counts := func(r verdict.Report) []string {
		var out []string
		for _, rr := range r.Rules {
			out = append(out, fmt.Sprint(rr.Firings, rr.White, rr.Black, rr.Refired, rr.DuringDeploys, rr.Excused))
		}
		slices.Sort(out)
		return out
	}
	if !slices.Equal(counts(a), counts(b)) || len(a.Firings) != len(b.Firings) || len(a.Incidents) != len(b.Incidents) {
		t.Errorf("numbers: %v, redacted %v", counts(a), counts(b))
	}
	// Without it, as it was; firings=false too.
	if _, again, _ := get(t, h, "/api/v1/report?redact=false&"+span); again != plain {
		t.Error("redact=false changed the report")
	}
	if code, body, _ := get(t, h, "/api/v1/report?firings=false&redact=true"); code != 200 || strings.Contains(body, `"Flap"`) {
		t.Errorf("firings=false: %d, Flap there: %v", code, strings.Contains(body, `"Flap"`))
	}
}

// /api/v1/report?format=csv&redact=true: the firings a quiet window
// excused, which the CSV has and the report doesn't, are given the same
// aliases as the rest, and so are the teams: no rule, label value or team
// is named, a rule is one alias in every row, and its team is said, by
// its alias.
func TestReportCSVRedacted(t *testing.T) {
	parallel(t)
	s := nocServer(t)
	ctx := context.Background()
	at := nocBase.Add(-20 * time.Hour)
	if _, err := s.l.PutWindow(ctx, ledger.Window{Title: "load test", Start: at.Add(-time.Minute), End: at.Add(10 * time.Minute), Rules: "^Noise$"}, "ana", s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if err := s.l.JudgePending(ctx, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	span := "from=" + nocBase.Add(-15*24*time.Hour).UTC().Format(time.RFC3339) + "&to=" + now.Add(time.Hour).UTC().Format(time.RFC3339)
	_, plainBody, _ := get(t, h, "/api/v1/report?format=csv&"+span)
	code, body, _ := get(t, h, "/api/v1/report?format=csv&redact=true&"+span)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	plain, err := csv.NewReader(strings.NewReader(plainBody)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	red, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Noise", "Flap", "DiskFull", "search", "cart", "shop", "cloud-test", "INC-1"} {
		if !strings.Contains(plainBody, name) && name != "shop" && name != "INC-1" && name != "cloud-test" {
			t.Fatalf("the CSV unredacted lacks %s: a test of nothing", name)
		}
		if strings.Contains(body, name) {
			t.Errorf("redacted: %s is there", name)
		}
	}
	if len(red) != len(plain) {
		t.Fatalf("%d rows redacted, %d not", len(red), len(plain))
	}
	// The excused firing's rule is the alias of Noise's other rows; the
	// search team's rows carry the team's alias, as many as are named
	// search unredacted.
	count := func(rows [][]string, col int) map[string]int {
		n := map[string]int{}
		for _, row := range rows[1:] {
			n[row[col]]++
		}
		return n
	}
	var excusedRule string
	for _, row := range red[1:] {
		if row[11] == "true" {
			excusedRule = row[0]
		}
	}
	if !strings.HasPrefix(excusedRule, "rule-") || count(red, 0)[excusedRule] != count(plain, 0)["Noise"] {
		t.Errorf("the excused firing's rule %q: %d rows, Noise %d", excusedRule, count(red, 0)[excusedRule], count(plain, 0)["Noise"])
	}
	teams := count(red, 2)
	named := 0
	for k, n := range teams {
		if strings.HasPrefix(k, "team-") {
			named += n
		}
	}
	if named == 0 || named != count(plain, 2)["search"] {
		t.Errorf("teams redacted %v, unredacted %v", teams, count(plain, 2))
	}
	if !slices.Equal(stones(red), stones(plain)) {
		t.Errorf("stones %v, unredacted %v", stones(red), stones(plain))
	}
}

// stones are each row's start, stone, no record, re-fired, during a
// deploy and excused, sorted: rows that start together are in order of
// their rule, by alias once redacted, so a row's place may differ.
func stones(rows [][]string) []string {
	var out []string
	for _, row := range rows[1:] {
		out = append(out, strings.Join([]string{row[4], row[6], row[7], row[9], row[10], row[11]}, ","))
	}
	slices.Sort(out)
	return out
}
