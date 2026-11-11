# On-call feeds, as each tool writes them

None of the tools documents its events' layout. Each file follows what
could be seen of its tool's exports, with names, emails and times changed
to the tests' world (Jane Doe, Ali Kaya and Sam Lee; late October and
early November 2026), lines ending CRLF and folded as RFC 5545 does.

| File | Tool | What it follows |
|---|---|---|
| `pagerduty.ics` | PagerDuty's schedule feed (`webcal://SUB.pagerduty.com/private/KEY/feed/SCHEDULE`) | real exports kept as test data in other projects: `PRODID` of the ri_cal gem, `X-WR-CALNAME`, `DTSTART;VALUE=DATE-TIME:...Z`, a bare `ATTENDEE` email, `SUMMARY:On Call - NAME - SCHEDULE` folded, a `UID` with spaces in it; https://docs.pagerduty.com/incident-management/schedules/schedules-in-apps |
| `opsgenie.ics` | Opsgenie's `GET /v2/schedules/ID.ics` | a real export: `PRODID:OPSGENIE-ICAL-2.0`, `SUMMARY:NAME (user) is on call for schedule: S.`, no attendee; https://docs.opsgenie.com/docs/schedule-api |
| `grafana-oncall.ics` | Grafana OnCall's schedule export | its source, `engine/apps/schedules/models/on_call_schedule.py` and `ical_utils.py`: `SUMMARY` the username, `PRIORITY` 1 for an override, a removed turn kept as `STATUS:CANCELLED` |
| `google.ics` | Google Calendar's secret address in iCal format | a calendar's export: `VTIMEZONE`, `TZID=`, weekly `RRULE` with `UNTIL` and `COUNT`, `EXDATE`, an occurrence replaced by `RECURRENCE-ID`, `ATTENDEE;CN=...:mailto:`, a `VALARM`, an all-day event, and two this reader passes over: a Windows zone name and a monthly repeat |
