#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-internal-ca.sh
# =============================================================================
# The internal CA, gentian-ca, is on every cluster: A-09 applies its
# ClusterIssuer and the ExternalSecret that materialises its key in every
# issuer mode but private-ca, where gentian-ca stays the person's own CA; it
# retires the CA cert-manager used to make on a self-signed cluster. B-08
# seeds the CA into the vault once, at random, or from the CA cert-manager
# made before; and the object store's signing keys beside it. Neither ever
# replaces what the vault holds, and no key travels as an argument.
#
# kubectl and bao are shell functions over files in a throwaway directory.
# No cluster and no vault is touched.
# =============================================================================
# Every check is a condition string evaluated after the run it inspects:
# shellcheck disable=SC2016,SC2034 # and the variables they read are set for them
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
REPO="$(pwd)"

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0

SANDBOX="$(mktemp -d)"
trap 'rm -rf "${SANDBOX}"' EXIT
for tool in openssl jq; do
    command -v "${tool}" >/dev/null || { echo "${tool} required"; exit 1; }
done

# A cluster is a directory: objects/<kind>/<namespace>/<name> exist as files,
# a Secret's file holding its JSON; calls.log records every mutating call; the
# vault is vault/<path>.json; bao-args.log records every bao argument list.
new_state() {
    STATE="$(mktemp -d "${SANDBOX}/state.XXXXXX")"
    mkdir -p "${STATE}/objects" "${STATE}/vault"
    : > "${STATE}/calls.log"; : > "${STATE}/bao-args.log"
}
have() { mkdir -p "$(dirname "${STATE}/objects/$1")"; printf '%s' "${2:-}" > "${STATE}/objects/$1"; }

# run <step-or-empty> <script> [VAR=value ...]
run() {
    local step="$1" script="$2"; shift 2
    env -i HOME="${SANDBOX}/home" PATH="${PATH}" SCRIPT_DIR="${REPO}" STATE="${STATE}" \
        KERNEL_DOMAIN=example.test STEP="${step}" SCRIPT="${script}" "$@" \
        bash -c '
            source "${SCRIPT_DIR}/scripts/lib/load.sh" >/dev/null 2>&1
            source "${SCRIPT_DIR}/scripts/lib/driver.sh"
            trap - ERR; set +e
            sleep() { :; }
            gentian_cert_manager_namespace() { echo kernel-edge; }
            apply_gentian_cluster_issuers() { echo "acme-issuers" >> "${STATE}/calls.log"; }
            gentian_dns_provider() { echo none; }
            kubectl() {
                local verb="$1" kind="${2:-}" name="${3:-}" ns="" a prev=""
                for a in "$@"; do [[ "${prev}" == "-n" ]] && ns="${a}"; prev="${a}"; done
                case "${verb}" in
                    get)
                        [[ "${kind}" == cluster.gentianos.io ]] && { echo "${ISSUER_MODE:-}"; return 0; }
                        [[ "${kind}" == deploy ]] && return 0
                        [[ -f "${STATE}/objects/${kind}/${ns:-_}/${name}" ]] || return 1
                        cat "${STATE}/objects/${kind}/${ns:-_}/${name}" ;;
                    apply)
                        local file="" ; prev=""
                        for a in "$@"; do [[ "${prev}" == "-f" ]] && file="${a}"; prev="${a}"; done
                        if [[ "${file}" == "-" ]]; then echo "apply - $(cat | tr "\n" " ")" >> "${STATE}/calls.log"
                        else echo "apply ${ns} ${file}" >> "${STATE}/calls.log"; fi ;;
                    delete) echo "delete ${kind} ${name} ${ns}" >> "${STATE}/calls.log"
                            rm -f "${STATE}/objects/${kind}/${ns:-_}/${name}" ;;
                    rollout|wait) return 0 ;;
                    *) return 1 ;;
                esac
            }
            bao() {
                printf "%s\n" "$*" >> "${STATE}/bao-args.log"
                local path="${*: -1}" field="" a
                for a in "$@"; do [[ "${a}" == -field=* ]] && field="${a#-field=}"; done
                case "$1 $2" in
                    "kv get")
                        [[ -f "${STATE}/vault/${path//\//_}.json" ]] || return 2
                        jq -r --arg f "${field}" ".[\$f] // empty" "${STATE}/vault/${path//\//_}.json" ;;
                    "kv put")
                        path="${*: -2:1}"
                        [[ -f "${STATE}/vault/${path//\//_}.json" ]] && return 2
                        cat > "${STATE}/vault/${path//\//_}.json" ;;
                    *) return 1 ;;
                esac
            }
            [[ -n "${STEP}" ]] && source "${SCRIPT_DIR}/scripts/steps/${STEP}.sh"
            eval "${SCRIPT}"
        ' 2>&1
}

check() {
    if eval "$2"; then printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1))
    else printf '  %sFAIL%s  %s\n%s\n' "${RED}" "${NC}" "$1" "${3:-}"; fail=$((fail + 1)); fi
}
calls() { cat "${STATE}/calls.log"; }
vault() { cat "${STATE}/vault/gentian-os_kernel_$1.json" 2>/dev/null; }

echo ""
echo "The internal CA on every cluster (A-09)"

for mode in acme-dns01 acme-http01 self-signed; do
    new_state
    out="$(run A-09-cluster-issuers 'apply' ISSUER_MODE="${mode}")"
    check "${mode}: the internal CA's ClusterIssuer and ExternalSecret are applied in cert-manager's namespace" \
        'grep -qx "apply kernel-edge ${REPO}/kernel/manifests/cert-manager/internal-ca.yaml" "${STATE}/calls.log"' "${out}"
    out="$(run A-09-cluster-issuers 'check; echo "rc=$?"' ISSUER_MODE="${mode}")"
    check "${mode}: missing until both exist" '[[ "${out}" == *"rc=1"* ]]' "${out}"
done
new_state
out="$(run A-09-cluster-issuers 'apply' ISSUER_MODE=acme-dns01)"
check "acme: the public issuers are applied as before, beside the internal CA" \
    '[[ "$(calls)" == *"acme-issuers"* ]]' "${out}"

new_state
have clusterissuer/_/gentian-ca; have externalsecret/kernel-edge/gentian-root-ca-tls
have clusterissuer/_/letsencrypt-http01
out="$(run A-09-cluster-issuers 'check; echo "rc=$?"' ISSUER_MODE=acme-http01)"
check "acme-http01: satisfied with the internal CA and the HTTP-01 issuer" '[[ "${out}" == *"rc=0"* ]]' "${out}"
rm -f "${STATE}/objects/externalsecret/kernel-edge/gentian-root-ca-tls"
out="$(run A-09-cluster-issuers 'check; echo "rc=$?"' ISSUER_MODE=acme-http01)"
check "acme-http01: a ClusterIssuer without its ExternalSecret is missing" '[[ "${out}" == *"rc=1"* ]]' "${out}"

new_state
have clusterissuer/_/gentian-ca; have externalsecret/kernel-edge/gentian-root-ca-tls
have certificate/kernel-edge/gentian-root-ca; have clusterissuer/_/gentian-selfsigned-bootstrap
out="$(run A-09-cluster-issuers 'check; echo "rc=$?"' ISSUER_MODE=self-signed)"
check "self-signed: a CA cert-manager still makes is missing work" '[[ "${out}" == *"rc=1"* ]]' "${out}"
out="$(run A-09-cluster-issuers 'apply' ISSUER_MODE=self-signed)"
check "self-signed: that CA's Certificate and bootstrap issuer are retired, its Secret is kept" \
    '[[ "$(calls)" == *"delete certificate gentian-root-ca kernel-edge"* && "$(calls)" == *"delete clusterissuer gentian-selfsigned-bootstrap"* && "$(calls)" != *"delete secret"* ]]' "${out}"
out="$(run A-09-cluster-issuers 'check; echo "rc=$?"' ISSUER_MODE=self-signed)"
check "self-signed: satisfied afterwards" '[[ "${out}" == *"rc=0"* ]]' "${out}"

new_state
have secret/kernel-edge/gentian-root-ca-tls '{}'
out="$(run A-09-cluster-issuers 'apply' ISSUER_MODE=private-ca)"
check "private-ca: gentian-ca is the person's CA, and the internal CA is not applied" \
    '[[ "$(calls)" == *"name: gentian-ca"*"secretName: gentian-root-ca-tls"* && "$(calls)" != *"internal-ca.yaml"* ]]' "${out}"

echo ""
echo "The internal CA in the vault (B-08)"

new_state
out="$(run "" 'seed_internal_ca' ISSUER_MODE=acme-dns01)"
crt="$(vault pki_internal-ca | jq -r .certificate)"
check "seeded at gentian-os/kernel/pki/internal-ca" '[[ -n "${crt}" && "$(vault pki_internal-ca | jq -r .private_key)" == *"PRIVATE KEY"* ]]' "${out}"
check "a CA, signing certificates only, no intermediates" \
    '[[ "$(openssl x509 -noout -text <<< "${crt}")" == *"CA:TRUE, pathlen:0"* && "$(openssl x509 -noout -text <<< "${crt}")" == *"Certificate Sign, CRL Sign"* ]]'
check "its key matches the certificate" \
    '[[ "$(openssl x509 -noout -pubkey <<< "${crt}")" == "$(vault pki_internal-ca | jq -r .private_key | openssl pkey -pubout)" ]]'
check "the key travels on stdin, never as an argument" '! grep -q "PRIVATE KEY\|BEGIN" "${STATE}/bao-args.log"'
before="$(vault pki_internal-ca)"
out="$(run "" 'seed_internal_ca' ISSUER_MODE=acme-dns01)"
check "seeded once: a second run leaves the vault's CA alone" '[[ "$(vault pki_internal-ca)" == "${before}" && "${out}" == *"left as it is"* ]]' "${out}"

new_state
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 -subj "/CN=gentian-root-ca" \
    -addext "basicConstraints=critical,CA:TRUE" -keyout "${SANDBOX}/old.key" -out "${SANDBOX}/old.crt" >/dev/null 2>&1
legacy() {
    jq -n --arg c "$(base64 -w0 < "${SANDBOX}/old.crt")" --arg k "$(base64 -w0 < "${SANDBOX}/old.key")" --arg cert "$1" \
        '{metadata: {name: "gentian-root-ca-tls", annotations: {"cert-manager.io/certificate-name": $cert}}, data: {"tls.crt": $c, "tls.key": $k}}'
}
have secret/kernel-edge/gentian-root-ca-tls "$(legacy gentian-root-ca)"
out="$(run "" 'seed_internal_ca' ISSUER_MODE=self-signed)"
check "a self-signed cluster keeps the CA cert-manager made: it is what is seeded" \
    '[[ "$(vault pki_internal-ca | jq -r .certificate)" == "$(cat "${SANDBOX}/old.crt")" && "${out}" == *"made on this cluster before"* ]]' "${out}"
new_state
have secret/kernel-edge/gentian-root-ca-tls "$(legacy somebody-elses)"
run "" 'seed_internal_ca' ISSUER_MODE=acme-dns01 >/dev/null
check "a Secret of that name that cert-manager's CA Certificate did not write is not read" \
    '[[ -n "$(vault pki_internal-ca)" && "$(vault pki_internal-ca | jq -r .certificate)" != "$(cat "${SANDBOX}/old.crt")" ]]'
new_state
out="$(run "" 'seed_internal_ca' ISSUER_MODE=private-ca)"
check "private-ca: nothing is seeded" '[[ -z "$(vault pki_internal-ca)" && "${out}" == *"private-ca"* ]]' "${out}"

echo ""
echo "The object store's signing keys (B-08)"
new_state
out="$(run "" 'seed_object_store_signing_keys')"
keys="$(vault storage_s3_jwt)"
check "four random keys at gentian-os/kernel/storage/s3/jwt" \
    '[[ "$(jq -r "[.signing_key, .signing_read_key, .filer_signing_key, .filer_signing_read_key] | map(test(\"^[0-9a-f]{64}$\")) | all" <<< "${keys}")" == true && "$(jq -r "[.[]] | unique | length" <<< "${keys}")" == 4 ]]' "${out}"
check "no key travels as an argument" '! grep -qE "[0-9a-f]{64}" "${STATE}/bao-args.log"'
out="$(run "" 'seed_object_store_signing_keys')"
check "seeded once" '[[ "$(vault storage_s3_jwt)" == "${keys}" && "${out}" == *"left as they are"* ]]' "${out}"

echo ""
if (( fail > 0 )); then
    printf '%s%d of %d failed.%s\n' "${RED}" "${fail}" "$((pass + fail))" "${NC}"; exit 1
fi
printf '%sAll %d internal-CA cases pass.%s\n' "${GREEN}" "${pass}" "${NC}"
