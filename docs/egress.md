# Egress: what Lapilli connects out to, and why the chart ships no NetworkPolicy

`DESIGN.md` §7 promises an "egress allowlist pinned to the export endpoint, so the sanctioned
export path can't become an exfil channel". This document is that allowlist. **The chart does not
ship a NetworkPolicy for it**, and the reason is not laziness — see
[Why not a template](#why-not-a-template).

## What the controller connects to

Everything below is read from the code, not from intent. Nothing else in the controller opens an
outbound connection.

| Peer | When | Why | Where |
|---|---|---|---|
| **Kubernetes API server** | always | collection, status patches, Events, the CaptureProfile read. Without it **nothing works** — this is not a degradable feature | `collector.rs`, `reconcile.rs` |
| **kube-dns** | always | every peer below is configured by name | — |
| Each `export.destinations` endpoint | `export.destinations` set | uploading sealed bundles (S3, GCS, or an S3-compatible host) | `export.rs` |
| The cloud's **STS / token endpoint** | ambient AWS credentials | IRSA exchanges a projected token for credentials; the controller names `sts.<region>.amazonaws.com` itself | `lapilli-kms/src/lib.rs` |
| The **node's credential endpoint** | EKS Pod Identity, GKE/GCE Workload Identity | `169.254.170.23` (EKS Pod Identity Agent) or `169.254.169.254` (GCE metadata). See the warning below | `object_store` credential chain |
| The **KMS endpoint** | `signing.mode=kms` | signing each manifest | `lapilli-kms/src/lib.rs` |
| Each `notify.routes[].host` | `notify.routes` set | posting the incident summary | `notify.rs` |
| `metrics.prometheusUrl` | set | the `metrics` collector's range queries | `metrics.rs` |

Not needed, and worth knowing so you do not open them: the **kubelet** (container logs are proxied
through the API server, `collector.rs`), any **admission webhook** (the chart registers none), and
anything for `lapilli demo` (it runs *inbound*, over `kubectl exec`). The opt-in **`mcp`
container** (`mcp.enabled`) needs no egress rule either: it is the in-image CLI, built without the
`remote` feature, serving bundles from the shared volume to *inbound* callers on its own port
(`mcp.rs` opens no outbound connection).

> **Do not blanket-except the link-local range.** It is tempting, because `169.254.169.254` is the
> cloud metadata endpoint an SSRF would target — and Lapilli already refuses link-local addresses at
> the application layer for every endpoint it parses (`lapilli-net`). But **EKS Pod Identity and
> GKE Workload Identity get the controller's own credentials from that range.** Excepting it
> breaks exactly the credential modes `docs/kms.md` recommends. If you use static credentials in a
> Secret, excepting link-local is safe and worth doing; otherwise allow the single agent address
> your platform uses and nothing else.

## Why not a template

**NetworkPolicy v1 cannot match a DNS name.** Its peers are `podSelector`, `namespaceSelector` and
`ipBlock` — nothing else. Most of the table above is cloud endpoints whose addresses change without
notice, so the policy a chart could generate would be either wrong tomorrow or so broad it allows
nothing to be caught. That is not a gap in the chart; it is the ceiling of the API.

A chart-generated policy was written and then withdrawn, for a sharper reason than that. It spliced
the admin's list in at rule level while documenting it as a peer list, so the API server **silently
pruned** the unknown fields and stored `{}` — *allow all egress, everywhere*. It rendered, it
linted, `helm install` returned 0, and `kubectl get networkpolicy` showed an empty rule. A reviewer
reading the values file would have seen a tight allowlist; the cluster had none. A false green
audit artifact is worse than no artifact, so the template is gone, and the kind E2E
(`test/e2e/run.sh`, "every chart render survives the API server's STRICT decoding") now runs each
render through `kubectl apply --dry-run=server --validate=strict` against the cluster, which is the
check that catches that class. It lives there and **not** in `scripts/helm-renders.sh`, on
purpose: strict decoding needs the API server's OpenAPI, so it cannot run without a cluster — and a
first attempt in the render script talked to whatever `kubectl` context happened to be current,
which is the same defect it was added to catch (the script says so where the check would have
been).

Egress policy is also usually the platform team's object rather than an application chart's: one
policy per namespace, maintained where the cluster's CIDRs and egress gateway are known.

## Three ways to do it properly

Pick by what your CNI gives you.

**1. FQDN policy, if your CNI has it.** Cilium's `CiliumNetworkPolicy` matches `toFQDNs`, and
`toEntities: [kube-apiserver]` covers the API server without you knowing its address. This is the
only option that expresses the table above directly. Calico has an equivalent
(`GlobalNetworkSet` / DNS policy in Enterprise).

**2. An egress gateway or proxy you own.** Point Lapilli at an HTTP proxy or route it through an
egress node, and the NetworkPolicy becomes one `ipBlock` for that hop. The allowlist then lives in
the proxy, where names work. This is the usual answer in regulated environments.

**3. Resolved IP ranges you maintain.** AWS publishes `ip-ranges.json`, Google publishes
`cloud.json`; Slack does not publish one. Workable for S3 and KMS in one region, painful otherwise,
and it needs a job that keeps the policy current.

## The API server is the one you will get wrong

`ipBlock` matches the **post-DNAT** destination, so the Service ClusterIP is the wrong value.
`10.96.0.1:443` — the address every guide shows — is not what leaves the pod.

```console
$ kubectl -n default get endpoints kubernetes
NAME         ENDPOINTS           AGE
kubernetes   172.21.0.2:6443     4h
```

That address and that **port** are what an `ipBlock` rule needs, and on a managed cluster they can
change when the control plane is replaced. Prefer option 1 (`toEntities: kube-apiserver`) or
option 2 if you can.

### And the pod will not tell you

If the controller cannot reach the API server, `/healthz` still answers `ok` — deliberately, so a
policy on the webhook port cannot fail the probes (`webhook.rs`). The pod stays `1/1 Running` with
no restarts while every capture stops, so **pod status is the wrong place to look**: an egress
policy that cuts off the API server looks like a healthy install.

`/metrics` does say so. `lapilli_apiserver_poll_ok` goes to `0`,
`lapilli_apiserver_polls_total{result="unreachable"}` climbs, and
`lapilli_apiserver_last_success_timestamp_seconds` stops moving — docs/metrics.md has the alerts.

**Wait a minute before you believe it.** The poller runs every 30 s and the gauge holds its
previous value until the next poll returns — and a policy that DROPs makes the request *hang*
rather than fail, so it takes up to two intervals, measured at about 60 s. Reading `/metrics`
immediately after `kubectl apply` prints `1` for a controller you have just locked out:

```console
$ kubectl -n lapilli-system port-forward deploy/lapilli 18081:8081 >/dev/null &
$ until curl -sf localhost:18081/metrics >/dev/null; do sleep 1; done
$ sleep 70                                    # two poll intervals: a DROP hangs, it does not fail
$ curl -s localhost:18081/metrics | grep '^lapilli_apiserver_poll_ok '
lapilli_apiserver_poll_ok 1
$ curl -s localhost:18081/metrics | grep '^lapilli_apiserver_polls_total{result="unreachable"}'
lapilli_apiserver_polls_total{result="unreachable"} 0
```

The `result` label is worth reading rather than just the gauge, because it separates the two
mistakes an egress policy makes: `unreachable` is the packet never arriving, while `forbidden` or
`unauthorized` means the controller reached the API server fine and your problem is RBAC, not the
network.

Either way this is necessary, not sufficient — it says nothing about the bucket, the KMS endpoint
or a notification host. Confirm the whole path by firing `lapilli demo` and checking a bundle
actually lands.

## A worked example (Cilium, option 1)

Covers a default install with S3 export, AWS KMS, a Slack route, and in-cluster Prometheus. Adapt
the names; do not copy the addresses.

```yaml
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata: { name: lapilli-egress, namespace: lapilli-system }
spec:
  endpointSelector:
    matchLabels: { app.kubernetes.io/name: lapilli }
  egress:
    - toEntities: [kube-apiserver]
    - toEndpoints:
        - matchLabels:
            io.kubernetes.pod.namespace: kube-system
            k8s-app: kube-dns
      toPorts:
        - ports: [{ port: "53", protocol: ANY }]
          rules: { dns: [{ matchPattern: "*" }] }
    - toFQDNs:
        - matchName: s3.ap-northeast-2.amazonaws.com
        - matchName: kms.ap-northeast-2.amazonaws.com
        - matchName: sts.ap-northeast-2.amazonaws.com   # IRSA only
        - matchName: hooks.slack.com
      toPorts: [{ ports: [{ port: "443", protocol: TCP }] }]
    - toEndpoints:
        - matchLabels:
            io.kubernetes.pod.namespace: monitoring
            app.kubernetes.io/name: prometheus
      toPorts: [{ ports: [{ port: "9090", protocol: TCP }] }]
```

With EKS Pod Identity instead of IRSA, drop the `sts` name and add
`- toCIDRSet: [{ cidr: 169.254.170.23/32 }]` on port 80.

## Checklist

- [ ] API server reachable — by entity, or by the **endpoint** address and port, not the ClusterIP
- [ ] kube-dns reachable (and NodeLocal DNSCache's `169.254.20.10`, if your cluster runs it)
- [ ] every `export.destinations[].url` host, on 443
- [ ] the KMS host, if `signing.mode=kms`
- [ ] your credential source: STS for IRSA, or the node agent address for Pod Identity / Workload
      Identity — and **not** blanket-excepted
- [ ] every `notify.routes[].host`, on 443
- [ ] `metrics.prometheusUrl`, if set
- [ ] IPv6 peers too, on a dual-stack cluster: `0.0.0.0/0` does not imply `::/0`
- [ ] `lapilli_apiserver_poll_ok` still `1` **a minute after** the policy is applied, and
      then verified end to end with `lapilli demo` — never with pod status, which stays green
      either way
