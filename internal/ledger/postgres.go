package ledger

// The ledger in a Postgres database (docs/design/postgres.md): the same
// tables, the same statements but where the two differ (dialect.go), one
// writer as SQLite has, and an advisory lock that keeps a second process
// from writing the same ledger.

import (
	"context"
	"crypto/tls"
	"database/sql"
	"database/sql/driver"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"net"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

//go:embed schema/postgres/*.sql
var pgSchema embed.FS

// ErrBadURL is a connection string pgx doesn't read. It never says what
// the string held: an invalid one can be too ambiguous for its password to
// be found and kept out.
var ErrBadURL = errors.New("not a Postgres connection string (postgres://USER:PASSWORD@HOST:PORT/DATABASE?sslmode=verify-full, or keyword=value pairs)")

// ErrUnchecked is a connection string to a server on another machine that
// would send the password to anything in the path that declines TLS: pgx's
// sslmode=prefer, its default (pgConfig).
var ErrUnchecked = errors.New("the ledger's connection string asks for no sslmode that keeps its password")

// ErrConnect is a connection to the ledger's database that failed.
var ErrConnect = errors.New("can't connect to the ledger's database")

// ErrLockLost is a ledger whose lock this process held and can't be sure
// of again: its connection dropped, and another process took the lock, or
// took it and let it go, in between. Nothing more is written: what this one
// knows of the ledger may no longer be so.
var ErrLockLost = errors.New("the ledger's lock was lost: another process may have written the ledger since; stopping")

// LockedError is a ledger another process writes: one process writes a
// ledger at a time, so that, among the rest, no notice is sent twice.
type LockedError struct {
	PID   int64     // the Postgres session that holds the lock
	Role  string    // the role it connected as, when the server says
	App   string    // its application_name, which it chose itself
	From  string    // its client's address, when the server says
	Since time.Time // when it connected, when the server says
}

func (e *LockedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "the ledger is in use by another process: Postgres session %d", e.PID)
	var of []string
	if e.Role != "" {
		of = append(of, "role "+strconv.Quote(e.Role))
	}
	if e.App != "" {
		of = append(of, "named "+strconv.Quote(e.App))
	}
	if e.From != "" {
		of = append(of, "from "+e.From)
	}
	if !e.Since.IsZero() {
		of = append(of, "connected "+e.Since.UTC().Format(time.RFC3339))
	}
	if len(of) > 0 {
		b.WriteString(" (" + strings.Join(of, ", ") + ")")
	}
	b.WriteString(" holds its lock, and one process writes a ledger at a time. Stop it, or end that session")
	return b.String()
}

// lockClass is the first of the advisory lock's two keys, the same for
// every ledger ("lapl"); the second is the ledger's schema's.
const lockClass = 0x6c61706c

// pgWriter is the writer's side of a Postgres ledger: the session that
// holds its lock, taken on every connection the writer makes.
type pgWriter struct {
	mu sync.Mutex
	// key is the advisory lock's second key: a hash of the schema the
	// ledger is in, so that ledgers in two schemas of one database each
	// have one.
	key int32
	// pid is the session that holds the lock for this process, once one
	// does; own, every session the writer made, before it asked for the
	// lock (the answer can be lost on its way): should one hold it after
	// its connection dropped, which the server can take minutes to notice,
	// it is this process's own.
	pid uint32
	own map[uint32]bool
	// writers is schema_version.writers once this process took the lock:
	// each process that takes it counts one more. Another connection that
	// finds another count finds the ledger written by someone else since.
	writers int64
	// again is set by each connection after the first: the caches are
	// read again from the ledger before the next write.
	again atomic.Bool
	lost  chan struct{}
	once  sync.Once
	// closed is set once the ledger is closed: no connection is made
	// again to take the lock.
	closed atomic.Bool
}

// release lets the lock go, when the writer's connection still holds it.
func (w *pgWriter) release(db *sql.DB) {
	if w.closed.Swap(true) {
		return
	}
	w.mu.Lock()
	held := w.pid != 0
	w.mu.Unlock()
	if !held {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db.ExecContext(ctx, "SELECT pg_advisory_unlock(?, ?)", int32(lockClass), w.key)
}

func (w *pgWriter) lose() { w.once.Do(func() { close(w.lost) }) }

// connecting refuses a connection the writer would make once the ledger is
// closed or its lock lost, before it reaches the server.
func (w *pgWriter) connecting(context.Context, *pgx.ConnConfig) error {
	if w.isLost() {
		return ErrLockLost
	}
	if w.closed.Load() {
		return errors.New("the ledger is closed")
	}
	return nil
}

func (w *pgWriter) isLost() bool {
	select {
	case <-w.lost:
		return true
	default:
		return false
	}
}

// connected readies a connection of the writer's: a transaction reads what
// was committed before each of its statements, as SQLite's single writer
// does (no one else writes), and the lock is this session's, or nothing is
// written through it.
func (w *pgWriter) connected(ctx context.Context, c *pgx.Conn) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.isLost() {
		return ErrLockLost
	}
	if w.closed.Load() {
		return errors.New("the ledger is closed")
	}
	if _, err := c.Exec(ctx, "SET default_transaction_isolation = 'read committed'"); err != nil {
		return err
	}
	if _, err := c.Exec(ctx, "SET default_transaction_read_only = off"); err != nil {
		return err
	}
	// So that the server ends this session about a minute after its client
	// is gone, rather than hours, and the lock is free for the pod that
	// replaces this one. Not every server takes them (they need TCP); none
	// is needed.
	for _, s := range []string{"SET tcp_keepalives_idle = 30", "SET tcp_keepalives_interval = 10", "SET tcp_keepalives_count = 3"} {
		c.Exec(ctx, s)
	}
	schema, err := pinSchema(ctx, c)
	if err != nil {
		return err
	}
	if w.key == 0 {
		h := fnv.New32a()
		h.Write([]byte(schema))
		w.key = int32(h.Sum32()&0x7fffffff) | 1
	}
	// This process's own from now, should the lock be taken and the answer
	// not reach it.
	w.own[c.PgConn().PID()] = true
	took, err := w.try(ctx, c)
	if err != nil {
		return err
	}
	if !took {
		// One of this process's own sessions, from before a connection
		// dropped (maybe while it was made), may hold it still, which the
		// server can keep for minutes: it is ended, and the lock taken
		// again. Its client is gone: it writes nothing more.
		if h, err := w.holder(ctx, c); err == nil && h != nil && w.own[uint32(h.PID)] {
			c.Exec(ctx, "SELECT pg_terminate_backend($1, 5000)", h.PID)
			if took, err = w.try(ctx, c); err != nil {
				return err
			}
		}
	}
	if !took {
		h, err := w.holder(ctx, c)
		if err != nil {
			return err
		}
		if w.writers != 0 {
			w.lose()
			return ErrLockLost
		}
		if h == nil {
			h = &LockedError{}
		}
		return h
	}
	if w.writers != 0 {
		var n int64
		if err := c.QueryRow(ctx, "SELECT writers FROM schema_version").Scan(&n); err != nil {
			w.unlock(c)
			return err
		}
		if n != w.writers {
			w.unlock(c)
			w.lose()
			return ErrLockLost
		}
		w.again.Store(true)
	}
	w.pid = c.PgConn().PID()
	return nil
}

// unlock lets the lock go on a session that took it, with a context of its
// own: the connection's may be what ran out.
func (w *pgWriter) unlock(c *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.Exec(ctx, "SELECT pg_advisory_unlock($1, $2)", int32(lockClass), w.key)
}

// pinSchema readies a connection's statements for the ledger's schema
// alone: the first schema of its search_path (current_schema()), where the
// ledger is made if there is none. A ledger further along the path is
// refused: the lock is the first schema's, and would let a second writer
// at the tables found further on.
func pinSchema(ctx context.Context, c *pgx.Conn) (string, error) {
	var schema, held *string
	if err := c.QueryRow(ctx, `SELECT current_schema(), (SELECT n.nspname FROM pg_class t JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE t.oid = to_regclass('schema_version'))`).Scan(&schema, &held); err != nil {
		return "", err
	}
	if schema == nil {
		return "", fmt.Errorf("%w: no schema on the search_path exists to keep the ledger in", ErrConnect)
	}
	if held != nil && *held != *schema {
		return "", fmt.Errorf("%w: the search_path finds the ledger in schema %q, not in its first, %q: name %q first", ErrConnect, *held, *schema, *held)
	}
	if _, err := c.Exec(ctx, "SELECT set_config('search_path', quote_ident($1), false)", *schema); err != nil {
		return "", err
	}
	return *schema, nil
}

func (w *pgWriter) try(ctx context.Context, c *pgx.Conn) (bool, error) {
	var took bool
	err := c.QueryRow(ctx, "SELECT pg_try_advisory_lock($1, $2)", int32(lockClass), w.key).Scan(&took)
	return took, err
}

// holder is the session that holds the lock, nil when none does.
func (w *pgWriter) holder(ctx context.Context, c *pgx.Conn) (*LockedError, error) {
	var h LockedError
	var since *time.Time
	err := c.QueryRow(ctx, `SELECT l.pid, coalesce(a.usename, ''), coalesce(a.application_name, ''), coalesce(host(a.client_addr), ''), a.backend_start
		FROM pg_locks l LEFT JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 2
			AND l.classid::bigint = $1 AND l.objid::bigint = $2
			AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
		LIMIT 1`, int64(lockClass), int64(w.key)).Scan(&h.PID, &h.Role, &h.App, &h.From, &since)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if since != nil {
		h.Since = *since
	}
	return &h, err
}

// How long the ledger waits on its Postgres server, at most. A server can
// stop answering and the connection stay up: behind a proxy that keeps it
// open (an auth proxy, a pooler, a load balancer) while what is past it is
// gone, or a server stopped; a firewall or a NAT that drops what it sends
// holds a statement for as long as TCP retries, some 15 minutes on Linux.
// Neither keepalives nor the server's tcp_keepalives_* see it: TCP is
// well, or data waits for an answer. So the ledger's own deadlines:
var (
	// connectWithin: a connection made, the writer's lock taken on it,
	// on top of what the string's connect_timeout gives pgx.
	connectWithin = 30 * time.Second
	// aliveWithin: a connection taken from a pool again after a second
	// or more answers pgx's ping, or another is made.
	aliveWithin = 10 * time.Second
	// writeWithin: a write's transaction, from its BEGIN to its COMMIT,
	// once it holds the writer. The largest serve makes, a source's week of
	// 100,000 firings, takes some 3 s; a copy and a migration have none.
	writeWithin = 5 * time.Minute
)

type unboundedKey struct{}

// withoutBound marks a context whose write has no deadline of the ledger's
// own (writeWithin): a copy's, which writes a whole ledger in one
// transaction, an operator watching it.
func withoutBound(ctx context.Context) context.Context {
	return context.WithValue(ctx, unboundedKey{}, true)
}

func unbounded(ctx context.Context) bool { return ctx.Value(unboundedKey{}) != nil }

// pgConnector is pgx's, with each statement's parameters written as
// Postgres takes them, and what fails to connect said without the
// connection string. A connection is made within connectWithin, on top of
// the string's connect_timeout, whatever the caller's context.
type pgConnector struct {
	inner   driver.Connector
	secrets []string
	within  time.Duration
}

func (c pgConnector) Connect(ctx context.Context) (driver.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, c.within)
	defer cancel()
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		cause := err
		if ctx.Err() != nil {
			cause = context.Cause(ctx)
		}
		return nil, connectError(err, c.secrets, cause)
	}
	return &pgConn{conn.(*stdlib.Conn)}, nil
}

func (c pgConnector) Driver() driver.Driver { return c.inner.Driver() }

// pgConn is a connection of pgx's whose statements are written as SQLite
// takes them: their parameters, ? and ?NNN, become $N.
type pgConn struct{ *stdlib.Conn }

// ResetSession is pgx's, which pings a connection taken from the pool after
// a second or more, within aliveWithin: one that doesn't answer is given
// up (driver.ErrBadConn), and database/sql makes another, rather than
// wait as long as the caller would.
func (c *pgConn) ResetSession(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, aliveWithin)
	defer cancel()
	return c.Conn.ResetSession(ctx)
}

// CheckNamedValue keeps a bool as 1 or 0, as SQLite keeps it: the
// ledger's flags are integers in both. A text is kept as Postgres can keep
// it (pgText).
func (c *pgConn) CheckNamedValue(v *driver.NamedValue) error {
	switch x := v.Value.(type) {
	case bool:
		v.Value = int64(0)
		if x {
			v.Value = int64(1)
		}
	case string:
		v.Value = pgText(x)
	}
	return nil
}

// pgText is a text as Postgres keeps it: Postgres refuses a NUL and bytes
// that aren't UTF-8, which SQLite keeps, and each is kept as U+FFFD, as
// encoding/json writes bytes that aren't UTF-8 anyway. Nothing lapilli
// makes holds a NUL; what people and alert managers send can.
func pgText(s string) string {
	if !utf8.ValidString(s) || strings.IndexByte(s, 0) >= 0 {
		s = strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", "\uFFFD"), "\uFFFD")
	}
	return s
}

// pgJSON is a JSON text as Postgres can read it as jsonb: pgText, and each
// NUL it holds, written \u0000, which jsonb refuses, written U+FFFD. It is
// what the ledger keeps of a text Postgres reads as jsonb (jsonCols), and
// what is given for a parameter it reads so (CAST(? AS jsonb)); any other
// text is kept as written, a JSON one too, as SQLite keeps it.
func pgJSON(s string) string {
	s = pgText(s)
	if strings.Contains(s, `\u0000`) && json.Valid([]byte(s)) {
		s = jsonNUL(s)
	}
	return s
}

// jsonCols are the columns Postgres reads as jsonb, in an index (the
// alarm's id) or a statement: their texts are kept as pgJSON.
var jsonCols = map[string]bool{"series.labels": true, "notified.labels": true, "changes.before": true, "changes.after": true,
	"versions.docs": true, "versions.names": true}

// asJSONB are the parameters a statement reads as jsonb, CAST($N AS jsonb),
// by statement.
var asJSONB sync.Map

var castJSONB = regexp.MustCompile(`CAST\(\$(\d+) AS jsonb\)`)

// jsonParams gives the parameters a statement reads as jsonb as pgJSON.
func jsonParams(q string, args []driver.NamedValue) {
	v, ok := asJSONB.Load(q)
	if !ok {
		var ns []int
		for _, m := range castJSONB.FindAllStringSubmatch(q, -1) {
			n, _ := strconv.Atoi(m[1])
			ns = append(ns, n)
		}
		v, _ = asJSONB.LoadOrStore(q, ns)
	}
	for _, n := range v.([]int) {
		if n >= 1 && n <= len(args) {
			if s, ok := args[n-1].Value.(string); ok {
				args[n-1].Value = jsonNUL(s)
			}
		}
	}
}

// jsonNUL is a JSON text with each NUL it holds, written \u0000, written
// U+FFFD, as json.Marshal writes it; the rest as it is. A \\u0000, an
// escaped backslash and a u, is left. s is valid JSON.
func jsonNUL(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	in := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case !in:
			in = c == '"'
		case c == '"':
			in = false
		case c == '\\' && i+1 < len(s):
			if strings.HasPrefix(s[i:], `\u0000`) {
				b.WriteString("\uFFFD")
				i += 5
				continue
			}
			b.WriteByte(c)
			i++
			c = s[i]
		}
		b.WriteByte(c)
	}
	return b.String()
}

func (c *pgConn) Prepare(q string) (driver.Stmt, error) {
	q, _ = rebind(q)
	return c.Conn.Prepare(q)
}

func (c *pgConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	q, _ = rebind(q)
	return c.Conn.PrepareContext(ctx, q)
}

// sent counts the statements the connections send: for tests of how many
// round trips a read takes.
var sent atomic.Int64

func (c *pgConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	sent.Add(1)
	if len(args) == 0 {
		// A migration, several statements in one: sent as it is.
		return c.Conn.ExecContext(ctx, q, args)
	}
	q, args = bind(q, args)
	jsonParams(q, args)
	return c.Conn.ExecContext(ctx, q, args)
}

func (c *pgConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	sent.Add(1)
	q, args = bind(q, args)
	jsonParams(q, args)
	return c.Conn.QueryContext(ctx, q, args)
}

// bind is a statement and its arguments as Postgres takes them: those past
// the statement's highest parameter are left out, as SQLite leaves them.
func bind(q string, args []driver.NamedValue) (string, []driver.NamedValue) {
	q, n := rebind(q)
	if len(args) > n {
		args = args[:n]
	}
	return q, args
}

// rebind writes a statement's parameters as Postgres takes them: SQLite's
// ? and ?NNN become $N, numbered as SQLite numbers them (a ? is one more
// than the highest before it). What is quoted, or a comment, is left as
// it is. A statement with parameters uses no ? operator of jsonb's, and no
// dollar-quoted string. It gives the highest parameter's number too.
func rebind(q string) (string, int) {
	if !strings.Contains(q, "?") {
		return q, 0
	}
	var b strings.Builder
	b.Grow(len(q) + 8)
	high := 0
	for i := 0; i < len(q); i++ {
		switch c := q[i]; {
		case c == '\'' || c == '"':
			j := i + 1
			for j < len(q) {
				if q[j] == c {
					if j+1 < len(q) && q[j+1] == c {
						j += 2
						continue
					}
					break
				}
				j++
			}
			end := min(j+1, len(q))
			b.WriteString(q[i:end])
			i = end - 1
		case c == '-' && i+1 < len(q) && q[i+1] == '-':
			j := strings.IndexByte(q[i:], '\n')
			if j < 0 {
				j = len(q) - i
			}
			b.WriteString(q[i : i+j])
			i += j - 1
		case c == '/' && i+1 < len(q) && q[i+1] == '*':
			j := strings.Index(q[i+2:], "*/")
			end := len(q)
			if j >= 0 {
				end = i + 2 + j + 2
			}
			b.WriteString(q[i:end])
			i = end - 1
		case c == '?':
			j := i + 1
			for j < len(q) && q[j] >= '0' && q[j] <= '9' {
				j++
			}
			n := high + 1
			if j > i+1 {
				n, _ = strconv.Atoi(q[i+1 : j])
			}
			high = max(high, n)
			b.WriteString("$" + strconv.Itoa(n))
			i = j - 1
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), high
}

// connectError says why connecting failed, with neither the connection
// string nor its password: the server's address and what it answered.
// cause is what to tell a timeout by: err itself, or the context error that
// stopped it.
func connectError(err error, secrets []string, cause error) error {
	var locked *LockedError
	if errors.As(err, &locked) || errors.Is(err, ErrLockLost) || errors.Is(err, ErrConnect) {
		return err
	}
	var ce *pgconn.ConnectError
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		// Where it was trying, and that it gave up: the server's address
		// alone, as pgx's message says more.
		where := "the server"
		if errors.As(err, &ce) && ce.Config != nil {
			where = net.JoinHostPort(ce.Config.Host, strconv.Itoa(int(ce.Config.Port)))
		}
		if errors.Is(cause, context.Canceled) {
			return fmt.Errorf("%w: connecting to %s was given up (%w)", ErrConnect, where, context.Canceled)
		}
		return fmt.Errorf("%w: connecting to %s timed out (%w)", ErrConnect, where, context.DeadlineExceeded)
	}
	var pe *pgconn.PgError
	var msg string
	switch {
	case errors.As(err, &pe):
		msg = fmt.Sprintf("the server says: %s (SQLSTATE %s)", pe.Message, pe.Code)
	case errors.As(err, &ce):
		// Without its first line, which names the user and the database.
		msg = errors.Unwrap(ce).Error()
	default:
		msg = err.Error()
	}
	return fmt.Errorf("%w: %s", ErrConnect, scrub(msg, secrets))
}

// scrub takes secrets out of a text, should one be there.
func scrub(s string, secrets []string) string {
	for _, x := range secrets {
		if len(x) >= 4 {
			s = strings.ReplaceAll(s, x, "xxxxx")
		}
	}
	return s
}

// secretsOf are what a connection string holds that no error may say: the
// string itself, and its passwords.
func secretsOf(raw string, cfg *pgx.ConnConfig) []string {
	s := []string{strings.TrimSpace(raw)}
	if cfg != nil && cfg.Password != "" {
		s = append(s, cfg.Password, url.QueryEscape(cfg.Password), url.PathEscape(cfg.Password))
	}
	if u, err := url.Parse(strings.TrimSpace(raw)); err == nil {
		if p, ok := u.User.Password(); ok {
			s = append(s, p)
		}
		if p := u.Query().Get("sslpassword"); p != "" {
			s = append(s, p)
		}
	}
	return s
}

// pgConfig reads a connection string as pgx does, its environment
// variables and service file among it, and names the connections lapilli.
// The string is read once, by pgx, and never written again: what TLS it
// asks for is read from the configuration pgx makes of it, to every host
// it may try, in the order it tries them. To a server not on this machine:
//
//   - a certificate checked on every try (verify-ca, verify-full): taken.
//   - pgx's default, prefer (TLS that checks nothing, and none when the
//     server declines it), is refused: anything in the path can decline
//     TLS and be sent the password. No sslmode and sslmode=prefer are one
//     to pgx; telling them apart would mean reading the string again,
//     otherwise than pgx may, and guessing. The string says what it wants.
//   - require, allow or disable, said by the string, a service file or
//     PGSSLMODE: taken at its word, and a warning given (what it returns).
//
// On this machine (a Unix socket, localhost, a loopback address), as given.
func pgConfig(raw string) (*pgx.ConnConfig, string, error) {
	cfg, err := pgx.ParseConfig(strings.TrimSpace(raw))
	if err != nil {
		return nil, "", ErrBadURL
	}
	if _, ok := cfg.RuntimeParams["application_name"]; !ok {
		cfg.RuntimeParams["application_name"] = "lapilli"
	}
	if onThisMachine(cfg) {
		return cfg, "", nil
	}
	type try struct {
		host string
		tls  *tls.Config
	}
	tries := []try{{cfg.Host, cfg.TLSConfig}}
	for _, f := range cfg.Fallbacks {
		tries = append(tries, try{f.Host, f.TLSConfig})
	}
	checked := func(c *tls.Config) bool { return c != nil && (!c.InsecureSkipVerify || c.VerifyPeerCertificate != nil) }
	var hosts []string
	for _, x := range tries {
		if !slices.Contains(hosts, x.host) {
			hosts = append(hosts, x.host)
		}
	}
	mode := "require"
	all := true
	for i, x := range tries {
		all = all && checked(x.tls)
		switch {
		case x.tls != nil && !checked(x.tls) && i+1 < len(tries) && tries[i+1].host == x.host && tries[i+1].tls == nil:
			return nil, "", fmt.Errorf("%w: to %s, TLS that checks no certificate, and none when the server declines it (sslmode=prefer, "+
				"pgx's when no sslmode is given): anything in the path can decline it and be sent the password. Ask for sslmode=verify-full "+
				"(sslrootcert=FILE for the server's CA), or say sslmode=require or sslmode=disable to go on unchecked", ErrUnchecked, strings.Join(hosts, ", "))
		case x.tls == nil && i+1 < len(tries) && tries[i+1].host == x.host && tries[i+1].tls != nil:
			mode = "allow"
		case x.tls == nil && mode != "allow":
			mode = "disable"
		}
	}
	if all {
		return cfg, "", nil
	}
	return cfg, "the ledger's connection doesn't check the Postgres server's certificate (sslmode=" + mode + "): anything in the path can read it, " +
		"and with allow or disable take the password: ask for sslmode=verify-full", nil
}

// onThisMachine says every host a configuration may connect to is this
// machine's: a Unix socket, localhost, or a loopback address.
var onThisMachine = func(cfg *pgx.ConnConfig) bool {
	hosts := []string{cfg.Host}
	for _, f := range cfg.Fallbacks {
		hosts = append(hosts, f.Host)
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); !strings.HasPrefix(h, "/") && h != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return false
		}
	}
	return true
}

// OpenPostgres opens the ledger in the Postgres database a connection
// string names, in its schema (search_path), making it if the database
// holds none, in the time zone its days and weeks are counted in. It takes
// the ledger's lock, and refuses a ledger another process holds
// (*LockedError). No error it gives holds the connection string.
func OpenPostgres(ctx context.Context, conn string, loc *time.Location) (*Ledger, error) {
	cfg, warn, err := pgConfig(conn)
	if err != nil {
		return nil, err
	}
	secrets := secretsOf(conn, cfg)
	pw := &pgWriter{lost: make(chan struct{}), own: map[uint32]bool{}}
	writer := stdlib.GetConnector(*cfg, stdlib.OptionAfterConnect(pw.connected), stdlib.OptionBeforeConnect(pw.connecting))
	w := sql.OpenDB(pgConnector{writer, secrets, cfg.ConnectTimeout + connectWithin})
	// One connection writes, as for SQLite: writes take turns, and the
	// lock is that connection's session's.
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxIdleTime(0)
	w.SetConnMaxLifetime(0)
	reader := stdlib.GetConnector(*cfg, stdlib.OptionAfterConnect(func(ctx context.Context, c *pgx.Conn) error {
		// The ledger's schema alone, as the writer's.
		if _, err := pinSchema(ctx, c); err != nil {
			return err
		}
		// A transaction of reads sees the ledger as of one moment, as
		// SQLite's do, and writes nothing.
		for _, s := range []string{"SET default_transaction_read_only = on", "SET default_transaction_isolation = 'repeatable read'"} {
			if _, err := c.Exec(ctx, s); err != nil {
				return err
			}
		}
		return nil
	}))
	r := sql.OpenDB(pgConnector{reader, secrets, cfg.ConnectTimeout + connectWithin})
	r.SetMaxOpenConns(4)
	r.SetMaxIdleConns(4)
	l := &Ledger{w: w, r: r, loc: loc, pg: pw, d: postgres}
	if warn != "" {
		l.warnings = append(l.warnings, warn)
	}
	l.ids.rules = map[string]int64{}
	l.ids.series = map[seriesKey]int64{}
	l.ids.labels = map[int64]labelsOf{}
	if err := w.PingContext(ctx); err != nil {
		l.Close()
		if !errors.Is(err, ErrConnect) && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
			// database/sql's own, the caller's context done before the
			// connection was: it says where no more than pgx would.
			err = connectError(&pgconn.ConnectError{Config: &cfg.Config}, secrets, err)
		}
		return nil, err
	}
	if err := l.migratePostgres(ctx); err != nil {
		l.Close()
		return nil, err
	}
	// This process writes the ledger from here: one more of them.
	if err := w.QueryRowContext(ctx, "UPDATE schema_version SET writers = writers + 1 RETURNING writers").Scan(&pw.writers); err != nil {
		l.Close()
		return nil, err
	}
	if err := l.zone(ctx); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// Hold says this process still holds the ledger, before it does what
// leaves the process on the ledger's word, such as sending a notice a reader
// found due: in Postgres, that the writer's session holds the lock,
// connecting again (and taking it again, or finding it lost) if the session
// went. A notice sent by a process whose lock went, while another took the
// ledger, would be sent twice. SQLite's, always.
func (l *Ledger) Hold(ctx context.Context) error {
	if l.pg == nil {
		return nil
	}
	if l.pg.isLost() {
		return ErrLockLost
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, writeWithin)
	defer cancel()
	var held bool
	if err := l.w.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND granted AND pid = pg_backend_pid()
		AND objsubid = 2 AND classid::bigint = ? AND objid::bigint = ?)`, int64(lockClass), int64(l.pg.key)).Scan(&held); err != nil {
		return err
	}
	if !held {
		l.pg.lose()
		return ErrLockLost
	}
	return nil
}

// Lost is closed when the ledger's lock is lost for good (ErrLockLost):
// lapilli serve stops. A SQLite ledger's never is.
func (l *Ledger) Lost() <-chan struct{} {
	if l.pg == nil {
		return nil
	}
	return l.pg.lost
}

// Warnings are what serve says at start of how the ledger is reached: a
// connection that doesn't check the server's certificate.
func (l *Ledger) Warnings() []string { return l.warnings }

// Postgres says whether the ledger is in Postgres.
func (l *Ledger) Postgres() bool { return l.pg != nil }

// pgFiles are the Postgres migrations, by the schema each brings the
// ledger to: the lowest is where a Postgres ledger starts, the rest are
// SQLite's of the same number.
func pgFiles() (map[int]string, []int, error) {
	files, err := fs.Glob(pgSchema, "schema/postgres/*.sql")
	if err != nil {
		return nil, nil, err
	}
	m := map[int]string{}
	var ns []int
	for _, f := range files {
		n, err := strconv.Atoi(strings.TrimSuffix(path.Base(f), ".sql"))
		if err != nil {
			return nil, nil, fmt.Errorf("%s: not a numbered migration", f)
		}
		m[n] = f
		ns = append(ns, n)
	}
	slices.Sort(ns)
	return m, ns, nil
}

// sqliteFiles is how many SQLite migrations there are: the schema this
// lapilli writes.
func sqliteFiles() (int, error) {
	files, err := fs.Glob(schema, "schema/*.sql")
	return len(files), err
}

// migratePostgres brings a Postgres ledger's schema up to date: an empty
// database is given the whole schema at once, then each later migration
// in turn. A database holding tables a ledger has but no schema_version
// is refused, not written over.
func (l *Ledger) migratePostgres(ctx context.Context) error {
	m, ns, err := pgFiles()
	if err != nil {
		return err
	}
	want, err := sqliteFiles()
	if err != nil {
		return err
	}
	if len(ns) == 0 || ns[len(ns)-1] != want {
		return fmt.Errorf("the Postgres schema ends at %v, SQLite's at %d", ns, want)
	}
	var have int
	var made bool
	if err := l.w.QueryRowContext(ctx, "SELECT to_regclass('schema_version') IS NOT NULL").Scan(&made); err != nil {
		return err
	}
	if made {
		if err := l.w.QueryRowContext(ctx, "SELECT version FROM schema_version").Scan(&have); err != nil {
			return err
		}
	} else {
		var n int
		if err := l.w.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables
			WHERE table_schema = current_schema() AND table_name IN ('meta', 'firings', 'rules', 'series', 'versions')`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return errors.New("the database's schema holds tables a ledger has, but no schema_version: not a ledger this lapilli made, and not written over")
		}
	}
	if have > want {
		return fmt.Errorf("%w (schema %d; this one knows %d)", ErrNewer, have, want)
	}
	for _, n := range ns {
		if n <= have {
			continue
		}
		b, err := pgSchema.ReadFile(m[n])
		if err != nil {
			return err
		}
		tx, err := l.w.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if have == 0 {
			if _, err := tx.ExecContext(ctx, `CREATE TABLE schema_version (version BIGINT NOT NULL, writers BIGINT NOT NULL DEFAULT 0);
				INSERT INTO schema_version (version) VALUES (0)`); err != nil {
				tx.Rollback()
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, string(b)); err != nil {
			tx.Rollback()
			return fmt.Errorf("%s: %w", m[n], err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE schema_version SET version = "+strconv.Itoa(n)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		have = n
	}
	return nil
}
