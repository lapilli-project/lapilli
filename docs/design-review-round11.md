# Design Review — Round 11 (incident notification)

Constitution: **Loop Engineering Constitution v0.5.0**. Artifact:
[`design-notify.md`](design-notify.md) v0 → v1.

**Outcome: R1 done, design revised; not dry.** Two lenses, one round. Four BLOCKERs, all
APPLIED into v1. The implementation is next and will be reviewed against v1.

## Round 0

- **Category:** security-sensitive and product-defining. It creates a **new egress path** out
  of a cluster from a tool whose purpose is custody of evidence, and it is the feature meant
  to make the product warm enough to adopt.
- **Break-even:** cost ≈ 2 critic runs. Downside: leaking captured content into a chat
  channel, or shipping a feature that gets muted the first night and takes the product's
  credibility with it. Downside ≫ cost.
- **Sizing:** medium-high, 2 lenses × 1 round so far: security/privacy (mandatory) and the
  on-call SRE who would be paged by it.
- **Independence: not achieved (calibration only).** Both critics were Claude. One of them
  did run the repo's redaction code in a scratch crate over realistic log lines, which is a
  real experiment rather than an opinion.

## Findings

### Security lens

| # | Sev | Finding | Disposition |
|---|---|---|---|
| S1 | BLOCKER | `facts` mode carried **alert-author-chosen** strings (`rule`, `namespace`, `pod`) into Slack unescaped, so an unprivileged `PrometheusRule` author could inject `<!channel>` and fake links under Lapilli's credibility. The claim "identifiers the operator chose" was false. | **APPLY.** `rule` gets a CRD pattern; every untrusted string renders as `plain_text` with `&`, `<`, `>` escaped and a cap; the design now marks these as untrusted, and the actor as client-asserted. |
| S2 | BLOCKER | `detail: content` rested on "the same redaction that protects the bundle applies" — **which does not exist for logs**: bundles deliberately don't redact logs, and `redact_text` hardcoded non-strict, so `strict` was byte-identical to `default` on free text (reproduced). Survivors shown: a password in a panic message, an `Authorization: Basic …` dump, PII. | **APPLY, and the claim dropped.** The last log line is **never** sent, in any mode; content mode carries only the changed field and values, which the redactor does cover. Separately, the redaction bug is fixed in code: `redact_text` honors `strict`, and a secret-looking `name: value` (not only `name=value`) is redacted, including a header whose value is the next token. New tests record both the catches and the honest limits (prose with no name or separator still survives). |
| S3 | MAJOR | The endpoint was weaker than the export precedent it cited: the whole URL in a runtime-read Secret (no chart change, no restart), no redirect policy, and the loopback exception reused `plain_http_allowed`, which `http://127.0.0.1@evil.example` defeats. | **APPLY.** Host in the ConfigMap and checked per send, only the secret path in the Secret; URL handling reuses the CLI's `parse_https`/`open_https` (no userinfo, no escapes, HTTPS only, 3xx is an error, URL never printed); non-global resolved addresses refused; the HTTP exception is test-only. |
| S4 | MAJOR | The once-only guard and the rendered bundle link were read from `status`, which round 6 already established as attacker-writable: replay, suppression, or an attacker-chosen "evidence" link in the channel. Also the summary was unreconstructible — staging is deleted during packing, before the message is sent. | **APPLY.** `O_EXCL` marker file as the guard (the project's existing pattern); path and object URL recomputed from config and spec; the rendered summary is persisted next to the bundle before staging is removed. |
| S5 | MAJOR | Notifying demo captures overloaded `lapilli.dev/export: local`, the one "never leaves the cluster" flag, so `lapilli demo` on a real cluster would post to production Slack. | **APPLY.** Demo captures don't notify unless `notify.includeDemo: true`. |

### On-call / product lens

| # | Sev | Finding | Disposition |
|---|---|---|---|
| P1 | BLOCKER | One message per capture inverts Alertmanager's grouping: 50 crashing pods → 50 messages, one Alertmanager message. The channel gets muted that night. | **APPLY.** Grouping by `(rule, namespace, owner)` with a coalescing window, a group message naming counts and the first three ids, and a per-route rate cap with one suppression notice. |
| P2 | BLOCKER | The message's payoff pointed at a path inside a pod, and the `lapilli verify` line it printed couldn't be run anywhere. No public command retrieves a bundle (`cat-bundle` is hidden). | **APPLY.** Exported captures print the `s3://` verify line; otherwise two copy-pasteable lines, which means promoting `cat-bundle` to a documented command. |
| P3 | MAJOR | Facts mode as specced was a "we captured something" ping, while the three facts that change the next action — the change's revision/timing/actor, memory peak vs limit, and whether the last log survived — were gated behind `content` or missing, though all three are numbers and identifiers. | **APPLY.** All three are facts now; cluster id, firing time and the signing line are cut as padding. |
| P4 | MAJOR | The Slack rendering copied Alertmanager's own header, so a team gets two red messages per incident and learns to read neither. | **APPLY.** Companion styling, finding first, incident id carried; the docs recommend an evidence channel, not the paging channel. |
| P5 | MAJOR | One cluster-wide URL means 12 namespaces' evidence in one channel, and a team's only mute is "off". | **APPLY.** Named routes, profiles reference by name — the export pattern. |

### Not taken

- **The postmortem draft is the stronger feature** (product lens). Recorded as the roadmap's
  next item rather than folded in: notification stays the thin, grouped pointer.
- **Pre-existing holes the reviews surfaced**, tracked separately, not in this design:
  `metrics.prometheusUrl` is a profile-editor-chosen URL fetched by a client with no
  `https_only` and default redirects (an SSRF next door to this feature); `status.message` is
  unbounded and echoes attacker-chosen `spec.clusterId`; the chart's NetworkPolicy has no
  egress rules despite DESIGN promising an egress allowlist.

## Verdict

**Not dry, by design:** this was R1 of a feature whose implementation doesn't exist yet. v1 of
the design answers every BLOCKER, and the redaction bug S2 uncovered is already fixed in
code with tests. The implementation will be reviewed against v1 before it ships, and the
three pre-existing holes above are now on the list rather than in a critic's notebook.
