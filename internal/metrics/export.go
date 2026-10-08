package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/golang/snappy"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/prompb"
	"gopkg.in/yaml.v3"
)

// AllSeries is the selector that freezes everything a Prometheus holds.
const AllSeries = `{__name__=~".+"}`

// Export copies an incident window out of a live Prometheus: the raw samples of every series a selector
// matches, for `window` before `at`.
//
// It uses the remote-read endpoint rather than the query API, and the reason is staleness. A range
// selector in PromQL drops staleness markers by definition, so an export through /api/v1/query would
// make a series that had just disappeared look alive for five more minutes. Remote read hands back what
// is stored, markers included, and needs neither the admin API nor a shell inside the pod.
func Export(ctx context.Context, client *http.Client, baseURL string, at time.Time, window time.Duration, selectors ...string) (*Store, error) {
	if client == nil {
		client = http.DefaultClient
	}
	store, err := read(ctx, client, baseURL, at, window, selectors...)
	if err != nil {
		return nil, err
	}
	store.Source = describe(ctx, client, strings.TrimRight(baseURL, "/"))
	return store, nil
}

// read is the remote read itself: what the selectors match, for `window` before `at`.
func read(ctx context.Context, client *http.Client, baseURL string, at time.Time, window time.Duration, selectors ...string) (*Store, error) {
	if len(selectors) == 0 {
		selectors = []string{AllSeries}
	}
	req := &prompb.ReadRequest{}
	for _, sel := range selectors {
		ms, err := selectorParser.ParseMetricSelector(sel)
		if err != nil {
			return nil, fmt.Errorf("selector %q: %w", sel, err)
		}
		q := &prompb.Query{StartTimestampMs: at.Add(-window).UnixMilli(), EndTimestampMs: at.UnixMilli()}
		for _, m := range ms {
			q.Matchers = append(q.Matchers, &prompb.LabelMatcher{Type: matcherType(m.Type), Name: m.Name, Value: m.Value})
		}
		req.Queries = append(req.Queries, q)
	}
	raw, err := req.Marshal()
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/api/v1/read", bytes.NewReader(snappy.Encode(nil, raw)))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/x-protobuf")
	hreq.Header.Set("Content-Encoding", "snappy")
	hreq.Header.Set("X-Prometheus-Remote-Read-Version", "0.1.0")
	resp, err := client.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("remote read: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if body, err = snappy.Decode(nil, body); err != nil {
		return nil, fmt.Errorf("remote read: %w", err)
	}
	var out prompb.ReadResponse
	if err := out.Unmarshal(body); err != nil {
		return nil, fmt.Errorf("remote read: %w", err)
	}

	store := &Store{}
	seen := map[string]bool{} // two selectors may match the same series
	for _, res := range out.Results {
		for _, ts := range res.Timeseries {
			if len(ts.Histograms) > 0 {
				return nil, fmt.Errorf("a series carries native histograms, which a case cannot hold yet: %v", ts.Labels)
			}
			lset := make(map[string]string, len(ts.Labels))
			for _, l := range ts.Labels {
				lset[l.Name] = l.Value
			}
			id := labels.FromMap(lset).String()
			if seen[id] || len(ts.Samples) == 0 {
				continue
			}
			seen[id] = true
			tms, vs := make([]int64, len(ts.Samples)), make([]float64, len(ts.Samples))
			for i, s := range ts.Samples {
				tms[i], vs[i] = s.Timestamp, s.Value
			}
			if err := store.Add(lset, tms, vs); err != nil {
				return nil, err
			}
		}
	}
	return store, nil
}

// Probe says whether an export from this Prometheus can be expected to work, without making one: the
// selectors parse, and its remote-read endpoint answers a read of nothing. A freeze reads the metrics
// last (freeze.go), and a wrong address or a store that cannot be read should not be learned of only
// after a cluster has been collected.
func Probe(ctx context.Context, client *http.Client, baseURL string, at time.Time, selectors ...string) error {
	if client == nil {
		client = http.DefaultClient
	}
	_, err := read(ctx, client, baseURL, at, 0, selectors...)
	return err
}

// describeTimeout is how long a Prometheus may take to say what it is. It is asked after its samples
// were read, and nothing waits on the answer but the freeze.
const describeTimeout = 15 * time.Second

// describe asks a Prometheus the things about itself that a replay wants. A store that speaks the
// query and read APIs and not these — there are several — answers neither, and that is not an
// error: the replay then goes by Prometheus's defaults. Of the configuration one line is read and
// nothing is kept.
func describe(ctx context.Context, client *http.Client, base string) Source {
	ctx, cancel := context.WithTimeout(ctx, describeTimeout)
	defer cancel()
	get := func(path string, into any) bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(into) == nil
	}
	var src Source
	var build struct {
		Data struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	if get("/api/v1/status/buildinfo", &build) {
		src.Version = build.Data.Version
	}
	var config struct {
		Data struct {
			YAML string `json:"yaml"`
		} `json:"data"`
	}
	if get("/api/v1/status/config", &config) {
		var loaded struct {
			Global struct {
				EvaluationInterval string `yaml:"evaluation_interval"`
			} `yaml:"global"`
		}
		if yaml.Unmarshal([]byte(config.Data.YAML), &loaded) == nil {
			if d, err := model.ParseDuration(loaded.Global.EvaluationInterval); err == nil {
				src.EvaluationInterval = time.Duration(d)
			}
		}
	}
	var flags struct {
		Data map[string]string `json:"data"`
	}
	if get("/api/v1/status/flags", &flags) {
		if d, err := model.ParseDuration(flags.Data["query.lookback-delta"]); err == nil {
			src.LookbackDelta = time.Duration(d)
		}
	}
	return src
}

func matcherType(t labels.MatchType) prompb.LabelMatcher_Type {
	switch t {
	case labels.MatchNotEqual:
		return prompb.LabelMatcher_NEQ
	case labels.MatchRegexp:
		return prompb.LabelMatcher_RE
	case labels.MatchNotRegexp:
		return prompb.LabelMatcher_NRE
	}
	return prompb.LabelMatcher_EQ
}
