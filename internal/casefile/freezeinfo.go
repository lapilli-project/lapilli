package casefile

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// FreezeInfo is freeze.json: when the incident was frozen and what the freeze found.
type FreezeInfo struct {
	// FreezeTime is the instant of the freeze, in Unix seconds: named before anything is read, the
	// cluster is collected from then on and the metrics are read up to it. It is the "now" a replayed
	// case answers from.
	FreezeTime      float64 `json:"freeze_time"`
	FrozenAt        string  `json:"frozen_at"`
	SecretsRedacted int     `json:"secrets_redacted"`
	// EvidenceInSnapshot says, per decisive pattern, whether the frozen copy contains it: for the
	// Kubernetes store, in some file of the snapshot; for the metrics store, in what a query prints at
	// the freeze. A case with a false here cannot be solved from the copy, whatever the agent does.
	EvidenceInSnapshot map[string]bool `json:"evidence_in_snapshot"`
	Stores             []string        `json:"stores"`
	Metrics            *MetricsInfo    `json:"metrics,omitempty"`
	// LogsAdded is how many logs `freeze` fetched itself because the collector leaves them out —
	// those of init and ephemeral containers — and LogsMissing names the ones it asked for and did
	// not get, as namespace/pod/container. A case made by `pack` has neither.
	LogsAdded   int      `json:"logs_added,omitempty"`
	LogsMissing []string `json:"logs_missing,omitempty"`
}

// MetricsInfo describes the frozen metrics store.
type MetricsInfo struct {
	Series  int   `json:"series"`
	Samples int   `json:"samples"`
	Oldest  int64 `json:"oldest_ms"`
	Newest  int64 `json:"newest_ms"`
	// PrometheusVersion and EvaluationIntervalMs are what `freeze` asked the Prometheus about itself:
	// its version, and its global evaluation interval, which is the step of a subquery that names
	// none. A store that did not say, and a case made by `pack` or frozen before 2026-10-08, has
	// neither, and is replayed with Prometheus's default of one minute.
	PrometheusVersion    string `json:"prometheus_version,omitempty"`
	EvaluationIntervalMs int64  `json:"evaluation_interval_ms,omitempty"`
	// LookbackDeltaMs is its --query.lookback-delta: how far before an instant a sample still counts.
	// Without it a replay uses Prometheus's default of five minutes.
	LookbackDeltaMs int64 `json:"lookback_delta_ms,omitempty"`
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
