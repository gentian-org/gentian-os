#!/usr/bin/env bash
# =============================================================================
# scripts/bootstrap/init-openbao-transit.sh
# =============================================================================
# Bootstrap the openbao-transit instance and create the Kubernetes Secret
# that the primary OpenBao reads for its transit auto-unseal token.
#
# This script is fully idempotent:
#   - If already initialised, init is skipped.
#   - If already unsealed, unseal is skipped.
#   - Transit engine / key / policy are created only if absent.
#   - k8s Secret is created only if absent.
#
# Requires: kubectl, jq, curl
#
# Optional env vars:
#   TRANSIT_INIT_FILE  — where to save init output (default: ~/.gentian/openbao-transit-init.json)
#   TRANSIT_NAMESPACE  — k8s namespace (default: openbao)
# =============================================================================

set -euo pipefail

RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'; CYAN='\033[0;36m'; NC='\033[0m'
info()    { echo -e "${CYAN}[INFO]${NC}  $*"; }
success() { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; }

TRANSIT_INIT_FILE="${TRANSIT_INIT_FILE:-${HOME}/.gentian/openbao-transit-init.json}"
TRANSIT_NS="${TRANSIT_NAMESPACE:-openbao}"

# ─── Resolve transit address ─────────────────────────────────────────────────
# Prefer the Service's ClusterIP when this host can route to it, otherwise fall
# back to a port-forward. Curling the ClusterIP directly only works when the
# installer runs on a node (local k3s/minikube); against a remote cluster it
# blackholes and the wait loop below times out on a perfectly healthy pod.
# shellcheck source=scripts/lib/portforward.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)/portforward.sh"

info "Resolving openbao-transit address..."
if ! kubectl get svc openbao-transit -n "${TRANSIT_NS}" >/dev/null 2>&1; then
  error "Service openbao-transit not found in namespace ${TRANSIT_NS}."
  echo "  Deploy the ArgoCD application first:"
  echo "    kubectl apply -f argocd/bootstrap/openbao-transit-application.yaml"
  exit 1
fi

# ─── Wait for pod ready (before probing: port-forward needs a live endpoint) ──
info "Waiting for openbao-transit-0 to be Ready (up to 5 min)..."
kubectl wait pod -n "${TRANSIT_NS}" openbao-transit-0 \
  --for=condition=Ready --timeout=300s
success "openbao-transit-0 is Ready."

if [[ -n "${TRANSIT_ADDR:-}" ]]; then
  success "Transit address: ${TRANSIT_ADDR} (from TRANSIT_ADDR)"
elif TRANSIT_ADDR=$(gentian_service_addr openbao-transit "${TRANSIT_NS}" 8200 http); then
  success "Transit address: ${TRANSIT_ADDR}"
else
  error "Could not reach openbao-transit in namespace ${TRANSIT_NS}."
  error "  Neither the ClusterIP nor a kubectl port-forward responded on :8200."
  error "  Check: kubectl get pod,svc -n ${TRANSIT_NS} -l app.kubernetes.io/instance=openbao-transit"
  error "  Override with: TRANSIT_ADDR=http://127.0.0.1:8200 (after your own port-forward)"
  exit 1
fi

# ─── Wait for HTTP listener to bind ──────────────────────────────────────────
# kubectl Ready means the pod's readiness probe succeeded, but OpenBao's HTTP
# listener can take a few extra seconds to accept connections — especially
# right after a kubelet/kubelite restart triggered by install.sh's
# auto-recovery. Without this loop, the very next curl below races and exits
# non-zero under set -e, aborting the whole script.
info "Waiting for OpenBao HTTP listener to accept connections (up to 180s)..."
i=0
until curl -sf -o /dev/null --max-time 3 "${TRANSIT_ADDR}/v1/sys/health?standbyok=true&sealedcode=200&uninitcode=200"; do
  sleep 2; i=$((i + 2))
  if [[ $i -ge 180 ]]; then
    error "OpenBao HTTP listener at ${TRANSIT_ADDR} never accepted connections within 180s."
    exit 1
  fi
done
success "OpenBao HTTP listener responding."

# ─── Init / unseal ───────────────────────────────────────────────────────────
INIT_STATUS=$(curl -sf "${TRANSIT_ADDR}/v1/sys/init" | jq -r '.initialized')

if [[ "$INIT_STATUS" == "true" ]]; then
  success "openbao-transit already initialized."

  # Unseal BEFORE token validation — a sealed vault returns 503 for all auth
  # requests, which would falsely invalidate a perfectly good cached token and
  # delete the cache file (including the unseal key), leaving no way to recover.
  SEALED=$(curl -sf "${TRANSIT_ADDR}/v1/sys/seal-status" | jq -r '.sealed')
  if [[ "$SEALED" == "true" ]]; then
    info "Transit is sealed — unsealing..."
    if [[ -f "${TRANSIT_INIT_FILE}" ]]; then
      UNSEAL_KEY=$(jq -r '.keys_base64[0]' "${TRANSIT_INIT_FILE}")
    elif kubectl get secret openbao-transit-unseal -n "${TRANSIT_NS}" >/dev/null 2>&1; then
      # Init file is gone (e.g. after OS reinstall / /tmp wipe) but the unseal
      # key was persisted to the openbao-transit-unseal Kubernetes Secret during
      # first-time init — use it automatically so the install is non-interactive.
      UNSEAL_KEY=$(kubectl get secret openbao-transit-unseal -n "${TRANSIT_NS}" \
        -o jsonpath='{.data.unseal-key}' | base64 -d | tr -d '\n')
      info "Unseal key sourced from openbao-transit-unseal k8s Secret."
    else
      echo "  (key is read silently — characters will not appear as you type)"
      read -rsp "  Enter transit unseal key: " UNSEAL_KEY; echo ""
    fi
    curl -sf -X PUT "${TRANSIT_ADDR}/v1/sys/unseal" \
      -H "Content-Type: application/json" \
      -d "{\"key\": \"${UNSEAL_KEY}\"}" >/dev/null
    success "Transit unsealed."
  else
    success "Transit already unsealed."
  fi

  # Nothing below this point is reachable without the ROOT token, and the root
  # token only lives in TRANSIT_INIT_FILE — which defaults to /tmp and therefore
  # disappears on reboot. But everything the root token would do (enable the
  # engine, create the autounseal key, write the policy, mint the app token,
  # create the k8s Secret) is already done if openbao-transit-token exists and
  # still authenticates. In that case the run has no work left, so finish here
  # rather than prompting for a token the operator no longer has.
  #
  # This is what makes a re-run genuinely idempotent, as the header claims: the
  # previous behaviour was to stop and ask, which fails outright in CI.
  if kubectl get secret openbao-transit-token -n "${TRANSIT_NS}" >/dev/null 2>&1; then
    EXISTING_APP_TOKEN=$(kubectl get secret openbao-transit-token -n "${TRANSIT_NS}" \
      -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
    if [[ -n "${EXISTING_APP_TOKEN}" ]]; then
      APP_LOOKUP_HTTP=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -H "X-Vault-Token: ${EXISTING_APP_TOKEN}" \
        "${TRANSIT_ADDR}/v1/auth/token/lookup-self" 2>/dev/null || echo 000)
      if [[ "${APP_LOOKUP_HTTP}" == "200" ]]; then
        success "openbao-transit-token Secret already present and valid — transit bootstrap is complete."
        info "  Transit engine, autounseal key, policy and app token are all in place."
        info "  (Root token not needed: nothing left to provision.)"
        exit 0
      fi
      info "openbao-transit-token exists but its token no longer authenticates (HTTP ${APP_LOOKUP_HTTP}); re-provisioning."
    fi
  fi

  if [[ -f "${TRANSIT_INIT_FILE}" ]]; then
    TRANSIT_ROOT_TOKEN=$(jq -r '.root_token' "${TRANSIT_INIT_FILE}")
    # Validate the cached token is still accepted by this transit instance.
    # If the hostpath data was wiped or the instance was re-keyed, the cached
    # token will be stale and we must fall back to prompting the user.
    LOOKUP_HTTP=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
      -H "X-Vault-Token: ${TRANSIT_ROOT_TOKEN}" \
      "${TRANSIT_ADDR}/v1/auth/token/lookup-self" 2>/dev/null || echo 000)
    if [[ "${LOOKUP_HTTP}" != "200" ]]; then
      warn "Cached token in ${TRANSIT_INIT_FILE} rejected by transit (HTTP ${LOOKUP_HTTP}). Falling back to prompt."
      TRANSIT_ROOT_TOKEN=""
      rm -f "${TRANSIT_INIT_FILE}"
    else
      success "Cached openbao-transit token validated."
    fi
  fi
  # :- is required, not cosmetic. TRANSIT_ROOT_TOKEN is only assigned inside the
  # `[[ -f TRANSIT_INIT_FILE ]]` block above, so when that file is missing this
  # test tripped `set -u` and killed the script with a bare
  # "TRANSIT_ROOT_TOKEN: unbound variable" — before it could reach the prompt
  # that exists precisely to handle a missing init file.
  if [[ -z "${TRANSIT_ROOT_TOKEN:-}" ]]; then
    echo "  (token is read silently — characters will not appear as you type)"
    read -rsp "  Enter openbao-transit root token: " TRANSIT_ROOT_TOKEN; echo ""
    if [[ -z "${TRANSIT_ROOT_TOKEN}" ]]; then
      error "Root token must not be empty."
      exit 1
    fi
    LOOKUP_HTTP=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
      -H "X-Vault-Token: ${TRANSIT_ROOT_TOKEN}" \
      "${TRANSIT_ADDR}/v1/auth/token/lookup-self" 2>/dev/null || echo 000)
    if [[ "${LOOKUP_HTTP}" != "200" ]]; then
      error "Root token rejected by transit (HTTP ${LOOKUP_HTTP}). Check the token and retry."
      exit 1
    fi
  fi

else
  # ── Fresh initialisation ─────────────────────────────────────────────────
  info "Initializing openbao-transit (1-of-1 key shares)..."
  INIT_RESP=$(curl -sf -X PUT "${TRANSIT_ADDR}/v1/sys/init" \
    -H "Content-Type: application/json" \
    -d '{"secret_shares": 1, "secret_threshold": 1}')

  echo "$INIT_RESP" > "${TRANSIT_INIT_FILE}"
  chmod 600 "${TRANSIT_INIT_FILE}"

  TRANSIT_UNSEAL_KEY=$(echo "$INIT_RESP"  | jq -r '.keys_base64[0]')
  TRANSIT_ROOT_TOKEN=$(echo "$INIT_RESP"  | jq -r '.root_token')

  # Neither value is printed. Both are already in ${TRANSIT_INIT_FILE}, mode
  # 600 — the same reasoning as the primary's init in scripts/lib/openbao.sh:
  # a terminal or a CI log is the one place they should never sit in the
  # clear, and nothing downstream needs the raw text. Unlike the primary,
  # nothing here asks the operator to save these anywhere durable either —
  # the root token is revoked by this same script once bootstrap finishes
  # (below), and the unseal key is written to the openbao-transit-unseal k8s
  # Secret a few steps from now, which is what the transit pod itself reads
  # to unseal on every restart. That Secret, not this file, is the durable
  # copy from here on; the cluster's own recovery kit (--export-recovery-kit)
  # captures the same key as TRANSIT_UNSEAL_KEY for a break-glass rebuild.
  echo ""
  info "openbao-transit initialised. Unseal key and root token are in"
  info "  ${TRANSIT_INIT_FILE} (mode 600) — nowhere else, and briefly."

  # Unseal
  curl -sf -X PUT "${TRANSIT_ADDR}/v1/sys/unseal" \
    -H "Content-Type: application/json" \
    -d "{\"key\": \"${TRANSIT_UNSEAL_KEY}\"}" >/dev/null
  success "Transit initialized and unsealed."
fi

# ─── Enable transit secrets engine (idempotent) ───────────────────────────────
MOUNTS=$(curl -sf \
  -H "X-Vault-Token: ${TRANSIT_ROOT_TOKEN}" \
  "${TRANSIT_ADDR}/v1/sys/mounts")

if echo "$MOUNTS" | jq -e '."transit/"' >/dev/null 2>&1; then
  success "Transit secrets engine already enabled."
else
  info "Enabling transit secrets engine..."
  curl -sf -X POST \
    -H "X-Vault-Token: ${TRANSIT_ROOT_TOKEN}" \
    -H "Content-Type: application/json" \
    -d '{"type": "transit"}' \
    "${TRANSIT_ADDR}/v1/sys/mounts/transit" >/dev/null
  success "Transit secrets engine enabled."
fi

# ─── Create autounseal key (idempotent) ───────────────────────────────────────
KEY_STATUS=$(curl -s \
  -H "X-Vault-Token: ${TRANSIT_ROOT_TOKEN}" \
  "${TRANSIT_ADDR}/v1/transit/keys/autounseal" 2>/dev/null \
  | jq -r '.data.name // empty')

if [[ "$KEY_STATUS" == "autounseal" ]]; then
  success "Key transit/keys/autounseal already exists."
else
  info "Creating transit/keys/autounseal (aes256-gcm96)..."
  curl -sf -X POST \
    -H "X-Vault-Token: ${TRANSIT_ROOT_TOKEN}" \
    -H "Content-Type: application/json" \
    -d '{"type": "aes256-gcm96"}' \
    "${TRANSIT_ADDR}/v1/transit/keys/autounseal" >/dev/null
  success "Key created."
fi

# ─── Write transit-autounseal policy (idempotent) ────────────────────────────
info "Writing policy transit-autounseal..."
POLICY_HCL='path "transit/encrypt/autounseal" { capabilities = ["update"] }
path "transit/decrypt/autounseal" { capabilities = ["update"] }
path "auth/token/renew-self"      { capabilities = ["update"] }
path "auth/token/lookup-self"     { capabilities = ["read"]   }'
POLICY_JSON=$(python3 -c "import json,sys; print(json.dumps({'policy': sys.stdin.read()}))" <<< "$POLICY_HCL")
curl -sf -X PUT \
  -H "X-Vault-Token: ${TRANSIT_ROOT_TOKEN}" \
  -H "Content-Type: application/json" \
  -d "$POLICY_JSON" \
  "${TRANSIT_ADDR}/v1/sys/policies/acl/transit-autounseal" >/dev/null
success "Policy transit-autounseal written."

# ─── Create autounseal token + k8s Secret (idempotent + validating) ──────────
# Important: a Secret left over from a prior install can hold a token from a
# now-gone transit instance (transit raft state was wiped, namespace was
# recreated, etc.). Reusing such a stale token is the #1 cause of the primary
# openbao-0 crash-looping with "Error parsing Seal configuration: ... Code:
# 403 ... permission denied" on transit/encrypt/autounseal — even though the
# transit pod is healthy and the policy is correct.
#
# So: if the Secret exists, validate the token against the *current* transit
# instance. Only skip creation when lookup-self succeeds. Otherwise delete
# the stale Secret and mint a fresh token.
NEED_NEW_TOKEN=1
if kubectl get secret openbao-transit-token -n "${TRANSIT_NS}" >/dev/null 2>&1; then
  EXISTING_TOKEN=$(kubectl get secret openbao-transit-token -n "${TRANSIT_NS}" \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [[ -n "$EXISTING_TOKEN" ]]; then
    LOOKUP_CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
      -H "X-Vault-Token: ${EXISTING_TOKEN}" \
      "${TRANSIT_ADDR}/v1/auth/token/lookup-self" 2>/dev/null || echo 000)
    if [[ "$LOOKUP_CODE" == "200" ]]; then
      success "k8s Secret openbao-transit-token already exists and token validates. Skipping."
      NEED_NEW_TOKEN=0
    else
      info "Existing openbao-transit-token Secret holds a stale token (lookup-self → ${LOOKUP_CODE}); recreating."
      kubectl delete secret openbao-transit-token -n "${TRANSIT_NS}" >/dev/null 2>&1 || true
    fi
  else
    info "Existing openbao-transit-token Secret is empty; recreating."
    kubectl delete secret openbao-transit-token -n "${TRANSIT_NS}" >/dev/null 2>&1 || true
  fi
fi

if [[ "$NEED_NEW_TOKEN" == "1" ]]; then
  info "Creating periodic orphan autounseal token (period=8760h)..."
  TOKEN_RESP=$(curl -sf -X POST \
    -H "X-Vault-Token: ${TRANSIT_ROOT_TOKEN}" \
    -H "Content-Type: application/json" \
    -d '{"policies":["transit-autounseal"],"period":"8760h","orphan":true,"no_parent":true,"display_name":"openbao-primary-autounseal"}' \
    "${TRANSIT_ADDR}/v1/auth/token/create-orphan")

  AUTOUNSEAL_TOKEN=$(echo "$TOKEN_RESP" | jq -r '.auth.client_token')

  if [[ -z "$AUTOUNSEAL_TOKEN" || "$AUTOUNSEAL_TOKEN" == "null" ]]; then
    error "Failed to create autounseal token: $TOKEN_RESP"
    exit 1
  fi

  info "Creating k8s Secret openbao-transit-token in namespace ${TRANSIT_NS}..."
  kubectl create secret generic openbao-transit-token \
    -n "${TRANSIT_NS}" \
    --from-literal=token="${AUTOUNSEAL_TOKEN}"
  success "k8s Secret openbao-transit-token created."
fi

# ─── Store unseal key in k8s Secret for auto-unseal postStart hook ────────────
# This secret is read by the transit pod's postStart hook (transit-values.yaml)
# so the transit instance unseals itself automatically on every pod/node restart.
# Always create/update — on fresh install it was a placeholder from bootstrap.
if [[ -z "${TRANSIT_UNSEAL_KEY:-}" ]]; then
  if [[ -f "${TRANSIT_INIT_FILE}" ]]; then
    TRANSIT_UNSEAL_KEY=$(jq -r '.keys_base64[0]' "${TRANSIT_INIT_FILE}")
  else
    read -rsp "  Enter transit unseal key (for k8s Secret): " TRANSIT_UNSEAL_KEY; echo ""
  fi
fi
info "Creating/updating k8s Secret openbao-transit-unseal in namespace ${TRANSIT_NS}..."
kubectl create secret generic openbao-transit-unseal \
  -n "${TRANSIT_NS}" \
  --from-literal=unseal-key="${TRANSIT_UNSEAL_KEY}" \
  --dry-run=client -o yaml | kubectl apply -f -
success "k8s Secret openbao-transit-unseal created/updated."

# ─── Revoke the root token; it has nothing left to do ────────────────────────
# Everything it can do has been done: the engine is enabled, the autounseal
# key exists, the policy is written, and — the two things anything ELSE ever
# actually needs — openbao-transit-token (what the primary authenticates
# with) and openbao-transit-unseal (what this transit pod unseals itself
# with on every restart) are both durable Kubernetes Secrets as of the lines
# just above. Nothing reachable from here needs the root token again.
#
# If it is ever needed again regardless — to rotate the autounseal key, say —
# it is not gone, only dormant: 'bao operator generate-root' mints a fresh one
# from the SAME unseal key that openbao-transit-unseal already holds, the same
# way the primary's own root token is disposable rather than precious (see
# E-04-revoke-bootstrap-token.sh). That is what makes revoking the current one
# safe rather than merely convenient.
info "Revoking the openbao-transit root token — its bootstrap work is done..."
if curl -sf -X POST \
    -H "X-Vault-Token: ${TRANSIT_ROOT_TOKEN}" \
    "${TRANSIT_ADDR}/v1/auth/token/revoke-self" >/dev/null 2>&1; then
  success "openbao-transit root token revoked."
else
  warn "Could not revoke the openbao-transit root token — revoke it by hand:"
  warn "  curl -X POST -H \"X-Vault-Token: <token>\" ${TRANSIT_ADDR}/v1/auth/token/revoke-self"
fi

# The local file held the same token, plus the unseal key — both now
# redundant with the k8s Secrets just written, so nothing needs it to exist.
# Deleting it outright rather than stripping one field: purge_local_state
# (scripts/lib/teardown.sh) already deletes this same file wholesale at
# uninstall for the identical reason ("init files holding unseal keys for
# storage that no longer exists"); this is that same judgment, made the
# moment the file stops being needed rather than only at teardown.
if [[ -f "${TRANSIT_INIT_FILE}" ]]; then
  rm -f "${TRANSIT_INIT_FILE}"
  info "Removed ${TRANSIT_INIT_FILE} — its content is now in the k8s Secrets above."
fi

echo ""
echo -e "${GREEN}╔══════════════════════════════════════════════════════════════╗${NC}"
echo -e "${GREEN}║  ✅ openbao-transit bootstrap complete!                      ║${NC}"
echo -e "${GREEN}╠══════════════════════════════════════════════════════════════╣${NC}"
echo -e "${GREEN}║  Address : ${TRANSIT_ADDR}${NC}"
echo -e "${GREEN}╚══════════════════════════════════════════════════════════════╝${NC}"
echo ""
echo "─── Existing cluster: seal migration ────────────────────────────────────"
echo "The primary OpenBao config now includes a transit seal stanza."
echo "The VAULT_TRANSIT_SEAL_TOKEN env var is injected from the k8s Secret."
echo ""
echo "  1. Sync ArgoCD app to pick up new openbao/values.yaml:"
echo "       argocd app sync openbao"
echo ""
echo "  2. Delete pod to restart with the seal stanza active:"
echo "       kubectl delete pod -n openbao openbao-0"
echo ""
echo "  3. Unseal once with the PRIMARY Shamir key + migration flag:"
echo "     (this is the last manual unseal ever required)"
echo "       kubectl exec -n openbao openbao-0 -- bao operator unseal -migrate <primary-shamir-key>"
echo "     NOTE: the transit unseal key is NOT the primary Shamir key."
echo "           Find the primary key in Bitwarden under gentian/openbao."
echo ""
echo "  4. Verify:"
echo "       kubectl exec -n openbao openbao-0 -- bao status | grep -E 'Seal Type|Sealed'"
echo "       # Seal Type: transit   Sealed: false"
echo "─────────────────────────────────────────────────────────────────────────"
