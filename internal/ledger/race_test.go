//go:build race

package ledger

func init() { slow = 10 }
