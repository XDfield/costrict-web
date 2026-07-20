# cs-user JWT Signing Key Rotation Runbook

## Scope

This runbook covers rotating the RS256 private key cs-user uses to self-sign
JWTs (Phase A3 / A7). It applies once cs-user is the JWT issuer (JWT_SIGN_MODE
`dual` or `single` on the @server side). Pre-cutover (Casdoor still signing)
this procedure is a no-op.

cs-user reads the signing key from a single on-disk PEM file referenced by
`CS_USER_JWT_SIGNING_KEY_PATH`. The corresponding public key is published via
the JWKS endpoint at `GET /.well-known/jwks` with the key's `kid` in RFC 7638
SHA-256 thumbprint form. The @server JWKSProvider caches JWKS for a short
TTL; verification picks up the new `kid` automatically once the cache expires.

**Key property**: rotation is **single-service** (cs-user only). @server does
NOT need a redeploy — it discovers the new public key via JWKS polling.

## When to rotate

- Scheduled: every 90 days (recommended; RS256 keys are cheap to rotate).
- Unscheduled: suspected key compromise, departing team member who had key
  access, any security incident touching the k8s secret store.
- After permission/claim-scope expansion that warrants invalidating all
  sessions (rare — prefer Phase C1 platform-admin reissue-token flows).

## Prerequisites

- Operator shell on a host with `openssl` available.
- Write access to the k8s secret / docker secret that materializes
  `CS_USER_JWT_SIGNING_KEY_PATH` (typically `/etc/cs-user/jwt-signing.pem`
  inside the container).
- `kubectl` (or docker secret CLI) configured against the target cluster.
- Pager / on-call aware if rotating during business hours (sessions issued
  in the last 5 min will need to re-auth).

## Helper script

`scripts/rotate-jwt-key.sh` automates steps 1–4 below (generation + on-host
verification). It does NOT touch k8s secrets — that step is intentionally
manual so the operator stays in the loop.

```bash
bash scripts/rotate-jwt-key.sh /tmp/cs-user-jwt-$(date +%Y%m%d)
```

Outputs `/tmp/cs-user-jwt-<DATE>/jwt-signing.pem` (PKCS#8) +
`jwt-signing.pub.pem` (SubjectPublicKeyInfo) + a `kid` preview for sanity
checking against the JWKS endpoint post-rotation.

## Procedure

### 1. Generate the new keypair

```bash
mkdir -p /tmp/cs-user-jwt-rotation
cd /tmp/cs-user-jwt-rotation

# Private key — PKCS#8 ("PRIVATE KEY"), 2048-bit RSA. cs-user's signer
# accepts PKCS#1 ("RSA PRIVATE KEY") too; PKCS#8 is preferred because it
# is provider-neutral (works for future Ed25519 / ECDSA migrations).
openssl genpkey -algorithm RSA -out jwt-signing.pem -pkeyopt rsa_keygen_bits:2048

# Public key in SubjectPublicKeyInfo form (the format JWKS expects).
openssl rsa -in jwt-signing.pem -pubout -out jwt-signing.pub.pem

# Lock down perms — file is a long-lived signing credential.
chmod 600 jwt-signing.pem
```

### 2. Sanity-check the new keypair before any deploy

```bash
# Round-trip sign + verify with openssl itself (independent of cs-user).
PAYLOAD='{"iss":"cs-user","sub":"rotation-self-test","exp":9999999999}'
echo -n "$PAYLOAD" | openssl dgst -sha256 -sign jwt-signing.pem > sig.bin
openssl dgst -sha256 -verify jwt-signing.pub.pem -signature sig.bin <(echo -n "$PAYLOAD")
# Expected: Verified OK
```

### 3. Update the k8s secret / docker secret

```bash
# k8s example — adjust Secret name + key to match the deployment manifest.
kubectl -n costrict create secret generic cs-user-jwt-signing \
  --from-file=jwt-signing.pem=./jwt-signing.pem \
  --dry-run=client -o yaml | kubectl apply -f -
```

For docker swarm:

```bash
echo "$SECRET_ID_NEW=$(docker secret create cs-user-jwt-signing-$(date +%s) jwt-signing.pem)"
```

### 4. Roll cs-user

```bash
kubectl -n costrict rollout restart deployment/cs-user
kubectl -n costrict rollout status  deployment/cs-user
```

cs-user reads `CS_USER_JWT_SIGNING_KEY_PATH` at startup; the new key is live
the moment the new pod finishes its readiness probe. The old key file is
superseded — keep it on disk for one TTL cycle (default 1h) so already-issued
tokens can finish expiring on the @server verification side, then delete.

### 5. Verify the JWKS endpoint serves the new `kid`

```bash
# Discover the new kid from the JWKS endpoint. cs-user computes the RFC 7638
# thumbprint server-side (cs-user/internal/auth/signer.go :: kidFor) and
# publishes it; there is no need to reproduce the algorithm locally.
curl -s https://cs-user.internal/.well-known/jwks | jq -r '.keys[].kid'
```

The kid is the SHA-256 thumbprint of the new public key — `grep` for it on
subsequent rotations to confirm cs-user picked up the new key. The @server
JWKSProvider caches for ~5 min, so the new `kid` shows up automatically; no
@server redeploy is needed.

### 6. Verify end-to-end token flow

```bash
# Trigger a reissue-token as a real user (login flow) or via the internal
# RPC, then verify the new token validates on @server.
TOKEN=$(curl -s -X POST https://cs-user.internal/api/internal/users/reissue-token \
  -H "X-Internal-Token: $INTERNAL_TOKEN" -d '{"subject":"<user-sub>"}' | jq -r .token)

# Hit a @server endpoint requiring auth.
curl -s https://api.costrict/internal/me -H "Authorization: Bearer $TOKEN"
# Expected: 200 with the user's profile.
```

### 7. Archive + clean up

- Keep the old private key in your secret-archive vault for 30 days for
  forensic / token-decoding purposes, then destroy.
- Wipe `/tmp/cs-user-jwt-rotation/` (shred if available: `shred -u *.pem`).

## Rollback

If the new key is bad (signer refuses to load it, JWKS endpoint 5xx, etc.):

```bash
# 1. Re-apply the OLD k8s secret.
kubectl -n costrict create secret generic cs-user-jwt-signing \
  --from-file=jwt-signing.pem=./jwt-signing.pem.previous \
  --dry-run=client -o yaml | kubectl apply -f -

# 2. Roll cs-user again.
kubectl -n costrict rollout restart deployment/cs-user
```

Tokens signed by the new (broken) key in the brief window are
unverifiable — affected users must re-authenticate. There is no silent
fallback; this is by design (any token cs-user signs is the canonical
identity, a broken signer cannot honor any prior identity claim).

## Validation checklist

- [ ] New private key on disk, chmod 600.
- [ ] Round-trip sign+verify with openssl succeeded.
- [ ] k8s Secret updated (check `kubectl get secret cs-user-jwt-signing -o yaml | kubectl ...`).
- [ ] cs-user rollout complete + ready.
- [ ] JWKS endpoint serves new `kid`.
- [ ] End-to-end token flow returns 200 from @server.
- [ ] Old private key archived + /tmp wiped.

## Failure modes

| Symptom | Likely cause | Fix |
|---|---|---|
| cs-user pod CrashLoopBackOff | New PEM malformed / wrong format | Re-run step 1, verify with `openssl rsa -in jwt-signing.pem -noout`; rollback |
| JWKS endpoint still serves old `kid` | cs-user didn't restart, or `CS_USER_JWT_SIGNING_KEY_PATH` env still points at old file | Verify pod env + that the new pod's volume mount reflects the new Secret |
| @server returns 401 on new tokens | JWKSProvider cache hasn't expired yet | Wait 5 min, or restart @server to flush cache |
| Old `kid` disappears immediately | cs-user only publishes the currently-loaded key (no historical key retention) | This is expected; pre-rotation tokens must expire or be reissued before rotation completes |
