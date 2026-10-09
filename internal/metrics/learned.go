package metrics

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
// freeze puts it there, and `pack` does not carry the file itself into a case.

// LearnedPath is where what an export learned is kept, beside the metrics file it wrote.
func LearnedPath(metricsFile string) string { return metricsFile + ".learned.json" }

// learned is that file. Its names are freeze.json's for the same things, and `metadata` holds what
// metrics-metadata.json holds. `of_sha256` says which metrics file it is of: the SHA-256 of that
// file's bytes. The series, the samples and their order are all in those bytes, and an export made
// again at the same instant — which is what a freeze tells its user to do — is of the same window
// and may be of another order, another head, other labels.
type learned struct {
	Of                   string                `json:"of_sha256"`
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

// longestSetting is more than any Prometheus is set to evaluate at or look back: a number past it in
// a file of what was learned is no setting, and one large enough is no duration at all.
const longestSetting = 366 * 24 * time.Hour

func digestOf(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ForgetLearned removes what is beside a metrics file, if anything is: before a file is written
// under that name again, what an earlier export left there is of another file; and beside a case's
// own metrics it has been read, and is no part of the case.
func ForgetLearned(metricsFile string) error {
	if err := os.Remove(LearnedPath(metricsFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// SaveLearned writes, beside a metrics file the store was just saved to, what the store knows that
// the file does not hold.
func (s *Store) SaveLearned(metricsFile string) error {
	of, err := digestOf(metricsFile)
	if err != nil {
		return err
	}
	l := learned{Of: of}
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
// of what it does not know. One that is of another metrics file is refused: what it says would be
// said of the wrong series. And so is one that says what no export says — a name it does not
// know, which may be one misspelt; a setting that is no duration; where the blocks ended and not
// that the order is the head's — since what it says is written into a case as it stands.
func (s *Store) LoadLearned(metricsFile string) (bool, error) {
	path := LearnedPath(metricsFile)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	var l learned
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	have, err := digestOf(metricsFile)
	if err != nil {
		return false, err
	}
	if l.Of != have {
		return false, fmt.Errorf("%s is of another metrics file than %s: it was written beside one whose SHA-256 is %s, and this one's is %s", path, metricsFile, l.Of, have)
	}
	oldest, _, any := s.Bounds()
	switch {
	case l.EvaluationIntervalMs < 0 || l.EvaluationIntervalMs > longestSetting.Milliseconds() || l.LookbackDeltaMs < 0 || l.LookbackDeltaMs > longestSetting.Milliseconds():
		return false, fmt.Errorf("%s: evaluation_interval_ms %d and lookback_delta_ms %d are not both what a Prometheus is set to", path, l.EvaluationIntervalMs, l.LookbackDeltaMs)
	case l.HeadFromMs < 0 || any && l.FromMs > oldest: // what was read begins no later than its first sample, and no block ends before there was time
		return false, fmt.Errorf("%s: from_ms %d and head_from_ms %d are not both of metrics whose first sample is at %d", path, l.FromMs, l.HeadFromMs, oldest)
	case l.SeriesOrder != "" && l.SeriesOrder != orderOfTheHead:
		return false, fmt.Errorf("%s: series_order is %q, and the one order a case can say is %q", path, l.SeriesOrder, orderOfTheHead)
	case l.HeadFromMs != 0 && l.SeriesOrder == "":
		return false, fmt.Errorf("%s: it says where the Prometheus's blocks ended and not that the series are in its head's order, which no export does", path)
	}
	for family, entries := range l.Metadata {
		if family == "" || len(entries) == 0 {
			return false, fmt.Errorf("%s: metadata has the family %q with nothing said of it", path, family)
		}
	}
	s.Source = Source{Version: l.PrometheusVersion, EvaluationInterval: time.Duration(l.EvaluationIntervalMs) * time.Millisecond, LookbackDelta: time.Duration(l.LookbackDeltaMs) * time.Millisecond}
	s.From, s.HeadFrom, s.ExternalLabels, s.OrderKnown = l.FromMs, l.HeadFromMs, l.ExternalLabels, l.SeriesOrder == orderOfTheHead
	s.Metadata = l.Metadata
	return true, nil
}
