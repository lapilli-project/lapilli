package ledger

// MissedFor is how long a rule's missed period is held for it: a run that
// misses it again past that gives it up. lapilli serve, yielding to a 429
// (docs/design/quota.md), moves the cursor no further than this past the
// oldest it missed so.
const MissedFor = missedFor
