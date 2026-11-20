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
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/signin"
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
	RulesHistory string
	// RulesSelect keeps, of the Kubernetes objects in the files RulesPath
	// and RulesHistory hold, those whose labels it matches.
	RulesSelect           rules.Selector
	Loaded                bool
	Grafana, GrafanaToken string
	// Loki is a Loki whose ruler's rules are replayed, beside the rest,
	// against it (docs/design/loki.md): its URL, the file holding its
	// token, and its tenant. LokiRules is where its rule files are, read
	// again every minute; LokiLoaded reads them from its ruler's API.
	Loki, LokiToken, LokiTenant, LokiRules string
	LokiLoaded                             bool
	// LokiLimit is the Limiter the Loki's queries are asked by, which its
	// ruler is read by each minute too (--max-rate, and its push back).
	LokiLimit *prom.Limiter
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
	// Signals are the named impact signals (docs/design/signals.md), from
	// --impact NAME=PROMQL or --impact-file, each "for" given: with them,
	// Impact is "", and no impact signal has no name. ImpactRules are the
	// rule entries: which rules are judged against which signals' windows,
	// and on which labels.
	Signals     []ledger.SignalDef
	ImpactRules []verdict.RuleEntry
	// Deploys is the deploys file, read again each run, which owns the
	// ledger's deploys (docs/design/noc.md); Options.DeployAfter is how
	// long after one a firing still came during it.
	Deploys string
	// Marked is the marked file (--marked), read again each run, which
	// owns the marked windows it holds; Marks are the expressions (--mark)
	// whose windows are read each run (docs/design/marked.md).
	Marked string
	Marks  []Mark
	// RunbookAnnotations are the annotations a rule's runbook is read
	// from, the first set: nil for content.RunbookAnnotations. RunbookBase
	// is an https URL a runbook that is a path in the rules' repository is
	// a link under (content.RepoLink).
	RunbookAnnotations []string
	RunbookBase        string

	// Options are the judging settings from the command line; the UI can
	// change Lead, Match, RefireWithin and the noisy thresholds.
	Options verdict.Options
	Step    time.Duration
	// Late is how late each rule's metrics are taken to arrive (--late).
	Late rules.Lateness
	Loc  *time.Location

	Ledger string // the directory the ledger is kept in
	// LedgerURLFile is a file holding a Postgres connection string: the
	// ledger is kept in that database, not in Ledger (docs/design/postgres.md).
	// The string is read once, at start, and said nowhere.
	LedgerURLFile string
	// CheckReplay sets the replay beside what really fired, read from
	// CheckSeries or ALERTS and GRAFANA_ALERTS; a rule differs as
	// CheckDiffers says (docs/design/fidelity.md).
	CheckReplay  bool
	CheckSeries  []string
	CheckDiffers Differs
	// History is the Grafana whose alert state history says what its
	// rules really fired, read in place of GRAFANA_ALERTS where it keeps
	// one it serves (docs/design/grafanahistory.md): --check-grafana's, or
	// --grafana's without --check-series; nil for none.
	History  *source.GrafanaHistory
	Every    time.Duration
	Settle   time.Duration
	Backfill time.Duration // MaxBackfill at most
	Keep     time.Duration // 0 keeps everything
	Watch    time.Duration
	Digest   Weekly
	Slack    string // a file holding a Slack incoming webhook's URL
	Timeout  time.Duration
	Parallel int
	// Yield yields to a history API that answers a run's request with HTTP
	// 429 (--yield-on-429): the run asks it nothing more and stops there,
	// and the rules it didn't replay are missed, replayed by a later run;
	// no run asks it for YieldFor after (--yield-for). Guard is a PromQL
	// expression asked before each run and between the days of a
	// catch-up: while it returns anything, runs wait (--guard). A catch-up
	// runs only within CatchUpHours, nil for any, and asks each API
	// CatchUpRate requests a second at most, 0 for --max-rate alone
	// (docs/design/quota.md).
	Yield        bool
	YieldFor     time.Duration
	Guard        string
	CatchUpHours *Window
	CatchUpRate  float64

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
	// KeepActivity is how long the activity log is kept, past Keep: 0
	// keeps it all (docs/design/serveops.md, "The activity log").
	KeepActivity time.Duration
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
	// Viewers are who may look (users, group:NAME, *@DOMAIN, "*"): with
	// them, or a sign-in of lapilli's own, no one else comes in at all.
	Viewers []string

	// A sign-in of lapilli's own (docs/design/signin.md), in place of
	// UserHeader: OIDC signs people in with an OpenID Connect provider,
	// sending them back to ExternalURL's /auth/callback; Proxy takes them
	// from the JWT a sign-in in front signs. Sessions last SessionMaxAge
	// (12h when 0), in cookies signed with the key in the file SessionKey,
	// or else one made for the process.
	OIDC          *signin.OIDC
	Proxy         *signin.Proxy
	SessionMaxAge time.Duration
	SessionKey    string

	// What happens (tasks, incidents, quiet windows, comments) is told to
	// the webhook whose URL is in NotifyWebhook, as JSON, and with
	// NotifySlack to Slack; their links are under ExternalURL. Lang is the
	// language of what goes to Slack. With NotifyWebhookSecret, a file
	// holding a key, each post to the webhook is signed with it
	// (report.Sign).
	NotifyWebhook       string
	NotifyWebhookSecret string
	NotifySlack         bool
	ExternalURL         string
	Lang                string
	// TeamDigests are files holding the incoming webhooks that take a
	// team's own weekly digest, by the team's name.
	TeamDigests map[string]string
	// TeamLabels are the labels whose values the Teams page suggests as
	// teams, when no team owns by them; none for defaultTeamLabels.
	TeamLabels []string
	// ReportDests are files holding the Slack-style incoming webhooks the
	// reports people schedule can go to, by the names they pick them by.
	ReportDests map[string]string
	// WebhookDests are files holding the URLs of webhooks that take what
	// --config's notify: routes to them, as JSON, as NotifyWebhook takes
	// it, by the names the routes send by; WebhookDestSecrets, by the same
	// names, files holding the keys each one's posts are signed with
	// (docs/design/notifications.md, "Who is told what").
	WebhookDests, WebhookDestSecrets map[string]string

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

	// Config is a YAML file declaring teams, quiet windows, reports and
	// roles (docs/design/serveops.md), which the ledger is kept holding,
	// read again as it changes; what it declares the UI doesn't change.
	// ConfigPrune deletes what it no longer declares, which is otherwise
	// the UI's again.
	Config      string
	ConfigPrune bool
	// ProposalsExpire closes a proposal that waited for approval longer
	// (docs/design/changes.md): 0 for never.
	ProposalsExpire time.Duration
	// BacktestListen, when set, is where pull request backtests are taken
	// (docs/design/backtests.md), from a CI job holding a token an entry of
	// Trust (--backtest-trust, read at start) takes, and nothing else.
	// BacktestShare is the lane's share of --max-rate (0.25 when 0),
	// BacktestQueue how many may wait (20 when 0), BacktestTimeout one's
	// time from its start (15m when 0). BacktestTransport carries what the
	// issuers are asked: nil but in tests.
	BacktestListen    string
	Trust             []Trust
	BacktestShare     float64
	BacktestQueue     int
	BacktestTimeout   time.Duration
	BacktestTransport http.RoundTripper

	// GoalTasks makes a task for each goal --config declares that a whole
	// week missed, 20 a run at most (docs/design/goals.md): off unless
	// turned on, as Lapilli would make tasks nobody asked for.
	GoalTasks bool
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
	// in is the sign-in of lapilli's own, nil without one.
	in *signIn
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
	// tries are, by reader, the proposals asked for in the last hour that
	// read the rules again (propose); impactTries, the incident criteria
	// tried or saved, which read Prometheus (impactTriesAnHour);
	// settingsTries, the settings tried on the last week
	// (settingsTriesAnHour).
	tries         tries
	impactTries   tries
	settingsTries tries
	// tried are the criteria a try read in full, lately (triedFor).
	tried triedCache
	// sources is what this replica saw of where incidents come from;
	// reads, how its reads of the rules and the files went (sourcelist.go).
	sources sourcesState
	reads   readsState
	// turns are how each named signal's asks went, that each round shares
	// --timeout by (inTurn).
	turns turns
	// outlasts are the proposals' for_outlasts, worked out as they are
	// read (outlastsNow), and parsing the versions of the rules older
	// proposals were made against.
	outlasts outlasts
	parsing  flights[namedRules]
	// life is the server's own context, ended (end) as it shuts down or is
	// closed: what runs apart from a request (a flight) runs under it.
	life     context.Context
	end      context.CancelFunc
	closed   sync.Once
	closeErr error
	// againFailed are the stale proposals Lapilli failed to backtest again
	// itself (backtestStale).
	againFailed againFailed
	// config is what the leader last made of --config.
	config configState
	// routing is what this replica made of --config's notify: last.
	routing routingState
	// bnd keeps the boundaries of change the ledger holds (bounds).
	bnd boundsCache
	// backtesting are the proposals being backtested again here, which
	// expiry leaves.
	backtesting inFlight
	// wall is the clock proposals are made and expire by, and the
	// webhook's posts are signed at: time.Now but in tests.
	wall func() time.Time
	// heavy is the one turn there is for a request that takes hundreds of
	// megabytes: every firing of a month, the settings tried over a week.
	heavy chan struct{}
	// renders are the turns to work out a page (pageRenders).
	renders chan struct{}
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
	// deploysRead is when the deploys file was last read, and kept, in
	// Unix seconds; 0 before it was.
	deploysRead atomic.Int64
	// deploysUnread is set once a ledger keeping deploys with no
	// --deploys to read was logged.
	deploysUnread atomic.Bool
	// markedRead is when the marked file was last read, and kept, in Unix
	// seconds; forgotMarks, that the keys of expressions no longer given
	// were let go.
	markedRead  atomic.Int64
	forgotMarks atomic.Bool
	// slackTried is when the worker last tried to post the digest.
	slackTried time.Time
	status     status
	cache      cache
	own        ownersCache
	sigs       signalsCache
	grantsC    grantsCache
	creds      creditCache
	hours      hoursCache
	// goalsC are the goals the ledger holds, goalRows the goals' weeks the
	// pages read, and goalsK what the leader worked out of them last.
	goalsC   goalsCache
	goalRows goalRowsCache
	goalsK   goalsKept
	goalsF   goalsFollowed
	outs     outcomesCache
	// wait keeps the counts the sidebar shows.
	wait waitingCache
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
	// places are where --loki's ruler runs each of its groups, as last
	// read with its rules, failing whether the last read of them failed;
	// notes, what was last said of its rule files.
	places struct {
		sync.Mutex
		m       map[rules.Place]time.Duration
		failing bool
		notes   string
	}
	// picked is what reading the rule files last passed over, as said.
	picked atomic.Value
	// leaderSaid is the leader's status as it last said it, read, for a
	// follower's pages (shownStatus).
	leaderSaid struct {
		sync.Mutex
		text string
		st   sharedStatus
	}
	// incidentsRead is set once this replica read the incidents file: its
	// metric is the leader's.
	incidentsRead atomic.Bool
	// stepAt is when the leader's work last made progress, in Unix
	// nanoseconds, 0 while it waits for its next round; rounded, when it
	// last finished a round, in Unix seconds. Progress is a job begun, a
	// run's piece, or, in the leader's context, a request to a history
	// (asked) or a rule replayed (replayed): moved is their sum, as last
	// looked at (stuck).
	stepAt, rounded, moved atomic.Int64
	asked                  prom.Counts
	replayed               replay.Progress
	// othersAsked counts what people and agents asked of the history (a
	// proposal's backtest, MCP's compare), apart from asked, which tells
	// the leader's progress. yield is what the runs yield to, nil without
	// --yield-on-429; holding is set while this round's run waits, or
	// yielded to the prometheus API (docs/design/quota.md).
	othersAsked prom.Counts
	yield       *prom.Yield
	holding     atomic.Bool
	// heldFor is why this round's run waited, one of heldReasons, "" for
	// none; catchUpPace paces every catch-up (--catch-up-rate), nil for
	// none.
	heldFor     atomic.Value
	catchUpPace *prom.Paces
	// btPort is what the backtest port keeps, nil without
	// --backtest-listen.
	btPort *backtestPort
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
	// historyWhere is where the Grafana of Config.History keeps its alert
	// state history, as it said at historyAt. checkHistory is where it was
	// read from for checkDay, "" when it wasn't; historyOff why it was of
	// no use then, its rules not checked that day: historyFrom,
	// historyTooMany or historyUnusable, or "".
	historyWhere source.HistoryWhere
	historyAt    time.Time
	checkHistory string
	historyOff   string
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
	if err := CheckRoles(cfg); err != nil {
		return nil, err
	}
	l, err := openLedger(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, h: h, l: l, log: log, now: time.Now, wall: time.Now, wake: make(chan struct{}, 1), boot: strconv.FormatInt(time.Now().UnixNano(), 36),
		heavy: make(chan struct{}, 1), renders: make(chan struct{}, pageRenders), tokenRate: &limiter{rate: 20, burst: 40}, tokenBusy: make(chan struct{}, 1),
		// A job sends a source's firings a period at a time, an hour or a
		// day apart: twenty at once for a first copy of the cloud's months,
		// then one each five seconds at most.
		firingsRate: &limiter{rate: 0.2, burst: 20}, firingsBusy: make(chan struct{}, 1), port: newNotifyPort(),
		// A tracker job reads a page of tasks, then makes an issue and
		// writes a link for each: a few a second, a first run's in a burst.
		tasksRate: &limiter{rate: 5, burst: 100}, tasksBusy: make(chan struct{}, 1), changes: newChanges(ctx, l),
		impactTries: tries{limit: impactTriesAnHour}, settingsTries: tries{limit: settingsTriesAnHour}}
	s.status.runs, s.status.notices, s.status.refused = map[string]int{}, map[string]int{}, map[string]int{}
	s.life, s.end = context.WithCancel(context.Background())
	s.parsing.base, s.parsing.log = s.life, log
	if s.ledgerID, err = ledgerID(ctx, l); err != nil {
		l.Close()
		return nil, fmt.Errorf("--ledger: %w", err)
	}
	s.open.Store(cfg.EveryoneAdmin || LoopbackOnly(cfg.Listen))
	if cfg.OIDC != nil || cfg.Proxy != nil {
		if err := s.newSignIn(ctx); err != nil {
			l.Close()
			return nil, err
		}
		// Signed in, people come from elsewhere, often through a proxy on
		// this machine: loopback says nothing of who they are, and no one
		// is an admin for it. Roles come from the lists alone.
		s.open.Store(false)
	}
	if err := s.joinable(ctx); err != nil {
		l.Close()
		return nil, err
	}
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
	// Routed by --config's notify:, which Run reads before anything is
	// told: until then, as without routes.
	l.RouteBy(s.route)
	s.shareLimits()
	if cfg.Yield {
		s.yield = &prom.Yield{For: cfg.YieldFor, Now: func() time.Time { return s.now() }}
	}
	s.catchUpPace = prom.NewPaces(cfg.CatchUpRate)
	if cfg.BacktestListen != "" {
		if err := s.backtestable(); err != nil {
			l.Close()
			return nil, err
		}
		s.btPort = s.newBacktestPort()
	}
	return s, nil
}

// backtestable says why the backtest port can't be served, nil when it
// can: who may ask, rules to replay, a history to ask, and writes taken.
func (s *Server) backtestable() error {
	return backtestable(s.cfg, s.replaying(), s.h != nil)
}

func backtestable(cfg Config, replaying, history bool) error {
	switch {
	case len(cfg.Trust) == 0:
		return errors.New("--backtest-listen needs --backtest-trust: who may ask")
	case cfg.Alerts:
		return errors.New("--backtest-listen replays a pull request's rules, and --alerts judges the ALERTS series, not rules")
	case !replaying || !history:
		return errors.New("--backtest-listen replays a pull request's rules against the history: it needs rules to replay and --prometheus or --gcp-project")
	case cfg.ReadOnly:
		return errors.New("--backtest-listen queues backtests in the ledger, and --read-only refuses writes")
	case cfg.BacktestShare != 0 && (cfg.BacktestShare < 0.05 || cfg.BacktestShare > 0.75):
		return errors.New("--backtest-share is 0.05 to 0.75")
	}
	return nil
}

// BacktestRefused is why New refuses cfg's backtest port, as it starts,
// nil when it takes it or there is none: lapilli preflight says it before
// serve is deployed. history says New is given a history to replay
// against (--prometheus or --gcp-project).
func BacktestRefused(cfg Config, history bool) error {
	if cfg.BacktestListen == "" {
		return nil
	}
	return backtestable(cfg, (&Server{cfg: cfg}).replaying(), history)
}

// openLedger opens the ledger: in Postgres, with --ledger-url-file, else
// the SQLite file in --ledger.
func openLedger(ctx context.Context, cfg Config, log *slog.Logger) (*ledger.Ledger, error) {
	if cfg.LedgerURLFile != "" {
		b, err := os.ReadFile(cfg.LedgerURLFile)
		if err != nil {
			return nil, fmt.Errorf("--ledger-url-file: %w", err)
		}
		l, err := ledger.OpenPostgres(ctx, string(b), cfg.Loc)
		if err != nil {
			return nil, fmt.Errorf("--ledger-url-file: %w", err)
		}
		for _, w := range l.Warnings() {
			log.Warn(w)
		}
		return l, nil
	}
	if err := os.MkdirAll(cfg.Ledger, 0o750); err != nil {
		return nil, fmt.Errorf("--ledger: %w", err)
	}
	l, err := ledger.Open(ctx, filepath.Join(cfg.Ledger, "ledger.db"), cfg.Loc)
	if err != nil {
		return nil, fmt.Errorf("--ledger: %w", err)
	}
	return l, nil
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
	if l.Shared() {
		// The one kept, by whichever replica made it first.
		return l.MetaOnce(ctx, "ledger-id", id)
	}
	return id, l.SetMeta(ctx, "ledger-id", id)
}

// requestAuth authenticates a ruler's requests with a cloud's credentials;
// tests replace it.
var requestAuth = cloud.Request

// writeAuth authenticates a cloud's writes as the pod's own identity where
// they take credentials of their own (Google Cloud's write scope); tests
// replace it.
var writeAuth = cloud.Writes

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
		} else if s.cfg.Changes && a.Kind == source.AlarmsGoogle {
			// The pod's own identity writes: its token of the scope that
			// writes, as the reads' is of the one that reads.
			if writes.Sign, err = writeAuth(ctx, kind, a.URL, ""); err != nil {
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
// Close ends what the server runs apart from its requests (the reads of
// older versions of the rules for what proposals say, flights) and closes
// the ledger: once, however many times it is called.
func (s *Server) Close() error {
	s.closed.Do(func() {
		s.end()
		s.closeErr = closeLedger(s.l)
	})
	return s.closeErr
}

// closeLedger closes a server's ledger: a variable, so that a test can
// count how often.
var closeLedger = (*ledger.Ledger).Close

// Run serves the UI and keeps the ledger until ctx is done. Every replica
// serves; the one that leads (all of them in turn, one at a time, on a
// Postgres ledger; the one process on SQLite's) runs what must run once
// (lead).
func (s *Server) Run(ctx context.Context) error {
	// Stopped as well when a server fails, or the ledger's lock is lost:
	// the workers stop with it.
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	// --config's notify:, as read now, before anything is told.
	s.loadRoutes(time.Now(), true)
	shared := s.l.Shared()
	if !shared {
		// The versions from before the ledger began, before anything
		// replays; a replica's leader adds them as it begins (lead).
		if err := s.history(ctx); err != nil {
			return err
		}
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
	if s.in != nil && s.in.oidc != nil {
		// Asked at once, so a setting that is wrong is in the log at start,
		// not first found by someone signing in.
		go s.discover(ctx)
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
	where := s.cfg.Ledger
	if s.l.Postgres() {
		where = "postgres"
	}
	if shared {
		s.log.Info("serving", "listen", ln.Addr().String(), "ledger", where, "replica", s.l.ReplicaID())
	} else {
		s.log.Info("serving", "listen", ln.Addr().String(), "ledger", where)
	}
	served := make(chan error, 5)
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
	if s.btPort != nil {
		bln, err := net.Listen("tcp", s.cfg.BacktestListen)
		if err != nil {
			srv.Close()
			return fmt.Errorf("--backtest-listen: %w", err)
		}
		bsrv := s.backtestServer(ctx)
		s.log.Info("taking pull request backtests", "listen", bln.Addr().String(), "trust", len(s.btPort.trust))
		go func() { served <- bsrv.Serve(bln) }()
		defer bsrv.Close()
		go s.discoverIssuers(ctx)
	}
	if s.cfg.Config != "" {
		// --config's notify:, read again by every replica, as each tells
		// of what it writes.
		go s.watchRoutes(ctx)
	}
	failed := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// What runs once among the replicas, while this one leads; at
		// once with SQLite.
		s.l.Elect(ctx, ledger.Elected{Version: s.cfg.Version, Status: s.publishedStatus, Stuck: s.stuck}, func(lctx context.Context) {
			s.lead(replay.Tracking(prom.Counting(lctx, &s.asked), &s.replayed), failed)
		})
	}()
	if shared {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.keepUp(ctx)
		}()
	}
	var serr error
	select {
	case <-ctx.Done():
	case serr = <-served:
	case serr = <-failed:
	case <-s.l.Lost():
		// A process may write the ledger alone now, or a newer lapilli
		// migrated it: this one stops, and writes nothing more, rather
		// than send a notice twice.
		serr = s.l.LostWhy()
	}
	stop()
	// What runs apart from the requests ends with them, so that none of
	// them waits on it to finish.
	s.end()
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
	// The named signals' generation, read before the settings: options
	// read before a change to them judge nothing it pended
	// (ledger.SignalsGen).
	gen, err := s.l.SignalsGen(ctx)
	if err != nil {
		return o, err
	}
	set, err := s.l.Settings(ctx)
	if err != nil {
		return o, err
	}
	o.SignalsGen = gen
	// With an impact signal, the record is kept from when the runs read it,
	// all of it, with the criterion in effect (verdict.Options.ImpactFrom,
	// impactFrom); with named ones, each from its own start, a firing by
	// the signals tied to it (signalsOf).
	if o.ImpactFrom, err = s.impactFrom(ctx, s.criterionOf(set)); err != nil {
		return o, err
	}
	// What it replays is judged by the weeks it keeps its stones by, and
	// its episodes parted by the ledger's days.
	o.RecordWeek = s.l.WeekOf
	o.Zone = s.l.Location()
	for k, v := range set {
		if slices.ContainsFunc(criteriaSettable, func(c struct{ key, flag string }) bool { return c.key == k }) || signalSetting(k) {
			continue // the incident criteria's, above and below
		}
		if err := apply(&o, k, v.Value); err != nil {
			s.log.Warn("a setting from the UI doesn't parse; the command line's is used", "setting", k, "err", err)
		}
	}
	// The rule entries are read with no named signal in effect too: they
	// keep their rules from the windows of a signal not in effect
	// (docs/design/judging.md, item 5).
	if n := s.signalsIn(set); n.named() || len(n.Entries) > 0 {
		if o.Signals, err = s.signalsOf(ctx, o.Match, n, gen); err != nil {
			return o, err
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
			// Any name, as ever: one that isn't a label's name is never
			// put in a query (source.SeenFrom).
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

// MaxBackfill is the longest --backfill: ten years. A first start replays
// it a day at a time, and lapilli preflight counts its requests a day at a
// time, both as long as it is; a longer one is a typing error more often
// than a history that keeps more.
const MaxBackfill = 3650 * 24 * time.Hour

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
