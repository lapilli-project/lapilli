package verdict

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// Of an impact signal with no name, the text says from when it keeps the
// record when that is within the period, and that it keeps none when it
// has no data; nothing when it keeps all of the period, nor in lapilli
// serve's digest (by week), whose record starts where its ledger says.
func TestImpactKeptText(t *testing.T) {
	from, to := t0, t0.Add(7*24*time.Hour)
	for _, c := range []struct {
		name string
		s    Settings
		want string
	}{
		{"within", Settings{ImpactFrom: t0.Add(3 * 24 * time.Hour)}, "--impact keeps the record from 2026-10-29 00:00, where the data of every selector it reads begins"},
		{"before the period", Settings{ImpactFrom: t0.Add(-3 * 24 * time.Hour)}, ""},
		{"no data", Settings{ImpactNoData: true}, "--impact keeps no record: a selector it reads has no data"},
		{"within, by week", Settings{ImpactFrom: t0.Add(3 * 24 * time.Hour), ByWeek: true}, ""},
		{"no data, by week", Settings{ImpactNoData: true, ByWeek: true}, ""},
	} {
		// Nothing fired, so no record is said of the period: the line says it.
		c.s.RecordLookback = RecordLookback
		var b bytes.Buffer
		r := &Report{From: from, To: to, Settings: &c.s}
		impactKept(&b, r, time.UTC)
		if got := strings.TrimSpace(b.String()); got != c.want {
			t.Errorf("%s: %q, not %q", c.name, got, c.want)
		}
		// Through the whole text, too.
		b.Reset()
		if err := Text(&b, r, time.UTC); err != nil {
			t.Fatal(err)
		}
		if has := strings.Contains(b.String(), "--impact keeps"); has != (c.want != "") {
			t.Errorf("%s, the text: %v\n%s", c.name, has, b.String())
		}
	}
}

// With no record, the line is the period's to say once anything fired, in
// what it says of no record; with nothing fired, the line says it.
func TestImpactKeptNoRecord(t *testing.T) {
	s := &Settings{ImpactNoData: true, RecordLookback: RecordLookback}
	quiet := &Report{From: t0, To: t0.Add(7 * 24 * time.Hour), NoRecord: true, Settings: s}
	fired := &Report{From: t0, To: t0.Add(7 * 24 * time.Hour), NoRecord: true, Settings: s, Rules: []RuleResult{{Rule: "R", Firings: 2, Black: 2, Unjudged: 2, NoRecord: true}}}
	for _, c := range []struct {
		name      string
		r         *Report
		line, why bool
	}{{"nothing fired", quiet, true, false}, {"fired", fired, false, true}} {
		var b bytes.Buffer
		if err := Text(&b, c.r, time.UTC); err != nil {
			t.Fatal(err)
		}
		out := b.String()
		if strings.Contains(out, "--impact keeps no record: a selector it reads has no data\n") != c.line || strings.Contains(out, NoRecordNoDataText) != c.why {
			t.Errorf("%s:\n%s", c.name, out)
		}
	}
}

// A comparison of a period --impact keeps no record of says why.
func TestNoRecordComparedNoData(t *testing.T) {
	c := &Comparison{Before: &Report{NoRecord: true, Settings: &Settings{ImpactNoData: true}}}
	if got := c.noRecordText(); !strings.HasPrefix(got, NoRecordCompared) || !strings.HasSuffix(got, "--impact keeps no record: a selector it reads has no data; give one whose metrics have data over the period.") {
		t.Errorf("%q", got)
	}
	c.Before.Settings.ImpactNoData = false
	if got := c.noRecordText(); got != NoRecordCompared {
		t.Errorf("%q", got)
	}
}
