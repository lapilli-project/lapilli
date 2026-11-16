package serve

import "lapilli.dev/internal/ledger"

// Status colours (docs/design/ui.md, "Status colours"): what a state the
// UI shows is, at a glance, as one of five kinds, each a class the
// stylesheet colours on small surfaces alone, a badge, a pill, a notice, a
// cell or a word, never a row or a card:
//
//   - ok: fine, done, as predicted;
//   - warn: needs a person's look: noisy, stale, waiting for approval;
//   - bad: failed, missed, overdue;
//   - info: under way, or in effect now;
//   - unknown: not known, as with no record, or not judged: never drawn
//     as ok or bad.
//
// The same state is the same kind on every page; each also has a word or
// a sign, never its colour alone.

// statusKinds are the kind of each state a page draws from the ledger, by
// what it is a state of and its value. A value not listed is unknown.
var statusKinds = map[string]string{
	// How a rule did, over the weeks shown.
	"perf/ok": "ok", "perf/noisy": "warn", "perf/unrecorded": "unknown", "perf/silent": "unknown", "perf/spare": "unknown",
	// Its backtest, when it can't be taken for granted.
	"trust/differs": "warn", "trust/unreplayed": "warn", "trust/recorded": "info",
	// What people mean to do with it, as its note says.
	"status/keep": "ok", "status/tune": "info", "status/retire": "unknown",
	// What is under way: a change proposed, and what its page lists to
	// check.
	"has/proposal": "info", "has/facts": "warn",
	// A task.
	"task/open": "info", "task/doing": "info", "task/done": "ok", "task/dropped": "unknown",
	// A proposal: waiting for a person, approved, under way, applied or
	// undone; closed by its reason, failed apart from the rest.
	"proposal/" + ledger.StateProposed: "warn", "proposal/" + ledger.StateApproved: "ok",
	"proposal/" + ledger.StateReady: "info", "proposal/" + ledger.StateApplying: "info",
	"proposal/" + ledger.StateOpened: "info", "proposal/" + ledger.StateMerged: "info",
	"proposal/" + ledger.StateApplied: "ok", "proposal/" + ledger.StateUndone: "unknown",
	"proposal/" + ledger.StateFailed: "bad", "proposal/" + ledger.StateExpired: "warn",
	"proposal/" + ledger.StateRefused: "unknown", "proposal/" + ledger.StateDropped: "unknown",
	"proposal/" + ledger.StateAmended: "unknown",
	// An incident, by what caught it; covered, confirmed and dismissed
	// impact windows aren't judged against.
	"incident/caught": "ok", "incident/noisy": "warn", "incident/missed": "bad",
	// A rule change, predicted and measured.
	"outcome/as-predicted": "ok", "outcome/otherwise": "warn", "outcome/measuring": "info", "outcome/added": "info",
	"outcome/unreplayed": "warn",
	// Where incidents come from (sources.go): alive; quiet, nothing
	// received, or read, in a while; no data, an impact signal a selector
	// of which has none; unread, one read by the runs and waiting to be
	// read again, which keeps no record meanwhile; failing, its last read
	// failed; waiting, set up and not read yet; none written in four
	// weeks, or not set up.
	"source/ok": "ok", "source/quiet": "warn", "source/nodata": "warn", "source/unread": "warn", "source/failing": "bad", "source/pending": "info",
	"source/none": "unknown", "source/off": "unknown",
}

// statusSigns are each kind's sign, before a notice's sentence: what a
// notice says, said again for the eye, never its colour alone.
var statusSigns = map[string]string{
	"status-ok": "✓", "status-warn": "⚠\uFE0E", "status-bad": "✕", "status-info": "↻", "status-unknown": "?",
}

// statusSign is a status class's sign.
func statusSign(class string) string { return statusSigns[class] }

// statusClass is the class that colours a state of what (perf, task,
// proposal …): status-ok, status-warn, status-bad, status-info or
// status-unknown.
func statusClass(what, value string) string {
	if k, ok := statusKinds[what+"/"+value]; ok {
		return "status-" + k
	}
	return "status-unknown"
}
