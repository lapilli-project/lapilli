package serve

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"syscall"

	"lapilli.dev/internal/prom"
)

// What the status banner says first, in plain words, when serve's work
// failed on reading a history source (docs/design/ui.md, "What a page
// says"): which address, what kind of failure, and what to check. The
// error itself stays behind "The error". The kind is found where the
// error is set, from the error's own type, never from its text.

// errClass is a failure of reading a history source, as fail sets it
// beside the error's text, err, that it is of.
type errClass struct {
	err  string // the status's error it is of
	kind string // refused, host, timeout, denied, tls or read
	at   string // the source's address as given, without a password
	flag string // the flag that gives it
	code int    // the HTTP status, of kind denied
}

// historyReads are the failures of the status banner that are a read of a
// history source: the address a failing read names is the one given. Not
// those that only write the ledger or read a file (keeping the impact
// signals, deploys, the marked windows). "a run" is a run's own failure
// (ALERTS with --alerts, --impact, the history), and its others, a
// ledger's, are told apart by the error: only the failure of a request is
// classified.
var historyReads = map[string]bool{
	"setting the replay beside what really fired": true, "reading the impact signal again": true,
	"reading what the impact signals read": true, "looking for where the impact signal's data begins": true,
	"copying in Grafana's alert state history": true, "reading the marked windows' expressions again": true,
	"replaying the rules": true, "a run": true,
}

// shownAddress is an address as the banner says it: without the user and
// password, the query and the fragment.
func shownAddress(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return scrubSecrets(raw)
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	// A token can sit in a path.
	return scrubSecrets(u.String())
}

// faultOf is how err, of what failed, is a failure of reading a source:
// its class, ok false when it isn't one, or the address isn't known.
func (s *Server) faultOf(what string, err error) (errClass, bool) {
	if !historyReads[what] || err == nil {
		return errClass{}, false
	}
	var c errClass
	var ae *prom.APIError
	var ue *url.Error
	var dns *net.DNSError
	var ne net.Error
	var ua x509.UnknownAuthorityError
	var he x509.HostnameError
	var ci x509.CertificateInvalidError
	var cv *tls.CertificateVerificationError
	var rh tls.RecordHeaderError
	// Only the failure of a request: the HTTP client wraps what it asked
	// that failed (refused, a name, a timeout, TLS) in a *url.Error, and an
	// answer is an *prom.APIError. A bare refusal, name or deadline came
	// from elsewhere, as a database's connect timeout does.
	if !errors.As(err, &ue) && !errors.As(err, &ae) {
		return errClass{}, false
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		c.kind = "refused"
	case errors.As(err, &dns):
		c.kind = "host"
	case errors.As(err, &ua), errors.As(err, &he), errors.As(err, &ci), errors.As(err, &cv), errors.As(err, &rh):
		c.kind = "tls"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		c.kind = "timeout"
	case errors.As(err, &ae):
		c.kind = "read"
		if ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden || ae.Status == http.StatusProxyAuthRequired {
			c.kind, c.code = "denied", ae.Status
		}
	case errors.As(err, &ue):
		c.kind = "read"
	default:
		return errClass{}, false
	}
	c.flag, c.at = "--prometheus", s.cfg.Prometheus
	if what == "copying in Grafana's alert state history" && s.cfg.Grafana != "" {
		c.flag, c.at = "--grafana", s.cfg.Grafana
	}
	// A failure that names the address it asked is of the source given
	// that address, as it may be Loki's, or a ruler's, or Grafana's.
	if errors.As(err, &ue) {
		if asked, perr := url.Parse(ue.URL); perr == nil {
			for _, g := range [][2]string{{"--loki", s.cfg.Loki}, {"--ruler", s.cfg.Ruler}, {"--grafana", s.cfg.Grafana}, {"--prometheus", s.cfg.Prometheus}} {
				if given, perr := url.Parse(g[1]); perr == nil && g[1] != "" && given.Host == asked.Host {
					c.flag, c.at = g[0], g[1]
					break
				}
			}
			if c.at == "" {
				c.at = asked.Scheme + "://" + asked.Host
			}
		}
	}
	if c.at == "" {
		return errClass{}, false
	}
	c.at = shownAddress(c.at)
	return c, true
}

// faultLead is what the banner says first of a failure to read a source,
// in language l: its address and flag as code, and what to check.
// With no address (c.at empty), as a reader below admin has it, the lead
// names the flag alone: the address is for an admin, as the Sources page's.
func faultLead(l string, c errClass) template.HTML {
	at, flag := asCode(c.at), asCode(c.flag)
	if c.at == "" {
		switch c.kind {
		case "refused":
			return trBDI(l, "Nothing answered at the address given to %s: the connection was refused. Check that it is the address of a running server this one can reach.", flag)
		case "host":
			return trBDI(l, "The address given to %s couldn't be found. Check its host name.", flag)
		case "timeout":
			return trBDI(l, "The server at the address given to %s didn't answer in time. Check that it is up and that this server can reach it.", flag)
		case "denied":
			return trBDI(l, "The server at the address given to %s turned Lapilli away (HTTP %d). Check the credentials given for it, or %s.", flag, c.code, asCode("--auth"))
		case "tls":
			return trBDI(l, "Lapilli couldn't trust the secure connection to the address given to %s. Check that the certificate is one this server trusts, and made for that address.", flag)
		}
		return trBDI(l, "Lapilli couldn't read the address given to %s. Check that it is right.", flag)
	}
	switch c.kind {
	case "refused":
		return trBDI(l, "Nothing answered at %s: the connection was refused. Check that %s is the address of a running server this one can reach.", at, flag)
	case "host":
		return trBDI(l, "The address %s couldn't be found. Check the host name given to %s.", at, flag)
	case "timeout":
		return trBDI(l, "%s didn't answer in time. Check that it is up and that this server can reach it, at the address given to %s.", at, flag)
	case "denied":
		return trBDI(l, "%s turned Lapilli away (HTTP %d). Check the credentials given for %s, or %s.", at, c.code, flag, asCode("--auth"))
	case "tls":
		return trBDI(l, "Lapilli couldn't trust the secure connection to %s. Check that the certificate is one this server trusts, and made for the address given to %s.", at, flag)
	}
	return trBDI(l, "Lapilli couldn't read %s. Check the address given to %s.", at, flag)
}
