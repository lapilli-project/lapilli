// dump writes every sample of a TSDB directory in the case metrics format (docs/case-format.md),
// uncompressed, read through Prometheus's own storage layer. Its output is what a case's
// metrics.jsonl.gz must decompress to if the export lost nothing. Like tsdbq it is not part of the
// lapilli module (see ../README.md).
//
//	dump <tsdb dir> > metrics.jsonl
package main

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"strconv"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
)

type record struct {
	Labels map[string]string `json:"labels"`
	T      []int64           `json:"t"`
	V      []string          `json:"v"`
}

func main() {
	db, err := tsdb.OpenDBReadOnly(os.Args[1], "", nil)
	if err != nil {
		panic(err)
	}
	defer db.Close()
	q, err := db.Querier(math.MinInt64, math.MaxInt64)
	if err != nil {
		panic(err)
	}
	defer q.Close() // before the database: closing it with a querier open blocks forever
	enc := json.NewEncoder(os.Stdout)
	set := q.Select(context.Background(), true, nil, labels.MustNewMatcher(labels.MatchRegexp, "__name__", ".+"))
	for set.Next() {
		series := set.At()
		rec := record{Labels: series.Labels().Map()}
		it := series.Iterator(nil)
		for it.Next() == chunkenc.ValFloat {
			t, v := it.At()
			text := strconv.FormatFloat(v, 'g', -1, 64)
			if value.IsStaleNaN(v) {
				text = "stale"
			}
			rec.T, rec.V = append(rec.T, t), append(rec.V, text)
		}
		if err := enc.Encode(rec); err != nil {
			panic(err)
		}
	}
	if err := set.Err(); err != nil {
		panic(err)
	}
}
