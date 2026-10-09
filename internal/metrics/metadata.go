package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
)

// Metadata is what a Prometheus knows of a metric family that is not a sample: what kind of metric it
// is, what its target says it measures, and in what unit. It is what /api/v1/metadata answers with,
// and what an agent asks when it wants to know whether a number is a counter before it takes a rate.
type Metadata struct {
	Type string `json:"type"`
	Help string `json:"help"`
	Unit string `json:"unit"`
}

// readMetadata asks a Prometheus what it knows of every metric family. A family has one entry for
// each thing its targets say of it, and they need not agree. The entries come in the order of a map,
// another each time, and are put in one order here.
//
// Nothing is returned if it cannot be asked: a store that speaks remote read need not have this, and
// a case is frozen without it, as it is without the Prometheus's version.
func readMetadata(ctx context.Context, client *http.Client, base string) map[string][]Metadata {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/metadata", nil)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var body struct {
		Status string                `json:"status"`
		Data   map[string][]Metadata `json:"data"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&body) != nil || body.Status != "success" || body.Data == nil {
		return nil
	}
	for _, entries := range body.Data {
		sortMetadata(entries)
	}
	return body.Data
}

func sortMetadata(entries []Metadata) {
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		if a.Help != b.Help {
			return a.Help < b.Help
		}
		return a.Unit < b.Unit
	})
}

// LoadMetadata reads the metadata file of a case.
func LoadMetadata(path string) (map[string][]Metadata, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string][]Metadata{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	for _, entries := range out {
		sortMetadata(entries)
	}
	return out, nil
}

// SaveMetadata writes it: the families by name and a family's entries in one order, so that the same
// metadata is the same bytes.
func SaveMetadata(path string, metadata map[string][]Metadata) error {
	ordered := make(map[string][]Metadata, len(metadata))
	for family, entries := range metadata {
		ordered[family] = append([]Metadata(nil), entries...)
		sortMetadata(ordered[family])
	}
	raw, err := json.MarshalIndent(ordered, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// metadata answers /api/v1/metadata from what the case carries, as a Prometheus answers it from what
// its targets last said: every family, or the one named by `metric`; no more than `limit` families,
// and no more than `limit_per_metric` entries of each.
//
// Which families a Prometheus keeps under a limit, and which entries of a family, is whichever its
// maps hand it first. A case keeps the first by name and the first in the order above. And a case
// that carries none — one frozen before `freeze` asked, or from a store that could not be asked —
// answers that it knows of none.
func (a *API) metadata(w http.ResponseWriter, r *http.Request) {
	limit, perMetric := -1, -1
	var err error
	if s := r.FormValue("limit"); s != "" {
		if limit, err = strconv.Atoi(s); err != nil {
			fail(w, http.StatusBadRequest, "bad_data", errors.New("limit must be a number"))
			return
		}
	}
	if s := r.FormValue("limit_per_metric"); s != "" {
		if perMetric, err = strconv.Atoi(s); err != nil {
			fail(w, http.StatusBadRequest, "bad_data", errors.New("limit_per_metric must be a number"))
			return
		}
	}
	names := make([]string, 0, len(a.store.Metadata))
	for name := range a.store.Metadata {
		if metric := r.FormValue("metric"); metric == "" || metric == name {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := map[string][]Metadata{}
	for _, name := range names {
		if limit >= 0 && len(out) >= limit {
			break
		}
		entries := a.store.Metadata[name]
		if perMetric > 0 && len(entries) > perMetric {
			entries = entries[:perMetric]
		}
		out[name] = entries
	}
	ok(w, out)
}
