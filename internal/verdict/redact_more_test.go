package verdict

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
)

// No list's order says which names sort first: at a tie, each is by alias.
// Two keys are picked so the two rules' aliases sort one way, then the
// other; every list must follow the aliases, never the names.
func TestRedactOrder(t *testing.T) {
	t0 := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	rr := func(n string) RuleResult {
		return RuleResult{Rule: n, Firings: 12, Black: 12, Unjudged: 12, Replayed: true}
	}
	at := func(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }
	r := &Report{From: t0, To: at(168),
		Rules: []RuleResult{rr("AlphaPayments"), rr("ZetaCheckout")}, // Judge's order: a tie, by name
		// A file's order, not by when: and two at one time.
		Incidents: []IncidentResult{{Incident: incident.Incident{ID: "INC-300", Start: at(48)}}, {Incident: incident.Incident{ID: "INC-100", Start: at(24)}},
			{Incident: incident.Incident{ID: "INC-200", Start: at(24)}}},
		Firings: []Judged{{Firing: firingAt("AlphaPayments", at(1))}, {Firing: firingAt("ZetaCheckout", at(1))}},
		Unrecorded: []Unrecorded{{Rule: "AlphaPayments", Firings: 12, Top: []AlertCount{{Labels: map[string]string{"pod": "a-1"}, Firings: 6},
			{Labels: map[string]string{"pod": "z-9"}, Firings: 6}}}, {Rule: "ZetaCheckout", Firings: 12}},
		NearMisses:  []NearMiss{{Incident: "INC-100", Rule: "AlphaPayments"}, {Incident: "INC-100", Rule: "ZetaCheckout"}},
		Suggestions: []Suggestion{{Rule: "AlphaPayments", Before: rr("AlphaPayments")}, {Rule: "ZetaCheckout", Before: rr("ZetaCheckout")}},
		Hours:       &HoursUsed{Zone: "Europe/Paris", Teams: []TeamHours{{Team: "alpha-team", Zone: "Europe/Paris"}, {Team: "zeta-team", Zone: "Asia/Kolkata"}}},
	}
	seen := map[bool]bool{}
	for k := 0; len(seen) < 2 && k < 100; k++ {
		x := NewRedactor([]byte(fmt.Sprintf("a key to try, number %03d", k)))
		red := x.Report(r)
		alpha, zeta := x.aliases["rule"]["AlphaPayments"], x.aliases["rule"]["ZetaCheckout"]
		first := min(alpha, zeta)
		alphaFirst := alpha < zeta
		if seen[alphaFirst] {
			continue
		}
		seen[alphaFirst] = true
		for what, got := range map[string]string{"rules": red.Rules[0].Rule, "firings": red.Firings[0].Rule, "unrecorded": red.Unrecorded[0].Rule,
			"near misses": red.NearMisses[0].Rule, "suggestions": red.Suggestions[0].Rule} {
			if got != first {
				t.Errorf("%s: %s first, not %s, the alias that sorts first", what, got, first)
			}
		}
		// By when they started; at one time, by alias.
		ids := []string{red.Incidents[0].ID, red.Incidents[1].ID, red.Incidents[2].ID}
		in100, in200, in300 := x.aliases["incident"]["INC-100"], x.aliases["incident"]["INC-200"], x.aliases["incident"]["INC-300"]
		if ids[2] != in300 || ids[0] != min(in100, in200) {
			t.Errorf("incidents %v: want by start, then alias", ids)
		}
		for _, u := range red.Unrecorded {
			if top := u.Top; u.Rule == alpha && (len(top) != 2 || labelsText(top[0].Labels) > labelsText(top[1].Labels)) {
				t.Errorf("unrecorded's alerts: %v", top)
			}
		}
		if teams := red.Hours.Teams; teams[0].Team > teams[1].Team || teams[0].Zone != Redacted || red.Hours.Zone != Redacted {
			t.Errorf("hours: %+v", red.Hours)
		}
	}
	if len(seen) < 2 {
		t.Fatal("no two keys found that sort the aliases both ways")
	}
}

func firingAt(rule string, at time.Time) firing.Firing { return firing.Firing{Rule: rule, Start: at} }

// A redacted report and comparison hold none of what they were given,
// field by field: each string field is given a text of its own, and none
// may come out as it went in but those allowed to stay. A field added to
// a report later fails here until it is redacted, or allowed.
func TestRedactEveryField(t *testing.T) {
	stay := map[string]bool{
		"RuleResult.Severity":      true, // how loud, naming nothing
		"Settings.Match":           true, // labels' names, which stay
		"Signal.Match":             true,
		"RuleEntry.Match":          true,
		"Incident.Match":           true,
		"RuleResult.Unseen":        true, // Lapilli's codes
		"HoursUsed.Work":           true, // hours, as given
		"HoursUsed.Sleep":          true,
		"TeamHours.Work":           true,
		"TeamHours.Sleep":          true,
		"Unrecorded.Bound":         true, // "> 0.5": a number
		"ByValue.Op":               true,
		"Unreplayed.Side":          true, // before or after
		"RecordingChange.Change":   true, // changed, added, removed
		"Comparison.PassedOver":    true, // Lapilli's own words and counts
		"Comparison.JudgedBy":      true, // whose record judged: Lapilli's own words
		"ExpectResult.Limit":       true, // numbers and durations Lapilli writes
		"ExpectResult.Before":      true,
		"ExpectResult.After":       true,
		"ExpectResult.BeforeState": true,
		"ExpectResult.AfterState":  true,
		"ExpectResult.WasLimit":    true,
		"Judged.Day":               true, // a date, which parts episodes
		"Judged.Action":            true, // yes or no: Lapilli's words
	}
	for _, v := range []any{&Report{}, &Comparison{}} {
		c := &canaries{texts: map[string]string{}}
		c.fill(reflect.ValueOf(v).Elem(), reflect.TypeOf(v).Elem().Name(), 0)
		var out bytes.Buffer
		switch v := v.(type) {
		case *Report:
			red := NewRedactor(nil).Report(v)
			Text(&out, red, time.UTC)
			JSON(&out, red)
			Prometheus(&out, red, time.Now())
		case *Comparison:
			red := NewRedactor(nil).Comparison(v)
			CompareText(&out, red, time.UTC)
			ExpectationsText(&out, red.Expectations)
			CompareMarkdown(&out, red, time.UTC)
			JSON(&out, red)
		}
		for text, field := range c.texts {
			if strings.Contains(out.String(), text) && !stay[field] {
				t.Errorf("%s: %s is there as it went in", reflect.TypeOf(v).Elem().Name(), field)
			}
		}
		if len(c.texts) < 40 {
			t.Errorf("%d fields filled: does fill still reach them?", len(c.texts))
		}
	}
}

// canaries fill every string field with a text of its own, and note whose
// field it is: Type.Field.
type canaries struct {
	n     int
	texts map[string]string
}

func (c *canaries) fill(v reflect.Value, field string, depth int) {
	if depth > 8 {
		return
	}
	switch v.Kind() {
	case reflect.String:
		c.n++
		s := fmt.Sprintf("canary%04dx", c.n)
		c.texts[s] = field
		v.SetString(s)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int64:
		v.SetInt(3)
	case reflect.Float64:
		v.SetFloat(0.5)
	case reflect.Pointer:
		if e := v.Type().Elem(); e != reflect.TypeOf(time.Location{}) {
			p := reflect.New(e)
			c.fill(p.Elem(), field, depth+1)
			v.Set(p)
		}
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			c.n++
			v.Set(reflect.ValueOf(time.Date(2026, 11, 2, 9, c.n, 0, 0, time.UTC)))
			return
		}
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if f.IsExported() {
				name := v.Type().Name() + "." + f.Name
				if f.Anonymous {
					name = field
				}
				c.fill(v.Field(i), name, depth+1)
			}
		}
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 2, 2)
		for i := range 2 {
			c.fill(s.Index(i), field, depth+1)
		}
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		key := reflect.New(v.Type().Key()).Elem()
		if key.Kind() == reflect.String {
			c.n++
			key.SetString(fmt.Sprintf("labelname%04d", c.n)) // a label's name stays
		}
		val := reflect.New(v.Type().Elem()).Elem()
		c.fill(val, field, depth+1)
		m.SetMapIndex(key, val)
		v.Set(m)
	}
}
