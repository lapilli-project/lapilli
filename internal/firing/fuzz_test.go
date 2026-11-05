package firing

import (
	"bytes"
	"testing"
)

// Firings read from a file never panic, whatever the file holds.
func FuzzReadJSONL(f *testing.F) {
	f.Add([]byte(`{"rule":"A","labels":{"service":"x"},"start":"2026-11-02T06:00:00Z","end":"2026-11-02T07:00:00Z"}
{"rule":"B","start":"2026-11-02T06:00:00Z"}
`))
	f.Fuzz(func(t *testing.T, b []byte) {
		ReadJSONL(bytes.NewReader(b))
	})
}
