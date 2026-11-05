//go:build race

package serve

func init() { slow = 10 }
