package scenario

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/content"
	"lapilli.dev/internal/rules"
)

// What an agent's test file may make promtool hold and do (docs/design/
// expectations.md, step 3). promtool loads a test's series into memory
// inside lapilli serve's container, each value its values notation
// expands to (0+1x100000000 is a hundred million), and evaluates the rules
// at each step from 0 to the test's latest eval_time, keeping each alert
// it finds: a test file of a kilobyte could take the container's memory.
// An operator's test files are the operator's; an agent's are read, and
// refused, by Bound before promtool runs them.
const (
	// MaxTests are the tests a file may hold.
	MaxTests = 100
	// MaxSeries are the input series a file may hold, in all its tests,
	// and MaxSeriesATest of one test.
	MaxSeries      = 1000
	MaxSeriesATest = 100
	// MaxSamples are the values the input series of a file, and the
	// histograms its expected samples give, may expand to, in all its
	// tests: each of a series' values, a blank (_) among them, is one
	// promtool holds, or allocates; a native histogram is as many as the
	// numbers it holds, its buckets among them, each time it is repeated.
	MaxSamples = 100000
	// MaxSteps are the evaluations of one test: its latest eval_time over
	// the file's evaluation_interval (a minute when it gives none).
	MaxSteps = 10000
)

// Bound reads a test file as promtool would expand it, without running
// anything, and refuses one past the bounds: more than MaxTests tests,
// MaxSeries input series (MaxSeriesATest in a test), values expanding to
// more than MaxSamples, or a test evaluating more than MaxSteps times. It
// reads what promtool parses as values notation: each input series, which
// promtool writes as a line of its load command, "  <series> <values>",
// and each histogram an expected sample gives. A file it can't read as
// promtool's is refused too: what isn't read here isn't run.
func Bound(b []byte) error {
	if _, err := rules.DecodeYAML(context.Background(), b); err != nil {
		return err
	}
	var f struct {
		EvaluationInterval string `yaml:"evaluation_interval"`
		Tests              []struct {
			InputSeries []struct {
				Series yaml.Node `yaml:"series"`
				Values yaml.Node `yaml:"values"`
			} `yaml:"input_series"`
			AlertRuleTests []struct {
				EvalTime string `yaml:"eval_time"`
			} `yaml:"alert_rule_test"`
			PromqlExprTests []struct {
				EvalTime   string `yaml:"eval_time"`
				ExpSamples []struct {
					Histogram yaml.Node `yaml:"histogram"`
				} `yaml:"exp_samples"`
			} `yaml:"promql_expr_test"`
		} `yaml:"tests"`
	}
	if err := yaml.NewDecoder(bytes.NewReader(b)).Decode(&f); err != nil {
		return fmt.Errorf("it can't be read as promtool's test file: %w", err)
	}
	if len(f.Tests) > MaxTests {
		return fmt.Errorf("%d tests: %d at most", len(f.Tests), MaxTests)
	}
	every := time.Minute
	if f.EvaluationInterval != "" {
		d, ok := content.ParseDuration(f.EvaluationInterval)
		if !ok || d <= 0 {
			return fmt.Errorf("evaluation_interval %q: want a duration such as 1m", f.EvaluationInterval)
		}
		every = d
	}
	series, samples := 0, 0
	// count adds what values notation expands to, refusing it past the
	// bound in all.
	count := func(i, line int, v string) error {
		k, err := Samples(v)
		if err != nil {
			return fmt.Errorf("test %d: line %d: %w", i+1, line, err)
		}
		if samples += k; samples > MaxSamples {
			return fmt.Errorf("the input series and expected histograms expand to more than %d values in all: give fewer, or repeat fewer times (0+1x100 is 101 values)", MaxSamples)
		}
		return nil
	}
	for i, t := range f.Tests {
		if len(t.InputSeries) > MaxSeriesATest {
			return fmt.Errorf("test %d: %d input series: %d a test at most", i+1, len(t.InputSeries), MaxSeriesATest)
		}
		series += len(t.InputSeries)
		if series > MaxSeries {
			return fmt.Errorf("more than %d input series in all", MaxSeries)
		}
		for _, s := range t.InputSeries {
			name, err := text(s.Series, "series")
			if err == nil {
				err = Series(name)
			}
			if err != nil {
				return fmt.Errorf("test %d: line %d: %w", i+1, s.Series.Line, err)
			}
			values, err := text(s.Values, "values")
			if err != nil {
				return fmt.Errorf("test %d: line %d: %w", i+1, s.Values.Line, err)
			}
			// A line break in its values would start another line of the
			// load command: another series, which nothing here counts.
			if j := strings.IndexByte(values, '\n'); j >= 0 && strings.TrimSpace(values[j:]) != "" {
				return fmt.Errorf("test %d: line %d: values holds a line break: promtool would read what follows it as another series", i+1, s.Values.Line)
			}
			if err := count(i, s.Values.Line, values); err != nil {
				return err
			}
		}
		var latest time.Duration
		evals := func(at string) error {
			d, ok := content.ParseDuration(at)
			if at == "" {
				d, ok = 0, true
			}
			if !ok {
				return fmt.Errorf("test %d: eval_time %q: want a duration such as 10m", i+1, at)
			}
			latest = max(latest, d)
			return nil
		}
		for _, e := range t.AlertRuleTests {
			if err := evals(e.EvalTime); err != nil {
				return err
			}
		}
		for _, e := range t.PromqlExprTests {
			if err := evals(e.EvalTime); err != nil {
				return err
			}
			// promtool reads an expected histogram as values notation too.
			for _, x := range e.ExpSamples {
				h, err := text(x.Histogram, "histogram")
				if err == nil {
					err = count(i, x.Histogram.Line, h)
				} else {
					err = fmt.Errorf("test %d: line %d: %w", i+1, x.Histogram.Line, err)
				}
				if err != nil {
					return err
				}
			}
		}
		if latest/every > MaxSteps {
			return fmt.Errorf("test %d: evaluated %d times, every %s to its latest eval_time: %d at most", i+1, latest/every, every, MaxSteps)
		}
	}
	return nil
}

// text is a scalar of a test file as promtool reads it into a string:
// plain text, or nothing; never an alias, a list or a mapping, nor a tag
// such as !!binary, whose text promtool's YAML would decode into another
// than the one read here.
func text(n yaml.Node, what string) (string, error) {
	switch {
	case n.Kind == 0, n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null" && n.Style&yaml.TaggedStyle == 0:
		return "", nil
	case n.Kind != yaml.ScalarNode, n.Style&yaml.TaggedStyle != 0 && n.ShortTag() != "!!str":
		return "", fmt.Errorf("%s is a text, as promtool reads it", what)
	}
	return n.Value, nil
}

// Why a series isn't one a line of promtool's load command holds alone.
var (
	errSeries = errors.New("series is a metric name, its labels in braces, or both, with no space, line break or anything else " +
		"outside the labels' quoted values: promtool reads what follows as values, or as another series")
	errControl = errors.New("series: a label's value holds a line break or another control character: promtool reads a line at a time")
)

// Series reads an input series' series as promtool writes it, at the
// start of a line of its load command with the values after a space: a
// metric name, its labels in braces, or both, and nothing else. A space
// would begin values, and a line break another series, neither counted
// by Samples; so neither is taken outside a label's quoted value, nor a
// line break or another control character inside one. promtool's parser
// reads it after this; what it refuses fails the test.
func Series(s string) error {
	i := ident(s, 0, true)
	if i == len(s) {
		if i == 0 {
			return errors.New("series is empty: give a metric name, its labels in braces, or both")
		}
		return nil
	}
	if s[i] != '{' {
		return errSeries
	}
	i++
	for {
		if i < len(s) && s[i] == '}' {
			i++
			break
		}
		// A label: its name and quoted value; or a quoted name, its own
		// (a metric's name in braces), or with a value.
		var err error
		if j := ident(s, i, false); j > i {
			if j >= len(s) || s[j] != '=' {
				return errSeries
			}
			i, err = quoted(s, j+1)
		} else if i, err = quoted(s, i); err == nil && i < len(s) && s[i] == '=' {
			i, err = quoted(s, i+1)
		}
		if err != nil {
			return err
		}
		if i < len(s) && s[i] == ',' {
			i++
			continue
		}
		if i < len(s) && s[i] == '}' {
			i++
			break
		}
		return errSeries
	}
	if i != len(s) {
		return errSeries
	}
	return nil
}

// ident is where a name that begins at s[i] ends: a metric's
// ([a-zA-Z_:][a-zA-Z0-9_:]*) or a label's (no colon); i itself where none
// does.
func ident(s string, i int, metric bool) int {
	j := i
	for ; j < len(s); j++ {
		c := s[j]
		if !(c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || j > i && '0' <= c && c <= '9' || metric && c == ':') {
			break
		}
	}
	return j
}

// quoted is where a quoted text that begins at s[i] ends, past its closing
// quote: "...", '...' (with escapes) or `...`, holding no line break nor
// another control character, valid UTF-8.
func quoted(s string, i int) (int, error) {
	if i >= len(s) || s[i] != '"' && s[i] != '\'' && s[i] != '`' {
		return i, errSeries
	}
	q := s[i]
	for j := i + 1; j < len(s); {
		r, size := utf8.DecodeRuneInString(s[j:])
		switch {
		case r == utf8.RuneError && size <= 1, unicode.IsControl(r):
			return j, errControl
		case r == rune(q):
			return j + 1, nil
		case r == '\\' && q != '`':
			// An escape: the character after it is the value's.
			if j += size; j >= len(s) {
				return j, errSeries
			}
			if r, size = utf8.DecodeRuneInString(s[j:]); r == utf8.RuneError && size <= 1 || unicode.IsControl(r) {
				return j, errControl
			}
		}
		j += size
	}
	return len(s), errSeries
}

// Samples is how many values a series' values notation expands to, as
// promtool's parser expands it before anything is loaded, read in one
// pass. Items are apart by spaces: an item is a value; a+bxN and axN, N+1;
// _xN, N blanks. A native histogram, {{...}}, is one item with what
// follows it, and counts as many as the numbers it holds (its fields' and
// buckets', one at least) each time it is repeated: {{...}}xN N+1 times,
// and {{a}}+{{b}}xN, which promtool copies at each step, the numbers of
// both N+1 times; an item of more than two is refused. Counted at most,
// never less: every x outside a histogram is a repetition of all its item
// holds, and what isn't values notation (a quote, a brace that doesn't
// open or close a histogram, a count that isn't a whole number) is
// refused rather than counted, as promtool would read labels there, or
// another number; so are values of more than MaxTest bytes, a test
// file's own bound.
func Samples(values string) (int, error) {
	if len(values) > MaxTest {
		return 0, fmt.Errorf("values: more than %d bytes, as a test file is", MaxTest)
	}
	n := 0
	var it item
	for i := 0; i < len(values); {
		c := values[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			if err := it.add(&n); err != nil {
				return 0, err
			}
			it = item{}
			i++
			continue
		}
		if !it.started {
			it = item{started: true, start: i}
		}
		switch {
		case c == '{':
			if !strings.HasPrefix(values[i:], "{{") {
				return 0, errors.New("values: a brace that doesn't open a histogram, {{: promtool would read labels there")
			}
			if it.hists++; it.hists > 2 {
				return 0, errors.New("values: an item of more than two histograms, {{...}}: one, repeated (xN), or two added (+) or taken away (-), repeated")
			}
			end, w, err := histogram(values, i+2)
			if err != nil {
				return 0, err
			}
			it.weight += int64(w)
			i = end
		case c == 'x':
			j := i + 1
			for j < len(values) && '0' <= values[j] && values[j] <= '9' {
				j++
			}
			if j == i+1 {
				return 0, errors.New("values: x is a repetition, x and a whole number (0+1x10)")
			}
			k, err := strconv.Atoi(values[i+1 : j])
			if err != nil || k > MaxSamples {
				return 0, fmt.Errorf("values repeat %s times: more than %d", values[i+1:j], MaxSamples)
			}
			// _xN is N blanks; any other, N+1 values.
			if it.reps == 0 {
				it.blank = i == it.start+1 && values[it.start] == '_'
			}
			it.reps += int64(k) + 1
			i = j
		case word(c):
			i++
		default:
			r, _ := utf8.DecodeRuneInString(values[i:])
			return 0, fmt.Errorf("values: %q isn't values notation: numbers, stale, _, x, + and -, and histograms, {{...}}", r)
		}
	}
	if err := it.add(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// item is an item of values notation, as Samples reads it.
type item struct {
	started bool
	start   int   // where it begins
	blank   bool  // it is _xN, N blanks
	hists   int   // its histograms, {{...}}
	weight  int64 // the numbers they hold, one at least each
	reps    int64 // N+1 of each repetition xN in it
}

// add adds what the item expands to, refusing past MaxSamples in all:
// one value, or its histograms' numbers, each time it is repeated.
func (it item) add(n *int) error {
	if !it.started {
		return nil
	}
	k := int64(1)
	if it.hists > 0 {
		k = it.weight
	}
	if it.reps > 0 {
		// Of MaxTest bytes at most, neither is past 2^37: their product
		// doesn't overflow.
		k *= it.reps
		if it.blank && it.hists == 0 {
			k--
		}
	}
	if k > int64(MaxSamples-*n) {
		return fmt.Errorf("values expand to more than %d", MaxSamples)
	}
	*n += int(k)
	return nil
}

// word says whether c is of a number, stale, or a blank, as values
// notation writes them.
func word(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || c == '.' || c == '+' || c == '-' || c == '_'
}

// histogram reads a native histogram's description from s[i], past its
// {{, to its }}: where it ends, past the }}, and the numbers it holds, one
// at least: each field's value (after a colon) and each bucket (inside
// [...]), as promtool's parser keeps them. A brace inside it, a bracket
// not closed or opened twice, or what isn't of a description is refused.
func histogram(s string, i int) (int, int, error) {
	nums := 0
	brackets, colon, in := false, false, false
	for ; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '}' && !brackets && strings.HasPrefix(s[i:], "}}"):
			return i + 2, max(1, nums), nil
		case c == '[' && !brackets:
			brackets, colon, in = true, false, false
		case c == ']' && brackets:
			brackets, in = false, false
		case c == ':' && !brackets:
			colon, in = true, false
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			in = false
		case word(c):
			if !in && (brackets || colon) {
				nums++
			}
			if !in {
				colon = false
			}
			in = true
		default:
			r, _ := utf8.DecodeRuneInString(s[i:])
			return 0, 0, fmt.Errorf("values: %q in a histogram, {{...}}, isn't of its description", r)
		}
	}
	return 0, 0, errors.New("values: a histogram, {{, isn't closed, }}")
}
