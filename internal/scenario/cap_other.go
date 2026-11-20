//go:build !linux

package scenario

// CanCap says a run's memory can be capped here: not on this platform
// (macOS refuses RLIMIT_DATA and doesn't hold mapped memory to it), where
// promtool runs without a cap.
const CanCap = false
