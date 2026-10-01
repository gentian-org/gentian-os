#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-wildcard-cache.sh
# =============================================================================
# The kernel wildcard is kept on the install host across purges so a reinstall
# orders no new certificate (Let's Encrypt allows five per week per name set).
# What matters is that a usable copy is handed back, an unusable one is
# removed rather than left behind, a live Secret is never overwritten, and only
# --purge --cluster-infra deletes the copy.
#
# kubectl is a shell function over files in a throwaway directory; the
# certificates are self-signed, made here. No cluster is touched.
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
REPO="$(pwd)"

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0

SANDBOX="$(mktemp -d)"
trap 'rm -rf "${SANDBOX}"' EXIT
command -v openssl >/dev/null && command -v jq >/dev/null || { echo "openssl and jq required"; exit 1; }

# cert <name> <days> <issuer-cn> [<san>] — a TLS Secret as kubectl -o json.
cert() {
    local name="$1" days="$2" cn="$3" san="${4:-DNS:example.test,DNS:*.example.test}"
    openssl req -x509 -newkey rsa:2048 -nodes -days "${days}" -subj "/CN=${cn}" \
        -addext "subjectAltName=${san}" \
        -keyout "${SANDBOX}/${name}.key" -out "${SANDBOX}/${name}.crt" >/dev/null 2>&1
    jq -n --arg c "$(base64 -w0 < "${SANDBOX}/${name}.crt")" --arg k "$(base64 -w0 < "${SANDBOX}/${name}.key")" \
        '{apiVersion:"v1",kind:"Secret",type:"kubernetes.io/tls",
          metadata:{name:"wildcard-kernel-tls",namespace:"kernel-edge",uid:"x",resourceVersion:"1",
                    annotations:{"cert-manager.io/issuer-name":"old","unrelated":"y"}},
          data:{"tls.crt":$c,"tls.key":$k}}' > "${SANDBOX}/${name}.json"
}
cert good 90 "Fake R3"
cert expiring 1 "Fake R3"
cert staging 90 "(STAGING) Pretend Pear X1"
cert other 90 "Fake R3" "DNS:other.test,DNS:*.other.test"
# A certificate with another certificate's key.
jq --arg k "$(base64 -w0 < "${SANDBOX}/other.key")" '.data["tls.key"]=$k' "${SANDBOX}/good.json" > "${SANDBOX}/mismatch.json"

# run <live-secret-json-or-empty> <script> — a fresh shell with the libs, the
# fake kubectl, and HOME in the sandbox.
run() {
    local live="$1" script="$2"
    rm -f "${SANDBOX}/applied"
    # shellcheck disable=SC2016 # expanded by the child shell
    env -i HOME="${SANDBOX}/home" PATH="${PATH}" SCRIPT_DIR="${REPO}" KERNEL_DOMAIN=example.test \
        LIVE="${live}" APPLIED="${SANDBOX}/applied" \
        bash -c 'set -u; source scripts/lib/load.sh >/dev/null 2>&1
            gentian_cert_manager_namespace() { echo kernel-edge; }
            kubectl() {
                case "$1 $2" in
                    "get secret") [[ -n "${LIVE}" ]] && cat "${LIVE}" ;;
                    "apply -f")   cat > "${APPLIED}" ;;
                    *)            return 1 ;;
                esac
            }
            '"${script}" 2>&1
}
CACHE="${SANDBOX}/home/.gentian/certs/example.test.json"

check() {
    if eval "$2"; then printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1))
    else printf '  %sFAIL%s  %s\n%s\n' "${RED}" "${NC}" "$1" "${3:-}"; fail=$((fail + 1)); fi
}

echo ""
echo "Kernel wildcard kept across purges"

out="$(run "${SANDBOX}/good.json" save_kernel_wildcard)"
check "save writes the Secret, mode 600" \
    '[[ -f "${CACHE}" && "$(stat -c %a "${CACHE}")" == 600 && "$(stat -c %a "$(dirname "${CACHE}")")" == 700 ]]' "${out}"
check "save keeps only cert-manager annotations and no server fields" \
    '[[ "$(jq -c ".metadata" "${CACHE}")" == "{\"name\":\"wildcard-kernel-tls\",\"labels\":{},\"annotations\":{\"cert-manager.io/issuer-name\":\"old\"}}" ]]'
out="$(run "${SANDBOX}/good.json" save_kernel_wildcard)"
check "save of an unchanged Secret says nothing" '[[ -z "${out}" ]]' "${out}"

out="$(run "${SANDBOX}/good.json" 'restore_kernel_wildcard letsencrypt-dns01-cloudflare')"
check "restore leaves a live Secret alone" '[[ ! -f "${SANDBOX}/applied" && -f "${CACHE}" ]]' "${out}"

out="$(run "" 'restore_kernel_wildcard letsencrypt-dns01-cloudflare')"
check "restore applies a usable copy into the namespace, issuer from the claim" \
    '[[ "$(jq -r "[.metadata.namespace, .metadata.annotations[\"cert-manager.io/issuer-name\"], .metadata.annotations[\"cert-manager.io/certificate-name\"]] | join(\" \")" "${SANDBOX}/applied")" == "kernel-edge letsencrypt-dns01-cloudflare wildcard-kernel" && "${out}" == *"instead of ordering"* ]]' "${out}"

for c in "expiring:expires within a day:letsencrypt-dns01-cloudflare" \
         "other:other names:letsencrypt-dns01-cloudflare" \
         "mismatch:key does not match:letsencrypt-dns01-cloudflare" \
         "staging:staging certificate and the claim asks for production:letsencrypt-dns01-cloudflare" \
         "good:production certificate and the claim asks for staging:letsencrypt-staging-dns01-cloudflare"; do
    IFS=: read -r name why issuer <<< "${c}"
    mkdir -p "$(dirname "${CACHE}")"; jq -c 'del(.metadata.uid,.metadata.resourceVersion,.metadata.namespace)' "${SANDBOX}/${name}.json" > "${CACHE}"
    out="$(run "" "restore_kernel_wildcard ${issuer}")"
    check "restore removes, not reuses, a copy whose ${why}" \
        '[[ ! -f "${SANDBOX}/applied" && ! -f "${CACHE}" && "${out}" == *"${why}"* ]]' "${out}"
done

run "${SANDBOX}/good.json" save_kernel_wildcard >/dev/null
out="$(run "" 'GENTIAN_PURGE_CLUSTER_INFRA=0 purge_local_state; purge_report_remaining')"
check "--purge keeps the copy and says so" '[[ -f "${CACHE}" && "${out}" == *"--purge --cluster-infra removes it"* ]]' "${out}"
out="$(run "" 'GENTIAN_PURGE_CLUSTER_INFRA=1 purge_local_state')"
check "--purge --cluster-infra removes it and its directory" \
    '[[ ! -e "${CACHE}" && ! -e "$(dirname "${CACHE}")" ]]' "${out}"

echo ""
if (( fail > 0 )); then
    printf '%s%d of %d failed.%s\n' "${RED}" "${fail}" "$((pass + fail))" "${NC}"; exit 1
fi
printf '%sAll %d wildcard-cache cases pass.%s\n' "${GREEN}" "${pass}" "${NC}"
