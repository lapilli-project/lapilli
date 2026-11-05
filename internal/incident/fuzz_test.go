package incident

import (
	"testing"

	"go.yaml.in/yaml/v3"
)

// An incidents file is read whatever it holds, and its labels, a value or a
// list of them, never panic.
func FuzzIncidents(f *testing.F) {
	f.Add([]byte(`incidents:
  - {id: INC-1, title: t, start: 2026-11-02T06:01:00Z, end: 2026-11-02T06:40:00Z, labels: {service: [checkout, payments], team: pay}}
`))
	f.Fuzz(func(t *testing.T, b []byte) {
		var doc struct {
			Incidents []Incident `yaml:"incidents"`
		}
		yaml.Unmarshal(b, &doc)
	})
}
