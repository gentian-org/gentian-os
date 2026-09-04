#!/usr/bin/env bash
# =============================================================================
# scripts/recovery.sh — do the recovery, instead of reading how to
# =============================================================================
# The procedures in docs/recovery-playbook.md, as commands. Three things go
# wrong at three different scales, and each needs a different key from a
# different place:
#
#   cluster    the whole cluster is gone      -> the recovery kit
#   tenant     one workspace's data is gone   -> a backup key or a passphrase
#   inspect    you want to know before acting -> the same key, nothing written
#
# The key is the part worth automating. It can arrive as a file, as a photo of
# the QR code printed with the kit, from OpenBao when the cluster escrows it,
# or as a passphrase typed at a prompt — and every one of those ends up as the
# same Secret, so the caller should not have to care which they have.
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; YELLOW=$'\033[1;33m'
DIM=$'\033[2m'; NC=$'\033[0m'
info()    { echo "${DIM}   $*${NC}"; }
warn()    { echo "${YELLOW}[WARN]${NC}  $*"; }
error()   { echo "${RED}[ERROR]${NC} $*" >&2; }
success() { echo "${GREEN}[OK]${NC}    $*"; }
banner()  { echo ""; echo "── $* ─────────────────────────────────────────"; echo ""; }

VAULT_IDENTITY_PATH="gentian-os/kernel/backup/identity"

# Everything the identity touches lives here, 0700, removed on exit however we
# leave. A private key must not survive the process that used it.
WORK=""
cleanup() { [[ -n "${WORK}" && -d "${WORK}" ]] && rm -rf "${WORK}"; }
trap cleanup EXIT

usage() {
    cat <<'USAGE'
Usage: scripts/recovery.sh <command> [options]

Commands
  cluster    rebuild this cluster from a recovery kit
  tenant     restore one tenant from a bundle
  inspect    read a bundle's manifest; changes nothing
  show-key   print the backup key, and write its QR code

Where the key comes from (tenant, inspect, show-key)
  --key-file PATH    an age identity file (AGE-SECRET-KEY-...)
  --qr PATH          a photo or PNG of the QR code, decoded here
  --from-vault       the escrowed identity in OpenBao (default when reachable)
  --passphrase       prompt for a passphrase, for a passphrase bundle

cluster
  --kit PATH         the recovery kit to rebuild from

tenant
  --tenant NAME      the workspace to restore into
  --export NAME      a TenantExport in that workspace
  --bundle ENDPOINT,BUCKET,PREFIX[,REGION]
                     a bundle by location, when the export is gone
  --apps a,b         only these apps (default: every app in the bundle)
  --confirm NAME     required; must equal --tenant
  --dry-run          print the TenantRestore instead of applying it

inspect
  --tenant NAME      the workspace whose bundle to read
  --export NAME      which export (default: the newest Ready one)

Reaching the bundle straight from object storage, with no cluster
  --s3-endpoint URL  e.g. https://sos-ch-dk-2.exo.io
  --s3-bucket NAME
  --s3-prefix NAME   the bundle's prefix, e.g. policy-20260904-0300
  --s3-region NAME
  --s3-access-key K  or AWS_ACCESS_KEY_ID
  --s3-secret-key S  or AWS_SECRET_ACCESS_KEY

  Credentials are taken from these flags first, then the environment, then the
  cluster -- so this works when there is no cluster left to ask.

Examples
  scripts/recovery.sh inspect --tenant corp --from-vault

  # nothing but a bucket, a key and the printed QR code
  scripts/recovery.sh inspect --s3-endpoint https://sos-ch-dk-2.exo.io \
                              --s3-bucket bigbucket --s3-prefix policy-20260904-0300 \
                              --s3-access-key EXO... --s3-secret-key ... \
                              --qr backup-key.png
  scripts/recovery.sh tenant  --tenant corp --export policy-20260904-0300 \
                              --qr backup-key.png --confirm corp
  scripts/recovery.sh cluster --kit gentian-recovery-kit-ifk-w4h.age
USAGE
}

need() {
    command -v "$1" >/dev/null 2>&1 && return 0
    error "$1 is required and not on PATH."
    [[ -n "${2:-}" ]] && error "  $2"
    return 1
}

# ── the key ──────────────────────────────────────────────────────────────────

# decode_qr <image> — the identity out of a photo of the printed QR code.
#
# zbarimg first, pyzbar second: zbar reads PNG only where it was built against
# ImageMagick, which is true on Debian and not on Alpine, so the fallback is
# not redundant.
decode_qr() {
    local image="$1" out=""
    [[ -r "${image}" ]] || { error "Cannot read ${image}"; return 1; }

    if command -v zbarimg >/dev/null 2>&1; then
        out="$(zbarimg --quiet --raw "${image}" 2>/dev/null | head -1 || true)"
    fi
    if [[ -z "${out}" ]] && command -v python3 >/dev/null 2>&1; then
        out="$(python3 - "${image}" <<'PY' 2>/dev/null || true
import sys
try:
    from pyzbar.pyzbar import decode
    from PIL import Image
except ImportError:
    sys.exit(1)
found = decode(Image.open(sys.argv[1]))
print(found[0].data.decode() if found else "")
PY
)"
    fi

    out="$(printf '%s' "${out}" | tr -d '[:space:]')"
    if [[ -z "${out}" ]]; then
        error "No QR code found in ${image}."
        error "  Install a decoder:  sudo apt install zbar-tools"
        error "                  or  pip install pyzbar pillow"
        return 1
    fi
    case "${out}" in
        AGE-SECRET-KEY-*) printf '%s\n' "${out}" ;;
        *) error "That QR code is not an age identity (got ${out:0:16}...)."; return 1 ;;
    esac
}

# identity_from_vault — the escrowed copy, when the cluster keeps one.
identity_from_vault() {
    need bao "See docs/commands.md §7 for reaching OpenBao." || return 1
    local out
    out="$(bao kv get -mount=secret -field=identity "${VAULT_IDENTITY_PATH}" 2>/dev/null || true)"
    if [[ -z "${out}" ]]; then
        error "OpenBao holds no identity at ${VAULT_IDENTITY_PATH}."
        error "  Either this cluster does not escrow it (spec.backup.escrowIdentity)"
        error "  or BAO_ADDR/BAO_TOKEN are not set. Use --key-file or --qr instead."
        return 1
    fi
    printf '%s\n' "${out}"
}

# resolve_key — whichever source was named, ending as ${WORK}/identity, or as
# ${WORK}/passphrase for a passphrase bundle. Sets KEY_KIND.
KEY_KIND=""
resolve_key() {
    WORK="$(mktemp -d)"; chmod 700 "${WORK}"

    if [[ "${OPT_PASSPHRASE}" == "1" ]]; then
        local p
        read -rsp "  Bundle passphrase: " p; echo ""
        [[ -n "${p}" ]] || { error "Empty passphrase."; return 1; }
        printf '%s' "${p}" > "${WORK}/passphrase"
        chmod 600 "${WORK}/passphrase"
        KEY_KIND="passphrase"
        return 0
    fi

    local identity=""
    if [[ -n "${OPT_KEY_FILE}" ]]; then
        [[ -r "${OPT_KEY_FILE}" ]] || { error "Cannot read ${OPT_KEY_FILE}"; return 1; }
        identity="$(grep -m1 'AGE-SECRET-KEY-' "${OPT_KEY_FILE}" || true)"
        [[ -n "${identity}" ]] || { error "No AGE-SECRET-KEY- line in ${OPT_KEY_FILE}"; return 1; }
    elif [[ -n "${OPT_QR}" ]]; then
        identity="$(decode_qr "${OPT_QR}")" || return 1
        success "Read the key from ${OPT_QR}"
    else
        identity="$(identity_from_vault)" || return 1
        success "Read the escrowed key from OpenBao"
    fi

    printf '%s\n' "${identity}" > "${WORK}/identity"
    chmod 600 "${WORK}/identity"
    KEY_KIND="identity"

    # Say which key this is, so a wrong one is caught here rather than by a
    # restore that half-runs.
    if command -v age-keygen >/dev/null 2>&1; then
        info "public half: $(age-keygen -y "${WORK}/identity" 2>/dev/null || echo '?')"
    fi
}

# ── bundle location ──────────────────────────────────────────────────────────

BUNDLE_ENDPOINT=""; BUNDLE_BUCKET=""; BUNDLE_PREFIX=""; BUNDLE_REGION=""; BUNDLE_SECRET=""
BUNDLE_MODE=""

# locate_bundle — from --bundle, or from the export's own status.
locate_bundle() {
    if [[ -n "${OPT_S3_BUCKET}" ]]; then
        BUNDLE_ENDPOINT="${OPT_S3_ENDPOINT}"
        BUNDLE_BUCKET="${OPT_S3_BUCKET}"
        BUNDLE_PREFIX="${OPT_S3_PREFIX}"
        BUNDLE_REGION="${OPT_S3_REGION}"
        [[ -n "${BUNDLE_ENDPOINT}" ]] || { error "--s3-bucket needs --s3-endpoint."; return 1; }
        [[ -n "${BUNDLE_PREFIX}" ]]  || { error "--s3-bucket needs --s3-prefix."; return 1; }
        return 0
    fi
    if [[ -n "${OPT_BUNDLE}" ]]; then
        BUNDLE_ENDPOINT="$(printf '%s' "${OPT_BUNDLE}" | cut -d, -f1)"
        BUNDLE_BUCKET="$(printf '%s' "${OPT_BUNDLE}" | cut -d, -f2)"
        BUNDLE_PREFIX="$(printf '%s' "${OPT_BUNDLE}" | cut -d, -f3)"
        BUNDLE_REGION="$(printf '%s' "${OPT_BUNDLE}" | cut -d, -f4)"
        return 0
    fi

    need kubectl || return 1
    need jq || return 1
    local ns="tenant-${OPT_TENANT}"
    # Newest by completion time, not by name. kubectl lists in name order, so
    # taking the last line picks whatever sorts last -- which on a tenant with
    # both scheduled and manual exports is the wrong backup, silently.
    if [[ -z "${OPT_EXPORT}" ]]; then
        local newest
        newest="$(kubectl get tenantexports.gentianos.io -n "${ns}" -o json 2>/dev/null | jq -r '
            [ .items[]
              | select(.status.phase == "Ready")
              | select(.status.completedAt != null) ]
            | sort_by(.status.completedAt) | last
            | if . == null then "" else "\(.metadata.name) \(.status.completedAt)" end')"
        OPT_EXPORT="$(printf '%s' "${newest}" | cut -d' ' -f1)"
        [[ -n "${OPT_EXPORT}" ]] || { error "No completed export in ${ns}. Name one with --export."; return 1; }
        info "newest export: ${OPT_EXPORT} (completed $(printf '%s' "${newest}" | cut -d' ' -f2))"
    fi

    local json
    json="$(kubectl get tenantexport "${OPT_EXPORT}" -n "${ns}" -o json 2>/dev/null || true)"
    [[ -n "${json}" ]] || { error "No export ${OPT_EXPORT} in ${ns}."; return 1; }

    BUNDLE_ENDPOINT="$(printf '%s' "${json}" | jq -r '.status.bundle.endpoint // ""')"
    BUNDLE_BUCKET="$(printf '%s'   "${json}" | jq -r '.status.bundle.bucket // ""')"
    BUNDLE_PREFIX="$(printf '%s'   "${json}" | jq -r '.status.bundle.prefix // ""')"
    BUNDLE_REGION="$(printf '%s'   "${json}" | jq -r '.status.bundle.region // ""')"
    BUNDLE_SECRET="$(printf '%s'   "${json}" | jq -r '.status.bundle.credentialSecret // ""')"
    BUNDLE_MODE="$(printf '%s'     "${json}" | jq -r '.status.encryption.mode // ""')"
    [[ -n "${BUNDLE_BUCKET}" ]] || { error "Export ${OPT_EXPORT} records no bundle."; return 1; }
}

# mc_alias — point mc at the bundle's storage.
#
# Credentials come from the flags first, then the environment, and only then
# from the cluster. That order is the whole point: the case this script exists
# for is a cluster that is gone, and reaching for kubectl first would make the
# tool useless exactly when it is needed. The cluster path stays because it
# saves looking anything up while the cluster is still there.
mc_alias() {
    need mc "https://min.io/docs/minio/linux/reference/minio-mc.html" || return 1
    local ak="" sk="" kn="platform-kernel"

    if [[ -z "${BUNDLE_ENDPOINT}" ]]; then
        error "No endpoint for this bundle. Pass --s3-endpoint, or --bundle with one."
        return 1
    fi

    if [[ -n "${OPT_S3_AK}" && -n "${OPT_S3_SK}" ]]; then
        ak="${OPT_S3_AK}"; sk="${OPT_S3_SK}"
        info "using the credentials given on the command line"
    elif [[ -n "${AWS_ACCESS_KEY_ID:-}" && -n "${AWS_SECRET_ACCESS_KEY:-}" ]]; then
        ak="${AWS_ACCESS_KEY_ID}"; sk="${AWS_SECRET_ACCESS_KEY}"
        info "using AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY"
    elif [[ -n "${BUNDLE_SECRET}" ]] && command -v kubectl >/dev/null 2>&1; then
        ak="$(kubectl get secret "${BUNDLE_SECRET}" -n "${kn}" -o jsonpath='{.data.accessKey}' 2>/dev/null | base64 -d || true)"
        sk="$(kubectl get secret "${BUNDLE_SECRET}" -n "${kn}" -o jsonpath='{.data.secretKey}' 2>/dev/null | base64 -d || true)"
        [[ -n "${ak}" ]] && info "using ${BUNDLE_SECRET} from the cluster"
    fi

    if [[ -z "${ak}" || -z "${sk}" ]]; then
        error "No credentials for ${BUNDLE_ENDPOINT}."
        error "  Pass --s3-access-key and --s3-secret-key, or set"
        error "  AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY."
        return 1
    fi
    mc alias set gtn-recovery "${BUNDLE_ENDPOINT}" "${ak}" "${sk}" --api S3v4 >/dev/null
}

# ── commands ─────────────────────────────────────────────────────────────────

cmd_inspect() {
    [[ -n "${OPT_TENANT}" || -n "${OPT_BUNDLE}" || -n "${OPT_S3_BUCKET}" ]] ||
        { error "inspect needs --tenant, --bundle or --s3-bucket."; usage; return 1; }
    locate_bundle || return 1

    banner "Bundle"
    info "endpoint : ${BUNDLE_ENDPOINT:-platform storage}"
    info "bucket   : ${BUNDLE_BUCKET}/${BUNDLE_PREFIX}"
    info "encrypted: ${BUNDLE_MODE:-unknown}"

    [[ "${BUNDLE_MODE}" == "passphrase" && "${OPT_PASSPHRASE}" != "1" ]] &&
        warn "This bundle is passphrase-encrypted; pass --passphrase."

    mc_alias || return 1
    local base="gtn-recovery/${BUNDLE_BUCKET}/${BUNDLE_PREFIX}"

    banner "bundle-info.json (never encrypted)"
    mc cat "${base}/bundle-info.json" || { error "Cannot read the bundle."; return 1; }

    resolve_key || return 1
    banner "manifest.json"
    if [[ "${KEY_KIND}" == "passphrase" ]]; then
        mc cat "${base}/manifest.json.age" | age -d -i /dev/null 2>/dev/null ||
            mc cat "${base}/manifest.json.age" > "${WORK}/m.age" &&
            age -d "${WORK}/m.age"
    else
        mc cat "${base}/manifest.json.age" | age -d -i "${WORK}/identity"
    fi
    echo ""
    success "The bundle is readable and this key opens it."
}

cmd_tenant() {
    [[ -n "${OPT_TENANT}" ]] || { error "tenant needs --tenant."; return 1; }
    if [[ "${OPT_CONFIRM}" != "${OPT_TENANT}" ]]; then
        error "A restore replaces live data. Repeat the workspace name to confirm:"
        error "  --confirm ${OPT_TENANT}"
        return 1
    fi
    need kubectl || return 1
    locate_bundle || return 1
    resolve_key || return 1

    local ns="tenant-${OPT_TENANT}" secret="gtn-recovery-key" apps="[]"
    [[ -n "${OPT_APPS}" ]] && apps="[$(printf '%s' "${OPT_APPS}" | sed 's/,/","/g; s/^/"/; s/$/"/')]"

    local source_block
    if [[ -n "${OPT_EXPORT}" && -z "${OPT_BUNDLE}" ]]; then
        source_block="  exportRef: ${OPT_EXPORT}"
    else
        source_block="  bundle:
    endpoint: ${BUNDLE_ENDPOINT}
    bucket: ${BUNDLE_BUCKET}
    prefix: ${BUNDLE_PREFIX}
    region: ${BUNDLE_REGION}"
    fi

    local ref="identitySecretRef"
    [[ "${KEY_KIND}" == "passphrase" ]] && ref="passphraseSecretRef"

    local name; name="recovery-$(date -u +%Y%m%d-%H%M%S)"
    local manifest="apiVersion: gentianos.io/v1alpha1
kind: TenantRestore
metadata:
  name: ${name}
  namespace: ${ns}
spec:
${source_block}
  confirmTenant: ${OPT_TENANT}
  apps: ${apps}
  decryption:
    ${ref}:
      name: ${secret}"

    if [[ "${OPT_DRY_RUN}" == "1" ]]; then
        banner "Would apply"
        printf '%s\n' "${manifest}"
        return 0
    fi

    banner "Restoring ${OPT_TENANT}"
    warn "This replaces live data. Apps pause one at a time while it runs,"
    warn "and every member needs a password reset afterwards."

    kubectl create secret generic "${secret}" -n "${ns}" \
        --from-file="${KEY_KIND}=${WORK}/${KEY_KIND}" \
        --dry-run=client -o yaml | kubectl apply -f - >/dev/null
    printf '%s\n' "${manifest}" | kubectl apply -f - >/dev/null
    success "Applied ${name}"

    info "waiting; this can take an hour on a large workspace"
    kubectl wait --for=jsonpath='{.status.phase}'=Ready \
        "tenantrestore/${name}" -n "${ns}" --timeout=90m || true

    kubectl delete secret "${secret}" -n "${ns}" >/dev/null 2>&1 || true

    banner "Result"
    kubectl get tenantrestore "${name}" -n "${ns}" \
        -o jsonpath='  phase   : {.status.phase}{"\n"}  elapsed : {.status.startedAt} -> {.status.completedAt}{"\n"}  resets  : {.status.passwordResetRequired}{"\n"}'
    echo ""
    info "Send members through a reset in Admin Console -> Members."
}

cmd_cluster() {
    [[ -n "${OPT_KIT}" ]] || { error "cluster needs --kit PATH."; return 1; }
    [[ -r "${OPT_KIT}" ]] || { error "Cannot read ${OPT_KIT}"; return 1; }

    banner "Rebuilding from ${OPT_KIT}"
    info "The kit supplies what Git cannot hold — the master password, the"
    info "derivation salt and the unseal material. Everything else comes back"
    info "from gentian-deployments."
    echo ""
    warn "Tenant data is NOT part of this. Once the cluster is up, deploy each"
    warn "tenant and then run:  scripts/recovery.sh tenant --tenant <name> ..."
    echo ""
    exec "${REPO_ROOT}/install.sh" --recover "${OPT_KIT}"
}

cmd_show_key() {
    resolve_key || return 1
    [[ "${KEY_KIND}" == "identity" ]] || { error "show-key needs an age key, not a passphrase."; return 1; }

    banner "Backup key"
    cat "${WORK}/identity"
    echo ""
    if command -v qrencode >/dev/null 2>&1; then
        local png="gentian-backup-key.png"
        (umask 077 && qrencode -o "${png}" -l H -s 8 "$(cat "${WORK}/identity")")
        success "QR written to ${png} — print it, then delete the file"
        qrencode -t UTF8 -l H -m 2 "$(cat "${WORK}/identity")"
    else
        warn "Install qrencode for the printable QR code."
    fi
}

# ── arguments ────────────────────────────────────────────────────────────────

OPT_KIT=""; OPT_TENANT=""; OPT_EXPORT=""; OPT_BUNDLE=""; OPT_APPS=""
OPT_CONFIRM=""; OPT_KEY_FILE=""; OPT_QR=""; OPT_PASSPHRASE="0"
OPT_DRY_RUN="0"
OPT_S3_ENDPOINT=""; OPT_S3_BUCKET=""; OPT_S3_PREFIX=""; OPT_S3_REGION=""
OPT_S3_AK=""; OPT_S3_SK=""

[[ $# -gt 0 ]] || { usage; exit 1; }
COMMAND="$1"; shift

while [[ $# -gt 0 ]]; do
    case "$1" in
        --kit)        shift; OPT_KIT="${1:-}" ;;
        --tenant)     shift; OPT_TENANT="${1:-}" ;;
        --export)     shift; OPT_EXPORT="${1:-}" ;;
        --bundle)     shift; OPT_BUNDLE="${1:-}" ;;
        --apps)       shift; OPT_APPS="${1:-}" ;;
        --confirm)    shift; OPT_CONFIRM="${1:-}" ;;
        --key-file)   shift; OPT_KEY_FILE="${1:-}" ;;
        --qr)         shift; OPT_QR="${1:-}" ;;
        --from-vault) : ;;
        --passphrase) OPT_PASSPHRASE="1" ;;
        --dry-run)    OPT_DRY_RUN="1" ;;
        --s3-endpoint)   shift; OPT_S3_ENDPOINT="${1:-}" ;;
        --s3-bucket)     shift; OPT_S3_BUCKET="${1:-}" ;;
        --s3-prefix)     shift; OPT_S3_PREFIX="${1:-}" ;;
        --s3-region)     shift; OPT_S3_REGION="${1:-}" ;;
        --s3-access-key) shift; OPT_S3_AK="${1:-}" ;;
        --s3-secret-key) shift; OPT_S3_SK="${1:-}" ;;
        -h|--help)    usage; exit 0 ;;
        *) error "Unknown option: $1"; usage; exit 1 ;;
    esac
    shift
done

case "${COMMAND}" in
    cluster)  cmd_cluster ;;
    tenant)   cmd_tenant ;;
    inspect)  cmd_inspect ;;
    show-key) cmd_show_key ;;
    -h|--help|help) usage ;;
    *) error "Unknown command: ${COMMAND}"; usage; exit 1 ;;
esac
