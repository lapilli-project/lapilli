package oncall

// What an on-call feed can make lapilli do (0.34.1). A feed is outside
// input (ics.go: "what a schedule's editor writes in it is outside
// input"), and TestFeedBounds holds a hostile feed to a read of five
// seconds at most. lapilli serve reads each feed one schedule after
// another, every --on-call "every" (5m at least), each within a minute
// (serve's feedTime).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// hostileBound is the time TestFeedBounds allows a hostile feed.
const hostileBound = 5 * time.Second

func hostileFeed(body string) []byte {
	return []byte("BEGIN:VCALENDAR\r\n" + body + "END:VCALENDAR\r\n")
}

// manyOverrides is repeats events under one UID, each every day over
// the whole span read (WEEKLY by all seven days: few steps), and overrides
// RECURRENCE-ID events of that UID with no DTSTART of their own, which make
// no turn: decoys first that match no occurrence, then one per occurrence.
// Every occurrence is then skipped as replaced (so no turn is made and
// maxTurns never trips), each after a linear scan past every decoy
// (ics.go: slices.ContainsFunc(replaced[e.uid], st.Equal)).
func manyOverrides(repeats, overrides int) (data []byte, occurrences int) {
	start := now.Add(-Back - Ahead).Truncate(24 * time.Hour)
	var occ []time.Time
	for t := start; !t.After(now.Add(Ahead)); t = t.AddDate(0, 0, 1) {
		occ = append(occ, t)
	}
	var b strings.Builder
	for range repeats {
		fmt.Fprintf(&b, "BEGIN:VEVENT\r\nUID:x\r\nDTSTART:%s\r\nDURATION:PT1H\r\nRRULE:FREQ=WEEKLY;BYDAY=SU,MO,TU,WE,TH,FR,SA\r\nSUMMARY:Sam Lee\r\nEND:VEVENT\r\n",
			start.Format("20060102T150405Z"))
	}
	for i := range overrides - len(occ) {
		fmt.Fprintf(&b, "BEGIN:VEVENT\r\nUID:x\r\nRECURRENCE-ID:%s\r\nEND:VEVENT\r\n", start.Add(time.Duration(i+1)*time.Second).Format("20060102T150405Z"))
	}
	for _, t := range occ {
		fmt.Fprintf(&b, "BEGIN:VEVENT\r\nUID:x\r\nRECURRENCE-ID:%s\r\nEND:VEVENT\r\n", t.Format("20060102T150405Z"))
	}
	return hostileFeed(b.String()), len(occ)
}

// A feed of 20,000 events (2 MB), half repeats and half overrides of one
// UID, held a core for about two minutes while every occurrence of every
// repeat scanned the overrides: they are looked up.
func TestFeedOverridesLookedUp(t *testing.T) {
	cfg := config(t, "  - {name: s, file: x}\n")
	s := cfg.Schedules[0]

	// The cost of 256 repeats, to say what the whole feed costs.
	small, occ := manyOverrides(256, 10000)
	began := time.Now()
	if _, err := Parse(context.Background(), small, s, cfg, now); err != nil {
		t.Fatal(err)
	}
	per := time.Since(began) / 256
	t.Logf("256 repeats x %d occurrences x 10,000 overrides: %s, %s a repeat; 10,000 repeats: about %s", occ, per*256, per, (per * 10000).Round(time.Second))

	data, _ := manyOverrides(10000, 10000)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	began = time.Now()
	f, err := Parse(ctx, data, s, cfg, now)
	took := time.Since(began)
	t.Logf("the whole feed, %d bytes, 20,000 events: %s, err %v", len(data), took, err)
	if took > hostileBound || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a 2 MB feed of 20,000 events took %s (cut off by an 8s deadline: %v), past the %s a hostile feed is held to; serve reads it with no deadline", took, errors.Is(err, context.DeadlineExceeded), hostileBound)
	}
	if f != nil && len(f.Turns) != 0 {
		t.Logf("turns: %d", len(f.Turns))
	}
}

// byDayRepeated is events weekly repeats counted from 1970 (every week a
// step), each BYDAY naming Monday days times: every step looks each of the
// six other days up in the whole list (slices.Contains(byday, ...)).
func byDayRepeated(events, days int) []byte {
	byday := strings.TrimSuffix(strings.Repeat("MO,", days), ",")
	var b strings.Builder
	for i := range events {
		fmt.Fprintf(&b, "BEGIN:VEVENT\r\nUID:b%d\r\nDTSTART:19700105T000000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=WEEKLY;COUNT=200000;BYDAY=%s\r\nSUMMARY:nobody\r\nEND:VEVENT\r\n", i, byday)
	}
	return hostileFeed(b.String())
}

// A feed of 128 events (8 MB) whose BYDAY names one day 21,000 times held
// a core for about 13 seconds: the days are looked up.
func TestFeedByDayRepeated(t *testing.T) {
	cfg := config(t, "  - {name: s, file: x}\n")
	s := cfg.Schedules[0]
	data := byDayRepeated(128, 21000)
	if len(data) > MaxFeed {
		t.Fatalf("%d bytes, over MaxFeed", len(data))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	began := time.Now()
	_, err := Parse(ctx, data, s, cfg, now)
	took := time.Since(began)
	t.Logf("%d bytes, 128 events, BYDAY of 21,000 days each: %s, err %v", len(data), took, err)
	if took > hostileBound {
		t.Errorf("an 8 MB feed of 128 events took %s, past the %s a hostile feed is held to", took, hostileBound)
	}
}

// zonedExdates is events events each leaving out exdates dates written in
// a zone TZID names: each date loads the zone from disk again
// (time.LoadLocation, uncached).
func zonedExdates(events, exdates int) []byte {
	var ex strings.Builder
	for i := range exdates {
		if i > 0 {
			ex.WriteByte(',')
		}
		ex.WriteString(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute).Format("20060102T150405"))
	}
	var b strings.Builder
	for i := range events {
		fmt.Fprintf(&b, "BEGIN:VEVENT\r\nUID:z%d\r\nDTSTART;TZID=America/New_York:20250101T000000\r\nDURATION:PT1H\r\nEXDATE;TZID=America/New_York:%s\r\nSUMMARY:nobody\r\nEND:VEVENT\r\n", i, ex.String())
	}
	return hostileFeed(b.String())
}

// A feed of 500 events (8 MB) of a thousand zoned EXDATEs each loaded the
// zone half a million times, about 11 seconds, its lines read without a
// look at the context: a zone is loaded once a feed.
func TestFeedZoneEachDate(t *testing.T) {
	cfg := config(t, "  - {name: s, file: x}\n")
	s := cfg.Schedules[0]
	data := zonedExdates(500, 1000)
	if len(data) > MaxFeed {
		t.Fatalf("%d bytes, over MaxFeed", len(data))
	}
	ctx, cancel := context.WithTimeout(context.Background(), hostileBound)
	defer cancel()
	began := time.Now()
	_, err := Parse(ctx, data, s, cfg, now)
	took := time.Since(began)
	t.Logf("%d bytes, 500 events x 1,000 zoned EXDATEs: %s, err %v", len(data), took, err)
	if took > hostileBound+time.Second || err != nil {
		t.Errorf("an 8 MB feed of zoned dates isn't read within the %s a hostile feed is held to: %s, %v", hostileBound, took, err)
	}
}

// The lines of a feed are read with a look at the context: one ended stops
// the read there, before a line past the bounds at the end is reached.
func TestFeedLinesHeedContext(t *testing.T) {
	cfg := config(t, "  - {name: s, file: x}\n")
	data := hostileFeed(strings.Repeat("X-PAD:1\r\n", 4096) + "X-LONG:" + strings.Repeat("x", maxLine) + "\r\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Parse(ctx, data, cfg.Schedules[0], cfg, now); !errors.Is(err, context.Canceled) {
		t.Errorf("a feed read with its context ended: %v", err)
	}
	if _, err := Parse(context.Background(), data, cfg.Schedules[0], cfg, now); err == nil || !strings.Contains(err.Error(), "over 64 KB") {
		t.Errorf("a line past the bounds: %v", err)
	}
}

// countdown is a context whose Err says nothing for its first n calls,
// and Canceled after.
type countdown struct {
	context.Context
	n atomic.Int32
}

func (c *countdown) Err() error {
	if c.n.Add(-1) < 0 {
		return context.Canceled
	}
	return nil
}

// A repeat is expanded with a look at the context: one that ends past the
// events' look stops it there.
func TestFeedRepeatHeedsContext(t *testing.T) {
	cfg := config(t, "  - {name: s, file: x}\n")
	data := hostileFeed("BEGIN:VEVENT\r\nUID:r\r\nDTSTART:20260101T000000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=10\r\nSUMMARY:Sam Lee\r\nEND:VEVENT\r\n")
	ctx := &countdown{Context: context.Background()}
	ctx.n.Store(1) // the events' look; the repeat's ends it
	if _, err := Parse(ctx, data, cfg.Schedules[0], cfg, now); !errors.Is(err, context.Canceled) {
		t.Errorf("a repeat expanded past its context's end: %v", err)
	}
	if f, err := Parse(context.Background(), data, cfg.Schedules[0], cfg, now); err != nil || len(f.Turns) == 0 {
		t.Errorf("the repeat: %v", err)
	}
}
