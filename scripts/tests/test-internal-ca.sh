#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-internal-ca.sh
# =============================================================================
# The internal CA, gentian-ca, is on every cluster, and its key is the
# vault's:
#
#   A-09  applies a ClusterIssuer that asks the vault's PKI engine to sign,
#         and the ServiceAccount it logs in as, in every issuer mode but
#         private-ca, where gentian-ca stays the person's own CA; it retires
#         whatever held the CA's key in a Kubernetes object before.
#   B-08  makes the CA in the vault once (generated there, or imported from
#         the CA cert-manager made on a self-signed cluster before), writes
#         the role that says what may be signed, and publishes the CA's
#         certificate -- never a key. It never replaces a CA, never makes one
#         when it cannot tell, and no key travels as an argument. The object
#         store's signing keys are seeded beside it, once.
#   B-01  moves the vault's own TLS onto its CA openbao-ca: copies that CA's
#         certificate to cert-manager's namespace, tells the vault to load
#         its new certificate, and restarts the custodian when the CA it
#         trusts changed -- each once.
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
# a Secret's file holding its JSON; jp/<key> answers one jsonpath query;
# calls.log records every mutating call, with what it read on stdin. The
# vault: vault/<kv path>.json, pki/ for the PKI engine; bao-args.log records
# every bao argument list and bao-stdin.log what bao read on stdin.
new_state() {
    STATE="$(mktemp -d "${SANDBOX}/state.XXXXXX")"
    mkdir -p "${STATE}/objects" "${STATE}/vault" "${STATE}/jp" "${STATE}/pki"
    : > "${STATE}/calls.log"; : > "${STATE}/bao-args.log"; : > "${STATE}/bao-stdin.log"
    : > "${STATE}/pods"; : > "${STATE}/deploys"
    echo '{"secret/":{}}' > "${STATE}/mounts.json"
}
have() { mkdir -p "$(dirname "${STATE}/objects/$1")"; printf '%s' "${2:-}" > "${STATE}/objects/$1"; }
jpkey() { printf '%s' "$*" | tr -c 'A-Za-z0-9' '_'; }
# jp <kind> <ns> <name> <jsonpath> <value>
jp() { printf '%s' "$5" > "${STATE}/jp/$(jpkey "$1" "$2" "$3" "$4")"; }

# run <step-or-empty> <script> [VAR=value ...]
run() {
    local step="$1" script="$2"; shift 2
    env -i HOME="${SANDBOX}/home" PATH="${PATH}" SCRIPT_DIR="${REPO}" STATE="${STATE}" \
        KERNEL_DOMAIN=example.test STEP="${step}" SCRIPT="${script}" \
        OPENBAO_TLS_TIMEOUT=0 OPENBAO_POD_WAIT=0 "$@" \
        bash -c '
            source "${SCRIPT_DIR}/scripts/lib/load.sh" >/dev/null 2>&1
            source "${SCRIPT_DIR}/scripts/lib/driver.sh"
            trap - ERR; set +e
            sleep() { :; }
            gentian_cert_manager_namespace() { echo kernel-edge; }
            apply_gentian_cluster_issuers() { echo "acme-issuers" >> "${STATE}/calls.log"; }
            gentian_dns_provider() { echo none; }
            jpkey() { printf "%s" "$*" | tr -c "A-Za-z0-9" "_"; }
            kubectl() {
                local verb="$1"; shift
                local ns="" out="" file="" pos=() a
                while (( $# )); do
                    case "$1" in
                        -n) ns="$2"; shift 2 ;;
                        -o) out="$2"; shift 2 ;;
                        -o=*) out="${1#-o=}"; shift ;;
                        -f) file="$2"; shift 2 ;;
                        -l|-c) shift 2 ;;
                        --) shift; pos+=(-- "$@"); break ;;
                        -*) shift ;;
                        *) pos+=("$1"); shift ;;
                    esac
                done
                local kind="${pos[0]:-}" name="${pos[1]:-}"
                if [[ "${kind}" == */* ]]; then name="${kind#*/}"; kind="${kind%%/*}"; fi
                case "${verb}" in
                    get)
                        [[ "${kind}" == cluster.gentianos.io ]] && { echo "${ISSUER_MODE:-}"; return 0; }
                        [[ "${kind}" == deploy && "${name}" == cert-manager-webhook ]] && return 0
                        [[ "${kind}" == pods ]] && { cat "${STATE}/pods"; return 0; }
                        [[ "${kind}" == deploy ]] && { cat "${STATE}/deploys"; return 0; }
                        if [[ "${out}" == jsonpath=* ]]; then
                            local k; k="${STATE}/jp/$(jpkey "${kind}" "${ns:-_}" "${name}" "${out#jsonpath=}")"
                            [[ -f "${k}" ]] && { cat "${k}"; return 0; }
                            [[ -f "${STATE}/objects/${kind}/${ns:-_}/${name}" ]] && return 0
                            return 1
                        fi
                        [[ -f "${STATE}/objects/${kind}/${ns:-_}/${name}" ]] || return 1
                        cat "${STATE}/objects/${kind}/${ns:-_}/${name}" ;;
                    apply)
                        if [[ "${file}" == "-" ]]; then echo "apply ${ns} - $(cat | tr "\n" " ")" >> "${STATE}/calls.log"
                        else echo "apply ${ns} ${file}" >> "${STATE}/calls.log"; fi ;;
                    delete) echo "delete ${kind} ${name} ${ns}" >> "${STATE}/calls.log"
                            rm -f "${STATE}/objects/${kind}/${ns:-_}/${name}" ;;
                    exec)
                        local cmd="${pos[*]:2}"
                        case "${cmd}" in
                            "cat /openbao/tls/tls.crt") cat "${STATE}/served" ;;
                            "pkill -HUP -x bao") echo "sighup ${name}" >> "${STATE}/calls.log" ;;
                            *) return 1 ;;
                        esac ;;
                    annotate)
                        local kv="${pos[1]}" key val
                        [[ "${pos[0]}" == */* ]] && kv="${pos[1]}"
                        key="${kv%%=*}"; val="${kv#*=}"
                        printf "%s" "${val}" > "${STATE}/jp/$(jpkey "${kind}" "${ns:-_}" "${name}" "{.metadata.annotations.${key//./\\.}}")"
                        echo "annotate ${kind}/${name} ${key}" >> "${STATE}/calls.log" ;;
                    rollout) echo "rollout ${pos[*]}" >> "${STATE}/calls.log" ;;
                    wait) return 0 ;;
                    *) return 1 ;;
                esac
            }
            bao() {
                printf "%s\n" "$*" >> "${STATE}/bao-args.log"
                [[ -n "${SEALED:-}" ]] && return 2
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
                    "secrets list") cat "${STATE}/mounts.json" ;;
                    "secrets enable")
                        jq ". + {\"pki-internal/\": {}}" "${STATE}/mounts.json" > "${STATE}/m" && mv "${STATE}/m" "${STATE}/mounts.json" ;;
                    "secrets tune") return 0 ;;
                    *)
                        case "$*" in
                            "read -format=json pki-internal/config/issuers")
                                jq -n --arg d "$(cat "${STATE}/pki/default" 2>/dev/null)" "{data: {default: \$d}}" ;;
                            "read -field=certificate pki-internal/cert/ca") cat "${STATE}/pki/ca.crt" ;;
                            "write pki-internal/root/generate/internal "*)
                                openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 \
                                    -subj "/CN=gentian-root-ca" -keyout /dev/null -out "${STATE}/pki/ca.crt" 2>/dev/null
                                echo generated > "${STATE}/pki/default" ;;
                            "write -format=json pki-internal/issuers/import/bundle pem_bundle=-")
                                cat > "${STATE}/pki/bundle"; cat "${STATE}/pki/bundle" >> "${STATE}/bao-stdin.log"
                                openssl x509 -in "${STATE}/pki/bundle" > "${STATE}/pki/ca.crt"
                                echo "{\"data\": {\"imported_issuers\": [\"imported\"]}}" ;;
                            "write pki-internal/config/issuers default="*) a="$*"; echo "${a##*=}" > "${STATE}/pki/default" ;;
                            "write pki-internal/roles/gentian-ca "*) printf "%s\n" "${@:3}" > "${STATE}/pki/role" ;;
                            *) return 1 ;;
                        esac ;;
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
baoargs() { cat "${STATE}/bao-args.log"; }
role() { cat "${STATE}/pki/role" 2>/dev/null; }
vault() { cat "${STATE}/vault/gentian-os_kernel_$1.json" 2>/dev/null; }

echo ""
echo "The internal CA on every cluster (A-09)"

for mode in acme-dns01 acme-http01 self-signed; do
    new_state
    out="$(run A-09-cluster-issuers 'apply' ISSUER_MODE="${mode}")"
    check "${mode}: a ClusterIssuer that asks the vault's PKI engine to sign, and its ServiceAccount" \
        '[[ "$(calls)" == *"apply kernel-edge - "*"kind: ServiceAccount"*"name: gentian-ca-issuer"*"kind: ClusterIssuer"*"path: pki-internal/sign/gentian-ca"* ]]' "${out}"
    check "${mode}: at this cluster's vault, verified against the vault's CA, with no placeholder left" \
        '[[ "$(calls)" == *"server: \"https://openbao.kernel-secrets.svc.cluster.local:8200\""*"name: openbao-ca-bundle"* && "$(calls)" == *"namespace: \"kernel-edge\""* && "$(calls)" != *"@"*"@"* ]]' "${out}"
    check "${mode}: no ExternalSecret, no Secret with a key, no CA issuer" \
        '[[ "$(calls)" != *"ExternalSecret"* && "$(calls)" != *"secretName"* && "$(calls)" != *"tls.key"* ]]' "${out}"
    out="$(run A-09-cluster-issuers 'check; echo "rc=$?"' ISSUER_MODE="${mode}")"
    check "${mode}: missing until it exists" '[[ "${out}" == *"rc=1"* ]]' "${out}"
done
new_state
out="$(run A-09-cluster-issuers 'apply' ISSUER_MODE=acme-dns01)"
check "acme: the public issuers are applied as before, beside the internal CA" \
    '[[ "$(calls)" == *"acme-issuers"* ]]' "${out}"

internal_ca_objects() {
    have clusterissuer/_/gentian-ca; jp clusterissuer _ gentian-ca '{.spec.vault.path}' pki-internal/sign/gentian-ca
    have serviceaccount/kernel-edge/gentian-ca-issuer; have rolebinding/kernel-edge/gentian-ca-issuer-token
}
new_state
internal_ca_objects; have clusterissuer/_/letsencrypt-http01
out="$(run A-09-cluster-issuers 'check; echo "rc=$?"' ISSUER_MODE=acme-http01)"
check "acme-http01: satisfied with the internal CA and the HTTP-01 issuer" '[[ "${out}" == *"rc=0"* ]]' "${out}"
jp clusterissuer _ gentian-ca '{.spec.vault.path}' ""
out="$(run A-09-cluster-issuers 'check; echo "rc=$?"' ISSUER_MODE=acme-http01)"
check "a gentian-ca that signs from a Secret is not the internal CA" '[[ "${out}" == *"rc=1"* ]]' "${out}"

new_state
internal_ca_objects
have certificate/kernel-edge/gentian-root-ca; have clusterissuer/_/gentian-selfsigned-bootstrap
have externalsecret/kernel-edge/gentian-root-ca-tls; have secret/kernel-edge/gentian-root-ca-tls '{}'
out="$(run A-09-cluster-issuers 'check; echo "rc=$?"' ISSUER_MODE=self-signed)"
check "self-signed: a CA cert-manager still makes is missing work" '[[ "${out}" == *"rc=1"* ]]' "${out}"
out="$(run A-09-cluster-issuers 'apply' ISSUER_MODE=self-signed)"
check "self-signed: that CA's Certificate and bootstrap issuer, and a KV-copying ExternalSecret, are retired" \
    '[[ "$(calls)" == *"delete certificate gentian-root-ca kernel-edge"* && "$(calls)" == *"delete clusterissuer gentian-selfsigned-bootstrap"* && "$(calls)" == *"delete externalsecret gentian-root-ca-tls kernel-edge"* ]]' "${out}"
check "self-signed: the CA's Secret stays for B-08 to import" '[[ "$(calls)" != *"delete secret"* ]]' "${out}"
out="$(run A-09-cluster-issuers 'check; echo "rc=$?"' ISSUER_MODE=self-signed)"
check "self-signed: satisfied afterwards" '[[ "${out}" == *"rc=0"* ]]' "${out}"

new_state
have secret/kernel-edge/gentian-root-ca-tls '{}'
out="$(run A-09-cluster-issuers 'apply' ISSUER_MODE=private-ca)"
check "private-ca: gentian-ca is the person's CA, and the vault's is not applied" \
    '[[ "$(calls)" == *"name: gentian-ca"*"secretName: gentian-root-ca-tls"* && "$(calls)" != *"pki-internal"* && "$(calls)" != *"gentian-ca-issuer"* ]]' "${out}"

echo ""
echo "The internal CA in the vault (B-08)"

new_state
out="$(run "" 'seed_internal_ca' ISSUER_MODE=acme-dns01)"
check "the PKI engine is enabled at pki-internal with a ten-year maximum" \
    'grep -qx "secrets enable -path=pki-internal -max-lease-ttl=87600h -description=gentian-ca, the cluster'"'"'s internal CA pki" "${STATE}/bao-args.log"' "$(baoargs)"
check "the CA is generated inside the vault: internal, EC P-256, ten years, no intermediates" \
    '[[ "$(baoargs)" == *"write pki-internal/root/generate/internal common_name=gentian-root-ca organization=gentian-os key_type=ec key_bits=256 ttl=87600h max_path_length=0"* && "$(baoargs)" != *"exported"* ]]' "$(baoargs)"
check "the role admits exactly the object store's names" \
    'grep -qx "allowed_domains=seaweedfs-server,seaweedfs-shell,seaweedfs-all-in-one,seaweedfs-all-in-one.system-s3,seaweedfs-all-in-one.system-s3.svc,seaweedfs-all-in-one.system-s3.svc.cluster.local" <<< "$(role)"' "$(role)"
check "and nothing beside them: no subdomains, wildcards, globs, localhost, IPs, URIs or any name" \
    '[[ "$(role)" == *"allow_subdomains=false"*"allow_wildcard_certificates=false"*"allow_glob_domains=false"*"allow_localhost=false"*"allow_any_name=false"*"allow_ip_sans=false"*"allowed_uri_sans="* ]]' "$(role)"
check "EC P-256 keys only, at most 90 days" '[[ "$(role)" == *"key_type=ec"*"key_bits=256"*"max_ttl=2160h"* ]]' "$(role)"
check "the CA's certificate is published as gentian-root-ca-tls, without a key" \
    '[[ "$(calls)" == *"apply  - "*"\"name\": \"gentian-root-ca-tls\""*"\"ca.crt\": \"-----BEGIN CERTIFICATE-----"* && "$(calls)" != *"PRIVATE KEY"* && "$(calls)" != *"tls.key"* ]]' "$(calls)"
check "no KV path holds the CA" '[[ -z "$(ls "${STATE}/vault")" && "$(baoargs)" != *"pki/internal-ca"* ]]' "$(baoargs)"
before="$(cat "${STATE}/pki/ca.crt")"
: > "${STATE}/bao-args.log"
out="$(run "" 'seed_internal_ca' ISSUER_MODE=acme-dns01)"
check "seeded once: a second run leaves the vault's CA alone" \
    '[[ "$(cat "${STATE}/pki/ca.crt")" == "${before}" && "${out}" == *"left as it is"* && "$(baoargs)" != *"root/generate"* && "$(baoargs)" != *"secrets enable"* ]]' "${out}"
check "and writes the role again" '[[ "$(baoargs)" == *"write pki-internal/roles/gentian-ca"* ]]' "$(baoargs)"
out="$(run "" 'seed_internal_ca' ISSUER_MODE=self-signed)"
check "self-signed: the role admits the kernel domain, the names under it and its wildcard too" \
    '[[ "$(role)" == *"allowed_domains=seaweedfs-server,"*",example.test"* && "$(role)" == *"allow_subdomains=true"*"allow_wildcard_certificates=true"* && "$(cat "${STATE}/pki/ca.crt")" == "${before}" ]]' "$(role)"

new_state
out="$(run "" 'seed_internal_ca; echo "rc=$?"' ISSUER_MODE=acme-dns01 SEALED=1)"
check "a vault that cannot answer: nothing is made, and the step fails" \
    '[[ "${out}" == *"rc=1"* && ! -f "${STATE}/pki/ca.crt" && "$(baoargs)" != *"root/generate"* ]]' "${out}"

new_state
openssl ecparam -name prime256v1 -genkey -noout -out "${SANDBOX}/old.key"
openssl req -x509 -key "${SANDBOX}/old.key" -days 30 -subj "/CN=gentian-root-ca" \
    -addext "basicConstraints=critical,CA:TRUE" -out "${SANDBOX}/old.crt" >/dev/null 2>&1
legacy() {
    jq -n --arg c "$(base64 -w0 < "${SANDBOX}/old.crt")" --arg k "$(base64 -w0 < "${SANDBOX}/old.key")" --arg cert "$1" \
        '{metadata: {name: "gentian-root-ca-tls", annotations: {"cert-manager.io/certificate-name": $cert}}, type: "kubernetes.io/tls", data: {"tls.crt": $c, "tls.key": $k}}'
}
have secret/kernel-edge/gentian-root-ca-tls "$(legacy gentian-root-ca)"
out="$(run "" 'seed_internal_ca' ISSUER_MODE=self-signed)"
check "a self-signed cluster keeps the CA cert-manager made: it is imported, not generated" \
    '[[ "$(cat "${STATE}/pki/ca.crt")" == "$(cat "${SANDBOX}/old.crt")" && "$(cat "${STATE}/pki/default")" == imported && "$(baoargs)" != *"root/generate"* && "${out}" == *"imported"* ]]' "${out}"
check "its key reached the vault on stdin, never as an argument" \
    '[[ "$(cat "${STATE}/bao-stdin.log")" == *"PRIVATE KEY"* ]] && ! grep -q "PRIVATE\|BEGIN" "${STATE}/bao-args.log"'
check "and the Secret is replaced by one without it" \
    '[[ "$(calls)" == *"delete secret gentian-root-ca-tls kernel-edge"* && "$(calls)" == *"\"gentianos.io/issuer-role\": \"internal-ca\""* && "$(calls)" != *"PRIVATE KEY"* && "$(calls)" != *"tls.key"* ]]' "$(calls)"

new_state
have secret/kernel-edge/gentian-root-ca-tls "$(legacy somebody-elses)"
out="$(run "" 'seed_internal_ca' ISSUER_MODE=acme-dns01)"
check "a Secret of that name that cert-manager's CA Certificate did not write is neither read nor replaced" \
    '[[ "$(cat "${STATE}/pki/default")" == generated && "$(calls)" != *"gentian-root-ca-tls"* && "${out}" == *"left as it is"* ]]' "${out}"

new_state
out="$(run "" 'seed_internal_ca' ISSUER_MODE=private-ca)"
check "private-ca: the vault is not asked anything" '[[ ! -s "${STATE}/bao-args.log" && "${out}" == *"private-ca"* ]]' "${out}"

echo ""
echo "The vault's own TLS on its CA openbao-ca (B-01)"
crt_a="$(printf 'leaf-a' | base64)"; ca_b64="$(printf 'the-ca' | base64)"
tls_state() {  # tls_state <issuer> <ca.crt of openbao-tls>
    new_state
    jp certificate kernel-secrets openbao-tls '{.spec.issuerRef.name}' "$1"
    jp certificate kernel-secrets openbao-tls '{.status.conditions[?(@.type=="Ready")].status}' True
    jp secret kernel-secrets openbao-ca-tls '{.data.tls\.crt}' "${ca_b64}"
    jp secret kernel-secrets openbao-tls '{.data.ca\.crt}' "$2"
    jp secret kernel-secrets openbao-tls '{.data.tls\.crt}' "${crt_a}"
    printf 'leaf-a' > "${STATE}/served"
    echo pod/openbao-0 > "${STATE}/pods"
    echo deployment.apps/gentian-os-custodian > "${STATE}/deploys"
}
tls_state openbao-selfsigned "${crt_a}"
out="$(run "" 'openbao_tls_settled; echo "rc=$?"')"
check "a self-signed serving certificate is not settled" '[[ "${out}" == *"rc=1"* ]]' "${out}"
out="$(run "" 'openbao_tls_settle; echo "rc=$?"')"
check "and settling waits for the CA's, then gives up rather than copy the wrong anchor" \
    '[[ "${out}" == *"rc=1"* && "$(calls)" != *"openbao-ca-bundle"* && "$(calls)" != *"sighup"* ]]' "${out}"

tls_state openbao-ca "${ca_b64}"
out="$(run "" 'openbao_tls_settle; echo "rc=$?"')"
check "the CA's certificate, and no key, is copied to cert-manager's namespace" \
    '[[ "$(calls)" == *"apply kernel-edge - "*"name: openbao-ca-bundle"*"ca.crt: ${ca_b64}"* && "$(calls)" != *"tls.key"* ]]' "${out}"
check "the vault is told to load its certificate, once the file is the new one" \
    '[[ "$(calls)" == *"sighup openbao-0"*"annotate pod/openbao-0 gentianos.io/tls-crt-sha256"* ]]' "$(calls)"
check "the custodian is restarted to trust the CA" \
    '[[ "$(calls)" == *"rollout restart deployment.apps/gentian-os-custodian"*"annotate deployment.apps/gentian-os-custodian gentianos.io/openbao-ca-sha256"* ]]' "$(calls)"
jp secret kernel-edge openbao-ca-bundle '{.data.ca\.crt}' "${ca_b64}"
out="$(run "" 'openbao_tls_settled; echo "rc=$?"')"
check "settled afterwards" '[[ "${out}" == *"rc=0"* ]]' "${out}"
: > "${STATE}/calls.log"
out="$(run "" 'openbao_tls_settle')"
check "a second run signals nothing and restarts nothing" \
    '[[ "$(calls)" != *"sighup"* && "$(calls)" != *"rollout"* ]]' "$(calls)"
jp secret kernel-secrets openbao-tls '{.data.tls\.crt}' "$(printf 'leaf-b' | base64)"
printf 'leaf-b' > "${STATE}/served"
out="$(run "" 'openbao_tls_settled; echo "rc=$?"')"
check "a renewed serving certificate the vault was not told about is not settled" '[[ "${out}" == *"rc=1"* ]]' "${out}"
out="$(run "" 'openbao_tls_settle')"
check "the vault is told about it; the custodian, whose CA is the same, is left alone" \
    '[[ "$(calls)" == *"sighup openbao-0"* && "$(calls)" != *"rollout"* ]]' "$(calls)"

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
