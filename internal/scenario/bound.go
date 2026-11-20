package scenario

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

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
	// MaxSamples are the values the input series of a file may expand to,
	// in all its tests: each of a series' values, a blank (_) among them,
	// is one promtool holds, or allocates.
	MaxSamples = 100000
	// MaxSteps are the evaluations of one test: its latest eval_time over
	// the file's evaluation_interval (a minute when it gives none).
	MaxSteps = 10000
)

// times is a repetition in values notation, at an item's end: the x and
// the count, which promtool reads as a whole number, in decimal.
var times = regexp.MustCompile(`x([0-9]+)$`)

// Bound reads a test file as promtool would expand it, without running
// anything, and refuses one past the bounds: more than MaxTests tests,
// MaxSeries input series (MaxSeriesATest in a test), values expanding to
// more than MaxSamples, or a test evaluating more than MaxSteps times. A
// file it can't read as promtool's is refused too: what isn't read here
// isn't run.
func Bound(b []byte) error {
	if _, err := rules.DecodeYAML(context.Background(), b); err != nil {
		return err
	}
	var f struct {
		EvaluationInterval string `yaml:"evaluation_interval"`
		Tests              []struct {
			InputSeries []struct {
				Series string    `yaml:"series"`
				Values yaml.Node `yaml:"values"`
			} `yaml:"input_series"`
			AlertRuleTests []struct {
				EvalTime string `yaml:"eval_time"`
			} `yaml:"alert_rule_test"`
			PromqlExprTests []struct {
				EvalTime string `yaml:"eval_time"`
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
	for i, t := range f.Tests {
		if len(t.InputSeries) > MaxSeriesATest {
			return fmt.Errorf("test %d: %d input series: %d a test at most", i+1, len(t.InputSeries), MaxSeriesATest)
		}
		series += len(t.InputSeries)
		if series > MaxSeries {
			return fmt.Errorf("more than %d input series in all", MaxSeries)
		}
		for _, s := range t.InputSeries {
			if s.Values.Kind != 0 && s.Values.Kind != yaml.ScalarNode {
				return fmt.Errorf("test %d: line %d: values is a text, as promtool reads it", i+1, s.Values.Line)
			}
			n, err := Samples(s.Values.Value)
			if err != nil {
				return fmt.Errorf("test %d: line %d: %w", i+1, s.Values.Line, err)
			}
			if samples += n; samples > MaxSamples {
				return fmt.Errorf("the input series expand to more than %d values in all: give fewer, or repeat fewer times (0+1x100 is 101 values)", MaxSamples)
			}
		}
		var latest time.Duration
		for _, e := range append(t.AlertRuleTests, t.PromqlExprTests...) {
			d, ok := content.ParseDuration(e.EvalTime)
			if e.EvalTime == "" {
				d, ok = 0, true
			}
			if !ok {
				return fmt.Errorf("test %d: eval_time %q: want a duration such as 10m", i+1, e.EvalTime)
			}
			latest = max(latest, d)
		}
		if latest/every > MaxSteps {
			return fmt.Errorf("test %d: evaluated %d times, every %s to its latest eval_time: %d at most", i+1, latest/every, every, MaxSteps)
		}
	}
	return nil
}

// Samples is how many values a series' values notation expands to, as
// promtool's parser expands it before anything is loaded: an item a
// value; a+bxN, axN and a histogram's xN, N+1; _xN, N blanks. Items are
// apart by spaces; a native histogram, {{...}}, is one item whatever it
// holds. Counted at most, never less: a count it can't read as a whole
// number is refused.
func Samples(values string) (int, error) {
	n := 0
	for _, item := range seriesItems(values) {
		rest := item
		// What a histogram holds counts nothing: only what follows it.
		for {
			i := strings.Index(rest, "{{")
			if i < 0 {
				break
			}
			j := strings.Index(rest[i:], "}}")
			if j < 0 {
				return 0, errors.New("a histogram, {{, isn't closed, }}")
			}
			rest = rest[:i] + "h" + rest[i+j+2:]
		}
		m := times.FindStringSubmatch(rest)
		if m == nil {
			n++
			continue
		}
		k, err := strconv.ParseUint(m[1], 10, 64)
		if err != nil || k > MaxSamples {
			return 0, fmt.Errorf("values repeat %s times: more than %d", m[1], MaxSamples)
		}
		if !strings.HasPrefix(rest, "_") {
			k++
		}
		n += int(k)
		if n > MaxSamples {
			return 0, fmt.Errorf("values expand to more than %d", MaxSamples)
		}
	}
	return n, nil
}

// seriesItems are the items of values notation, apart by spaces, a
// histogram's {{...}} one item with what follows it.
func seriesItems(s string) []string {
	var out []string
	var b strings.Builder
	open := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case !open && strings.HasPrefix(s[i:], "{{"):
			open = true
			b.WriteString("{{")
			i++
		case open && strings.HasPrefix(s[i:], "}}"):
			open = false
			b.WriteString("}}")
			i++
		case !open && (c == ' ' || c == '\t' || c == '\n' || c == '\r'):
			if b.Len() > 0 {
				out = append(out, b.String())
				b.Reset()
			}
		default:
			b.WriteByte(c)
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}
