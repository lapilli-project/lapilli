package rules

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// A history of the rules is a directory of earlier versions of them, one
// subdirectory each, named by the time it took effect and holding the rule
// files as they stood then (docs/design/history.md). recipes/versions
// writes one from git.

// Then is a version in a history: when it took effect, and where its files
// are.
type Then struct {
	At   time.Time
	Path string
}

// basicTime is the basic form of an RFC 3339 time in UTC, for tools that
// don't like colons in names.
const basicTime = "20060102T150405Z"

// History lists the versions in dir, the earliest first. A name is an RFC
// 3339 time, 2026-10-14T16:40:31Z, or its basic form, 20261014T164031Z.
// Names starting with a dot are passed over: a mounted volume keeps its own
// there. Anything else is refused, naming it, as are two names for one time.
func History(dir string) ([]Then, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Then
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(dir, name)
		at, ok := historyTime(name)
		if !ok {
			return nil, fmt.Errorf("%s: a version is named by when it took effect, an RFC 3339 time such as 2026-10-14T16:40:31Z or 20261014T164031Z", path)
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("%s: a version is a directory of rule files", path)
		}
		if i := slices.IndexFunc(out, func(t Then) bool { return t.At.Equal(at) }); i >= 0 {
			return nil, fmt.Errorf("%s and %s are named for one time", out[i].Path, path)
		}
		out = append(out, Then{At: at, Path: path})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s holds no versions of the rules", dir)
	}
	slices.SortFunc(out, func(a, b Then) int { return a.At.Compare(b.At) })
	return out, nil
}

// historyTime reads a version's name as a time, to the second.
func historyTime(name string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, name); err == nil {
		return t.UTC().Truncate(time.Second), true
	}
	if t, err := time.Parse(basicTime, name); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}
