//! `lapilli demo` — a synthetic incident, end to end, on a cluster where Lapilli is installed.
//!
//! It stages a realistic "bad rollout": a healthy v1 `checkout` Deployment, then a v2 whose
//! only change is one config env var — which makes the new pod crash-loop (or get
//! OOMKilled). Once the crashed instance exists, it fires an Alertmanager-shaped webhook,
//! waits for the `IncidentCapture` to export, pulls the `.ieb` out of the cluster, verifies
//! it offline, and prints what you would otherwise have lost — read from the **bundle
//! file**, not the cluster.
//!
//! This is also the standing kind E2E harness (DESIGN §8, round-2 T4), so it drives the
//! cluster through `kubectl` rather than linking kube-rs: `lapilli-cli` stays dependent on
//! `lapilli-bundle` only.

use std::io::Write;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

use anyhow::{anyhow, bail, Context, Result};
use clap::{Args, ValueEnum};
use lapilli_bundle::{verify_bundle, VerifyOptions, VerifyReport};
use serde_json::{json, Value};

const DEMO_NS: &str = "lapilli-demo";
const APP: &str = "checkout";
const CONTAINER: &str = "app";
/// A fake credential planted in the demo app's env. Redaction must keep it out of every
/// bundle file; `test/e2e/run.sh` greps for it.
pub const CANARY: &str = "lapilliDemoCanary7Qx2Lp9w";
/// Field manager for the demo's applies — it shows up as the "who" in `changes.json`.
const DEPLOYER: &str = "demo-deployer";

#[derive(Args, Debug)]
pub struct DemoArgs {
    /// Which incident to stage.
    #[arg(long, value_enum, default_value_t = Scenario::Crashloop)]
    pub scenario: Scenario,
    /// Namespace Lapilli is installed in.
    #[arg(long, default_value = "lapilli-system")]
    pub lapilli_namespace: String,
    /// kubeconfig context to use (defaults to the current context).
    #[arg(long)]
    pub context: Option<String>,
    /// Directory to write `<incident>.ieb` and its unpacked `<incident>/` into.
    #[arg(long, default_value = ".")]
    pub out: PathBuf,
    /// Leave the `lapilli-demo` namespace in place afterwards.
    #[arg(long)]
    pub keep: bool,
    /// Trusted public key (from `lapilli keygen`) to verify the bundle's signature against.
    /// Use when the chart runs with `signing.mode=static`.
    #[arg(long)]
    pub key: Option<PathBuf>,
    /// Per-step timeout, in seconds.
    #[arg(long, default_value_t = 180)]
    pub timeout: u64,
}

#[derive(ValueEnum, Clone, Copy, Debug, PartialEq, Eq)]
pub enum Scenario {
    /// v2 config makes the app exit on startup → CrashLoopBackOff.
    Crashloop,
    /// v2 config makes the app blow its memory limit → OOMKilled.
    Oomkill,
}

impl Scenario {
    fn name(self) -> &'static str {
        match self {
            Scenario::Crashloop => "crashloop",
            Scenario::Oomkill => "oomkill",
        }
    }

    fn alertname(self) -> &'static str {
        match self {
            Scenario::Crashloop => "KubePodCrashLooping",
            Scenario::Oomkill => "KubeContainerOOMKilled",
        }
    }

    /// The app's startup script. Identical for v1 and v2 — only `CACHE_WARMUP` differs, so
    /// the incident is caused purely by a config change (what `changes.json` should reveal).
    fn script(self) -> &'static str {
        match self {
            Scenario::Crashloop => concat!(
                "echo \"[checkout] starting, CACHE_WARMUP=$CACHE_WARMUP\"; ",
                "if [ \"$CACHE_WARMUP\" = eager ]; then ",
                "echo '[checkout] warming cache from redis://cache:6379 ...'; ",
                "echo '[checkout] FATAL: cache warmup failed: connection refused (redis://cache:6379)'; ",
                "exit 1; fi; ",
                "echo '[checkout] ready'; while true; do sleep 3600; done"
            ),
            // A steady leak (~2 MiB/s) into an awk array, not an instant spike and not shell
            // string concatenation (which transiently doubles memory, so samples would show a
            // plateau at half the limit). The log records the climb and Prometheus gets a
            // clean curve up to the limit before the OOM kill.
            Scenario::Oomkill => concat!(
                "echo \"[checkout] starting, CACHE_WARMUP=$CACHE_WARMUP\"; ",
                "if [ \"$CACHE_WARMUP\" = eager ]; then ",
                "echo '[checkout] warming cache: loading every catalog page into memory'; ",
                "awk 'BEGIN { while (1) { a[i++] = sprintf(\"%1048576s\", \"\"); ",
                "printf \"[checkout] cache pages loaded: %d MiB\\n\", i; fflush(); ",
                "system(\"sleep 0.5\") } }'; fi; ",
                "echo '[checkout] ready'; while true; do sleep 3600; done"
            ),
        }
    }
}

pub fn run(args: DemoArgs) -> Result<i32> {
    let k = Kubectl {
        context: args.context.clone(),
    };
    let timeout = Duration::from_secs(args.timeout);
    let started = Instant::now();
    println!("lapilli demo — scenario: {}\n", args.scenario.name());

    let ctrl_pod = preflight(&k, &args.lapilli_namespace)?;
    ok(format!(
        "Lapilli controller ready ({}/{ctrl_pod})",
        args.lapilli_namespace
    ));

    // Fresh namespace every run, so leftovers from a --keep run can't skew the capture.
    k.run(&[
        "delete",
        "namespace",
        DEMO_NS,
        "--ignore-not-found",
        "--wait=true",
    ])?;
    apply(&k, &manifest(args.scenario, "lazy"))?;
    k.run(&[
        "-n",
        DEMO_NS,
        "rollout",
        "status",
        &format!("deploy/{APP}"),
        &format!("--timeout={}s", args.timeout),
    ])
    .context("v1 never became ready")?;
    ok(format!(
        "deployed {APP} v1 (CACHE_WARMUP=lazy) — healthy, namespace {DEMO_NS}"
    ));

    apply(&k, &manifest(args.scenario, "eager"))?;
    ok("rolled out v2 (CACHE_WARMUP=eager) — the one-line config change behind this incident");

    let (pod, reason) = wait_for_crash(&k, timeout)?;
    ok(format!(
        "pod {pod} crashed ({reason}); its logs are now one kubelet GC away from gone"
    ));

    let capture = fire_alert(&k, &args, &pod)?;
    ok(format!(
        "fired {} → IncidentCapture {capture}",
        args.scenario.alertname()
    ));

    let ic = wait_for_export(&k, &args.lapilli_namespace, &capture, timeout)?;
    ok("capture sealed and exported");

    std::fs::create_dir_all(&args.out)?;
    let ieb = args.out.join(format!("{}.ieb", ic.incident_id));
    let ctrl_pod = preflight(&k, &args.lapilli_namespace)?;
    fetch_bundle(
        &k,
        &args.lapilli_namespace,
        &ctrl_pod,
        &ic.bundle_path,
        &ieb,
    )?;
    let size = std::fs::metadata(&ieb)?.len();
    ok(format!(
        "fetched {} ({:.1} KiB)",
        ieb.display(),
        size as f64 / 1024.0
    ));

    let trusted_key_pem = args
        .key
        .as_ref()
        .map(std::fs::read_to_string)
        .transpose()
        .context("reading --key")?;
    let report = verify_bundle(
        &ieb,
        &VerifyOptions {
            expected_cluster: Some(ic.cluster_id.clone()),
            expected_incident: Some(ic.incident_id.clone()),
            trusted_key_pem,
        },
    )?;
    let key_arg = args
        .key
        .as_ref()
        .map(|k| format!(" --key {}", k.display()))
        .unwrap_or_default();
    let mark = if report.verdict == lapilli_bundle::Verdict::Ok {
        "✓"
    } else {
        "✗"
    };
    println!(
        "  {mark} lapilli verify {} --cluster {} --incident {}{key_arg}\n      {}",
        ieb.display(),
        ic.cluster_id,
        ic.incident_id,
        crate::report_line(&report)
    );
    for p in &report.problems {
        println!("      - {p}");
    }
    if let Some(w) = crate::redaction_warning(&report) {
        println!("      ! {w}");
    }

    let dir = args.out.join(&ic.incident_id);
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir)?;
    lapilli_bundle::unpack(&ieb, &dir)?;
    print_findings(&dir, &report)?;

    if args.keep {
        println!("\n(kept namespace {DEMO_NS}; delete it with `kubectl delete ns {DEMO_NS}`)");
    } else {
        k.run(&["delete", "namespace", DEMO_NS, "--wait=false"])?;
    }
    println!(
        "\nDone in {:.0}s. Browse the evidence: {}",
        started.elapsed().as_secs_f64(),
        dir.display()
    );
    Ok(report.verdict.exit_code())
}

// --- steps -------------------------------------------------------------------------------

/// Lapilli must already be installed; returns a Running controller pod to exec into.
fn preflight(k: &Kubectl, ns: &str) -> Result<String> {
    let pods = k
        .json(&[
            "-n",
            ns,
            "get",
            "pods",
            "-l",
            "app.kubernetes.io/name=lapilli",
            "-o",
            "json",
        ])
        .with_context(|| format!("cannot reach the cluster or namespace {ns}"))?;
    running_pod(&pods).ok_or_else(|| {
        anyhow!(
            "no running Lapilli controller in namespace {ns}. Install Lapilli first \
             (see README → Quickstart), or pass --lapilli-namespace."
        )
    })
}

fn apply(k: &Kubectl, manifest: &str) -> Result<()> {
    k.run_stdin(
        &[
            "apply",
            "--server-side",
            "--field-manager",
            DEPLOYER,
            "-f",
            "-",
        ],
        manifest.as_bytes(),
    )
    .map(drop)
}

/// Waits until a pod of the v2 ReplicaSet has restarted at least once — only then does a
/// `previous` container instance exist for the logs collector to recover.
fn wait_for_crash(k: &Kubectl, timeout: Duration) -> Result<(String, String)> {
    poll(timeout, "the v2 pod to crash and restart", || {
        let pods = k.json(&[
            "-n",
            DEMO_NS,
            "get",
            "pods",
            "-l",
            &format!("app={APP}"),
            "-o",
            "json",
        ])?;
        Ok(crashed_pod(&pods))
    })
}

fn fire_alert(k: &Kubectl, args: &DemoArgs, pod: &str) -> Result<String> {
    let payload = json!({
        "version": "4",
        "status": "firing",
        "receiver": "lapilli",
        "alerts": [{
            "status": "firing",
            "labels": {
                "alertname": args.scenario.alertname(),
                "severity": "warning",
                "namespace": DEMO_NS,
                "pod": pod,
                "container": CONTAINER,
                // Demo captures stay local: never land in a (possibly WORM) bucket.
                "lapilli.dev/export": "local",
            },
            "annotations": { "summary": format!("lapilli demo: {} ({})", APP, args.scenario.name()) },
        }],
    });
    // POST from inside the controller pod (`lapilli post-alert` → 127.0.0.1:8080), which
    // presents the webhook token mounted there. The API server's service proxy can't carry
    // an Authorization header, and this way the token never leaves the pod.
    let ctrl = preflight(k, &args.lapilli_namespace)?;
    let out = k
        .run_stdin(
            &[
                "-n",
                &args.lapilli_namespace,
                "exec",
                "-i",
                &ctrl,
                "-c",
                "controller",
                "--",
                "/usr/local/bin/lapilli",
                "post-alert",
            ],
            payload.to_string().as_bytes(),
        )
        .context("webhook POST failed")?;
    let resp: Value =
        serde_json::from_str(&out).with_context(|| format!("unexpected webhook reply: {out}"))?;
    resp["captures"][0]
        .as_str()
        .map(str::to_string)
        .ok_or_else(|| anyhow!("webhook created no capture: {out}"))
}

struct Exported {
    bundle_path: String,
    cluster_id: String,
    incident_id: String,
}

fn wait_for_export(k: &Kubectl, ns: &str, name: &str, timeout: Duration) -> Result<Exported> {
    poll(timeout, "the capture to export", || {
        let ic = k.json(&["-n", ns, "get", "incidentcapture", name, "-o", "json"])?;
        match ic["status"]["phase"].as_str() {
            Some("Exported") => Ok(Some(Exported {
                bundle_path: str_at(&ic, &["status", "bundlePath"])?,
                cluster_id: str_at(&ic, &["spec", "clusterId"])?,
                incident_id: str_at(&ic, &["spec", "incidentId"])?,
            })),
            Some("Failed") => bail!(
                "capture {name} failed: {}",
                ic["status"]["message"].as_str().unwrap_or("(no message)")
            ),
            _ => Ok(None),
        }
    })
}

/// The controller image is distroless (no `tar`, so no `kubectl cp`): stream the file
/// through `lapilli cat-bundle` over `kubectl exec` instead. Transfer corruption can't go
/// unnoticed — `lapilli verify` re-hashes every file right after.
fn fetch_bundle(k: &Kubectl, ns: &str, pod: &str, remote: &str, local: &Path) -> Result<()> {
    let out = k
        .cmd()
        .args(["-n", ns, "exec", pod, "-c", "controller", "--"])
        .args(["/usr/local/bin/lapilli", "cat-bundle", remote])
        .output()
        .context("running kubectl exec")?;
    if !out.status.success() {
        bail!(
            "fetching {remote} failed: {}",
            String::from_utf8_lossy(&out.stderr).trim()
        );
    }
    std::fs::write(local, out.stdout)?;
    Ok(())
}

// --- findings: read from the bundle, not the cluster ------------------------------------

fn print_findings(dir: &Path, report: &VerifyReport) -> Result<()> {
    let f = Findings::from_bundle(dir)?;
    println!("\nWhat this bundle kept that the cluster was about to lose:");
    if let Some(tail) = &f.log_tail {
        // Only an untruncated tail may be called last words. A tail the collector cut at its
        // byte bound is missing the lines nearest the crash, so the demo says what it has
        // instead of claiming what it does not (spec/IEB-SPEC.md §`logs/index.json`).
        match &tail.cut_at_the_crash_end {
            None => println!("\n  last words of the crashed instance ({}):", tail.file),
            Some(why) => println!(
                "\n  the crashed instance's log, {why} ({}) — NOT its last words:",
                tail.file
            ),
        }
        for line in &tail.lines {
            println!("    │ {line}");
        }
        if tail.cut_at_the_crash_end.is_some() {
            println!(
                "    (the byte budget is spent forward from the start of the tail, so the lines"
            );
            println!("     nearest the crash are the ones that were dropped.)");
        }
    }
    for gap in &f.log_gaps {
        println!("    (gone already: {gap})");
    }
    if let Some(t) = &f.termination {
        println!("\n  how it died (resources/pod.json):  {t}");
    }
    if let Some(d) = &f.diff {
        println!("  what changed (diffs/):             {}", d.headline);
        for line in &d.lines {
            println!("    │ {line}");
        }
    } else if let Some(c) = &f.change {
        println!("  what changed (changes.json):       {c}");
    }
    if let Some(m) = &f.memory {
        println!("  memory (metrics/):                 {m}");
    }
    if !f.timeline.is_empty() {
        println!(
            "  timeline (timeline.json):          {} events — {}",
            f.timeline_len,
            f.timeline.join(" → ")
        );
    }
    if report.partial {
        println!("\n  note: PARTIAL capture — some collectors did not run (see verify output).");
    }
    Ok(())
}

/// The tail of the most recently terminated instance's log, and whether it may be called the
/// crash's last words.
///
/// The demo reads the bundle itself rather than going through `lapilli_bundle::summary` — that
/// is the point of the section it prints — so the rule about truncated tails has to hold here
/// too, and [`Summary`](lapilli_bundle::summary::Summary) is the other copy of it.
#[derive(Debug, Default, PartialEq)]
struct LogTail {
    /// Path inside the bundle.
    file: String,
    lines: Vec<String>,
    /// Set when `logs/index.json` says the collector's byte bound cut the end of this tail —
    /// the end nearest the crash. Then `lines` are real log lines but **not** the last ones,
    /// and nothing may present them as the crash's last words. The string is how to say it.
    cut_at_the_crash_end: Option<String>,
}

#[derive(Debug, Default, PartialEq)]
struct Findings {
    /// Tail of the most recently terminated instance's log, if one was captured.
    log_tail: Option<LogTail>,
    /// Instances whose logs the kubelet had already discarded at capture time.
    log_gaps: Vec<String>,
    termination: Option<String>,
    change: Option<String>,
    /// The spec diff nearest the alert, from `diffs/index.json`.
    diff: Option<DiffFinding>,
    /// Sparkline of the crashed container's memory against its limit, if metrics were captured.
    memory: Option<String>,
    timeline_len: usize,
    timeline: Vec<String>,
}

impl Findings {
    fn from_bundle(dir: &Path) -> Result<Self> {
        let read_json = |rel: &str| -> Option<Value> {
            let bytes = std::fs::read(dir.join(rel)).ok()?;
            serde_json::from_slice(&bytes).ok()
        };

        let instances: Vec<Value> = read_json("logs/index.json")
            .and_then(|i| {
                i["containers"]
                    .as_array()?
                    .iter()
                    .find(|c| c["container"] == CONTAINER)?["instances"]
                    .as_array()
                    .cloned()
            })
            .unwrap_or_default();
        // The crash's last words live in whichever *terminated* instance ended last — in a
        // fast crash loop that is often "current", not "previous".
        let log_tail = instances
            .iter()
            .filter(|i| i["state"] == "terminated")
            .filter_map(|i| Some((i["finished_at"].as_str()?, i["file"].as_str()?, i)))
            .max_by_key(|(finished, _, _)| finished.to_string())
            .and_then(|(_, file, instance)| {
                let text = std::fs::read_to_string(dir.join(file)).ok()?;
                let mut lines: Vec<String> = text
                    .lines()
                    .filter(|l| !l.trim().is_empty())
                    .map(str::to_string)
                    .collect();
                lines.drain(..lines.len().saturating_sub(5));
                Some(LogTail {
                    file: file.to_string(),
                    lines,
                    cut_at_the_crash_end: cut_at_the_crash_end(&instance["truncated"]),
                })
            });
        let log_gaps = instances
            .iter()
            .filter(|i| i["unavailable"].is_string() && i["state"] == "terminated")
            .map(|i| {
                format!(
                    "{} instance {} — kubelet had already discarded its logs",
                    i["which"].as_str().unwrap_or("?"),
                    short_id(i["container_id"].as_str().unwrap_or("?"))
                )
            })
            .collect();

        let timeline = read_json("timeline.json")
            .and_then(|v| v.as_array().cloned())
            .unwrap_or_default();
        let mut reasons: Vec<String> = Vec::new();
        for e in &timeline {
            if let Some(r) = e["reason"].as_str().filter(|r| !r.is_empty()) {
                if reasons.last().map(String::as_str) != Some(r) {
                    reasons.push(r.to_string());
                }
            }
        }

        let pod = read_json("resources/pod.json");
        let limit = pod.as_ref().and_then(memory_limit_bytes);
        let memory = read_json("metrics/memory_working_set_bytes.json")
            .and_then(|m| memory_shape(&m, limit, read_json("metrics/index.json").as_ref()));

        Ok(Findings {
            log_tail,
            log_gaps,
            memory,
            termination: pod.as_ref().and_then(termination),
            change: read_json("changes.json").and_then(|c| latest_change(&c)),
            diff: read_json("diffs/index.json").and_then(|i| diff_finding(&i)),
            timeline_len: timeline.len(),
            timeline: reasons,
        })
    }
}

/// How to describe a `logs/index.json` `truncated` block that cost us the crash's last words,
/// or `None` when the tail's newest end survived and the last line really is the last line.
///
/// Only `cut: "oldest"` says the end nearest the crash survived. `"newest"` — what the reference
/// collector writes, because the kubelet spends the byte budget forward from the start of the
/// tail window — says it did not, and an absent or unrecognised `cut` leaves it unknown, which
/// is treated the same way: a demo that guesses here prints a line as a container's dying words
/// on a file that may not hold them (`spec/IEB-SPEC.md` §`logs/index.json`).
fn cut_at_the_crash_end(truncated: &Value) -> Option<String> {
    truncated.as_object()?;
    if truncated["cut"].as_str() == Some("oldest") {
        return None;
    }
    Some(match truncated["limit_bytes"].as_f64() {
        Some(limit) => format!("cut at the {:.0} MiB collection limit", limit / 1_048_576.0),
        None => "cut at the collection limit".to_string(),
    })
}

/// `containerd://dadf16832e48…` → `containerd://dadf16832e48`.
fn short_id(id: &str) -> &str {
    match id.find("://") {
        Some(i) => &id[..(i + 3 + 12).min(id.len())],
        None => &id[..12.min(id.len())],
    }
}

/// `OOMKilled (exit 137) at …, restartCount=N` from the pod's last terminated state.
fn termination(pod: &Value) -> Option<String> {
    let cs = pod["status"]["containerStatuses"]
        .as_array()?
        .iter()
        .find(|c| c["name"] == CONTAINER)?;
    let t = &cs["lastState"]["terminated"];
    Some(format!(
        "{} (exit {}) at {}, restartCount={}",
        t["reason"].as_str()?,
        t["exitCode"].as_i64()?,
        t["finishedAt"].as_str().unwrap_or("?"),
        cs["restartCount"].as_i64().unwrap_or(0)
    ))
}

/// `▁▂▄▆█ peak 61.2 MiB of 64 MiB limit` for the series that peaked highest, scaled to the
/// limit so "it climbed into the ceiling" is visible at a glance.
fn memory_shape(result: &Value, limit: Option<f64>, index: Option<&Value>) -> Option<String> {
    let series = result["data"]["result"].as_array()?;
    let values = |s: &Value| -> Vec<f64> {
        s["values"]
            .as_array()
            .map(|vs| {
                vs.iter()
                    .filter_map(|p| p[1].as_str()?.parse::<f64>().ok())
                    .collect()
            })
            .unwrap_or_default()
    };
    let peak_of = |v: &[f64]| v.iter().cloned().fold(0.0_f64, f64::max);
    let best = series
        .iter()
        .map(values)
        .filter(|v| !v.is_empty())
        .max_by(|a, b| peak_of(a).total_cmp(&peak_of(b)))?;
    let peak = peak_of(&best);
    let scale = limit.unwrap_or(peak).max(peak).max(1.0);
    let mib = |b: f64| b / (1024.0 * 1024.0);
    let mut out = format!("{} peak {:.1} MiB", sparkline(&best, scale), mib(peak));
    if let Some(l) = limit {
        out.push_str(&format!(" of {:.0} MiB limit", mib(l)));
    }
    if let Some(step) = index.and_then(|i| i["step_seconds"].as_u64()) {
        out.push_str(&format!(" ({} samples, {step}s step)", best.len()));
    }
    Some(out)
}

/// Eight-level sparkline, downsampled (by max) to at most 40 cells.
fn sparkline(values: &[f64], scale: f64) -> String {
    const BARS: [char; 8] = ['▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'];
    let per_cell = values.len().div_ceil(40).max(1);
    values
        .chunks(per_cell)
        .map(|c| {
            let v = c.iter().cloned().fold(0.0_f64, f64::max);
            let level = ((v / scale) * 7.0).round().clamp(0.0, 7.0) as usize;
            BARS[level]
        })
        .collect()
}

/// The app container's memory limit from the captured pod spec (`64Mi` → bytes).
fn memory_limit_bytes(pod: &Value) -> Option<f64> {
    let q = pod["spec"]["containers"]
        .as_array()?
        .iter()
        .find(|c| c["name"] == CONTAINER)?["resources"]["limits"]["memory"]
        .as_str()?;
    let (num, mult) = [
        ("Ki", 1u64 << 10),
        ("Mi", 1 << 20),
        ("Gi", 1 << 30),
        ("Ti", 1 << 40),
    ]
    .iter()
    .find_map(|(suffix, m)| q.strip_suffix(suffix).map(|n| (n, *m)))
    .unwrap_or((q, 1));
    num.parse::<f64>().ok().map(|n| n * mult as f64)
}

#[derive(Debug, PartialEq)]
struct DiffFinding {
    headline: String,
    lines: Vec<String>,
}

/// The in-range change closest before firing (else the first `ok` entry): headline with
/// revisions, timing and actor, then its summary lines.
fn diff_finding(index: &Value) -> Option<DiffFinding> {
    let entries = index["entries"].as_array()?;
    let ok = |e: &&Value| e["status"] == "ok" && e["source"] == "replicaset-history";
    let entry = entries
        .iter()
        .filter(ok)
        .filter(|e| e["in_range"] == true && e["after_firing"] != true)
        .max_by_key(|e| e["seconds_relative_to_firing"].as_i64().unwrap_or(i64::MIN))
        .or_else(|| entries.iter().find(ok))?;
    let when = match entry["seconds_relative_to_firing"].as_i64() {
        Some(s) if s <= 0 => format!("{}s before the alert", -s),
        Some(s) => format!("{s}s after the alert"),
        None => "time unknown".into(),
    };
    let who = entry["actor"]
        .as_str()
        .map(|a| format!(", by {a}"))
        .unwrap_or_default();
    Some(DiffFinding {
        headline: format!(
            "{}/{} revision {} → {}, {when}{who}",
            entry["kind"].as_str().unwrap_or("?"),
            entry["name"].as_str().unwrap_or("?"),
            entry["before"]["revision"].as_str().unwrap_or("?"),
            entry["after"]["revision"].as_str().unwrap_or("?"),
        ),
        lines: entry["summary"]
            .as_array()
            .map(|l| {
                l.iter()
                    .filter_map(|x| x.as_str().map(str::to_string))
                    .collect()
            })
            .unwrap_or_default(),
    })
}

/// The Deployment's revision plus its most recent *spec* writer, from the change indicators
/// (`status` subresource writes are controllers reporting, not someone changing things).
fn latest_change(changes: &Value) -> Option<String> {
    let dep = changes["indicators"]
        .as_array()?
        .iter()
        .find(|i| i["kind"] == "Deployment")?;
    let latest = dep["managedFields"]
        .as_array()?
        .iter()
        .filter(|m| m["time"].as_str().is_some_and(|t| !t.is_empty()))
        .filter(|m| m["subresource"] != "status")
        .max_by_key(|m| m["time"].as_str().unwrap_or_default().to_string())?;
    Some(format!(
        "Deployment/{} → revision {}, last written by {} ({}) at {}",
        dep["name"].as_str().unwrap_or("?"),
        dep["revision"].as_str().unwrap_or("?"),
        latest["manager"].as_str().unwrap_or("?"),
        latest["operation"].as_str().unwrap_or("?"),
        latest["time"].as_str().unwrap_or("?"),
    ))
}

// --- small helpers -----------------------------------------------------------------------

fn ok(msg: impl AsRef<str>) {
    println!("  ✓ {}", msg.as_ref());
}

fn manifest(scenario: Scenario, cache_warmup: &str) -> String {
    let v = json!([
        { "apiVersion": "v1", "kind": "Namespace", "metadata": { "name": DEMO_NS } },
        {
            "apiVersion": "apps/v1",
            "kind": "Deployment",
            "metadata": { "name": APP, "namespace": DEMO_NS, "labels": { "app": APP } },
            "spec": {
                "replicas": 1,
                "selector": { "matchLabels": { "app": APP } },
                "template": {
                    "metadata": { "labels": { "app": APP } },
                    "spec": {
                        "terminationGracePeriodSeconds": 1,
                        "containers": [{
                            "name": CONTAINER,
                            "image": "busybox:1.36",
                            "command": ["sh", "-c", scenario.script()],
                            // Planted credentials: the E2E asserts they never reach a bundle.
                            "env": [
                                { "name": "CACHE_WARMUP", "value": cache_warmup },
                                { "name": "DB_PASSWORD", "value": CANARY },
                                { "name": "JAVA_OPTS",
                                  "value": format!("-Xmx48m -Dspring.datasource.password={CANARY}") }
                            ],
                            "resources": {
                                "requests": { "memory": "16Mi", "cpu": "10m" },
                                "limits": { "memory": "64Mi", "cpu": "100m" }
                            }
                        }]
                    }
                }
            }
        }
    ]);
    // One JSON document (a `List`): older kubectl versions read a stream that starts with
    // `{` as pure JSON and fail on a `---` separator (seen with kubectl 1.30).
    json!({ "apiVersion": "v1", "kind": "List", "items": v }).to_string()
}

/// First Running pod name in a `PodList`.
fn running_pod(list: &Value) -> Option<String> {
    list["items"].as_array()?.iter().find_map(|p| {
        (p["status"]["phase"] == "Running" && p["metadata"]["deletionTimestamp"].is_null())
            .then(|| p["metadata"]["name"].as_str().map(str::to_string))
            .flatten()
    })
}

/// A pod whose app container has a terminated previous instance: `(name, reason)`.
fn crashed_pod(list: &Value) -> Option<(String, String)> {
    list["items"].as_array()?.iter().find_map(|p| {
        let cs = p["status"]["containerStatuses"]
            .as_array()?
            .iter()
            .find(|c| c["name"] == CONTAINER)?;
        if cs["restartCount"].as_i64().unwrap_or(0) < 1 {
            return None;
        }
        let reason = cs["lastState"]["terminated"]["reason"].as_str()?;
        Some((
            p["metadata"]["name"].as_str()?.to_string(),
            reason.to_string(),
        ))
    })
}

fn str_at(v: &Value, path: &[&str]) -> Result<String> {
    path.iter()
        .fold(v, |acc, key| &acc[*key])
        .as_str()
        .map(str::to_string)
        .ok_or_else(|| anyhow!("missing .{}", path.join(".")))
}

fn poll<T>(timeout: Duration, what: &str, mut f: impl FnMut() -> Result<Option<T>>) -> Result<T> {
    let deadline = Instant::now() + timeout;
    loop {
        if let Some(v) = f()? {
            return Ok(v);
        }
        if Instant::now() >= deadline {
            bail!("timed out after {}s waiting for {what}", timeout.as_secs());
        }
        std::thread::sleep(Duration::from_secs(2));
    }
}

struct Kubectl {
    context: Option<String>,
}

impl Kubectl {
    fn cmd(&self) -> Command {
        let mut c = Command::new("kubectl");
        if let Some(ctx) = &self.context {
            c.args(["--context", ctx]);
        }
        c
    }

    fn run(&self, args: &[&str]) -> Result<String> {
        self.run_stdin(args, &[])
    }

    fn run_stdin(&self, args: &[&str], input: &[u8]) -> Result<String> {
        let mut child = self
            .cmd()
            .args(args)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .context("failed to run kubectl — is it installed and on PATH?")?;
        child.stdin.take().unwrap().write_all(input)?;
        let out = child.wait_with_output()?;
        if !out.status.success() {
            bail!(
                "kubectl {}: {}",
                args.join(" "),
                String::from_utf8_lossy(&out.stderr).trim()
            );
        }
        Ok(String::from_utf8_lossy(&out.stdout).into_owned())
    }

    fn json(&self, args: &[&str]) -> Result<Value> {
        Ok(serde_json::from_str(&self.run(args)?)?)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn pod_list(restarts: i64, reason: Option<&str>) -> Value {
        let last = match reason {
            Some(r) => json!({ "terminated": { "reason": r, "exitCode": 137 } }),
            None => json!({}),
        };
        json!({ "items": [{
            "metadata": { "name": "checkout-abc" },
            "status": { "phase": "Running", "containerStatuses": [
                { "name": "app", "restartCount": restarts, "lastState": last }
            ]}
        }]})
    }

    #[test]
    fn crashed_pod_needs_a_terminated_previous_instance() {
        assert_eq!(crashed_pod(&pod_list(0, None)), None);
        assert_eq!(
            crashed_pod(&pod_list(1, Some("OOMKilled"))),
            Some(("checkout-abc".into(), "OOMKilled".into()))
        );
    }

    #[test]
    fn running_pod_skips_terminating() {
        let list = json!({ "items": [
            { "metadata": { "name": "old", "deletionTimestamp": "x" }, "status": { "phase": "Running" } },
            { "metadata": { "name": "new" }, "status": { "phase": "Running" } }
        ]});
        assert_eq!(running_pod(&list).as_deref(), Some("new"));
    }

    #[test]
    fn findings_read_from_an_unpacked_bundle() {
        let dir = tempfile::tempdir().unwrap();
        let d = dir.path();
        std::fs::create_dir_all(d.join("logs")).unwrap();
        std::fs::create_dir_all(d.join("resources")).unwrap();
        // Fast crash loop: current is terminated too and holds the last words; previous was
        // already garbage-collected by the kubelet.
        std::fs::write(
            d.join("logs/app-current.log"),
            "a\nb\n\nc\nd\ne\nf: FATAL boom\n",
        )
        .unwrap();
        std::fs::write(
            d.join("logs/index.json"),
            json!({ "containers": [{ "container": "app", "instances": [
                { "which": "current", "file": "logs/app-current.log", "state": "terminated",
                  "container_id": "containerd://be730d349222509b", "finished_at": "2026-09-18T01:00:05Z" },
                { "which": "previous", "file": null, "state": "terminated",
                  "container_id": "containerd://dadf16832e48d47f", "finished_at": "2026-09-18T01:00:00Z",
                  "unavailable": "unable to retrieve container logs for containerd://dadf" }
            ] }] })
            .to_string(),
        )
        .unwrap();
        std::fs::write(
            d.join("resources/pod.json"),
            json!({ "status": { "containerStatuses": [{ "name": "app", "restartCount": 2,
                "lastState": { "terminated": { "reason": "Error", "exitCode": 1,
                "finishedAt": "2026-09-18T01:00:00Z" } } }] } })
            .to_string(),
        )
        .unwrap();
        std::fs::write(
            d.join("changes.json"),
            json!({ "indicators": [{ "kind": "Deployment", "name": "checkout", "revision": "2",
                "managedFields": [
                    { "manager": "demo-deployer", "operation": "Apply", "subresource": "", "time": "2026-09-18T00:59:30Z" },
                    { "manager": "kube-controller-manager", "operation": "Update", "subresource": "status", "time": "2026-09-18T00:59:40Z" }
                ] }] })
            .to_string(),
        )
        .unwrap();
        std::fs::write(
            d.join("timeline.json"),
            json!([{ "reason": "Pulled" }, { "reason": "BackOff" }, { "reason": "BackOff" }])
                .to_string(),
        )
        .unwrap();

        let f = Findings::from_bundle(d).unwrap();
        let tail = f.log_tail.unwrap();
        assert_eq!(tail.file, "logs/app-current.log");
        assert_eq!(tail.lines, ["b", "c", "d", "e", "f: FATAL boom"]);
        // No `truncated` block on the instance: the tail fit, so these really are last words.
        assert_eq!(tail.cut_at_the_crash_end, None);
        assert_eq!(
            f.log_gaps,
            ["previous instance containerd://dadf16832e48 — kubelet had already discarded its logs"]
        );
        assert_eq!(
            f.termination.as_deref(),
            Some("Error (exit 1) at 2026-09-18T01:00:00Z, restartCount=2")
        );
        let change = f.change.unwrap();
        assert!(
            change.contains("revision 2, last written by demo-deployer (Apply)"),
            "{change}"
        );
        assert_eq!(f.timeline_len, 3);
        assert_eq!(f.timeline, ["Pulled", "BackOff"]);
    }

    /// The same blocker as `lapilli_bundle::summary`'s `a_tail_cut_at_the_crash_end_yields_no_
    /// last_line`, on the demo's own copy of the logic: a tail the collector cut at its byte
    /// bound is missing the lines nearest the crash, so the demo must not head them "last words
    /// of the crashed instance". The lines themselves are still worth printing — they are real
    /// evidence — but only under a heading that says what they are.
    #[test]
    fn a_truncated_tail_is_not_presented_as_last_words() {
        let bundle = |truncated: Value| {
            let dir = tempfile::tempdir().unwrap();
            std::fs::create_dir_all(dir.path().join("logs")).unwrap();
            std::fs::write(
                dir.path().join("logs/app-current.log"),
                "4 MiB in\nthis is NOT the last thing the container said\n",
            )
            .unwrap();
            std::fs::write(
                dir.path().join("logs/index.json"),
                json!({ "containers": [{ "container": CONTAINER, "instances": [
                    { "which": "current", "file": "logs/app-current.log", "state": "terminated",
                      "container_id": "containerd://be730d349222509b",
                      "finished_at": "2026-09-18T01:00:05Z", "truncated": truncated }
                ] }] })
                .to_string(),
            )
            .unwrap();
            dir
        };

        let cut = bundle(json!({ "limit_bytes": 4194304, "bytes": 4194304, "cut": "newest" }));
        let tail = Findings::from_bundle(cut.path())
            .unwrap()
            .log_tail
            .expect("the tail is still read — it is evidence, just not last words");
        assert_eq!(
            tail.cut_at_the_crash_end.as_deref(),
            Some("cut at the 4 MiB collection limit")
        );
        assert_eq!(tail.lines.len(), 2, "the surviving lines are still printed");

        // The other end cut: the lines nearest the crash are there, so they are last words.
        let cut = bundle(json!({ "limit_bytes": 4194304, "bytes": 4194304, "cut": "oldest" }));
        let tail = Findings::from_bundle(cut.path()).unwrap().log_tail.unwrap();
        assert_eq!(tail.cut_at_the_crash_end, None);

        // Unknown or missing `cut`: which end is gone is unknown, so it is not last words.
        for unknown in [json!({ "bytes": 4194304 }), json!({ "cut": "middle" })] {
            let cut = bundle(unknown.clone());
            let tail = Findings::from_bundle(cut.path()).unwrap().log_tail.unwrap();
            assert!(
                tail.cut_at_the_crash_end.is_some(),
                "an unrecognised truncation {unknown} must not read as last words"
            );
        }
    }

    #[test]
    fn memory_shape_scales_to_the_limit() {
        let mib = |n: f64| (n * 1048576.0).to_string();
        let result = json!({ "data": { "result": [
            { "values": [[1, mib(1.0)], [2, mib(2.0)]] },
            { "values": [[1, mib(8.0)], [2, mib(32.0)], [3, mib(64.0)]] }
        ] } });
        let index = json!({ "step_seconds": 5 });
        let shape = memory_shape(&result, Some(64.0 * 1048576.0), Some(&index)).unwrap();
        assert_eq!(
            shape,
            "▂▅█ peak 64.0 MiB of 64 MiB limit (3 samples, 5s step)"
        );
        assert_eq!(
            memory_shape(&json!({ "data": { "result": [] } }), None, None),
            None
        );
    }

    #[test]
    fn diff_finding_prefers_the_in_range_change_before_firing() {
        let index = json!({ "entries": [
            { "status": "ok", "source": "replicaset-history", "kind": "Deployment", "name": "checkout",
              "before": { "revision": "1" }, "after": { "revision": "2" },
              "in_range": true, "after_firing": false, "seconds_relative_to_firing": -94,
              "actor": "demo-deployer",
              "summary": ["containers[name=app].env[name=CACHE_WARMUP].value: lazy → eager"] },
            { "status": "ok", "source": "replicaset-history", "kind": "Deployment", "name": "checkout",
              "before": { "revision": "2" }, "after": { "revision": "3" },
              "in_range": true, "after_firing": true, "seconds_relative_to_firing": 30,
              "summary": ["rollback"] }
        ] });
        let f = diff_finding(&index).unwrap();
        assert_eq!(
            f.headline,
            "Deployment/checkout revision 1 → 2, 94s before the alert, by demo-deployer"
        );
        assert_eq!(
            f.lines,
            ["containers[name=app].env[name=CACHE_WARMUP].value: lazy → eager"]
        );
    }

    #[test]
    fn memory_limit_parses_binary_suffixes() {
        let pod = json!({ "spec": { "containers": [
            { "name": "app", "resources": { "limits": { "memory": "64Mi" } } }
        ] } });
        assert_eq!(memory_limit_bytes(&pod), Some(64.0 * 1048576.0));
    }

    #[test]
    fn manifest_is_one_list_document() {
        // A single JSON document: every kubectl version reads it (a `---` stream of JSON
        // documents is rejected by kubectl 1.30).
        let m = manifest(Scenario::Oomkill, "eager");
        let v: Value = serde_json::from_str(&m).expect("one JSON document");
        assert_eq!(v["kind"], "List");
        let kinds: Vec<&str> = v["items"]
            .as_array()
            .unwrap()
            .iter()
            .map(|i| i["kind"].as_str().unwrap())
            .collect();
        assert_eq!(kinds, ["Namespace", "Deployment"]);
        assert!(m.contains("\"value\":\"eager\""));
    }
}
