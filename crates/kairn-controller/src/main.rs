//! Kairn controller (placeholder).
//!
//! Tracer-bullet tasks landing here (see `docs/design-review-round3.md`):
//!   2. `IncidentCapture` + `CaptureProfile` CRDs via kube-derive (+ a `crdgen` subcommand).
//!   3. Alertmanager webhook receiver (axum) → create an `IncidentCapture` CR.
//!   4. Reconcile phase machine: Pending → Capturing → Sealing → Exported | Failed.
//!   5. Collector: log tails incl. `previous=true` (the timing-sensitive money collector).
//!   8. PVC export.
//!
//! Sealing/signing/verification live in the `kairn-bundle` crate (already implemented).

fn main() -> anyhow::Result<()> {
    println!("kairn-controller: not yet wired to the cluster. See docs/design-review-round3.md.");
    Ok(())
}
