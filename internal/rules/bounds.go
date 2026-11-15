package rules

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"unsafe"
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
//     9,999 labels on 6,700 recording rules, a file of 286 KB, made 4.2 GB;
//   - a Google Rules' namespace, and an Azure rule group's cluster, put into
//     each selector of each rule's expression (scopeWith): a namespace of
//     40 KB on 1,000 rules of 16 selectors, a file of 99 KB, made 6.5 GB,
//     read and hashed.
//
// Real sets are far inside it: their text is what they make, but for the
// labels a group shares, some hundreds of bytes a rule, and annotations
// shared by an alias. kube-prometheus-stack's rules four times over, each
// group with labels of its own, make 1.5 MB of the 100 MB they may; 5,000
// rules, a group's 300 bytes of labels on each and 800 bytes of
// annotations shared by an alias, 6.8 MB beyond a file of 1 MB
// (TestRealSetsRead).
const (
	ExpandMin   = 4 << 20
	ExpandTimes = 16
	MaxMade     = 256 << 20
)

// What a map of labels takes beside its text, as Go 1.26 holds one: some
// 340 bytes for up to 8 labels, its first table, and some 50 to 85 bytes a
// label past them, as tables double; charged a third more, so that another
// Go's maps don't take more than is charged (TestLabelsCost). aliasValue is
// what a value or a key an alias brings in again takes beside its text: an
// entry of a map or a list, and the interface it is held in.
const (
	smallLabels = 8
	labelsSmall = 448
	labelEntry  = 112
	aliasValue  = 64
)

// madeLimit is what an input of size bytes may make beyond them.
func madeLimit(size int) int { return min(MaxMade, max(ExpandMin, ExpandTimes*size)) }

// MadeError is an input refused for what it would make beyond its own text
// (madeLimit): By says what made it, at the point it went past. Together
// says it is the inputs read together (Together) that went past MaxMade,
// not the input alone past what it may make.
type MadeError struct {
	By                string
	Made, Size, Limit int
	Together          bool
}

func (e *MadeError) Error() string {
	if e.Together {
		return fmt.Sprintf("%s: what the inputs read together make beyond their own text, %d bytes so far, is past the %d MiB they may make in all, refused ones' among it",
			e.By, e.Made, MaxMade>>20)
	}
	return fmt.Sprintf("%s: what the input makes beyond its own text, %d bytes so far, is past the %d an input of %d bytes may make: the larger of %d MiB and %d times its size, %d MiB at most",
		e.By, e.Made, e.Limit, e.Size, ExpandMin>>20, ExpandTimes, MaxMade>>20)
}

// refused reports whether err is an input refused for what it makes: never
// passed over, as a rule file that doesn't read may be, but said.
func refused(err error) bool {
	var m *MadeError
	return errors.As(err, &m)
}

// OverTotal reports whether err is inputs read together refused for what
// they made in all (Together): a reader passing inputs over one by one
// (a ruler's namespaces) gives up the whole read then, as each read after
// would be refused too.
func OverTotal(err error) bool {
	var m *MadeError
	return errors.As(err, &m) && m.Together
}

// total is what inputs read together (Together) have made beyond their
// text, refused and passed over ones' too: never given back, MaxMade at
// most.
type total struct {
	mu   sync.Mutex
	made int
}

// add counts n made by the inputs together, refusing them past MaxMade.
func (t *total) add(n int, by func() string) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.made += n
	if t.made > MaxMade {
		return &MadeError{By: by(), Made: t.made, Limit: MaxMade, Together: true}
	}
	return nil
}

type totalKey struct{}

// Together is ctx under which the inputs read, each of its own (OneInput,
// NoAliases, FromDocsContext's), are counted together too: what they make
// in all, refused ones' among it and never given back, is MaxMade at most
// (OverTotal). A ruler's namespaces are each an input, so that one can't
// use up what another may make, and together so, so that refused ones
// can't make without end.
func Together(ctx context.Context) context.Context {
	if _, ok := ctx.Value(totalKey{}).(*total); ok {
		return ctx
	}
	return context.WithValue(ctx, totalKey{}, &total{})
}

func totalOf(ctx context.Context) *total {
	t, _ := ctx.Value(totalKey{}).(*total)
	return t
}

// inputOf is the input ctx says the document read is of, or one of its own.
func inputOf(ctx context.Context) *input {
	if b, ok := ctx.Value(inputKey{}).(*input); ok {
		return b
	}
	return &input{total: totalOf(ctx)}
}

// charge counts n bytes made, refusing the input past madeLimit, and the
// inputs read with it past MaxMade (Together); by says what made them,
// only worked out then.
func (b *input) charge(n int, by func() string) error {
	b.mu.Lock()
	b.made += n
	made, size := b.made, b.size
	b.mu.Unlock()
	if limit := madeLimit(size); made > limit {
		b.total.add(n, by)
		return &MadeError{By: by(), Made: made, Size: size, Limit: limit}
	}
	return b.total.add(n, by)
}

// room is what the input may make still.
func (b *input) room() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return max(0, madeLimit(b.size)-b.made)
}

// mark is what an input had counted, to take back what a document passed
// over counted (undo), as nothing it made is kept: a file below a
// directory's top that doesn't read, the pieces of a stream read again
// whole. What the inputs read together made (Together) isn't taken back.
type mark struct{ size, made, written, decoded, aliased int }

func (b *input) mark() mark {
	b.mu.Lock()
	defer b.mu.Unlock()
	return mark{b.size, b.made, b.written, b.decoded, b.aliased}
}

func (b *input) undo(m mark) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.size, b.made, b.written, b.decoded, b.aliased = m.size, m.made, m.written, m.decoded, m.aliased
}

// errReadAgain is a ConfigMap's value read as rules already in the input:
// only an alias makes one value two keys', which a ConfigMap the API
// server keeps never holds, and each would be read into rules again.
var errReadAgain = errors.New("a rule file another key holds already, by a YAML alias, which a ConfigMap the API server keeps holds none of: each would be read again")

// once says whether a ConfigMap's value s, by where its text is, was read
// in the input already, and whether it held rules then: an alias brings
// in the same text, two values written never share it.
func (b *input) once(s string) (seen, rules bool) {
	if len(s) == 0 {
		return false, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rules, seen = b.values[unsafe.StringData(s)]
	return seen, rules
}

// read notes a ConfigMap's value read, and whether it held rules (once).
func (b *input) read(s string, rules bool) {
	if len(s) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.values == nil {
		b.values = map[*byte]bool{}
	}
	b.values[unsafe.StringData(s)] = rules
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
