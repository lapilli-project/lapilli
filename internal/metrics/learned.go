package metrics

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// What an export learns of its Prometheus besides the samples is in the Store and not in a metrics
// file, which holds samples: the Prometheus's version and how it evaluates, where the metrics begin,
// where its blocks ended and whether the series are in its head's order, the labels it adds to what
// it sends elsewhere, and what kind each metric is. `freeze` has the Store in hand and writes all of
// that into the case. `export-metrics` has a file to leave, for `pack` to seal later — the path a
// freeze names when its metrics fail after the cluster was collected — and a case made that way said
// nothing of a query that looks before its beginning, handed every query its series in one order,
// and knew no metric's kind. So the export leaves a second file beside the first, and `pack` reads
// the two together.
//
// It is no part of a case: what it holds goes into freeze.json and metrics-metadata.json, as a
// freeze puts it there.

// LearnedPath is where what an export learned is kept, beside the metrics file it wrote.
func LearnedPath(metricsFile string) string { return metricsFile + ".learned.json" }

// learned is that file. Its names are freeze.json's for the same things, and `metadata` holds what
// metrics-metadata.json holds. The four numbers before them say which metrics file it is of.
type learned struct {
	Series               int                   `json:"series"`
	Samples              int                   `json:"samples"`
	Oldest               int64                 `json:"oldest_ms"`
	Newest               int64                 `json:"newest_ms"`
	PrometheusVersion    string                `json:"prometheus_version,omitempty"`
	EvaluationIntervalMs int64                 `json:"evaluation_interval_ms,omitempty"`
	LookbackDeltaMs      int64                 `json:"lookback_delta_ms,omitempty"`
	FromMs               int64                 `json:"from_ms,omitempty"`
	HeadFromMs           int64                 `json:"head_from_ms,omitempty"`
	SeriesOrder          string                `json:"series_order,omitempty"`
	ExternalLabels       map[string]string     `json:"external_labels,omitempty"`
	Metadata             map[string][]Metadata `json:"metadata,omitempty"`
}

// orderOfTheHead is what freeze.json says of a case whose series are in its Prometheus's head's
// order (casefile.OrderOfTheHead; a test holds the two to one word).
const orderOfTheHead = "head"

// of is the four numbers by which a file of what was learned is known to be of this store.
func (s *Store) of() (l learned) {
	l.Series, l.Samples = s.Size()
	l.Oldest, l.Newest, _ = s.Bounds()
	return l
}

// SaveLearned writes, beside a metrics file, what the store knows that the file does not hold.
func (s *Store) SaveLearned(metricsFile string) error {
	l := s.of()
	l.PrometheusVersion, l.EvaluationIntervalMs, l.LookbackDeltaMs = s.Source.Version, s.Source.EvaluationInterval.Milliseconds(), s.Source.LookbackDelta.Milliseconds()
	l.FromMs, l.HeadFromMs, l.ExternalLabels = s.From, s.HeadFrom, s.ExternalLabels
	if s.OrderKnown {
		l.SeriesOrder = orderOfTheHead
	}
	l.Metadata = make(map[string][]Metadata, len(s.Metadata))
	for family, entries := range s.Metadata { // in one order, so that the same is the same bytes; and not the caller's own put in order
		l.Metadata[family] = append([]Metadata(nil), entries...)
		sortMetadata(l.Metadata[family])
	}
	raw, err := json.MarshalIndent(l, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(LearnedPath(metricsFile), append(raw, '\n'), 0o644)
}

// LoadLearned reads it back into a store loaded from that metrics file, and says whether there was
// such a file. A metrics file with none beside it was written before an export left one, or was
// moved without it: the store is left as the file made it, and a case sealed from it says nothing
// of what it does not know. One that is of another metrics file — other series, other samples — is
// refused: what it says would be said of the wrong ones.
func (s *Store) LoadLearned(metricsFile string) (bool, error) {
	raw, err := os.ReadFile(LearnedPath(metricsFile))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	var l learned
	if err := json.Unmarshal(raw, &l); err != nil {
		return false, fmt.Errorf("%s: %w", LearnedPath(metricsFile), err)
	}
	if have := s.of(); l.Series != have.Series || l.Samples != have.Samples || l.Oldest != have.Oldest || l.Newest != have.Newest {
		return false, fmt.Errorf("%s is of another metrics file: it says %d series and %d samples from %d to %d, and %s holds %d and %d from %d to %d",
			LearnedPath(metricsFile), l.Series, l.Samples, l.Oldest, l.Newest, metricsFile, have.Series, have.Samples, have.Oldest, have.Newest)
	}
	if l.SeriesOrder != "" && l.SeriesOrder != orderOfTheHead {
		return false, fmt.Errorf("%s: series_order is %q, and the one order a case can say is %q", LearnedPath(metricsFile), l.SeriesOrder, orderOfTheHead)
	}
	s.Source = Source{Version: l.PrometheusVersion, EvaluationInterval: time.Duration(l.EvaluationIntervalMs) * time.Millisecond, LookbackDelta: time.Duration(l.LookbackDeltaMs) * time.Millisecond}
	s.From, s.HeadFrom, s.ExternalLabels, s.OrderKnown = l.FromMs, l.HeadFromMs, l.ExternalLabels, l.SeriesOrder == orderOfTheHead
	s.Metadata = l.Metadata
	return true, nil
}
