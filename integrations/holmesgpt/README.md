# Lapilli for HolmesGPT

HolmesGPT investigates an alert; Lapilli captured the evidence for that alert seconds after it
fired — the point-in-time object body, the rollout diff, the timeline — sealed in a bundle the
cluster was about to lose. This directory connects the two.

## The MCP server (recommended)

Enable the server in the Lapilli chart. It runs as a second container in the controller pod,
serving the bundle volume read-only over streamable HTTP with a bearer token:

```sh
helm upgrade lapilli charts/lapilli -n lapilli-system --reuse-values --set mcp.enabled=true
kubectl -n lapilli-system get secret lapilli-mcp-token -o jsonpath='{.data.token}' | base64 -d
```

Then point Holmes at it — `mcp_servers.yaml` here is the snippet, for the Helm values or a
local `~/.holmes/config.yaml`:

```yaml
mcp_servers:
  lapilli:
    description: "Lapilli incident evidence bundles: find by alert, verify, read the object body and rollout diff"
    url: "http://lapilli-mcp.lapilli-system.svc:8082/mcp"
    transport: streamable-http
    config:
      headers:
        Authorization: "Bearer {{ env.LAPILLI_MCP_TOKEN }}"
```

The tools an investigation uses, in the order the server's own instructions suggest:

| tool | what the agent gets |
|---|---|
| `find_bundles` | by the alert's `namespace`, `pod` (or `checkout-*`), `rule`, `since`/`until` — the incident id, capture window, target and path |
| `verify` | the `verify-result/v1` document; read `verdict` first |
| `read_file` | `resources/pod.json` (env, args, limits, probes, annotations, owner chain at the time), `diffs/index.json` and each diff, `timeline.json` — redacted at capture |
| `summary` | the headline facts: termination, restarts, the change, memory peak |
| `postmortem` | a Markdown draft whose every line came from the bundle |

Log files are served only when the chart sets `mcp.allowLogs: true`, and come back flagged
`untrusted: true`: a container's log is text the workload wrote.

## The bash toolset (no MCP client)

`toolset-lapilli.yaml` wraps the `lapilli` CLI for a Holmes install that runs it on a host with
the bundles present (pulled with `kubectl exec … lapilli cat-bundle`). Same information, fewer
guarantees: the LLM fills a shell template rather than typed arguments.

## Status

The MCP server and its five tools are exercised in Lapilli's release gate against a kind
cluster with the reference MCP client (`test/e2e/mcp.sh`, `test/mcp/check.sh`). Loading this
configuration into a running HolmesGPT has **not** been verified by the Lapilli project yet;
that is the conversation this integration exists to start.
