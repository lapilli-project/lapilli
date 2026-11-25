package deploys

import (
	"time"

	"lapilli.dev/internal/firing"
)

// Deploy is when something was deployed (docs/design/noc.md): a service, a
// namespace, as its labels say, from Start to End.
type Deploy struct {
	Start    time.Time         `json:"start"`
	End      time.Time         `json:"end"`
	Labels   map[string]string `json:"labels,omitempty"`
	Revision string            `json:"revision,omitempty"`
	// Source is the history it was read from: a file's name.
	Source string `json:"source,omitempty"`
}

// Bounds on what the deploys file holds: the ledger keeps MaxDeploys of
// them, each lasting MaxDeployLen at most, their labels MaxDeployLabels
// bytes in all, counted for each deploy; a firing counts as during one up
// to MaxDeployAfter before its start or after its end. A deploy starts no
// later than DeployAhead after the file is read.
const (
	MaxDeploys      = 50000
	MaxDeployLen    = 24 * time.Hour
	MaxDeployAfter  = 24 * time.Hour
	MaxDeployLabels = 8 << 20
	DeployAhead     = time.Hour
)

// During reports whether a firing came during the deploy: it started from
// before the deploy's start to after its end, and agrees with the deploy on
// every label both have, one at least (the same service, or a namespace's
// alert during a deploy in it). A label the firing hasn't stands for any
// value. A deploy that names no label is everything's.
func (d Deploy) During(f firing.Firing, before, after time.Duration) bool {
	if f.Start.Before(d.Start.Add(-before)) || f.Start.After(d.End.Add(after)) {
		return false
	}
	return TiedLabels(d.Labels, f.Labels)
}

// Tied reports whether a firing with labels is of what the deploy names,
// as During holds it, whenever it started: it agrees with the deploy on
// every label both have, one at least; a deploy that names no label is
// everything's.
func (d Deploy) Tied(labels map[string]string) bool { return TiedLabels(d.Labels, labels) }

// TiedLabels reports whether labels agree with a deploy's: every label both
// have, one at least; a deploy naming no label is everything's.
func TiedLabels(deployed, labels map[string]string) bool {
	if len(deployed) == 0 {
		return true
	}
	shared := false
	for k, v := range deployed {
		have, ok := labels[k]
		if !ok {
			continue
		}
		if have != v {
			return false
		}
		shared = true
	}
	return shared
}
