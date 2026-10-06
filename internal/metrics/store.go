// Package metrics is the frozen metrics store of a case: the samples as they were, Prometheus's own
// PromQL engine over them, and a clock that stands still at the moment of the freeze.
//
// It deliberately does not import Prometheus's top-level tsdb package. That package reaches the remote
// storage configuration and with it the AWS, Azure and Google SDKs, for the sake of reading a block from
// disk. A case carries its samples in a format this project owns instead (docs/case-format.md), and an
// in-memory store is all the engine has to be given. CI fails if a cloud SDK ever enters the graph.
package metrics

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/util/annotations"
)

// staleToken is how a staleness marker is written. A marker is a NaN with one particular payload, and
// "this series stopped being exposed here" is what it means; JSON has no NaN at all, so a store that
// writes values as numbers silently loses every series that contains one. That happened once.
const staleToken = "stale"

func formatValue(f float64) string {
	if value.IsStaleNaN(f) {
		return staleToken
	}
	return strconv.FormatFloat(f, 'g', -1, 64) // shortest form that parses back to the same bits
}

func parseValue(s string) (float64, error) {
	if s == staleToken {
		return math.Float64frombits(value.StaleNaN), nil
	}
	return strconv.ParseFloat(s, 64)
}

type sample struct {
	t int64
	f float64
}

func (s sample) T() int64                      { return s.t }
func (s sample) ST() int64                     { return 0 } // start timestamps are not carried by a case
func (s sample) F() float64                    { return s.f }
func (s sample) H() *histogram.Histogram       { return nil }
func (s sample) FH() *histogram.FloatHistogram { return nil }
func (s sample) Type() chunkenc.ValueType      { return chunkenc.ValFloat }
func (s sample) Copy() chunks.Sample           { return s }

type series struct {
	lset    labels.Labels
	samples []chunks.Sample // ascending by time
}

// Store holds every series of a case in memory. A case's metrics are an incident window, not a
// database: the one this was built against is fifteen series and two thousand samples.
type Store struct {
	series []series
}

// record is one line of metrics.jsonl: a series, its timestamps in milliseconds, and its values as
// strings so that every float survives bit for bit.
type record struct {
	Labels map[string]string `json:"labels"`
	T      []int64           `json:"t"`
	V      []string          `json:"v"`
}

// Add puts a series into the store. Timestamps are milliseconds and must ascend.
func (s *Store) Add(lset map[string]string, ts []int64, vs []float64) error {
	if len(ts) != len(vs) {
		return fmt.Errorf("series %v: %d timestamps for %d values", lset, len(ts), len(vs))
	}
	out := make([]chunks.Sample, len(ts))
	for i := range ts {
		if i > 0 && ts[i] <= ts[i-1] {
			return fmt.Errorf("series %v: timestamps do not ascend at index %d", lset, i)
		}
		out[i] = sample{ts[i], vs[i]}
	}
	// Kept sorted by label set: it is the order the engine wants and the order the file is written in.
	ls := labels.FromMap(lset)
	at := sort.Search(len(s.series), func(i int) bool { return labels.Compare(s.series[i].lset, ls) >= 0 })
	if at < len(s.series) && labels.Equal(s.series[at].lset, ls) {
		return fmt.Errorf("series %v appears twice", lset)
	}
	s.series = append(s.series, series{})
	copy(s.series[at+1:], s.series[at:])
	s.series[at] = series{ls, out}
	return nil
}

// Bounds returns the oldest and newest sample time in milliseconds, and false for an empty store.
func (s *Store) Bounds() (oldest, newest int64, ok bool) {
	for _, se := range s.series {
		if len(se.samples) == 0 {
			continue
		}
		first, last := se.samples[0].T(), se.samples[len(se.samples)-1].T()
		if !ok || first < oldest {
			oldest = first
		}
		if !ok || last > newest {
			newest = last
		}
		ok = true
	}
	return oldest, newest, ok
}

// Size reports how many series and samples the store holds.
func (s *Store) Size() (nSeries, nSamples int) {
	for _, se := range s.series {
		nSamples += len(se.samples)
	}
	return len(s.series), nSamples
}

// MaxBytes is how much a metrics file may decompress to. The store is held in memory, and a case is
// something one downloads from a stranger.
var MaxBytes = int64(2) << 30

// Read parses the gzip-compressed JSON-lines form.
func Read(r io.Reader) (*Store, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	s := &Store{}
	limited := &io.LimitedReader{R: zr, N: MaxBytes + 1}
	sc := bufio.NewScanner(limited)
	sc.Buffer(make([]byte, 1<<20), 1<<28) // one series is one line: a quarter of a gigabyte of it is not a series
	for n := 1; sc.Scan(); n++ {
		if limited.N <= 0 { // whatever this line is, it is where the file was cut off
			break
		}
		var rec record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		if len(rec.T) != len(rec.V) {
			return nil, fmt.Errorf("line %d: %d timestamps for %d values", n, len(rec.T), len(rec.V))
		}
		vs := make([]float64, len(rec.V))
		for i, v := range rec.V {
			if vs[i], err = parseValue(v); err != nil {
				return nil, fmt.Errorf("line %d: value %q: %w", n, v, err)
			}
		}
		if err := s.Add(rec.Labels, rec.T, vs); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
	}
	if limited.N <= 0 {
		return nil, fmt.Errorf("the metrics decompress to more than %d bytes", MaxBytes)
	}
	return s, sc.Err()
}

// Load reads a store from a file.
func Load(path string) (*Store, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Read(f)
}

// Write emits the store in its file form. Series are sorted and the gzip header carries no timestamp,
// so one build writes the same samples to the same bytes every time. Across Go releases only the
// uncompressed bytes are stable: the compressor itself has changed between them.
func (s *Store) Write(w io.Writer) error {
	zw, _ := gzip.NewWriterLevel(w, gzip.BestCompression)
	enc := json.NewEncoder(zw)
	for _, se := range s.series {
		rec := record{Labels: se.lset.Map(), T: make([]int64, len(se.samples)), V: make([]string, len(se.samples))}
		for i, sm := range se.samples {
			rec.T[i], rec.V[i] = sm.T(), formatValue(sm.F())
		}
		if err := enc.Encode(rec); err != nil {
			return err
		}
	}
	return zw.Close()
}

// Save writes the store to a file.
func (s *Store) Save(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := s.Write(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Querier makes the store a storage.Queryable, which is all the PromQL engine asks for.
func (s *Store) Querier(mint, maxt int64) (storage.Querier, error) {
	return &querier{s, mint, maxt}, nil
}

type querier struct {
	s          *Store
	mint, maxt int64
}

func matches(lset labels.Labels, ms []*labels.Matcher) bool {
	for _, m := range ms {
		if !m.Matches(lset.Get(m.Name)) {
			return false
		}
	}
	return true
}

func (q *querier) Select(_ context.Context, _ bool, _ *storage.SelectHints, ms ...*labels.Matcher) storage.SeriesSet {
	var out []storage.Series
	for _, se := range q.s.series { // already sorted by label set
		if !matches(se.lset, ms) {
			continue
		}
		lo := sort.Search(len(se.samples), func(i int) bool { return se.samples[i].T() >= q.mint })
		hi := sort.Search(len(se.samples), func(i int) bool { return se.samples[i].T() > q.maxt })
		out = append(out, storage.NewListSeries(se.lset, se.samples[lo:hi]))
	}
	return &seriesSet{list: out, i: -1}
}

func (q *querier) LabelValues(_ context.Context, name string, _ *storage.LabelHints, ms ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	seen := map[string]struct{}{}
	for _, se := range q.s.series {
		if v := se.lset.Get(name); v != "" && matches(se.lset, ms) {
			seen[v] = struct{}{}
		}
	}
	return sortedKeys(seen), nil, nil
}

func (q *querier) LabelNames(_ context.Context, _ *storage.LabelHints, ms ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	seen := map[string]struct{}{}
	for _, se := range q.s.series {
		if matches(se.lset, ms) {
			se.lset.Range(func(l labels.Label) { seen[l.Name] = struct{}{} })
		}
	}
	return sortedKeys(seen), nil, nil
}

func (q *querier) Close() error { return nil }

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type seriesSet struct {
	list []storage.Series
	i    int
}

func (s *seriesSet) Next() bool                        { s.i++; return s.i < len(s.list) }
func (s *seriesSet) At() storage.Series                { return s.list[s.i] }
func (s *seriesSet) Err() error                        { return nil }
func (s *seriesSet) Warnings() annotations.Annotations { return nil }
