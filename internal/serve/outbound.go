package serve

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// What the notifications port fetches, an SNS certificate or a subscription's
// confirmation, it fetches from a provider's public host. The host is
// checked by name first; publicOnly checks where the name led, so that a
// name that resolves to this cluster, or to the machine itself, is never
// asked anything.

// errNotPublic is an address that isn't on the internet.
var errNotPublic = errors.New("the host resolves to an address that isn't public")

// notPublic are ranges that aren't the internet, beside those netip names:
// this network, carrier-grade NAT (as a cluster's may be), benchmarking,
// reserved, Azure's host address, and IPv6 forms that carry an IPv4 address
// or are kept for sites (NAT64, 6to4, IPv4-compatible, site-local).
var notPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("168.63.129.16/32"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("fec0::/10"),
}

// public reports whether an address is one the internet routes to: not the
// machine's own, a private network's, a link's, multicast, unspecified, or
// one of notPublic.
func public(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return false
	}
	for _, p := range notPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// dialPublic refuses to connect where public doesn't hold.
func dialPublic(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil || !public(ap.Addr()) {
		return errNotPublic
	}
	return nil
}

// fetcher is the client for a provider's host, following no redirect. Where
// the environment names a proxy for it, it goes through the proxy, which
// reaches the host; else it connects only to a public address.
func fetcher(req *http.Request) *http.Client {
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if p, err := http.ProxyFromEnvironment(req); err == nil && p != nil {
		return c
	}
	d := &net.Dialer{Timeout: 10 * time.Second, Control: dialPublic}
	// Fetched a few times an hour at most: no connection is kept open.
	c.Transport = &http.Transport{DialContext: d.DialContext, TLSHandshakeTimeout: 10 * time.Second, ForceAttemptHTTP2: true, DisableKeepAlives: true}
	return c
}
