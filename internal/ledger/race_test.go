//go:build race

package ledger

func init() { slow, race = 10, true }
