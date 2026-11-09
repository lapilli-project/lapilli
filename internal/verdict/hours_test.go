package verdict

import (
	"testing"
	"time"

	"lapilli.dev/internal/firing"
)

func TestHoursOf(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip(err)
	}
	h := DefaultHours(berlin)
	for at, want := range map[string]string{
		"2026-11-02T07:59:00Z": AtWork,   // Monday 08:59 in Berlin
		"2026-11-02T06:59:00Z": Asleep,   // 07:59
		"2026-11-02T07:00:00Z": AtWork,   // 08:00
		"2026-11-02T17:00:00Z": OffHours, // 18:00
		"2026-11-02T20:59:00Z": OffHours, // 21:59
		"2026-11-02T21:00:00Z": Asleep,   // 22:00
		"2026-11-01T10:00:00Z": OffHours, // Sunday 11:00
		"2026-11-07T23:30:00Z": Asleep,   // Sunday 00:30
		// The clocks went forward on 29 March: 01:30 UTC was 03:30 there.
		"2026-03-29T01:30:00Z": Asleep,
		"2026-03-30T06:30:00Z": AtWork, // Monday 08:30, summer time
		// And back on 25 October: 07:30 UTC on the Monday is 08:30 again.
		"2026-10-26T07:30:00Z": AtWork,
		"2026-10-26T06:30:00Z": Asleep,
	} {
		tm, _ := time.Parse(time.RFC3339, at)
		if got := h.Of(tm); got != want {
			t.Errorf("%s (%s): %s, want %s", at, tm.In(berlin).Format("Mon 15:04"), got, want)
		}
	}
}

func TestHoursParse(t *testing.T) {
	for in, want := range map[string]string{
		"mon-fri@08:00-18:00":      "mon-fri@08:00-18:00",
		"09:00-17:30":              "mon-fri@09:00-17:30",
		"mon,wed,fri@09:00-17:30":  "mon,wed,fri@09:00-17:30",
		"sat-sun@10:00-14:00":      "sat-sun@10:00-14:00",
		"fri-mon@08:00-24:00":      "mon,fri-sun@08:00-24:00",
		"Mon-Sun@00:00-24:00":      "mon-sun@00:00-24:00",
		"none":                     "none",
		"tue@08:00-12:00,thu":      "",
		"mon-fri@18:00-08:00":      "mon-fri@18:00-08:00", // into the next morning
		"mon-fri@08:00-08:00":      "",
		"mon-fri@8-18":             "",
		"mon-fri@08:00-18:60":      "",
		"mon-fri@24:00-24:00":      "",
		"funday@08:00-18:00":       "",
		"mon-fri@08:00-18:00extra": "",
	} {
		h := DefaultHours(time.UTC)
		err := h.ParseWork(in)
		switch {
		case want == "" && err == nil:
			t.Errorf("ParseWork(%q) took it, as %s", in, h.WorkText())
		case want != "" && err != nil:
			t.Errorf("ParseWork(%q): %v", in, err)
		case want != "" && h.WorkText() != want:
			t.Errorf("ParseWork(%q) = %s, want %s", in, h.WorkText(), want)
		}
	}
	for in, want := range map[string]string{"22:00-08:00": "22:00-08:00", "00:00-06:00": "00:00-06:00", "23:00-24:00": "23:00-24:00",
		"none": "none", "08:00-08:00": "", "22:00": "", "-1:00-08:00": ""} {
		h := DefaultHours(time.UTC)
		err := h.ParseSleep(in)
		switch {
		case want == "" && err == nil:
			t.Errorf("ParseSleep(%q) took it, as %s", in, h.SleepText())
		case want != "" && (err != nil || h.SleepText() != want):
			t.Errorf("ParseSleep(%q) = %s, %v; want %s", in, h.SleepText(), err, want)
		}
	}
	// No working hours: awake is off hours.
	h := DefaultHours(time.UTC)
	if err := h.ParseWork("none"); err != nil {
		t.Fatal(err)
	}
	if got := h.Of(time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)); got != OffHours {
		t.Errorf("Monday 10:00 with no working hours: %s", got)
	}
	// Asleep wins where they overlap.
	if err := h.ParseWork("mon-fri@00:00-24:00"); err != nil {
		t.Fatal(err)
	}
	if got := h.Of(time.Date(2026, 11, 2, 23, 0, 0, 0, time.UTC)); got != Asleep {
		t.Errorf("Monday 23:00, at work all day and asleep from 22:00: %s", got)
	}
}

func TestJudgeHours(t *testing.T) {
	o := opts()
	o.Hours = DefaultHours(time.UTC)
	tue := t0.Add(24 * time.Hour)
	var fs []firing.Firing
	for _, h := range []int{3, 4, 19, 10} {
		at := tue.Add(time.Duration(h) * time.Hour)
		fs = append(fs, firing.Firing{Rule: "Night", Start: at, End: at.Add(time.Minute)})
	}
	rep := Judge(nil, fs, nil, o)
	r := rep.Rules[0]
	if r.Asleep != 2 || r.AsleepBlack != 2 || r.OffHours != 1 || r.OffHoursBlack != 1 || r.Black != 4 {
		t.Errorf("%+v", r)
	}
	if rep.Hours == nil || rep.Hours.Zone != "UTC" || rep.Hours.Work != "mon-fri@08:00-18:00" || rep.Hours.Sleep != "22:00-08:00" {
		t.Errorf("hours: %+v", rep.Hours)
	}
	// Without hours, nothing is counted by them.
	if rep := Judge(nil, fs, nil, opts()); rep.Hours != nil || rep.Rules[0].Asleep != 0 {
		t.Errorf("without hours: %+v %+v", rep.Hours, rep.Rules[0])
	}
}
