#!/usr/bin/env bash
# scripts/rotate-jwt-key.sh
#
# Helper for cs-user JWT signing key rotation. See
# docs/operations/jwt-key-rotation.md for the full runbook.
#
# What this script does:
#   1. Generates a fresh RSA-2048 keypair (PKCS#8 private + SPKI public).
#   2. Round-trip signs + verifies a self-test payload with openssl.
#   3. Prints the public key's SHA-256 DER fingerprint for cross-checking
#      against the JWKS endpoint post-rotation.
#
# What this script DOES NOT do:
#   - Touch k8s / docker secrets. That step is intentionally manual so the
#     operator stays in the loop.
#   - Restart cs-user. Manual `kubectl rollout restart`.
#   - Compute the RFC 7638 kid. cs-user owns that derivation (see
#     cs-user/internal/auth/signer.go :: kidFor) and exposes it via JWKS —
#     operators discover the new kid by curling /.well-known/jwks after the
#     restart, not by reproducing the algorithm locally.
#
# Usage:
#   bash scripts/rotate-jwt-key.sh <output-dir>
#       Generates keys into <output-dir>/{jwt-signing.pem,jwt-signing.pub.pem}.

set -euo pipefail

die() { echo "rotate-jwt-key: $*" >&2; exit 1; }

require() { command -v "$1" >/dev/null 2>&1 || die "missing required tool: $1"; }

require openssl

case "${1:-}" in
  ""|-h|--help)
    cat <<USAGE
Usage:
  $0 <output-dir>   Generate a new RSA-2048 keypair into <output-dir>.

After deploy, discover the new kid via:
  curl -s https://cs-user.internal/.well-known/jwks | jq -r '.keys[].kid'
USAGE
    exit 0
    ;;
esac

OUT_DIR="$1"
mkdir -p "$OUT_DIR"
[[ -w "$OUT_DIR" ]] || die "cannot write to: $OUT_DIR"

PUB="$OUT_DIR/jwt-signing.pub.pem"
PRIV="$OUT_DIR/jwt-signing.pem"

echo "==> generating RSA-2048 keypair (PKCS#8) into $OUT_DIR"
openssl genpkey -algorithm RSA -out "$PRIV" -pkeyopt rsa_keygen_bits:2048
openssl rsa -in "$PRIV" -pubout -out "$PUB" 2>/dev/null
chmod 600 "$PRIV"

echo "==> round-trip self-test (sign + verify with openssl)"
PAYLOAD='{"iss":"cs-user","sub":"rotation-self-test","exp":9999999999}'
SIG_FILE="$OUT_DIR/.sig.tmp"
printf '%s' "$PAYLOAD" | openssl dgst -sha256 -sign "$PRIV" > "$SIG_FILE"
printf '%s' "$PAYLOAD" | openssl dgst -sha256 -verify "$PUB" -signature "$SIG_FILE"
rm -f "$SIG_FILE"

echo "==> public key SHA-256 fingerprint (DER, for sanity cross-check)"
# OpenSSL prints the digest hex inline — no xxd / od dependency.
openssl pkey -pubin -in "$PUB" -outform DER 2>/dev/null \
  | openssl dgst -sha256

echo
echo "Next steps (manual — see docs/operations/jwt-key-rotation.md):"
echo "  1. Update the k8s Secret with $PRIV"
echo "  2. kubectl rollout restart deployment/cs-user"
echo "  3. curl /.well-known/jwks and confirm the new kid appears"
echo "  4. End-to-end token test (login + /internal/me with the new token)"
