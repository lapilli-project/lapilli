package metrics

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
// endpoint, printed one series per line. With --range it asks for the window ending now.
func Promq(w io.Writer, client *http.Client, baseURL string, args []string, now time.Time) error {
	if len(args) == 0 {
		return errors.New(PromqUsage)
	}
	opt := map[string]string{}
	for i := 1; i+1 < len(args); i += 2 {
		opt[args[i]] = args[i+1]
	}
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
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Get(strings.TrimRight(baseURL, "/") + path + "?" + params.Encode())
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

	number := func(v any) string {
		f, err := strconv.ParseFloat(fmt.Sprint(v), 64)
		if err != nil {
			return fmt.Sprint(v)
		}
		return strconv.FormatFloat(f, 'g', 6, 64)
	}
	clock := func(v any) string {
		f, _ := strconv.ParseFloat(fmt.Sprint(v), 64)
		return FromSeconds(f).UTC().Format("15:04:05")
	}
	switch body.Data.ResultType {
	case "scalar", "string":
		var point [2]any
		if err := json.Unmarshal(body.Data.Result, &point); err != nil {
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
	if err := json.Unmarshal(body.Data.Result, &result); err != nil {
		return err
	}
	if len(result) == 0 {
		fmt.Fprintln(w, "(empty result)")
		return nil
	}
	for i, s := range result {
		if i == maxSeries {
			fmt.Fprintf(w, "(%d more series not shown; narrow the query)\n", len(result)-maxSeries)
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
