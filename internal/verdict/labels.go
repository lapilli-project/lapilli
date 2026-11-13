package verdict

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"lapilli.dev/internal/rules"
)

// The bounds of a replayed alert's labels as lapilli serve's ledger keeps
// them: those of the alerts it is sent (recorded firings, notifications,
// incidents), 4 KB in all, as JSON; a name of 128 bytes and a value of
// 256, as an incident's. What a replay finds is what the API answered,
// which can be hostile, and a label set is kept for good, and cached.
const (
	maxKeptBytes = 4 << 10
	maxKeptName  = 128
	maxKeptValue = 256
	// CutLabel is the label a set past maxKeptBytes has its longest
	// labels folded into, as their hash. One an API gives is folded too,
	// so that it can't stand for a fold.
	CutLabel = "__lapilli_cut__"
)

// KeptLabels are an alert's labels as a replayed alert's are kept: each
// name and value as KeptName and KeptValue give them, and a set still past
// 4 KB, as JSON, with its longest labels folded into one, CutLabel, the
// hash of them, the short ones an incident is matched by kept. As they are
// when they are within the bounds. Alerts apart stay apart, but for two
// values cut alike whose hashes' first 48 bits are the same (KeptValue),
// or a collision of SHA-256 (the fold).
func KeptLabels(m map[string]string) map[string]string {
	size, over := 2, false
	for k, v := range m {
		over = over || k == CutLabel || KeptName(k) != k || KeptValue(v) != v
		size += jsonLen(k) + jsonLen(v) + 2
	}
	if !over && size <= maxKeptBytes {
		return m
	}
	out := make(map[string]string, len(m))
	cost := make(map[string]int, len(m))
	size = 2
	folded := map[string]string{}
	// In order, so that a name kept as another label is named (a cut
	// name's, an API's own) is the same one each time: the later folded.
	for _, k := range slices.Sorted(maps.Keys(m)) {
		v := m[k]
		kk := KeptName(k)
		if _, taken := out[kk]; taken || k == CutLabel {
			folded[k] = v
			continue
		}
		v = KeptValue(v)
		out[kk] = v
		cost[kk] = jsonLen(kk) + jsonLen(v) + 2
		size += cost[kk]
	}
	if size <= maxKeptBytes && len(folded) == 0 {
		return out
	}
	// The longest folded first, so that the short ones an incident is
	// matched by stay; the fold's own label, a hash of 64 hex digits,
	// fits in what is left.
	room := maxKeptBytes - (jsonLen(CutLabel) + 2 + 2*sha256.Size + 2)
	names := slices.Collect(maps.Keys(out))
	slices.SortFunc(names, func(a, b string) int { return cmp.Or(cmp.Compare(cost[b], cost[a]), strings.Compare(a, b)) })
	for _, k := range names {
		if size <= room {
			break
		}
		size -= cost[k]
		folded[k] = out[k]
		delete(out, k)
	}
	b, _ := json.Marshal(folded)
	sum := sha256.Sum256(b)
	out[CutLabel] = hex.EncodeToString(sum[:])
	return out
}

// KeptName and KeptValue are a label's name and value as a replayed
// alert's are kept: as they are, within 128 and 256 bytes, with nothing
// that turns text around (rules.Shown); else shown, cut to fit, and the
// start of a hash of what they were, so that two apart stay apart.
func KeptName(k string) string  { return keptText(k, maxKeptName) }
func KeptValue(v string) string { return keptText(v, maxKeptValue) }

func keptText(s string, n int) string {
	shown := rules.Shown(s)
	if shown == s && len(s) <= n {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	tail := "…" + hex.EncodeToString(sum[:6])
	keep := min(len(shown), n-len(tail))
	for keep > 0 && keep < len(shown) && !utf8.RuneStart(shown[keep]) {
		keep--
	}
	return shown[:keep] + tail
}

// asciiJSON is how long encoding/json writes each ASCII byte, as this
// Go's does: measured once, as versions differ in what they escape.
var asciiJSON = func() (n [utf8.RuneSelf]int) {
	for c := range n {
		b, _ := json.Marshal(string(rune(c)))
		n[c] = len(b) - 2
	}
	return n
}()

// jsonLen is how long s is as encoding/json writes it, quoted, its HTML
// escaped: an ASCII text counted, not written. Past ASCII it is written,
// once: how a byte that isn't UTF-8 is written, or U+2028, differs from
// one Go to the next (go1.27 writes U+FFFD as it is, before it "\ufffd").
func jsonLen(s string) int {
	n := 2
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= utf8.RuneSelf {
			b, _ := json.Marshal(s)
			return len(b)
		}
		n += asciiJSON[c]
	}
	return n
}
