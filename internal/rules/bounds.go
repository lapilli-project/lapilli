package rules

import (
	"context"
	"errors"
	"fmt"
)

// What an input of rules may make beyond its own text, however it makes it,
// counted for the whole input (OneInput: a file, a directory's files, a
// stream's documents, a ConfigMap's rule files, a ruler's namespaces, a
// version's documents in the ledger): ExpandMin at least, past it
// ExpandTimes its size, and MaxMade at most. It is charged where a copy is
// made, before it is made, so that an input refused never makes what it
// would have:
//
//   - the text YAML aliases bring in again (YAMLValue): yaml.v3's bound
//     counts values, not their text, and one string of 8 MB under an anchor,
//     aliased by 300 rules' labels, was 2.4 GB of labels;
//   - a group's labels, on each of its rules (prometheus): each alert rule's
//     alerts carry them (Rule.AlertLabels, Rule.Whole, the replay's), which
//     is worked out for each rule wherever its rules are judged, shown or
//     hashed, and each recording rule has a copy of its own. A group of
//     9,999 labels on 6,700 recording rules, a file of 268 KB, made 3.1 GB;
//   - a Google Rules' namespace, and an Azure rule group's cluster, put into
//     each selector of each rule's expression (scopeWith): a namespace of
//     40 KB in a file of 78 KB made 1.6 GB.
//
// Real sets are far inside it: their text is what they make, but for the
// labels a group shares, some kilobyte a rule, and annotations shared by an
// alias. 5,000 rules, a group's 300 bytes of labels on each and 800 bytes
// of annotations shared, make some 10 MB beyond a file of 1.1 MB.
const (
	ExpandMin   = 4 << 20
	ExpandTimes = 16
	MaxMade     = 256 << 20
)

// What a map of labels takes beside its text, as Go 1.26 holds one: some
// 340 bytes for up to 8 labels, its first table, and some 50 to 85 bytes a
// label past them, as tables double (TestLabelsCost).
const (
	smallLabels = 8
	labelsSmall = 352
	labelEntry  = 96
)

// madeLimit is what an input of size bytes may make beyond them.
func madeLimit(size int) int { return min(MaxMade, max(ExpandMin, ExpandTimes*size)) }

// MadeError is an input refused for what it would make beyond its own text
// (madeLimit): By says what made it, at the point it went past.
type MadeError struct {
	By                string
	Made, Size, Limit int
}

func (e *MadeError) Error() string {
	return fmt.Sprintf("%s: what the input makes beyond its own text, %d bytes so far, is past the %d an input of %d bytes may make: the larger of %d MiB and %d times its size, %d MiB at most",
		e.By, e.Made, e.Limit, e.Size, ExpandMin>>20, ExpandTimes, MaxMade>>20)
}

// refused reports whether err is an input refused for what it makes: never
// passed over, as a rule file that doesn't read may be, but said.
func refused(err error) bool {
	var m *MadeError
	return errors.As(err, &m)
}

// inputOf is the input ctx says the document read is of, or one of its own.
func inputOf(ctx context.Context) *input {
	if b, ok := ctx.Value(inputKey{}).(*input); ok {
		return b
	}
	return &input{}
}

// charge counts n bytes made, refusing the input past madeLimit; by says
// what made them, only worked out then.
func (b *input) charge(n int, by func() string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.made += n
	if limit := madeLimit(b.size); b.made > limit {
		return &MadeError{By: by(), Made: b.made, Size: b.size, Limit: limit}
	}
	return nil
}

// room is what the input may make still.
func (b *input) room() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return max(0, madeLimit(b.size)-b.made)
}

// mark is what an input had counted, to take back what a document refused
// counted (undo), as it makes nothing kept: one of a ruler's namespaces,
// passed over, mustn't use up what the others may make.
type mark struct{ size, made, written int }

func (b *input) mark() mark {
	b.mu.Lock()
	defer b.mu.Unlock()
	return mark{b.size, b.made, b.written}
}

func (b *input) undo(m mark) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.size, b.made, b.written = m.size, m.made, m.written
}

// labelsCost is what a copy of labels takes: the map, and their text,
// which a key or a page made of them writes out.
func labelsCost(l map[string]string) int {
	n := labelsSmall
	if len(l) > smallLabels {
		n = labelEntry * len(l)
	}
	for k, v := range l {
		n += len(k) + len(v)
	}
	return n
}

// sizedKey is on a ctx under which the document being read has been counted
// to its input's size already: one read within it (a ConfigMap's rule file,
// an Amazon namespace's, a picked stream's pieces) isn't counted again.
type sizedKey struct{}

// counted adds a document's size to its input's (OneInput), what it may
// make judged against it, unless it is one within a document counted
// already.
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
