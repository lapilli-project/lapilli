// Package serve is lapilli serve: it keeps replaying the rules over the
// hours since it last did, records what it finds in a ledger, and shows the
// stones in a web UI, an API and Prometheus metrics.
package serve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/verdict"
)

// Config is how lapilli serve was started.
type Config struct {
	// Where the rules are: a file or directory, the API's loaded rules, a
	// Grafana; or, with Alerts, no rules but the ALERTS series.
	RulesPath             string
	Loaded                bool
	Grafana, GrafanaToken string
	Alerts                bool
	RuleRE                *regexp.Regexp

	Incidents string // the incidents file, read again each run
	Impact    string
	ImpactFor time.Duration

	// Options are the judging settings from the command line; the UI can
	// change Lead, Match, RefireWithin and the noisy thresholds.
	Options verdict.Options
	Step    time.Duration
	Loc     *time.Location

	Ledger   string // the directory the ledger is kept in
	Every    time.Duration
	Settle   time.Duration
	Backfill time.Duration
	Keep     time.Duration // 0 keeps everything
	Watch    time.Duration
	Digest   Weekly
	Slack    string // a file holding a Slack incoming webhook's URL
	Timeout  time.Duration
	Parallel int

	Listen     string
	UserHeader string
	ReadOnly   bool
	Version    string

	// Roles: with Editors or Admins (users or groups, "*" for everyone),
	// the rest may only look. GroupsHeader names the header a sign-in
	// proxy lists the reader's groups in.
	Editors, Admins []string
	GroupsHeader    string

	// What happens (tasks, incidents, quiet windows, comments) is told to
	// the webhook whose URL is in NotifyWebhook, as JSON, and with
	// NotifySlack to Slack; their links are under ExternalURL. Lang is the
	// language of what goes to Slack.
	NotifyWebhook string
	NotifySlack   bool
	ExternalURL   string
	Lang          string
}

// Weekly is a time of the week.
type Weekly struct {
	Day          time.Weekday
	Hour, Minute int
}

// ParseWeekly reads mon@09:00.
func ParseWeekly(s string) (Weekly, error) {
	day, at, ok := strings.Cut(strings.ToLower(s), "@")
	days := []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}
	i := slices.Index(days, strings.TrimSpace(day))
	h, m, ok2 := strings.Cut(at, ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || !ok2 || i < 0 || err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return Weekly{}, fmt.Errorf("%q: want a day and a time, like mon@09:00", s)
	}
	return Weekly{time.Weekday(i), hh, mm}, nil
}

// Last is the latest time of the week at or before t, in loc.
func (w Weekly) Last(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	y, m, d := t.Date()
	at := time.Date(y, m, d, w.Hour, w.Minute, 0, 0, loc)
	at = at.AddDate(0, 0, -((int(t.Weekday()) - int(w.Day) + 7) % 7))
	if at.After(t) {
		at = at.AddDate(0, 0, -7)
	}
	return at
}

func (w Weekly) String() string {
	return fmt.Sprintf("%s@%02d:%02d", strings.ToLower(w.Day.String()[:3]), w.Hour, w.Minute)
}

// Server is lapilli serve, running.
type Server struct {
	cfg Config
	h   replay.History
	l   *ledger.Ledger
	log *slog.Logger
	now func() time.Time

	// wake asks the worker to look at its jobs now.
	wake chan struct{}
	// rejudge is set when the settings that decide the stones changed.
	rejudge atomic.Bool
	// slackTried is when the worker last tried to post the digest.
	slackTried time.Time
	status     status
	cache      cache
	own        ownersCache
	ready      atomic.Bool
}

// status is what the worker is doing, for the UI: a text to be shown in
// the reader's language, and what goes in it.
type status struct {
	sync.Mutex
	doing   string
	args    []any
	errWhat string  // what failed, also a text to translate
	judged  float64 // how far judging everything again has got, 0 to 1
	err     string
	started time.Time
	runs    map[string]int // by kind and result, since start
	notices map[string]int // by where to and result, since start
	took    time.Duration  // the last run
}

func (st *status) set(doing string, args ...any) {
	st.Lock()
	st.doing, st.args, st.started = doing, args, time.Now()
	st.Unlock()
}

// New opens the ledger and readies the server; h is where the history is.
func New(ctx context.Context, cfg Config, h replay.History, log *slog.Logger) (*Server, error) {
	if err := os.MkdirAll(cfg.Ledger, 0o750); err != nil {
		return nil, fmt.Errorf("--ledger: %w", err)
	}
	l, err := ledger.Open(ctx, filepath.Join(cfg.Ledger, "ledger.db"), cfg.Loc)
	if err != nil {
		return nil, fmt.Errorf("--ledger: %w", err)
	}
	s := &Server{cfg: cfg, h: h, l: l, log: log, now: time.Now, wake: make(chan struct{}, 1)}
	s.status.runs, s.status.notices = map[string]int{}, map[string]int{}
	if !known(s.cfg.Lang) {
		s.cfg.Lang = "en"
	}
	l.TellTo(s.dests())
	return s, nil
}

// Close closes the ledger.
func (s *Server) Close() error { return s.l.Close() }

// Run serves the UI and keeps the ledger until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("--listen: %w", err)
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	s.log.Info("serving", "listen", ln.Addr().String(), "ledger", s.cfg.Ledger)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	// The rules are read before the first run, which replays them.
	raw := ""
	if !s.cfg.Alerts {
		if err := s.look(ctx, &raw); err != nil {
			s.fail("reading the rules", err)
		}
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.work(ctx)
	}()
	if !s.cfg.Alerts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.watch(ctx, raw)
		}()
	}
	if len(s.dests()) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.notify(ctx)
		}()
	}
	var serr error
	select {
	case <-ctx.Done():
	case serr = <-served:
	}
	shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shut)
	wg.Wait()
	if errors.Is(serr, http.ErrServerClosed) {
		serr = nil
	}
	return serr
}

// Settings the UI can change, by name, with the flag each stands for.
var settable = []struct{ key, flag string }{
	{"lead", "--lead"}, {"match", "--match"}, {"refire", "--refire"},
	{"noisy-firings", "--noisy-firings"}, {"noisy-precision", "--noisy-precision"},
}

// options are the judging settings in effect: the command line's, with
// those changed in the UI.
func (s *Server) options(ctx context.Context) (verdict.Options, error) {
	o := s.cfg.Options
	set, err := s.l.Settings(ctx)
	if err != nil {
		return o, err
	}
	for k, v := range set {
		if err := apply(&o, k, v.Value); err != nil {
			s.log.Warn("a setting from the UI doesn't parse; the command line's is used", "setting", k, "err", err)
		}
	}
	return o, nil
}

// apply sets one judging setting from its text.
func apply(o *verdict.Options, key, v string) error {
	var err error
	switch key {
	case "lead":
		o.Lead, err = ParseDuration(v)
	case "refire":
		o.RefireWithin, err = ParseDuration(v)
	case "match":
		// "none" is no labels: an empty value goes back to the command
		// line's.
		o.Match = nil
		for _, m := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
			if m != "none" {
				o.Match = append(o.Match, m)
			}
		}
	case "noisy-firings":
		o.NoisyFirings, err = strconv.Atoi(v)
		if err == nil && o.NoisyFirings < 1 {
			err = errors.New("want 1 or more")
		}
	case "noisy-precision":
		var p float64
		p, err = strconv.ParseFloat(strings.TrimSuffix(v, "%"), 64)
		if strings.HasSuffix(v, "%") {
			p /= 100
		}
		if err == nil && (p <= 0 || p > 1) {
			err = errors.New("want a share like 20% or 0.2")
		}
		o.NoisyPrecision = p
	default:
		err = errors.New("not a setting")
	}
	return err
}

// show is a setting as it is in o, as text.
func show(o verdict.Options, key string) string {
	switch key {
	case "lead":
		return verdict.Duration(o.Lead)
	case "refire":
		return verdict.Duration(o.RefireWithin)
	case "match":
		if len(o.Match) == 0 {
			return "none"
		}
		return strings.Join(o.Match, ", ")
	case "noisy-firings":
		n := o.NoisyFirings
		if n == 0 {
			n = verdict.NoisyFirings
		}
		return strconv.Itoa(n)
	case "noisy-precision":
		p := o.NoisyPrecision
		if p == 0 {
			p = verdict.NoisyPrecision
		}
		return strconv.FormatFloat(100*p, 'f', -1, 64) + "%"
	}
	return ""
}

// ParseDuration takes Go durations and, as Prometheus does, days: 7d, 1d12h.
func ParseDuration(s string) (time.Duration, error) {
	if d, rest, ok := strings.Cut(s, "d"); ok {
		n, err := strconv.Atoi(d)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		more := time.Duration(0)
		if rest != "" {
			if more, err = time.ParseDuration(rest); err != nil {
				return 0, err
			}
		}
		return time.Duration(n)*24*time.Hour + more, nil
	}
	return time.ParseDuration(s)
}
