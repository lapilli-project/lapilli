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
	// FromMs is the instant the freeze began reading the Prometheus at: the window it was asked for
	// before the freeze, and as far again as an instant looks back. It is where the case's metrics
	// begin, which the oldest sample is not when the Prometheus was younger than that. A replay
	// says so beside the answer to a query that looks further back. Absent — a case frozen before
	// `freeze` recorded it, which it has since 2026-10-08, or made by `pack` — nothing is said.
	FromMs int64 `json:"from_ms,omitempty"`
	// HeadFromMs is where the Prometheus's blocks ended when it was frozen, if it had any. Its series
	// come from the head alone in the order the head made them, and by label where a block is reached;
	// a replay hands a query its series the one way or the other by how far back the query looks.
	// Absent, the series are handed over in the order of the metrics file whatever is asked — rightly,
	// if SeriesOrder says the order is the head's, because that Prometheus had no block.
	HeadFromMs int64 `json:"head_from_ms,omitempty"`
	// SeriesOrder is "head" when the metrics file is in the order the Prometheus's head had its
	// series: it said where its blocks end, or that it has none, and its head could be listed.
	// Absent, the file is in the order it was read in, which is by label where the reading reached a
	// block, and nothing is known of the head's.
	SeriesOrder string `json:"series_order,omitempty"`
	// ExternalLabels are what the Prometheus adds to every series it sends over remote read, and its
	// own queries do not have. `freeze` takes them off again; they are named here to say that it did.
	ExternalLabels map[string]string `json:"external_labels,omitempty"`
	// PrometheusVersion and EvaluationIntervalMs are what `freeze` asked the Prometheus about itself:
	// its version, and its global evaluation interval, which is the step of a subquery that names
	// none. A store that did not say, and a case made by `pack` or frozen before 2026-10-08, has
	// neither, and is replayed with Prometheus's default of one minute.
	PrometheusVersion    string `json:"prometheus_version,omitempty"`
	EvaluationIntervalMs int64  `json:"evaluation_interval_ms,omitempty"`
	// LookbackDeltaMs is its --query.lookback-delta: how far before an instant a sample still counts.
	// Without it a replay uses Prometheus's default of five minutes.
	LookbackDeltaMs int64 `json:"lookback_delta_ms,omitempty"`
	// MetadataFamilies is how many metric families the case's metadata file describes: what kind of
	// metric each is, its help and its unit, as the Prometheus knew them. Absent, the case has no such
	// file — it was frozen before `freeze` asked for it, which it has since 2026-10-08, made by
	// `pack`, or read from a store that could not be asked — and a replay answers that it knows of
	// none.
	MetadataFamilies int `json:"metadata_families,omitempty"`
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
