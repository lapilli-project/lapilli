//! Lapilli's rules for talking to an endpoint someone configured: strict parsing, no
//! redirects, and refused address ranges.
//!
//! One place for it, because every caller here is security-sensitive — verifying evidence
//! from a bucket, signing with a KMS, posting an incident summary to a chat webhook — and
//! the rules are easy to get subtly wrong. URL parsers disagree about backslashes,
//! whitespace, user info and escapes, so the host that is printed must be the host that is
//! contacted, and that is only true if the accepted syntax is narrow.
//!
//! The binary must install a rustls crypto provider (ring) before any request.

use std::net::{IpAddr, SocketAddr};
use std::time::Duration;

/// An endpoint Lapilli is willing to talk to.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Endpoint {
    /// `https`, or `http` only for a local host when the caller allowed it.
    pub scheme: String,
    /// Lowercase host, no brackets, no user info.
    pub host: String,
    pub port: Option<u16>,
    /// Path with its query, as given (`/` when empty).
    pub path: String,
}

impl Endpoint {
    /// `host[:port]`.
    pub fn authority(&self) -> String {
        match self.port {
            Some(p) => format!("{}:{p}", self.host),
            None => self.host.clone(),
        }
    }

    /// The URL to request.
    pub fn url(&self) -> String {
        format!("{}://{}{}", self.scheme, self.authority(), self.path)
    }

    /// The endpoint as it may be printed, logged, or put in a status field: **scheme and
    /// authority only**.
    ///
    /// Not the path, and not the query. For a chat webhook the path *is* the credential — a
    /// Slack hook needs no query to be usable by whoever reads the log — and for a presigned
    /// URL the query is. This crate cannot know which component holds which caller's secret,
    /// so it elides both, always. A caller whose path is genuinely public can format one from
    /// the public fields and own that decision.
    pub fn display(&self) -> String {
        format!("{}://{}", self.scheme, self.authority())
    }

    /// True for a host that can only be inside this cluster or this machine.
    pub fn is_local(&self) -> bool {
        is_local_host(&self.host)
    }
}

/// The names that can only resolve inside this cluster or this machine, so plain HTTP to
/// them stays on the pod network. A single-label name (`prometheus`, `minio`) qualifies:
/// public DNS has no single-label hosts, and in a pod it resolves through the search list.
/// A two-label name does **not** (`prometheus.monitoring` is indistinguishable from a public
/// name without a public-suffix list) — write `prometheus.monitoring.svc`.
fn is_local_host(host: &str) -> bool {
    // `prom.monitoring.svc.` names the same service as `prom.monitoring.svc`.
    let host = host.strip_suffix('.').unwrap_or(host);
    // A prefix test ("127.") would accept `127.evil.example`; parse it as an address instead.
    if let Ok(ip) = host.parse::<IpAddr>() {
        return ip.is_loopback();
    }
    !host.contains('.')
        || host == "localhost"
        || host.ends_with(".localhost")
        || host.ends_with(".svc")
        || host.ends_with(".cluster.local")
}

/// Parse a configured endpoint. `allow_http_local` permits `http://` for loopback and
/// cluster-local names (an in-cluster Prometheus, an emulator in a test); everything else
/// must be HTTPS.
///
/// Refused: anything with whitespace, control characters, a backslash, non-ASCII, user info
/// (`user@host`, the form that defeats a naive `starts_with("127.")` check), percent escapes
/// in the authority, an IP literal in brackets, or a port that isn't digits.
pub fn parse(url: &str, allow_http_local: bool) -> Result<Endpoint, String> {
    if url
        .chars()
        .any(|c| c.is_whitespace() || c.is_control() || c == '\\' || !c.is_ascii())
    {
        return Err("the URL has whitespace, backslashes or non-ASCII characters".into());
    }
    let (scheme, rest) = url
        .split_once("://")
        .ok_or_else(|| format!("{}: not an http(s) URL", elide(url)))?;
    let scheme = scheme.to_ascii_lowercase();
    if scheme != "https" && scheme != "http" {
        return Err(format!(
            "unsupported scheme {scheme}:// (https, or http locally)"
        ));
    }
    let (authority, path) = match rest.find(['/', '?', '#']) {
        Some(i) => rest.split_at(i),
        None => (rest, "/"),
    };
    if authority.contains('@') {
        return Err("the URL carries user info before the host".into());
    }
    let (host, port) = match authority.rsplit_once(':') {
        Some((h, p)) => (h, Some(p)),
        None => (authority, None),
    };
    let host_ok = !host.is_empty()
        && host
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'.' || b == b'-')
        && !host.starts_with('-');
    let port = match port {
        None => None,
        Some(p) => Some(
            p.parse::<u16>()
                .map_err(|_| "the URL's port is not a number".to_string())?,
        ),
    };
    if !host_ok {
        return Err("the URL needs a plain host[:port] (no user info, brackets or escapes)".into());
    }
    let endpoint = Endpoint {
        scheme,
        host: host.to_ascii_lowercase(),
        port,
        path: if path.is_empty() {
            "/".to_string()
        } else {
            path.to_string()
        },
    };
    if endpoint.scheme == "http" && !(allow_http_local && endpoint.is_local()) {
        return Err(format!(
            "{}: plain HTTP is only allowed to loopback or cluster-local hosts",
            endpoint.display()
        ));
    }
    Ok(endpoint)
}

/// A client with Lapilli's outbound rules: **no redirects** (a 3xx is the caller's error, not
/// a hop to follow), no referer, HTTPS enforced when the endpoint is HTTPS, and bounded
/// timeouts.
pub fn client(
    endpoint: &Endpoint,
    request_timeout: Option<Duration>,
) -> Result<reqwest::Client, String> {
    build(reqwest::Client::builder(), endpoint, request_timeout)
}

/// `request_timeout: None` means **no overall deadline**. For a caller that streams a large
/// body and enforces its own per-chunk stall timeout, one number for "the whole response" is
/// either too short for a slow link or too long to protect anything.
fn build(
    builder: reqwest::ClientBuilder,
    endpoint: &Endpoint,
    request_timeout: Option<Duration>,
) -> Result<reqwest::Client, String> {
    let builder = builder
        .https_only(endpoint.scheme == "https")
        .redirect(reqwest::redirect::Policy::none())
        .referer(false)
        .connect_timeout(Duration::from_secs(5));
    match request_timeout {
        Some(t) => builder.timeout(t),
        None => builder,
    }
    .build()
    .map_err(|e| format!("HTTP client: {e}"))
}

/// How long a name may take to resolve. Bounded for the reason in [`resolve`].
pub const RESOLVE_TIMEOUT: Duration = Duration::from_secs(5);

/// Which addresses a configured endpoint may resolve to.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Reach {
    /// Only addresses outside the cluster and the machine: refuses loopback, link-local,
    /// private ranges, multicast and unspecified. For an endpoint on the internet (a chat
    /// webhook, a cloud API), this is what keeps a DNS answer from pointing at 169.254.169.254
    /// or at a service only the controller can see.
    Internet,
    /// Also allows loopback and private ranges (an in-cluster service), but never link-local:
    /// the cloud metadata endpoints live there.
    Cluster,
}

/// Resolve `endpoint` and refuse addresses outside `reach`. Returns the addresses that
/// passed, so a caller can connect to a checked address instead of resolving again.
pub async fn resolve(endpoint: &Endpoint, reach: Reach) -> Result<Vec<SocketAddr>, String> {
    let port = endpoint
        .port
        .unwrap_or(if endpoint.scheme == "https" { 443 } else { 80 });
    // Bounded, because `lookup_host` is `getaddrinfo` on a blocking thread and dropping the
    // runtime *waits* for blocking tasks: an unbounded lookup can hold a shutting-down process
    // past its grace period long after the caller gave up on it.
    let looked_up = tokio::time::timeout(
        RESOLVE_TIMEOUT,
        tokio::net::lookup_host((endpoint.host.clone(), port)),
    )
    .await
    .map_err(|_| format!("{}: name lookup timed out", endpoint.display()))?;
    let addrs: Vec<SocketAddr> = looked_up
        .map_err(|e| format!("{}: {e}", endpoint.display()))?
        .collect();
    if addrs.is_empty() {
        return Err(format!("{}: no addresses", endpoint.display()));
    }
    for addr in &addrs {
        if let Some(why) = refuse(addr.ip(), reach) {
            return Err(format!("{}: {why}", endpoint.display()));
        }
    }
    Ok(addrs)
}

/// A client that will only connect to the addresses this call vetted. Resolving first and
/// then letting the client resolve again leaves a rebinding window: a name that answered with
/// a public address during the check can answer with 169.254.169.254 a millisecond later.
/// Pinning closes it.
pub async fn connect(
    endpoint: &Endpoint,
    reach: Reach,
    request_timeout: Option<Duration>,
) -> Result<reqwest::Client, String> {
    let addrs = resolve(endpoint, reach).await?;
    build(
        reqwest::Client::builder().resolve_to_addrs(&endpoint.host, &addrs),
        endpoint,
        request_timeout,
    )
}

/// Why this address is refused, if it is. Link-local is refused for every reach: that is
/// where the cloud metadata endpoints live (169.254.169.254, fd00:ec2::254).
fn refuse(ip: IpAddr, reach: Reach) -> Option<&'static str> {
    // `::ffff:169.254.169.254` is that same metadata address wearing a v6 costume: judge the
    // v4 address it maps to, or every v4 rule below silently misses it.
    let ip = match ip {
        IpAddr::V6(v6) => v6.to_ipv4_mapped().map_or(ip, IpAddr::V4),
        v4 => v4,
    };
    let link_local = match ip {
        IpAddr::V4(v4) => v4.is_link_local(),
        IpAddr::V6(v6) => (v6.segments()[0] & 0xffc0) == 0xfe80,
    };
    if link_local {
        return Some("resolves to a link-local address (cloud metadata lives there)");
    }
    // Refused for every reach: none of these is a host, so a name resolving to one is a
    // configuration error at best. `0.0.0.0/8` is "this network"; 255.255.255.255 is the
    // limited broadcast address.
    let meaningless = ip.is_multicast()
        || ip.is_unspecified()
        || match ip {
            IpAddr::V4(v4) => v4.is_broadcast() || v4.octets()[0] == 0,
            IpAddr::V6(_) => false,
        };
    if meaningless {
        return Some("resolves to a multicast, broadcast or unspecified address");
    }
    if reach == Reach::Cluster {
        return None;
    }
    let internal = match ip {
        IpAddr::V4(v4) => v4.is_loopback() || v4.is_private() || v4.is_broadcast(),
        IpAddr::V6(v6) => v6.is_loopback() || (v6.segments()[0] & 0xfe00) == 0xfc00,
    };
    internal.then_some("resolves to an address inside the cluster or machine")
}

/// A URL in a message: never the query string.
fn elide(url: &str) -> String {
    // Scheme and authority only, to match `Endpoint::display()`. Keeping the path here would be
    // a trap for the next caller: this is the message a URL that failed to *parse* gets, and
    // for a chat webhook the path is the credential.
    let after_scheme = url.split_once("://");
    match after_scheme {
        Some((scheme, rest)) => {
            let authority = rest.split(['/', '?', '#']).next().unwrap_or("");
            format!("{scheme}://{authority}")
        }
        // No scheme: there is no path to separate, and the whole thing is what was rejected.
        None => url.split(['/', '?', '#']).next().unwrap_or("").to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn accepts_plain_https() {
        let e = parse("https://hooks.example.com/services/T0/B0/xyz", false).unwrap();
        assert_eq!(e.host, "hooks.example.com");
        assert_eq!(e.port, None);
        assert_eq!(e.path, "/services/T0/B0/xyz");
        assert_eq!(e.url(), "https://hooks.example.com/services/T0/B0/xyz");
        let with_port = parse("https://PROM.example:9090/api/v1/query?q=up", false).unwrap();
        assert_eq!(with_port.authority(), "prom.example:9090");
        // Neither the path nor the query is ever printed: one of them is a credential for
        // some caller, and this crate cannot tell which.
        assert_eq!(with_port.display(), "https://prom.example:9090");
        let hook = parse("https://hooks.slack.com/services/T0/B0/SECRET", false).unwrap();
        assert_eq!(hook.display(), "https://hooks.slack.com");
        // A URL that fails to parse is reported the same way: no path, no query. `elide` is the
        // message for exactly the inputs `Endpoint` never gets built from.
        let err = parse("ftp://hooks.slack.com/services/T0/B0/SECRET", false).unwrap_err();
        assert!(!err.contains("SECRET"), "{err}");
        let err = parse("hooks.slack.com/services/T0/B0/SECRET", false).unwrap_err();
        assert!(!err.contains("SECRET"), "{err}");
        assert_eq!(parse("https://host.example", false).unwrap().path, "/");
    }

    #[test]
    fn refuses_the_shapes_that_fool_naive_checks() {
        for bad in [
            // user info: the form that makes `starts_with("127.")` say "local"
            "http://127.0.0.1@evil.example/hook",
            "https://user:pw@hooks.example.com/x",
            // a host that only looks local
            "http://127.evil.example/",
            "http://localhost.evil.example/",
            // escapes, brackets, control characters, non-ASCII
            "https://ho%73t.example/",
            "https://[::1]/",
            "https://host.example\\@evil.example/",
            "https://host.example\t/x",
            "https://hóst.example/",
            // scheme and port
            "ftp://host.example/",
            "host.example/x",
            "https://host.example:99999/",
            "https://host.example:abc/",
            "https:///x",
            "",
        ] {
            assert!(parse(bad, true).is_err(), "accepted {bad}");
        }
    }

    #[test]
    fn plain_http_only_for_local_hosts_and_only_when_allowed() {
        for local in [
            "http://localhost:4566/",
            "http://127.0.0.1:8080/x",
            "http://receiver.notify-e2e.svc:8080/hook",
            "http://minio.minio.svc.cluster.local:9000/",
            // a single label resolves only through the pod's search list
            "http://prometheus:9090/",
            // the root dot names the same service
            "http://prom.monitoring.svc./api/v1/query",
        ] {
            assert!(parse(local, true).is_ok(), "refused {local}");
            // Not allowed unless the caller opted in.
            assert!(parse(local, false).is_err(), "accepted {local} unasked");
        }
        assert!(parse("http://hooks.example.com/x", true).is_err());
        // Two labels are not enough: `prometheus.monitoring` could be a public name.
        assert!(parse("http://prometheus.monitoring:9090/", true).is_err());
    }

    #[test]
    fn link_local_is_refused_everywhere() {
        for reach in [Reach::Internet, Reach::Cluster] {
            // The whole ranges, not just the famous addresses: 169.254.0.0/16, fe80::/10
            // (`febf::` is still in it), IPv4 and IPv6 multicast, both unspecified addresses.
            for bad in [
                "169.254.169.254",
                "169.254.0.1",
                "fe80::1",
                "febf::1",
                "0.0.0.0",
                "::",
                "224.0.0.1",
                "ff02::1",
                // Not hosts either: the limited broadcast address and "this network".
                "255.255.255.255",
                "0.1.2.3",
            ] {
                assert!(refuse(bad.parse().unwrap(), reach).is_some(), "{bad}");
            }
            // The same addresses mapped into IPv6 are the same addresses.
            for mapped in [
                "::ffff:169.254.169.254",
                "::ffff:0.0.0.0",
                "::ffff:224.0.0.1",
            ] {
                assert!(refuse(mapped.parse().unwrap(), reach).is_some(), "{mapped}");
            }
        }
        // …and a mapped private address is internal, not a public one.
        assert!(refuse("::ffff:10.4.1.7".parse().unwrap(), Reach::Internet).is_some());
        assert!(refuse("::ffff:10.4.1.7".parse().unwrap(), Reach::Cluster).is_none());
    }

    #[test]
    fn internet_reach_refuses_internal_addresses_cluster_reach_allows_them() {
        for ip in [
            "127.0.0.1",
            "10.4.1.7",
            "192.168.1.9",
            "172.16.0.3",
            // an in-cluster endpoint can be a loopback sidecar or a ULA address
            "::1",
            "fd00::1",
        ] {
            let ip: IpAddr = ip.parse().unwrap();
            assert!(refuse(ip, Reach::Internet).is_some(), "{ip} allowed");
            assert!(refuse(ip, Reach::Cluster).is_none(), "{ip} refused");
        }
        for ip in ["1.1.1.1", "2606:4700::1111"] {
            let ip: IpAddr = ip.parse().unwrap();
            assert!(refuse(ip, Reach::Internet).is_none(), "{ip} refused");
        }
    }

    #[tokio::test]
    async fn resolve_checks_every_address() {
        let local = parse("http://localhost:9/", true).unwrap();
        let addrs = resolve(&local, Reach::Cluster).await.unwrap();
        assert!(addrs.iter().all(|a| a.port() == 9), "{addrs:?}");
        let err = resolve(&local, Reach::Internet).await.unwrap_err();
        assert!(err.contains("inside the cluster or machine"), "{err}");
        // A name that does not resolve fails rather than being assumed safe.
        let missing = parse("https://lapilli-no-such-host.invalid/", false).unwrap();
        assert!(resolve(&missing, Reach::Internet).await.is_err());
        // …and it cannot take longer than the bound, which is what keeps a shutting-down
        // process from waiting on `getaddrinfo` past its grace period.
        let t = std::time::Instant::now();
        let _ = resolve(&missing, Reach::Internet).await;
        assert!(t.elapsed() < RESOLVE_TIMEOUT + Duration::from_secs(2));
    }
}
