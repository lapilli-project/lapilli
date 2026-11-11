package serve

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lapilli.dev/internal/ledger"
)

// The notifications port takes notifications as an alert manager sent
// them, and nothing else, so a NetworkPolicy can let the alert manager
// reach it and no one else. Each source has a path of its own, since each
// proves itself its own way:
//
//	/v1/alertmanager  a bearer token (Alertmanager's http_config.authorization)
//	/v1/grafana       the same token, or Grafana's HMAC-SHA256 signature
//	/v1/sns           Amazon SNS's signature, from a topic in --notify-sns
//	/v1/azure         the token as the password in the action group's URL
//	/v1/google        the token as the password, after a 401 asking for it
//	/v1/oci/SOURCE    the token as the password in the subscription's URL
//
// A cloud's alarms are told of on the paths of its alert managers: a
// CloudWatch alarm's on /v1/sns, an Azure Monitor alert's on /v1/azure, a
// Cloud Monitoring policy's on /v1/google, an OCI alarm's on /v1/oci. The
// URL can name the source the alarm's firings are copied in as (?source=,
// or the path for OCI), so its alerts tie to the rule copied in from there.
//
// It doesn't check the host name the request names, as the UI's port does
// against DNS rebinding: every request here carries its own proof, which a
// browser doesn't add of itself, and the alert managers reach it by names
// of their own, Google Cloud's by a public one.

// notifyMax is the largest body taken.
const notifyMax = 1 << 20

// Bounds on what the port takes before, and after, a request proves itself.
const (
	// preAuth is how many requests may be sending their bodies at once
	// before anything says who sent them: SNS's and Grafana's signatures are
	// of the body. Only one whose headers say it could be from SNS or
	// Grafana takes a place. A request past them is answered 503, which SNS
	// tries again. 64 bodies of 1 MB at most stay well within the chart's
	// memory.
	preAuth = 64
	// preAuthRead is how long such a body may take to come.
	preAuthRead = 10 * time.Second
	// snsAge is how old a message SNS signed may be: SNS stops trying to
	// deliver one after an hour. A little more allows for clocks.
	snsAge = time.Hour + 5*time.Minute
	// clockSkew is how far ahead of this server's clock a signed time may
	// be.
	clockSkew = 5 * time.Minute
	// grafanaReplay is how long a Grafana signature is remembered: a
	// signature with a timestamp holds five minutes either side of it.
	grafanaReplay = 10 * time.Minute
	// maxReplays is how many signed messages are remembered at once.
	maxReplays = 100_000
)

// maxAlertsPerDay is how many alerts the notifications kept in a day (UTC)
// from one source carry at most: a token's holder can't fill the disk with
// them, nor, sending on one path, keep another's out. Kept alerts take
// about a kilobyte each.
var maxAlertsPerDay = 200_000

// notifyPort is what the port keeps between requests.
type notifyPort struct {
	// rates are each source's, so one can't use up another's.
	rates map[string]*limiter
	busy  chan struct{}
	// preAuth holds a place for each request sending its body before it
	// has proved itself.
	preAuth chan struct{}
	// replays are the signed messages kept lately, by source: one sent
	// again is answered as kept, without a turn at the ledger.
	replays replays
	// day counts the alerts kept today from each source, against
	// maxAlertsPerDay.
	day struct {
		sync.Mutex
		start  time.Time
		alerts map[string]int
		warned map[string]bool
	}
	// warned are the files whose token or secret can't be used, logged
	// once each.
	warnedMu sync.Mutex
	warned   map[string]bool
	sns      snsTrust
	// snsWarned is when a refused SNS message was last logged: once a
	// minute at most, as anyone can send one.
	snsWarned atomic.Int64
	oci       ociConfirm
}

// ociConfirm confirms OCI Notifications subscriptions, from requests that
// carry the token only, a few at a time, each URL once: get fetches one,
// from a Notifications API host only. Tests give get a stand-in.
type ociConfirm struct {
	get     func(ctx context.Context, url string) ([]byte, error)
	mu      sync.Mutex
	done    map[string]time.Time
	fetches *limiter
	fetchMu sync.Mutex
	wg      sync.WaitGroup
}

// snsTrust is how SNS messages are checked: get fetches a signing
// certificate or confirms a subscription, from an SNS host only; certs keeps
// the certificates fetched. Tests give get a stand-in.
type snsTrust struct {
	get   func(ctx context.Context, url string) ([]byte, error)
	mu    sync.Mutex
	certs map[string]*x509.Certificate
	used  map[string]time.Time
	// proven are the certificates that have verified a message: one that
	// hasn't makes way for them, never they for it.
	proven map[string]bool
	// A message is checked before anything says who sent it, so fetching
	// certificates is bounded: a few a minute, one at a time, and a URL
	// that gave no certificate isn't tried again for a while.
	fetches *limiter
	fetchMu sync.Mutex
	failed  map[string]failure
}

// failure is a certificate URL that gave none: when, and whether it may
// give one if tried again soon, as after a timeout or a 5xx.
type failure struct {
	at        time.Time
	transient bool
}

// errSNSLater is a message that may hold, but can't be checked now: it is
// answered 503, which SNS tries again, rather than 403, which it drops.
var errSNSLater = errors.New("can't check the signature now")

func newNotifyPort() *notifyPort {
	p := &notifyPort{rates: map[string]*limiter{}, busy: make(chan struct{}, 1), preAuth: make(chan struct{}, preAuth),
		warned: map[string]bool{}}
	for _, src := range []string{"alertmanager", "grafana", "sns", "azure", "google", "oci"} {
		p.rates[src] = &limiter{rate: 20, burst: 100}
	}
	p.oci.get = ociGet
	p.oci.done = map[string]time.Time{}
	p.oci.fetches = &limiter{rate: 0.1, burst: 5}
	p.replays.at = map[string]time.Time{}
	p.sns.get = snsGet
	p.sns.certs, p.sns.used, p.sns.proven = map[string]*x509.Certificate{}, map[string]time.Time{}, map[string]bool{}
	p.sns.fetches = &limiter{rate: 0.1, burst: 5}
	p.sns.failed = map[string]failure{}
	return p
}

// replays remembers signed messages kept, each for a while.
type replays struct {
	mu sync.Mutex
	at map[string]time.Time // when each is forgotten
}

// seen reports whether a message is remembered.
func (r *replays) seen(key string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	until, ok := r.at[key]
	return ok && now.Before(until)
}

// add remembers a message for a while: when full, those due are
// forgotten, then, if need be, the one due soonest.
func (r *replays) add(key string, now time.Time, keep time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.at) >= maxReplays {
		for k, until := range r.at {
			if !now.Before(until) {
				delete(r.at, k)
			}
		}
	}
	if len(r.at) >= maxReplays {
		soonest := ""
		for k, until := range r.at {
			if soonest == "" || until.Before(r.at[soonest]) {
				soonest = k
			}
		}
		delete(r.at, soonest)
	}
	r.at[key] = now.Add(keep)
}

// proof is how a path's requests prove who sent them.
type proof struct {
	// header says whether the headers carry the token: the body is read
	// only then, or when body can check it.
	header func(*http.Request) bool
	// could says whether the headers say the body could prove it: only then
	// is a body from anyone read. Nil means never.
	could func(*http.Request) bool
	// refuse answers a request that can't prove itself.
	refuse func(http.ResponseWriter)
	// body checks a signature of the body, answering a request it refuses.
	// It gives a key for the message, so the same one sent again is known,
	// and how long to remember it.
	body func(w http.ResponseWriter, r *http.Request, b []byte) (ok bool, key string, keep time.Duration)
	// turn is how long a request waits for its turn at the ledger before it
	// is answered 503, for a source that gives an answer little time; 0 is
	// five seconds, then 429.
	turn time.Duration
}

// NotifyHandler is the notifications port.
func (s *Server) NotifyHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/alertmanager", s.takeNotifications("alertmanager", proof{header: s.bearerOK, refuse: refuseBearer}, parseAlertmanager))
	mux.HandleFunc("POST /v1/grafana", s.takeNotifications("grafana", proof{header: s.bearerOK, could: s.grafanaCould, refuse: refuseBearer, body: s.grafanaSigned}, parseGrafana))
	mux.HandleFunc("POST /v1/sns", s.takeNotifications("sns", proof{could: s.snsCould, refuse: s.refuseSNS, body: s.snsSigned}, s.parseSNS))
	mux.HandleFunc("POST /v1/azure", s.takeNotifications("azure", proof{header: s.basicOK, refuse: refuseBasic}, parseAzure))
	mux.HandleFunc("POST /v1/google", s.takeNotifications("google", proof{header: s.basicOK, refuse: refuseBasic}, parseGoogle))
	// OCI Notifications gives an endpoint five seconds, TLS included.
	mux.HandleFunc("POST /v1/oci/{source}", s.takeNotifications("oci", proof{header: s.basicOK, refuse: refuseBasic, turn: 2 * time.Second}, s.parseOCI))
	return headers(mux)
}

// errRefused is a request that didn't prove itself; the check has answered.
var errRefused = errors.New("refused")

// tooMany is a notification with more alerts than are taken.
type tooMany struct{ n int }

func (e tooMany) Error() string {
	return fmt.Sprintf("%d alerts in one notification; %d at most (Alertmanager's max_alerts can keep it under)", e.n, ledger.MaxAlerts)
}

// takeNotifications has a request prove who sent it, reading its body only
// once its headers have, or to check a signature of it; then parse reads
// the notifications in it, and they are kept, one write at a time.
func (s *Server) takeNotifications(source string, p proof,
	parse func(r *http.Request, body []byte, now time.Time) ([]ledger.Notification, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.ReadOnly {
			http.Error(w, "lapilli serve is read-only (--read-only)", http.StatusForbidden)
			return
		}
		proved := p.header != nil && p.header(r)
		release := func() {}
		if !proved {
			if p.body == nil || p.could == nil || !p.could(r) {
				p.refuse(w)
				return
			}
			// A body from anyone: a few at a time, and not slowly.
			select {
			case s.port.preAuth <- struct{}{}:
				var once sync.Once
				release = func() { once.Do(func() { <-s.port.preAuth }) }
				defer release()
			default:
				w.Header().Set("Retry-After", "5")
				http.Error(w, "too many notifications arriving: try again in a few seconds", http.StatusServiceUnavailable)
				return
			}
			http.NewResponseController(w).SetReadDeadline(time.Now().Add(preAuthRead))
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, notifyMax))
		if err != nil {
			if mb := (*http.MaxBytesError)(nil); errors.As(err, &mb) {
				http.Error(w, "a notification of 1 MB at most", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "the notification came too slowly, or was cut off", http.StatusRequestTimeout)
			return
		}
		key, keep := "", time.Duration(0)
		if !proved {
			ok := false
			if ok, key, keep = p.body(w, r, body); !ok {
				return
			}
			release()
		}
		// The same signed message again is answered as kept: sent again by
		// the source, or by someone who saw it, it takes no turn.
		if key != "" && s.port.replays.seen(source+"\x00"+key, s.now()) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, "{\"kept\":0,\"same_as_kept\":1}\n")
			return
		}
		// The source the alerts' firings are copied in as, if the URL names
		// one: the path for OCI, ?source= for the others.
		copied := r.URL.Query().Get("source")
		if source == "oci" {
			copied = r.PathValue("source")
		}
		if copied != "" && !ledger.ValidSource(copied) {
			http.Error(w, "a source is lowercase letters, digits and dashes, 40 at most, such as cloudwatch-prod", http.StatusBadRequest)
			return
		}
		// An alert manager sends in bursts, and tries again; someone with the
		// token shouldn't get more than that.
		if !s.port.rates[source].allow(time.Now()) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many notifications: try again in a second", http.StatusTooManyRequests)
			return
		}
		ns, err := parse(r, body, s.now())
		var many tooMany
		switch {
		case errors.As(err, &many):
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		case errors.Is(err, errRefused):
			http.Error(w, "refused", http.StatusForbidden)
			return
		case err != nil:
			http.Error(w, source+": "+err.Error(), http.StatusBadRequest)
			return
		}
		// One write at a time, waiting a little for the turn: a burst is
		// taken, not refused.
		wait, busy := 5*time.Second, http.StatusTooManyRequests
		if p.turn > 0 {
			wait, busy = p.turn, http.StatusServiceUnavailable
		}
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case s.port.busy <- struct{}{}:
			defer func() { <-s.port.busy }()
		case <-t.C:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "busy keeping other notifications: try again in a second", busy)
			return
		case <-r.Context().Done():
			return
		}
		alerts := 0
		for _, n := range ns {
			alerts += len(n.Alerts)
		}
		if ok, err := s.dayRoom(r.Context(), source, alerts); err != nil {
			s.log.Error("counting the alerts kept today", "err", err)
			http.Error(w, "the notification couldn't be kept: try again", http.StatusInternalServerError)
			return
		} else if !ok {
			w.Header().Set("Retry-After", "3600")
			http.Error(w, fmt.Sprintf("the notifications kept today from %s carry %d alerts, as many as are taken from it in a day (UTC): try again tomorrow", source, maxAlertsPerDay),
				http.StatusTooManyRequests)
			return
		}
		kept := 0
		defer func() {
			if key != "" && err == nil {
				s.port.replays.add(source+"\x00"+key, s.now(), keep)
			}
		}()
		for _, n := range ns {
			n.Source, n.Copied = source, copied
			var ok bool
			ok, err = s.l.Notify(r.Context(), n)
			switch {
			case errors.Is(err, ledger.ErrBadNotification):
				http.Error(w, source+": "+err.Error(), http.StatusBadRequest)
				return
			case errors.Is(err, context.Canceled):
				return
			case err != nil:
				s.log.Error("keeping a notification", "source", source, "err", err)
				http.Error(w, "the notification couldn't be kept: try again", http.StatusInternalServerError)
				return
			}
			if ok {
				kept++
				s.port.day.Lock()
				s.port.day.alerts[source] += len(n.Alerts)
				s.port.day.Unlock()
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "{\"kept\":%d,\"same_as_kept\":%d}\n", kept, len(ns)-kept)
	}
}

// dayRoom reports whether the alerts of a notification from a source fit in
// what is kept from it today: maxAlertsPerDay, counted from the ledger when
// the day (UTC) begins, or the port starts.
func (s *Server) dayRoom(ctx context.Context, source string, alerts int) (bool, error) {
	d := &s.port.day
	d.Lock()
	defer d.Unlock()
	now := s.now().UTC()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if !d.start.Equal(start) {
		n, err := s.l.AlertsSince(ctx, start)
		if err != nil {
			return false, err
		}
		d.start, d.alerts, d.warned = start, n, map[string]bool{}
	}
	if d.alerts[source]+alerts <= maxAlertsPerDay {
		return true, nil
	}
	if !d.warned[source] {
		d.warned[source] = true
		s.log.Warn("refusing notifications from a source until tomorrow (UTC): those kept today carry as many alerts as are taken from it in a day",
			"source", source, "alerts", d.alerts[source], "most", maxAlertsPerDay)
	}
	return false, nil
}

// secret reads a file holding a token or a secret, for each request, so it
// can be rotated: "" when it can't be read or is shorter than 16
// characters, which is logged once.
func (s *Server) secret(file, what string) string {
	b, err := os.ReadFile(file)
	v := strings.TrimSpace(string(b))
	bad := ""
	switch {
	case err != nil || len(v) < minToken:
		bad = "the " + what + " can't be read, or is shorter than 16 characters: no notifications are taken with it until it is put right"
	case what == "notifications token" && s.reused(v):
		// It is in Azure's and Google Cloud's webhook URLs: one that may
		// write incidents or firings too would be handed out with them.
		bad = "the notifications token is also the incidents, the firings or the changes token: none is taken with it until each has its own"
	}
	s.port.warnedMu.Lock()
	defer s.port.warnedMu.Unlock()
	if bad != "" {
		if !s.port.warned[file] {
			s.port.warned[file] = true
			s.log.Warn(bad, "err", err)
		}
		return ""
	}
	delete(s.port.warned, file)
	return v
}

// reused reports whether a token is the incidents, firings, tasks or changes
// token.
func (s *Server) reused(v string) bool {
	for _, f := range []string{s.cfg.IncidentsToken, s.cfg.FiringsToken, s.cfg.TasksToken, s.cfg.ChangesToken} {
		if f == "" {
			continue
		}
		if b, err := os.ReadFile(f); err == nil && strings.TrimSpace(string(b)) == v {
			return true
		}
	}
	return false
}

// same compares two tokens, hashed first, so it takes as long whatever
// their lengths.
func same(got, want string) bool {
	if got == "" || want == "" {
		return false
	}
	x, y := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}

// bearerOK reports whether a request carries the token in
// --notify-token-file as a bearer token.
func (s *Server) bearerOK(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && s.cfg.NotifyToken != "" && same(got, s.secret(s.cfg.NotifyToken, "notifications token"))
}

// grafanaCould reports whether a request says it is signed by Grafana: a
// signature, with a secret to check it by.
func (s *Server) grafanaCould(r *http.Request) bool {
	return s.cfg.NotifyGrafanaSecret != "" && r.Header.Get("X-Grafana-Alerting-Signature") != ""
}

// snsCould reports whether a request says it is from SNS, of a topic taken:
// SNS names the message's type and topic in headers as well as the body.
func (s *Server) snsCould(r *http.Request) bool {
	return len(s.cfg.NotifySNS) > 0 && r.Header.Get("X-Amz-Sns-Message-Type") != "" &&
		slices.Contains(s.cfg.NotifySNS, r.Header.Get("X-Amz-Sns-Topic-Arn"))
}

// refuseSNS answers a request that isn't from SNS, of a topic taken: alike
// whether the topic isn't taken or the signature doesn't hold.
func (s *Server) refuseSNS(w http.ResponseWriter) {
	if len(s.cfg.NotifySNS) == 0 {
		http.Error(w, "SNS isn't taken: give its topics in --notify-sns", http.StatusNotFound)
		return
	}
	http.Error(w, snsRefused, http.StatusForbidden)
}

// snsRefused is what a message not taken from SNS is told.
const snsRefused = "not a message from a topic in --notify-sns, signed by SNS"

func refuseBearer(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, "this path takes the token in --notify-token-file, as a bearer token", http.StatusUnauthorized)
}

// basicOK reports whether a request carries the token as the password of
// basic authentication, whatever the user, as an action group's or a
// webhook channel's URL carries it. Google Cloud sends the first request
// without it, and the password only once asked for with a 401.
func (s *Server) basicOK(r *http.Request) bool {
	_, got, ok := r.BasicAuth()
	return ok && s.cfg.NotifyToken != "" && same(got, s.secret(s.cfg.NotifyToken, "notifications token"))
}

func refuseBasic(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="lapilli notifications", charset="UTF-8"`)
	http.Error(w, "this path takes the token in --notify-token-file as the password of basic authentication", http.StatusUnauthorized)
}

// grafanaSigned takes Grafana's HMAC-SHA256 signature of the body with the
// secret in --notify-grafana-secret-file, for a request without the token:
// in X-Grafana-Alerting-Signature, and, when Grafana sends a timestamp in
// X-Grafana-Alerting-Timestamp, signed with it, and no more than five
// minutes off. The signature is the message's key: the same one again is
// a replay.
func (s *Server) grafanaSigned(w http.ResponseWriter, r *http.Request, body []byte) (bool, string, time.Duration) {
	if sig := r.Header.Get("X-Grafana-Alerting-Signature"); sig != "" && s.cfg.NotifyGrafanaSecret != "" {
		if secret := s.secret(s.cfg.NotifyGrafanaSecret, "Grafana signing secret"); secret != "" {
			mac := hmac.New(sha256.New, []byte(secret))
			ts := r.Header.Get("X-Grafana-Alerting-Timestamp")
			fresh := true
			if ts != "" {
				n, err := strconv.ParseInt(ts, 10, 64)
				d := s.now().Sub(time.Unix(n, 0))
				fresh = err == nil && d < 5*time.Minute && d > -5*time.Minute
				mac.Write([]byte(ts + ":"))
			}
			mac.Write(body)
			want := mac.Sum(nil)
			got, err := hex.DecodeString(strings.TrimSpace(sig))
			if err == nil && fresh && hmac.Equal(got, want) {
				return true, hex.EncodeToString(want), grafanaReplay
			}
		}
	}
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, "this path takes the token in --notify-token-file as a bearer token, or Grafana's HMAC signature with the secret in --notify-grafana-secret-file", http.StatusUnauthorized)
	return false, "", 0
}

// The alerts of Alertmanager's and Grafana's webhooks, and of what AWS's
// JSON template for Amazon Managed Service for Prometheus sends to SNS.
type amMessage struct {
	Version            string            `json:"version"`
	GroupKey           string            `json:"groupKey"`
	Status             string            `json:"status"`
	Receiver           string            `json:"receiver"`
	GroupLabels        map[string]string `json:"groupLabels"`
	NotificationReason string            `json:"notification_reason"`
	Alerts             []amAlert         `json:"alerts"`
}

type amAlert struct {
	Status   string            `json:"status"`
	Labels   map[string]string `json:"labels"`
	StartsAt string            `json:"startsAt"`
}

// amReasons are Alertmanager's notification reasons (0.32.0 on), as the
// ledger keeps them; one not here is worked out.
var amReasons = map[string]string{
	"first notification":      ledger.ReasonFirst,
	"new alerts added":        ledger.ReasonNew,
	"some alerts resolved":    ledger.ReasonResolved,
	"all alerts resolved":     ledger.ReasonResolved,
	"repeat interval elapsed": ledger.ReasonRepeat,
}

// startsAt reads an alert's start: RFC 3339, or as a Go template writes a
// time, as AWS's JSON template for SNS does ("2026-11-09 03:10:00 +0000
// UTC").
func startsAt(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	if i := strings.Index(s, " m="); i > 0 {
		s = s[:i] // a monotonic reading
	}
	for _, layout := range []string{"2006-01-02 15:04:05.999999999 -0700 MST", "2006-01-02 15:04:05.999999999 -0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("startsAt %q isn't a time", s)
}

// receiverOf is the receiver a notification names, or the one the URL
// gives (?receiver=) for a source whose notifications don't.
func receiverOf(r *http.Request, named string) string {
	if named != "" {
		return named
	}
	return r.URL.Query().Get("receiver")
}

func (m amMessage) notification(r *http.Request, now time.Time) (ledger.Notification, error) {
	n := ledger.Notification{At: now, Receiver: receiverOf(r, m.Receiver), Group: m.GroupKey, Reason: amReasons[m.NotificationReason]}
	if len(m.Alerts) > ledger.MaxAlerts {
		return n, tooMany{len(m.Alerts)}
	}
	if len(m.Alerts) == 0 {
		return n, errors.New("no alerts")
	}
	if n.Group == "" {
		// What it was grouped by stands for the group key.
		b, _ := json.Marshal(noneIfNil(m.GroupLabels))
		n.Group = string(b)
	}
	for i, a := range m.Alerts {
		rule := a.Labels["alertname"]
		if rule == "" {
			return n, fmt.Errorf("alert %d has no alertname", i+1)
		}
		start, err := startsAt(a.StartsAt)
		if err != nil {
			return n, fmt.Errorf("alert %d: %w", i+1, err)
		}
		if start.IsZero() {
			// An alert manager always says when an alert started; a start the
			// ledger works out is for a cloud's message that doesn't.
			return n, fmt.Errorf("alert %d doesn't say when it started", i+1)
		}
		n.Alerts = append(n.Alerts, ledger.Alert{Rule: rule, Labels: a.Labels, Start: start, Resolved: a.Status == "resolved"})
	}
	return n, nil
}

func noneIfNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func parseAlertmanager(r *http.Request, body []byte, now time.Time) ([]ledger.Notification, error) {
	var m amMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("not Alertmanager's webhook JSON: %w", err)
	}
	if m.Version != "4" {
		return nil, fmt.Errorf("version %q: want Alertmanager's webhook, version 4", m.Version)
	}
	n, err := m.notification(r, now)
	return []ledger.Notification{n}, err
}

// parseGrafana reads Grafana's webhook. Its alerts are named by alertname,
// the rule's title, as Lapilli names Grafana's rules; it says no reason.
func parseGrafana(r *http.Request, body []byte, now time.Time) ([]ledger.Notification, error) {
	var m amMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("not Grafana's webhook JSON: %w", err)
	}
	if m.Version != "1" {
		return nil, fmt.Errorf("version %q: want Grafana's webhook, version 1, without a custom payload", m.Version)
	}
	m.NotificationReason = ""
	n, err := m.notification(r, now)
	return []ledger.Notification{n}, err
}

// azureAlert is an Azure Monitor alert in the common alert schema.
type azureAlert struct {
	SchemaID string `json:"schemaId"`
	Data     struct {
		Essentials struct {
			AlertID          string   `json:"alertId"`
			AlertRule        string   `json:"alertRule"`
			Severity         string   `json:"severity"`
			MonitorCondition string   `json:"monitorCondition"`
			FiredDateTime    string   `json:"firedDateTime"`
			AlertTargetIDs   []string `json:"alertTargetIDs"`
		} `json:"essentials"`
		AlertContext struct {
			Labels    map[string]string `json:"labels"`
			Condition struct {
				AllOf []struct {
					Dimensions []struct {
						Name  string `json:"name"`
						Value any    `json:"value"`
					} `json:"dimensions"`
				} `json:"allOf"`
			} `json:"condition"`
		} `json:"alertContext"`
	} `json:"data"`
}

// parseAzure reads an action group's webhook in the common alert schema: a
// Prometheus rule group's alert carries its labels, the rule's severity
// among them as Sev0 to Sev4; other alerts carry none.
func parseAzure(r *http.Request, body []byte, now time.Time) ([]ledger.Notification, error) {
	var a azureAlert
	if err := json.Unmarshal(body, &a); err != nil {
		return nil, fmt.Errorf("not Azure Monitor's alert JSON: %w", err)
	}
	if a.SchemaID != "azureMonitorCommonAlertSchema" {
		return nil, fmt.Errorf("schemaId %q: turn on the common alert schema for this action", a.SchemaID)
	}
	e := a.Data.Essentials
	if e.AlertRule == "" {
		return nil, errors.New("no alertRule")
	}
	start, err := time.Parse(time.RFC3339Nano, e.FiredDateTime)
	if err != nil {
		return nil, fmt.Errorf("firedDateTime %q isn't a time", e.FiredDateTime)
	}
	labels := map[string]string{}
	for k, v := range a.Data.AlertContext.Labels {
		labels[k] = v
	}
	if len(labels) == 0 {
		// Not a Prometheus rule's: the labels recipes/firings/azure-monitor.jq
		// gives the alert's firing, so the two tie. The resource is the one
		// the alert is on, the last part of its target's id, and its group as
		// that id names it: not configurationItems, which for a log search
		// alert are what the query found. Azure writes the two in either
		// case, so both are lower-cased, here and in the recipe.
		if len(e.AlertTargetIDs) > 0 {
			parts := strings.Split(strings.TrimRight(e.AlertTargetIDs[0], "/"), "/")
			for i := 0; i+1 < len(parts); i++ {
				if strings.EqualFold(parts[i], "resourceGroups") {
					labels["resource_group"] = ledger.ASCIILower(parts[i+1])
					break
				}
			}
			if last := parts[len(parts)-1]; last != "" {
				labels["resource"] = ledger.ASCIILower(last)
			}
		}
		if all := a.Data.AlertContext.Condition.AllOf; len(all) > 0 {
			for _, d := range all[0].Dimensions {
				if d.Name != "" {
					labels[d.Name] = fmt.Sprint(d.Value)
				}
			}
		}
	}
	if _, ok := labels["severity"]; !ok && e.Severity != "" {
		labels["severity"] = e.Severity
	}
	return []ledger.Notification{{At: now, Receiver: receiverOf(r, ""), Group: e.AlertID,
		Alerts: []ledger.Alert{{Rule: e.AlertRule, Labels: labels, Start: start, Resolved: e.MonitorCondition == "Resolved"}}}}, nil
}

// googleIncident is Cloud Monitoring's webhook, schema 1.2.
type googleIncident struct {
	Version  string `json:"version"`
	Incident struct {
		IncidentID       string            `json:"incident_id"`
		Renotify         bool              `json:"renotify"`
		StartedAt        *int64            `json:"started_at"`
		State            string            `json:"state"`
		Severity         string            `json:"severity"`
		PolicyName       string            `json:"policy_name"`
		PolicyUserLabels map[string]string `json:"policy_user_labels"`
		Resource         struct {
			Labels map[string]string `json:"labels"`
		} `json:"resource"`
		Metric struct {
			Labels map[string]string `json:"labels"`
		} `json:"metric"`
		Condition struct {
			PromQL *struct {
				AlertRule string `json:"alertRule"`
			} `json:"conditionPrometheusQueryLanguage"`
		} `json:"condition"`
	} `json:"incident"`
}

// parseGoogle reads a webhook channel's notification: a policy's incident,
// named as Lapilli names the policy (its PromQL condition's alertRule, or
// the policy's display name), labelled as recipes/firings labels it.
func parseGoogle(r *http.Request, body []byte, now time.Time) ([]ledger.Notification, error) {
	var g googleIncident
	if err := json.Unmarshal(body, &g); err != nil {
		return nil, fmt.Errorf("not Cloud Monitoring's webhook JSON: %w", err)
	}
	if g.Version != "1.2" {
		return nil, fmt.Errorf("version %q: want the webhook's schema 1.2", g.Version)
	}
	in := g.Incident
	rule := in.PolicyName
	if in.Condition.PromQL != nil && in.Condition.PromQL.AlertRule != "" {
		rule = in.Condition.PromQL.AlertRule
	}
	if rule == "" || in.StartedAt == nil {
		return nil, errors.New("an incident needs policy_name and started_at")
	}
	labels := map[string]string{}
	for _, m := range []map[string]string{in.PolicyUserLabels, in.Resource.Labels, in.Metric.Labels} {
		for k, v := range m {
			labels[k] = v
		}
	}
	if sev := strings.ToLower(in.Severity); slices.Contains([]string{"critical", "error", "warning"}, sev) {
		labels["severity"] = sev
	}
	n := ledger.Notification{At: now, Receiver: receiverOf(r, ""), Group: in.IncidentID,
		Alerts: []ledger.Alert{{Rule: rule, Labels: labels, Start: time.Unix(*in.StartedAt, 0).UTC(), Resolved: in.State == "closed"}}}
	if in.Renotify {
		n.Reason = ledger.ReasonRepeat
	}
	return []ledger.Notification{n}, nil
}

// snsMessage is what Amazon SNS posts to an HTTPS subscription.
type snsMessage struct {
	Type             string  `json:"Type"`
	MessageID        string  `json:"MessageId"`
	Token            string  `json:"Token"`
	TopicArn         string  `json:"TopicArn"`
	Subject          *string `json:"Subject"`
	Message          string  `json:"Message"`
	Timestamp        string  `json:"Timestamp"`
	SignatureVersion string  `json:"SignatureVersion"`
	Signature        string  `json:"Signature"`
	SigningCertURL   string  `json:"SigningCertURL"`
	SubscribeURL     string  `json:"SubscribeURL"`
}

// signed is what SNS signs: the message's fields, each name and value on a
// line of its own, in this order.
func (m snsMessage) signed() []byte {
	var b strings.Builder
	add := func(k, v string) { b.WriteString(k + "\n" + v + "\n") }
	add("Message", m.Message)
	add("MessageId", m.MessageID)
	if m.Type == "Notification" {
		if m.Subject != nil {
			add("Subject", *m.Subject)
		}
	} else {
		add("SubscribeURL", m.SubscribeURL)
	}
	add("Timestamp", m.Timestamp)
	if m.Type != "Notification" {
		add("Token", m.Token)
	}
	add("TopicArn", m.TopicArn)
	add("Type", m.Type)
	return []byte(b.String())
}

// snsHost is an Amazon SNS endpoint's host name: certificates come from
// one, subscriptions are confirmed at one, and nowhere else. The part
// between is a region's name (us-east-1, us-gov-west-1, cn-north-1), not
// anything: sns.s3.amazonaws.com is the S3 bucket named sns, which anyone
// may own.
var snsHost = regexp.MustCompile(`^sns\.(?:[a-z]{2}(?:-gov)?-[a-z]+-[0-9]{1,2}\.amazonaws\.com(?:\.cn)?|eusc-[a-z]{2}-[a-z]+-[0-9]{1,2}\.amazonaws\.eu)$`)

// snsCertPath is where SNS keeps a signing certificate on such a host, as
// SNS names it: lowercase hex, so one certificate has one spelling.
var snsCertPath = regexp.MustCompile(`^/SimpleNotificationService-[0-9a-f]{16,64}\.pem$`)

// snsPartitions are the host names' ends of the partitions SNS is in, by
// the partition a topic's ARN names.
var snsPartitions = map[string]string{
	"aws": ".amazonaws.com", "aws-us-gov": ".amazonaws.com", "aws-cn": ".amazonaws.com.cn", "aws-eusc": ".amazonaws.eu",
}

// snsHostOf is the SNS host of a topic's region, as its ARN names it
// (arn:aws:sns:us-east-1:123456789012:alerts): its messages are signed with
// a certificate from there.
func snsHostOf(topicArn string) (string, bool) {
	parts := strings.Split(topicArn, ":")
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "sns" {
		return "", false
	}
	end, ok := snsPartitions[parts[1]]
	host := "sns." + parts[3] + end
	return host, ok && snsHost.MatchString(host)
}

// snsURL reports whether a URL is https to an SNS host.
func snsURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.User == nil && u.Port() == "" && snsHost.MatchString(u.Host)
}

// isSNSCert reports whether a URL is a signing certificate on a topic's
// SNS host, spelled the one way SNS spells it: nothing escaped, nothing
// added, so one certificate is one URL to fetch and to keep.
func isSNSCert(raw, topicArn string) bool {
	host, ok := snsHostOf(topicArn)
	if !ok {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.RawPath == "" && snsCertPath.MatchString(u.Path) && raw == "https://"+host+u.Path
}

// httpStatus is an answer other than 200 from an SNS host.
type httpStatus struct {
	url  string
	code int
}

func (e httpStatus) Error() string {
	return fmt.Sprintf("%s: %d %s", e.url, e.code, http.StatusText(e.code))
}

// snsGet fetches from an SNS host, following no redirect.
func snsGet(ctx context.Context, raw string) ([]byte, error) {
	if !snsURL(raw) {
		return nil, fmt.Errorf("%q isn't an SNS host", raw)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	resp, err := fetcher(req).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpStatus{raw, resp.StatusCode}
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<10))
}

// cert is the certificate at an SNS URL, fetched if it isn't kept. One
// that couldn't be fetched for now, or past the fetches allowed, is
// errSNSLater.
func (t *snsTrust) cert(ctx context.Context, raw string) (*x509.Certificate, error) {
	t.mu.Lock()
	c := t.certs[raw]
	if c != nil {
		t.used[raw] = time.Now()
	}
	t.mu.Unlock()
	if c != nil {
		return c, nil
	}
	// One fetch at a time; another waiting for it may find it fetched.
	t.fetchMu.Lock()
	defer t.fetchMu.Unlock()
	t.mu.Lock()
	c = t.certs[raw]
	f, tried := t.failed[raw]
	t.mu.Unlock()
	switch {
	case c != nil:
		return c, nil
	case tried && f.transient && time.Since(f.at) < 30*time.Second:
		return nil, fmt.Errorf("%w: that SigningCertURL couldn't be fetched a moment ago", errSNSLater)
	case tried && !f.transient && time.Since(f.at) < 10*time.Minute:
		return nil, errors.New("that SigningCertURL gave no certificate lately")
	case !t.fetches.allow(time.Now()):
		return nil, fmt.Errorf("%w: too many signing certificates fetched lately", errSNSLater)
	}
	c, err := t.fetch(ctx, raw)
	if err != nil {
		// What isn't a certificate, or an SNS host's 4xx, won't be one soon;
		// after a timeout or a 5xx, it may.
		var st httpStatus
		transient := !errors.Is(err, errNotCert) && (!errors.As(err, &st) || st.code >= 500 || st.code == http.StatusTooManyRequests)
		t.mu.Lock()
		if len(t.failed) >= 64 {
			clear(t.failed)
		}
		t.failed[raw] = failure{time.Now(), transient}
		t.mu.Unlock()
		if transient {
			return nil, fmt.Errorf("%w: %v", errSNSLater, err)
		}
		// A URL with no certificate behind it costs SNS's host one answer,
		// and is remembered: it doesn't use up the fetches a certificate
		// SNS has just begun to sign with needs. What reaches the host is
		// bounded by the source's rate, one fetch at a time.
		t.fetches.give()
		return nil, err
	}
	t.keep(raw, c, false)
	return c, nil
}

// errNotCert is an answer from an SNS host that isn't a certificate.
var errNotCert = errors.New("the signing certificate isn't a PEM certificate")

// fetch gets and reads a signing certificate.
func (t *snsTrust) fetch(ctx context.Context, raw string) (*x509.Certificate, error) {
	b, err := t.get(ctx, raw)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errNotCert
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNotCert, err)
	}
	return c, nil
}

// keep keeps a certificate, 32 at most: one not proven makes way, the one
// used longest ago, for any; a proven one only for another proven one.
// One that finds no room isn't kept.
func (t *snsTrust) keep(raw string, c *x509.Certificate, proven bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.certs[raw]; !ok && len(t.certs) >= 32 {
		oldest := ""
		for _, pass := range []bool{false, true} {
			if pass && !proven {
				break
			}
			for k, at := range t.used {
				if t.proven[k] == pass && (oldest == "" || at.Before(t.used[oldest])) {
					oldest = k
				}
			}
			if oldest != "" {
				break
			}
		}
		if oldest == "" {
			return
		}
		delete(t.certs, oldest)
		delete(t.used, oldest)
		delete(t.proven, oldest)
	}
	t.certs[raw], t.used[raw] = c, time.Now()
	if proven {
		t.proven[raw] = true
	}
}

// verify checks a message's signature against its certificate; one that
// holds proves the certificate.
func (t *snsTrust) verify(ctx context.Context, m snsMessage, sig []byte, now time.Time) error {
	c, err := t.cert(ctx, m.SigningCertURL)
	if err != nil {
		return err
	}
	if now.Before(c.NotBefore) || now.After(c.NotAfter) {
		return errors.New("the signing certificate is out of date")
	}
	pub, ok := c.PublicKey.(*rsa.PublicKey)
	if !ok {
		return errors.New("the signing certificate's key isn't RSA")
	}
	switch m.SignatureVersion {
	case "1":
		h := sha1.Sum(m.signed())
		err = rsa.VerifyPKCS1v15(pub, crypto.SHA1, h[:], sig)
	case "2":
		h := sha256.Sum256(m.signed())
		err = rsa.VerifyPKCS1v15(pub, crypto.SHA256, h[:], sig)
	default:
		err = fmt.Errorf("SignatureVersion %q", m.SignatureVersion)
	}
	if err == nil {
		t.keep(m.SigningCertURL, c, true)
	}
	return err
}

// snsTypes are the messages SNS posts to a subscription.
var snsTypes = []string{"Notification", "SubscriptionConfirmation", "UnsubscribeConfirmation"}

// snsMessageOf reads a message SNS posts, and checks what can be checked
// without its certificate: anything may post one, so this comes before
// anything is fetched. It says nothing of which topics are taken.
func snsMessageOf(body []byte, now time.Time) (snsMessage, []byte, error) {
	var m snsMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return m, nil, errors.New("not an SNS message")
	}
	if !slices.Contains(snsTypes, m.Type) {
		return m, nil, fmt.Errorf("an SNS message of Type %q", m.Type)
	}
	if m.SignatureVersion != "1" && m.SignatureVersion != "2" {
		return m, nil, fmt.Errorf("SignatureVersion %q: SNS signs with 1 or 2", m.SignatureVersion)
	}
	sig, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil || len(sig) < 128 || len(sig) > 1024 {
		return m, nil, errors.New("the signature isn't one SNS makes")
	}
	if m.MessageID == "" || len(m.MessageID) > 128 {
		return m, nil, errors.New("an SNS message needs its MessageId")
	}
	at, err := time.Parse(time.RFC3339Nano, m.Timestamp)
	if err != nil {
		return m, nil, fmt.Errorf("Timestamp %q isn't a time", m.Timestamp)
	}
	if d := now.Sub(at); d > snsAge || d < -clockSkew {
		return m, nil, errors.New("a message SNS signed more than an hour ago, or later than now: SNS gives up sooner, so it is sent again by someone else")
	}
	if !isSNSCert(m.SigningCertURL, m.TopicArn) {
		return m, nil, errors.New("SigningCertURL isn't a certificate on the SNS host of the topic's region, as SNS spells it")
	}
	return m, sig, nil
}

// snsSigned takes a message SNS signed, from a topic --notify-sns names. A
// subscription to such a topic is confirmed here, by a GET of its
// SubscribeURL, on an SNS host only: the one request the port makes, with
// fetching certificates. Its key is its MessageId.
func (s *Server) snsSigned(w http.ResponseWriter, r *http.Request, body []byte) (bool, string, time.Duration) {
	if len(s.cfg.NotifySNS) == 0 {
		http.Error(w, "SNS isn't taken: give its topics in --notify-sns", http.StatusNotFound)
		return false, "", 0
	}
	m, sig, err := snsMessageOf(body, s.now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return false, "", 0
	}
	// A topic not taken, and a signature that doesn't hold, are answered
	// alike: the answer doesn't say which topics are.
	err = errors.New("a topic not in --notify-sns, or not the one its headers name")
	if slices.Contains(s.cfg.NotifySNS, m.TopicArn) && r.Header.Get("X-Amz-Sns-Topic-Arn") == m.TopicArn {
		err = s.port.sns.verify(r.Context(), m, sig, s.now())
	}
	if err != nil {
		if now := time.Now().Unix(); now-s.port.snsWarned.Load() >= 60 {
			s.port.snsWarned.Store(now)
			s.log.Warn("refused an SNS message (logged once a minute at most)", "topic", m.TopicArn, "err", err)
		}
		if errors.Is(err, errSNSLater) {
			w.Header().Set("Retry-After", "30")
			http.Error(w, "the signature can't be checked now: try again", http.StatusServiceUnavailable)
			return false, "", 0
		}
		http.Error(w, snsRefused, http.StatusForbidden)
		return false, "", 0
	}
	switch m.Type {
	case "Notification":
		return true, m.MessageID, snsAge
	case "SubscriptionConfirmation":
		if !snsURL(m.SubscribeURL) {
			http.Error(w, "SubscribeURL isn't on an SNS host", http.StatusForbidden)
			return false, "", 0
		}
		if _, err := s.port.sns.get(r.Context(), m.SubscribeURL); err != nil {
			s.log.Warn("couldn't confirm an SNS subscription", "topic", m.TopicArn, "err", err)
			http.Error(w, "couldn't confirm the subscription", http.StatusBadGateway)
			return false, "", 0
		}
		s.log.Info("confirmed an SNS subscription", "topic", m.TopicArn)
	case "UnsubscribeConfirmation":
		s.log.Info("an SNS subscription was taken away", "topic", m.TopicArn)
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, "{\"kept\":0,\"same_as_kept\":0}\n")
	return false, "", 0
}

// parseSNS reads the alerts in an SNS notification, told apart by their
// shape: what Amazon Managed Service for Prometheus's alert manager sends
// with AWS's JSON template (version, alerts), a CloudWatch alarm's message
// (AlarmArn, NewStateValue), or its state change as EventBridge writes it
// (detail-type). The message's id tells the same message sent again.
func (s *Server) parseSNS(r *http.Request, body []byte, now time.Time) ([]ledger.Notification, error) {
	var m snsMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	if m.MessageID == "" {
		return nil, errors.New("an SNS message without its MessageId")
	}
	var shape struct {
		AlarmArn      string `json:"AlarmArn"`
		NewStateValue string `json:"NewStateValue"`
		DetailType    string `json:"detail-type"`
	}
	if err := json.Unmarshal([]byte(m.Message), &shape); err != nil {
		return nil, errors.New("the SNS message isn't JSON: give the alert manager AWS's JSON template for SNS, and keep raw message delivery off")
	}
	var ns []ledger.Notification
	var err error
	switch {
	case shape.AlarmArn != "" && shape.NewStateValue != "":
		ns, err = cloudWatch(r, []byte(m.Message), now)
	case shape.DetailType == "CloudWatch Alarm State Change" || shape.DetailType == "CloudWatch Alarm Contributor State Change":
		ns, err = cloudWatchEvent(r, []byte(m.Message), now)
	case strings.HasPrefix(shape.DetailType, "CloudWatch Alarm "):
		// Another of CloudWatch's events, such as a change to an alarm's
		// configuration: of no firing, kept as nothing.
		return nil, nil
	default:
		var am amMessage
		if err := json.Unmarshal([]byte(m.Message), &am); err != nil {
			return nil, errors.New("the SNS message isn't JSON: give the alert manager AWS's JSON template for SNS")
		}
		var n ledger.Notification
		n, err = am.notification(r, now)
		ns = []ledger.Notification{n}
	}
	for i := range ns {
		ns[i].Msg = "sns:" + m.MessageID
	}
	return ns, err
}
