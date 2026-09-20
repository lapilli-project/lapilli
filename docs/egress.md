# Egress: what Kairn connects out to, and why the chart ships no NetworkPolicy

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
| The cloud's **STS / token endpoint** | ambient AWS credentials | IRSA exchanges a projected token for credentials; the controller names `sts.<region>.amazonaws.com` itself | `kairn-kms/src/lib.rs` |
| The **node's credential endpoint** | EKS Pod Identity, GKE/GCE Workload Identity | `169.254.170.23` (EKS Pod Identity Agent) or `169.254.169.254` (GCE metadata). See the warning below | `object_store` credential chain |
| The **KMS endpoint** | `signing.mode=kms` | signing each manifest | `kairn-kms/src/lib.rs` |
| Each `notify.routes[].host` | `notify.routes` set | posting the incident summary | `notify.rs` |
| `metrics.prometheusUrl` | set | the `metrics` collector's range queries | `metrics.rs` |

Not needed, and worth knowing so you do not open them: the **kubelet** (container logs are proxied
through the API server, `collector.rs`), any **admission webhook** (the chart registers none), and
anything for `kairn demo` (it runs *inbound*, over `kubectl exec`).

> **Do not blanket-except the link-local range.** It is tempting, because `169.254.169.254` is the
> cloud metadata endpoint an SSRF would target — and Kairn already refuses link-local addresses at
> the application layer for every endpoint it parses (`kairn-net`). But **EKS Pod Identity and
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
audit artifact is worse than no artifact, so the template is gone and
`scripts/helm-renders.sh` now runs every render through `kubectl apply --validate=strict`, which is
the check that catches that class.

Egress policy is also usually the platform team's object rather than an application chart's: one
policy per namespace, maintained where the cluster's CIDRs and egress gateway are known.

## Three ways to do it properly

Pick by what your CNI gives you.

**1. FQDN policy, if your CNI has it.** Cilium's `CiliumNetworkPolicy` matches `toFQDNs`, and
`toEntities: [kube-apiserver]` covers the API server without you knowing its address. This is the
only option that expresses the table above directly. Calico has an equivalent
(`GlobalNetworkSet` / DNS policy in Enterprise).

**2. An egress gateway or proxy you own.** Point Kairn at an HTTP proxy or route it through an
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

### And you will not be told

If the controller cannot reach the API server, `/healthz` still answers `ok` — deliberately, so a
policy on the webhook port cannot fail the probes (`webhook.rs`). The pod stays `1/1 Running` with
no restarts while every capture stops. **There is no metric for API reachability yet**; what you
will see is captures that never appear, or `kairn_reconcile_errors_total` climbing if they do.
Until that gauge exists, test an egress policy by firing `kairn demo` and checking a bundle
actually lands — not by looking at pod status.

## A worked example (Cilium, option 1)

Covers a default install with S3 export, AWS KMS, a Slack route, and in-cluster Prometheus. Adapt
the names; do not copy the addresses.

```yaml
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata: { name: kairn-egress, namespace: kairn-system }
spec:
  endpointSelector:
    matchLabels: { app.kubernetes.io/name: kairn }
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
- [ ] verified with `kairn demo`, not with pod status
