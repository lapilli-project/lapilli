#!/usr/bin/env bash
# KMS signing against local emulators (docs/design-kms.md "Testing"):
#   AWS: LocalStack 4.12 (the last tag that runs without an auth token), ECC_NIST_P256.
#   GCP: blackwell-systems/gcp-kms-emulator (Apache-2.0), built from a pinned commit.
# Runs the kairn-kms integration tests against both. Needs Docker.
set -euo pipefail
cd "$(dirname "$0")/../.."
LOCALSTACK=localstack/localstack:4.12@sha256:0df3a97da57de03a588c05d9b8f390f15c7033fc7c4512f94619d344bc3cd317
GCP_EMU_REPO=https://github.com/blackwell-systems/gcp-kms-emulator
GCP_EMU_COMMIT=8f63728
AWS_PORT=${AWS_PORT:-14566}
GCP_PORT=${GCP_PORT:-18085}

cleanup() { docker rm -f kairn-test-localstack kairn-test-gcpkms >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

if ! docker image inspect "kairn-test/gcp-kms-emulator:$GCP_EMU_COMMIT" >/dev/null 2>&1; then
  tmp=$(mktemp -d)
  git clone -q "$GCP_EMU_REPO" "$tmp/src"
  git -C "$tmp/src" checkout -q "$GCP_EMU_COMMIT"
  docker build -q --build-arg VARIANT=rest -t "kairn-test/gcp-kms-emulator:$GCP_EMU_COMMIT" "$tmp/src" >/dev/null
  rm -rf "$tmp"
fi
docker run -d --name kairn-test-localstack -p "$AWS_PORT:4566" -e SERVICES=kms "$LOCALSTACK" >/dev/null
docker run -d --name kairn-test-gcpkms -p "$GCP_PORT:8080" "kairn-test/gcp-kms-emulator:$GCP_EMU_COMMIT" >/dev/null
for _ in $(seq 1 90); do
  curl -sf "localhost:$AWS_PORT/_localstack/health" | grep -q '"kms": "\(available\|running\)"' && break
  sleep 2
done
for _ in $(seq 1 30); do curl -s "localhost:$GCP_PORT/v1/projects/p/locations/global/keyRings" >/dev/null && break; sleep 1; done

export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_REGION=us-east-1
export AWS_ENDPOINT_URL_KMS="http://localhost:$AWS_PORT"
AWS_KEY=$(python3 - "$AWS_PORT" <<'PY'
import json, sys, urllib.request
req = urllib.request.Request(f"http://localhost:{sys.argv[1]}/", data=json.dumps(
    {"KeySpec": "ECC_NIST_P256", "KeyUsage": "SIGN_VERIFY"}).encode(), headers={
    "Content-Type": "application/x-amz-json-1.1", "X-Amz-Target": "TrentService.CreateKey",
    "Authorization": "AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/kms/aws4_request, SignedHeaders=host, Signature=x"})
print(json.load(urllib.request.urlopen(req))["KeyMetadata"]["Arn"])
PY
)
G="http://localhost:$GCP_PORT/v1/projects/p/locations/global"
curl -sf -X POST "$G/keyRings?keyRingId=kairn" -d '{}' >/dev/null
curl -sf -X POST "$G/keyRings/kairn/cryptoKeys?cryptoKeyId=bundles" -H 'Content-Type: application/json' \
  -d '{"purpose":"ASYMMETRIC_SIGN","versionTemplate":{"algorithm":"EC_SIGN_P256_SHA256","protectionLevel":"SOFTWARE"}}' >/dev/null
export KAIRN_GCP_KMS_ENDPOINT="http://localhost:$GCP_PORT" GOOGLE_OAUTH_ACCESS_TOKEN=test
export KAIRN_TEST_AWS_KMS_KEY="$AWS_KEY"
export KAIRN_TEST_GCP_KMS_KEY="projects/p/locations/global/keyRings/kairn/cryptoKeys/bundles/cryptoKeyVersions/1"
cargo test -q -p kairn-kms --test emulators -- --nocapture
echo "kms emulators OK"
