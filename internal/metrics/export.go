package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
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
// matches as a remote read takes it (read), for `window` before `at` — and as far again before that as an instant looks back for a
// sample, five minutes unless the Prometheus was set otherwise. Without that the window's own first
// minutes would be the poorer for it: an instant at its beginning finds its sample just before the
// beginning, and a `rate` over five minutes there needs the five minutes. With it, an instant anywhere
// in the window finds the sample the Prometheus found, and a range no longer than that at its very
// beginning is whole; a query that reaches further back finds where the case begins, and a replay
// says so (From).
//
// It uses the remote-read endpoint rather than the query API, and the reason is staleness. A range
// selector in PromQL drops staleness markers by definition, so an export through /api/v1/query would
// make a series that had just disappeared look alive for five more minutes. Remote read hands back what
// is stored, markers included, and needs neither the admin API nor a shell inside the pod.
func Export(ctx context.Context, client *http.Client, baseURL string, at time.Time, window time.Duration, selectors ...string) (*Store, error) {
	if client == nil {
		client = http.DefaultClient
	}
	base := strings.TrimRight(baseURL, "/")
	// What the Prometheus says of itself first: how far an instant looks back is how much is read, and
	// its external labels are on every series a remote read returns.
	source, external := describe(ctx, client, base)
	reach := window + defaultLookbackDelta
	if source.LookbackDelta > 0 {
		reach = window + source.LookbackDelta
	}
	store, err := read(ctx, client, baseURL, at, reach, ownSeries(ctx, client, base, external, at.Add(-reach), at), selectors...)
	if err != nil {
		return nil, err
	}
	store.Source, store.From, store.ExternalLabels = source, at.Add(-reach).UnixMilli(), external
	store.Metadata = describedMetrics(ctx, client, base)
	if len(selectors) == 0 {
		selectors = []string{AllSeries}
	}
	store.keepHeadOrder(ctx, client, base, at.UnixMilli(), selectors)
	return store, nil
}

// read is the remote read itself: what the selectors match, for `window` before `at`.
//
// A Prometheus adds its external labels to every series it returns over remote read, where the series
// has no label of that name; its own queries do not have them. They are taken off again here
// (externalLabels), so that a case holds the series as the Prometheus's own queries name them. Two
// series that differ by nothing but such a label, one having it of its own, are read alike, and what
// is read does not say which samples are whose: that is refused, and said.
//
// It reads a selector as well as a remote read does: a Prometheus takes a matcher for an external
// label with its very value to mean the series that have no such label of their own, which is every
// series as another store would see it, and not the ones its own queries would find by it.
func read(ctx context.Context, client *http.Client, baseURL string, at time.Time, window time.Duration, added *externalLabels, selectors ...string) (*Store, error) {
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
		alike := map[string]bool{} // what one selector's series were read as
		for _, ts := range res.Timeseries {
			if len(ts.Histograms) > 0 {
				return nil, fmt.Errorf("a series carries native histograms, which a case cannot hold yet: %v", ts.Labels)
			}
			lset := make(map[string]string, len(ts.Labels))
			for _, l := range ts.Labels {
				lset[l.Name] = l.Value
			}
			if as := labels.FromMap(lset).String(); alike[as] {
				return nil, fmt.Errorf("remote read: two series were read as %s. A Prometheus that adds labels to what it sends (external_labels) returns a series that has such a label of its own, and one that is the same but for it, alike; which samples are whose cannot be told", as)
			} else {
				alike[as] = true
			}
			lset = added.takenOff(lset)
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

// externalLabels are the labels a Prometheus adds to what it sends elsewhere, and what is needed to
// take them off a series again without taking off a label that was the series's own.
//
// What a remote read returns does not say which it was: `cluster="prod"` on a series may be the
// Prometheus's addition or the label a target exposed. The Prometheus's own listing of its series
// does say, since nothing is added there, and it names each series whole. So for each external label
// the series that have it of their own are listed (ownSeries), and a series that was read is the
// listed one it would be read as, if there is one: the same series once the external labels are off
// both. Otherwise it is what was read with every external label off, which is right of every series
// that has none of its own. Where a listing could not be had, a series whose only label of its own
// among them is that one is not known to have it, and loses it.
type externalLabels struct {
	of     map[string]string              // the labels, as the Prometheus's configuration has them
	listed map[string][]map[string]string // the series that have one of their own, by what each is with all of them off
}

// without is a series with every label off it that has an external label's name and value.
func (e *externalLabels) without(lset map[string]string) map[string]string {
	out := make(map[string]string, len(lset))
	for name, value := range lset {
		if added, is := e.of[name]; !is || added != value {
			out[name] = value
		}
	}
	return out
}

// ownSeries lists the series within the window that have an external label of their own, with its
// very value: what the series endpoint has for each label, asked one after another, each with the
// time a Prometheus has to say something of itself.
func ownSeries(ctx context.Context, client *http.Client, base string, external map[string]string, from, to time.Time) *externalLabels {
	e := &externalLabels{of: external, listed: map[string][]map[string]string{}}
	secs := func(at time.Time) string { return strconv.FormatFloat(float64(at.UnixMilli())/1000, 'f', 3, 64) }
	names := make([]string, 0, len(external))
	for name := range external {
		names = append(names, name)
	}
	sort.Strings(names)
	had := map[string]bool{} // a series with two of them of its own is in two listings
	for _, name := range names {
		var listed struct {
			Data *[]map[string]string `json:"data"`
		}
		selector := "{" + selectorName(name) + "=" + strconv.Quote(external[name]) + "}"
		one, cancel := context.WithTimeout(ctx, describeTimeout)
		answered := getJSON(one, client, base+"/api/v1/series?"+url.Values{"match[]": {selector}, "start": {secs(from)}, "end": {secs(to)}}.Encode(), &listed)
		cancel()
		if !answered || listed.Data == nil {
			continue
		}
		for _, lset := range *listed.Data {
			if id := labels.FromMap(lset).String(); !had[id] {
				had[id] = true
				bare := labels.FromMap(e.without(lset)).String()
				e.listed[bare] = append(e.listed[bare], lset)
			}
		}
	}
	return e
}

// selectorName is a label's name as a selector takes it: as it is, or in quotes if it is not made of
// the letters, digits and underscores a name was once held to.
func selectorName(name string) string {
	for i, c := range name {
		if !(c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || i > 0 && '0' <= c && c <= '9') {
			return strconv.Quote(name)
		}
	}
	if name == "" {
		return strconv.Quote(name)
	}
	return name
}

// takenOff is a series as the Prometheus itself names it, given the labels it was read with.
func (e *externalLabels) takenOff(read map[string]string) map[string]string {
	if e == nil || len(e.of) == 0 {
		return read
	}
	bare := e.without(read)
	if len(bare) == len(read) {
		return read
	}
	// The listed series it could be: one with no label it was not read with. Of several, the one with
	// the most labels, which takes the fewest off; and of those the first by name, so that it is the
	// same one every time.
	var its map[string]string
	for _, own := range e.listed[labels.FromMap(bare).String()] {
		within := true
		for name, value := range own {
			if got, has := read[name]; !has || got != value {
				within = false
				break
			}
		}
		if within && (its == nil || len(own) > len(its) || len(own) == len(its) && labels.FromMap(own).String() < labels.FromMap(its).String()) {
			its = own
		}
	}
	if its == nil {
		return bare
	}
	out := make(map[string]string, len(its))
	for name, value := range its {
		out[name] = value
	}
	return out
}

// Probe says whether an export from this Prometheus can be expected to work, without making one: the
// selectors parse, and its remote-read endpoint answers a read of nothing. A freeze reads the metrics
// last (freeze.go), and a wrong address or a store that cannot be read should not be learned of only
// after a cluster has been collected.
func Probe(ctx context.Context, client *http.Client, baseURL string, at time.Time, selectors ...string) error {
	if client == nil {
		client = http.DefaultClient
	}
	_, err := read(ctx, client, baseURL, at, 0, nil, selectors...)
	return err
}

// describeTimeout is how long a Prometheus may take to say what it is: each of the things it is asked
// about itself has this long, and the reading of its samples waits for the first of them.
var describeTimeout = 15 * time.Second

// describedMetrics is readMetadata, in the time describe has.
func describedMetrics(ctx context.Context, client *http.Client, base string) map[string][]Metadata {
	ctx, cancel := context.WithTimeout(ctx, describeTimeout)
	defer cancel()
	return readMetadata(ctx, client, base)
}

// describe asks a Prometheus the things about itself that an export and a replay want: its version,
// its evaluation interval, how far an instant looks back, and the labels it adds to what it sends
// elsewhere. A store that speaks the query and read APIs and not these — there are several —
// answers none, and that is not an error: the replay then goes by Prometheus's defaults. Of the
// configuration two things are read and nothing else is kept.
func describe(ctx context.Context, client *http.Client, base string) (Source, map[string]string) {
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
	var loaded struct {
		Global struct {
			EvaluationInterval string            `yaml:"evaluation_interval"`
			ExternalLabels     map[string]string `yaml:"external_labels"`
		} `yaml:"global"`
	}
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
	return src, loaded.Global.ExternalLabels
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
