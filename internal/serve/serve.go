// Package serve is lapilli serve: it keeps replaying the rules over the
// hours since it last did, records what it finds in a ledger, and shows the
// stones in a web UI, an API and Prometheus metrics.
package serve

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

	"lapilli.dev/internal/cloud"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/oncall"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
	"lapilli.dev/internal/verdict"
)

// Config is how lapilli serve was started.
type Config struct {
	// Where the rules are: a file or directory, the API's loaded rules, a
	// Grafana; or, with Alerts, no rules but the ALERTS series.
	RulesPath string
	// RulesHistory is a directory of earlier versions of the rules at
	// RulesPath, added to a ledger that has none (docs/design/history.md).
	RulesHistory          string
	Loaded                bool
	Grafana, GrafanaToken string
	// Loki is a Loki whose ruler's rules are replayed, beside the rest,
	// against it (docs/design/loki.md): its URL, the file holding its
	// token, and its tenant. LokiRules is where its rule files are, read
	// again every minute; LokiLoaded reads them from its ruler's API.
	Loki, LokiToken, LokiTenant, LokiRules string
	LokiLoaded                             bool
	// Ruler is a managed Prometheus ruler the rules are read from, and
	// changed through (docs/design/rulers.md): its URL, the file holding a
	// token that reads it (Mimir, Cortex), and its tenant, sent as
	// X-Scope-OrgID. Auth is the cloud whose credentials read an Amazon or
	// an Azure one (--auth).
	Ruler, RulerToken, RulerTenant, Auth string
	// OnCall are the on-call schedules --on-call reads, and their people:
	// shifts written on-call=NAME take turns by them
	// (docs/design/inputs.md).
	OnCall *oncall.Config
	// OnCallClient fetches their feeds; tests give a stand-in.
	OnCallClient *http.Client
	// Alarms are a cloud's own alarms, read through its API beside any
	// other place (docs/design/alarms.md): each named by the source its
	// history is copied in under. AlarmsEvery is how often they are read.
	Alarms      []AlarmsAt
	AlarmsEvery time.Duration
	Alerts      bool
	RuleRE      *regexp.Regexp

	Incidents string // the incidents file, read again each run
	Impact    string
	ImpactFor time.Duration
	// RunbookAnnotations are the annotations a rule's runbook is read
	// from, the first set: nil for content.RunbookAnnotations.
	RunbookAnnotations []string

	// Options are the judging settings from the command line; the UI can
	// change Lead, Match, RefireWithin and the noisy thresholds.
	Options verdict.Options
	Step    time.Duration
	// Late is how late each rule's metrics are taken to arrive (--late).
	Late rules.Lateness
	Loc  *time.Location

	Ledger string // the directory the ledger is kept in
	// CheckReplay sets the replay beside what really fired, read from
	// CheckSeries or ALERTS and GRAFANA_ALERTS; a rule differs as
	// CheckDiffers says (docs/design/fidelity.md).
	CheckReplay  bool
	CheckSeries  []string
	CheckDiffers Differs
	Every        time.Duration
	Settle       time.Duration
	Backfill     time.Duration
	Keep         time.Duration // 0 keeps everything
	Watch        time.Duration
	Digest       Weekly
	Slack        string // a file holding a Slack incoming webhook's URL
	Timeout      time.Duration
	Parallel     int

	Listen string
	// MetricsListen, when set, is where the health checks and the metrics
	// are served too, alone.
	MetricsListen string
	// IncidentsListen, when set, is where incidents can be written with the
	// token in IncidentsToken, and firings recorded elsewhere with the one
	// in FiringsToken, and nothing else can be done.
	IncidentsListen string
	IncidentsToken  string
	FiringsToken    string
	// TasksToken lets a job keep tasks in step with a tracker on the same
	// port: read them, and set their link and status (docs/design/trackers.md).
	TasksToken string
	// SourceQuiet is how long a source may go without firings copied in
	// before it is told of; 0 for sourceQuiet.
	SourceQuiet time.Duration
	// NotifyListen, when set, is where notifications are taken as alert
	// managers sent them, and nothing else: with the token in the file
	// NotifyToken, Grafana's signature with the secret in the file
	// NotifyGrafanaSecret, or SNS's signature from the topics in NotifySNS.
	NotifyListen        string
	NotifyToken         string
	NotifyGrafanaSecret string
	NotifySNS           []string

	UserHeader string
	// AllowedHosts are host names to answer to besides those always
	// answered (see hostOK), such as the name a proxy passes; "*" for any.
	AllowedHosts []string
	ReadOnly     bool
	Version      string
	// EveryoneAdmin lets everyone who reaches the UI and the API change
	// anything, as an admin, with no roles given and Listen beyond
	// loopback. Without roles, and without it, only a server listening on
	// loopback alone takes changes: beyond it, everyone may only look.
	EveryoneAdmin bool

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
	// TeamDigests are files holding the incoming webhooks that take a
	// team's own weekly digest, by the team's name.
	TeamDigests map[string]string
	// ReportDests are files holding the Slack-style incoming webhooks the
	// reports people schedule can go to, by the names they pick them by.
	ReportDests map[string]string

	// MCPCompare is who an agent must be acting for to replay a change to
	// the rules over MCP, which runs PromQL of its own: "editors",
	// "admins", or "" for no one.
	MCPCompare string

	// CheckContent checks what each rule says when it fires, and how it is
	// written (docs/design/content.md): against Severities, the team's,
	// when given, and with ScrapeInterval, or else the one Prometheus
	// gives, for the range check.
	CheckContent   bool
	Severities     []string
	ScrapeInterval time.Duration

	// MCPPropose is who an agent must be acting for to propose a change to
	// a rule over MCP: "editors", "admins", or "" for no one. An agent
	// never approves one.
	MCPPropose string

	// Changes turns changes to the rules on (docs/design/changes.md):
	// approved proposals are handed to the recipes holding ChangesToken
	// (a file), on IncidentsListen, or applied through Grafana's API with
	// the token in the file ChangesGrafanaToken. Approvers approve, or the
	// admins when none are given; Approvals is how many must (1 or 2).
	// ChangesLimits bound how many are carried out; while the file
	// ChangesPaused is there, none is.
	Changes             bool
	Approvers           []string
	Approvals           int
	ChangesToken        string
	ChangesGrafanaToken string
	ChangesLimits       Limits
	ChangesPaused       string
	// ChangesIn are the namespaces, or Azure rule groups (RG/NAME), of the
	// ruler Lapilli may change: a name, or a prefix ending in *. Nothing in
	// a ruler is changed without it. ChangesManaged are the tags,
	// KEY=VALUE or KEY=*, that mean another tool keeps an Amazon namespace
	// or an Azure rule group.
	ChangesIn, ChangesManaged []string
	// The write credentials, one a kind: the file holding a token that may
	// write rules (Mimir, Cortex), a role the pod's identity assumes to
	// write (Amazon's rules and CloudWatch's alarms), a user-assigned
	// identity's client ID to write as (Azure's), a service account the
	// pod's identity impersonates (Google Cloud's alerting policies), an
	// OCI config file and the API key it names (OCI's alarms). Without
	// one, a Mimir's or a Cortex's writes are sent with no token, and a
	// cloud's with the pod's own identity.
	ChangesRulerToken, ChangesAWSRole, ChangesAzureClientID string
	ChangesGoogleServiceAccount, ChangesOCIConfig           string
	// RulerTransport carries the ruler's and the alarms' requests: nil but
	// in tests.
	RulerTransport http.RoundTripper
}

// AlarmsAt is an --alarms: the source a cloud's alarms are named by, and
// the URL they are read from.
type AlarmsAt struct{ Source, URL string }

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

	// open says that, with no roles, everyone is an admin: it listens on
	// loopback alone, or EveryoneAdmin says so. Run says where it is bound.
	open atomic.Bool
	// ledgerID names the ledger, made at random when it is first opened:
	// the change recipes' branches carry it.
	ledgerID string

	// onCall is how each on-call schedule was last read.
	onCall onCallState

	// wake asks the worker to look at its jobs now.
	wake chan struct{}
	// boot is this process's, in every ETag: the ledger's generation starts
	// again with it, and a new release's pages differ from the last's.
	boot string
	// rejudge is set when the settings that decide the stones changed.
	rejudge atomic.Bool
	// comparing is held while an agent's proposed change is replayed.
	comparing sync.Mutex
	// heavy is the one turn there is for a request that takes hundreds of
	// megabytes: every firing of a month, the settings tried over a week.
	heavy chan struct{}
	// refused is set once a request to an unknown host name was logged.
	refused atomic.Bool
	// tokenWarned is set once an incidents token that can't be used was
	// logged; tokenRate is how fast the token may write.
	tokenWarned atomic.Bool
	tokenRate   *limiter
	tokenBusy   chan struct{}
	// firingsWarned, firingsRate and firingsBusy are the firings token's.
	firingsWarned atomic.Bool
	firingsRate   *limiter
	firingsBusy   chan struct{}
	// tasksWarned, tasksRate and tasksBusy are the tasks token's.
	tasksWarned atomic.Bool
	tasksRate   *limiter
	tasksBusy   chan struct{}
	// port is what the notifications port keeps.
	port *notifyPort
	// paged keeps the notifications of the weeks shown, tied to firings.
	paged pagedCache
	// passedOver is how many ids the incidents file has that the ledger
	// holds from people, as the last read of it found.
	passedOver atomic.Int64
	// slackTried is when the worker last tried to post the digest.
	slackTried time.Time
	status     status
	cache      cache
	own        ownersCache
	grantsC    grantsCache
	creds      creditCache
	hours      hoursCache
	outs       outcomesCache
	// saidRules are the rules as last read, for what they say.
	saidRules saidRules
	ready     atomic.Bool
	// changes is what changes to the rules keep while serve runs.
	changes *changesState
	// ruler reads --ruler, with the credentials that read it, and
	// rulerWrites are those that write to it.
	ruler       *source.Ruler
	rulerWrites source.Creds
	// alarms read each --alarms, with the credentials that read it, and
	// alarmWrites are those that write to it, by source.
	alarms      []*source.Alarms
	alarmWrites map[string]source.Creds
	// dataSources are the scrape intervals --grafana's data sources say
	// they have, and their types, as read with its rules, an hour at most
	// before; of uids.
	dataSources struct {
		sync.Mutex
		ds    rules.DataSources
		kinds map[string]string
		uids  []string
		at    time.Time
	}
	// places are where --loki's ruler runs each of its groups, as read
	// with its rules; notes, what was last said of its rule files.
	places struct {
		sync.Mutex
		m     map[rules.Place]time.Duration
		notes string
	}
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
	// refused are the notifications the notifications port was sent and
	// couldn't keep, by source, since start.
	refused map[string]int
	took    time.Duration // the last run
	// checkEmpty are the series that held nothing over checkDay, the day
	// the replay was last set beside what really fired.
	checkEmpty []string
	checkDay   time.Time
	// checkHeld are the days whose check waited for a cloud's alarms'
	// history: once checked, they are told of as a day just checked is.
	checkHeld map[int64]bool
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
	s := &Server{cfg: cfg, h: h, l: l, log: log, now: time.Now, wake: make(chan struct{}, 1), boot: strconv.FormatInt(time.Now().UnixNano(), 36),
		heavy: make(chan struct{}, 1), tokenRate: &limiter{rate: 20, burst: 40}, tokenBusy: make(chan struct{}, 1),
		// A job sends a source's firings a period at a time, an hour or a
		// day apart: twenty at once for a first copy of the cloud's months,
		// then one each five seconds at most.
		firingsRate: &limiter{rate: 0.2, burst: 20}, firingsBusy: make(chan struct{}, 1), port: newNotifyPort(),
		// A tracker job reads a page of tasks, then makes an issue and
		// writes a link for each: a few a second, a first run's in a burst.
		tasksRate: &limiter{rate: 5, burst: 100}, tasksBusy: make(chan struct{}, 1), changes: newChanges(ctx, l)}
	s.status.runs, s.status.notices, s.status.refused = map[string]int{}, map[string]int{}, map[string]int{}
	if s.ledgerID, err = ledgerID(ctx, l); err != nil {
		l.Close()
		return nil, fmt.Errorf("--ledger: %w", err)
	}
	s.open.Store(cfg.EveryoneAdmin || LoopbackOnly(cfg.Listen))
	if !known(s.cfg.Lang) {
		s.cfg.Lang = "en"
	}
	if cfg.Ruler != "" {
		if err := s.newRuler(ctx); err != nil {
			l.Close()
			return nil, err
		}
	}
	if err := s.newAlarms(ctx); err != nil {
		l.Close()
		return nil, err
	}
	l.TellTo(s.dests())
	return s, nil
}

// ledgerID is the ledger's name, twelve hex digits made at random the first
// time it is asked for, and kept.
func ledgerID(ctx context.Context, l *ledger.Ledger) (string, error) {
	id, err := l.Meta(ctx, "ledger-id")
	if err != nil || id != "" {
		return id, err
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id = hex.EncodeToString(b)
	return id, l.SetMeta(ctx, "ledger-id", id)
}

// requestAuth authenticates a ruler's requests with a cloud's credentials;
// tests replace it.
var requestAuth = cloud.Request

// callerAccount is the AWS account the pod's own identity is in.
var callerAccount = cloud.AWSAccount

// newRuler readies the reader of --ruler, with the credentials that read
// it, and, with --changes, those that write to it, which aren't read
// otherwise.
func (s *Server) newRuler(ctx context.Context) error {
	r, err := source.NewRuler(s.cfg.Ruler, "lapilli/"+s.cfg.Version)
	if err != nil {
		return err
	}
	r.Tenant, r.Transport = s.cfg.RulerTenant, s.cfg.RulerTransport
	r.Warn = func(what string) { s.log.Warn(what) }
	switch r.Kind {
	case source.RulerConfig:
		r.Reads.TokenFile = s.cfg.RulerToken
		if s.cfg.Changes {
			s.rulerWrites.TokenFile = s.cfg.ChangesRulerToken
		}
	default:
		cloudKind, as := "aws", s.cfg.ChangesAWSRole
		if r.Kind == source.RulerAzure {
			cloudKind, as = "azure", s.cfg.ChangesAzureClientID
		}
		if r.Reads.Sign, err = requestAuth(ctx, cloudKind, s.cfg.Ruler, ""); err != nil {
			return fmt.Errorf("--ruler: %w", err)
		}
		s.rulerWrites.Sign = r.Reads.Sign
		if s.cfg.Changes && as != "" {
			if s.rulerWrites.Sign, err = requestAuth(ctx, cloudKind, s.cfg.Ruler, as); err != nil {
				return fmt.Errorf("the ruler's write credentials: %w", err)
			}
		}
	}
	s.ruler = r
	return nil
}

// alarmsCloud is the cloud whose credentials read and write alarms of a
// kind.
var alarmsCloud = map[string]string{source.AlarmsAWS: "aws", source.AlarmsAzure: "azure", source.AlarmsGoogle: "google", source.AlarmsOCI: "oci"}

// newAlarms readies the readers of --alarms, each with its cloud's
// credentials, whatever --auth says; and, with --changes, the identity
// each cloud is written as, which isn't read otherwise.
func (s *Server) newAlarms(ctx context.Context) error {
	s.alarmWrites = map[string]source.Creds{}
	for _, at := range s.cfg.Alarms {
		a, err := source.NewAlarms(at.Source, at.URL, "lapilli/"+s.cfg.Version)
		if err != nil {
			return err
		}
		a.Transport, a.Now = s.cfg.RulerTransport, func() time.Time { return s.now() }
		a.Warn = func(what string) { s.log.Warn(what) }
		kind := alarmsCloud[a.Kind]
		if a.Reads.Sign, err = requestAuth(ctx, kind, a.URL, ""); err != nil {
			return fmt.Errorf("--alarms %s: %w", a.Source, err)
		}
		writes := a.Reads
		if as := s.writeAs(a.Kind); s.cfg.Changes && as != "" {
			if writes.Sign, err = requestAuth(ctx, kind, a.URL, as); err != nil {
				return fmt.Errorf("--alarms %s: the write credentials: %w", a.Source, err)
			}
		}
		s.alarms = append(s.alarms, a)
		s.alarmWrites[a.Source] = writes
	}
	return nil
}

// writeAs is the identity a cloud's alarms are written as, "" for the
// pod's own.
func (s *Server) writeAs(kind string) string {
	switch kind {
	case source.AlarmsAWS:
		return s.cfg.ChangesAWSRole
	case source.AlarmsAzure:
		return s.cfg.ChangesAzureClientID
	case source.AlarmsGoogle:
		return s.cfg.ChangesGoogleServiceAccount
	case source.AlarmsOCI:
		return s.cfg.ChangesOCIConfig
	}
	return ""
}

// alarmsOf is the --alarms a source names, nil for none.
func (s *Server) alarmsOf(src string) *source.Alarms {
	for _, a := range s.alarms {
		if a.Source == src {
			return a
		}
	}
	return nil
}

// alarmsEvery is how often --alarms is read: --alarms-every, 10m by
// default.
func (s *Server) alarmsEvery() time.Duration {
	if s.cfg.AlarmsEvery != 0 {
		return s.cfg.AlarmsEvery // below zero in tests: read each time
	}
	return 10 * time.Minute
}

// Close closes the ledger.
func (s *Server) Close() error { return s.l.Close() }

// Run serves the UI and keeps the ledger until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	// The versions from before the ledger began, before anything replays.
	if err := s.history(ctx); err != nil {
		return err
	}
	s.logDigest(ctx)
	// The on-call turns kept, before anything is counted by them.
	if err := s.onCallKept(ctx); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("--listen: %w", err)
	}
	s.listening(ln.Addr())
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
	served := make(chan error, 4)
	go func() { served <- srv.Serve(ln) }()
	if s.cfg.MetricsListen != "" {
		mln, err := net.Listen("tcp", s.cfg.MetricsListen)
		if err != nil {
			srv.Close()
			return fmt.Errorf("--metrics-listen: %w", err)
		}
		msrv := &http.Server{Handler: s.MetricsHandler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
			WriteTimeout: time.Minute, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 16 << 10}
		s.log.Info("serving the metrics", "listen", mln.Addr().String())
		go func() { served <- msrv.Serve(mln) }()
		defer msrv.Close()
	}
	if s.cfg.IncidentsListen != "" {
		iln, err := net.Listen("tcp", s.cfg.IncidentsListen)
		if err != nil {
			srv.Close()
			return fmt.Errorf("--incidents-listen: %w", err)
		}
		isrv := &http.Server{Handler: s.IncidentsHandler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
			WriteTimeout: time.Minute, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 16 << 10,
			BaseContext: func(net.Listener) context.Context { return ctx }}
		s.log.Info("taking incidents and firings", "listen", iln.Addr().String())
		go func() { served <- isrv.Serve(iln) }()
		defer isrv.Close()
	}
	if s.cfg.NotifyListen != "" {
		nln, err := net.Listen("tcp", s.cfg.NotifyListen)
		if err != nil {
			srv.Close()
			return fmt.Errorf("--notify-listen: %w", err)
		}
		nsrv := &http.Server{Handler: s.NotifyHandler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
			WriteTimeout: time.Minute, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 16 << 10,
			BaseContext: func(net.Listener) context.Context { return ctx }}
		s.log.Info("taking notifications", "listen", nln.Addr().String())
		go func() { served <- nsrv.Serve(nln) }()
		defer nsrv.Close()
	}
	// The rules are read before the first run, which replays them.
	raw := ""
	if s.replaying() {
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
	if s.replaying() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.watch(ctx, raw)
		}()
	}
	if len(s.sendsTo()) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.notify(ctx)
		}()
	}
	if s.cfg.OnCall != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.readOnCall(ctx)
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
