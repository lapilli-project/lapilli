package verdict

// Semantics numbers how lapilli replays a change and judges what it fired:
// bumped whenever a change to replaying or judging would make a backtest of
// the same history say something else (0.41.0's firings with no record,
// counted and not judged, would have been one), and never for a release
// that changes neither. A change's backtest kept under another number, or
// under none, as before it was kept, is stale: lapilli serve backtests a
// proposal waiting for approval again before anyone approves it. The
// version of lapilli that ran a backtest is kept beside it, to be shown.
const Semantics = 1
