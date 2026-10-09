package metrics

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"

	"github.com/prometheus/prometheus/model/labels"
)

// A Prometheus keeps what it scraped lately in its head and what is older in blocks, and the order it
// hands a query its series in depends on which of the two the query reaches. From the head alone they
// come as the head made them, which is the order its targets first exposed them in. Where a block is
// reached as well — or a block alone — they come merged, and so by label.
//
// The order is the engine's to go by: which of several equal series `topk` keeps, the order of an
// aggregation's groups, the order of an answer that is not sorted. So a case frozen from a Prometheus
// that has blocks keeps two things. Its series are in the head's order, the ones the head did not hold
// after them. And it knows where the blocks end (Store.HeadFrom): a query that looks no further back
// is handed the series as they are kept, and one that does is handed them by label (byLabel).
//
// The time a query reaches back to is the whole query's, the earliest any of its selectors looks at —
// that is what an engine asks its store for, once — and not each selector's own.

// blocksEnd asks a Prometheus where its blocks end: the instant before which a query reaches one. It
// is zero if there is no block, so that no query reaches any; and not known if it cannot be asked,
// which a store that is no Prometheus need not answer.
//
// A Prometheus that lists its blocks says it outright: the latest end among them, which is the very
// number it holds a query's earliest instant against. One that does not — the list is newer than
// 3.5 — says its head's least time: a Prometheus started over blocks sets that to their end, and
// from the next time it cuts its head it is the oldest sample the head has left, some seconds
// later; started over none, it is the head's first sample. Which of those it is, it does not say,
// so it is asked whether it knows of any label from before then.
func blocksEnd(ctx context.Context, client *http.Client, base string) (end int64, known bool) {
	ctx, cancel := context.WithTimeout(ctx, describeTimeout)
	defer cancel()
	var listed struct {
		Data struct {
			Blocks *[]struct {
				MaxTime *int64 `json:"maxTime"`
			} `json:"blocks"`
		} `json:"data"`
	}
	if getJSON(ctx, client, base+"/api/v1/status/tsdb/blocks", &listed) && listed.Data.Blocks != nil {
		said := true
		for _, block := range *listed.Data.Blocks {
			if block.MaxTime == nil || *block.MaxTime <= 0 { // a block that does not say where it ends: the list is not one to go by
				said = false
				break
			}
			end = max(end, *block.MaxTime)
		}
		if said {
			return end, true
		}
	}
	var status struct {
		Data struct {
			HeadStats struct {
				MinTime *int64 `json:"minTime"`
			} `json:"headStats"`
		} `json:"data"`
	}
	if !getJSON(ctx, client, base+"/api/v1/status/tsdb", &status) || status.Data.HeadStats.MinTime == nil {
		return 0, false
	}
	head := *status.Data.HeadStats.MinTime // of a head that has been cut, the oldest sample it has left: later than where the blocks end, and a query that reaches between the two is handed its series the other way
	var older struct {
		Data *[]string `json:"data"`
	}
	before := url.Values{"end": {strconv.FormatFloat(float64(head-1)/1000, 'f', 3, 64)}, "limit": {"1"}}
	if !getJSON(ctx, client, base+"/api/v1/labels?"+before.Encode(), &older) || older.Data == nil {
		return 0, false
	}
	if len(*older.Data) == 0 {
		return 0, true
	}
	return head, true
}

// headOrder asks for the series a selector matches in the head alone, in the order the head has them:
// what the series endpoint lists for one selector over a stretch of time no block reaches into.
func headOrder(ctx context.Context, client *http.Client, base string, from, at int64, selector string) ([]labels.Labels, bool) {
	secs := func(ms int64) string { return strconv.FormatFloat(float64(ms)/1000, 'f', 3, 64) }
	var listed struct {
		Data *[]map[string]string `json:"data"`
	}
	if !getJSON(ctx, client, base+"/api/v1/series?"+url.Values{"match[]": {selector}, "start": {secs(from)}, "end": {secs(at)}}.Encode(), &listed) || listed.Data == nil {
		return nil, false
	}
	out := make([]labels.Labels, len(*listed.Data))
	for i, lset := range *listed.Data {
		out[i] = labels.FromMap(lset)
	}
	return out, true
}

// getJSON reads the data of an answer of the API into a struct that names it: an answer that is not
// 200, that does not say success, that has no data or is not JSON is no answer.
func getJSON(ctx context.Context, client *http.Client, address string, into any) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var body struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<30)).Decode(&body) != nil || body.Status != "success" {
		return false
	}
	return json.Unmarshal([]byte(`{"data":`+string(body.Data)+`}`), into) == nil
}

// keepHeadOrder puts the store's series in the order its Prometheus's head has them, and notes where
// that Prometheus's blocks end, if both can be learned — of a store read with one selector: with
// several, neither is.
//
// If there is no block, what was read came from the head alone and is in its order already. If there
// are blocks, the head is listed apart, over the time from where the blocks end to the freeze. The
// listing is believed only if it accounts for what was read: every
// series read with a sample since the blocks' end has to be in it. One that is cut short, or is of
// another store than the samples came from, would otherwise put the series in an order that is
// nobody's and call it the head's; the case is then left as it was read, and says of its order
// nothing (OrderKnown).
func (s *Store) keepHeadOrder(ctx context.Context, client *http.Client, base string, at int64, selectors []string) {
	if len(selectors) != 1 {
		return // what was read came one selector after another, which is no order of the head's, and the head lists several by label
	}
	end, known := blocksEnd(ctx, client, base)
	if !known {
		return
	}
	if end == 0 { // no block: every query is answered from the head, and this was read from it
		s.OrderKnown = true
		return
	}
	var listed []labels.Labels
	if end <= at { // otherwise nothing of the head lies before the freeze, and there is no order of it to keep
		var ok bool
		if listed, ok = headOrder(ctx, client, base, end, at, selectors[0]); !ok {
			return
		}
	}
	place := make(map[string]int, len(listed))
	for i, lset := range listed {
		if _, twice := place[lset.String()]; !twice { // a listing that has a series twice: it is where it first is
			place[lset.String()] = i
		}
	}
	for _, se := range s.series {
		if _, there := place[se.lset.String()]; !there && len(se.samples) > 0 && se.samples[len(se.samples)-1].T() >= end {
			return
		}
	}
	// The series the head listed, in its order; and after them, by label, the ones it did not: series
	// that ended before the blocks did.
	sort.SliceStable(s.series, func(i, j int) bool {
		a, inHead := place[s.series[i].lset.String()]
		b, alsoInHead := place[s.series[j].lset.String()]
		switch {
		case inHead && alsoInHead:
			return a < b
		case inHead != alsoInHead:
			return inHead
		}
		return labels.Compare(s.series[i].lset, s.series[j].lset) < 0
	})
	s.HeadFrom, s.OrderKnown = end, true
}

// byLabel says whether a query that looks back as far as mint is handed its series by label: it is,
// if the Prometheus the case was frozen from would have had to reach a block for it.
func (s *Store) byLabel(mint int64) bool {
	return s.HeadFrom != 0 && mint < s.HeadFrom
}
