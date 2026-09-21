# Signing with AWS KMS or GCP Cloud KMS

With `signing.mode=kms`, the controller asks a cloud KMS to sign every bundle. The private
key never enters the cluster. The bundle format doesn't change: auditors run
`lapilli verify --key lapilli.pub` exactly as with a static key. The design and its limits are
in [`design-kms.md`](design-kms.md).

## AWS KMS

1. **Create a dedicated key**, used for nothing but Lapilli:
   ```sh
   aws kms create-key --key-spec ECC_NIST_P256 --key-usage SIGN_VERIFY \
     --description "Lapilli bundle signing (cluster prod-apne2)"
   ```
   Use one key per cluster. The auditor maps `key_id` to cluster.
2. **Key policy.** Don't leave `kms:Sign` open to the whole account. The default policy's
   root statement lets any IAM principal with a `kms:Sign` permission sign, and then forge
   bundles offline. Either list principals explicitly, or add:
   ```json
   { "Sid": "OnlyLapilliSigns", "Effect": "Deny", "Principal": "*", "Action": "kms:Sign",
     "Resource": "*",
     "Condition": { "ArnNotEquals": { "aws:PrincipalArn": "<controller role ARN>" } } }
   ```
3. **The controller role**, via IRSA or EKS Pod Identity. Use
   `serviceAccount.annotations` in the chart:
   ```json
   { "Version": "2012-10-17", "Statement": [
     { "Effect": "Allow", "Action": "kms:Sign", "Resource": "<key ARN>",
       "Condition": { "StringEquals": { "kms:SigningAlgorithm": "ECDSA_SHA_256",
                                        "kms:MessageType": "DIGEST" } } },
     { "Effect": "Allow", "Action": "kms:GetPublicKey", "Resource": "<key ARN>" } ] }
   ```
   The two actions must be separate statements. The condition keys exist only on `Sign`:
   combined, the `GetPublicKey` permission would never apply.
4. **Install:**
   ```sh
   helm upgrade lapilli <chart> -n lapilli-system --reuse-values \
     --set signing.mode=kms --set signing.kms.key=arn:aws:kms:<region>:<account>:key/<id>
   ```
   Use the **key ARN**. Alias ARNs are refused: an alias can be repointed to another key.

## GCP Cloud KMS

1. **Create a key and pin its version:**
   ```sh
   gcloud kms keys create lapilli-bundles --keyring <ring> --location <loc> \
     --purpose asymmetric-signing --default-algorithm ec-sign-p256-sha256
   ```
   Lapilli signs with one **version**:
   `projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/lapilli-bundles/cryptoKeyVersions/1`.
2. **Grant** the controller's Workload Identity principal `roles/cloudkms.signer` and
   `roles/cloudkms.publicKeyViewer` on that key only.
   - Also check for `cloudkms.signer` inherited from the key ring, the project or a folder:
     it allows signing too.
3. **Enable Data Access audit logs** (`DATA_READ`) for `cloudkms.googleapis.com`. Without
   them, signatures are not logged at all.
4. **Install** with `--set signing.mode=kms --set signing.kms.key=<the version>`.

## Auditors: getting the key

Ask the KMS itself (the trust anchor), with read access to the public key:

```sh
lapilli key fetch --kms <key ARN or version>     # writes lapilli.pub, prints key_id
lapilli verify bundle.ieb --key lapilli.pub --cluster <c> --incident <i>
```

The controller logs the `key_id` it pinned at startup, and records it with each capture in
`status.seal.keyId`. Status is informational: anyone allowed to patch it can write
anything there.

## Matching signatures to the audit log

Each capture records `status.seal.manifestSha256` and, on AWS, `status.seal.requestId`, and
the controller logs both.

The control that catches forgery is the **reverse match**: every `Sign` event for the key in
CloudTrail or Cloud Audit Logs should correspond to a bundle the controller recorded. An
unmatched event means someone else signed with the key.

## When KMS is unavailable

Captures still collect. They wait in `Sealing` with `status.seal.reason`
(`signing-unavailable`, `signing-denied`, …) and retry with backoff, from 30 s up to 1 h,
24 attempts.

- **No partial output.** No bundle is written until it is signed with the pinned key.
- **Restarts.** A restart resumes from the collected data.
- **After the last attempt.** The capture is `Failed` and the data is kept. Retry with:
  ```sh
  kubectl -n lapilli-system annotate incidentcapture <name> lapilli.dev/retry-seal="$(date +%s)" --overwrite
  ```
- **Disk.** Waiting captures use PVC space. Size `persistence.size` for your longest
  plausible outage.

## Rotating the key

AWS doesn't rotate asymmetric keys automatically. To rotate on either cloud:

1. Create the new key or version, and grant it.
2. **Keep the old public key** — but you no longer have to remember to: see
   [The archived public key](#the-archived-public-key) below. Old bundles verify only with it, and
   once the old key is disabled the KMS will not hand it to you again.
3. `helm upgrade --set signing.kms.key=<new>`. The controller pins the new key at startup.

## The archived public key

Every time the controller pins a signing key it writes the public half to
`<bundle path>/keys/<key_id>.pub`, and the exporter copies it to
`<prefix>/<cluster>/keys/<key_id>.pub` beside the bundles it signed — once per key per
destination. The bucket copy is the one that matters: the PVC dies with the cluster, and "evidence
outlives the cluster that produced it" has to include the means to verify it.

**It is not a trust anchor, and Lapilli will not use it as one.** `lapilli verify --key` takes the key
*you* chose; nothing reads the archive automatically. Anyone who can write the bucket could replace
a bundle and a key together, and a verification that trusted the neighbouring key would happily
confirm the forgery.

What the archive is for is this: once you know **which key id you expect**, you can verify a bundle
whose key no longer exists anywhere else. Lapilli records that key id in three places that are not
the bucket — the signed manifest, `status.seal.keyId` on the capture, and the controller's startup
log. Take it from one of those, or from your own audit record, and then:

```console
$ lapilli verify s3://evidence/prod/prod-apne2/<incident>.ieb \
    --key ./keys/<the key id you expect>.pub --cluster prod-apne2 --incident <incident>
```

Because the file is named by its own key id — the SHA-256 of the SPKI DER — the name and the
content check each other, and `lapilli verify` **refuses** a file whose name says one key id and
whose bytes are another. That catches a swapped archive; it does not, and cannot, catch an attacker
who rewrote the bundle, the key and your record of the key id together.

## Rolling back to a release without KMS signing

A controller older than KMS support doesn't know the `Sealing` phase. It would collect such
captures again and seal them with the profile's settings. Before rolling back, wait until
this shows nothing:

```sh
kubectl -n lapilli-system get incidentcaptures -o jsonpath='{range .items[?(@.status.phase=="Sealing")]}{.metadata.name}{"\n"}{end}'
```
