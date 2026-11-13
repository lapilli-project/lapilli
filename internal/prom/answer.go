package prom

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// What one answer to a range query may hold. An API is outside input, and
// a hostile one, or one compromised, can answer a few KB on the wire,
// gzipped, for hundreds of MB of JSON: the answer is read as it comes, and
// only what was asked for is kept.
var (
	// maxBody is how much of an answer is read, once decompressed.
	maxBody int64 = 256 << 20
	// maxAnswerSeries and maxAnswerSamples are the most series, and
	// samples in all, an answer may hold: past them, it is an answer too
	// large, and a shorter range is asked (fetch), as when the API says
	// so itself. Replaced in tests.
	maxAnswerSeries  = 50000
	maxAnswerSamples = 5000000
	// maxAnswerLabels is what an answer's series' labels may take, as
	// labelCost counts them: a hostile API's one series of tiny labels,
	// repeated, gzips to almost nothing, and each copy is a map. 50,000
	// series of some 2.7 KB each: an alert as kube-prometheus labels one,
	// fourteen labels, is 1.4 KB. Past it, the answer can't be read: a
	// shorter range holds the same series, as wide.
	maxAnswerLabels = 128 << 20
)

// What the answers to one range (QueryRange) may hold in all: it is asked
// a day at a time (maxPoints), and a day in halves as often as the API
// says an answer is too large, each answer held to maxAnswer*. A hostile
// API, saying so of every range of more than one step and answering each
// step with series of its own, would be held whole: a day's answers at a
// minute, 1,440 of 50,000 series. Twice what an answer may hold, as the
// halves of a day may hold their own series, as pods come and go; and
// samples as a Client is given (DefaultMaxSamples, --max-samples), which
// take most of what a range is held in, and --parallel ranges at once.
// Replaced in tests.
var (
	maxRangeSeries = 100000
	maxRangeLabels = 256 << 20
)

// labelCost is what a series' labels take in memory, near enough, as
// measured: a map's entry and the strings decoded into it, the map grown
// to hold them, some 96 bytes a label beyond its text.
func labelCost(m map[string]string) int {
	n := 64
	for k, v := range m {
		n += len(k) + len(v) + 96
	}
	return n
}

// headKept is how much of an answer is kept to say what it was when it
// isn't the API's.
const headKept = 4 << 10

// answer is what queryRange reads of an answer.
type answer struct {
	status, errMsg, resultType string
	warnings                   []string
	series                     []Series
	// bad is why the result, as it came, isn't a matrix lapilli can
	// read, said once its type is known; big why it is too large.
	bad error
	big string
}

// errNotJSON is an answer that isn't the JSON of the API's answers.
var errNotJSON = errors.New("not JSON of the API's form")

// readAnswer reads an answer to a range query asked from start to end
// every step as it streams in: a sample outside [start, end] is dropped,
// a series of more samples than the range has steps is unreadable, and
// more series or samples than maxAnswerSeries or maxAnswerSamples make it
// too big. The time of a sample is read as written (ParseTime). Each value
// is read with what it may need of the stream at most: a series what its
// steps could take (seriesBytes), anything else valueBytes; a longer one
// is unreadable, and the rest isn't read.
func readAnswer(r io.Reader, start, end time.Time, step time.Duration) (*answer, error) {
	in := &metered{r: r}
	dec := json.NewDecoder(in)
	dec.UseNumber()
	a := &answer{}
	per := 1
	if step > 0 && end.After(start) {
		per = int(min(int64(end.Sub(start)/step), int64(maxAnswerSamples))) + 1
	}
	rd := reading{dec: dec, in: in, a: a, start: start, end: end, per: per}
	err := rd.object(func(key string) error {
		switch strings.ToLower(key) {
		case "status":
			return rd.decode(&a.status)
		case "error":
			return rd.decode(&a.errMsg)
		case "warnings":
			return rd.decode(&a.warnings)
		case "data":
			return rd.object(func(key string) error {
				switch strings.ToLower(key) {
				case "resulttype":
					return rd.decode(&a.resultType)
				case "result":
					return rd.result()
				}
				return rd.skip()
			})
		}
		return rd.skip()
	})
	if errors.Is(err, errLong) {
		a.bad = &unreadable{msg: fmt.Sprintf("the answer holds a value longer than is read of it (%d KiB)", rd.longest>>10)}
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := rd.token(); err != io.EOF {
		return nil, errNotJSON // more after the answer
	}
	return a, nil
}

// How much of the stream one value of an answer may take: a series, its
// labels and its steps, each sample in some 64 bytes; anything else.
const (
	sampleBytes = 64
	valueBytes  = 4 << 20
)

// metered is an answer as it is read, each step of the decoder allowed so
// many bytes more of it (left): what reading one value can make it hold.
type metered struct {
	r    io.Reader
	left int64
}

// errLong is a value longer than it is allowed of the stream.
var errLong = errors.New("a value longer than is read")

func (m *metered) Read(p []byte) (int, error) {
	if m.left <= 0 {
		return 0, errLong
	}
	if int64(len(p)) > m.left {
		p = p[:m.left]
	}
	n, err := m.r.Read(p)
	m.left -= int64(n)
	return n, err
}

// reading is an answer being read.
type reading struct {
	dec        *json.Decoder
	in         *metered
	a          *answer
	start, end time.Time
	per        int
	samples    int
	// labels is what the series' labels take, as labelCost counts.
	labels int
	// longest is the most a value read was allowed, as said when one was
	// longer.
	longest int64
}

// allow lets the decoder's next step read n bytes more of the stream.
func (rd *reading) allow(n int64) {
	rd.in.left, rd.longest = n, n
}

func (rd *reading) token() (json.Token, error) {
	rd.allow(valueBytes)
	return rd.dec.Token()
}

func (rd *reading) more() bool {
	rd.allow(valueBytes)
	return rd.dec.More()
}

func (rd *reading) decode(v any) error {
	rd.allow(valueBytes)
	return rd.dec.Decode(v)
}

// object reads a JSON object, or null, giving each key to f, which reads
// its value.
func (rd *reading) object(f func(string) error) error {
	t, err := rd.token()
	if err != nil {
		return notJSON(err)
	}
	if t == nil {
		return nil
	}
	if t != json.Delim('{') {
		return errNotJSON
	}
	for rd.more() {
		t, err := rd.token()
		if err != nil {
			return notJSON(err)
		}
		key, ok := t.(string)
		if !ok {
			return errNotJSON
		}
		if err := f(key); err != nil {
			return notJSON(err)
		}
	}
	_, err = rd.token() // }
	return notJSON(err)
}

// result reads data.result: a matrix's series, kept as readAnswer says,
// until the answer is too big or a sample can't be read; then, and once
// the result is known not to be a matrix, the rest is passed over. Each
// series is read whole, one at a time; what isn't of the form of a list of
// series isn't the API's.
func (rd *reading) result() error {
	t, err := rd.token()
	if err != nil || t == nil {
		return err
	}
	if t != json.Delim('[') {
		return errNotJSON
	}
	// A result given twice is the last, as json.Unmarshal reads it.
	rd.a.series, rd.a.bad, rd.a.big, rd.samples, rd.labels = nil, nil, "", 0, 0
	for rd.more() {
		if rd.a.bad != nil || rd.a.big != "" || rd.a.resultType != "" && rd.a.resultType != "matrix" {
			if err := rd.skip(); err != nil {
				return err
			}
			continue
		}
		if len(rd.a.series) == maxAnswerSeries {
			rd.a.big = fmt.Sprintf("the answer holds more than %d series", maxAnswerSeries)
			continue
		}
		s := Series{Samples: []Sample{}}
		one := struct {
			Metric map[string]string `json:"metric"`
			Values seriesValues      `json:"values"`
		}{Values: seriesValues{rd, &s}}
		rd.allow(int64(rd.per)*sampleBytes + valueBytes)
		if err := rd.dec.Decode(&one); err != nil {
			return err
		}
		if rd.labels += labelCost(one.Metric); rd.labels > maxAnswerLabels && rd.a.bad == nil && rd.a.big == "" {
			// Not too big for a shorter range, which holds the same
			// series: too many, or too wide, to read.
			rd.a.bad = &unreadable{msg: fmt.Sprintf("the answer's %d series hold more than %d MiB of labels: too many, or too wide, series to read, over any range", len(rd.a.series)+1, maxAnswerLabels>>20)}
		}
		if rd.a.bad == nil && rd.a.big == "" {
			s.Labels = one.Metric
			rd.a.series = append(rd.a.series, s)
		}
	}
	_, err = rd.token() // ]
	return err
}

// seriesValues reads a series' values, [[1730419200.5, "42"], ...], as
// [2]json.RawMessage would, into its series: a time, a JSON number read as
// written (ParseTime), then the value, a string or a number. The JSON is
// whole and valid: the decoder scanned it first.
type seriesValues struct {
	rd *reading
	s  *Series
}

func (v *seriesValues) UnmarshalJSON(b []byte) error {
	rd, s := v.rd, v.s
	// Values given twice are the last, as json.Unmarshal reads them.
	rd.samples -= len(s.Samples)
	s.Samples = s.Samples[:0]
	p := space(b, 0)
	if p < len(b) && b[p] == 'n' {
		return nil // null
	}
	if p >= len(b) || b[p] != '[' {
		return errNotJSON
	}
	for p = space(b, p+1); p < len(b) && b[p] != ']'; p = space(b, p) {
		if b[p] == ',' {
			p = space(b, p+1)
		}
		if p >= len(b) || b[p] != '[' {
			return errNotJSON
		}
		// The first two of its values, any more passed over.
		var pair [2][]byte
		n := 0
		for p = space(b, p+1); p < len(b) && b[p] != ']'; p = space(b, p) {
			if b[p] == ',' {
				p = space(b, p+1)
			}
			from := p
			if p = value(b, p); p == from {
				return errNotJSON
			}
			if n < 2 {
				pair[n] = b[from:p]
			}
			n++
		}
		p++
		if rd.a.bad != nil || rd.a.big != "" {
			return nil // refused whole: the rest isn't read
		}
		ts, f, ok := sample(pair)
		switch {
		case !ok:
			// Kept whole, to 64 KB: what the request carried is taken out
			// before it is cut (queryRange), or a long token's start stays.
			rd.a.bad = &unreadable{sample: [2]string{keepRaw(pair[0]), keepRaw(pair[1])}}
		case ts.Before(rd.start) || ts.After(rd.end):
			// Not asked for.
		case len(s.Samples) == rd.per:
			rd.a.bad = &unreadable{msg: fmt.Sprintf("a series holds more samples than the %d steps asked for", rd.per)}
		case rd.samples == maxAnswerSamples:
			rd.a.big = fmt.Sprintf("the answer holds more than %d samples", maxAnswerSamples)
		default:
			rd.samples++
			s.Samples = append(s.Samples, Sample{T: ts, V: f})
		}
	}
	return nil
}

// keepRaw is a sample's JSON as an error quotes it, 64 KB at most.
func keepRaw(b []byte) string { return string(b[:min(len(b), 64<<10)]) }

// sample is a value's time and number, from their JSON: the time a
// number, read as written (ParseTime), the value a string or a number.
func sample(pair [2][]byte) (time.Time, float64, bool) {
	t, v := pair[0], pair[1]
	if len(t) == 0 || t[0] != '-' && (t[0] < '0' || t[0] > '9') {
		return time.Time{}, 0, false
	}
	ts, err := ParseTime(string(t))
	if err != nil || len(v) == 0 {
		return time.Time{}, 0, false
	}
	var text string
	switch {
	case v[0] == '"' && bytes.IndexByte(v, '\\') < 0:
		text = string(v[1 : len(v)-1])
	case v[0] == '"':
		if json.Unmarshal(v, &text) != nil {
			return time.Time{}, 0, false
		}
	case v[0] == '-' || v[0] >= '0' && v[0] <= '9':
		text = string(v)
	default:
		return time.Time{}, 0, false
	}
	f, err := strconv.ParseFloat(text, 64)
	return ts, f, err == nil
}

// space is where the JSON in b goes on after the white space at p.
func space(b []byte, p int) int {
	for p < len(b) && (b[p] == ' ' || b[p] == '\t' || b[p] == '\n' || b[p] == '\r') {
		p++
	}
	return p
}

// value is where the JSON value at p in b, which is valid, ends.
func value(b []byte, p int) int {
	depth := 0
	for ; p < len(b); p++ {
		switch c := b[p]; c {
		case '"':
			for p++; p < len(b) && b[p] != '"'; p++ {
				if b[p] == '\\' {
					p++
				}
			}
			if depth == 0 {
				return p + 1
			}
		case '[', '{':
			depth++
		case ']', '}':
			if depth == 0 {
				return p
			}
			if depth--; depth == 0 {
				return p + 1
			}
		case ',':
			if depth == 0 {
				return p
			}
		case ' ', '\t', '\n', '\r':
			if depth == 0 {
				return p
			}
		}
	}
	return p
}

// skip passes over the next value whole, reading it a token at a time.
func (rd *reading) skip() error {
	t, err := rd.token()
	if err != nil {
		return err
	}
	d, ok := t.(json.Delim)
	if !ok || d == '}' || d == ']' {
		return nil
	}
	for depth := 1; depth > 0; {
		t, err := rd.token()
		if err != nil {
			return err
		}
		switch t {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
	}
	return nil
}

// notJSON is err as the answer not being JSON of the API's form, when it
// is a reading error, and not one of the body's own (dropped, too long).
func notJSON(err error) error {
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &syn), errors.As(err, &typ), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return errNotJSON
	}
	return err
}

// head keeps the first headKept bytes read through it, to say what an
// answer was; and counts them all, to say whether it was cut at max.
type head struct {
	r   io.Reader
	buf bytes.Buffer
	n   int64
}

func (h *head) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	if room := headKept - h.buf.Len(); room > 0 {
		h.buf.Write(p[:min(n, room)])
	}
	h.n += int64(n)
	return n, err
}
