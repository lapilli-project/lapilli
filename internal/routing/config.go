package routing

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"regexp/syntax"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// What a configuration may hold: the bounds content applies when it reads
// one, held again here so a Config built any other way is bounded as well.
const (
	MaxRoutes       = 2000
	MaxDepth        = 32
	MaxReceivers    = 5000
	MaxIntegrations = 64
	MaxIntervals    = 5000
	MaxInhibitRules = 2000
	MaxMatchers     = 20000
	maxEqual        = 64
	maxNames        = 256
)

// What a text or a pattern may be: bytes of a matcher's name and value, of a
// time interval's name and an inhibition rule's name, and of a route's key
// (its matchers and its ancestors', as Alertmanager writes them); the
// instructions of one regular expression's program, and of all of a
// configuration's (or of a silences file's); the time ranges, weekdays, days,
// months and years a configuration's time intervals list in all; and the
// shortest interval the model follows.
const (
	MaxMatcherText        = 4096
	MaxName               = 1024
	MaxRouteKey           = 64 << 10
	MaxRegexProgram       = 10000
	MaxConfigRegexProgram = 500000
	MaxTimeParts          = 20000
	MinInterval           = time.Second
)

// Alertmanager's own defaults for a route's options
// (dispatch.DefaultRouteOpts).
const (
	DefaultGroupWait      = 30 * time.Second
	DefaultGroupInterval  = 5 * time.Minute
	DefaultRepeatInterval = 4 * time.Hour
)

// A MatchOp is how a matcher compares a label's value.
type MatchOp string

// The four operators of Alertmanager's matchers.
const (
	OpEqual    MatchOp = "="
	OpNotEqual MatchOp = "!="
	OpRegexp   MatchOp = "=~"
	OpNotRegex MatchOp = "!~"
)

// A Matcher is one of Alertmanager's matchers: a label's name, an operator
// and a value. A label an alert doesn't have is the empty text, as it is
// for Alertmanager, so foo!="x" and foo=~".*" match an alert without foo.
// A regular expression is anchored, as Alertmanager anchors it.
type Matcher struct {
	Name  string
	Op    MatchOp
	Value string
	re    *regexp.Regexp
	cost  int // the instructions of its regular expression's program
}

// A RegexBudget is the instructions of regular expressions' programs that
// the matchers of one configuration (or one silences file) may add up to.
type RegexBudget struct{ left int }

// NewRegexBudget is a budget of MaxConfigRegexProgram instructions.
func NewRegexBudget() *RegexBudget { return &RegexBudget{left: MaxConfigRegexProgram} }

// NewMatcher makes a matcher; an operator that isn't one of the four, a
// regular expression that doesn't compile or whose program is over
// MaxRegexProgram instructions, or a name or value over MaxMatcherText
// bytes, is an error.
func NewMatcher(name, op, value string) (Matcher, error) {
	return NewMatcherBudget(nil, name, op, value)
}

// NewMatcherBudget is NewMatcher with the regular expression's program also
// taken from a budget (nil for none): it is an error when it is spent.
func NewMatcherBudget(b *RegexBudget, name, op, value string) (Matcher, error) {
	m := Matcher{Name: name, Op: MatchOp(op), Value: value}
	if len(name) > MaxMatcherText || len(value) > MaxMatcherText {
		return Matcher{}, fmt.Errorf("a matcher's name and value are %d bytes at most", MaxMatcherText)
	}
	switch m.Op {
	case OpEqual, OpNotEqual:
	case OpRegexp, OpNotRegex:
		expr := "^(?:" + value + ")$"
		parsed, err := syntax.Parse(expr, syntax.Perl)
		if err != nil {
			return Matcher{}, fmt.Errorf("matcher %s%s%q: %w", name, op, clipText(value), err)
		}
		prog, err := syntax.Compile(parsed.Simplify())
		if err != nil {
			return Matcher{}, fmt.Errorf("matcher %s%s%q: %w", name, op, clipText(value), err)
		}
		m.cost = len(prog.Inst)
		if m.cost > MaxRegexProgram {
			return Matcher{}, fmt.Errorf("matcher %s%s%q: its regular expression compiles to %d instructions, more than the %d Lapilli follows", name, op, clipText(value), m.cost, MaxRegexProgram)
		}
		if b != nil {
			if b.left -= m.cost; b.left < 0 {
				return Matcher{}, fmt.Errorf("the regular expressions of the matchers compile to more than %d instructions in all", MaxConfigRegexProgram)
			}
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return Matcher{}, fmt.Errorf("matcher %s%s%q: %w", name, op, clipText(value), err)
		}
		m.re = re
	default:
		return Matcher{}, fmt.Errorf("matcher on %q: %q isn't an operator", name, op)
	}
	if name == "" {
		return Matcher{}, errors.New("a matcher with no label name")
	}
	return m, nil
}

// Matches reports whether the labels satisfy the matcher.
func (m Matcher) Matches(labels map[string]string) bool {
	v := labels[m.Name]
	switch m.Op {
	case OpEqual:
		return v == m.Value
	case OpNotEqual:
		return v != m.Value
	case OpRegexp:
		return m.re != nil && m.re.MatchString(v)
	case OpNotRegex:
		return m.re != nil && !m.re.MatchString(v)
	}
	return false
}

func (m Matcher) String() string {
	return m.Name + string(m.Op) + strconv.Quote(m.Value)
}

// matchAll reports whether every matcher matches.
func matchAll(ms []Matcher, labels map[string]string) bool {
	for _, m := range ms {
		if !m.Matches(labels) {
			return false
		}
	}
	return true
}

// A Route is a node of the route tree as it is written: what it says of
// itself, inheriting nothing. NewConfig works out what each route is with.
type Route struct {
	// Receiver is the route's receiver; empty to take its parent's. The
	// root must name one.
	Receiver string
	Matchers []Matcher
	Continue bool
	// GroupBy is the labels to group by when GroupBySet is true (an empty
	// list groups all of the route's alerts together); GroupByAll is
	// group_by: ['...'], a group for each distinct label set. Unset, they
	// are the parent's.
	GroupBy    []string
	GroupBySet bool
	GroupByAll bool
	// GroupWait, GroupInterval and RepeatInterval are nil to take the
	// parent's. A route's mute and active time intervals aren't inherited,
	// as Alertmanager's aren't.
	GroupWait, GroupInterval, RepeatInterval *time.Duration
	MuteTimeIntervals                        []string
	ActiveTimeIntervals                      []string
	Routes                                   []*Route
}

// An Integration is one way a receiver sends: its kind (slack, pagerduty,
// webhook, ...), its index among the receiver's integrations of that kind,
// and whether it tells when an alert resolves.
type Integration struct {
	Kind         string
	Index        int
	SendResolved bool
}

// A Receiver is a receiver (Grafana: a contact point) and the integrations
// it sends with, in the order Alertmanager keeps them; none is a receiver
// that sends nothing.
type Receiver struct {
	Name         string
	Integrations []Integration
}

// An InhibitRule mutes the alerts that match its target matchers while an
// alert that matches its source matchers fires and agrees with it on every
// label in Equal (absent agreeing with absent). The older source_match,
// source_match_re, target_match and target_match_re are matchers too.
type InhibitRule struct {
	Name           string
	SourceMatchers []Matcher
	TargetMatchers []Matcher
	Equal          []string
}

// A Config is a routing configuration, resolved: build one with NewConfig.
type Config struct {
	root      *node
	nodes     []*node // in the order of a walk down the tree
	receivers map[string]*Receiver
	names     []string // the receivers' names, sorted
	intervals map[string][]TimeInterval
	inhibit   []InhibitRule
	grafana   bool
	warnings  []string
}

// Warnings are what reading the configuration changed without refusing it
// (a Grafana timing under a second read as one second).
func (c *Config) Warnings() []string { return slices.Clone(c.warnings) }

// Warn adds a warning, at most 20 of them, each of 300 bytes at most.
func (c *Config) Warn(msg string) {
	if len(c.warnings) < 20 && !slices.Contains(c.warnings, msg) {
		c.warnings = append(c.warnings, clipTo(msg, 300))
	}
}

// clipTo cuts text to n bytes at a character's edge.
func clipTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}

func clipText(s string) string { return clipTo(s, 80) }

// A node is a route with what it inherits worked out.
type node struct {
	index                                    int
	parent                                   *node
	receiver                                 string
	matchers                                 []Matcher
	cont                                     bool
	groupBy                                  []string // sorted
	groupByAll                               bool
	groupWait, groupInterval, repeatInterval time.Duration
	mute, active                             []string
	children                                 []*node
	key                                      string
	short                                    string
}

// RouteInfo is a route as it works, inheritance applied.
type RouteInfo struct {
	// Index numbers the routes of the tree in the order of a walk down it,
	// the root 0.
	Index    int
	Receiver string
	// Key is the route's place in the tree as Alertmanager writes it in a
	// group's key: each route's matchers, from the root, joined by /. ID is
	// the short key a Notification's Group starts with: the route's place
	// among its siblings at each level, from the root (0, 0.2, 0.2.1).
	Key, ID                                  string
	GroupBy                                  []string
	GroupByAll                               bool
	GroupWait, GroupInterval, RepeatInterval time.Duration
	MuteTimeIntervals, ActiveTimeIntervals   []string
}

// NewConfig resolves a configuration: it works out what each route
// inherits, as Alertmanager does (dispatch.newRoute), checks the receivers
// and time intervals the routes name exist, and applies the bounds. grafana
// says the receivers are Grafana's contact points, whose kinds include
// oncall among the paging ones.
func NewConfig(root *Route, receivers []Receiver, intervals map[string][]TimeInterval, inhibit []InhibitRule, grafana bool) (*Config, error) {
	if root == nil {
		return nil, errors.New("no route")
	}
	if root.Receiver == "" {
		return nil, errors.New("the root route has no receiver")
	}
	if len(receivers) > MaxReceivers {
		return nil, fmt.Errorf("more than %d receivers", MaxReceivers)
	}
	if len(intervals) > MaxIntervals {
		return nil, fmt.Errorf("more than %d time intervals", MaxIntervals)
	}
	if len(inhibit) > MaxInhibitRules {
		return nil, fmt.Errorf("more than %d inhibition rules", MaxInhibitRules)
	}
	c := &Config{receivers: map[string]*Receiver{}, intervals: intervals, grafana: grafana}
	for i := range receivers {
		r := receivers[i]
		if len(r.Integrations) > MaxIntegrations {
			return nil, fmt.Errorf("receiver %q: more than %d integrations", r.Name, MaxIntegrations)
		}
		if _, dup := c.receivers[r.Name]; dup {
			return nil, fmt.Errorf("receiver %q is defined twice", r.Name)
		}
		c.receivers[r.Name] = &r
	}
	c.names = slices.Sorted(maps.Keys(c.receivers))
	matchers := 0
	regex, parts := 0, 0
	for name, tis := range intervals {
		if len(name) > MaxName {
			return nil, fmt.Errorf("a time interval's name is %d bytes at most", MaxName)
		}
		for _, ti := range tis {
			if parts += ti.Parts(); parts > MaxTimeParts {
				return nil, fmt.Errorf("the time intervals list more than %d ranges, weekdays, days, months and years in all", MaxTimeParts)
			}
		}
	}
	var build func(r *Route, parent *node, depth int, short string) (*node, error)
	build = func(r *Route, parent *node, depth int, short string) (*node, error) {
		if len(c.nodes) >= MaxRoutes {
			return nil, fmt.Errorf("more than %d routes", MaxRoutes)
		}
		if depth > MaxDepth {
			return nil, fmt.Errorf("routes more than %d levels deep", MaxDepth)
		}
		n := &node{index: len(c.nodes), parent: parent, cont: r.Continue, short: short}
		c.nodes = append(c.nodes, n)
		// Create the defaults, and overwrite them with what the route says.
		if parent == nil {
			n.groupWait, n.groupInterval, n.repeatInterval = DefaultGroupWait, DefaultGroupInterval, DefaultRepeatInterval
		} else {
			n.receiver, n.groupBy, n.groupByAll = parent.receiver, parent.groupBy, parent.groupByAll
			n.groupWait, n.groupInterval, n.repeatInterval = parent.groupWait, parent.groupInterval, parent.repeatInterval
		}
		if r.Receiver != "" {
			n.receiver = r.Receiver
		}
		if _, ok := c.receivers[n.receiver]; !ok {
			return nil, fmt.Errorf("route %d: receiver %q isn't defined", n.index, n.receiver)
		}
		if r.GroupBySet {
			n.groupBy = slices.Clone(r.GroupBy)
			sort.Strings(n.groupBy)
			n.groupBy = slices.Compact(n.groupBy)
			n.groupByAll = false
		} else if r.GroupByAll {
			n.groupByAll = true
		}
		if r.GroupWait != nil {
			n.groupWait = *r.GroupWait
		}
		if r.GroupInterval != nil {
			n.groupInterval = *r.GroupInterval
		}
		if r.RepeatInterval != nil {
			n.repeatInterval = *r.RepeatInterval
		}
		if n.groupInterval <= 0 || n.repeatInterval <= 0 || n.groupWait < 0 {
			return nil, fmt.Errorf("route %d: group_interval and repeat_interval must be more than zero", n.index)
		}
		// Alertmanager follows a timing of any length; the model works out a
		// flush for each interval, and a millisecond's a week's is a billion.
		for _, t := range []struct {
			name string
			d    time.Duration
			zero bool
		}{{"group_wait", n.groupWait, true}, {"group_interval", n.groupInterval, false}, {"repeat_interval", n.repeatInterval, false}} {
			if t.d < MinInterval && !(t.zero && t.d == 0) {
				return nil, fmt.Errorf("route %d: %s is %s: Alertmanager would accept it, but Lapilli doesn't model timings under %s; set %s or more", n.index, t.name, t.d, MinInterval, MinInterval)
			}
		}
		if len(r.MuteTimeIntervals) > maxNames || len(r.ActiveTimeIntervals) > maxNames {
			return nil, fmt.Errorf("route %d names more than %d time intervals", n.index, maxNames)
		}
		for _, name := range append(slices.Clone(r.MuteTimeIntervals), r.ActiveTimeIntervals...) {
			if _, ok := intervals[name]; !ok {
				return nil, fmt.Errorf("route %d: time interval %q isn't defined", n.index, clipText(name))
			}
		}
		n.mute, n.active = dedupe(r.MuteTimeIntervals), dedupe(r.ActiveTimeIntervals)
		if matchers += len(r.Matchers); matchers > MaxMatchers {
			return nil, fmt.Errorf("more than %d matchers", MaxMatchers)
		}
		for _, m := range r.Matchers {
			if regex += m.cost; regex > MaxConfigRegexProgram {
				return nil, fmt.Errorf("the regular expressions of the matchers compile to more than %d instructions in all", MaxConfigRegexProgram)
			}
		}
		n.matchers = slices.Clone(r.Matchers)
		slices.SortStableFunc(n.matchers, func(a, b Matcher) int {
			return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Value, b.Value), strings.Compare(string(a.Op), string(b.Op)))
		})
		n.key = matcherKey(n.matchers)
		if parent != nil {
			n.key = parent.key + "/" + n.key
		}
		if len(n.key) > MaxRouteKey {
			return nil, fmt.Errorf("route %d: its matchers, with its ancestors', are more than %d bytes", n.index, MaxRouteKey)
		}
		for ci, cr := range r.Routes {
			ch, err := build(cr, n, depth+1, short+"."+strconv.Itoa(ci))
			if err != nil {
				return nil, err
			}
			n.children = append(n.children, ch)
		}
		return n, nil
	}
	var err error
	if c.root, err = build(root, nil, 0, "0"); err != nil {
		return nil, err
	}
	for i, r := range inhibit {
		if len(r.Equal) > maxEqual {
			return nil, fmt.Errorf("inhibition rule %d: more than %d equal labels", i, maxEqual)
		}
		if len(r.Name) > MaxName {
			return nil, fmt.Errorf("inhibition rule %d: a name of %d bytes at most", i, MaxName)
		}
		for _, ms := range [][]Matcher{r.SourceMatchers, r.TargetMatchers} {
			if matchers += len(ms); matchers > MaxMatchers {
				return nil, fmt.Errorf("more than %d matchers", MaxMatchers)
			}
			for _, m := range ms {
				if regex += m.cost; regex > MaxConfigRegexProgram {
					return nil, fmt.Errorf("the regular expressions of the matchers compile to more than %d instructions in all", MaxConfigRegexProgram)
				}
			}
		}
	}
	c.inhibit = slices.Clone(inhibit)
	return c, nil
}

// dedupe is a list of names without the repeats, in its order.
func dedupe(names []string) []string {
	if len(names) < 2 {
		return names
	}
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// GroupKey is a notification's group key written as Alertmanager writes it:
// the route's matchers from the root, a colon and the group's labels,
// {}/{severity="critical"}:{alertname="X"}. A group's labels longer than
// 1,000 bytes were cut when it was made (a hash of the rest ends them).
func (c *Config) GroupKey(nt Notification) string {
	_, labels, _ := strings.Cut(nt.Group, ":")
	if nt.Route >= 0 && nt.Route < len(c.nodes) {
		return c.nodes[nt.Route].key + ":" + labels
	}
	return ownKey(nt.Receiver) + ":" + labels
}

// matcherKey is a route's matchers as Alertmanager writes them in a group's
// key: {a="b", c=~"d"}.
func matcherKey(ms []Matcher) string {
	parts := make([]string, len(ms))
	for i, m := range ms {
		parts[i] = m.String()
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// Routes are the tree's routes, inheritance applied, in the order of a walk
// down it.
func (c *Config) Routes() []RouteInfo {
	out := make([]RouteInfo, len(c.nodes))
	for i, n := range c.nodes {
		out[i] = RouteInfo{Index: n.index, Receiver: n.receiver, Key: n.key, ID: n.short, GroupBy: slices.Clone(n.groupBy), GroupByAll: n.groupByAll,
			GroupWait: n.groupWait, GroupInterval: n.groupInterval, RepeatInterval: n.repeatInterval,
			MuteTimeIntervals: slices.Clone(n.mute), ActiveTimeIntervals: slices.Clone(n.active)}
	}
	return out
}

// InhibitRules are the configuration's inhibition rules.
func (c *Config) InhibitRules() []InhibitRule { return slices.Clone(c.inhibit) }

// Receiver is the receiver by its name.
func (c *Config) Receiver(name string) (Receiver, bool) {
	r, ok := c.receivers[name]
	if !ok {
		return Receiver{}, false
	}
	return Receiver{Name: r.Name, Integrations: slices.Clone(r.Integrations)}, true
}

// Receivers are the receivers' names, sorted.
func (c *Config) Receivers() []string { return slices.Clone(c.names) }

// Match reports the routes an alert with these labels is sent to, as their
// indexes (Routes), in Alertmanager's order: the tree is walked depth first,
// left to right; a route none of whose children match is itself a match, and
// a matching child ends the search among its siblings unless it continues.
// No route at all if the root's matchers don't match: the root has none in
// Alertmanager, and any in a Grafana policy is ignored.
func (c *Config) Match(labels map[string]string) []int {
	var walk func(n *node) []int
	walk = func(n *node) []int {
		if n.parent != nil && !matchAll(n.matchers, labels) {
			return nil
		}
		var all []int
		for _, ch := range n.children {
			m := walk(ch)
			all = append(all, m...)
			if m != nil && !ch.cont {
				break
			}
		}
		if len(all) == 0 {
			all = append(all, n.index)
		}
		return all
	}
	return walk(c.root)
}

// Alertmanager's integrations in the order its Receiver struct holds them,
// which is the order it builds a receiver's integrations in
// (config/receiver/receiver.go, cmd/alertmanager's buildReceiverIntegrations).
var kindOrder = []string{"discord", "email", "incidentio", "pagerduty", "slack", "webhook", "opsgenie", "wechat",
	"pushover", "victorops", "sns", "telegram", "webex", "msteams", "msteamsv2", "jira", "rocketchat", "mattermost"}

// KindOrder sorts integration kinds as Alertmanager builds a receiver's
// integrations: its known kinds in its order, any other after, by name.
func KindOrder(a, b string) int {
	ia, ib := slices.Index(kindOrder, a), slices.Index(kindOrder, b)
	switch {
	case ia >= 0 && ib >= 0:
		return ia - ib
	case ia >= 0:
		return -1
	case ib >= 0:
		return 1
	}
	return strings.Compare(a, b)
}

// DefaultSendResolved is whether an Alertmanager integration of a kind sends
// resolves when its configuration doesn't say (the VSendResolved of each
// kind's default configuration, config/notifiers.go before 0.35 and
// notify/<kind>/config.go after): off for Slack, email, WeChat and
// Rocket.Chat, on for the others.
func DefaultSendResolved(kind string) bool {
	switch kind {
	case "slack", "email", "wechat", "rocketchat":
		return false
	}
	return true
}

// The kinds that page someone: Alertmanager's pagerduty, opsgenie, victorops,
// incidentio and pushover, and Grafana's oncall as well. Every other kind is a message.
var pagingKinds = []string{"pagerduty", "opsgenie", "victorops", "incidentio", "pushover"}

// PagingKind reports whether an integration kind pages someone. grafana
// adds Grafana OnCall.
func PagingKind(kind string, grafana bool) bool {
	return slices.Contains(pagingKinds, kind) || grafana && kind == "oncall"
}

// ParseDuration reads a duration as Alertmanager and Grafana write them
// (Prometheus's model.ParseDuration): a number of y, w, d, h, m, s and ms,
// each unit once and in that order (1h30m, 90s, 5m), or 0. Negative or empty
// text isn't one.
func ParseDuration(s string) (time.Duration, error) {
	if s == "0" {
		return 0, nil
	}
	if s == "" {
		return 0, errors.New("empty duration")
	}
	orig := s
	unit := map[string]time.Duration{
		"y": 365 * 24 * time.Hour, "w": 7 * 24 * time.Hour, "d": 24 * time.Hour,
		"h": time.Hour, "m": time.Minute, "s": time.Second, "ms": time.Millisecond,
	}
	order := []string{"y", "w", "d", "h", "m", "s", "ms"}
	var total time.Duration
	last := -1
	for s != "" {
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == 0 || i > 9 {
			return 0, fmt.Errorf("not a duration: %q", orig)
		}
		n, _ := strconv.Atoi(s[:i])
		s = s[i:]
		j := 0
		for j < len(s) && (s[j] < '0' || s[j] > '9') {
			j++
		}
		u := s[:j]
		s = s[j:]
		k := slices.Index(order, u)
		if k < 0 || k <= last {
			return 0, fmt.Errorf("not a duration: %q", orig)
		}
		last = k
		d := time.Duration(n) * unit[u]
		if d/unit[u] != time.Duration(n) || total+d < total || total+d > 100*365*24*time.Hour {
			return 0, fmt.Errorf("duration %q is too long", orig)
		}
		total += d
	}
	return total, nil
}
