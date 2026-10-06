# The reference the frozen metrics store is compared against

`internal/metrics` evaluates PromQL over a case's samples without Prometheus's storage layer. Two
claims rest on that being a faithful substitute, and both can be re-checked from this directory:

1. `cases/s2-periodic-saturation/metrics.jsonl.gz` holds exactly the samples of the TSDB block it was
   frozen from — staleness markers included.
2. Six queries return, over those samples, the digits Prometheus's own TSDB reader and engine return
   over the block, at the instant a replay of the case evaluates them
   (`TestTheSealedCaseAnswersAsItsPrometheusDid`).

| file | what |
|---|---|
| `s2-tsdb-block.tar.gz` | the block the prototype sealed on 2026-10-06, packed again without owner names or times; the four files inside are unchanged (SHA-256 of the archive `9bb72f03d9a0599373c39e76c7ea8f67df7d9b757ad50b99f28ad6e73361ad42`) |
| `tsdbq/main.go` | one instant query over a TSDB directory, through `tsdb.OpenDBReadOnly` and `promql.Engine` |
| `dump/main.go` | every sample of a TSDB directory, in the case metrics format, uncompressed |

The two programs are **not part of the `lapilli` module** and are not built by CI: they import the
top-level `tsdb` package, which compiles the AWS, Azure and Google SDKs, and keeping those out of the
graph is why the store exists. Go ignores a `testdata` directory, and no `go.mod` or `go.sum` is
committed here. To run them, from the repository root:

```
work=$(mktemp -d) && mkdir -p "$work/tsdb/wal" "$work/mod"
tar -xzf internal/metrics/testdata/reference/s2-tsdb-block.tar.gz -C "$work/tsdb"   # the reader wants a wal/ beside the block, even an empty one
cp -R internal/metrics/testdata/reference/tsdbq internal/metrics/testdata/reference/dump "$work/mod/"
(cd "$work/mod" && go mod init reference && go get github.com/prometheus/prometheus@v0.305.0 && go mod tidy \
   && go build -o "$work/tsdbq" ./tsdbq && go build -o "$work/dump" ./dump)

# 1. the same samples
gunzip -c cases/s2-periodic-saturation/metrics.jsonl.gz | cmp - <("$work/dump" "$work/tsdb") && echo identical

# 2. the same answers; the time is the case's freeze_time, 1791228354.43095, rounded to the millisecond
"$work/tsdbq" "$work/tsdb" 'sum by (client) (increase(thumb_requests_total[10m]))' 1791228354431
```

v0.305.0 is the module version of Prometheus 3.5.0, the server the block was written by. Run on
2026-10-07 with Go 1.25.1: `identical`, 15 series, 3 staleness markers, and

```
{client="catalog-indexer"} => 5902.174954327808 @[1791228354431]
{client="email-renderer"} => 171.4530178466593 @[1791228354431]
{client="image-proxy"} => 434.6331407640582 @[1791228354431]
{client="web-frontend"} => 729.0744049169665 @[1791228354431]
```

The instant matters. The test first pinned the digits one millisecond earlier, at …430, which is what
truncating `freeze_time` gives and not what a replay uses; seven of the ten pinned values differ
between the two instants, the furthest in its sixth significant digit (7683.171… against 7683.182…).
An audit of the documents against the code found it.

The engine `internal/metrics` links is a later one (see `go.mod`); that it still returns these digits
is what the test asserts.
