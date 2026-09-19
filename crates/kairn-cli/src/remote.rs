//! `kairn verify` on remote objects: `s3://`, `gs://`, presigned `https://`
//! (docs/design-remote-verify.md). The object body is streamed straight into the verifier;
//! nothing is written to disk.
// Without the `remote` feature only URL recognition is used (to say what's missing).
#![cfg_attr(not(feature = "remote"), allow(dead_code))]

use std::io::Read;

use sha2::{Digest, Sha256};

#[derive(Debug, PartialEq)]
pub(crate) enum Source {
    S3 {
        bucket: String,
        key: String,
    },
    Gs {
        bucket: String,
        key: String,
    },
    /// A presigned URL: the query string is a credential and is never printed.
    Https {
        url: String,
        /// `host[:port]`, validated so that every URL parser agrees on it.
        authority: String,
        /// The path, without query or fragment.
        path: String,
    },
}

/// `None` when `arg` is not a URL (a local path); otherwise the parsed source or why it
/// can't be used (a usage error).
pub(crate) fn parse(arg: &str) -> Option<Result<Source, String>> {
    let (scheme, rest) = arg.split_once("://")?;
    if scheme.is_empty()
        || !scheme
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b"+.-".contains(&b))
    {
        return None;
    }
    Some(match scheme {
        "s3" | "gs" => parse_bucket_url(scheme, rest),
        "https" => parse_https(arg, rest),
        "http" => Err("plaintext http:// is refused; use https:// (or s3:// with \
                       AWS_ALLOW_HTTP=true for a test endpoint)"
            .into()),
        other => Err(format!(
            "unsupported URL scheme {other}:// (s3://, gs:// or https://)"
        )),
    })
}

fn parse_bucket_url(scheme: &str, rest: &str) -> Result<Source, String> {
    let (bucket, key) = rest
        .split_once('/')
        .filter(|(b, k)| !b.is_empty() && !k.is_empty())
        .ok_or_else(|| format!("a {scheme}:// URL names one object: {scheme}://<bucket>/<key>"))?;
    // Bucket naming rules (S3: 3–63, GCS: up to 222 with dots). Anything else could end up
    // in a hostname (virtual-hosted requests) and send the signed request elsewhere.
    let bucket_ok = (3..=222).contains(&bucket.len())
        && bucket
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b"._-".contains(&b));
    if !bucket_ok {
        return Err(format!("{bucket:?} is not a valid bucket name"));
    }
    // The key is fetched exactly as written: segments that a client would normalise away
    // (a leading `/`, `//`, `.`, `..`) would make it verify a different object.
    if key
        .split('/')
        .any(|s| s.is_empty() || s == "." || s == "..")
        || key.chars().any(char::is_control)
    {
        return Err(format!(
            "object key {key:?} has empty, `.` or `..` segments or control characters; \
             such keys are not supported"
        ));
    }
    let (bucket, key) = (bucket.to_string(), key.to_string());
    Ok(if scheme == "s3" {
        Source::S3 { bucket, key }
    } else {
        Source::Gs { bucket, key }
    })
}

fn parse_https(url: &str, rest: &str) -> Result<Source, String> {
    // Strict on purpose: the host that is printed must be the host that is contacted, and
    // URL parsers disagree on backslashes, whitespace, userinfo and escapes.
    if url
        .chars()
        .any(|c| c.is_whitespace() || c.is_control() || c == '\\' || !c.is_ascii())
    {
        return Err("the https:// URL has whitespace, backslashes or non-ASCII characters".into());
    }
    let before_query = rest.split(['?', '#']).next().unwrap_or("");
    let (authority, path) = match before_query.find('/') {
        Some(i) => before_query.split_at(i),
        None => (before_query, "/"),
    };
    let (host, port) = match authority.rsplit_once(':') {
        Some((h, p)) => (h, Some(p)),
        None => (authority, None),
    };
    let host_ok = !host.is_empty()
        && host
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'.' || b == b'-');
    let port_ok =
        port.is_none_or(|p| !p.is_empty() && p.len() <= 5 && p.bytes().all(|b| b.is_ascii_digit()));
    if !host_ok || !port_ok {
        return Err(
            "the https:// URL needs a plain host[:port] (no user info, IP literal brackets or \
             escapes)"
                .into(),
        );
    }
    Ok(Source::Https {
        url: url.to_string(),
        authority: authority.to_ascii_lowercase(),
        path: path.to_string(),
    })
}

/// `[A-Za-z0-9._-]`, at most 100, not `.`/`..`: the segments Kairn's export writes.
fn path_safe(s: &str) -> bool {
    !s.is_empty()
        && s.len() <= 100
        && s.bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b'-'))
        && s != "."
        && s != ".."
}

impl Source {
    /// The URL as it may be printed: never the query string (a presigned credential).
    pub(crate) fn display(&self) -> String {
        match self {
            Source::S3 { bucket, key } => format!("s3://{bucket}/{key}"),
            Source::Gs { bucket, key } => format!("gs://{bucket}/{key}"),
            Source::Https {
                url,
                authority,
                path,
            } => {
                let elided = if url.contains(['?', '#']) { "?…" } else { "" };
                format!("https://{authority}{path}{elided}")
            }
        }
    }

    /// Kairn's export layout is `<prefix>/<cluster_id>/<incident_id>.ieb`: the cluster
    /// and incident a bucket object must hold. `None` for keys outside that layout.
    pub(crate) fn key_identity(&self) -> Option<(String, String)> {
        let key = match self {
            Source::S3 { key, .. } | Source::Gs { key, .. } => key,
            Source::Https { .. } => return None,
        };
        let mut segments = key.rsplit('/');
        let incident = segments.next()?.strip_suffix(".ieb")?;
        let cluster = segments.next()?;
        (path_safe(incident) && path_safe(cluster))
            .then(|| (cluster.to_string(), incident.to_string()))
    }
}

/// The object body on its way into the verifier. It hashes and counts every byte, enforces
/// the byte limit, and records transport failures on the side: the tar reader would report
/// a broken connection as a corrupt archive (FAILED), but a failed read says nothing about
/// the bundle and must be "cannot evaluate".
pub(crate) struct Body<R> {
    inner: R,
    hasher: Sha256,
    bytes: u64,
    limit: u64,
    /// Why the body could not be read completely, if it couldn't.
    pub(crate) failure: Option<String>,
}

impl<R: Read> Body<R> {
    pub(crate) fn new(inner: R, limit: u64) -> Self {
        Self {
            inner,
            hasher: Sha256::new(),
            bytes: 0,
            limit,
            failure: None,
        }
    }

    /// Read whatever the verifier left unread (trailing bytes), so the hash covers the
    /// whole object.
    pub(crate) fn drain(&mut self) {
        if self.failure.is_none() {
            let _ = std::io::copy(self, &mut std::io::sink());
        }
    }

    pub(crate) fn bytes_read(&self) -> u64 {
        self.bytes
    }

    pub(crate) fn sha256(&self) -> String {
        hex(&self.hasher.clone().finalize())
    }
}

pub(crate) fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

impl<R: Read> Read for Body<R> {
    fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
        if let Some(f) = &self.failure {
            return Err(std::io::Error::other(f.clone()));
        }
        match self.inner.read(buf) {
            Ok(n) => {
                self.bytes += n as u64;
                if self.bytes > self.limit {
                    let f = format!(
                        "the object exceeds the verifier limit ({} MiB)",
                        self.limit >> 20
                    );
                    self.failure = Some(f.clone());
                    return Err(std::io::Error::other(f));
                }
                self.hasher.update(&buf[..n]);
                Ok(n)
            }
            Err(e) if e.kind() == std::io::ErrorKind::Interrupted => Err(e),
            Err(e) => {
                self.failure = Some(format!("reading the object failed: {e}"));
                Err(e)
            }
        }
    }
}

/// What was read, for the `object:` line.
pub(crate) struct Object {
    pub(crate) size: u64,
    pub(crate) sha256: String,
    pub(crate) version: Option<String>,
    pub(crate) history: History,
}

/// What the store says about other versions of the key.
#[derive(Debug, PartialEq)]
pub(crate) enum History {
    /// Every version of this exact key (S3 ListObjectVersions).
    Listed {
        versions: usize,
        delete_markers: usize,
        /// The fetched version is the newest listed one.
        fetched_is_latest: bool,
        /// More than one page: counts are lower bounds.
        truncated: bool,
    },
    /// The bucket returned no version id: an overwrite would leave no trace.
    Unversioned,
    /// Not looked at, and why (`--version-id`, `--current-only`, not S3).
    NotChecked(&'static str),
}

impl History {
    pub(crate) fn summary(&self) -> String {
        match self {
            History::Listed {
                versions,
                delete_markers,
                truncated,
                ..
            } => {
                let more = if *truncated { "+" } else { "" };
                format!("history=versions:{versions}{more},delete-markers:{delete_markers}{more}")
            }
            History::Unversioned => "history=unversioned".into(),
            History::NotChecked(why) => format!("history=not-checked({why})"),
        }
    }

    /// Why the history makes the object untrustworthy as evidence, if it does. Kairn writes
    /// each key once (conditional create), so any other version or a delete marker means
    /// the key was written again: the current bytes may not be what Kairn uploaded.
    pub(crate) fn problem(&self) -> Option<String> {
        match self {
            History::Listed {
                versions,
                delete_markers,
                fetched_is_latest,
                ..
            } if *versions > 1 || *delete_markers > 0 || !fetched_is_latest => Some(format!(
                "the key was written more than once ({versions} versions, {delete_markers} \
                 delete markers): the current object may not be the one Kairn uploaded; \
                 verify each version with --version-id (status.exports.<name>.versionId \
                 names the original)"
            )),
            _ => None,
        }
    }
}

/// Parse an S3 ListObjectVersions response, counting only entries for exactly `key`.
pub(crate) fn parse_versions(xml: &str, key: &str, fetched: &str) -> History {
    let mut versions = 0;
    let mut delete_markers = 0;
    let mut latest: Option<String> = None;
    for (tag, block) in xml_blocks(xml, &["Version", "DeleteMarker"]) {
        if xml_field(block, "Key").map(xml_unescape).as_deref() != Some(key) {
            continue;
        }
        if tag == "Version" {
            versions += 1;
        } else {
            delete_markers += 1;
        }
        if xml_field(block, "IsLatest") == Some("true") {
            latest = Some(if tag == "Version" {
                xml_field(block, "VersionId")
                    .unwrap_or_default()
                    .to_string()
            } else {
                String::new() // a delete marker is the latest: nothing current
            });
        }
    }
    History::Listed {
        versions,
        delete_markers,
        fetched_is_latest: latest.as_deref() == Some(fetched),
        truncated: xml_field(xml, "IsTruncated") == Some("true"),
    }
}

/// Top-level `<tag>…</tag>` blocks for any of `tags`, in document order.
fn xml_blocks<'a>(xml: &'a str, tags: &[&'static str]) -> Vec<(&'static str, &'a str)> {
    let mut out = Vec::new();
    let mut rest = xml;
    loop {
        let next = tags
            .iter()
            .filter_map(|t| rest.find(&format!("<{t}>")).map(|i| (i, *t)))
            .min();
        let Some((start, tag)) = next else { break };
        let body_start = start + tag.len() + 2;
        let Some(len) = rest[body_start..].find(&format!("</{tag}>")) else {
            break;
        };
        out.push((tag, &rest[body_start..body_start + len]));
        rest = &rest[body_start + len + tag.len() + 3..];
    }
    out
}

fn xml_field<'a>(xml: &'a str, name: &str) -> Option<&'a str> {
    let open = format!("<{name}>");
    let start = xml.find(&open)? + open.len();
    let len = xml[start..].find(&format!("</{name}>"))?;
    Some(&xml[start..start + len])
}

fn xml_unescape(s: &str) -> String {
    s.replace("&lt;", "<")
        .replace("&gt;", ">")
        .replace("&quot;", "\"")
        .replace("&apos;", "'")
        .replace("&amp;", "&")
}

/// Store errors can carry a multi-line XML body; keep one line and the error code.
pub(crate) fn one_line(msg: &str) -> String {
    let code = xml_field(msg, "Code").map(|c| format!(" ({c})"));
    let head = msg
        .split("<?xml")
        .next()
        .unwrap_or(msg)
        .split('<')
        .next()
        .unwrap_or(msg)
        .lines()
        .next()
        .unwrap_or("")
        .trim_end_matches([':', ' ']);
    format!("{head}{}", code.unwrap_or_default())
}

#[cfg(feature = "remote")]
pub(crate) use net::fetch_and_verify;

#[cfg(feature = "remote")]
mod net {
    use std::io;
    use std::time::Duration;

    use futures::{StreamExt, TryStreamExt};
    use kairn_bundle::{verify_reader, Verdict, VerifyOptions, VerifyReport, VERIFY_MAX_BYTES};
    use object_store::aws::{AmazonS3, AmazonS3Builder, AmazonS3ConfigKey, AwsAuthorizer};
    use object_store::gcp::GoogleCloudStorageBuilder;
    use object_store::path::Path;
    use object_store::{
        client::HttpRequestBody, ClientOptions, GetOptions, ObjectStore, RetryConfig,
    };

    use super::{one_line, parse_versions, Body, History, Object, Source};

    type Stream = futures::stream::BoxStream<'static, io::Result<bytes::Bytes>>;

    /// How the object was opened: the body, its size (if known), its version id, and
    /// for S3 what's needed to list the key's versions afterwards.
    struct Opened {
        stream: Stream,
        size: Option<u64>,
        version: Option<String>,
        s3: Option<(AmazonS3, AmazonS3Builder)>,
    }

    /// Fetch the object and verify it as it streams; then look at the key's version
    /// history. `Err` = cannot evaluate (exit 3).
    pub(crate) fn fetch_and_verify(
        src: &Source,
        version_id: Option<String>,
        current_only: bool,
        opts: &VerifyOptions,
    ) -> Result<(Object, VerifyReport), String> {
        // Same as the controller: rustls with ring, installed explicitly.
        let _ = rustls::crypto::ring::default_provider().install_default();
        let rt = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(1)
            .enable_all()
            .build()
            .map_err(|e| format!("starting the runtime: {e}"))?;
        let pinned = version_id.is_some();
        let opened = rt.block_on(open(src, version_id))?;
        let reader = tokio_util::io::StreamReader::new(opened.stream);
        let bridge = tokio_util::io::SyncIoBridge::new_with_handle(reader, rt.handle().clone());
        let mut body = Body::new(bridge, VERIFY_MAX_BYTES);
        let mut report = verify_reader(&mut body, opts);
        if let Some(f) = body.failure.take() {
            return Err(f);
        }
        if report.verdict == Verdict::CannotEvaluate && report.format.is_none() {
            // Over the verifier's own limits: don't pull the rest of a huge object.
            return Err(report.problems.join("; "));
        }
        body.drain();
        if let Some(f) = body.failure.take() {
            return Err(f);
        }
        if let Some(expected) = opened.size {
            if body.bytes_read() != expected {
                return Err(format!(
                    "read {} bytes but the object has {expected}",
                    body.bytes_read()
                ));
            }
        }
        let history = match (&opened.s3, &opened.version, src) {
            _ if pinned => History::NotChecked("--version-id"),
            _ if current_only => History::NotChecked("--current-only"),
            (Some(_), None, _) => History::Unversioned,
            (Some((store, builder)), Some(v), Source::S3 { bucket, key }) => {
                rt.block_on(list_versions(store, builder, bucket, key, v))?
            }
            _ => History::NotChecked("not s3"),
        };
        if let Some(p) = history.problem() {
            report.problems.push(p);
            if matches!(report.verdict, Verdict::Ok | Verdict::Partial) {
                report.verdict = Verdict::Failed;
            }
        }
        let object = Object {
            size: body.bytes_read(),
            sha256: body.sha256(),
            version: opened.version,
            history,
        };
        Ok((object, report))
    }

    async fn open(src: &Source, version: Option<String>) -> Result<Opened, String> {
        let (store, key, s3): (Box<dyn ObjectStore>, &str, _) = match src {
            Source::S3 { bucket, key } => {
                check_aws_credentials()?;
                let builder = AmazonS3Builder::from_env()
                    .with_bucket_name(bucket)
                    .with_client_options(client_options(true))
                    .with_retry(retry());
                let store = builder
                    .clone()
                    .build()
                    .map_err(|e| format!("S3 client: {}", one_line(&e.to_string())))?;
                (Box::new(store.clone()), key, Some((store, builder)))
            }
            Source::Gs { bucket, key } => {
                let store = GoogleCloudStorageBuilder::from_env()
                    .with_bucket_name(bucket)
                    .with_client_options(client_options(false))
                    .with_retry(retry())
                    .build()
                    .map_err(|e| format!("GCS client: {}", one_line(&e.to_string())))?;
                (Box::new(store), key, None)
            }
            Source::Https { url, .. } => return open_https(url).await,
        };
        let path = Path::parse(key).map_err(|e| format!("invalid object key: {e}"))?;
        // Belt and braces: never fetch a key other than the one that is printed.
        if path.as_ref() != key {
            return Err(format!("object key {key:?} would be fetched as {path}"));
        }
        let get = GetOptions {
            version,
            ..Default::default()
        };
        let result = store.get_opts(&path, get).await.map_err(|e| match e {
            object_store::Error::NotFound { .. } => {
                "no such object (or no permission to read it)".to_string()
            }
            e => format!("reading the object: {}", one_line(&e.to_string())),
        })?;
        let (size, version) = (result.meta.size, result.meta.version.clone());
        let stream = result.into_stream().map_err(io::Error::other).boxed();
        Ok(Opened {
            stream,
            size: Some(size),
            version,
            s3,
        })
    }

    /// `object_store` does not read `~/.aws` profiles or SSO caches. Say so, instead of
    /// letting it fall through to instance metadata and a baffling timeout.
    fn check_aws_credentials() -> Result<(), String> {
        let set = |k: &str| std::env::var_os(k).is_some_and(|v| !v.is_empty());
        let has_env = set("AWS_ACCESS_KEY_ID")
            || set("AWS_WEB_IDENTITY_TOKEN_FILE")
            || set("AWS_CONTAINER_CREDENTIALS_FULL_URI")
            || set("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI");
        if !has_env && (set("AWS_PROFILE") || set("AWS_DEFAULT_PROFILE")) {
            return Err(
                "AWS profiles and SSO are not read by kairn; export the profile's \
                        credentials first: eval \"$(aws configure export-credentials \
                        --format env)\""
                    .into(),
            );
        }
        Ok(())
    }

    fn truthy(v: &str) -> bool {
        matches!(
            v.to_ascii_lowercase().as_str(),
            "true" | "1" | "yes" | "on" | "y"
        )
    }

    /// Every version (and delete marker) of exactly `key` (S3 ListObjectVersions, which
    /// needs `s3:ListBucketVersions`). `object_store` has no call for it; the request is
    /// signed with its SigV4 signer and the same credentials as the GET.
    async fn list_versions(
        store: &AmazonS3,
        builder: &AmazonS3Builder,
        bucket: &str,
        key: &str,
        fetched: &str,
    ) -> Result<History, String> {
        let cfg = |k| builder.get_config_value(&k);
        let region = cfg(AmazonS3ConfigKey::Region).unwrap_or_else(|| "us-east-1".into());
        let endpoint = cfg(AmazonS3ConfigKey::S3Endpoint).or(cfg(AmazonS3ConfigKey::Endpoint));
        let virtual_hosted =
            cfg(AmazonS3ConfigKey::VirtualHostedStyleRequest).is_some_and(|v| truthy(&v));
        // Same endpoint rules as object_store's own requests.
        let base = match (endpoint, virtual_hosted) {
            (Some(e), true) => e.trim_end_matches('/').to_string(),
            (Some(e), false) => format!("{}/{bucket}", e.trim_end_matches('/')),
            (None, true) => format!("https://{bucket}.s3.{region}.amazonaws.com"),
            (None, false) => format!("https://s3.{region}.amazonaws.com/{bucket}"),
        };
        let encoded: String = key
            .bytes()
            .map(|b| match b {
                b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => {
                    (b as char).to_string()
                }
                _ => format!("%{b:02X}"),
            })
            .collect();
        let url = format!("{base}?max-keys=1000&prefix={encoded}&versions=");
        let credential = store
            .credentials()
            .get_credential()
            .await
            .map_err(|e| format!("credentials: {}", one_line(&e.to_string())))?;
        let mut request = http::Request::builder()
            .method("GET")
            .uri(&url)
            .body(HttpRequestBody::empty())
            .map_err(|e| format!("listing versions: {e}"))?;
        AwsAuthorizer::new(&credential, "s3", &region)
            .try_authorize(&mut request, None)
            .map_err(|e| format!("signing the version listing: {e}"))?;
        let allow_http = std::env::var("AWS_ALLOW_HTTP").is_ok_and(|v| truthy(&v));
        let client = reqwest::Client::builder()
            .https_only(!allow_http)
            .redirect(reqwest::redirect::Policy::none())
            .connect_timeout(Duration::from_secs(10))
            .timeout(Duration::from_secs(60))
            .build()
            .map_err(|e| format!("HTTP client: {e}"))?;
        let resp = client
            .get(&url)
            .headers(request.headers().clone())
            .send()
            .await
            .map_err(|e| format!("listing versions: {e}"))?;
        let status = resp.status();
        let text = resp
            .text()
            .await
            .map_err(|e| format!("listing versions: {e}"))?;
        if !status.is_success() {
            return Err(format!(
                "cannot list the key's versions ({status}{}): the history decides whether \
                 the current object is the one uploaded. Grant s3:ListBucketVersions, or pass \
                 --current-only to verify the current version without it",
                super::xml_field(&text, "Code")
                    .map(|c| format!(", {c}"))
                    .unwrap_or_default()
            ));
        }
        Ok(parse_versions(&text, key, fetched))
    }

    async fn open_https(url: &str) -> Result<Opened, String> {
        // reqwest errors print the URL, and a presigned URL's query is a credential:
        // every error goes through `without_url`. No redirects: a redirect would carry the
        // credential (Referer, or the next hop) to a host that is never printed.
        let client = reqwest::Client::builder()
            .https_only(true)
            .redirect(reqwest::redirect::Policy::none())
            .referer(false)
            .connect_timeout(Duration::from_secs(10))
            .read_timeout(Duration::from_secs(60))
            .build()
            .map_err(|e| format!("HTTPS client: {}", e.without_url()))?;
        let resp = client
            .get(url)
            .send()
            .await
            .map_err(|e| format!("requesting the object: {}", e.without_url()))?;
        let status = resp.status();
        if status.is_redirection() {
            return Err(format!(
                "the server answered {status}; redirects are not followed"
            ));
        }
        if !status.is_success() {
            return Err(format!("the server answered {status}"));
        }
        let size = resp.content_length();
        let stream = resp
            .bytes_stream()
            .map(|r| r.map_err(|e| io::Error::other(e.without_url())))
            .boxed();
        Ok(Opened {
            stream,
            size,
            version: None,
            s3: None,
        })
    }

    /// Client options from the environment (proxy, CA, `AWS_ALLOW_HTTP`, …: what
    /// `from_env` would read, since `with_client_options` replaces them), then Kairn's
    /// timeouts: none overall (a large body takes a while), but a stalled read fails.
    fn client_options(aws: bool) -> ClientOptions {
        let mut options = ClientOptions::new();
        if aws {
            for (k, v) in std::env::vars() {
                if !k.starts_with("AWS_") {
                    continue;
                }
                if let Ok(AmazonS3ConfigKey::Client(key)) = k.to_ascii_lowercase().parse() {
                    options = options.with_config(key, v);
                }
            }
        }
        options
            .with_connect_timeout(Duration::from_secs(10))
            .with_read_timeout(Duration::from_secs(60))
            .with_timeout_disabled()
    }

    fn retry() -> RetryConfig {
        RetryConfig {
            max_retries: 2,
            retry_timeout: Duration::from_secs(30),
            ..Default::default()
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn s3(bucket: &str, key: &str) -> Source {
        Source::S3 {
            bucket: bucket.into(),
            key: key.into(),
        }
    }

    #[test]
    fn local_paths_are_not_urls() {
        for p in [
            "bundle.ieb",
            "./dir",
            "/abs/x.ieb",
            "C:\\x.ieb",
            "we ird://x",
            "S3://b/k",
        ] {
            assert!(parse(p).is_none(), "{p}");
        }
    }

    #[test]
    fn parses_object_urls() {
        assert_eq!(
            parse("s3://bkt/p/c/i.ieb"),
            Some(Ok(s3("bkt", "p/c/i.ieb")))
        );
        assert!(matches!(parse("gs://bkt/k"), Some(Ok(Source::Gs { .. }))));
        for bad in [
            "s3://bkt",
            "s3://bkt/",
            "s3:///k",
            "gs://bkt/dir/",
            "http://h/x",
            "ftp://h/x",
            // bucket names that could become a hostname
            "s3://evil.example#/k/i.ieb",
            "s3://localhost:18443/k",
            "s3://Upper/k",
            "s3://ab/k",
            // keys a client would normalise into another object
            "s3://bkt//k.ieb",
            "s3://bkt/a//k.ieb",
            "s3://bkt/a/../k.ieb",
            "s3://bkt/./k.ieb",
        ] {
            assert!(matches!(parse(bad), Some(Err(_))), "{bad}");
        }
    }

    #[test]
    fn https_host_printed_is_host_contacted() {
        for bad in [
            "https://evil.example\\@trusted.example/p/k.ieb?sig",
            "https://user:pw@h.example/k.ieb",
            "https://h.example\t/k.ieb",
            "https://[::1]/k.ieb",
            "https://h%2eexample/k.ieb",
            "https://h.example:x/k.ieb",
            "https:///k.ieb",
        ] {
            assert!(matches!(parse(bad), Some(Err(_))), "{bad}");
        }
        let d = parse("https://H.example:8443/b/k.ieb?X-Amz-Signature=secret#frag")
            .unwrap()
            .unwrap()
            .display();
        assert_eq!(d, "https://h.example:8443/b/k.ieb?…");
        let plain = parse("https://h.example/k.ieb").unwrap().unwrap().display();
        assert_eq!(plain, "https://h.example/k.ieb");
    }

    #[test]
    fn key_identity_follows_the_export_layout() {
        let id = |k: &str| s3("bkt", k).key_identity();
        assert_eq!(
            id("e2e/kind/ic-1.ieb"),
            Some(("kind".into(), "ic-1".into()))
        );
        assert_eq!(id("kind/ic-1.ieb"), Some(("kind".into(), "ic-1".into())));
        assert_eq!(id("ic-1.ieb"), None);
        assert_eq!(id("p/kind/ic-1.tar"), None);
        assert_eq!(id("p/kind/.ieb"), None);
        // Not segments Kairn writes (the controller refuses such cluster ids): no guess.
        assert_eq!(id("p/prod%7Eeu/ic-1.ieb"), None);
        let https = parse("https://h.example/c/i.ieb").unwrap().unwrap();
        assert_eq!(https.key_identity(), None);
    }

    const LISTING: &str = r#"<?xml version="1.0" encoding="UTF-8"?>
<ListVersionsResult><IsTruncated>false</IsTruncated>
<Version><Key>p/c/i.ieb</Key><VersionId>v2</VersionId><IsLatest>true</IsLatest></Version>
<DeleteMarker><Key>p/c/i.ieb</Key><VersionId>d1</VersionId><IsLatest>false</IsLatest></DeleteMarker>
<Version><Key>p/c/i.ieb</Key><VersionId>v1</VersionId><IsLatest>false</IsLatest></Version>
<Version><Key>p/c/i.ieb.bak</Key><VersionId>x</VersionId><IsLatest>true</IsLatest></Version>
</ListVersionsResult>"#;

    #[test]
    fn version_history_counts_only_the_exact_key() {
        let h = parse_versions(LISTING, "p/c/i.ieb", "v2");
        assert_eq!(
            h,
            History::Listed {
                versions: 2,
                delete_markers: 1,
                fetched_is_latest: true,
                truncated: false
            }
        );
        assert!(h.problem().is_some());
        let once = parse_versions(LISTING, "p/c/i.ieb.bak", "x");
        assert!(once.problem().is_none(), "{once:?}");
        // The listing moved on since the GET: not the fetched version.
        assert!(parse_versions(LISTING, "p/c/i.ieb.bak", "other")
            .problem()
            .is_some());
        assert!(History::Unversioned.problem().is_none());
    }

    #[test]
    fn errors_are_one_line() {
        let msg = "reading the object: Generic S3 error: 403 Forbidden: <?xml version=\"1.0\"?>\n<Error><Code>AccessDenied</Code><RequestId>X</RequestId></Error>";
        assert_eq!(
            one_line(msg),
            "reading the object: Generic S3 error: 403 Forbidden (AccessDenied)"
        );
        assert_eq!(one_line("plain"), "plain");
    }

    struct Flaky(Vec<u8>, bool);
    impl Read for Flaky {
        fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
            if self.0.is_empty() {
                return if self.1 {
                    Err(std::io::Error::other("connection reset"))
                } else {
                    Ok(0)
                };
            }
            let n = buf.len().min(self.0.len());
            buf[..n].copy_from_slice(&self.0[..n]);
            self.0.drain(..n);
            Ok(n)
        }
    }

    #[test]
    fn body_hashes_everything_and_records_failures() {
        let mut ok = Body::new(Flaky(b"abc".to_vec(), false), 10);
        ok.drain();
        assert_eq!(ok.bytes_read(), 3);
        assert!(ok.failure.is_none());
        assert_eq!(
            ok.sha256(),
            "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
        );

        let mut reset = Body::new(Flaky(b"abc".to_vec(), true), 10);
        reset.drain();
        assert!(reset
            .failure
            .as_deref()
            .unwrap()
            .contains("connection reset"));

        let mut big = Body::new(Flaky(vec![0; 64], false), 10);
        big.drain();
        assert!(big.failure.as_deref().unwrap().contains("limit"));
    }

    #[test]
    fn a_broken_body_is_not_a_verdict_on_the_bundle() {
        // The verifier alone would call a truncated stream corrupt (FAILED); the recorded
        // failure is what lets the caller report "cannot evaluate" instead.
        let mut body = Body::new(Flaky(vec![0x28, 0xb5, 0x2f, 0xfd], true), 1 << 20);
        let report = kairn_bundle::verify_reader(&mut body, &Default::default());
        assert_eq!(report.verdict, kairn_bundle::Verdict::Failed);
        assert!(body.failure.is_some());
    }
}
