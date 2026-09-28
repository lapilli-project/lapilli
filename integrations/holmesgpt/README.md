# Lapilli for HolmesGPT

One worked example of a consumer. Lapilli's own interface to its evidence is `lapilli mcp`
(`docs/design-distribution-path.md`); it speaks the Model Context Protocol, so any client can
ask a bundle what it holds — Claude Code, Cursor, a script, or HolmesGPT. This directory shows
the HolmesGPT wiring because it is a common one, not because Lapilli is built for it.

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

## Where the data goes

Worth settling before the first investigation, because the wiring above does not decide it and
Lapilli cannot see it: **whatever Holmes reads, its model sees.**

Holmes runs the LLM it is configured with. Point it at a **hosted** model — Anthropic, OpenAI,
Azure, Bedrock, Vertex — and every tool result above is sent to that provider's API as prompt
text: `resources/**` (the pod's env, args, image, limits, annotations, redacted at capture),
`diffs/**` (changed field paths and their values), the timeline's event messages, and, if the
chart sets `mcp.allowLogs: true`, unredacted container log lines. Point it at a model you **host**
— vLLM, Ollama, an in-cluster inference service — and none of it leaves the cluster.

This is a property of Holmes' deployment, not of the MCP server: the request looks identical
either way, and the bundle leaves through an *inbound* read, so no egress policy on the Lapilli
pod can bound it (`docs/egress.md`, "Data that leaves by being read"). The controls that do apply
are `mcp.networkPolicy.from`, which decides that Holmes may read at all, and `mcp.allowLogs`,
which decides whether the one unredacted file in a bundle is part of what it reads. Both are the
Lapilli chart's. `docs/data-handling.md` has the full picture.

## The bash toolset (no MCP client)

`toolset-lapilli.yaml` wraps the `lapilli` CLI for a Holmes install that runs it on a host with
the bundles present (pulled with `kubectl exec … lapilli cat-bundle`). Same information, fewer
guarantees: the LLM fills a shell template rather than typed arguments.

## Status

The MCP server and its five tools are exercised in Lapilli's release gate against a kind
cluster with the reference MCP client (`test/e2e/mcp.sh`, `test/mcp/check.sh`). Loading this
configuration into a running HolmesGPT has **not** been verified by the Lapilli project yet.
