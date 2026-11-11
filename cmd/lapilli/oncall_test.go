package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// lapilli verdict --on-call reads each feed once, and --shifts
// on-call=NAME counts firings by the hours of whoever's turn they came in;
// shifts naming a schedule the file doesn't are refused before anything is
// read.
func TestVerdictOnCall(t *testing.T) {
	newHistory = func(context.Context, *verdictFlags) (history, error) { return fakeHistory{}, nil }
	now = func() time.Time { return t0 }
	// The checkout rule fires from 3h to 2h before t0 (2026-11-02 00:00
	// UTC): 21:00 to 22:00 UTC, 06:00 to 07:00 in Seoul, Jane asleep.
	feed := write(t, "payments.ics", "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nDTSTART:20261101T000000Z\r\nDTEND:20261103T000000Z\r\n"+
		"ATTENDEE:jane@example.com\r\nSUMMARY:On Call\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	cfg := write(t, "on-call.yaml", "schedules:\n  - {name: payments-verdict, file: "+feed+"}\n"+
		"people:\n  - {name: Jane Doe, also: [jane@example.com], tz: Asia/Seoul, sleep: '23:00-07:30', work: none}\n")
	rules := write(t, "rules.yaml", "groups:\n  - name: shop\n    rules:\n      - alert: CheckoutErrors\n        expr: errors > 5\n        for: 5m\n")
	incidents := write(t, "incidents.yaml", "incidents: []\n")
	base := []string{"verdict", "--rules", rules, "--prometheus", "http://p", "--incidents", incidents, "--tz", "UTC", "--json"}
	var stdout, stderr bytes.Buffer
	if code := run(append(base, "--on-call", cfg, "--shifts", "on-call=payments-verdict"), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"asleep": 1`) || !strings.Contains(stdout.String(), `"shifts": "on-call=payments-verdict"`) {
		t.Errorf("by Jane's hours:\n%s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(append(base, "--shifts", "on-call=payments-verdict"), &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "no schedule") {
		t.Errorf("without --on-call: %d %s", code, stderr.String())
	}
}
