package casefile

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// FreezeInfo is freeze.json: when the incident was frozen and what the freeze found.
type FreezeInfo struct {
	// FreezeTime is the instant, in Unix seconds, at which every store was read. It is the "now" a
	// replayed case answers from.
	FreezeTime      float64 `json:"freeze_time"`
	FrozenAt        string  `json:"frozen_at"`
	SecretsRedacted int     `json:"secrets_redacted"`
	// EvidenceInSnapshot says, per decisive pattern, whether the frozen copy contains it: for the
	// Kubernetes store, in some file of the snapshot; for the metrics store, in what a query prints at
	// the freeze. A case with a false here cannot be solved from the copy, whatever the agent does.
	EvidenceInSnapshot map[string]bool `json:"evidence_in_snapshot"`
	Stores             []string        `json:"stores"`
	Metrics            *MetricsInfo    `json:"metrics,omitempty"`
}

// MetricsInfo describes the frozen metrics store.
type MetricsInfo struct {
	Series  int   `json:"series"`
	Samples int   `json:"samples"`
	Oldest  int64 `json:"oldest_ms"`
	Newest  int64 `json:"newest_ms"`
}

// LoadFreezeInfo reads freeze.json from a case directory.
func LoadFreezeInfo(dir string) (*FreezeInfo, error) {
	raw, err := os.ReadFile(filepath.Join(dir, FreezeName))
	if err != nil {
		return nil, err
	}
	var info FreezeInfo
	return &info, json.Unmarshal(raw, &info)
}

// Save writes freeze.json into a case directory.
func (f *FreezeInfo) Save(dir string) error {
	raw, err := json.MarshalIndent(f, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, FreezeName), append(raw, '\n'), 0o644)
}
