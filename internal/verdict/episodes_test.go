package verdict

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
)

// episodeOpts judge with --refire 1h, matched on service, days in UTC.
var episodeOpts = Options{Lead: 15 * time.Minute, RefireWithin: time.Hour, UnknownLen: time.Hour, Match: []string{"service"}, Zone: time.UTC}

// ept is a time on 2026-11-02 (a Monday), hours and minutes in UTC.
func ept(h, m int) time.Time { return time.Date(2026, 11, 2, h, m, 0, 0, time.UTC) }

func fired(rule string, labels map[string]string, start, end time.Time) firing.Firing {
	return firing.Firing{Rule: rule, Labels: labels, Start: start, End: end}
}

// episodesOf are the report's firings' episodes, in the order Judge gives
// them, after its ins.
func episodesOf(fs []firing.Firing, ins []incident.Incident, o Options) ([]int, *Report) {
	o.From, o.To = ept(0, 0).Add(-24*time.Hour), ept(0, 0).Add(3*24*time.Hour)
	rep := Judge(nil, fs, ins, o)
	var out []int
	for _, j := range rep.Firings {
		out = append(out, j.Episode)
	}
	return out, rep
}

// Which firings make an episode: one rule, the same values of the match
// labels, overlapping or within --refire of the latest end so far, parted
// at midnight; the pods of one service are one, two services two, another
// rule another, and with no match every overlapping firing of a rule.
func TestEpisodesGrouping(t *testing.T) {
	svc := func(s, pod string) map[string]string { return map[string]string{"service": s, "pod": pod} }
	for _, c := range []struct {
		name string
		fs   []firing.Firing
		o    func(*Options)
		want []int
	}{
		{"overlapping pods of one service are one", []firing.Firing{
			fired("Restart", svc("search", "a"), ept(10, 0), ept(10, 30)),
			fired("Restart", svc("search", "b"), ept(10, 10), ept(10, 20)),
			fired("Restart", svc("search", "c"), ept(10, 25), ept(10, 40)),
		}, nil, []int{1, 1, 1}},
		{"two services are two", []firing.Firing{
			fired("Restart", svc("search", "a"), ept(10, 0), ept(10, 30)),
			fired("Restart", svc("cart", "a"), ept(10, 10), ept(10, 20)),
		}, nil, []int{1, 2}},
		{"another rule is another", []firing.Firing{
			fired("Restart", svc("search", "a"), ept(10, 0), ept(10, 30)),
			fired("Errors", svc("search", "a"), ept(10, 10), ept(10, 20)),
		}, nil, []int{1, 2}},
		{"within --refire of the latest end", []firing.Firing{
			fired("Flap", svc("search", "a"), ept(10, 0), ept(10, 5)),
			fired("Flap", svc("search", "a"), ept(11, 5), ept(11, 10)),  // an hour to the second
			fired("Flap", svc("search", "a"), ept(12, 11), ept(12, 20)), // an hour and a minute
		}, nil, []int{1, 1, 2}},
		{"the latest end, not the last firing's", []firing.Firing{
			fired("Flap", svc("search", "a"), ept(10, 0), ept(13, 0)),
			fired("Flap", svc("search", "b"), ept(10, 10), ept(10, 20)),
			fired("Flap", svc("search", "c"), ept(13, 30), ept(13, 40)), // 30m after the first's end
		}, nil, []int{1, 1, 1}},
		{"one still on holds it open", []firing.Firing{
			fired("Flap", svc("search", "a"), ept(10, 0), time.Time{}),
			fired("Flap", svc("search", "b"), ept(20, 0), ept(20, 5)),
		}, nil, []int{1, 1}},
		{"midnight parts it", []firing.Firing{
			fired("Flap", svc("search", "a"), ept(23, 50), ept(0, 0).Add(24*time.Hour+10*time.Minute)),
			fired("Flap", svc("search", "b"), ept(0, 5).Add(24*time.Hour), ept(0, 20).Add(24*time.Hour)),
		}, nil, []int{1, 2}},
		{"a label not carried is a value of its own", []firing.Firing{
			fired("Disk", map[string]string{"node": "n1"}, ept(10, 0), ept(10, 30)),
			fired("Disk", svc("search", "a"), ept(10, 10), ept(10, 20)),
			fired("Disk", map[string]string{"node": "n2"}, ept(10, 15), ept(10, 20)),
		}, nil, []int{1, 2, 1}},
		{"with no match, every overlapping firing of a rule", []firing.Firing{
			fired("Restart", svc("search", "a"), ept(10, 0), ept(10, 30)),
			fired("Restart", svc("cart", "a"), ept(10, 10), ept(10, 20)),
		}, func(o *Options) { o.Match = nil }, []int{1, 1}},
		{"numbered in order of their first firing", []firing.Firing{
			fired("B", svc("search", "a"), ept(10, 0), ept(10, 30)),
			fired("A", svc("search", "a"), ept(10, 5), ept(10, 30)),
			fired("B", svc("search", "a"), ept(10, 20), ept(10, 40)),
		}, nil, []int{1, 2, 1}},
	} {
		o := episodeOpts
		if c.o != nil {
			c.o(&o)
		}
		got, rep := episodesOf(c.fs, nil, o)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: episodes %v, want %v", c.name, got, c.want)
		}
		for _, j := range rep.Firings {
			if want := j.Start.UTC().Format(time.DateOnly); j.Day != want {
				t.Errorf("%s: a firing at %s on day %q, want %q", c.name, j.Start, j.Day, want)
			}
		}
	}
}

// Midnight is the zone's: the same two firings, one episode in UTC, two
// in Seoul, where 23:50 UTC is past 08:00 of the next day and 07:55 the
// day before it.
func TestEpisodesMidnightOfTheZone(t *testing.T) {
	seoul, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Skip(err)
	}
	fs := []firing.Firing{
		fired("Flap", map[string]string{"service": "search"}, ept(14, 50), ept(15, 10)), // 23:50 and 00:10 in Seoul
		fired("Flap", map[string]string{"service": "search"}, ept(15, 5), ept(15, 20)),
	}
	if got, _ := episodesOf(fs, nil, episodeOpts); !slices.Equal(got, []int{1, 1}) {
		t.Errorf("in UTC: %v, want one episode", got)
	}
	o := episodeOpts
	o.Zone = seoul
	got, rep := episodesOf(fs, nil, o)
	if !slices.Equal(got, []int{1, 2}) {
		t.Errorf("in Seoul: %v, want two", got)
	}
	if rep.Settings.Zone != "Asia/Seoul" || rep.Firings[0].Day != "2026-11-02" || rep.Firings[1].Day != "2026-11-03" {
		t.Errorf("zone %q, days %q %q", rep.Settings.Zone, rep.Firings[0].Day, rep.Firings[1].Day)
	}
}

// An episode is white when any of its firings is; else judged by its first
// firing's record: black when it had one, not judged when it hadn't.
func TestEpisodesJudged(t *testing.T) {
	search := map[string]string{"service": "search"}
	cart := map[string]string{"service": "cart"}
	// The record from 2026-11-02 08:00 on: an incident then, of nothing's.
	keep := incident.Incident{ID: "KEEP", Start: ept(8, 0), End: ept(8, 1), Labels: incident.Labels{"service": {"nothing"}}}
	in := incident.Incident{ID: "INC-1", Start: ept(10, 20), End: ept(10, 40), Labels: incident.Labels{"service": {"search"}}}
	fs := []firing.Firing{
		// White by its second firing.
		fired("Errors", search, ept(10, 0), ept(10, 10)),
		fired("Errors", search, ept(10, 30), ept(10, 35)),
		// Black, judged: the record is kept from 08:00.
		fired("Errors", cart, ept(10, 0), ept(10, 10)),
	}
	_, rep := episodesOf(fs, []incident.Incident{keep, in}, episodeOpts)
	e := rep.Rules[0].Episodes
	if e != (EpisodeCount{Total: 2, White: 1}) {
		t.Errorf("with a record: %+v, want 2, 1 white, none unjudged", e)
	}
	// With no record but the incident, at its start: the first firings of
	// both episodes have none, the first one's second firing caught it.
	_, rep = episodesOf(fs, []incident.Incident{in}, episodeOpts)
	if e := rep.Rules[0].Episodes; e.Total != 2 || e.White != 1 {
		t.Errorf("an episode with a white firing is white: %+v", e)
	}
	for _, j := range rep.Firings {
		if j.Labels["service"] == "cart" && !j.NoRecord {
			t.Fatalf("the cart firing has a record: the test shows less")
		}
	}
	if e := rep.Rules[0].Episodes; e.Unjudged != 1 {
		t.Errorf("an episode whose first firing had no record isn't judged: %+v", e)
	}
}

// Episodes change none of today's numbers: a rule restarting all of its
// pods each night, and caught twice in a real incident (the design's
// second case), is noisy by its firings as before, every count of them as
// before, and not noisy by its episodes, which decide nothing.
func TestEpisodesChangeNoNumber(t *testing.T) {
	var fs []firing.Firing
	var ins []incident.Incident
	for night := range 5 {
		base := ept(2, 0).Add(time.Duration(night) * 24 * time.Hour)
		for pod := range 12 {
			fs = append(fs, fired("PodRestarting", map[string]string{"service": "search", "pod": fmt.Sprint(pod)}, base.Add(time.Duration(pod)*time.Minute), base.Add(20*time.Minute)))
		}
	}
	// A real incident restarts one pod twice, in the afternoon; one of
	// nothing's, before the week, keeps the record of every firing.
	ins = append(ins, incident.Incident{ID: "INC-1", Start: ept(15, 0), End: ept(16, 0), Labels: incident.Labels{"service": {"search"}}},
		incident.Incident{ID: "KEEP", Start: ept(0, 0).Add(-48 * time.Hour), End: ept(0, 0).Add(-47 * time.Hour), Labels: incident.Labels{"service": {"nothing"}}})
	fs = append(fs, fired("PodRestarting", map[string]string{"service": "search", "pod": "3"}, ept(15, 5), ept(15, 10)),
		fired("PodRestarting", map[string]string{"service": "search", "pod": "3"}, ept(15, 30), ept(15, 35)))
	o := episodeOpts
	o.From, o.To = ept(0, 0), ept(0, 0).Add(7*24*time.Hour)
	rep := Judge(nil, fs, ins, o)
	r := rep.Rules[0]
	want := RuleResult{Rule: "PodRestarting", Firings: 62, White: 2, Black: 60, Refired: 1, Noisy: true, Incidents: 1, First: 1, Alone: 1}
	got := r
	got.Episodes = EpisodeCount{}
	if got.Rule != want.Rule || got.Firings != want.Firings || got.White != want.White || got.Black != want.Black || got.Refired != want.Refired ||
		got.Noisy != want.Noisy || got.Unjudged != 0 || got.Incidents != 1 {
		t.Errorf("the firings' numbers: %+v", got)
	}
	if p, ok := r.Precision(); !ok || p >= 0.2 {
		t.Errorf("precision by firings %v %v", p, ok)
	}
	if r.Episodes != (EpisodeCount{Total: 6, White: 1}) {
		t.Errorf("episodes %+v, want 6 (five nights and the afternoon), 1 white, not noisy", r.Episodes)
	}
	if !rep.Incidents[0].CaughtBy[0].Noisy {
		t.Error("the catch of a rule noisy by firings is a noisy one's, whatever its episodes")
	}
	// And Judge with episodes counted as nothing of the rule: the same
	// report but its episodes.
	again := Judge(nil, slices.Clone(fs), ins, o)
	for i := range again.Rules {
		again.Rules[i].Episodes = EpisodeCount{}
	}
	for i := range again.Firings {
		again.Firings[i].Episode, again.Firings[i].Day = 0, ""
	}
	rep.Rules[0].Episodes = EpisodeCount{}
	for i := range rep.Firings {
		rep.Firings[i].Episode, rep.Firings[i].Day = 0, ""
	}
	if fmt.Sprint(rep.Rules, rep.Incidents) != fmt.Sprint(again.Rules, again.Incidents) {
		t.Error("judged twice, the rest differs")
	}
}

// Noisy by episodes is by the same thresholds as by firings.
func TestEpisodesNoisy(t *testing.T) {
	var fs []firing.Firing
	for day := range 12 {
		base := ept(3, 0).Add(time.Duration(day) * 24 * time.Hour)
		fs = append(fs, fired("Disk", map[string]string{"service": "db"}, base, base.Add(time.Minute)))
	}
	o := episodeOpts
	o.From, o.To = ept(0, 0), ept(0, 0).Add(14*24*time.Hour)
	keep := incident.Incident{ID: "KEEP", Start: ept(0, 0).Add(-24 * time.Hour), End: ept(0, 0).Add(15 * 24 * time.Hour), Labels: incident.Labels{"service": {"nothing"}}}
	rep := Judge(nil, fs, []incident.Incident{keep}, o)
	if e := rep.Rules[0].Episodes; e.Total != 12 || !e.Noisy || !rep.Rules[0].Noisy {
		t.Errorf("12 episodes a day apart, none caught: %+v, noisy %v", e, rep.Rules[0].Noisy)
	}
	o.NoisyFirings = 13
	if e := Judge(nil, fs, []incident.Incident{keep}, o).Rules[0].Episodes; e.Noisy {
		t.Errorf("under noisy_at.firings: %+v", e)
	}
}
