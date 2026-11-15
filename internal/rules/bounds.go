package rules

import (
	"context"
	"fmt"
	"maps"
	"slices"
)

// What a document of rules may make beyond its own text, however it makes
// it: aliases bringing in what an anchor holds again (YAMLValue), and a
// group's labels on each of its rules, recording rules too. ExpandMin at
// least, past it ExpandTimes its own size, and never more than MaxFile, as
// a file of rules is no more. A label's name and value are MaxLabel each at
// most, and what a rule says MaxSays each (cut). A YAML bound that counts
// values let through one string of 8 MB under an anchor aliased by 300
// rules, and nothing bounded a group's labels: one label of 8 MB in a group
// of 3,000 rules was 26 GB of rules to hash, each page that showed them
// going through them again. Real rule sets are far inside these: 5,000
// rules that share 300 bytes of group labels and 800 of annotations by an
// alias make some 5.5 MB beyond a file of 1 MB, a label is an identifier
// or a URL, rarely a KB, and a file of rules is 16 MiB at most.
const (
	ExpandMin   = 4 << 20
	ExpandTimes = 16
	MaxLabel    = 16 << 10
)

// expandLimit is what a document of size bytes may make beyond them.
func expandLimit(size int) int { return min(MaxFile, max(ExpandMin, ExpandTimes*size)) }

// bounded refuses the rules and recording rules a document of size bytes
// made, past MaxLabel or past what expandLimit lets it make beyond its own
// text: in time linear in what was read, each text's length alone looked
// at, before any rule is looked through, shown or hashed. The group whose
// labels made most is named.
func bounded(rs []Rule, recs []Recording, size int) error {
	made := 0
	type fan struct {
		bytes, rules int
	}
	groups := map[string]*fan{}
	for i := range rs {
		r := &rs[i]
		made += len(r.Name) + len(r.Group) + len(r.Expr) + len(r.Says.Summary) + len(r.Says.Description) + len(r.Says.Runbook)
		for k, v := range r.Says.Annotations {
			made += len(k) + len(v)
		}
		own, err := labelText(r.Labels, "rule", r.Name)
		if err != nil {
			return err
		}
		group, err := labelText(r.GroupLabels, "group", r.Group)
		if err != nil {
			return err
		}
		made += own + group
		if group > 0 {
			f := groups[r.Group]
			if f == nil {
				f = &fan{bytes: group}
				groups[r.Group] = f
			}
			f.rules++
		}
	}
	for _, r := range recs {
		n, err := labelText(r.Labels, "recording rule", r.Record)
		if err != nil {
			return err
		}
		made += len(r.Record) + len(r.Expr) + n
	}
	if limit := expandLimit(size); made > size+limit {
		why := ""
		most := ""
		for _, g := range slices.Sorted(maps.Keys(groups)) {
			if most == "" || groups[g].bytes*groups[g].rules > groups[most].bytes*groups[most].rules {
				most = g
			}
		}
		if most != "" && groups[most].rules > 1 {
			why = fmt.Sprintf("; group %.80q puts its labels, %d bytes, on each of its %d rules", most, groups[most].bytes, groups[most].rules)
		}
		return fmt.Errorf("its rules hold %d bytes of names, expressions, labels and what they say, %d more than its own %d bytes, and a document may make %d more at most: the larger of %d MiB and %d times its size, %d MiB at most%s",
			made, made-size, size, limit, ExpandMin>>20, ExpandTimes, MaxFile>>20, why)
	}
	return nil
}

// labelText is how long labels are, their names and values, refusing a
// name or a value past MaxLabel: what has them is a rule, a group or a
// recording rule, named.
func labelText(l map[string]string, what, name string) (int, error) {
	n := 0
	for k, v := range l {
		if len(k) > MaxLabel || len(v) > MaxLabel {
			return 0, fmt.Errorf("%s %.80q: label %.40q is %d bytes, and its value %d: a label's name and value are %d KiB at most",
				what, name, k, len(k), len(v), MaxLabel>>10)
		}
		n += len(k) + len(v)
	}
	return n, nil
}

// sized is ctx under which the document being read has been counted to
// its input's size already: one read within it (a ConfigMap's rule file,
// a namespace's) isn't counted again.
type sizedKey struct{}

// counted adds a document's size to its input's (OneInput), what aliases
// are let bring in again judged against it, unless it is one within a
// document counted already.
func counted(ctx context.Context, size int) context.Context {
	if ctx.Value(sizedKey{}) != nil {
		return ctx
	}
	if b, ok := ctx.Value(inputKey{}).(*input); ok {
		b.mu.Lock()
		b.size += size
		b.mu.Unlock()
	}
	return context.WithValue(ctx, sizedKey{}, true)
}
