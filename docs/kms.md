# Signing with AWS KMS or GCP Cloud KMS

With `signing.mode=kms`, the controller asks a cloud KMS to sign every bundle. The private
key never enters the cluster. The bundle format doesn't change: auditors run
`kairn verify --key kairn.pub` exactly as with a static key. The design and its limits are
in [`design-kms.md`](design-kms.md).

## AWS KMS

1. **Create a dedicated key**, used for nothing but Kairn:
   ```sh
   aws kms create-key --key-spec ECC_NIST_P256 --key-usage SIGN_VERIFY \
     --description "Kairn bundle signing (cluster prod-apne2)"
   ```
   Use one key per cluster. The auditor maps `key_id` to cluster.
2. **Key policy.** Don't leave `kms:Sign` open to the whole account. The default policy's
   root statement lets any IAM principal with a `kms:Sign` permission sign, and then forge
   bundles offline. Either list principals explicitly, or add:
   ```json
   { "Sid": "OnlyKairnSigns", "Effect": "Deny", "Principal": "*", "Action": "kms:Sign",
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
   helm upgrade kairn <chart> -n kairn-system --reuse-values \
     --set signing.mode=kms --set signing.kms.key=arn:aws:kms:<region>:<account>:key/<id>
   ```
   Use the **key ARN**. Alias ARNs are refused: an alias can be repointed to another key.

## GCP Cloud KMS

1. **Create a key and pin its version:**
   ```sh
   gcloud kms keys create kairn-bundles --keyring <ring> --location <loc> \
     --purpose asymmetric-signing --default-algorithm ec-sign-p256-sha256
   ```
   Kairn signs with one **version**:
   `projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/kairn-bundles/cryptoKeyVersions/1`.
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
kairn key fetch --kms <key ARN or version>     # writes kairn.pub, prints key_id
kairn verify bundle.ieb --key kairn.pub --cluster <c> --incident <i>
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
  kubectl -n kairn-system annotate incidentcapture <name> kairn.dev/retry-seal="$(date +%s)" --overwrite
  ```
- **Disk.** Waiting captures use PVC space. Size `persistence.size` for your longest
  plausible outage.

## Rotating the key

AWS doesn't rotate asymmetric keys automatically. To rotate on either cloud:

1. Create the new key or version, and grant it.
2. **Keep the old public key**, with `kairn key fetch` or the controller log, before
   disabling the old key. Old bundles verify only with it.
3. `helm upgrade --set signing.kms.key=<new>`. The controller pins the new key at startup.

## Rolling back to a release without KMS signing

A controller older than KMS support doesn't know the `Sealing` phase. It would collect such
captures again and seal them with the profile's settings. Before rolling back, wait until
this shows nothing:

```sh
kubectl -n kairn-system get incidentcaptures -o jsonpath='{range .items[?(@.status.phase=="Sealing")]}{.metadata.name}{"\n"}{end}'
```
