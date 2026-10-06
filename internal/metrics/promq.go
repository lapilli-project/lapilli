package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/model"
)

// PromqUsage is what `promq` prints when it is given nothing to ask.
const PromqUsage = "usage: promq '<PromQL>' [--range 30m] [--step 15s]"

// maxSeries bounds what one query prints: an agent that asks for everything should get an answer it can read.
const maxSeries = 60

// Promq is the metrics helper an agent is given: one PromQL query against a Prometheus-compatible
// endpoint, printed one series per line. With --range it asks for the window ending now, and prints
// each point with the time the endpoint stamped it — which, from a frozen store, is the incident's own
// time, the one in the pod logs beside it.
func Promq(w io.Writer, client *http.Client, baseURL string, args []string, now time.Time) error {
	if len(args) == 0 {
		return errors.New(PromqUsage)
	}
	opt := map[string]string{}
	for i := 1; i+1 < len(args); i += 2 {
		opt[args[i]] = args[i+1]
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	base := strings.TrimRight(baseURL, "/")
	path, params := "/api/v1/query", url.Values{"query": {args[0]}}
	if r, ok := opt["--range"]; ok {
		window, err := model.ParseDuration(r)
		if err != nil {
			return fmt.Errorf("--range %q: %w", r, err)
		}
		step := opt["--step"]
		if step == "" {
			step = "15s"
		}
		stamp := func(t time.Time) string { return strconv.FormatFloat(float64(t.UnixMilli())/1000, 'f', 3, 64) }
		path = "/api/v1/query_range"
		params.Set("start", stamp(now.Add(-time.Duration(window))))
		params.Set("end", stamp(now))
		params.Set("step", step)
	}
	resp, err := client.Get(base + path + "?" + params.Encode())
	if err != nil {
		return fmt.Errorf("query failed: %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("query failed: %s: %w", resp.Status, err)
	}
	if body.Status != "success" {
		return fmt.Errorf("query failed: %s", body.Error)
	}
	return render(w, body.Data.ResultType, body.Data.Result, maxSeries)
}

// number prints a value to ten significant digits, in plain notation wherever that is readable. Six
// digits with an exponent, which this used to print, turns two counters of 1234567 and 1234580 into the
// same "1.23457e+06", and their difference is what an agent was asked to quantify.
func number(v any) string {
	f, err := strconv.ParseFloat(fmt.Sprint(v), 64)
	if err != nil {
		return fmt.Sprint(v)
	}
	rounded, _ := strconv.ParseFloat(strconv.FormatFloat(f, 'g', 10, 64), 64)
	if abs := math.Abs(rounded); abs != 0 && (abs < 1e-6 || abs >= 1e15) || math.IsInf(rounded, 0) || math.IsNaN(rounded) {
		return strconv.FormatFloat(rounded, 'g', -1, 64)
	}
	return strconv.FormatFloat(rounded, 'f', -1, 64)
}

// render prints a query result the way promq does: one series per line, labels sorted. limit bounds
// the series printed; zero prints all of them.
func render(w io.Writer, resultType string, raw json.RawMessage, limit int) error {
	clock := func(v any) string {
		f, _ := strconv.ParseFloat(fmt.Sprint(v), 64)
		return FromSeconds(f).UTC().Format("15:04:05")
	}
	switch resultType {
	case "scalar", "string":
		var point [2]any
		if err := json.Unmarshal(raw, &point); err != nil {
			return err
		}
		fmt.Fprintln(w, number(point[1]))
		return nil
	}
	var result []struct {
		Metric map[string]string `json:"metric"`
		Value  *[2]any           `json:"value"`
		Values [][2]any          `json:"values"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	if len(result) == 0 {
		fmt.Fprintln(w, "(empty result)")
		return nil
	}
	for i, s := range result {
		if limit > 0 && i == limit {
			fmt.Fprintf(w, "(%d more series not shown; narrow the query)\n", len(result)-limit)
			break
		}
		names := make([]string, 0, len(s.Metric))
		for k := range s.Metric {
			if k != model.MetricNameLabel {
				names = append(names, k)
			}
		}
		sort.Strings(names)
		pairs := make([]string, len(names))
		for j, k := range names {
			pairs[j] = k + "=" + s.Metric[k]
		}
		fmt.Fprintf(w, "%s{%s}", s.Metric[model.MetricNameLabel], strings.Join(pairs, ","))
		if s.Value != nil {
			fmt.Fprintf(w, " %s", number(s.Value[1]))
		}
		for _, p := range s.Values {
			fmt.Fprintf(w, " %s=%s", clock(p[0]), number(p[1]))
		}
		fmt.Fprintln(w)
	}
	return nil
}

// Text evaluates an instant query at the freeze and returns what promq would print for it, every
// series included. It is how a case is checked for evidence that lives in its metrics: the pattern in
// the answer key is written against this text.
func (a *API) Text(ctx context.Context, query string) (string, error) {
	q, err := a.engine.NewInstantQuery(ctx, a.store, nil, query, a.freeze)
	if err != nil {
		return "", err
	}
	defer q.Close()
	res := q.Exec(ctx)
	if res.Err != nil {
		return "", res.Err
	}
	raw, err := json.Marshal(encode(res.Value))
	if err != nil {
		return "", err
	}
	var out strings.Builder
	err = render(&out, string(res.Value.Type()), raw, 0)
	return out.String(), err
}
