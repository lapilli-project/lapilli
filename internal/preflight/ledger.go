package preflight

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/serve"
	"lapilli.dev/internal/signin"
)

// ledger reads the ledger serve would open, through a probe that opens it
// read-only: never ledger.Open, which makes and migrates it.
func (p *run) ledger(ctx context.Context) {
	if p.c.Serve.LedgerURLFile != "" {
		p.postgres(ctx)
		return
	}
	p.sqlite(ctx)
}

// schemaSays is what a ledger's schema means for serve, as it starts.
func schemaSays(pr ledger.Probed) (State, string, string) {
	switch {
	case !pr.There:
		return OK, fmt.Sprintf("a new ledger: serve makes it, at schema %03d", pr.Knows), ""
	case pr.Version == pr.Knows:
		return OK, fmt.Sprintf("schema %03d, this release's", pr.Version), ""
	case pr.Version < pr.Knows:
		return OK, fmt.Sprintf("schema %03d: serve upgrades the ledger from %03d to %03d as it starts, and a release before %03d can't open it after: keep a copy first",
			pr.Version, pr.Version, pr.Knows, pr.Knows), ""
	}
	return Problem, fmt.Sprintf("schema %03d, newer than this release's %03d", pr.Version, pr.Knows),
		"serve refuses a ledger a newer lapilli wrote: run that release, or a newer one"
}

func (p *run) sqlite(ctx context.Context) {
	dir := p.c.Serve.Ledger
	if p.c.Offline {
		p.offline("ledger.sqlite", "the ledger is where serve runs")
		return
	}
	// Where serve would make it, if it isn't there: the nearest directory
	// that is.
	at := dir
	for {
		if _, err := os.Stat(at); err == nil || filepath.Dir(at) == at {
			break
		}
		at = filepath.Dir(at)
	}
	var notes []string
	if fi, err := os.Stat(at); err != nil || !fi.IsDir() {
		p.add(Item{ID: "ledger.sqlite", State: Problem, What: "--ledger " + dir + " isn't a directory serve can keep the ledger in"})
		return
	}
	if err := writable(at); err != nil {
		p.add(Item{ID: "ledger.sqlite", State: Problem, What: "--ledger " + dir + ": " + at + " isn't writable by this user", Why: said(err),
			Fix: "run as the user serve runs as, with its volume; serve can't keep a ledger there"})
		return
	}
	if free, ok := freeBytes(at); ok {
		notes = append(notes, fmt.Sprintf("%s free", bytesOf(free)))
	}
	pr, err := ledger.Probe(ctx, dir)
	if errors.Is(err, ledger.ErrCutShort) {
		p.add(Item{ID: "ledger.sqlite", State: Problem, What: "--ledger " + dir + ": the ledger's file is cut short",
			Why: "ledger.db is shorter than a page of SQLite's, with a write-ahead log (-wal) beside it, which holds the ledger: " +
				"SQLite, opening the file, takes it for an empty database and deletes the log. The probe didn't open it",
			Fix: "keep a copy of the directory, and put ledger.db back from a copy before serve starts: serve would open it, and the log would be lost"})
		return
	}
	if err != nil {
		p.add(Item{ID: "ledger.sqlite", State: Problem, What: "--ledger " + dir + ": the ledger can't be read", Why: said(err)})
		return
	}
	state, what, fix := schemaSays(pr)
	p.add(Item{ID: "ledger.sqlite", State: state, What: what, Why: strings.Join(notes, "; "), Fix: fix})
}

func (p *run) postgres(ctx context.Context) {
	if p.c.Offline {
		p.offline("ledger.postgres", "the ledger's server not asked")
		return
	}
	b, err := os.ReadFile(p.c.Serve.LedgerURLFile)
	if err != nil {
		p.add(Item{ID: "ledger.postgres", State: Problem, What: "--ledger-url-file can't be read", Why: said(err)})
		return
	}
	// The server's name, the user's and the database's are the file's as
	// much as its password is: said in neither what the server answers (a
	// refusal names the user and the database) nor the TLS warning. The
	// lock's holder is said by its session, never its role or its name.
	for _, h := range ledger.Hosts(string(b)) {
		p.hide("--ledger-url-file", h)
	}
	user, database := ledger.Names(string(b))
	p.hideSaid("--ledger-url-file's user", user)
	p.hideSaid("--ledger-url-file's database", database)
	pr, err := ledger.ProbePostgres(ctx, string(b))
	if err != nil {
		p.add(Item{ID: "ledger.postgres", State: Problem, What: "the ledger's database can't be read", Why: said(err),
			Fix: "check the connection string, the server's address and TLS, and the user"})
		return
	}
	p.postgresProbed(pr)
}

// postgresProbed is the item of a Postgres ledger, as its probe read it.
// The schema is said as what the server said is (saidText): it is the
// server's answer, and the search_path's first ("$user") makes a schema
// named after the user the ledger's, whose name the rest of what the
// server said doesn't say.
func (p *run) postgresProbed(pr ledger.Probed) {
	state, what, fix := schemaSays(pr)
	schema := saidText(strconv.Quote(pr.Schema))
	var notes []string
	if pr.Warning != "" {
		notes = append(notes, pr.Warning)
	}
	needsCreate := !pr.There || pr.Version < pr.Knows
	switch {
	case needsCreate && !pr.Create:
		state, fix = Problem, "the user may not create in schema "+schema+", which making or upgrading the ledger needs: grant CREATE on it, or run the first start as its owner"
	case pr.Holder != nil && pr.Holder.Alone:
		state, fix = Problem, "a process holds the ledger alone, as a copy does: serve is refused until it is done"
		notes = append(notes, heldBy(pr.Holder))
	case pr.Holder != nil:
		notes = append(notes, fmt.Sprintf("a serve holds the ledger's lock now (Postgres session %d): replicas share it, and the probe took none", pr.Holder.PID))
	}
	p.add(Item{ID: "ledger.postgres", State: state, What: what + ", in schema " + schema, Why: strings.Join(notes, "; "), Fix: fix})
}

// heldBy says the session that holds the ledger's lock by its number and
// when it connected: not by the role it connected as, nor the name it
// gave itself, nor where it came from, which are a database's user, a
// process's choosing and an address.
func heldBy(h *ledger.LockedError) string {
	shown := *h
	shown.Role, shown.App, shown.From = "", "", ""
	return said(&shown)
}

// signin asks the sign-in what serve would at start: --oidc-issuer's
// configuration and keys, --proxy-jwt's keys.
func (p *run) signin(ctx context.Context) {
	s := p.c.Serve
	switch {
	case s.OIDC != nil:
		if p.c.Offline {
			p.offline("signin", "--oidc-issuer not asked")
			return
		}
		g := s.OIDC
		o := &signin.OIDC{Issuer: g.Issuer, ClientID: g.ClientID, SecretFile: g.SecretFile, Redirect: g.Redirect, Scopes: g.Scopes, Claims: g.Claims,
			HostedDomains: g.HostedDomains, TrustedClients: g.TrustedClients, NoBearer: g.NoBearer, Client: signin.Client(nil), Now: g.Now}
		each, cancel := context.WithTimeout(ctx, eachTime)
		defer cancel()
		keys, err := o.KeysURL(each)
		if err != nil {
			fix := "check --oidc-issuer, and that the pod reaches it"
			if errors.Is(err, signin.ErrMisconfigured) {
				fix = "serve says the same at start, and signs no one in"
			}
			p.add(Item{ID: "signin", State: Problem, What: "--oidc-issuer's configuration won't do", Why: said(err), Fix: fix})
			return
		}
		p.allowURL("signin", keys)
		p.hide("--oidc-issuer's keys", keys)
		n, err := o.FetchKeys(each)
		if err != nil {
			p.add(Item{ID: "signin", State: Problem, What: "--oidc-issuer's keys can't be fetched", Why: said(err)})
			return
		}
		p.add(Item{ID: "signin", State: OK, What: fmt.Sprintf("--oidc-issuer's configuration read, its issuer as given, %s", plural(n, "public key"))})
	case s.Proxy != nil:
		if p.c.Offline {
			p.offline("signin", "--proxy-jwt's keys not fetched")
			return
		}
		px := *s.Proxy
		px.Client = signin.Client(nil)
		p.hide("--proxy-jwt-jwks", px.JWKS)
		p.hide("--proxy-jwt-issuer", px.Issuer)
		pp, err := signin.NewProxy(px)
		if err != nil {
			p.add(Item{ID: "signin", State: Problem, What: "--proxy-jwt won't do", Why: said(err)})
			return
		}
		each, cancel := context.WithTimeout(ctx, eachTime)
		defer cancel()
		n, err := pp.FetchKeys(each)
		switch {
		case errors.Is(err, signin.ErrKeyPerToken):
			p.add(Item{ID: "signin", State: Unknown, What: "--proxy-jwt " + px.Kind + ": its keys aren't fetched before a request comes", Why: said(err)})
		case err != nil:
			p.add(Item{ID: "signin", State: Problem, What: "--proxy-jwt's keys can't be fetched", Why: said(err)})
		default:
			p.add(Item{ID: "signin", State: OK, What: fmt.Sprintf("--proxy-jwt %s: %s fetched", px.Kind, plural(n, "public key"))})
		}
	}
}

// access says who may change what, as serve says at start, and what each
// port takes. Nothing is bound.
func (p *run) access() {
	s := p.c.Serve
	var who string
	signedIn := s.OIDC != nil || s.Proxy != nil
	switch {
	case s.ReadOnly:
		who = "--read-only: no one changes anything through the UI or the API"
	case len(s.Editors)+len(s.Admins) > 0:
		who = "roles: those --editors and --admins name change what their role allows; everyone else may only look"
	case signedIn:
		who = "a sign-in and only --viewers: those it names may look, and no one changes anything"
	case s.EveryoneAdmin:
		who = "--everyone-admin: everyone who reaches the UI and the API changes anything, as an admin, with no sign-in"
	case serve.LoopbackOnly(s.Listen):
		who = "no roles: everyone who reaches the UI and the API changes anything, as an admin; it listens on this machine alone"
	default:
		who = "no roles, listening beyond this machine: everyone may only look"
	}
	ports := []string{"--listen (the UI, the API, /mcp)"}
	if s.MetricsListen != "" {
		ports = append(ports, "--metrics-listen (/metrics, /healthz, /readyz)")
	}
	if s.IncidentsListen != "" {
		ports = append(ports, "--incidents-listen (what its tokens write)")
	}
	if s.NotifyListen != "" {
		ports = append(ports, "--notify-listen (notifications)")
	}
	it := Item{ID: "access", State: OK, What: who + "; " + strings.Join(ports, ", ")}
	if s.UserHeader != "" && !serve.LoopbackOnly(s.Listen) {
		it.Why = "--user-header is taken from whatever reaches --listen: only a proxy that sets it, and strips it from callers, should reach it"
	}
	p.add(it)
}
