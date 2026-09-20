//! The controller's own Prometheus metrics, on `/metrics` of the health port.
//!
//! Hand-rolled (no client library): a handful of counters, gauges and one histogram, all
//! with bounded label sets. Never an incident id, a pod name or a namespace as a label —
//! cardinality has to stay flat however many captures there are.
//!
//! The metric names and their labels are a compatibility surface: see
//! docs/COMPATIBILITY.md §2. Series may be added; names, labels and types don't change.

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::OnceLock;
use std::time::Duration;

/// Seconds buckets for capture durations (cumulative, `le`).
const DURATION_BUCKETS: [f64; 9] = [0.5, 1.0, 2.5, 5.0, 10.0, 30.0, 60.0, 300.0, 900.0];
/// Byte buckets for bundle sizes, up to the producer's 1 GiB cap (spec/IEB-SPEC.md).
const BYTE_BUCKETS: [f64; 8] = [
    65_536.0,
    262_144.0,
    1_048_576.0,
    4_194_304.0,
    16_777_216.0,
    67_108_864.0,
    268_435_456.0,
    1_073_741_824.0,
];

#[derive(Default)]
struct Counter(AtomicU64);

impl Counter {
    fn inc(&self) {
        self.0.fetch_add(1, Ordering::Relaxed);
    }
    fn get(&self) -> u64 {
        self.0.load(Ordering::Relaxed)
    }
}

/// A cumulative histogram over its own buckets. `sum_units` is exact integer arithmetic:
/// milliseconds for seconds, bytes for sizes.
struct Histogram<const N: usize> {
    buckets: [AtomicU64; N],
    inf: AtomicU64,
    sum_units: AtomicU64,
}

impl<const N: usize> Default for Histogram<N> {
    fn default() -> Self {
        Self {
            buckets: std::array::from_fn(|_| AtomicU64::new(0)),
            inf: AtomicU64::new(0),
            sum_units: AtomicU64::new(0),
        }
    }
}

impl<const N: usize> Histogram<N> {
    /// `units` is the value in this histogram's sum unit (millis, or bytes).
    fn observe(&self, value: f64, units: u64, bounds: &[f64; N]) {
        // `+Inf` first: a scrape in between then sees `le="+Inf"` no smaller than any
        // bucket, never a bucket above the count.
        self.inf.fetch_add(1, Ordering::Relaxed);
        self.sum_units.fetch_add(units, Ordering::Relaxed);
        for (i, upper) in bounds.iter().enumerate() {
            if value <= *upper {
                self.buckets[i].fetch_add(1, Ordering::Relaxed);
            }
        }
    }

    fn render(&self, out: &mut String, name: &str, help: &str, bounds: &[f64; N], sum: String) {
        metric_header(out, name, help, "histogram");
        for (i, upper) in bounds.iter().enumerate() {
            out.push_str(&format!(
                "{name}_bucket{{le=\"{upper}\"}} {}\n",
                self.buckets[i].load(Ordering::Relaxed)
            ));
        }
        let count = self.inf.load(Ordering::Relaxed);
        out.push_str(&format!("{name}_bucket{{le=\"+Inf\"}} {count}\n"));
        out.push_str(&format!("{name}_sum {sum}\n"));
        out.push_str(&format!("{name}_count {count}\n"));
    }

    fn sum_units(&self) -> u64 {
        self.sum_units.load(Ordering::Relaxed)
    }
}

/// Outcomes of a capture, as the `result` label of `kairn_captures_total`.
#[derive(Clone, Copy)]
pub enum CaptureResult {
    /// Sealed (and packed), whatever its remote exports do next.
    Sealed,
    /// The controller refused to capture (cluster mismatch, unsafe or claimed id …).
    Refused,
    /// Collected but never sealed (signing gave up, staged data lost …).
    Failed,
}

/// One counter per notification outcome. Pinned to the enum, so adding a variant without a
/// counter is a compile error rather than a silently missing series.
const NOTIFY_RESULTS: usize = 6;
const _: () = assert!(crate::notify::SendResult::ALL.len() == NOTIFY_RESULTS);

/// Every series the controller exposes. One process-wide instance ([`metrics`]).
#[derive(Default)]
pub struct Metrics {
    captures_sealed: Counter,
    captures_refused: Counter,
    captures_failed: Counter,
    capture_seconds: Histogram<{ DURATION_BUCKETS.len() }>,
    bundle_bytes: Histogram<{ BYTE_BUCKETS.len() }>,
    partial_captures: Counter,
    collector_failures: Counter,
    seal_attempts_ok: Counter,
    seal_attempts_failed: Counter,
    seal_pack_failures: Counter,
    export_attempts_ok: Counter,
    export_attempts_failed: Counter,
    reconcile_errors: Counter,
    /// Notification outcomes, one counter per `SendResult`. Bounded: the label set is the
    /// enum, never a route name (an admin can define any number of routes).
    notifications: [Counter; NOTIFY_RESULTS],
    /// Notification routes that loaded, and routes that are configured but unusable. Set once
    /// at startup: a misconfigured route otherwise shows up only in a log line nobody reads
    /// until an incident has already been missed.
    notify_routes: std::sync::Mutex<Option<(u64, u64)>>,
    /// Captures by phase and destinations by state, counted from the API itself (see
    /// [`State`]): side effects in the reconcile loop can't see deletions, and would go
    /// stale after a restart.
    state: std::sync::Mutex<State>,
    webhook_accepted: Counter,
    webhook_duplicate: Counter,
    webhook_rejected: Counter,
    /// An authenticated request the API server then refused to turn into a capture.
    webhook_error: Counter,
    /// `1` once the KMS public key is pinned, with its key id as a label.
    signing_key_id: std::sync::Mutex<Option<String>>,
    /// Whether the poller's last `list` of IncidentCapture worked, and why not if it didn't.
    ///
    /// This is the one failure that stops the product and reported nothing: `/healthz` is
    /// deliberately decoupled from the API (`webhook.rs`, and see docs/egress.md), so a
    /// controller that cannot use the API stays `1/1 Running` with no restarts while every
    /// capture stops. `kairn_reconcile_errors_total` does not cover it either — nothing is
    /// reconciled when no alert can arrive, so the counter sits flat.
    ///
    /// The claim this makes is deliberately narrow: **the poller's own list succeeded**. It is
    /// the work the controller already has to do, so there is no synthetic probe to disagree
    /// with reality — but it covers one of the chart's three RBAC bindings, not all of them.
    /// A broken *collector* binding shows up as `kairn_collector_failures_total` and a PARTIAL
    /// bundle instead, and only once a capture runs.
    apiserver_poll: AtomicU64,
    /// One counter per [`PollResult`], so a new variant without a counter is a compile error.
    apiserver_polls: [Counter; POLL_RESULTS],
    /// Unix seconds of the last successful poll; `0` until there has been one.
    apiserver_last_ok: AtomicU64,
}

/// [`Metrics::apiserver_poll`]: unknown until the first poll returns, so a pod that has not
/// polled yet emits no gauge rather than claiming either answer.
const POLL_UNKNOWN: u64 = 0;
const POLL_OK: u64 = 1;
const POLL_BAD: u64 = 2;

/// Why a poll of the API server ended as it did — the `result` label of
/// `kairn_apiserver_polls_total`, and the answer to the operator's first question, which is
/// always "RBAC, or the network?".
///
/// The split is drawn where it can be drawn *reliably*: [`kube::Error::Api`] means the API
/// server answered and the answer was an HTTP status, so anything else means the request never
/// got an answer at all. Guessing finer than that from an error string would be a label that
/// lies.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum PollResult {
    /// The list returned.
    Ok,
    /// 403: reached the API server, which refused. A missing or narrowed RBAC binding.
    Forbidden,
    /// 401: reached the API server, which would not authenticate the request. Usually an
    /// expired or unmounted ServiceAccount token.
    Unauthorized,
    /// 404: the CRD is not installed (Helm does not upgrade `crds/`, so this happens).
    NotFound,
    /// Any other status the API server returned — it is up and talking, so this is neither an
    /// RBAC nor a network problem. 429 and 5xx land here.
    ApiError,
    /// No answer: DNS, routing, TLS, a dropped egress packet, or a poll that outlasted its own
    /// interval. This is the one an egress policy produces.
    Unreachable,
}

const POLL_RESULTS: usize = 6;

/// What one poll changed, so the caller can log a recovery differently from a cold start. On a
/// live cluster the two lines were byte-identical, which meant a log search could not tell a
/// controller that recovered from one that had been restarted.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Transition {
    /// Same verdict as last time. Nothing to say; an outage must not write a line per interval.
    None,
    /// The first poll of this process succeeded.
    FirstOk,
    /// It had been failing and now works.
    Recovered,
    /// It had been working (or had never polled) and now fails.
    Broke,
}

impl PollResult {
    pub const ALL: [PollResult; POLL_RESULTS] = [
        PollResult::Ok,
        PollResult::Forbidden,
        PollResult::Unauthorized,
        PollResult::NotFound,
        PollResult::ApiError,
        PollResult::Unreachable,
    ];

    fn label(self) -> &'static str {
        match self {
            PollResult::Ok => "ok",
            PollResult::Forbidden => "forbidden",
            PollResult::Unauthorized => "unauthorized",
            PollResult::NotFound => "not-found",
            PollResult::ApiError => "api-error",
            PollResult::Unreachable => "unreachable",
        }
    }

    /// Classify a `kube` error. `Api` means the server answered; everything else means the
    /// request never reached one.
    fn of(err: &kube::Error) -> Self {
        match err {
            kube::Error::Api(resp) => match resp.code {
                403 => PollResult::Forbidden,
                401 => PollResult::Unauthorized,
                404 => PollResult::NotFound,
                _ => PollResult::ApiError,
            },
            _ => PollResult::Unreachable,
        }
    }
}

/// What the captures in the API say right now: filled by a poller, so a deleted capture
/// disappears from the gauges and a restart rebuilds them.
#[derive(Default, Clone)]
pub struct State {
    /// `phase` label → captures.
    pub captures_by_phase: std::collections::BTreeMap<String, usize>,
    /// export `state` label → destinations.
    pub destinations_by_state: std::collections::BTreeMap<String, usize>,
    /// Set once the first poll succeeded (before that the gauges are not emitted).
    pub known: bool,
}

/// Keep the state-derived gauges current by asking the API, every `interval`. Side effects
/// in the reconcile loop can't do this: a deleted capture is never reconciled, and a
/// restart would start from an empty picture.
pub fn spawn_state_poller(api: kube::Api<crate::crd::IncidentCapture>, interval: Duration) {
    tokio::spawn(async move {
        loop {
            poll_once(&api, metrics(), interval).await;
            tokio::time::sleep(interval).await;
        }
    });
}

/// One poll: refresh the state gauges, and record whether the API server could be used.
///
/// Split out of [`spawn_state_poller`] so it can be tested against a real HTTP server. It has
/// to be: the loop is the whole behaviour, and while the recording lived inline, inverting
/// `apiserver_poll`'s argument — turning every success into a failure and back — left the
/// entire unit suite green.
///
/// `budget` bounds the list. Without it the only ceiling is `kube`'s 295 s read timeout, so a
/// poll that **hangs** rather than failing — which is exactly what a NetworkPolicy that DROPs
/// looks like from inside the pod — never reports anything at all, and the gauge sits at `1`
/// through the outage it exists to report.
async fn poll_once(api: &kube::Api<crate::crd::IncidentCapture>, m: &Metrics, budget: Duration) {
    let listed = tokio::time::timeout(budget, api.list(&kube::api::ListParams::default())).await;
    match listed {
        Ok(Ok(list)) => {
            let mut state = State {
                known: true,
                ..Default::default()
            };
            for ic in list {
                let status = ic.status.unwrap_or_default();
                *state
                    .captures_by_phase
                    .entry(phase_label(&status.phase).to_string())
                    .or_default() += 1;
                for export in status.exports.values() {
                    *state
                        .destinations_by_state
                        .entry(export_label(&export.state).to_string())
                        .or_default() += 1;
                }
            }
            m.set_state(state);
            // After `set_state`, and with a release barrier, so a scrape that straddles a
            // recovery reads the *old* verdict beside the new picture rather than the reverse.
            // Calling a fresh picture stale for one scrape is the harmless direction.
            match m.apiserver_poll(PollResult::Ok) {
                Transition::FirstOk => {
                    tracing::info!("the API server is usable: the capture list came back")
                }
                Transition::Recovered => {
                    tracing::info!("the API server is usable again: the capture list came back")
                }
                _ => {}
            }
        }
        // Keep the last picture rather than reporting zeros — which is only honest because the
        // gauge below goes to 0 to say the picture is stale.
        Ok(Err(e)) => {
            let why = PollResult::of(&e);
            if m.apiserver_poll(why) != Transition::None {
                tracing::warn!(
                    error = %e, reason = why.label(),
                    "the API server cannot be used: captures will not run until this is fixed. \
                     /healthz stays ok on purpose, so this pod will not be restarted — see \
                     docs/egress.md"
                );
            } else {
                tracing::debug!(error = %e, reason = why.label(), "metrics: listing captures failed");
            }
        }
        Err(_elapsed) => {
            if m.apiserver_poll(PollResult::Unreachable) != Transition::None {
                tracing::warn!(
                    budget_secs = budget.as_secs(),
                    "the API server did not answer within one poll interval, so it counts as \
                     unreachable: a dropped egress packet looks exactly like this from inside \
                     the pod — see docs/egress.md"
                );
            } else {
                tracing::debug!("metrics: listing captures timed out");
            }
        }
    }
}

fn phase_label(phase: &crate::crd::Phase) -> &'static str {
    use crate::crd::Phase;
    match phase {
        Phase::Pending => "pending",
        Phase::Capturing => "capturing",
        Phase::Sealing => "sealing",
        Phase::Exported => "exported",
        Phase::Failed => "failed",
    }
}

fn export_label(state: &crate::crd::ExportState) -> &'static str {
    use crate::crd::ExportState;
    match state {
        ExportState::Pending => "pending",
        ExportState::Uploaded => "uploaded",
        ExportState::Refused => "refused",
        ExportState::Conflict => "conflict",
        ExportState::Failed => "failed",
    }
}

pub fn metrics() -> &'static Metrics {
    static METRICS: OnceLock<Metrics> = OnceLock::new();
    METRICS.get_or_init(Metrics::default)
}

impl Metrics {
    pub fn capture(&self, result: CaptureResult) {
        match result {
            CaptureResult::Sealed => self.captures_sealed.inc(),
            CaptureResult::Refused => self.captures_refused.inc(),
            CaptureResult::Failed => self.captures_failed.inc(),
        }
    }

    /// A sealed bundle: how long from the start of collection, its size, and whether some
    /// intended collectors didn't run.
    pub fn sealed(&self, seconds: f64, bytes: u64, partial: bool) {
        let millis = (seconds * 1000.0).max(0.0) as u64;
        self.capture_seconds
            .observe(seconds, millis, &DURATION_BUCKETS);
        self.bundle_bytes
            .observe(bytes as f64, bytes, &BYTE_BUCKETS);
        if partial {
            self.partial_captures.inc();
        }
    }

    pub fn collector_failure(&self) {
        self.collector_failures.inc();
    }

    /// One call to the KMS to sign a manifest (only that: packing has its own counter, so
    /// this series can be reconciled against the cloud's audit log).
    pub fn seal_attempt(&self, ok: bool) {
        if ok {
            self.seal_attempts_ok.inc()
        } else {
            self.seal_attempts_failed.inc()
        }
    }

    /// A signature was obtained but the bundle could not be packed.
    pub fn seal_pack_failure(&self) {
        self.seal_pack_failures.inc();
    }

    pub fn reconcile_error(&self) {
        self.reconcile_errors.inc();
    }

    /// Replace the state-derived gauges (the poller's job).
    pub fn set_state(&self, state: State) {
        if let Ok(mut slot) = self.state.lock() {
            *slot = state;
        }
    }

    /// How many notification routes loaded, and how many are configured but unusable.
    pub fn set_notify_routes(&self, ready: u64, error: u64) {
        if let Ok(mut slot) = self.notify_routes.lock() {
            *slot = Some((ready, error));
        }
    }

    /// One grouped notification's outcome.
    pub fn notification(&self, result: crate::notify::SendResult) {
        let i = crate::notify::SendResult::ALL
            .iter()
            .position(|r| *r == result)
            .expect("SendResult::ALL lists every variant");
        self.notifications[i].inc();
    }

    pub fn export_attempt(&self, ok: bool) {
        if ok {
            self.export_attempts_ok.inc()
        } else {
            self.export_attempts_failed.inc()
        }
    }

    pub fn webhook_accepted(&self) {
        self.webhook_accepted.inc();
    }
    pub fn webhook_duplicate(&self) {
        self.webhook_duplicate.inc();
    }
    pub fn webhook_rejected(&self) {
        self.webhook_rejected.inc();
    }
    pub fn webhook_error(&self) {
        self.webhook_error.inc();
    }

    /// Record one poll. Returns `true` when this changed the verdict, so the caller logs the
    /// transition once instead of every interval for the length of an outage.
    ///
    /// The gauge is left raw — a single lost poll flips it — because smoothing belongs in the
    /// alert rule's `for:`, where an operator can see and change it, not buried here.
    pub fn apiserver_poll(&self, result: PollResult) -> Transition {
        let i = PollResult::ALL
            .iter()
            .position(|r| *r == result)
            .expect("PollResult::ALL lists every variant");
        self.apiserver_polls[i].inc();
        if result == PollResult::Ok {
            match std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH) {
                Ok(since) => self
                    .apiserver_last_ok
                    .store(since.as_secs(), Ordering::Relaxed),
                // Don't swallow it. If this is dropped silently the timestamp series stays
                // absent forever while the gauge says the poll worked, and absence is
                // documented to mean "has not polled yet" (docs/COMPATIBILITY.md §2). One
                // state must not carry two meanings.
                Err(e) => tracing::error!(
                    error = %e,
                    "this pod's clock is before 1970, so the last-success timestamp cannot be \
                     published; the poll itself was fine"
                ),
            }
        }
        let now = if result == PollResult::Ok {
            POLL_OK
        } else {
            POLL_BAD
        };
        // Release, paired with the Acquire in `render`: the verdict is the state-defining write
        // and is published last, so a scraper on another core cannot see a fresh verdict beside
        // a stale timestamp.
        match (self.apiserver_poll.swap(now, Ordering::Release), now) {
            (was, is) if was == is => Transition::None,
            (POLL_UNKNOWN, POLL_OK) => Transition::FirstOk,
            (_, POLL_OK) => Transition::Recovered,
            _ => Transition::Broke,
        }
    }

    pub fn signing_key_pinned(&self, key_id: &str) {
        if let Ok(mut slot) = self.signing_key_id.lock() {
            *slot = Some(key_id.to_string());
        }
    }

    /// The Prometheus text exposition of everything above.
    pub fn render(&self) -> String {
        let mut out = String::with_capacity(4096);
        // Read first, with Acquire (paired with the Release in `apiserver_poll`). The poller
        // writes the picture and *then* the verdict; reading them in the same order would let a
        // scrape pair a recovery's verdict with the outage's gauges.
        let poll = self.apiserver_poll.load(Ordering::Acquire);
        metric_header(
            &mut out,
            "kairn_build_info",
            "Controller build, as labels; always 1.",
            "gauge",
        );
        out.push_str(&format!(
            "kairn_build_info{{version=\"{}\",image_digest=\"{}\"}} 1\n",
            escape(env!("CARGO_PKG_VERSION")),
            escape(&std::env::var("KAIRN_IMAGE_DIGEST").unwrap_or_else(|_| "unknown".into()))
        ));

        metric_header(
            &mut out,
            "kairn_captures_total",
            "Captures by outcome (sealed, refused, failed).",
            "counter",
        );
        for (label, value) in [
            ("sealed", self.captures_sealed.get()),
            ("refused", self.captures_refused.get()),
            ("failed", self.captures_failed.get()),
        ] {
            out.push_str(&format!(
                "kairn_captures_total{{result=\"{label}\"}} {value}\n"
            ));
        }
        counter(
            &mut out,
            "kairn_partial_captures_total",
            "Sealed captures where some intended collector did not run.",
            self.partial_captures.get(),
        );
        counter(
            &mut out,
            "kairn_collector_failures_total",
            "Collectors that failed during a capture (the capture itself goes on).",
            self.collector_failures.get(),
        );
        self.capture_seconds.render(
            &mut out,
            "kairn_capture_seconds",
            "Seconds from the start of collection to a sealed bundle.",
            &DURATION_BUCKETS,
            format!("{:.3}", self.capture_seconds.sum_units() as f64 / 1000.0),
        );
        self.bundle_bytes.render(
            &mut out,
            "kairn_bundle_bytes",
            "Size of sealed bundles in bytes.",
            &BYTE_BUCKETS,
            self.bundle_bytes.sum_units().to_string(),
        );

        metric_header(
            &mut out,
            "kairn_seal_attempts_total",
            "KMS signing attempts by outcome.",
            "counter",
        );
        for (label, value) in [
            ("ok", self.seal_attempts_ok.get()),
            ("failed", self.seal_attempts_failed.get()),
        ] {
            out.push_str(&format!(
                "kairn_seal_attempts_total{{result=\"{label}\"}} {value}\n"
            ));
        }
        counter(
            &mut out,
            "kairn_seal_pack_failures_total",
            "Bundles signed by the KMS that could not then be packed (retried).",
            self.seal_pack_failures.get(),
        );

        metric_header(
            &mut out,
            "kairn_export_attempts_total",
            "Object-store export attempts by outcome.",
            "counter",
        );
        for (label, value) in [
            ("ok", self.export_attempts_ok.get()),
            ("failed", self.export_attempts_failed.get()),
        ] {
            out.push_str(&format!(
                "kairn_export_attempts_total{{result=\"{label}\"}} {value}\n"
            ));
        }
        metric_header(
            &mut out,
            "kairn_notifications_total",
            "Grouped incident notifications by outcome (one per incident, not per pod).",
            "counter",
        );
        for (i, result) in crate::notify::SendResult::ALL.iter().enumerate() {
            out.push_str(&format!(
                "kairn_notifications_total{{result=\"{}\"}} {}\n",
                result.label(),
                self.notifications[i].get()
            ));
        }
        // Absent when no route is configured at all, so "notification is off" and
        // "notification is broken" are never the same reading.
        if let Some((ready, error)) = self.notify_routes.lock().ok().and_then(|s| *s) {
            metric_header(
                &mut out,
                "kairn_notify_routes",
                "Configured notification routes by state; error means the route is disabled.",
                "gauge",
            );
            out.push_str(&format!("kairn_notify_routes{{state=\"ready\"}} {ready}\n"));
            out.push_str(&format!("kairn_notify_routes{{state=\"error\"}} {error}\n"));
        }
        counter(
            &mut out,
            "kairn_reconcile_errors_total",
            "Reconcile attempts that ended in an error (the capture is retried).",
            self.reconcile_errors.get(),
        );

        // State-derived gauges: emitted only once a poll has succeeded, so a scrape never
        // reads "0 captures" when the controller simply hasn't looked yet.
        let state = self
            .state
            .lock()
            .ok()
            .map(|s| s.clone())
            .unwrap_or_default();
        if state.known {
            metric_header(
                &mut out,
                "kairn_captures",
                "Captures that exist right now, by phase.",
                "gauge",
            );
            for phase in ["pending", "capturing", "sealing", "exported", "failed"] {
                let n = state.captures_by_phase.get(phase).copied().unwrap_or(0);
                out.push_str(&format!("kairn_captures{{phase=\"{phase}\"}} {n}\n"));
            }
            gauge(
                &mut out,
                "kairn_captures_awaiting_seal",
                "Captures collected and waiting for a signature (phase Sealing).",
                state.captures_by_phase.get("sealing").copied().unwrap_or(0) as i64,
            );
            metric_header(
                &mut out,
                "kairn_export_destinations",
                "Export destinations of existing captures, by state. `pending` is still \
                 being retried; `refused`, `conflict` and `failed` are terminal: the \
                 evidence never reached that destination.",
                "gauge",
            );
            for st in ["pending", "uploaded", "refused", "conflict", "failed"] {
                let n = state.destinations_by_state.get(st).copied().unwrap_or(0);
                out.push_str(&format!(
                    "kairn_export_destinations{{state=\"{st}\"}} {n}\n"
                ));
            }
            gauge(
                &mut out,
                "kairn_exports_unsettled",
                "Export destinations still being retried (state pending).",
                state
                    .destinations_by_state
                    .get("pending")
                    .copied()
                    .unwrap_or(0) as i64,
            );
        }

        // Kept out of the `state.known` block above on purpose: "the picture is unknown" and
        // "the API cannot be used" are different facts, and the gauges above deliberately hold
        // their last value through an outage. Something has to say the value is stale.
        if poll != POLL_UNKNOWN {
            gauge(
                &mut out,
                "kairn_apiserver_poll_ok",
                "1 if the poller's last list of IncidentCapture succeeded, 0 if it did not. \
                 Read from the work the controller already has to do, not a synthetic probe. \
                 Absent until the first poll returns.",
                i64::from(poll == POLL_OK),
            );
        }
        metric_header(
            &mut out,
            "kairn_apiserver_polls_total",
            "Polls of the API server by outcome. `forbidden`, `unauthorized`, `not-found` and \
             `api-error` mean the API server answered — so the network is fine and the problem \
             is RBAC, the token or the CRD; `unreachable` means no answer came at all.",
            "counter",
        );
        for result in PollResult::ALL {
            let i = PollResult::ALL.iter().position(|r| *r == result).unwrap();
            out.push_str(&format!(
                "kairn_apiserver_polls_total{{result=\"{}\"}} {}\n",
                result.label(),
                self.apiserver_polls[i].get()
            ));
        }
        let last_ok = self.apiserver_last_ok.load(Ordering::Relaxed);
        if last_ok > 0 {
            gauge(
                &mut out,
                "kairn_apiserver_last_success_timestamp_seconds",
                "When the API server was last used successfully, in Unix seconds. Absent until \
                 there has been a success. Read it on a dashboard, not in an alert: it is this \
                 pod's clock, and subtracting it from Prometheus's clock pages on skew alone.",
                last_ok as i64,
            );
        }

        metric_header(
            &mut out,
            "kairn_webhook_requests_total",
            "Alert webhook requests by outcome. `rejected` is a failed bearer token; `error` is \
             an authenticated alert the API server would not let become a capture, which is what \
             a missing `create` permission looks like.",
            "counter",
        );
        for (label, value) in [
            ("accepted", self.webhook_accepted.get()),
            ("duplicate", self.webhook_duplicate.get()),
            ("rejected", self.webhook_rejected.get()),
            ("error", self.webhook_error.get()),
        ] {
            out.push_str(&format!(
                "kairn_webhook_requests_total{{result=\"{label}\"}} {value}\n"
            ));
        }

        if let Some(key_id) = self.signing_key_id.lock().ok().and_then(|s| s.clone()) {
            metric_header(
                &mut out,
                "kairn_signing_key_info",
                "The pinned signing key id (ieb/v1 key_id); always 1 once pinned.",
                "gauge",
            );
            out.push_str(&format!(
                "kairn_signing_key_info{{key_id=\"{}\"}} 1\n",
                escape(&key_id)
            ));
        }
        out
    }
}

fn metric_header(out: &mut String, name: &str, help: &str, kind: &str) {
    out.push_str(&format!("# HELP {name} {help}\n# TYPE {name} {kind}\n"));
}

fn counter(out: &mut String, name: &str, help: &str, value: u64) {
    metric_header(out, name, help, "counter");
    out.push_str(&format!("{name} {value}\n"));
}

fn gauge(out: &mut String, name: &str, help: &str, value: i64) {
    metric_header(out, name, help, "gauge");
    out.push_str(&format!("{name} {value}\n"));
}

/// Escape a label value (the exposition format's three escapes).
fn escape(v: &str) -> String {
    v.replace('\\', "\\\\")
        .replace('"', "\\\"")
        .replace('\n', "\\n")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn exposition_is_well_formed() {
        let m = Metrics::default();
        m.capture(CaptureResult::Sealed);
        m.capture(CaptureResult::Refused);
        m.sealed(3.25, 48_213, true);
        m.seal_attempt(false);
        m.seal_pack_failure();
        m.reconcile_error();
        m.export_attempt(true);
        m.set_state(State {
            captures_by_phase: [("sealing".to_string(), 2), ("exported".to_string(), 7)].into(),
            destinations_by_state: [("pending".to_string(), 1), ("conflict".to_string(), 3)].into(),
            known: true,
        });
        m.webhook_accepted();
        m.signing_key_pinned("ab\"cd");
        m.apiserver_poll(PollResult::Ok);
        m.apiserver_poll(PollResult::Forbidden);
        let text = m.render();

        // Every non-comment line is `name value` or `name{labels} value`, and every metric
        // has HELP and TYPE before its samples.
        let mut declared = std::collections::BTreeSet::new();
        for line in text.lines() {
            if let Some(rest) = line.strip_prefix("# HELP ") {
                declared.insert(rest.split(' ').next().unwrap().to_string());
                continue;
            }
            if line.starts_with("# TYPE ") {
                continue;
            }
            let (series, value) = line.rsplit_once(' ').expect("`name value`");
            value.parse::<f64>().expect("a number");
            let name = series.split(['{']).next().unwrap();
            let base = name
                .strip_suffix("_bucket")
                .or(name.strip_suffix("_sum"))
                .or(name.strip_suffix("_count"))
                .unwrap_or(name);
            assert!(declared.contains(base), "{base} has no HELP: {line}");
            assert!(name.starts_with("kairn_"), "{name} is not namespaced");
        }
        // A few specific series.
        assert!(text.contains("kairn_captures_total{result=\"sealed\"} 1\n"));
        assert!(text.contains("kairn_captures_total{result=\"failed\"} 0\n"));
        assert!(text.contains("kairn_partial_captures_total 1\n"));
        assert!(text.contains("kairn_capture_seconds_bucket{le=\"5\"} 1\n"));
        assert!(text.contains("kairn_capture_seconds_bucket{le=\"2.5\"} 0\n"));
        assert!(text.contains("kairn_capture_seconds_sum 3.250\n"));
        assert!(text.contains("kairn_captures_awaiting_seal 2\n"));
        assert!(text.contains("kairn_captures{phase=\"sealing\"} 2\n"));
        assert!(text.contains("kairn_captures{phase=\"pending\"} 0\n"));
        assert!(text.contains("kairn_export_destinations{state=\"conflict\"} 3\n"));
        assert!(text.contains("kairn_exports_unsettled 1\n"));
        // The last poll failed, so the gauge is 0 — but the earlier success is still dated,
        // and the failure says *why*, which is the operator's first question.
        assert!(text.contains("kairn_apiserver_poll_ok 0\n"));
        assert!(text.contains("kairn_apiserver_polls_total{result=\"forbidden\"} 1\n"));
        assert!(text.contains("kairn_apiserver_polls_total{result=\"unreachable\"} 0\n"));
        assert!(text.contains("kairn_apiserver_last_success_timestamp_seconds "));
        // Bundle sizes get byte buckets, not the duration ones.
        assert!(text.contains("kairn_bundle_bytes_bucket{le=\"65536\"} 1\n"));
        assert!(text.contains("kairn_bundle_bytes_bucket{le=\"1073741824\"} 1\n"));
        assert!(text.contains("kairn_bundle_bytes_sum 48213\n"));
        assert!(text.contains("kairn_bundle_bytes_count 1\n"));
        // Label values are escaped.
        assert!(text.contains("kairn_signing_key_info{key_id=\"ab\\\"cd\"} 1\n"));
    }

    /// The documented set (docs/COMPATIBILITY.md §2): renaming or dropping one is a
    /// breaking change, so it fails here first.
    #[test]
    fn every_documented_series_is_emitted() {
        let m = Metrics::default();
        m.signing_key_pinned("f00d");
        m.set_state(State {
            known: true,
            ..Default::default()
        });
        m.set_notify_routes(1, 0);
        m.apiserver_poll(PollResult::Ok);
        let text = m.render();
        let names: std::collections::BTreeSet<&str> = text
            .lines()
            .filter_map(|l| l.strip_prefix("# TYPE "))
            .map(|l| l.split(' ').next().unwrap())
            .collect();
        let documented = [
            "kairn_build_info",
            "kairn_captures_total",
            "kairn_partial_captures_total",
            "kairn_collector_failures_total",
            "kairn_capture_seconds",
            "kairn_bundle_bytes",
            "kairn_seal_attempts_total",
            "kairn_seal_pack_failures_total",
            "kairn_reconcile_errors_total",
            "kairn_captures",
            "kairn_captures_awaiting_seal",
            "kairn_export_attempts_total",
            "kairn_export_destinations",
            "kairn_exports_unsettled",
            "kairn_notifications_total",
            "kairn_notify_routes",
            "kairn_webhook_requests_total",
            "kairn_signing_key_info",
            "kairn_apiserver_poll_ok",
            "kairn_apiserver_polls_total",
            "kairn_apiserver_last_success_timestamp_seconds",
        ];
        for name in documented {
            assert!(names.contains(name), "{name} is no longer emitted");
        }
        assert_eq!(
            names.len(),
            documented.len(),
            "a series was added without documenting it: {names:?}"
        );
    }

    /// A pod that has not polled yet must not claim either answer. Reporting `1` would hide a
    /// controller that never reached the API at all; reporting `0` would alert on every rollout
    /// for the first interval.
    #[test]
    fn the_verdict_is_absent_until_the_first_poll_returns() {
        let m = Metrics::default();
        let text = m.render();
        assert!(!text.contains("kairn_apiserver_poll_ok"), "{text}");
        assert!(
            !text.contains("kairn_apiserver_last_success_timestamp_seconds"),
            "{text}"
        );
        // The counter is still there at zero, as a counter should be. It is also the only one of
        // the three emitted from process start, which is what makes `absent()` on it the
        // sentinel for "this controller is not reporting at all" (docs/metrics.md).
        assert!(text.contains("kairn_apiserver_polls_total{result=\"ok\"} 0\n"));

        // A first poll that fails says so — that case is a real outage, not a warm-up.
        m.apiserver_poll(PollResult::Unreachable);
        let text = m.render();
        assert!(text.contains("kairn_apiserver_poll_ok 0\n"), "{text}");
        assert!(!text.contains("kairn_apiserver_last_success_timestamp_seconds"));
    }

    /// The poller logs the transition, not every failed poll: an hour-long outage would
    /// otherwise write 120 identical warnings, which is how a real signal gets filtered out.
    /// A change of *reason* without a change of verdict is not a transition — still broken is
    /// still broken.
    #[test]
    fn only_a_change_in_the_verdict_is_worth_a_log_line() {
        let m = Metrics::default();
        use Transition::*;
        assert_eq!(m.apiserver_poll(PollResult::Ok), FirstOk, "a cold start");
        assert_eq!(
            m.apiserver_poll(PollResult::Ok),
            None,
            "still ok says nothing"
        );
        assert_eq!(m.apiserver_poll(PollResult::Forbidden), Broke, "ok → bad");
        // Still broken, for a different reason: not a transition. An hour-long outage must not
        // write a line every interval, or the one that matters is lost in 120 copies.
        assert_eq!(m.apiserver_poll(PollResult::Unreachable), None, "bad → bad");
        assert_eq!(
            m.apiserver_poll(PollResult::Ok),
            Recovered,
            "and a recovery is not a cold start"
        );
        let text = m.render();
        assert!(
            text.contains("kairn_apiserver_polls_total{result=\"ok\"} 3\n"),
            "{text}"
        );
        assert!(text.contains("kairn_apiserver_polls_total{result=\"forbidden\"} 1\n"));
        assert!(text.contains("kairn_apiserver_polls_total{result=\"unreachable\"} 1\n"));
    }

    /// The timestamp survives the outage it is meant to describe. If a failed poll cleared it,
    /// nothing would say *when* the controller went blind.
    #[test]
    fn a_failed_poll_does_not_erase_the_last_success() {
        let m = Metrics::default();
        m.apiserver_poll(PollResult::Ok);
        let dated = m.apiserver_last_ok.load(Ordering::Relaxed);
        assert!(dated > 1_700_000_000, "a plausible unix time, got {dated}");
        m.apiserver_poll(PollResult::Unreachable);
        m.apiserver_poll(PollResult::Forbidden);
        assert_eq!(m.apiserver_last_ok.load(Ordering::Relaxed), dated);
        assert!(m.render().contains(&format!(
            "kairn_apiserver_last_success_timestamp_seconds {dated}\n"
        )));
    }

    /// An HTTP status means the API server answered, so the network is not the problem. Getting
    /// this backwards would send an operator to the wrong place: the alert text says "check RBAC
    /// and any egress policy" precisely because the metric used to be unable to say which.
    #[test]
    fn an_answer_and_no_answer_are_classified_apart() {
        fn api(code: u16) -> kube::Error {
            kube::Error::Api(kube::error::ErrorResponse {
                status: "Failure".into(),
                message: "no".into(),
                reason: "Forbidden".into(),
                code,
            })
        }
        assert_eq!(PollResult::of(&api(403)), PollResult::Forbidden);
        assert_eq!(PollResult::of(&api(401)), PollResult::Unauthorized);
        assert_eq!(PollResult::of(&api(404)), PollResult::NotFound);
        assert_eq!(PollResult::of(&api(429)), PollResult::ApiError);
        assert_eq!(PollResult::of(&api(503)), PollResult::ApiError);
        // Anything that is not an HTTP answer never reached a server.
        assert_eq!(
            PollResult::of(&kube::Error::LinesCodecMaxLineLengthExceeded),
            PollResult::Unreachable
        );
    }

    /// A fake API server for the poller. `code` 200 serves one empty `IncidentCapture` list;
    /// anything else returns that status. `hang` holds every request open instead of answering,
    /// which is what a dropped egress packet looks like from inside the pod.
    async fn fake_apiserver(code: u16, hang: bool) -> kube::Api<crate::crd::IncidentCapture> {
        let _ = rustls::crypto::ring::default_provider().install_default();
        let app = axum::Router::new().fallback(move || async move {
            if hang {
                tokio::time::sleep(Duration::from_secs(600)).await;
            }
            let body = axum::Json(serde_json::json!({
                "apiVersion": "kairn.dev/v1alpha1",
                "kind": "IncidentCaptureList",
                "metadata": { "resourceVersion": "1" },
                "items": [],
            }));
            (axum::http::StatusCode::from_u16(code).unwrap(), body)
        });
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
        let cfg = kube::Config::new(format!("http://{addr}/").parse().unwrap());
        kube::Api::namespaced(kube::Client::try_from(cfg).unwrap(), "kairn-system")
    }

    /// The caller, not the primitive. While the recording lived inline in the poll loop, two
    /// separate mutations of it — never reporting a failure, and swapping success for failure so
    /// the metric meant the opposite — left all 75 unit tests green, because every test drove
    /// `apiserver_poll` directly and nothing drove its only caller.
    #[tokio::test]
    async fn a_working_api_server_is_recorded_as_a_usable_one() {
        let m = Metrics::default();
        poll_once(
            &fake_apiserver(200, false).await,
            &m,
            Duration::from_secs(5),
        )
        .await;
        let text = m.render();
        assert!(text.contains("kairn_apiserver_poll_ok 1\n"), "{text}");
        assert!(
            text.contains("kairn_apiserver_polls_total{result=\"ok\"} 1\n"),
            "{text}"
        );
        // The state gauges appear too: a successful poll is also how they are refreshed.
        assert!(
            text.contains("kairn_captures{phase=\"pending\"} 0\n"),
            "{text}"
        );
    }

    /// A 403 must not read as a network problem: the API server answered.
    #[tokio::test]
    async fn a_refusing_api_server_is_recorded_as_forbidden_not_unreachable() {
        let m = Metrics::default();
        poll_once(
            &fake_apiserver(403, false).await,
            &m,
            Duration::from_secs(5),
        )
        .await;
        let text = m.render();
        assert!(text.contains("kairn_apiserver_poll_ok 0\n"), "{text}");
        assert!(
            text.contains("kairn_apiserver_polls_total{result=\"forbidden\"} 1\n"),
            "{text}"
        );
        assert!(text.contains("kairn_apiserver_polls_total{result=\"unreachable\"} 0\n"));
        // Nothing was learned about the captures, so the picture stays unknown rather than zero.
        assert!(!text.contains("kairn_captures{"), "{text}");
    }

    /// The one that was broken. `kube`'s only ceiling is a 295 s read timeout, so a poll that
    /// hangs instead of failing used to report *nothing* — the gauge sat at its last value for
    /// almost five minutes while every capture stopped. A blackholed API server must count as
    /// unreachable within one interval.
    #[tokio::test]
    async fn a_blackholed_api_server_counts_as_unreachable_within_one_interval() {
        let m = Metrics::default();
        let api = fake_apiserver(200, true).await;
        let budget = Duration::from_millis(200);
        // The outer timeout matters: without the budget inside `poll_once` this test would sit
        // for kube's 295 s read timeout instead of failing, and a test that hangs is a test
        // nobody keeps. (Measured: reverting the budget made exactly that happen, at 295.77 s.)
        tokio::time::timeout(Duration::from_secs(5), poll_once(&api, &m, budget))
            .await
            .expect("poll_once must return within its budget, not wait for the read timeout");
        let text = m.render();
        assert!(text.contains("kairn_apiserver_poll_ok 0\n"), "{text}");
        assert!(
            text.contains("kairn_apiserver_polls_total{result=\"unreachable\"} 1\n"),
            "{text}"
        );
    }

    #[test]
    fn histogram_buckets_are_cumulative() {
        let h = Histogram::<{ DURATION_BUCKETS.len() }>::default();
        for s in [0.1, 3.0, 3.0, 1200.0] {
            h.observe(s, (s * 1000.0) as u64, &DURATION_BUCKETS);
        }
        let mut out = String::new();
        h.render(
            &mut out,
            "kairn_test_seconds",
            "test",
            &DURATION_BUCKETS,
            format!("{:.3}", h.sum_units() as f64 / 1000.0),
        );
        assert!(out.contains("kairn_test_seconds_bucket{le=\"0.5\"} 1\n"));
        assert!(out.contains("kairn_test_seconds_bucket{le=\"5\"} 3\n"));
        assert!(out.contains("kairn_test_seconds_bucket{le=\"900\"} 3\n"));
        assert!(out.contains("kairn_test_seconds_bucket{le=\"+Inf\"} 4\n"));
        assert!(out.contains("kairn_test_seconds_count 4\n"));
    }
}
