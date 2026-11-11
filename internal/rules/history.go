package rules

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ErrNoVersions is a history directory with no version in it.
var ErrNoVersions = errors.New("holds no versions of the rules")

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
// there. Anything else is refused, naming it, as are two names for one time,
// and a version, or a file in one, that is a link to somewhere outside it:
// what a version holds comes from a repository, which could hold a link to
// any file the reader can read.
func History(dir string) ([]Then, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(dir)
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
		if err := inside(root, path); err != nil {
			return nil, err
		}
		if i := slices.IndexFunc(out, func(t Then) bool { return t.At.Equal(at) }); i >= 0 {
			return nil, fmt.Errorf("%s and %s are named for one time", out[i].Path, path)
		}
		out = append(out, Then{At: at, Path: path})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s %w", dir, ErrNoVersions)
	}
	slices.SortFunc(out, func(a, b Then) int { return a.At.Compare(b.At) })
	return out, nil
}

// inside checks that a version directory, and every link in it, leads
// nowhere outside it: the version to within root, the history; a link in
// it, to within the version.
func inside(root, version string) error {
	at, err := filepath.EvalSymlinks(version)
	if err != nil {
		return err
	}
	if !within(root, at) {
		return fmt.Errorf("%s: a version is a directory in the history, not a link to elsewhere", version)
	}
	// The version as it is, not the link to it: WalkDir doesn't walk
	// through a link, and what is behind one would go unchecked.
	return filepath.WalkDir(at, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink == 0 {
			return err
		}
		to, err := filepath.EvalSymlinks(path)
		if err != nil || !within(at, to) {
			return fmt.Errorf("%s: a link to outside its version: a version holds its rule files", path)
		}
		return nil
	})
}

// within reports whether path is dir or below it.
func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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
