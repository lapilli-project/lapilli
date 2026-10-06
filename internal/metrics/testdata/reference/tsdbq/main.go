// tsdbq evaluates one PromQL instant query over a TSDB directory with Prometheus's own storage
// layer and engine. It is the reference the frozen metrics store is compared against, and it is not
// part of the lapilli module: it imports the top-level tsdb package, which is exactly what the store
// exists to avoid (see ../README.md for how to build it).
//
//	tsdbq <tsdb dir> '<PromQL>' <evaluation time, Unix milliseconds>
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"
)

func main() {
	dir, query := os.Args[1], os.Args[2]
	at, err := strconv.ParseInt(os.Args[3], 10, 64)
	if err != nil {
		panic(err)
	}
	db, err := tsdb.OpenDBReadOnly(dir, "", nil)
	if err != nil {
		panic(err)
	}
	defer db.Close()
	queryable := storage.QueryableFunc(func(mint, maxt int64) (storage.Querier, error) { return db.Querier(mint, maxt) })
	engine := promql.NewEngine(promql.EngineOpts{MaxSamples: 50_000_000, Timeout: 30 * time.Second})
	q, err := engine.NewInstantQuery(context.Background(), queryable, nil, query, time.UnixMilli(at))
	if err != nil {
		panic(err)
	}
	res := q.Exec(context.Background())
	if res.Err != nil {
		panic(res.Err)
	}
	fmt.Println(res.Value.String())
}
