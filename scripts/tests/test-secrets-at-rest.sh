#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-secrets-at-rest.sh
# =============================================================================
# Pre-flight says whether Kubernetes Secrets are encrypted at rest (roadmap
# 1.18, certification plan C9). Where it can see the API server it reads the
# flags; where it cannot -- a managed control plane -- it reads the Cluster
# claim's controlPlane.secretsAtRest. Held here:
#
#   - a visible API server with --encryption-provider-config, or k3s/rke2 node
#     arguments with --secrets-encryption, is reported encrypted whatever the
#     claim says: what the server runs with wins over an attestation;
#   - a visible API server without the flag is a warning, and the claim cannot
#     talk it away;
#   - nothing visible and the claim saying provider or cluster is reported
#     encrypted, with the evidence echoed, and a provider without evidence is
#     asked for it;
#   - nothing visible and none, or no claim value, is a warning that names what
#     to do;
#   - GENTIAN_REQUIRE_SECRET_ENCRYPTION=true turns each warning into a refusal
#     (rc=1), and leaves the encrypted verdicts alone.
#
# No cluster: kubectl is a stand-in that answers from FAKE_* variables.
# =============================================================================
# Every check is a string handed to eval, so what it names is expanded there.
# shellcheck disable=SC2016
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0
ok()  { printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1)); }
bad() { printf '  %sFAIL%s  %s\n' "${RED}" "${NC}" "$1"; [[ -n "${2:-}" ]] && printf '%s\n' "$2"; fail=$((fail + 1)); }
check() { if eval "$2"; then ok "$1"; else bad "$1" "${3:-}"; fi; }

echo ""
echo "secrets at rest: pre-flight reads the API server where it can, the claim where it cannot"
echo ""

SB="$(mktemp -d "${TMPDIR:-/tmp}/gsar.XXXXXX")"
trap 'rm -rf "${SB}"' EXIT
mkdir -p "${SB}/bin" "${SB}/home"
# The stand-in answers the two questions the verdict asks: the API server pods'
# command line (FAKE_APISERVER_ARGS) and the nodes' k3s/rke2 arguments
# (FAKE_NODE_ARGS). Empty is "nothing visible", which is what a managed control
# plane shows.
cat >"${SB}/bin/kubectl" <<'STUB'
case "$*" in
    *"component=kube-apiserver"*) printf '%s' "${FAKE_APISERVER_ARGS:-}" ;;
    *"get nodes"*) printf '%s' "${FAKE_NODE_ARGS:-}" ;;
esac
exit 0
STUB
chmod +x "${SB}/bin/kubectl"

# run <snippet> [VAR=value ...] — the snippet, with the installer's functions
# loaded and the stand-in first on PATH. Prints what it printed, then rc=<n>.
run() {
    local snippet="$1"; shift
    env -i HOME="${SB}/home" PATH="${SB}/bin:${PATH}" SCRIPT_DIR="$(pwd)" "$@" \
        bash -c 'set -u; source scripts/lib/load.sh >/dev/null 2>&1; trap - ERR; set +e
'"${snippet}"'
echo "rc=$?"' 2>&1
}

v="$(run secrets_at_rest_verdict "FAKE_APISERVER_ARGS=[kube-apiserver --encryption-provider-config=/etc/kubernetes/enc.yaml --etcd-servers=x]")"
check "a visible API server with the flag is 'flag'" '[[ "${v}" == *"flag"* && "${v}" != *"no-flag"* ]]' "${v}"
v="$(run secrets_at_rest_verdict "FAKE_APISERVER_ARGS=[kube-apiserver --etcd-servers=x]" SECRETS_AT_REST=provider)"
check "a visible API server without the flag is 'no-flag', whatever the claim says" '[[ "${v}" == *"no-flag"* ]]' "${v}"
v="$(run secrets_at_rest_verdict "FAKE_NODE_ARGS=[\"server\",\"--secrets-encryption\",\"--disable=traefik\"]")"
check "k3s node arguments with --secrets-encryption are 'flag'" '[[ "${v}" == *"flag"* && "${v}" != *"no-flag"* ]]' "${v}"
v="$(run secrets_at_rest_verdict "FAKE_NODE_ARGS=[\"server\",\"--disable=traefik\"]")"
check "k3s node arguments without it are 'no-flag'" '[[ "${v}" == *"no-flag"* ]]' "${v}"
for want in provider cluster none; do
    v="$(run secrets_at_rest_verdict SECRETS_AT_REST=${want})"
    check "nothing visible and the claim says ${want}: '${want}'" '[[ "${v}" == "'"${want}"'"$'"'"'\n'"'"'"rc=0" ]]' "${v}"
done
v="$(run secrets_at_rest_verdict)"
check "nothing visible and no claim value is 'unknown'" '[[ "${v}" == "unknown"$'"'"'\n'"'"'"rc=0" ]]' "${v}"

out="$(run check_secrets_at_rest "FAKE_APISERVER_ARGS=[kube-apiserver --encryption-provider-config=/e.yaml]")"
check "the flag is reported encrypted and passes" '[[ "${out}" == *"[OK]"*"encryption provider"* && "${out}" == *"rc=0" ]]' "${out}"
out="$(run check_secrets_at_rest "FAKE_APISERVER_ARGS=[kube-apiserver --etcd-servers=x]")"
check "no flag is a warning that names the EncryptionConfiguration and the rewrite tool" \
    '[[ "${out}" == *"[WARN]"*"plain base64"* && "${out}" == *"EncryptionConfiguration"* && "${out}" == *"rewrite-secrets.sh"* && "${out}" == *"rc=0" ]]' "${out}"
out="$(run check_secrets_at_rest SECRETS_AT_REST=provider "SECRETS_AT_REST_EVIDENCE=Provider page §3, read 2026-10-20")"
check "provider with evidence passes and echoes the evidence" '[[ "${out}" == *"[OK]"*"read 2026-10-20"* && "${out}" != *"[WARN]"* && "${out}" == *"rc=0" ]]' "${out}"
out="$(run check_secrets_at_rest SECRETS_AT_REST=provider)"
check "provider without evidence passes and asks for the evidence" '[[ "${out}" == *"[OK]"* && "${out}" == *"[WARN]"*"evidence is empty"* && "${out}" == *"rc=0" ]]' "${out}"
out="$(run check_secrets_at_rest SECRETS_AT_REST=cluster)"
check "cluster passes" '[[ "${out}" == *"[OK]"*"secretsAtRest: cluster"* && "${out}" == *"rc=0" ]]' "${out}"
out="$(run check_secrets_at_rest SECRETS_AT_REST=none)"
check "none is a warning that says what to record" '[[ "${out}" == *"[WARN]"*"not encrypted"* && "${out}" == *"controlPlane.secretsAtRest"* && "${out}" == *"rc=0" ]]' "${out}"
out="$(run check_secrets_at_rest)"
check "unknown is a warning that names the provider question" '[[ "${out}" == *"[WARN]"*"unknown"* && "${out}" == *"Ask the provider"* && "${out}" == *"rc=0" ]]' "${out}"
out="$(run check_secrets_at_rest GENTIAN_REQUIRE_SECRET_ENCRYPTION=true)"
check "GENTIAN_REQUIRE_SECRET_ENCRYPTION=true makes unknown a refusal" '[[ "${out}" == *"[ERROR]"* && "${out}" == *"made this a refusal"* && "${out}" == *"rc=1" ]]' "${out}"
out="$(run check_secrets_at_rest GENTIAN_REQUIRE_SECRET_ENCRYPTION=true "FAKE_APISERVER_ARGS=[kube-apiserver --etcd-servers=x]")"
check "and a visible server without the flag a refusal" '[[ "${out}" == *"[ERROR]"* && "${out}" == *"rc=1" ]]' "${out}"
out="$(run check_secrets_at_rest GENTIAN_REQUIRE_SECRET_ENCRYPTION=true SECRETS_AT_REST=provider SECRETS_AT_REST_EVIDENCE=x)"
check "but leaves an attested provider alone" '[[ "${out}" == *"[OK]"* && "${out}" == *"rc=0" ]]' "${out}"

echo ""
echo "  ${pass} passed, ${fail} failed"
[[ ${fail} -eq 0 ]]
