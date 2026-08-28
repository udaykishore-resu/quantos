#!/usr/bin/env bash
#
# verify-release.sh — check a release before deploying it.
#
# Usage:
#   scripts/verify-release.sh v1.4.2
#   REGISTRY=ghcr.io/acme/quantos scripts/verify-release.sh v1.4.2
#
# Environment:
#   REGISTRY        image repository prefix (default ghcr.io/udaykishoreresu/quantos)
#   REPO            GitHub owner/repo for the cosign identity (default udaykishoreresu/quantos)
#   SKIP_SBOM       set to 1 to skip the SBOM attestation check
#
# For each of the nine service images this checks that:
#   1. the tag resolves to a manifest, and both linux/amd64 and linux/arm64 are in it
#   2. the digest carries a valid cosign signature from this repository's
#      release workflow, verified against the transparency log
#   3. an SPDX SBOM attestation is attached
#
# Signatures are verified against the *digest*, never the tag. A tag is a name
# that can be repointed; verifying one proves nothing about what is running.
#
# The identity check is the part that matters. `cosign verify` without
# --certificate-identity-regexp will accept a signature from anyone with a
# Sigstore identity, which is everyone.
set -euo pipefail

VERSION="${1:-}"
[ -n "$VERSION" ] || { sed -n '2,26p' "$0"; exit 2; }

REPO="${REPO:-udaykishoreresu/quantos}"
REGISTRY="${REGISTRY:-ghcr.io/${REPO}}"
SKIP_SBOM="${SKIP_SBOM:-0}"

SERVICES=(
  api-gateway
  market-service
  signal-service
  risk-service
  portfolio-service
  news-service
  evaluation-service
  alert-service
  backtest-service
)

# The workflow that is permitted to have signed these images. A signature from
# any other workflow, in any other repository, is a signature from someone who
# is not the release process.
IDENTITY="https://github.com/${REPO}/.github/workflows/release.yml@refs/tags/${VERSION}"
OIDC_ISSUER="https://token.actions.githubusercontent.com"

pass=0
fail=0

log() { printf '\033[36m==>\033[0m %s\n' "$*"; }
ok()  { printf '\033[32m  ok\033[0m %s\n' "$*"; pass=$((pass + 1)); }
bad() { printf '\033[31mfail\033[0m %s\n' "$*" >&2; fail=$((fail + 1)); }
die() { printf '\033[31mfail\033[0m %s\n' "$*" >&2; exit 1; }

command -v cosign >/dev/null || die "cosign is not installed — https://docs.sigstore.dev/cosign/installation/"
command -v docker >/dev/null || die "docker is not installed (needed for manifest inspection)"

log "verifying ${VERSION} in ${REGISTRY}"
log "expected signer: ${IDENTITY}"
echo

for svc in "${SERVICES[@]}"; do
  image="${REGISTRY}/${svc}:${VERSION}"

  # -- 1. the tag resolves, and the platforms are there ---------------------

  if ! manifest=$(docker manifest inspect "$image" 2>/dev/null); then
    bad "${svc}: ${VERSION} does not resolve"
    continue
  fi

  digest=$(docker manifest inspect --verbose "$image" 2>/dev/null \
    | sed -n 's/.*"digest": "\(sha256:[a-f0-9]*\)".*/\1/p' | head -1)
  if [ -z "$digest" ]; then
    bad "${svc}: could not read a digest"
    continue
  fi

  for platform in amd64 arm64; do
    if echo "$manifest" | grep -q "\"architecture\": \"${platform}\""; then
      ok "${svc}: linux/${platform} present"
    else
      bad "${svc}: linux/${platform} missing from the manifest list"
    fi
  done

  # -- 2. signature ----------------------------------------------------------

  # By digest, not by tag, and with both identity constraints. cosign verify
  # without them succeeds against a signature from any Sigstore identity.
  if cosign verify \
      --certificate-identity "$IDENTITY" \
      --certificate-oidc-issuer "$OIDC_ISSUER" \
      "${REGISTRY}/${svc}@${digest}" > /dev/null 2>&1; then
    ok "${svc}: signature valid (${digest:0:19}…)"
  else
    bad "${svc}: signature verification failed for ${digest}"
    continue
  fi

  # -- 3. SBOM ---------------------------------------------------------------

  if [ "$SKIP_SBOM" != "1" ]; then
    if cosign verify-attestation \
        --type spdxjson \
        --certificate-identity "$IDENTITY" \
        --certificate-oidc-issuer "$OIDC_ISSUER" \
        "${REGISTRY}/${svc}@${digest}" > /dev/null 2>&1; then
      ok "${svc}: SBOM attestation present"
    else
      bad "${svc}: no valid SPDX SBOM attestation"
    fi
  fi
done

# ------------------------------------------------------------ helm chart -----

CHART_REF="oci://${REGISTRY%/*}/charts/quantos"
if command -v helm >/dev/null 2>&1; then
  log "checking the chart at ${CHART_REF}"
  if helm show chart "$CHART_REF" --version "${VERSION#v}" > /dev/null 2>&1; then
    ok "chart ${VERSION#v} published"
  else
    bad "chart ${VERSION#v} is not published"
  fi
else
  printf '\033[33m warn\033[0m helm not installed; skipping the chart check\n' >&2
fi

# --------------------------------------------------------------- result ------

echo
if [ "$fail" -gt 0 ]; then
  printf '\033[31m%d checks failed\033[0m (%d passed)\n' "$fail" "$pass" >&2
  echo "Do not deploy this release. An unsigned or unattributable image is one" >&2
  echo "nobody can say the provenance of, which is the same problem ADR-003" >&2
  echo "solves for signals." >&2
  exit 1
fi
printf '\033[32mall %d checks passed; %s is safe to deploy\033[0m\n' "$pass" "$VERSION"
