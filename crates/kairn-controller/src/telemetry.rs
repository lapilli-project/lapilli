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
    /// `1` once the KMS public key is pinned, with its key id as a label.
    signing_key_id: std::sync::Mutex<Option<String>>,
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
            match api.list(&kube::api::ListParams::default()).await {
                Ok(list) => {
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
                    metrics().set_state(state);
                }
                // Keep the last picture rather than reporting zeros.
                Err(e) => tracing::debug!(error = %e, "metrics: listing captures failed"),
            }
            tokio::time::sleep(interval).await;
        }
    });
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

    pub fn signing_key_pinned(&self, key_id: &str) {
        if let Ok(mut slot) = self.signing_key_id.lock() {
            *slot = Some(key_id.to_string());
        }
    }

    /// The Prometheus text exposition of everything above.
    pub fn render(&self) -> String {
        let mut out = String::with_capacity(4096);
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

        metric_header(
            &mut out,
            "kairn_webhook_requests_total",
            "Alert webhook requests by outcome (accepted, duplicate, rejected).",
            "counter",
        );
        for (label, value) in [
            ("accepted", self.webhook_accepted.get()),
            ("duplicate", self.webhook_duplicate.get()),
            ("rejected", self.webhook_rejected.get()),
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
