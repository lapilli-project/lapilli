//! Collectors gather evidence into a staging directory. v0.1 ships the log-tail collector,
//! which grabs the **last-terminated** container instance (`previous=true`) — the timing-
//! sensitive "money" evidence that's gone by the time a human looks (DESIGN §4).
//!
//! Collectors are failure-isolated: an error is recorded and the collector is simply absent
//! from `collectors_run` (driving the PARTIAL coverage verdict), never blocking the seal.

use std::path::Path;

use k8s_openapi::api::core::v1::Pod;
use kube::api::LogParams;
use kube::{Api, Client};

use crate::crd::TargetRef;

/// Result of running the collector set over a staging dir.
pub struct CollectOutcome {
    pub run: Vec<String>,
    pub intended: Vec<String>,
}

/// Run the requested collectors into `stage_dir`. Currently only "logs" is implemented;
/// unknown collectors are counted as intended-but-not-run (→ PARTIAL).
pub async fn collect_all(
    client: &Client,
    target: &TargetRef,
    collectors: &[String],
    stage_dir: &Path,
) -> CollectOutcome {
    let mut run = Vec::new();
    for name in collectors {
        let ok = match name.as_str() {
            "logs" => collect_logs(client, target, stage_dir).await.is_ok(),
            _ => false, // unknown/not-yet-implemented collector → not run
        };
        if ok {
            run.push(name.clone());
        }
    }
    CollectOutcome {
        run,
        intended: collectors.to_vec(),
    }
}

/// Collect current + previous-instance log tails for the target pod's containers into
/// `stage_dir/logs/`. Best-effort per container; a container with no previous instance
/// simply has no `*-previous.log` file.
async fn collect_logs(client: &Client, target: &TargetRef, stage_dir: &Path) -> anyhow::Result<()> {
    let pods: Api<Pod> = Api::namespaced(client.clone(), &target.namespace);
    let pod = pods.get(&target.pod).await?;

    // Determine which containers to collect.
    let containers: Vec<String> = if let Some(c) = &target.container {
        vec![c.clone()]
    } else {
        pod.spec
            .as_ref()
            .map(|s| s.containers.iter().map(|c| c.name.clone()).collect())
            .unwrap_or_default()
    };

    let logs_dir = stage_dir.join("logs");
    std::fs::create_dir_all(&logs_dir)?;

    for container in containers {
        // Current instance (best-effort).
        if let Ok(cur) = pods
            .logs(
                &target.pod,
                &LogParams {
                    container: Some(container.clone()),
                    previous: false,
                    tail_lines: Some(2000),
                    ..Default::default()
                },
            )
            .await
        {
            std::fs::write(logs_dir.join(format!("{container}-current.log")), cur)?;
        }

        // Previous (last-terminated) instance — the timing-sensitive win. Absent if the
        // container has never restarted, so treat an error as "nothing to collect".
        if let Ok(prev) = pods
            .logs(
                &target.pod,
                &LogParams {
                    container: Some(container.clone()),
                    previous: true,
                    tail_lines: Some(2000),
                    ..Default::default()
                },
            )
            .await
        {
            std::fs::write(logs_dir.join(format!("{container}-previous.log")), prev)?;
        }
    }

    Ok(())
}
