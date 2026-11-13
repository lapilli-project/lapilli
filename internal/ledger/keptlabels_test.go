package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
)

// A replayed alert's labels as kept: as they are within the bounds; a
// name or a value past them cut, with its hash; a set past 4 KB with its
// longest labels folded into one hash, the short ones kept.
func TestKeptLabelsBounds(t *testing.T) {
	for _, c := range []struct {
		name string
		in   map[string]string
	}{
		{"within", map[string]string{"service": "checkout", "x": strings.Repeat("v", 256)}},
		{"a long value", map[string]string{"service": "checkout", "x": strings.Repeat("v", 257)}},
		{"a long name", map[string]string{strings.Repeat("n", 129): "v"}},
		{"a turn", map[string]string{"service": "checkout", "x": "disk \u202efull"}},
		{"many", func() map[string]string {
			m := map[string]string{"service": "checkout"}
			for i := range 40 {
				m[fmt.Sprintf("k%02d", i)] = strings.Repeat("v", 200)
			}
			return m
		}()},
	} {
		if got := keptLabels(c.in); got == nil {
			t.Fatalf("%s: none", c.name)
		}
		got := keptLabels(c.in)
		b, _ := json.Marshal(got)
		if len(b) > 4<<10 || got["service"] != c.in["service"] {
			t.Errorf("%s: %d bytes, service %q", c.name, len(b), got["service"])
		}
		if c.name == "within" && !reflect.DeepEqual(got, c.in) {
			t.Errorf("within: changed: %v", got)
		}
		for k, v := range got {
			if len(k) > 128 || len(v) > 256 && k != "__lapilli_cut__" || strings.ContainsRune(v, 0x202e) {
				t.Errorf("%s: %d, %d bytes, %q", c.name, len(k), len(v), v)
			}
		}
		// Another value cut is another alert.
		other := maps.Clone(c.in)
		for k := range other {
			if k != "service" {
				other[k] += "!"
			}
		}
		if reflect.DeepEqual(keptLabels(other), got) {
			t.Errorf("%s: two alerts kept as one", c.name)
		}
	}
}

// The cache of ids and labels holds maxCached bytes at most: what isn't
// held is read again from the ledger, the same.
func TestCacheBounded(t *testing.T) {
	was := maxCached
	t.Cleanup(func() { maxCached = was })
	maxCached = 32 << 10
	l := open(t, time.UTC)
	var fs []firing.Firing
	for i := range 400 {
		fs = append(fs, firing.Firing{Rule: "Errors", Labels: map[string]string{"service": "checkout", "pod": fmt.Sprintf("%03d-%s", i, strings.Repeat("p", 200))},
			Start: t0.Add(time.Duration(i) * time.Minute), End: t0.Add(time.Duration(i)*time.Minute + 30*time.Second)})
	}
	record(t, l, t0, t0.Add(7*time.Hour), fs)
	before, err := l.ReplayedFirings(context.Background(), t0, t0.Add(7*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	record(t, l, t0, t0.Add(7*time.Hour), fs)
	after, err := l.ReplayedFirings(context.Background(), t0, t0.Add(7*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	l.ids.RLock()
	held, n, labels := l.ids.rulesBytes+l.ids.seriesBytes+l.ids.labelsBytes, len(l.ids.series), len(l.ids.labels)
	l.ids.RUnlock()
	// Each series' labels are some 600 bytes as cached: 32 KB holds 50.
	if held > maxCached || n >= 400 || labels > maxCached/500 {
		t.Errorf("the cache holds %d bytes, %d series, %d label sets, past %d bytes", held, n, labels, maxCached)
	}
	var count int
	l.r.QueryRow("SELECT count(*) FROM series").Scan(&count)
	if count != 400 || len(after) != 400 || !reflect.DeepEqual(before, after) {
		t.Errorf("%d series kept, want 400; the period read again the same: %v", count, reflect.DeepEqual(before, after))
	}
}
