//go:build !unix

package preflight

// writable can't be asked here without writing: taken as yes.
func writable(string) error { return nil }

// freeBytes isn't known here.
func freeBytes(string) (uint64, bool) { return 0, false }
