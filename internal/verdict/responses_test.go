package verdict

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/rules"
)

// said is a Responder that answers by the firing's pod label: its kind and
// whether it was useful, and notes what it was asked.
type said struct {
	kinds  map[string]string
	useful map[string]bool // absent: unknown
	asked  []time.Duration
	judged map[string]bool
}

func (s *said) Respond(f firing.Firing, within time.Duration, now time.Time, white, judged bool, answer string) FiringResponse {
	s.asked = append(s.asked, within)
	if s.judged == nil {
		s.judged = map[string]bool{}
	}
	s.judged[f.Labels["pod"]] = judged
	fr := FiringResponse{Kind: s.kinds[f.Labels["pod"]]}
	if fr.Kind == "" {
		fr.Kind = ResponseNone
	}
	if u, ok := s.useful[f.Labels["pod"]]; ok {
		fr.Useful = &u
	}
	return fr
}

func (s *said) Totals() ResponseTotals {
	return ResponseTotals{Pages: 5, PagesUnmatched: 2, Changes: 3}
}

// firingsOf makes n firings of a rule, an hour apart, the pods p0 on.
func firingsOf(rule string, n int, from time.Time) []firing.Firing {
	var fs []firing.Firing
	for i := range n {
		at := from.Add(time.Duration(i) * time.Hour)
		fs = append(fs, firing.Firing{Rule: rule, Labels: map[string]string{"pod": "p" + string(rune('0'+i))}, Start: at, End: at.Add(10 * time.Minute)})
	}
	return fs
}

var rt0 = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

// A rule's firings are counted by their responses, and its actionable
// share is the useful over the useful and not useful; the report has what
// was read.
func TestJudgeResponsesCounts(t *testing.T) {
	s := &said{
		kinds:  map[string]string{"p0": ResponseChanged, "p1": ResponseChanged, "p2": ResponseUnanswered, "p3": ResponseAcknowledged, "p4": ResponseRoutine},
		useful: map[string]bool{"p0": true, "p1": true, "p2": false, "p3": false},
	}
	o := Options{From: rt0, To: rt0.Add(24 * time.Hour), Lead: 15 * time.Minute, RefireWithin: time.Hour, UnknownLen: time.Hour,
		Responder: s, ResponseStep: time.Minute, NoisyBy: NoisyByCaught, Routine: 0.5}
	known := []rules.Rule{{Name: "Disk", Interval: 5 * time.Minute}}
	rep := Judge(known, firingsOf("Disk", 5, rt0.Add(time.Hour)), nil, o)
	r := rep.Rules[0]
	c := r.Responses
	if c == nil || c.Changed != 2 || c.Routine != 1 || c.Acknowledged != 1 || c.Unanswered != 1 || c.None != 0 || c.Useful != 2 || c.NotUseful != 2 {
		t.Fatalf("counts %+v", c)
	}
	if c.Actionable == nil || *c.Actionable != 0.5 {
		t.Errorf("actionable %v, want 0.5", c.Actionable)
	}
	if p, ok := r.Actionable(); !ok || p != 0.5 {
		t.Errorf("Actionable() %v %v", p, ok)
	}
	if rep.Responses == nil || *rep.Responses != (ResponseTotals{Pages: 5, PagesUnmatched: 2, Changes: 3}) {
		t.Errorf("totals %+v", rep.Responses)
	}
	if rep.Settings.NoisyBy != NoisyByCaught || rep.Settings.Routine != 0.5 {
		t.Errorf("settings %+v", rep.Settings)
	}
	for _, j := range rep.Firings {
		if j.Response == nil {
			t.Fatalf("a firing with no response: %+v", j)
		}
	}
	// A page may be two evaluations from its firing: its group's interval of
	// five minutes beats the step.
	if s.asked[0] != 10*time.Minute {
		t.Errorf("within %v, want 10m", s.asked[0])
	}
	// Without an interval, the step, or a minute.
	s.asked = nil
	Judge([]rules.Rule{{Name: "Disk"}}, firingsOf("Disk", 1, rt0.Add(time.Hour)), nil, o)
	if s.asked[0] != 2*time.Minute {
		t.Errorf("within %v, want 2m", s.asked[0])
	}
	// Judged says a stone was kept: with no incident, none was.
	if s.judged["p0"] {
		t.Error("a firing with no record said to be judged")
	}
	// The text's table, and the line of its pages.
	var b bytes.Buffer
	if err := Text(&b, rep, time.UTC); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ACTIONABLE", "50%", "2 (1)", "5 pages read, 2 matched no firing", "3 changes read"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("the text lacks %q:\n%s", want, b.String())
		}
	}
}

// A rule is noisy by what it caught, or by what was done about its firings,
// as --noisy-by says; by useful a rule with no record can be noisy, from its
// responses alone, and the report says how many firings were so judged.
func TestJudgeNoisyBy(t *testing.T) {
	// Twelve firings, none useful but two, and three of them caught.
	kinds, useful := map[string]string{}, map[string]bool{}
	fs := firingsOf("Flaky", 12, rt0.Add(time.Hour))
	for i := range fs {
		fs[i].Labels["pod"] = "p" + string(rune('a'+i))
		useful[fs[i].Labels["pod"]] = i < 2
		kinds[fs[i].Labels["pod"]] = ResponseUnanswered
	}
	s := &said{kinds: kinds, useful: useful}
	// Three incidents, each catching one firing, so a quarter of them caught.
	var incs []incident.Incident
	for _, i := range []int{3, 5, 7} {
		incs = append(incs, incident.Incident{ID: "INC-" + string(rune('0'+i)), Start: fs[i].Start.Add(time.Minute), End: fs[i].Start.Add(5 * time.Minute)})
	}
	o := Options{From: rt0, To: rt0.Add(24 * time.Hour), Lead: 15 * time.Minute, RefireWithin: time.Minute, UnknownLen: time.Hour}
	by := func(noisyBy string, withResponder bool, incs []incident.Incident) *Report {
		x := o
		if withResponder {
			x.Responder, x.ResponseStep = s, time.Minute
		}
		x.NoisyBy = noisyBy
		return Judge(nil, fs, incs, x)
	}
	if rep := by(NoisyByCaught, true, incs); rep.Rules[0].Noisy || rep.Rules[0].White != 3 {
		t.Errorf("by caught, 3 of 12 white: %+v", rep.Rules[0])
	}
	rep := by(NoisyByUseful, true, incs)
	r := rep.Rules[0]
	if !r.Noisy || r.Responses.Useful != 2 || r.Responses.NotUseful != 10 {
		t.Errorf("by useful, 2 of 12: noisy %v %+v", r.Noisy, r.Responses)
	}
	// Without responses, --noisy-by useful asks for none: as before.
	if rep := by(NoisyByUseful, false, incs); rep.Rules[0].Noisy || rep.Rules[0].Responses != nil || rep.Responses != nil {
		t.Errorf("with no responder: %+v", rep.Rules[0])
	}
	// With no incident written down at all, nothing is judged by stones,
	// and by useful the rule is noisy from its responses alone.
	rep = by(NoisyByUseful, true, nil)
	r = rep.Rules[0]
	if !rep.NoRecord || !r.NoRecord || !r.Noisy {
		t.Errorf("no record: rep %v rule %+v", rep.NoRecord, r)
	}
	if n := rep.ByResponsesAlone(); n != 12 {
		t.Errorf("%d firings judged by responses alone, want 12", n)
	}
	var b bytes.Buffer
	if err := Text(&b, rep, time.UTC); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "Of them, 12 firings were judged by what was done about them alone") {
		t.Errorf("the no-record line:\n%s", b.String())
	}
	// By caught, the same period has no noisy rule.
	if rep := by(NoisyByCaught, true, nil); rep.Rules[0].Noisy {
		t.Error("noisy by caught with no record")
	}
}

// The firings that were not useful or not are counted apart: a firing whose
// usefulness isn't known is in neither, so a rule with none known has no
// actionable share.
func TestActionableUnknown(t *testing.T) {
	s := &said{kinds: map[string]string{}, useful: map[string]bool{}}
	o := Options{From: rt0, To: rt0.Add(24 * time.Hour), Lead: 15 * time.Minute, Responder: s, ResponseStep: time.Minute}
	rep := Judge(nil, firingsOf("Quiet", 2, rt0.Add(time.Hour)), nil, o)
	r := rep.Rules[0]
	if _, ok := r.Actionable(); ok || r.Responses.Actionable != nil || r.Responses.None != 2 {
		t.Errorf("%+v", r.Responses)
	}
	var b bytes.Buffer
	JSON(&b, rep)
	if !strings.Contains(b.String(), `"actionable": null`) || !strings.Contains(b.String(), `"useful": null`) {
		t.Errorf("JSON:\n%s", b.String())
	}
}

// --compare shows ACTIONABLE beside precision, before and after, when either
// side has responses, and not otherwise.
func TestCompareActionable(t *testing.T) {
	useful := func(share int) *said {
		s := &said{kinds: map[string]string{}, useful: map[string]bool{}}
		for i := range 4 {
			pod := "p" + string(rune('0'+i))
			s.kinds[pod] = ResponseChanged
			s.useful[pod] = i < share
		}
		return s
	}
	o := Options{From: rt0, To: rt0.Add(24 * time.Hour), Lead: 15 * time.Minute, RefireWithin: time.Hour, UnknownLen: time.Hour, ResponseStep: time.Minute}
	known := []rules.Rule{{Name: "Disk"}}
	fs := firingsOf("Disk", 4, rt0.Add(time.Hour))
	ob, oa := o, o
	ob.Responder, oa.Responder = useful(1), useful(3)
	c := Compare(Judge(known, fs, nil, ob), Judge(known, fs[:3], nil, oa), []string{"Disk"}, 0)
	var b bytes.Buffer
	if err := CompareText(&b, c, time.UTC); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "ACTIONABLE") || !strings.Contains(b.String(), "25% → 100%") {
		t.Errorf("the comparison:\n%s", b.String())
	}
	c = Compare(Judge(known, fs, nil, o), Judge(known, fs[:3], nil, o), []string{"Disk"}, 0)
	b.Reset()
	CompareText(&b, c, time.UTC)
	if strings.Contains(b.String(), "ACTIONABLE") {
		t.Errorf("a comparison with no responses:\n%s", b.String())
	}
}
