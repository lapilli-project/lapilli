package metrics

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/prompb"
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
