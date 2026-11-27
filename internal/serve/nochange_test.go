package serve

import (
	"strings"
	"testing"
	"time"
)

// TestNoChangeIsNeitherBetterNorWorse: a change is one between the numbers
// shown. Two that show the same are neither better nor worse, with no
// colour and no word, on the overview's tiles, the Outcomes page's then and
// now, and a rule's page; two that differ never read "0 pt", "0" or "0s".
func TestNoChangeIsNeitherBetterNorWorse(t *testing.T) {
	parallel(t)
	week := func(f, w int, delays ...time.Duration) *weekView {
		return &weekView{Firings: f, White: w, Black: f - w, Delays: delays, Kept: true}
	}
	tileOf := func(ts []tile, label string) tile {
		for _, x := range ts {
			if x.key == label {
				return x
			}
		}
		t.Fatalf("no %s tile", label)
		return tile{}
	}
	overview := func(last, before *weekView) []tile {
		return (&Server{}).tiles("en", []*weekView{before, last}, last, before, "", "", "", false, func() (int, int) { return 0, 0 })
	}
	for _, c := range []struct {
		name         string
		last, before *weekView
		label        string
		delta        string
		better, wrse bool
	}{
		// 1.2% and 0.8% both read 1%.
		{"precision the same as shown", week(250, 3), week(125, 1), "Precision", "as the week before", false, false},
		// 1.6% reads 2%, 1.2% 1%: a point, better.
		{"precision a point apart as shown", week(250, 4), week(250, 3), "Precision", "Up 1 pt from 1% the week before", true, false},
		// 1001 beside 1000 is +0.1%: the change itself, not "+0%".
		{"firings a hair apart", week(1001, 3), week(1000, 3), "Firings", "Up 1 from 1000 the week before", false, false},
		// 10.2s and 10.7s both read 10s.
		{"time to catch the same as shown", week(9, 3, 10200*time.Millisecond), week(9, 3, 10700*time.Millisecond), "Time to catch", "as the week before", false, false},
		// 11.2s reads 11s, 10.9s 10s: a second, worse; not "+0s".
		{"time to catch a second apart as shown", week(9, 3, 11200*time.Millisecond), week(9, 3, 10900*time.Millisecond), "Time to catch", "Up 1s from 10s the week before", false, true},
		// 1h5m1s reads 1h5m, 1h4m20s 1h4m: a minute, in the minutes
		// shown, not "+41s".
		{"time to catch past an hour", week(9, 3, 3901*time.Second), week(9, 3, 3860*time.Second), "Time to catch", "Up 1m from 1h4m the week before", false, true},
		// 1h0m10s reads 1h0m, 59m50s as it is: ten seconds between the
		// two as shown, not a minute.
		{"time to catch across the hour", week(9, 3, 3610*time.Second), week(9, 3, 3590*time.Second), "Time to catch", "Up 10s from 59m50s the week before", false, true},
		{"time to catch back across the hour", week(9, 3, 3590*time.Second), week(9, 3, 3610*time.Second), "Time to catch", "Down 10s from 1h0m the week before", true, false},
		// Before the incident began: -1h5m1s reads -1h5m, -1h4m20s -1h4m.
		{"time to catch below nothing", week(9, 3, -3901*time.Second), week(9, 3, -3860*time.Second), "Time to catch", "Down 1m from -1h4m the week before", true, false},
		// 1h0m10s and 1h0m50s both read 1h0m.
		{"time to catch the same past an hour", week(9, 3, 3610*time.Second), week(9, 3, 3650*time.Second), "Time to catch", "as the week before", false, false},
	} {
		x := tileOf(overview(c.last, c.before), c.label)
		if x.Delta != c.delta || x.Better != c.better || x.Worse != c.wrse {
			t.Errorf("overview, %s: %q better %v worse %v, want %q %v %v", c.name, x.Delta, x.Better, x.Worse, c.delta, c.better, c.wrse)
		}
	}

	// Outcomes' then and now.
	from := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	span := func(weeks, f, w, inc, missed int) period {
		return period{From: from, To: from.Add(time.Duration(weeks) * weekLen), Firings: f, White: w, Incidents: inc, Missed: missed}
	}
	impact := func(then, now period) map[string]tile {
		out := map[string]tile{}
		for _, x := range thenNow("en", then, now) {
			out[x.Label] = x
		}
		return out
	}
	for _, c := range []struct {
		name         string
		then, now    period
		label        string
		delta        string
		better, wrse bool
	}{
		{"precision the same as shown", span(1, 125, 1, 0, 0), span(1, 250, 3, 0, 0), "Precision", "", false, false},
		{"precision a point apart as shown", span(1, 250, 3, 0, 0), span(1, 250, 4, 0, 0), "Precision", "Up 1 pt", true, false},
		// 1.5 and 2.5 a week both read 2, as "%.0f" rounds half to even.
		{"a week, the same as shown", span(2, 3, 0, 0, 0), span(2, 5, 0, 0, 0), "Firings a week", "", false, false},
		// 1001 ● beside 1000: one more, worse; not "+0%".
		{"● a week a hair apart", span(1, 1000, 0, 0, 0), span(1, 1001, 0, 0, 0), "● a week", "Up 1", false, true},
		// None a week then, four now: four more, not hidden for none then.
		{"a week, from none", span(1, 0, 0, 0, 0), span(1, 4, 0, 0, 0), "Firings a week", "Up 4", false, false},
		{"● a week, from none", span(1, 0, 0, 0, 0), span(1, 4, 0, 0, 0), "● a week", "Up 4", false, true},
		// 1.4 a week reads 1, 2.6 reads 3: two more, not the
		// unrounded rates' 1.2 more.
		{"a week, of the numbers shown", span(5, 7, 0, 0, 0), span(5, 13, 0, 0, 0), "Firings a week", "Up 2", false, false},
		// Then not measured, over no span: nothing to set beside.
		{"a week, then not measured", period{From: from, To: from}, span(1, 4, 0, 0, 0), "Firings a week", "", false, false},
		{"incidents missed, the same share", span(1, 9, 3, 3, 1), span(1, 9, 3, 3, 1), "Incidents missed", "", false, false},
		{"incidents missed, fewer", span(1, 9, 3, 4, 1), span(1, 9, 3, 5, 1), "Incidents missed", "Down 5 pt from 25%", true, false},
	} {
		x := impact(c.then, c.now)[c.label]
		if x.Delta != c.delta || x.Better != c.better || x.Worse != c.wrse {
			t.Errorf("Outcomes, %s: %q better %v worse %v, want %q %v %v", c.name, x.Delta, x.Better, x.Worse, c.delta, c.better, c.wrse)
		}
	}

	// Said whole in each language, so that the amount and what it was
	// can't be swapped.
	wkly := func(f, w int) *weekView { return &weekView{Firings: f, White: w, Black: f - w, Kept: true} }
	for lang, want := range map[string][3]string{
		"en": {"Up 2 from 98 the week before", "Up 1 pt from 1% the week before", "Down 5 pt from 25%"},
		"ko": {"전주 98에서 2 증가", "전주 1%에서 1%p 증가", "25%에서 5%p 감소"},
		"ja": {"先週の98から2増加", "先週の1%から1ポイント増加", "25%から5ポイント減少"},
		"es": {"Sube 2 respecto a 98 la semana anterior", "Sube 1 p. p. respecto a 1% la semana anterior", "Baja 5 p. p. respecto a 25%"},
	} {
		a, b := wkly(250, 4), wkly(250, 3)
		got := map[string]string{}
		for _, x := range (&Server{}).tiles(lang, []*weekView{wkly(98, 3), wkly(100, 3)}, wkly(100, 3), wkly(98, 3), "", "", "", false, func() (int, int) { return 0, 0 }) {
			got[x.key] = x.Delta
		}
		for _, x := range (&Server{}).tiles(lang, []*weekView{b, a}, a, b, "", "", "", false, func() (int, int) { return 0, 0 }) {
			if x.key == "Precision" {
				got["pt"] = x.Delta
			}
		}
		for _, x := range thenNow(lang, span(1, 9, 3, 4, 1), span(1, 9, 3, 5, 1)) {
			if x.Label == tr(lang, "Incidents missed") {
				got["missed"] = x.Delta
			}
		}
		for i, k := range []string{"Firings", "pt", "missed"} {
			if got[k] != want[i] {
				t.Errorf("%s %s: %q, want %q", lang, k, got[k], want[i])
			}
		}
	}

	// As drawn: no colour and no word for what didn't change, in each
	// language; a rule's page the same.
	same := impact(span(1, 125, 1, 0, 0), span(1, 250, 3, 0, 0))["Precision"]
	for _, n := range names {
		var b strings.Builder
		if err := pagesIn[n.Code].ExecuteTemplate(&b, "tiledelta", same); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(b.String(), "status-") || strings.Contains(b.String(), tr(n.Code, "%s pt", "")) {
			t.Errorf("%s: an unchanged tile draws %q", n.Code, b.String())
		}
		b.Reset()
		if err := pagesIn[n.Code].ExecuteTemplate(&b, "delta", moved{On: true, By: 0, Up: true, Pt: true}); err != nil {
			t.Fatal(err)
		}
		if got := b.String(); strings.Contains(got, "status-") || strings.Contains(got, tr(n.Code, "better")) || strings.Contains(got, tr(n.Code, "worse")) {
			t.Errorf("%s: a rule's unchanged number draws %q", n.Code, got)
		}
	}
}

// TestChangeReadsOneWay: a headline number's change is a direction word and
// an amount in the number's own unit, in each language: never a bare signed
// number, in points for a share and plain for a count.
func TestChangeReadsOneWay(t *testing.T) {
	parallel(t)
	up := map[string]string{"en": "Up ", "ko": "증가", "ja": "増加", "es": "Sube "}
	down := map[string]string{"en": "Down ", "ko": "감소", "ja": "減少", "es": "Baja "}
	point := map[string]string{"en": "pt", "ko": "%p", "ja": "ポイント", "es": "p. p."}
	week := func(f, w int, delays ...time.Duration) *weekView {
		return &weekView{Firings: f, White: w, Black: f - w, Delays: delays, Kept: true}
	}
	from := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	span := func(f, w, inc, missed int) period {
		return period{From: from, To: from.Add(weekLen), Firings: f, White: w, Incidents: inc, Missed: missed}
	}
	check := func(lang, what, delta, label string, share bool) {
		t.Helper()
		if delta == "" {
			t.Errorf("%s, %s: no delta", lang, what)
			return
		}
		if strings.HasPrefix(delta, "+") || strings.HasPrefix(delta, "-") || strings.HasPrefix(delta, "−") || strings.Contains(delta, " +") || strings.Contains(delta, " -") {
			t.Errorf("%s, %s: %q is signed", lang, what, delta)
		}
		if !strings.Contains(delta, up[lang]) && !strings.Contains(delta, down[lang]) {
			t.Errorf("%s, %s: %q has no direction", lang, what, delta)
		}
		if has := strings.Contains(delta, point[lang]); has != share {
			t.Errorf("%s, %s: %q in points %v, want %v", lang, what, delta, has, share)
		}
	}
	for lang := range up {
		for _, c := range []struct {
			label string
			share bool
			a, b  *weekView
		}{
			{"Precision", true, week(250, 4), week(250, 3)},
			{"Firings", false, week(100, 3), week(98, 3)},
			{"Incidents missed", false, &weekView{Missed: 4}, &weekView{Missed: 2}},
			{"Time to catch", false, week(9, 3, 4*time.Minute), week(9, 3, 8*time.Minute)},
		} {
			for _, x := range (&Server{}).tiles(lang, []*weekView{c.b, c.a}, c.a, c.b, "", "", "", false, func() (int, int) { return 0, 0 }) {
				if x.key == c.label {
					check(lang, "overview "+c.label, x.Delta, c.label, c.share)
				}
			}
		}
		for _, c := range []struct {
			label     string
			share     bool
			then, now period
		}{
			{"Precision", true, span(250, 3, 0, 0), span(250, 4, 0, 0)},
			{"Firings a week", false, span(3, 0, 0, 0), span(9, 0, 0, 0)},
			{"Incidents missed", true, span(9, 3, 4, 1), span(9, 3, 5, 1)},
		} {
			for _, x := range thenNow(lang, c.then, c.now) {
				if x.Label == tr(lang, c.label) {
					check(lang, "Outcomes "+c.label, x.Delta, c.label, c.share)
				}
			}
		}
	}
}
