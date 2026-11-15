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
// stream's documents, a ConfigMap's rule files, a ruler's namespace, the
// documents of one source in a version kept in the ledger): ExpandMin at
// least, past it ExpandTimes its size, and MaxMade at most; inputs read
// together (Together: a ruler's namespaces, a version's sources, a
// directory's files passed over) MaxMade in all. It is charged where a
// copy is made, before it is made, so that an input refused never makes
// what it would have:
//
//   - what YAML aliases bring in again (YAMLValue): its text, and the maps
//     and lists its values are held in (aliasValue); yaml.v3's bound counts
//     values, not their text, and one string of 8 MB under an anchor,
//     aliased by 100 rules' labels, made 5.1 GB, parsed and hashed;
//   - a group's labels and its name, on each of its rules (prometheus,
//     grafana, azure, fromAPI, a Google policy's): each alert rule's alerts
//     carry its labels, and its key holds its name (Rule.AlertLabels,
//     Rule.Key, Rule.Whole, the replay's), worked out for each rule wherever
//     its rules are judged, shown or hashed, and each recording rule has a
//     copy of its labels of its own. A group of 9,999 labels on 6,700
//     recording rules, a file of 286 KB, made 4.2 GB; a name of 1 MiB with
//     a direction mark on 1,000 rules kept 1.07 GB;
//   - a Google Rules' namespace, and an Azure rule group's cluster, put into
//     each selector of each rule's expression (scopeWith): a namespace of
//     40 KB on 1,000 rules of 16 selectors, a file of 99 KB, made 6.5 GB,
//     read and hashed;
//   - a ruler's namespace or an alarm's, on each of its rules (FromDocs);
//   - a document picked out of a stream and written afresh, its aliases in
//     full (written).
//
// Real sets are far inside it: their text is what they make, but for the
// labels and name a group shares, some hundreds of bytes a rule, and what
// is shared by aliases. kube-prometheus-stack's rules four times over, each
// group with labels of its own, make 1.8 MB of the 208 MB they may; 5,000
// rules sharing six labels and three annotations by aliases, 10.4 MB of
// the 20 MB a file of 648 KB may, at 16 times its size more than it may
// (TestRealSetsRead).
const (
	ExpandMin   = 4 << 20
	ExpandTimes = 32
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
// not the input alone past what it may make; File, that what is kept of a
// stream, written afresh (pick), is Made bytes, past MaxFile, which it is
// held to as the file it is read as.
type MadeError struct {
	By                string
	Made, Size, Limit int
	Together, File    bool
	// Most, of inputs read together, names the one that had made most of
	// it, MostMade bytes (input.name): what took them past may be little.
	Most     string
	MostMade int
}

func (e *MadeError) Error() string {
	switch {
	case e.File:
		return fmt.Sprintf("%s: %d bytes, more than the %d MiB a file of rules is read of", e.By, e.Made, MaxFile>>20)
	case e.Together && e.Most != "":
		return fmt.Sprintf("%s: what the inputs read together make beyond their own text, %d bytes so far, is past the %d MiB they may make in all, refused ones' among it; %.200s made the most of it, %d bytes",
			e.By, e.Made, MaxMade>>20, e.Most, e.MostMade)
	case e.Together:
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
// text, refused and passed over ones' too, what they made before they
// were: never given back, MaxMade at most. A charge refused isn't made,
// and isn't counted: what all of them make is the same, read in any
// order, and the read past MaxMade names the input whose making took it
// past.
type total struct {
	mu   sync.Mutex
	made int
	// of is what each input made, by its name (input.name), so the one
	// that made most is named with the one that took them past.
	of map[string]int
}

// add counts n made by the inputs together, b among them, refusing it past
// MaxMade, and not counting it then. Nothing made is never refused: the
// input that took them past MaxMade was.
func (t *total) add(b *input, n int, by func() string) error {
	if t == nil || n <= 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.made+n > MaxMade {
		e := &MadeError{By: by(), Made: t.made + n, Limit: MaxMade, Together: true}
		e.Most, e.MostMade = t.mostLocked("", 0)
		return e
	}
	t.made += n
	if b != nil && b.name != "" {
		if t.of == nil {
			t.of = map[string]int{}
		}
		t.of[b.name] += n
	}
	return nil
}

// most is the input that has made most of what the inputs together made,
// and how much, the input named made also made more, not counted yet (as
// YAMLValue counts what it made once it ends); "" when none is named.
func (t *total) most(named string, more int) (string, int) {
	if t == nil {
		return "", 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.mostLocked(named, more)
}

func (t *total) mostLocked(named string, more int) (string, int) {
	name, made := "", 0
	pick := func(k string, n int) {
		if n > made || n == made && k < name {
			name, made = k, n
		}
	}
	for k, n := range t.of {
		if k == named {
			n += more
		}
		pick(k, n)
	}
	if _, ok := t.of[named]; named != "" && !ok {
		pick(named, more)
	}
	return name, made
}

// left is what the inputs read together may make still.
func (t *total) left() int {
	if t == nil {
		return MaxMade
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return max(0, MaxMade-t.made)
}

// takeBack takes back n the inputs together made, of a document read
// again whole (pick), not passed over.
func (t *total) takeBack(n int) {
	if t == nil || n <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.made = max(0, t.made-n)
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
// inputs read with it past MaxMade (Together), the input named either way;
// by says what made them, only worked out then. A charge refused isn't
// counted, as it isn't made.
func (b *input) charge(n int, by func() string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if limit := madeLimit(b.size); b.made+n > limit {
		return &MadeError{By: by(), Made: b.made + n, Size: b.size, Limit: limit}
	}
	if err := b.total.add(b, n, by); err != nil {
		return err
	}
	b.made += n
	return nil
}

// room is what the input may make still, and the inputs read with it.
func (b *input) room() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return min(max(0, madeLimit(b.size)-b.made), b.total.left())
}

// mark is what an input had counted, to take back what a document passed
// over counted (undo), as nothing it made is kept: a file below a
// directory's top that doesn't read. What the inputs read together made
// (Together) isn't taken back then; it is (again) of the pieces of a
// stream read again whole, which are read, not passed over.
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

// again is undo, what the inputs read together made since m taken back
// too: the input reads what it read since again.
func (b *input) again(m mark) {
	b.mu.Lock()
	made := b.made - m.made
	b.mu.Unlock()
	b.total.takeBack(made)
	b.undo(m)
}

// errAliased is a ConfigMap's value a YAML alias brings in, whatever its
// tag (!!str, !!binary) or wherever the alias is (the value, the data, the
// whole ConfigMap): a ConfigMap the API server keeps holds none, and each
// would be read into rules again, a !!binary one decoded afresh.
var errAliased = errors.New("a value a YAML alias brings in, which a ConfigMap the API server keeps holds none of: each would be read into rules again")

// aliasedText notes the text of strings YAMLValue made in an alias's
// expansion, by where it is: an alias of a !!str brings in its anchor's
// text, so the anchor's own is noted too; one of a !!binary, a string
// decoded afresh. Strings of fewer than two bytes are passed over: Go
// keeps one of each single byte, which any value of it shares.
func (b *input) aliasedText(ss []string) {
	if len(ss) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.brought == nil {
		b.brought = map[*byte]bool{}
	}
	for _, s := range ss {
		b.brought[unsafe.StringData(s)] = true
	}
}

// broughtIn reports whether s is the text of a string an alias brought
// in (aliasedText).
func (b *input) broughtIn(s string) bool {
	if len(s) < 2 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.brought[unsafe.StringData(s)]
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
