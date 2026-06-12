#!/usr/bin/env bash
# =============================================================================
# scripts/install-lib.sh — Shared helper functions for install.sh
# =============================================================================
# NOT a standalone installer.  Sourced by install.sh with
# GENTIAN_INSTALL_LIB_ONLY=1 to make its helper functions available without
# running main().
#
# This file is the shared library extracted from the legacy Tofu-based
# installer.  The current installer is install.sh (Crossplane-based).
#
# Environment variables consumed (when invoked via install.sh — see that
# file for the full list):
#   MASTER_PASSWORD, OD_PRIVATE_REGISTRY_USERNAME/PASSWORD,
#   OD_SMTP_RELAY_USERNAME/PASSWORD, MAIL_SERVICE_MODE, EXTERNAL_SMTP_HOST,
#   KERNEL_DOMAIN, NODE_IP, NETWORK_MODE, SKIP_TOOLS, OPENBAO_INIT_FILE,
#   GENTIAN_APPS_REPO, GENTIAN_APPS_BRANCH, GENTIAN_DEPLOYMENTS_REPO,
#   GENTIAN_DEPLOYMENTS_BRANCH, GENTIAN_NONINTERACTIVE,
#   INSTALL_CONFIG_FILE, INSTALL_SECRETS_FILE, INSTALL_AUTO_LOAD_CONFIG
# =============================================================================

set -euo pipefail

# ─── Colour helpers ──────────────────────────────────────────────────────────
RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'; CYAN='\033[0;36m'; NC='\033[0m'
info()    { echo -e "${CYAN}[INFO]${NC}  $*"; }
success() { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; }
banner()  { echo -e "\n${CYAN}══════════════════════════════════════════════════${NC}"; echo -e "${CYAN}  $*${NC}"; echo -e "${CYAN}══════════════════════════════════════════════════${NC}\n"; }

# ─── Shared CRI / kubelet runtime helpers ────────────────────────────────────
# ensure_sudo, cri_cleanup and kubelite_restart live in scripts/lib-runtime.sh
# so install.sh and uninstall.sh can use the same implementations.
# This file lives in scripts/, so lib-runtime.sh is a sibling.
__GENTIAN_INSTALL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib-runtime.sh
source "${__GENTIAN_INSTALL_DIR}/lib-runtime.sh"

# =============================================================================
# wait_for_running_pod NS LABEL_SELECTOR FRIENDLY_NAME [TIMEOUT_SECS]
#
# Polls every POLL_INTERVAL seconds until at least one pod matching
# LABEL_SELECTOR in NS is in phase=Running. Every STATUS_INTERVAL seconds it
# prints a status line with the current pod status. If a pod sits in a
# non-Running, non-Pending phase (e.g. ContainerCreating, ImagePullBackOff)
# for STUCK_THRESHOLD seconds it dumps the most recent kubelet events.
#
# Auto-recovery: when a pod has been stuck in ContainerCreating with no IP
# assigned for STUCK_RECOVERY_THRESHOLD seconds (a microk8s/containerd CNI
# sandbox hang we've seen repeatedly), delete the pod so its StatefulSet /
# Deployment / DaemonSet recreates it cleanly. Up to MAX_RECOVERY_ATTEMPTS
# automatic kicks per call.
#
# Returns 0 on success, 1 on timeout.
# =============================================================================
wait_for_running_pod() {
    local ns="$1" selector="$2" friendly="$3" timeout="${4:-300}"
    local poll_interval=5 status_interval=30 stuck_threshold=60
    local stuck_recovery_threshold=120 max_recovery_attempts=2
    local elapsed=0 stuck_for=0 last_status="" recovery_attempts=0

    info "Waiting for ${friendly} pod in namespace '${ns}' to become Running (up to ${timeout}s)..."
    while (( elapsed < timeout )); do
        local line phase
        line=$(kubectl get pods -n "$ns" -l "$selector" \
                --no-headers 2>/dev/null | head -1 || true)
        if [[ -z "$line" ]]; then
            phase="NotScheduledYet"
        else
            phase=$(awk '{print $3}' <<<"$line")
        fi

        if [[ "$phase" == "Running" ]]; then
            # Confirm at least one container is Ready.
            local ready
            ready=$(kubectl get pods -n "$ns" -l "$selector" \
                    -o jsonpath='{.items[0].status.containerStatuses[0].ready}' 2>/dev/null || echo "false")
            if [[ "$ready" == "true" ]]; then
                echo ""
                success "${friendly} pod is Running and Ready."
                return 0
            fi
        fi

        # Periodic status line.
        if (( elapsed % status_interval == 0 )); then
            echo "  [${elapsed}s] status: ${phase:-<none>} ${line:+($line)}"
        fi

        # Track stuck-in-non-progressing state.
        if [[ "$phase" != "Pending" && "$phase" != "Running" && "$phase" != "NotScheduledYet" ]] \
           || [[ "$phase" == "Pending" && -n "$line" ]]; then
            if [[ "$phase" == "$last_status" ]]; then
                stuck_for=$(( stuck_for + poll_interval ))
            else
                stuck_for=0
            fi
        else
            stuck_for=0
        fi
        last_status="$phase"

        if (( stuck_for == stuck_threshold )); then
            warn "${friendly} pod has been in '${phase}' for ${stuck_for}s. Recent events:"
            kubectl get events -n "$ns" --sort-by=.lastTimestamp 2>/dev/null \
                | tail -10 | sed 's/^/    /'
        fi

        # Auto-recovery for the silent CNI / containerd sandbox hang we've
        # seen on microk8s: pod sits in ContainerCreating with no assigned IP,
        # and kubelet has stopped emitting events. Deleting the pod forces the
        # owning controller to create a fresh one, which usually unsticks it.
        if (( stuck_for >= stuck_recovery_threshold )) \
           && (( recovery_attempts < max_recovery_attempts )) \
           && [[ "$phase" == "ContainerCreating" || "$phase" == "Init:0/"* ]]; then
            local pod_name pod_ip
            pod_name=$(kubectl get pods -n "$ns" -l "$selector" \
                    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
            pod_ip=$(kubectl get pods -n "$ns" -l "$selector" \
                    -o jsonpath='{.items[0].status.podIP}' 2>/dev/null || true)
            if [[ -n "$pod_name" && -z "$pod_ip" ]]; then
                recovery_attempts=$(( recovery_attempts + 1 ))
                warn "Auto-recovery ${recovery_attempts}/${max_recovery_attempts}:" \
                     "deleting wedged pod ${pod_name} (no IP after ${stuck_for}s in ${phase})."
                kubectl delete pod "$pod_name" -n "$ns" --grace-period=0 --force \
                    >/dev/null 2>&1 || true
                # On the second attempt, also sweep stale CRI state — the
                # most common root cause of this wedge is leaked sandboxes
                # from prior install/uninstall cycles.
                if (( recovery_attempts >= 2 )); then
                    cri_cleanup
                    # If pod-delete + cri_cleanup both failed, the kubelet
                    # status manager itself is wedged (calico assigns an IP
                    # but PodReadyToStartContainers never flips True). The
                    # only known-reliable fix is a kubelite restart. Done
                    # under sudo, gated on microk8s being detected.
                    kubelite_restart
                fi
                stuck_for=0
                last_status=""
            fi
        fi

        sleep "$poll_interval"
        elapsed=$(( elapsed + poll_interval ))
    done

    echo ""
    error "${friendly} pod did not reach Running within ${timeout}s."
    warn  "Diagnostics:"
    kubectl get pods -n "$ns" -l "$selector" -o wide 2>&1 | sed 's/^/    /'
    kubectl get events -n "$ns" --sort-by=.lastTimestamp 2>/dev/null \
        | tail -15 | sed 's/^/    /'

    # Diagnose actual failure mode rather than printing generic boilerplate.
    local d_pod d_ip d_phase d_started d_pvc_bound d_pull_err d_ready_cond
    d_pod=$(kubectl get pods -n "$ns" -l "$selector" \
            -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    d_ip=$(kubectl get pod -n "$ns" "$d_pod" \
            -o jsonpath='{.status.podIP}' 2>/dev/null || true)
    d_phase=$(kubectl get pod -n "$ns" "$d_pod" \
            -o jsonpath='{.status.phase}' 2>/dev/null || true)
    d_started=$(kubectl get pod -n "$ns" "$d_pod" \
            -o jsonpath='{.status.containerStatuses[0].started}' 2>/dev/null || true)
    d_ready_cond=$(kubectl get pod -n "$ns" "$d_pod" \
            -o jsonpath='{range .status.conditions[?(@.type=="PodReadyToStartContainers")]}{.status}{end}' 2>/dev/null || true)
    d_pvc_bound=$(kubectl get pvc -n "$ns" --no-headers 2>/dev/null \
            | awk '$2!="Bound"' | wc -l | tr -d ' ')
    d_pull_err=$(kubectl get events -n "$ns" --field-selector=reason=Failed \
            -o jsonpath='{range .items[*]}{.message}{"\n"}{end}' 2>/dev/null \
            | grep -iE 'pull|image' | head -1 || true)
    # Detect missing-secret / missing-configmap (CreateContainerConfigError).
    local d_missing_ref
    d_missing_ref=$(kubectl get events -n "$ns" --field-selector=reason=Failed \
            -o jsonpath='{range .items[*]}{.message}{"\n"}{end}' 2>/dev/null \
            | grep -iE '(secret|configmap) "[^"]+" not found' | head -1 || true)

    if (( d_pvc_bound > 0 )); then
        warn "Likely cause: PVC binding failure ($d_pvc_bound PVC(s) not Bound)."
    elif [[ -n "$d_missing_ref" ]]; then
        warn "Likely cause: missing referenced resource: ${d_missing_ref}"
        warn "Create the missing Secret/ConfigMap (often produced by a prior bootstrap step) and retry."
    elif [[ -n "$d_pull_err" ]]; then
        warn "Likely cause: image pull error: ${d_pull_err}"
    elif [[ "$d_started" == "true" && "$d_ready_cond" != "True" ]]; then
        warn "Likely cause: kubelet status-sync wedge (container started but"
        warn "PodReadyToStartContainers stays False)."
        warn "Recovery: fully restart your Kubernetes runtime, then re-run this script."
        warn "  microk8s : sudo microk8s stop && sudo microk8s start"
        warn "  k3s      : sudo systemctl restart k3s"
        warn "  kubeadm  : sudo systemctl restart kubelet containerd"
    elif [[ -n "$d_ip" && "$d_phase" == "Pending" ]]; then
        warn "Likely cause: kubelet wedge (IP assigned but phase still Pending)."
        warn "Recovery: fully restart your Kubernetes runtime, then re-run this script."
        warn "  microk8s : sudo microk8s stop && sudo microk8s start"
        warn "  k3s      : sudo systemctl restart k3s"
        warn "  kubeadm  : sudo systemctl restart kubelet containerd"
    else
        warn "Cause unclear. Check 'kubectl describe pod -n $ns $d_pod' for details."
    fi
    return 1
}

# cri_cleanup() and kubelite_restart() are defined in scripts/lib-runtime.sh
# (sourced near the top of this file) so they can be reused by uninstall.sh.

# ─── Paths ────────────────────────────────────────────────────────────────────
# When sourced as a library (GENTIAN_INSTALL_LIB_ONLY=1), SCRIPT_DIR is
# already set by the outer install.sh to the repo root.  Do not overwrite it.
# The ":-" default only applies when SCRIPT_DIR is unset or empty.
SCRIPT_DIR="${SCRIPT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"

# ─── Runtime defaults ─────────────────────────────────────────────────────────
OPENBAO_INIT_FILE="${OPENBAO_INIT_FILE:-/tmp/openbao-init.json}"
INSTALL_CLUSTER_INFRA="${INSTALL_CLUSTER_INFRA:-1}"
# Operator-managed env files (config + secrets). These are optional, but when
# present they are sourced automatically before prompting so installs can be
# fully declarative and non-interactive.
INSTALL_CONFIG_FILE="${INSTALL_CONFIG_FILE:-${SCRIPT_DIR}/install.env}"
INSTALL_SECRETS_FILE="${INSTALL_SECRETS_FILE:-${SCRIPT_DIR}/install.secrets.env}"
INSTALL_AUTO_LOAD_CONFIG="${INSTALL_AUTO_LOAD_CONFIG:-1}"
INSTALL_VALIDATE_ONLY="${INSTALL_VALIDATE_ONLY:-0}"
INSTALL_VERIFY_ONLY="${INSTALL_VERIFY_ONLY:-0}"
# Local on-disk cache of the credentials prompted on the first run, so that
# re-running install.sh after a partial failure does not re-prompt. The file
# is gitignored and chmod 600. Set INSTALL_SECRETS_CACHE=/dev/null to disable.
INSTALL_SECRETS_CACHE="${INSTALL_SECRETS_CACHE:-${SCRIPT_DIR}/.install-secrets.env}"

# Local on-disk cache of non-secret installer state (kernel domain, etc.) so
# re-runs do not re-prompt. Gitignored. Set INSTALL_STATE_FILE=/dev/null to
# disable persistence.
INSTALL_STATE_FILE="${INSTALL_STATE_FILE:-${SCRIPT_DIR}/.install-state.env}"
CERT_MANAGER_NAMESPACE="${CERT_MANAGER_NAMESPACE:-cert-manager}"

# Input precedence (highest -> lowest):
#   1) CLI flags / existing shell environment
#   2) installer config files (install.env, install.secrets.env)
#   3) local caches (.install-secrets.env, .install-state.env)
#   4) gentian-deployments cluster-settings.env (overrides 3 for cluster runtime)
#   5) OpenBao backfill for missing values
#   6) interactive prompts for missing required values
#
# Cluster runtime vars (KERNEL_DOMAIN, MAIL_SERVICE_MODE, …) belong in
# clusters/<cluster>/kernel/cluster-settings.env; .install-state.env keeps only
# installer-local state (see save_install_state).
INPUT_HIERARCHY_VARS=(
    MASTER_PASSWORD
    OD_PRIVATE_REGISTRY_USERNAME
    OD_PRIVATE_REGISTRY_PASSWORD
    OD_SMTP_RELAY_USERNAME
    OD_SMTP_RELAY_PASSWORD
    MAIL_SERVICE_MODE
    EXTERNAL_SMTP_HOST
    EXTERNAL_SMTP_PORT
    EXTERNAL_SMTP_SSL
    EXTERNAL_SMTP_STARTTLS
    KERNEL_DOMAIN
    TENANCY_MODE
    NODE_IP
    NETWORK_MODE
    SKIP_TOOLS
    OPENBAO_INIT_FILE
    LETSENCRYPT_EMAIL
    INGRESS_CLASS_NAME
    ROUTING_MODE
    GENTIAN_APPS_REPO
    GENTIAN_APPS_BRANCH
    GENTIAN_DEPLOYMENTS_REPO
    GENTIAN_DEPLOYMENTS_BRANCH
    GENTIAN_DEPLOYMENTS_PATH
    GENTIAN_DEPLOYMENTS_CLUSTER
    GENTIAN_DEPLOYMENTS_STAGE
    GENTIAN_NONINTERACTIVE
    INSTALL_CLUSTER_INFRA
    GENTIAN_MANAGED_CERT_MANAGER
    CF_API_TOKEN
    CF_ZONE_NAME
    SECRET_MODE
    MINIO_ENDPOINT
    CNPG_HOST
    STORAGE_CLASS
)

# ─── Versions ────────────────────────────────────────────────────────────────
TOFU_VERSION="1.9.0"
BAO_VERSION="2.5.1"
ESO_CHART_VERSION="2.4.1"
ENVOY_GATEWAY_CHART_VERSION="${ENVOY_GATEWAY_CHART_VERSION:-v1.2.5}"
ENVOY_GATEWAY_NAMESPACE="${ENVOY_GATEWAY_NAMESPACE:-envoy-gateway-system}"
GENTIAN_GATEWAY_CONTROLLER_NAME="${GENTIAN_GATEWAY_CONTROLLER_NAME:-gateway.envoyproxy.io/gentian-gatewayclass-controller}"

usage() {
    cat <<'EOF'
Usage: ./install.sh [options]

Options:
  --no-cluster-infra   Skip cluster infra installation (cert-manager, reloader, CNPG)
  --cluster-infra      Force cluster infra installation (default)
  --config-file PATH   Source non-secret installer config from PATH
  --secrets-file PATH  Source secret installer values from PATH
  --no-config-files    Disable auto-loading of install.env / install.secrets.env
    --verify-only        Skip install steps and only run ArgoCD health verification
  --validate, --check  Validate config and secrets; print a report and exit (no
                       cluster actions are taken)
  -h, --help           Show this help

Environment overrides:
  INSTALL_CLUSTER_INFRA=1|0
  INSTALL_CONFIG_FILE=/path/to/install.env
  INSTALL_SECRETS_FILE=/path/to/install.secrets.env
  INSTALL_VALIDATE_ONLY=1
EOF
}

parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --no-cluster-infra)
                INSTALL_CLUSTER_INFRA="0"
                ;;
            --cluster-infra)
                INSTALL_CLUSTER_INFRA="1"
                ;;
            --config-file)
                shift
                [[ $# -gt 0 ]] || { error "--config-file requires a value"; exit 1; }
                INSTALL_CONFIG_FILE="$1"
                ;;
            --secrets-file)
                shift
                [[ $# -gt 0 ]] || { error "--secrets-file requires a value"; exit 1; }
                INSTALL_SECRETS_FILE="$1"
                ;;
            --no-config-files)
                INSTALL_AUTO_LOAD_CONFIG="0"
                ;;
            --verify-only)
                INSTALL_VERIFY_ONLY="1"
                ;;
            --validate|--check)
                INSTALL_VALIDATE_ONLY="1"
                ;;
            -h|--help)
                usage
                exit 0
                ;;
            *)
                error "Unknown option: $1"
                usage
                exit 1
                ;;
        esac
        shift
    done
}

load_env_file() {
    local file="$1"
    local label="$2"
    local var
    local -A before=()

    [[ "${file}" == "/dev/null" ]] && return 0
    [[ -r "${file}" ]] || return 0

    # Do not let a lower-precedence source override values that are already
    # set by higher-precedence sources.
    for var in "${INPUT_HIERARCHY_VARS[@]}"; do
        if [[ -n "${!var+x}" ]]; then
            before["$var"]="${!var}"
        fi
    done

    set -a
    # shellcheck disable=SC1090
    if ! source "${file}"; then
        set +a
        error "Failed to load ${label} from ${file}. Fix shell syntax in this file and retry."
        return 1
    fi
    set +a

    for var in "${!before[@]}"; do
        declare -gx "$var=${before[$var]}"
    done

    info "Loaded ${label} from ${file}."
}

# Source an env file and allow its values to override any already-set variables.
# Used for gentian-deployments cluster-settings.env (Git source of truth for
# cluster runtime behavior).
load_env_file_override() {
    local file="$1"
    local label="$2"

    [[ "${file}" == "/dev/null" ]] && return 0
    [[ -r "${file}" ]] || return 0

    set -a
    # shellcheck disable=SC1090
    if ! source "${file}"; then
        set +a
        error "Failed to load ${label} from ${file}. Fix shell syntax in this file and retry."
        return 1
    fi
    set +a

    info "Loaded ${label} from ${file} (overrides prior values)."
}

# validate_config checks that all required environment variables are set and
# that key values pass basic format validation. Exits 0 on success, 1 on
# failure. No cluster actions are taken.
validate_config() {
    local errors=0 warnings=0
    local deployments_root cluster stage
    local cluster_settings_file

    deployments_root="${GENTIAN_DEPLOYMENTS_PATH:-${HOME}/.gentian/gentian-deployments}"
    cluster="${GENTIAN_DEPLOYMENTS_CLUSTER:-default-cluster}"
    stage="${GENTIAN_DEPLOYMENTS_STAGE:-dev}"
    cluster_settings_file="${deployments_root}/clusters/${cluster}/kernel/cluster-settings.env"

    _file_header() {
        local file="$1" role="$2"
        echo ""
        echo "━━━ ${role} ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        if [[ -r "${file}" ]]; then
            echo "  [FILE]     ${file}"
        else
            echo "  [ABSENT]   ${file}"
        fi
    }

    _req_from() {
        local var="$1" hint="$2" source_file="$3"
        if [[ -z "${!var:-}" ]]; then
            echo "  [MISSING]  ${var}  — ${hint} (set in ${source_file})"
            (( errors++ )) || true
        else
            echo "  [OK]       ${var}"
        fi
    }

    _opt_from() {
        local var="$1" hint="$2" source_file="$3"
        if [[ -z "${!var:-}" ]]; then
            echo "  [WARN]     ${var}  — not set (${hint}; set in ${source_file})"
            (( warnings++ )) || true
        else
            echo "  [OK]       ${var}"
        fi
    }

    _file_header "${INSTALL_SECRETS_FILE}" "Secrets checks (install.secrets.env)"
    _req_from MASTER_PASSWORD          "HKDF master secret — used to derive all app secrets" "${INSTALL_SECRETS_FILE}"
    _req_from OD_PRIVATE_REGISTRY_USERNAME "registry.opencode.de username" "${INSTALL_SECRETS_FILE}"
    _req_from OD_PRIVATE_REGISTRY_PASSWORD "registry.opencode.de password or token" "${INSTALL_SECRETS_FILE}"
    _req_from OD_SMTP_RELAY_USERNAME   "SMTP username (e.g. Gmail address)" "${INSTALL_SECRETS_FILE}"
    _req_from OD_SMTP_RELAY_PASSWORD   "SMTP password (e.g. Gmail App Password)" "${INSTALL_SECRETS_FILE}"
    _opt_from CF_API_TOKEN       "Cloudflare token — needed for DNS-01 wildcard certificates" "${INSTALL_SECRETS_FILE}"
    if [[ -z "${CF_ZONE_NAME:-}" ]]; then
        echo "  [OK]       CF_ZONE_NAME  (optional; derived from KERNEL_DOMAIN when unset; set override in ${INSTALL_SECRETS_FILE})"
    else
        echo "  [OK]       CF_ZONE_NAME"
    fi

    _file_header "${cluster_settings_file}" "Cluster checks (cluster-settings.env)"

    MAIL_SERVICE_MODE="${MAIL_SERVICE_MODE:-external}"
    if [[ "${MAIL_SERVICE_MODE}" != "external" && "${MAIL_SERVICE_MODE}" != "kernel" ]]; then
        echo "  [INVALID]  MAIL_SERVICE_MODE=${MAIL_SERVICE_MODE}  — must be 'external' or 'kernel' (set in ${cluster_settings_file})"
        (( errors++ )) || true
    else
        echo "  [OK]       MAIL_SERVICE_MODE=${MAIL_SERVICE_MODE}"
    fi
    if [[ "${MAIL_SERVICE_MODE}" == "external" ]]; then
        _req_from EXTERNAL_SMTP_HOST "External SMTP host (e.g. smtp.gmail.com)" "${cluster_settings_file}"
    fi

    if [[ -z "${KERNEL_DOMAIN:-}" ]]; then
        echo "  [MISSING]  KERNEL_DOMAIN  — platform-wide DNS suffix (e.g. platform.example.com; set in ${cluster_settings_file})"
        (( errors++ )) || true
    elif [[ ! "${KERNEL_DOMAIN}" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ ]]; then
        echo "  [INVALID]  KERNEL_DOMAIN=${KERNEL_DOMAIN}  — must match ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?\$ (set in ${cluster_settings_file})"
        (( errors++ )) || true
    else
        echo "  [OK]       KERNEL_DOMAIN=${KERNEL_DOMAIN}"
    fi
    TENANCY_MODE="${TENANCY_MODE:-multi}"
    if [[ "${TENANCY_MODE}" != "multi" && "${TENANCY_MODE}" != "single" ]]; then
        echo "  [INVALID]  TENANCY_MODE=${TENANCY_MODE}  — must be 'multi' or 'single' (set in ${cluster_settings_file})"
        (( errors++ )) || true
    else
        echo "  [OK]       TENANCY_MODE=${TENANCY_MODE}"
    fi

    _opt_from NETWORK_MODE  "networking mode: tunnel (default) or static-ip" "${cluster_settings_file}"
    if [[ "${NETWORK_MODE:-tunnel}" == "static-ip" ]]; then
        _req_from NODE_IP   "required in static-ip mode" "${cluster_settings_file}"
    else
        echo "  [OK]       NODE_IP  (not required for NETWORK_MODE=${NETWORK_MODE:-tunnel})"
    fi

    _file_header "${INSTALL_CONFIG_FILE}" "Installer config checks (install.env)"
    _opt_from LETSENCRYPT_EMAIL  "required for Let's Encrypt ACME; falls back to a dummy address" "${INSTALL_CONFIG_FILE}"
    _opt_from INGRESS_CLASS_NAME "defaults to 'nginx' if not set" "${INSTALL_CONFIG_FILE}"
    _opt_from GENTIAN_APPS_REPO       "defaults to https://github.com/gentian-org/gentian-apps" "${INSTALL_CONFIG_FILE}"
    _opt_from GENTIAN_APPS_BRANCH     "defaults to 'main'" "${INSTALL_CONFIG_FILE}"
    _opt_from GENTIAN_DEPLOYMENTS_REPO    "defaults to https://github.com/gentian-org/gentian-deployments" "${INSTALL_CONFIG_FILE}"
    _opt_from GENTIAN_DEPLOYMENTS_BRANCH  "defaults to 'main'" "${INSTALL_CONFIG_FILE}"

    echo ""
    if (( errors > 0 )); then
        echo -e "${RED}Result: ${errors} error(s), ${warnings} warning(s) — config is NOT ready.${NC}"
        exit 1
    elif (( warnings > 0 )); then
        echo -e "${YELLOW}Result: 0 errors, ${warnings} warning(s) — config is ready (with caveats).${NC}"
        exit 0
    else
        echo -e "${GREEN}Result: all checks passed — config is ready.${NC}"
        exit 0
    fi
}

# load_operator_config sources declarative operator-provided env files before
# cache loading and interactive prompts. This enables non-interactive installs
# driven by pre-filled templates.
load_operator_config() {
    if [[ "${INSTALL_AUTO_LOAD_CONFIG}" != "1" ]]; then
        return 0
    fi
    load_env_file "${INSTALL_CONFIG_FILE}" "installer config"
    load_env_file "${INSTALL_SECRETS_FILE}" "installer secrets"
}

# =============================================================================
# Try to load any missing credentials from a previously seeded OpenBao before
# prompting the operator. Useful on re-runs: the operator only has to provide
# secrets the first time. Silently skipped if OpenBao is not yet reachable
# (e.g. on the very first run).
# =============================================================================
try_load_creds_from_openbao() {
    # Fast path: if everything is already exported, nothing to do.
    if [[ -n "${MASTER_PASSWORD:-}" \
        && -n "${OD_PRIVATE_REGISTRY_USERNAME:-}" \
        && -n "${OD_PRIVATE_REGISTRY_PASSWORD:-}" \
        && -n "${OD_SMTP_RELAY_USERNAME:-}" \
        && -n "${OD_SMTP_RELAY_PASSWORD:-}" ]]; then
        return
    fi

    # Need a root token to read secrets. Prefer env, fall back to init file.
    local token=""
    if [[ -n "${BAO_TOKEN:-}" ]]; then
        token="$BAO_TOKEN"
    elif [[ -f "${OPENBAO_INIT_FILE}" ]]; then
        token=$(jq -r '.root_token // empty' "${OPENBAO_INIT_FILE}" 2>/dev/null || true)
    fi
    [[ -n "$token" ]] || return 0

    # Need a reachable OpenBao service. Skip silently if not yet deployed.
    local bao_ip
    bao_ip=$(kubectl get svc openbao -n openbao -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)
    [[ -n "$bao_ip" ]] || return 0
    local bao_addr="http://${bao_ip}:8200"

    # Don't bother if OpenBao is sealed/unreachable.
    curl -sf --max-time 3 "${bao_addr}/v1/sys/health" >/dev/null 2>&1 || return 0

    _bao_get() {
        # $1 = relative path under secret/data/gentian-os/kernel/
        # $2 = jq filter to extract the field, e.g. '.data.data.value'
        curl -sf --max-time 5 \
            -H "X-Vault-Token: ${token}" \
            "${bao_addr}/v1/secret/data/gentian-os/kernel/$1" 2>/dev/null \
            | jq -r "$2 // empty" 2>/dev/null
    }

    local loaded=0 v
    if [[ -z "${MASTER_PASSWORD:-}" ]]; then
        v=$(_bao_get "internal/master-password" '.data.data.value')
        [[ -n "$v" ]] && { export MASTER_PASSWORD="$v"; loaded=1; }
    fi
    if [[ -z "${OD_PRIVATE_REGISTRY_USERNAME:-}" ]]; then
        v=$(_bao_get "storage/registry" '.data.data.username')
        [[ -n "$v" ]] && { export OD_PRIVATE_REGISTRY_USERNAME="$v"; loaded=1; }
    fi
    if [[ -z "${OD_PRIVATE_REGISTRY_PASSWORD:-}" ]]; then
        v=$(_bao_get "storage/registry" '.data.data.password')
        [[ -n "$v" ]] && { export OD_PRIVATE_REGISTRY_PASSWORD="$v"; loaded=1; }
    fi
    if [[ -z "${OD_SMTP_RELAY_USERNAME:-}" ]]; then
        v=$(_bao_get "mail/postfix" '.data.data.relay_username')
        [[ -n "$v" ]] && { export OD_SMTP_RELAY_USERNAME="$v"; loaded=1; }
    fi
    if [[ -z "${OD_SMTP_RELAY_PASSWORD:-}" ]]; then
        v=$(_bao_get "mail/postfix" '.data.data.relay_password')
        [[ -n "$v" ]] && { export OD_SMTP_RELAY_PASSWORD="$v"; loaded=1; }
    fi

    if [[ "$loaded" -eq 1 ]]; then
        info "Loaded missing credentials from OpenBao."
    fi
}

# =============================================================================
# Local on-disk cache of installer credentials
# =============================================================================
load_creds_cache() {
    if [[ -r "${INSTALL_SECRETS_CACHE}" ]]; then
        load_env_file "${INSTALL_SECRETS_CACHE}" "cached credentials"
    fi
}

save_creds_cache() {
    [[ "${INSTALL_SECRETS_CACHE}" == "/dev/null" ]] && return 0
    local tmp
    tmp="$(mktemp)"
    {
        echo "# Auto-generated by install.sh — keep secret, do not commit."
        echo "# Delete to be re-prompted on next run."
        for var in MASTER_PASSWORD OD_PRIVATE_REGISTRY_USERNAME OD_PRIVATE_REGISTRY_PASSWORD \
                   OD_SMTP_RELAY_USERNAME OD_SMTP_RELAY_PASSWORD CF_API_TOKEN; do
            local val="${!var:-}"
            [[ -n "$val" ]] || continue
            # printf %q escapes safely for re-sourcing.
            printf 'export %s=%q\n' "$var" "$val"
        done
    } >"$tmp"
    install -m 0600 "$tmp" "${INSTALL_SECRETS_CACHE}"
    rm -f "$tmp"
    info "Cached credentials to ${INSTALL_SECRETS_CACHE} (chmod 600)."
}

# =============================================================================
# Prompt for any required credentials that were not pre-exported
# =============================================================================
prompt_credentials() {
    local prompted=0

    if [[ -z "${MASTER_PASSWORD:-}" ]]; then
        read -rp "  MASTER_PASSWORD (HKDF master secret): " MASTER_PASSWORD; echo ""
        export MASTER_PASSWORD
        prompted=1
    fi
    if [[ -z "${OD_PRIVATE_REGISTRY_USERNAME:-}" ]]; then
        read -rp  "  OD_PRIVATE_REGISTRY_USERNAME (registry.opencode.de): " OD_PRIVATE_REGISTRY_USERNAME; echo ""
        export OD_PRIVATE_REGISTRY_USERNAME
        prompted=1
    fi
    if [[ -z "${OD_PRIVATE_REGISTRY_PASSWORD:-}" ]]; then
        read -rp "  OD_PRIVATE_REGISTRY_PASSWORD (registry.opencode.de token): " OD_PRIVATE_REGISTRY_PASSWORD; echo ""
        export OD_PRIVATE_REGISTRY_PASSWORD
        prompted=1
    fi
    if [[ -z "${OD_SMTP_RELAY_USERNAME:-}" ]]; then
        read -rp  "  OD_SMTP_RELAY_USERNAME (e.g. user@gmail.com): " OD_SMTP_RELAY_USERNAME; echo ""
        export OD_SMTP_RELAY_USERNAME
        prompted=1
    fi
    if [[ -z "${OD_SMTP_RELAY_PASSWORD:-}" ]]; then
        read -rp "  OD_SMTP_RELAY_PASSWORD (e.g. Gmail App Password): " OD_SMTP_RELAY_PASSWORD; echo ""
        export OD_SMTP_RELAY_PASSWORD
        prompted=1
    fi

    MAIL_SERVICE_MODE="${MAIL_SERVICE_MODE:-external}"
    if [[ "${MAIL_SERVICE_MODE}" != "external" && "${MAIL_SERVICE_MODE}" != "kernel" ]]; then
        if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" ]]; then
            error "MAIL_SERVICE_MODE=${MAIL_SERVICE_MODE} invalid in non-interactive mode. Use external|kernel."
            exit 1
        fi
        read -rp "  MAIL_SERVICE_MODE [external|kernel] (default: external): " MAIL_SERVICE_MODE; echo ""
        MAIL_SERVICE_MODE="${MAIL_SERVICE_MODE:-external}"
    fi
    export MAIL_SERVICE_MODE

    if [[ "${MAIL_SERVICE_MODE}" == "external" ]]; then
        if [[ -z "${EXTERNAL_SMTP_HOST:-}" ]]; then
            read -rp "  EXTERNAL_SMTP_HOST (e.g. smtp.gmail.com): " EXTERNAL_SMTP_HOST; echo ""
            export EXTERNAL_SMTP_HOST
            prompted=1
        fi
        : "${EXTERNAL_SMTP_PORT:=587}"
        : "${EXTERNAL_SMTP_SSL:=false}"
        : "${EXTERNAL_SMTP_STARTTLS:=true}"
        export EXTERNAL_SMTP_PORT EXTERNAL_SMTP_SSL EXTERNAL_SMTP_STARTTLS
    fi

    if [[ "$prompted" -eq 1 ]]; then
        echo ""
        save_creds_cache
        save_install_state
    fi
}

# =============================================================================
# Prompt for the gentian-apps and gentian-deployments repo URLs/branches.
# Defaults point at the upstream gentian-org repos. Persist results to
# ~/.gentian/config (bash-sourceable) so the kubectl-gentian plugin can locate
# the deployments repo when running `kubectl gentian apps install/uninstall`.
# =============================================================================
prompt_app_repos() {
    local default_apps_repo="https://github.com/gentian-org/gentian-apps"
    local default_apps_branch="main"
    local default_deploy_repo="https://github.com/gentian-org/gentian-deployments"
    local default_deploy_branch="main"
    local default_deploy_cluster="default-cluster"
    local default_deploy_stage="${ENV:-dev}"
    local v

    if [[ -z "${GENTIAN_APPS_REPO:-}" ]]; then
        if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" ]]; then
            GENTIAN_APPS_REPO="${default_apps_repo}"
        else
            echo ""
            info "App catalogue and deployment repositories (missing values only):"
            read -rp "  gentian-apps repo URL [${default_apps_repo}]: " v
            GENTIAN_APPS_REPO="${v:-${default_apps_repo}}"
        fi
    fi

    if [[ -z "${GENTIAN_APPS_BRANCH:-}" ]]; then
        if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" ]]; then
            GENTIAN_APPS_BRANCH="${default_apps_branch}"
        else
            read -rp "  gentian-apps branch [${default_apps_branch}]: " v
            GENTIAN_APPS_BRANCH="${v:-${default_apps_branch}}"
        fi
    fi

    if [[ -z "${GENTIAN_DEPLOYMENTS_REPO:-}" ]]; then
        if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" ]]; then
            GENTIAN_DEPLOYMENTS_REPO="${default_deploy_repo}"
        else
            read -rp "  gentian-deployments repo URL [${default_deploy_repo}]: " v
            GENTIAN_DEPLOYMENTS_REPO="${v:-${default_deploy_repo}}"
        fi
    fi

    if [[ -z "${GENTIAN_DEPLOYMENTS_BRANCH:-}" ]]; then
        if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" ]]; then
            GENTIAN_DEPLOYMENTS_BRANCH="${default_deploy_branch}"
        else
            read -rp "  gentian-deployments branch [${default_deploy_branch}]: " v
            GENTIAN_DEPLOYMENTS_BRANCH="${v:-${default_deploy_branch}}"
        fi
    fi

    if [[ -z "${GENTIAN_DEPLOYMENTS_CLUSTER:-}" ]]; then
        if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" ]]; then
            GENTIAN_DEPLOYMENTS_CLUSTER="${default_deploy_cluster}"
        else
            read -rp "  gentian-deployments cluster path segment [${default_deploy_cluster}]: " v
            GENTIAN_DEPLOYMENTS_CLUSTER="${v:-${default_deploy_cluster}}"
        fi
    fi

    if [[ -z "${GENTIAN_DEPLOYMENTS_STAGE:-}" ]]; then
        if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" ]]; then
            GENTIAN_DEPLOYMENTS_STAGE="${default_deploy_stage}"
        else
            read -rp "  gentian-deployments stage [${default_deploy_stage}]: " v
            GENTIAN_DEPLOYMENTS_STAGE="${v:-${default_deploy_stage}}"
        fi
    fi

    : "${GENTIAN_APPS_REPO:=${default_apps_repo}}"
    : "${GENTIAN_APPS_BRANCH:=${default_apps_branch}}"
    : "${GENTIAN_DEPLOYMENTS_REPO:=${default_deploy_repo}}"
    : "${GENTIAN_DEPLOYMENTS_BRANCH:=${default_deploy_branch}}"
        : "${GENTIAN_DEPLOYMENTS_CLUSTER:=${default_deploy_cluster}}"
        : "${GENTIAN_DEPLOYMENTS_STAGE:=${default_deploy_stage}}"
    : "${GENTIAN_DEPLOYMENTS_PATH:=${HOME}/.gentian/gentian-deployments}"
    export GENTIAN_APPS_REPO GENTIAN_APPS_BRANCH \
            GENTIAN_DEPLOYMENTS_REPO GENTIAN_DEPLOYMENTS_BRANCH \
            GENTIAN_DEPLOYMENTS_CLUSTER GENTIAN_DEPLOYMENTS_STAGE \
            GENTIAN_DEPLOYMENTS_PATH

    # Persist to ~/.gentian/config (bash-sourceable) so kubectl-gentian can read it.
    # The plugin sources this file directly; keep variable names aligned with the
    # plugin's expectations (GENTIAN_DEPLOYMENTS_PATH / GENTIAN_DEPLOYMENTS_REPO).
    local cfg_dir="${HOME}/.gentian"
    local cfg_file="${cfg_dir}/config"
    mkdir -p "$cfg_dir"
    cat >"$cfg_file" <<EOF
# Auto-generated by gentian-os/install.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ).
# Sourced by the kubectl-gentian plugin (~/.gentian/config).
GENTIAN_APPS_REPO="${GENTIAN_APPS_REPO}"
GENTIAN_APPS_BRANCH="${GENTIAN_APPS_BRANCH}"
GENTIAN_DEPLOYMENTS_REPO="${GENTIAN_DEPLOYMENTS_REPO}"
GENTIAN_DEPLOYMENTS_BRANCH="${GENTIAN_DEPLOYMENTS_BRANCH}"
GENTIAN_DEPLOYMENTS_CLUSTER="${GENTIAN_DEPLOYMENTS_CLUSTER}"
GENTIAN_DEPLOYMENTS_STAGE="${GENTIAN_DEPLOYMENTS_STAGE}"
GENTIAN_DEPLOYMENTS_PATH="${GENTIAN_DEPLOYMENTS_PATH}"
EOF
    chmod 0600 "$cfg_file"
    success "App repo configuration saved to ${cfg_file}"
}

# =============================================================================
# Persist installer-local state across re-runs (.install-state.env).
# Cluster runtime settings are NOT stored here — they live in gentian-deployments
# clusters/<cluster>/kernel/cluster-settings.env.
# =============================================================================
load_install_state() {
    if [[ -r "${INSTALL_STATE_FILE}" ]]; then
        load_env_file "${INSTALL_STATE_FILE}" "installer state"
    fi
}

save_install_state() {
    [[ "${INSTALL_STATE_FILE}" == "/dev/null" ]] && return 0
    local tmp
    local val
    tmp="$(mktemp)"
    {
        echo "# Auto-generated by install.sh — installer-local state only."
        echo "# Cluster runtime settings: gentian-deployments/clusters/<cluster>/kernel/cluster-settings.env"
        echo "# Delete this file to reset installer-local caches."
        val="${GENTIAN_MANAGED_CERT_MANAGER:-}"
        [[ -n "$val" ]] && printf 'export GENTIAN_MANAGED_CERT_MANAGER=%q\n' "$val"
        val="${INSTALL_START_EPOCH:-}"
        [[ -n "$val" ]] && printf 'export INSTALL_START_EPOCH=%q\n' "$val"
    } >"$tmp"
    install -m 0644 "$tmp" "${INSTALL_STATE_FILE}"
    rm -f "$tmp"
    info "Saved installer state to ${INSTALL_STATE_FILE}."
}

# =============================================================================
# Load cluster-scoped non-secret settings from gentian-deployments checkout.
# File path convention:
#   ${GENTIAN_DEPLOYMENTS_PATH}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER}/kernel/cluster-settings.env
# =============================================================================
load_deployments_cluster_settings() {
    : "${GENTIAN_DEPLOYMENTS_PATH:=${HOME}/.gentian/gentian-deployments}"

    # Local developer layout often checks out sibling repos under the same
    # parent directory (../gentian-deployments). Prefer that path when the
    # default cache location does not exist.
    if [[ ! -d "${GENTIAN_DEPLOYMENTS_PATH}" ]]; then
        local sibling_repo
        sibling_repo="$(cd "${SCRIPT_DIR}/.." && pwd)/gentian-deployments"
        if [[ -d "${sibling_repo}" ]]; then
            GENTIAN_DEPLOYMENTS_PATH="${sibling_repo}"
            export GENTIAN_DEPLOYMENTS_PATH
            info "Using sibling deployments repo at ${GENTIAN_DEPLOYMENTS_PATH}."
        fi
    fi

    local cluster="${GENTIAN_DEPLOYMENTS_CLUSTER:-default-cluster}"
    local settings_file="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${cluster}/kernel/cluster-settings.env"

    if [[ -r "${settings_file}" ]]; then
        load_env_file_override "${settings_file}" "deployments cluster settings"
    else
        info "No deployments cluster settings file found at ${settings_file} (optional)."
    fi
}

# =============================================================================
# Prompt for the cluster's kernel domain (the single platform-wide domain on
# which all kernel UIs — Keycloak, Nubus, Argo CD, Intercom — are served, and
# which provides the tenant app zone fallback when Tenant.spec.domain is unset
# (shape depends on TENANCY_MODE — see docs/design/multi-tenancy.md §3).
#
# Persisted via cluster-settings.env in gentian-deployments when set there;
# prompts only run when the value is still missing after load_deployments_cluster_settings.
prompt_kernel_domain() {
    if [[ -n "${KERNEL_DOMAIN:-}" ]]; then
        info "Using KERNEL_DOMAIN=${KERNEL_DOMAIN}"
        export KERNEL_DOMAIN
        return
    fi

    if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" ]]; then
        error "KERNEL_DOMAIN is not set and GENTIAN_NONINTERACTIVE=1; aborting."
        error "Export KERNEL_DOMAIN=<your-domain> and re-run."
        exit 1
    fi

    echo ""
    info "Kernel domain (single platform-wide DNS suffix used for all kernel UIs"
    info "and as the default base for tenant apps without a vanity domain):"
    info "  examples: platform.example.com, desk.example.org"

    local v=""
    while [[ -z "$v" ]]; do
        read -rp "  KERNEL_DOMAIN: " v
        if [[ ! "$v" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ ]]; then
            warn "Invalid domain: must match ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$"
            v=""
        fi
    done
    export KERNEL_DOMAIN="$v"
    save_install_state
}

# =============================================================================
# Prompt for the cluster networking mode.
#
#   tunnel    — cluster sits behind a reverse-proxy/tunnel (e.g. Cloudflare
#               Tunnel). DNS *.${KERNEL_DOMAIN} resolves to the tunnel
#               endpoint. NODE_IP is optional. DNS-01 ACME is preferred for
#               wildcard certificates.
#   static-ip — node has a public/reachable static IP. DNS *.${KERNEL_DOMAIN}
#               points directly to NODE_IP, which must be set and valid.
#               MetalLB (or equivalent) must be pre-configured. HTTP-01 ACME
#               challenges work without Cloudflare.
#
# Persisted to ${INSTALL_STATE_FILE} so re-runs do not re-prompt.
# =============================================================================
prompt_network_mode() {
    if [[ -n "${NETWORK_MODE:-}" ]]; then
        if [[ "${NETWORK_MODE}" != "tunnel" && "${NETWORK_MODE}" != "static-ip" ]]; then
            error "NETWORK_MODE=${NETWORK_MODE} is invalid. Must be 'tunnel' or 'static-ip'."
            exit 1
        fi
        info "Using NETWORK_MODE=${NETWORK_MODE}"
        export NETWORK_MODE
        return
    fi

    if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" ]]; then
        NETWORK_MODE="tunnel"
        info "NETWORK_MODE not set; defaulting to tunnel (non-interactive)."
        export NETWORK_MODE
        save_install_state
        return
    fi

    echo ""
    info "Networking mode — how external traffic reaches this cluster:"
    info "  tunnel    : cluster is behind a tunnel/proxy (e.g. Cloudflare Tunnel)"
    info "  static-ip : cluster node has a public reachable static IP"
    local v
    while true; do
        read -rp "  NETWORK_MODE [tunnel|static-ip] (default: tunnel): " v
        v="${v:-tunnel}"
        if [[ "$v" == "tunnel" || "$v" == "static-ip" ]]; then
            break
        fi
        warn "Invalid value '${v}'. Enter 'tunnel' or 'static-ip'."
    done
    export NETWORK_MODE="$v"
    save_install_state
}

# =============================================================================
# Prompt for kernel-level secrets that depend on KERNEL_DOMAIN. Currently:
#
#   - CF_API_TOKEN: Cloudflare API token with Zone:Read + DNS:Edit on the
#     KERNEL_DOMAIN's zone. Used to solve DNS-01 ACME challenges for the
#     kernel wildcard *.${KERNEL_DOMAIN} (platform UIs only). Tenant apps
#     use per-tenant DNS-01 wildcards via TENANT_DNS01_CLUSTER_ISSUER.
#     Optional — see docs/design/multi-tenancy.md §3.
#
# Persisted in ${INSTALL_SECRETS_CACHE} alongside the other credentials.
# =============================================================================

# Derive the apex zone from a hostname (last two labels). Good enough for
# normal TLDs like example.org; users with compound TLDs (e.g. co.uk) can
# override by exporting CF_ZONE_NAME before running install.sh.
_derive_zone_from_domain() {
    local d="$1"
    if [[ -n "${CF_ZONE_NAME:-}" ]]; then
        echo "$CF_ZONE_NAME"
        return
    fi
    echo "$d" | awk -F. '{n=NF; print $(n-1)"."$n}'
}

# Verify a Cloudflare API token has Zone:Read on the apex zone of
# KERNEL_DOMAIN by querying /zones?name=<apex>. Notes:
#   - The /user/tokens/verify endpoint rejects some valid scoped-token
#     formats (e.g. cfat_ prefix) with a false-negative, so we hit the
#     actual zone API instead.
#   - DNS:Edit is not directly testable read-only; if Zone:Read works on
#     the right zone, that's the strongest signal we can get without
#     mutating state.
# Returns 0 on success, 1 on any failure. Prints diagnostics either way.
verify_cloudflare_token() {
    local token="$1"
    local domain="$2"
    local zone
    zone=$(_derive_zone_from_domain "$domain")

    info "Verifying Cloudflare token can read zone ${zone}..."
    local resp
    if ! resp=$(curl -sS --max-time 10 \
            -H "Authorization: Bearer ${token}" \
            "https://api.cloudflare.com/client/v4/zones?name=${zone}" 2>&1); then
        warn "Cloudflare API call failed: ${resp}"
        return 1
    fi

    local ok count
    ok=$(echo "$resp" | jq -r '.success // false' 2>/dev/null)
    # Cloudflare may return result_count as null on some accounts/tokens.
    # Fall back to the actual array length so we don't produce false negatives.
    count=$(echo "$resp" | jq -r 'if (.result_count // null) == null then (.result | length) else .result_count end' 2>/dev/null)

    if [[ "$ok" != "true" ]]; then
        local err
        err=$(echo "$resp" | jq -r '.errors[]? | "[\(.code)] \(.message)"' 2>/dev/null | head -3)
        warn "Cloudflare API rejected token: ${err:-unknown error}"
        return 1
    fi
    if [[ "$count" == "0" ]]; then
        warn "Token authenticated, but has no access to zone ${zone}."
        warn "  → grant Zone:Read + DNS:Edit on ${zone} (or set CF_ZONE_NAME)."
        return 1
    fi

    local zid
    zid=$(echo "$resp" | jq -r '.result[0].id')
    success "Cloudflare token verified (zone=${zone}, id=${zid})."
    return 0
}

prompt_kernel_secrets() {
    if [[ -n "${CF_API_TOKEN:-}" ]]; then
        info "Using cached CF_API_TOKEN (Cloudflare DNS-01 enabled)."
        export CF_API_TOKEN
        if ! verify_cloudflare_token "$CF_API_TOKEN" "$KERNEL_DOMAIN"; then
            warn "Cached Cloudflare token failed verification — wildcard issuance"
            warn "will likely fail. Re-run with CF_API_TOKEN= to clear and re-prompt."
        fi
        return
    fi
    if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" ]]; then
        info "CF_API_TOKEN not set; skipping kernel wildcard (non-interactive)."
        return
    fi

    echo ""
    info "Cloudflare API token for kernel wildcard *.${KERNEL_DOMAIN} (optional)."
    info "Leave empty to skip the kernel wildcard (tenant per-zone certs still need DNS-01)."
    info "Token requires Zone:Read + DNS:Edit on the ${KERNEL_DOMAIN} zone."

    # Loop: re-prompt up to 3 times on verification failure. User can always
    # type empty to skip the wildcard entirely.
    local attempt v
    for attempt in 1 2 3; do
        v=""
        read -rp "  CF_API_TOKEN [empty=skip]: " v
        if [[ -z "$v" ]]; then
            warn "No Cloudflare token provided; kernel wildcard Certificate will be skipped."
            return
        fi
        if verify_cloudflare_token "$v" "$KERNEL_DOMAIN"; then
            export CF_API_TOKEN="$v"
            save_creds_cache
            success "Cloudflare token captured (will be stored in OpenBao)."
            return
        fi
        if [[ $attempt -lt 3 ]]; then
            warn "Token verification failed (attempt ${attempt}/3) — try again, or empty to skip."
        fi
    done
    warn "Three verification failures; skipping kernel wildcard Certificate."
    warn "Re-run install.sh later with a valid token to enable the wildcard."
}

# =============================================================================
# 0. Pre-flight checks
# =============================================================================
check_prereqs() {
    banner "Pre-flight checks"

    local missing=0

    # ── CLI tools ─────────────────────────────────────────────────────────────
    local base_tools=(kubectl helm jq openssl curl bao)
    # Crossplane-based installer also needs the crossplane CLI and python3.
    local extra_tools=()
    [[ "${CROSSPLANE_MODE:-0}" == "1" ]] && extra_tools=(crossplane python3)

    for cmd in "${base_tools[@]}" "${extra_tools[@]}"; do
        if ! command -v "$cmd" &>/dev/null; then
            error "Required command not found: $cmd"
            missing=$((missing + 1))
        else
            success "$cmd found"
        fi
    done

    # ── Cluster connectivity ──────────────────────────────────────────────────
    if ! kubectl cluster-info --request-timeout=5s >/dev/null 2>&1; then
        error "No reachable Kubernetes cluster — set KUBECONFIG or connect to your cluster first."
        missing=$((missing + 1))
    else
        success "cluster reachable (context: $(kubectl config current-context 2>/dev/null || echo unknown))"
    fi

    # ── MicroK8s kubelet max-pods ─────────────────────────────────────────────
    # Gentian OS runs many pods (100+). The microk8s default of --max-pods=110
    # is too low and causes silent scheduling failures once the limit is reached.
    # If this is a microk8s cluster and the limit is below 220, increase it now
    # and restart microk8s so the new limit takes effect before any workloads
    # are deployed. This is idempotent.
    local kubelet_args_file="/var/snap/microk8s/current/args/kubelet"
    if [[ -f "${kubelet_args_file}" ]]; then
        local cur_max_pods
        cur_max_pods=$(grep -E '^--max-pods=' "${kubelet_args_file}" | cut -d= -f2 || true)
        cur_max_pods=${cur_max_pods:-110}
        local target_max_pods=220
        if (( cur_max_pods < target_max_pods )); then
            info "microk8s kubelet max-pods=${cur_max_pods} is below ${target_max_pods}; updating to ${target_max_pods}..."
            sudo sed -i '/^--max-pods=/d' "${kubelet_args_file}"
            echo "--max-pods=${target_max_pods}" | sudo tee -a "${kubelet_args_file}" >/dev/null
            info "Restarting microk8s to apply new max-pods limit (this takes ~30 s)..."
            sudo microk8s stop
            sudo microk8s start
            if kubectl wait node --all --for=condition=Ready --timeout=120s; then
                success "microk8s max-pods updated to ${target_max_pods} and cluster is Ready."
            else
                warn "Cluster not Ready after 120 s — continuing anyway."
            fi
        else
            success "microk8s max-pods=${cur_max_pods} (≥ ${target_max_pods}, ok)"
        fi
    fi

    # ── Required environment variables ───────────────────────────────────────
    for var in MASTER_PASSWORD OD_PRIVATE_REGISTRY_USERNAME OD_PRIVATE_REGISTRY_PASSWORD \
               OD_SMTP_RELAY_USERNAME OD_SMTP_RELAY_PASSWORD; do
        if [[ -z "${!var:-}" ]]; then
            error "$var is not set"
            missing=$((missing + 1))
        else
            success "$var set"
        fi
    done

    MAIL_SERVICE_MODE="${MAIL_SERVICE_MODE:-external}"
    export MAIL_SERVICE_MODE
    if [[ "${MAIL_SERVICE_MODE}" == "external" && -z "${EXTERNAL_SMTP_HOST:-}" ]]; then
        error "EXTERNAL_SMTP_HOST is required when MAIL_SERVICE_MODE=external"
        missing=$((missing + 1))
    fi

    if [[ "$missing" -gt 0 ]]; then
        error "$missing prerequisite(s) missing. Aborting."
        exit 1
    fi

    if [[ -z "${NODE_IP:-}" ]]; then
        NODE_IP=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
        info "Auto-detected NODE_IP: $NODE_IP"
    fi
    export NODE_IP

    # In static-ip mode NODE_IP must be a real, non-documentation address.
    if [[ "${NETWORK_MODE:-tunnel}" == "static-ip" ]]; then
        if _is_testnet_ip "${NODE_IP}"; then
            error "NETWORK_MODE=static-ip requires a real NODE_IP, but NODE_IP=${NODE_IP} looks like a documentation/testnet address."
            error "Set NODE_IP to the cluster node's actual public or reachable IP and re-run."
            exit 1
        fi
        # Check that the edge LoadBalancer service has an external IP.
        local lb_ip lb_label
        if [[ "${ROUTING_MODE:-ingress}" == "gateway" ]]; then
            lb_label='app.kubernetes.io/name=gateway-helm'
            lb_ip=$(kubectl get svc -A -l "${lb_label}" \
                -o jsonpath='{.items[0].status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
        else
            lb_ip=$(kubectl get svc -A -l 'app.kubernetes.io/name=ingress-nginx' \
                -o jsonpath='{.items[0].status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
        fi
        if [[ -z "$lb_ip" ]]; then
            if [[ "${ROUTING_MODE:-ingress}" == "gateway" ]]; then
                warn "NETWORK_MODE=static-ip: Envoy Gateway LoadBalancer has no external IP yet."
            else
                warn "NETWORK_MODE=static-ip: ingress LoadBalancer has no external IP yet."
            fi
            warn "  Make sure MetalLB (or a cloud LB) is configured with ${NODE_IP} before traffic can reach the cluster."
        else
            info "Ingress LoadBalancer external IP: ${lb_ip}"
        fi
    fi

    success "All pre-flight checks passed."
}

# =============================================================================
# upsert_gentian_cluster_config — cluster-wide ConfigMap for Crossplane / apps
# =============================================================================
# Idempotent. Used by install.sh (after Cluster XR Ready) and update.sh
# (--crossplane / --all) so day-2 runs pick up node.ip and LDAP endpoints.
upsert_gentian_cluster_config() {
    if [[ -z "${NODE_IP:-}" ]]; then
        NODE_IP=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null || true)
        if [[ -n "${NODE_IP}" ]]; then
            info "Auto-detected NODE_IP: ${NODE_IP}"
        fi
    fi
    export NODE_IP

    local _ldap_server="${LDAP_SERVER:-nubus-${ENV:-dev}-ldap-server.${SERVICES_NAMESPACE:-gentian-dev}.svc.cluster.local}"
    local _udm_url="http://nubus-${ENV:-dev}-udm-rest-api.${SERVICES_NAMESPACE:-gentian-dev}.svc.cluster.local"
    local _minio_endpoint="${MINIO_ENDPOINT:-http://minio-${ENV:-dev}.gentian-infra-${ENV:-dev}.svc.cluster.local:9000}"
    local _cnpg_host="${CNPG_HOST:-postgres-rw.platform-kernel.svc.cluster.local}"
    local _storage_class="${STORAGE_CLASS:-}"
    local _mail_mode="${MAIL_SERVICE_MODE:-external}"

    info "Upserting gentian-cluster-config (ldap.server=${_ldap_server}, node.ip=${NODE_IP:-<unset>})..."
    kubectl apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: gentian-cluster-config
  namespace: crossplane-system
  labels:
    app.kubernetes.io/managed-by: gentian-os-install
    gentianos.io/config-type: cluster-config
data:
  ldap.server: "${_ldap_server}"
  ldap.baseDn: "${LDAP_BASE_DN:-}"
  udm.url: "${_udm_url}"
  minio.endpoint: "${_minio_endpoint}"
  cnpg.host: "${_cnpg_host}"
  storageClass: "${_storage_class}"
  mail.serviceMode: "${_mail_mode}"
  secretMode: "${SECRET_MODE:-derived}"
  node.ip: "${NODE_IP:-}"
  appInit.image: "ghcr.io/gentian-org/gentian-app-init:${APP_INIT_IMAGE_TAG:-develop}"
EOF
    success "gentian-cluster-config ConfigMap upserted."
}

# upsert_gentian_jitsi_oidc_overlays_configmap — cluster-wide Jitsi OIDC/JWT file
# overlays consumed by app-default composition (portal iframe SSO, kernel IdP broker).
upsert_gentian_jitsi_oidc_overlays_configmap() {
    local overlay_dir="${SCRIPT_DIR}/overlays/jitsi"
    if [[ ! -d "${overlay_dir}" ]]; then
        warn "Jitsi OIDC overlay directory missing (${overlay_dir}); skipping"
        return
    fi

    info "Upserting gentian-jitsi-oidc-overlays ConfigMap..."
    kubectl create configmap gentian-jitsi-oidc-overlays \
        -n crossplane-system \
        --from-file="${overlay_dir}" \
        --dry-run=client -o yaml \
        | kubectl label --local -f - \
            gentianos.io/config-type=jitsi-oidc-overlays \
            app.kubernetes.io/managed-by=gentian-os-install \
            --dry-run=client -o yaml \
        | kubectl apply -f - >/dev/null
    success "gentian-jitsi-oidc-overlays ConfigMap upserted."
}

# =============================================================================
# Crossplane platform compositions (not per-AppProfile)
# =============================================================================
# Tenant apps (jitsi, cryptpad, …) are AppProfile CRs from gentian-apps; they use
# one of these composition *variants* via spec.compositionRef (default: app-default).
# Adding a new AppProfile does not require editing install/update/uninstall unless
# you introduce a new variant file matching app-<name>.yaml in crossplane/compositions/.

apply_crossplane_app_compositions() {
    local comp_dir="${SCRIPT_DIR}/crossplane/compositions"
    local f
    shopt -s nullglob
    for f in "${comp_dir}"/app-*.yaml; do
        info "Applying Composition $(basename "${f}")..."
        kubectl apply -f "${f}"
    done
    shopt -u nullglob
}

apply_crossplane_platform_compositions() {
    info "Applying Composition (cluster-default)..."
    kubectl apply -f "${SCRIPT_DIR}/crossplane/compositions/cluster-default.yaml"
    apply_crossplane_app_compositions
    info "Applying Composition (tenant-default)..."
    kubectl apply -f "${SCRIPT_DIR}/crossplane/compositions/tenant-default.yaml"
}

apply_crossplane_platform_compositions_update() {
    # Day-2: apply every composition YAML in the directory (including any new app-* variant).
    local f
    shopt -s nullglob
    for f in "${SCRIPT_DIR}"/crossplane/compositions/*.yaml; do
        info "Applying Composition $(basename "${f}")..."
        kubectl apply -f "${f}"
    done
    shopt -u nullglob
}

delete_crossplane_compositions() {
    if ! kubectl get crd compositions.apiextensions.crossplane.io >/dev/null 2>&1; then
        info "  Composition CRD absent; skipping Composition deletion."
        return
    fi
    local f
    shopt -s nullglob
    for f in "${SCRIPT_DIR}"/crossplane/compositions/*.yaml; do
        kubectl delete -f "${f}" --ignore-not-found=true 2>/dev/null || true
        success "  Removed: $(basename "${f}")"
    done
    shopt -u nullglob
}

# Collect Helm Release CR names from kernel/services manifests (Pattern B kernel
# charts). Tenant app Releases are owned by App XRs and are removed with Tenant CRs.
_collect_kernel_helm_release_names() {
    local env="$1"
    local -n _outvar="$2"
    _outvar=()
    local release_file name
    while IFS= read -r -d '' release_file; do
        while IFS= read -r name; do
            [[ -n "${name}" ]] && _outvar+=("${name}")
        done < <(awk '
            /^kind: Release/ { in_release=1 }
            in_release && /^  name:/ { print $2; in_release=0 }
            /^---/ { in_release=0 }
        ' "${release_file}")
    done < <(find "${SCRIPT_DIR}/kernel/services" \
        -name "release.yaml" \
        -path "*/${env}/*" \
        -print0 2>/dev/null | sort -z)
}

_delete_helm_release_cr() {
    local release_name="$1"
    if ! kubectl get release.helm.crossplane.io/"${release_name}" >/dev/null 2>&1; then
        return 0
    fi
    info "Deleting provider-helm Release ${release_name}..."
    kubectl delete release.helm.crossplane.io/"${release_name}" --timeout=60s 2>/dev/null || true
    local local_deadline=$((SECONDS + 180))
    while kubectl get release.helm.crossplane.io/"${release_name}" >/dev/null 2>&1; do
        if (( SECONDS > local_deadline )); then
            warn "Release ${release_name} still present after 3m — forcing finalizer removal."
            kubectl patch release.helm.crossplane.io/"${release_name}" \
                --type=json -p='[{"op":"remove","path":"/metadata/finalizers"}]' \
                2>/dev/null || true
            break
        fi
        sleep 5
    done
    success "Release ${release_name} removed."
}

# Delete Pattern B kernel Releases declared in kernel/services (reverse install order).
delete_kernel_helm_releases() {
    local env="${1:-dev}"
    local -a release_names=()
    _collect_kernel_helm_release_names "${env}" release_names
    if [[ ${#release_names[@]} -eq 0 ]]; then
        info "No kernel Helm Release names found under kernel/services/*/${env}/"
        return
    fi
    local i
    for (( i=${#release_names[@]}-1; i>=0; i-- )); do
        _delete_helm_release_cr "${release_names[i]}"
    done
}

# =============================================================================
# 1. Install CLI tools
# =============================================================================
install_tools() {
    if [[ "${SKIP_TOOLS:-0}" == "1" ]]; then
        warn "SKIP_TOOLS=1 — skipping CLI tool installation."
        return
    fi

    banner "Step 1 — Installing CLI tools"

    if command -v tofu &>/dev/null && tofu version 2>/dev/null | grep -q "$TOFU_VERSION"; then
        success "tofu $TOFU_VERSION already installed."
    else
        info "Installing OpenTofu v${TOFU_VERSION}..."
        local arch; arch=$(uname -m); [[ "$arch" == "x86_64" ]] && arch="amd64"
        local pkg="tofu_${TOFU_VERSION}_linux_${arch}.deb"
        curl -fsSL "https://github.com/opentofu/opentofu/releases/download/v${TOFU_VERSION}/${pkg}" -o "/tmp/${pkg}"
        sudo dpkg -i "/tmp/${pkg}"
        rm -f "/tmp/${pkg}"
        success "tofu $TOFU_VERSION installed."
    fi

    if command -v bao &>/dev/null && bao version 2>/dev/null | grep -q "$BAO_VERSION"; then
        success "bao $BAO_VERSION already installed."
    else
        info "Installing OpenBao CLI v${BAO_VERSION}..."
        local arch; arch=$(uname -m); [[ "$arch" == "x86_64" ]] && arch="amd64"
        local pkg="openbao_${BAO_VERSION}_linux_${arch}.deb"
        curl -fsSL "https://github.com/openbao/openbao/releases/download/v${BAO_VERSION}/${pkg}" -o "/tmp/${pkg}"
        sudo dpkg -i "/tmp/${pkg}"
        rm -f "/tmp/${pkg}"
        success "bao $BAO_VERSION installed."
    fi
}

# =============================================================================
# 2. Create namespaces (idempotent)
# =============================================================================
create_namespaces() {
    banner "Step 2 — Creating namespaces"

    local namespaces=(openbao external-secrets argocd gentian-system platform-kernel)
    if [[ "$INSTALL_CLUSTER_INFRA" == "1" ]]; then
        namespaces+=(stakater-system cnpg-system cert-manager)
        if [[ "${ROUTING_MODE:-ingress}" == "gateway" ]]; then
            namespaces+=("${ENVOY_GATEWAY_NAMESPACE}")
        fi
    fi

    for ns in "${namespaces[@]}"; do
        if kubectl get namespace "$ns" &>/dev/null; then
            success "Namespace $ns already exists."
        else
            kubectl create namespace "$ns"
            success "Namespace $ns created."
        fi
    done
}

# =============================================================================
# 2b. Pre-warm cluster (distro-agnostic PLEG/CRI-race mitigation)
# =============================================================================
# On a freshly-bootstrapped cluster, the very first workload pod often races
# against kubelet's Pod Lifecycle Event Generator (PLEG) and containerd's
# event-subscription init. Symptom: pod stuck in ContainerCreating with no IP
# even though containerd has the sandbox RUNNING and CNI assigned an address.
# This is well-known across containerd-based distros (kind, k3d, microk8s,
# kubeadm) and is what install.sh's auto-recovery was designed to clean up
# after the fact.
#
# Pre-warming with a throwaway pod forces one full successful pod-create cycle
# through kubelet+containerd+CNI BEFORE any real workload is deployed. By the
# time cert-manager / ESO / ArgoCD pods land, the kubelet<->containerd event
# pipe is fully wired and the race is gone.
#
# This is the same trick kind/k3d use internally and is purely kubectl-based,
# so it works on any conformant cluster.
prewarm_cluster() {
    if [[ "${SKIP_PREWARM:-0}" == "1" ]]; then
        warn "Pre-warm disabled via SKIP_PREWARM=1; skipping."
        return
    fi

    banner "Step 2b — Pre-warming cluster (PLEG/CRI race mitigation)"

    # We pre-warm TWO things, in order, because they are independent races:
    #
    #   (a) volumeless pod  → exercises kubelet ↔ containerd ↔ CNI event pipe
    #                         (fixes the classic PLEG/CRI race that kind/k3d
    #                         work around the same way).
    #   (b) pod + hostpath PVC → exercises kubelet's volume_manager + the
    #                         microk8s.io/hostpath provisioner. On microk8s
    #                         specifically, the FIRST pod to bind a hostpath
    #                         PVC after a fresh cluster / kubelite restart
    #                         frequently wedges in ContainerCreating with
    #                         the container `Started` but PodReadyToStart-
    #                         Containers stuck False (kubelet status-sync
    #                         hang on first volume attach). Burning that
    #                         race on a throwaway pod here means
    #                         openbao-transit-0 (the first real PVC
    #                         consumer) lands cleanly.
    #
    # Both pre-warm pods get auto-recovery if they wedge, so worst case we
    # spend ~60s on cleanup at install time instead of 240s of
    # ContainerCreating later.

    local stamp pod_no_vol pod_pvc pvc_name
    stamp="$(date +%s)"
    pod_no_vol="prewarm-${stamp}"
    pod_pvc="prewarm-pvc-${stamp}"
    pvc_name="prewarm-pvc-${stamp}"

    # ── (a) volumeless pre-warm ─────────────────────────────────────────────
    info "Applying throwaway pod kube-system/${pod_no_vol}..."
    kubectl apply -f - <<EOF >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: ${pod_no_vol}
  namespace: kube-system
  labels:
    app.kubernetes.io/managed-by: gentian-install
    app.kubernetes.io/component: prewarm
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
    - name: prewarm
      image: busybox:1.36
      imagePullPolicy: IfNotPresent
      command: ["sh","-c","exit 0"]
      resources:
        requests: {cpu: "10m", memory: "8Mi"}
        limits:   {cpu: "50m", memory: "32Mi"}
EOF
    _wait_prewarm_pod "${pod_no_vol}" 180 "volumeless"

    # ── (b) PVC-backed pre-warm ─────────────────────────────────────────────
    # The volume_manager race we're mitigating is specific to node-local
    # provisioners (microk8s.io/hostpath, rancher.io/local-path, etc.):
    # the FIRST pod to bind one of these PVCs on a freshly-started kubelet
    # wedges in ContainerCreating because kubelet's volume_manager lags
    # the CRI sandbox. Network provisioners like nfs.csi.k8s.io do NOT
    # exhibit this — they use a separate attach/mount path. So we do NOT
    # prewarm the *default* SC; we prewarm every hostpath/local-path SC
    # we can find (typically just one, microk8s-hostpath). On clusters
    # that have no such SC (pure NFS / cloud CSI), we skip silently.
    local racy_scs sc
    racy_scs=$(kubectl get sc -o jsonpath='{range .items[*]}{.metadata.name}{"="}{.provisioner}{"\n"}{end}' 2>/dev/null \
        | awk -F= 'tolower($2) ~ /hostpath|local-path/ {print $1}')
    if [[ -z "${racy_scs}" ]]; then
        info "No hostpath/local-path StorageClass detected; skipping PVC pre-warm."
    else
        local idx=0
        while IFS= read -r sc; do
            [[ -z "${sc}" ]] && continue
            idx=$((idx + 1))
            local pod_n="${pod_pvc}-${idx}"
            local pvc_n="${pvc_name}-${idx}"
            info "Applying throwaway PVC + pod kube-system/${pod_n} (sc=${sc})..."
            kubectl apply -f - <<EOF >/dev/null
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${pvc_n}
  namespace: kube-system
  labels:
    app.kubernetes.io/managed-by: gentian-install
    app.kubernetes.io/component: prewarm
spec:
  storageClassName: ${sc}
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 16Mi
---
apiVersion: v1
kind: Pod
metadata:
  name: ${pod_n}
  namespace: kube-system
  labels:
    app.kubernetes.io/managed-by: gentian-install
    app.kubernetes.io/component: prewarm
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
    - name: prewarm
      image: busybox:1.36
      imagePullPolicy: IfNotPresent
      command: ["sh","-c","echo ok > /data/ok && exit 0"]
      volumeMounts:
        - name: data
          mountPath: /data
      resources:
        requests: {cpu: "10m", memory: "8Mi"}
        limits:   {cpu: "50m", memory: "32Mi"}
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: ${pvc_n}
EOF
            _wait_prewarm_pod "${pod_n}" 240 "PVC-backed (${sc})"
            kubectl delete pvc -n kube-system "${pvc_n}" --grace-period=1 --wait=false >/dev/null 2>&1 || true
        done <<<"${racy_scs}"
    fi
}

# Helper for prewarm_cluster: wait for ${1} pod in kube-system to reach a
# terminal phase (Succeeded/Failed). Auto-recovers via cri_cleanup +
# kubelite_restart if it wedges in ContainerCreating with no IP. Always
# best-effort: caller continues regardless.
_wait_prewarm_pod() {
    local pod="$1" timeout="${2:-180}" label="${3:-prewarm}"
    info "Waiting for ${label} pre-warm pod to reach a terminal phase (up to ${timeout}s)..."
    local deadline=$((SECONDS + timeout))
    local phase="" stuck_for=0 last_phase="" recovery_done=0
    while (( SECONDS < deadline )); do
        phase=$(kubectl get pod -n kube-system "${pod}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
        case "${phase}" in
            Succeeded) success "${label} pre-warm pod completed."; break ;;
            Failed)    warn "${label} pre-warm pod failed (phase=Failed); continuing anyway."; break ;;
        esac
        # Track ContainerCreating wedge identical to wait_for_running_pod.
        local pod_ip
        pod_ip=$(kubectl get pod -n kube-system "${pod}" -o jsonpath='{.status.podIP}' 2>/dev/null || true)
        if [[ "${phase}" == "Pending" && -z "${pod_ip}" ]]; then
            if [[ "${phase}" == "${last_phase}" ]]; then
                stuck_for=$(( stuck_for + 3 ))
            else
                stuck_for=0
            fi
        else
            stuck_for=0
        fi
        last_phase="${phase}"
        if (( stuck_for >= 90 && recovery_done == 0 )); then
            warn "${label} pre-warm pod wedged in ContainerCreating with no IP after ${stuck_for}s — running recovery."
            kubectl delete pod -n kube-system "${pod}" --grace-period=0 --force >/dev/null 2>&1 || true
            cri_cleanup
            kubelite_restart
            recovery_done=1
            stuck_for=0
            # Re-apply the same pod so the wait loop has something to poll.
            # The owning controller (none — bare Pod) won't recreate it.
            # We bail out instead and rely on the next prewarm step / real
            # workload, which now lands on a freshly-restarted kubelite.
            warn "${label} pre-warm pod recovery issued; not re-creating bare pod. Continuing."
            return 0
        fi
        sleep 3
    done
    if [[ "${phase}" != "Succeeded" && "${phase}" != "Failed" ]]; then
        warn "${label} pre-warm pod did not finish within ${timeout}s (last phase: ${phase:-unknown})."
        kubectl describe pod -n kube-system "${pod}" 2>/dev/null | tail -20 || true
    fi
    kubectl delete pod -n kube-system "${pod}" --grace-period=1 --wait=false >/dev/null 2>&1 || true
}

# =============================================================================
# 3. Install cert-manager via Helm
# =============================================================================
install_cert_manager() {
    if [[ "$INSTALL_CLUSTER_INFRA" != "1" ]]; then
        warn "Cluster infra disabled: skipping cert-manager installation."
        return
    fi

    banner "Step 3 — Installing cert-manager"

    if helm status cert-manager -n cert-manager &>/dev/null; then
        # Existing Helm release may have been created by a previous install.sh run.
        : "${GENTIAN_MANAGED_CERT_MANAGER:=1}"
        save_install_state
        success "cert-manager already installed (Helm release present). Skipping."
        return
    fi

    # Discover an existing non-Helm cert-manager install by finding the
    # webhook deployment/service in any namespace.
    local detected_ns=""
    detected_ns=$(kubectl get deploy -A -o json 2>/dev/null \
        | jq -r '.items[] | select(.metadata.name=="cert-manager-webhook") | .metadata.namespace' \
        | head -1 || true)

    if [[ -z "${detected_ns}" ]]; then
        detected_ns=$(kubectl get svc -A -o json 2>/dev/null \
            | jq -r '.items[] | select(.metadata.name=="cert-manager-webhook") | .metadata.namespace' \
            | head -1 || true)
    fi

    # Only skip Helm if a real cert-manager installation is present.
    if [[ -n "${detected_ns}" ]]; then
        CERT_MANAGER_NAMESPACE="${detected_ns}"
        export CERT_MANAGER_NAMESPACE
        GENTIAN_MANAGED_CERT_MANAGER="0"
        save_install_state
        warn "cert-manager already present but not managed by Helm (e.g. distro addon)."
        info "Detected cert-manager webhook in namespace ${CERT_MANAGER_NAMESPACE}; using that installation as-is."
        return
    fi

    # Stale CRDs can remain after a partial uninstall; do not treat that as a
    # valid installation. Proceed with Helm install if webhook/service is absent.
    local has_existing_crds="0"
    if kubectl get crd certificates.cert-manager.io &>/dev/null; then
        has_existing_crds="1"
        warn "cert-manager CRDs exist, but no cert-manager webhook/service was detected."
        warn "Proceeding with Helm install while reusing existing CRDs."
    fi

    helm repo add jetstack https://charts.jetstack.io --force-update
    helm repo update
    if [[ "${has_existing_crds}" == "1" ]]; then
        # Existing CRDs may come from distro addons or prior non-Helm installs;
        # do not ask Helm to import/manage them.
        helm upgrade --install cert-manager jetstack/cert-manager \
            -n cert-manager \
            --create-namespace \
            --set crds.enabled=false \
            --wait --timeout 5m
    else
        helm upgrade --install cert-manager jetstack/cert-manager \
            -n cert-manager \
            --create-namespace \
            --set crds.enabled=true \
            --wait --timeout 5m
    fi
    CERT_MANAGER_NAMESPACE="cert-manager"
    export CERT_MANAGER_NAMESPACE
    GENTIAN_MANAGED_CERT_MANAGER="1"
    save_install_state
    success "cert-manager installed."
}

# ACME_ENV: production (default) or staging (Let's Encrypt staging API).
# Staging avoids production rate limits; certs are not browser-trusted.
gentian_dns01_cluster_issuer_name() {
    if [[ "${ACME_ENV:-production}" == "staging" ]]; then
        echo "letsencrypt-staging-dns01-cloudflare"
    else
        echo "letsencrypt-dns01-cloudflare"
    fi
}

gentian_cluster_issuers_manifest() {
    if [[ "${ACME_ENV:-production}" == "staging" ]]; then
        echo "${SCRIPT_DIR}/kernel/manifests/cert-manager/cluster-issuers-staging.yaml"
    else
        echo "${SCRIPT_DIR}/kernel/manifests/cert-manager/cluster-issuers.yaml"
    fi
}

# Apply (or refresh) kernel ClusterIssuers. Safe to re-run (update.sh --acme-issuers).
apply_gentian_cluster_issuers() {
    if [[ -z "${KERNEL_DOMAIN:-}" ]]; then
        warn "KERNEL_DOMAIN unset: skipping ClusterIssuers."
        return
    fi

    : "${LETSENCRYPT_EMAIL:=admin@${KERNEL_DOMAIN}}"
    : "${INGRESS_CLASS_NAME:=nginx}"
    export LETSENCRYPT_EMAIL INGRESS_CLASS_NAME KERNEL_DOMAIN

    if ! command -v envsubst &>/dev/null; then
        error "envsubst not found (install gettext-base). Aborting."
        exit 1
    fi

    if ! kubectl get deploy cert-manager-webhook -n "${CERT_MANAGER_NAMESPACE:-cert-manager}" &>/dev/null; then
        local detected_ns=""
        detected_ns=$(kubectl get deploy -A -o json 2>/dev/null \
            | jq -r '.items[] | select(.metadata.name=="cert-manager-webhook") | .metadata.namespace' \
            | head -1 || true)
        if [[ -n "${detected_ns}" ]]; then
            CERT_MANAGER_NAMESPACE="${detected_ns}"
            export CERT_MANAGER_NAMESPACE
        fi
    fi

    if ! kubectl get deploy cert-manager-webhook -n "${CERT_MANAGER_NAMESPACE:-cert-manager}" &>/dev/null; then
        error "cert-manager webhook not found; cannot apply ClusterIssuers."
        exit 1
    fi

    if [[ "${ACME_ENV:-production}" == "staging" ]]; then
        info "ACME_ENV=staging: using Let's Encrypt staging (untrusted certs, separate rate limits)."
    fi

    envsubst "\${LETSENCRYPT_EMAIL} \${INGRESS_CLASS_NAME}" \
        < "$(gentian_cluster_issuers_manifest)" \
        | kubectl apply -f -
}

# =============================================================================
# 3b. Install kernel cert-manager ClusterIssuers (always — both HTTP-01 and
# DNS-01-Cloudflare). The wildcard Certificate + cloudflare-api-token
# ExternalSecret are applied later by `install_kernel_wildcard` (after the
# OpenBao seeding step has populated the token). See docs/design/multi-tenancy.md §3.
# =============================================================================
install_kernel_cert_resources() {
    if [[ "$INSTALL_CLUSTER_INFRA" != "1" ]]; then
        warn "Cluster infra disabled: skipping kernel cert-manager resources."
        return
    fi
    if [[ -z "${KERNEL_DOMAIN:-}" ]]; then
        warn "KERNEL_DOMAIN unset: skipping kernel cert-manager resources."
        return
    fi

    banner "Step 3b — Installing kernel cert-manager ClusterIssuers"

    if ! kubectl get deploy cert-manager-webhook -n "${CERT_MANAGER_NAMESPACE:-cert-manager}" &>/dev/null; then
        local detected_ns=""
        detected_ns=$(kubectl get deploy -A -o json 2>/dev/null \
            | jq -r '.items[] | select(.metadata.name=="cert-manager-webhook") | .metadata.namespace' \
            | head -1 || true)
        if [[ -n "${detected_ns}" ]]; then
            CERT_MANAGER_NAMESPACE="${detected_ns}"
            export CERT_MANAGER_NAMESPACE
        fi
    fi
    : "${CERT_MANAGER_NAMESPACE:=cert-manager}"
    export CERT_MANAGER_NAMESPACE

    if ! kubectl get deploy cert-manager-webhook -n "${CERT_MANAGER_NAMESPACE}" &>/dev/null; then
        error "cert-manager webhook deployment not found in namespace ${CERT_MANAGER_NAMESPACE}."
        error "Fix cert-manager first, then re-run install.sh."
        exit 1
    fi

    # Wait for cert-manager webhook to be ready (Certificate/ClusterIssuer
    # admission would otherwise be rejected by an uninitialized webhook).
    info "Waiting for cert-manager webhook to be ready in namespace ${CERT_MANAGER_NAMESPACE}..."
    kubectl rollout status -n "${CERT_MANAGER_NAMESPACE}" deploy/cert-manager-webhook --timeout=180s >/dev/null \
        || warn "cert-manager-webhook not Ready within 180s (continuing)."

    apply_gentian_cluster_issuers
    if [[ "${ACME_ENV:-production}" == "staging" ]]; then
        success "ClusterIssuers letsencrypt-staging-http01 and letsencrypt-staging-dns01-cloudflare applied."
    else
        success "ClusterIssuers letsencrypt-http01 and letsencrypt-dns01-cloudflare applied."
    fi
}

# =============================================================================
# 3c. Install Envoy Gateway (Gateway API edge stack)
# =============================================================================
install_envoy_gateway() {
    if [[ "$INSTALL_CLUSTER_INFRA" != "1" ]]; then
        warn "Cluster infra disabled: skipping Envoy Gateway installation."
        return
    fi

    : "${ROUTING_MODE:=ingress}"
    export ROUTING_MODE
    if [[ "${ROUTING_MODE}" != "gateway" ]]; then
        info "ROUTING_MODE=${ROUTING_MODE}: skipping Envoy Gateway install."
        return
    fi

    banner "Step 3c — Installing Envoy Gateway and Gateway API CRDs"

    local ns="${ENVOY_GATEWAY_NAMESPACE}"
    local chart_version="${ENVOY_GATEWAY_CHART_VERSION}"
    local svc_type="ClusterIP"
    if [[ "${NETWORK_MODE:-tunnel}" == "static-ip" ]]; then
        svc_type="LoadBalancer"
    fi

    if helm status eg -n "${ns}" &>/dev/null; then
        success "Envoy Gateway Helm release already present in ${ns}."
    else
        info "Installing Envoy Gateway ${chart_version} (service type ${svc_type})..."
        helm upgrade --install eg oci://docker.io/envoyproxy/gateway-helm \
            --version "${chart_version}" \
            -n "${ns}" \
            --create-namespace \
            --set "config.envoyGateway.gateway.controllerName=${GENTIAN_GATEWAY_CONTROLLER_NAME}" \
            --set deployment.replicas=1 \
            --set "kubernetesService.type=${svc_type}" \
            --wait --timeout 5m
        success "Envoy Gateway installed in namespace ${ns}."
    fi

    info "Waiting for Envoy Gateway controller deployment..."
    if ! kubectl rollout status -n "${ns}" deploy/envoy-gateway --timeout=180s >/dev/null 2>&1; then
        warn "Envoy Gateway deployment not Ready within 180s (continuing)."
    fi

    info "Verifying Gateway API CRDs..."
    local crd
    for crd in \
        gatewayclasses.gateway.networking.k8s.io \
        gateways.gateway.networking.k8s.io \
        httproutes.gateway.networking.k8s.io; do
        if kubectl get crd "${crd}" &>/dev/null; then
            kubectl wait --for=condition=Established "crd/${crd}" --timeout=120s >/dev/null 2>&1 \
                || warn "CRD ${crd} not Established within 120s."
        else
            warn "Gateway API CRD ${crd} not found after Envoy Gateway install."
        fi
    done
    success "Envoy Gateway and Gateway API CRDs ready (ROUTING_MODE=gateway)."
    info "  GatewayClass: ${GENTIAN_GATEWAY_CLASS_NAME:-gentian-envoy}"
    info "  Controller:   ${GENTIAN_GATEWAY_CONTROLLER_NAME}"
    info "  Status:       kubectl get gatewayclass,gateway -A"
}

# =============================================================================
# 12b. Apply the kernel wildcard Certificate + ExternalSecret backing the
# Cloudflare API token. Runs after seed_secrets so the OpenBao path
# `secret/gentian-os/kernel/dns/cloudflare` is populated. Skipped silently
# when CF_API_TOKEN was not provided.
# =============================================================================
install_kernel_wildcard() {
    if [[ "$INSTALL_CLUSTER_INFRA" != "1" ]]; then
        return
    fi
    if [[ -z "${KERNEL_DOMAIN:-}" ]]; then
        return
    fi
    if [[ -z "${CF_API_TOKEN:-}" ]]; then
        info "CF_API_TOKEN not set; skipping kernel wildcard Certificate."
        info "  (Tenant app TLS still requires DNS-01 per-tenant wildcards; configure TENANT_DNS01_CLUSTER_ISSUER on the operator.)"
        return
    fi

    banner "Step 12b — Installing kernel wildcard Certificate"

    : "${LETSENCRYPT_EMAIL:=admin@${KERNEL_DOMAIN}}"
    : "${INGRESS_CLASS_NAME:=nginx}"
    DNS01_CLUSTER_ISSUER="$(gentian_dns01_cluster_issuer_name)"
    export LETSENCRYPT_EMAIL INGRESS_CLASS_NAME KERNEL_DOMAIN DNS01_CLUSTER_ISSUER

    # 1) ExternalSecret in cert-manager → materializes cloudflare-api-token
    #    Secret from OpenBao. Requires the ClusterSecretStore "openbao" to
    #    exist; install via Argo's globals app or apply directly here as a
    #    fallback (idempotent).
    if ! kubectl get clustersecretstore openbao &>/dev/null; then
        info "ClusterSecretStore/openbao missing — applying directly."
        kubectl apply -f "${SCRIPT_DIR}/kernel/eso/cluster-secret-store.yaml"
    fi
    kubectl apply -f "${SCRIPT_DIR}/kernel/manifests/cert-manager/cloudflare-api-token-externalsecret.yaml"

    # 2) Wait for the underlying Secret to materialize (ESO refresh).
    info "Waiting for Secret cert-manager/cloudflare-api-token (max 120s)..."
    local i
    for i in {1..60}; do
        if kubectl get secret cloudflare-api-token -n cert-manager &>/dev/null; then
            success "cloudflare-api-token materialized after ${i}x2s."
            break
        fi
        sleep 2
    done
    if ! kubectl get secret cloudflare-api-token -n cert-manager &>/dev/null; then
        warn "cloudflare-api-token did not materialize within 120s; check ExternalSecret status:"
        warn "  kubectl describe externalsecret cloudflare-api-token -n cert-manager"
        warn "Continuing — wildcard Certificate will issue once the Secret appears."
    fi

    # 3) Apply the wildcard Certificate (with domain name templating).
    envsubst "\${KERNEL_DOMAIN} \${DNS01_CLUSTER_ISSUER}" \
        < "${SCRIPT_DIR}/kernel/manifests/cert-manager/wildcard-kernel-cert.yaml" \
        | kubectl apply -f -
    success "Kernel wildcard Certificate wildcard-kernel applied (cert-manager namespace)."
    info "Issuance status:  kubectl get certificate wildcard-kernel -n cert-manager"

    # 4) Propagate wildcard-kernel-tls → wildcard-tls in every kernel app namespace
    #    that references it (nubus, nextcloud, intercom-service, nextcloud-notifypush).
    #    The Tenant operator issues per-tenant wildcard certs (tenant-*-wildcard-tls),
    #    but the kernel service namespaces are not managed by the operator.
    #    Wait up to 180 s for the cert to be issued first.
    info "Waiting for wildcard-kernel-tls to be issued (max 180s)..."
    local i
    for i in {1..90}; do
        if kubectl get secret wildcard-kernel-tls -n cert-manager &>/dev/null; then
            success "wildcard-kernel-tls Secret exists after ${i}x2s."
            break
        fi
        sleep 2
    done
    local app_ns="gentian-${ENV:-dev}"
    if ! kubectl get secret wildcard-kernel-tls -n cert-manager &>/dev/null; then
        warn "wildcard-kernel-tls not yet issued (LE rate-limited or still pending)."
        # Fallback: if the nubus CA Issuer is available in the app namespace,
        # issue wildcard-tls directly from it so ingresses work immediately.
        local nubus_issuer="nubus-${ENV:-dev}-ca-issuer"
        if kubectl get issuer "${nubus_issuer}" -n "${app_ns}" &>/dev/null; then
            info "Issuing wildcard-tls from nubus CA (${nubus_issuer}) in ${app_ns}..."
            kubectl apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: wildcard-dev-tls
  namespace: ${app_ns}
  labels:
    app.kubernetes.io/managed-by: gentian-install
    app.kubernetes.io/part-of: gentian-os
spec:
  secretName: wildcard-tls
  issuerRef:
    name: ${nubus_issuer}
    kind: Issuer
  commonName: "*.${KERNEL_DOMAIN:-desk.gentian.org}"
  dnsNames:
    - "${KERNEL_DOMAIN:-desk.gentian.org}"
    - "*.${KERNEL_DOMAIN:-desk.gentian.org}"
  duration: 8760h
  renewBefore: 720h
EOF
            if kubectl wait certificate "wildcard-dev-tls" -n "${app_ns}" \
                --for=condition=Ready --timeout=60s; then
                success "wildcard-tls issued from nubus CA in ${app_ns}."
            else
                warn "wildcard-dev-tls not ready in time; ingresses may show TLS warnings."
            fi
        else
            warn "No nubus CA issuer (${nubus_issuer}) found; wildcard-tls unavailable."
            warn "Re-run install.sh or manually copy the secret once the Certificate is Ready."
        fi
        return
    fi
    # Remove the fallback nubus-CA Certificate CR if present — it was created when
    # wildcard-kernel-tls was not yet ready, and cert-manager will keep overwriting
    # wildcard-tls with the nubus-CA cert as long as it exists.
    if kubectl get certificate wildcard-dev-tls -n "${app_ns}" &>/dev/null; then
        kubectl delete certificate wildcard-dev-tls -n "${app_ns}"
        success "Deleted fallback wildcard-dev-tls Certificate CR from ${app_ns}."
    fi
    # Propagate to all namespaces that reference wildcard-tls.
    for _wc_ns in "${app_ns}" argocd; do
        info "Propagating wildcard-tls into namespace ${_wc_ns}..."
        kubectl get secret wildcard-kernel-tls -n cert-manager -o json \
            | python3 -c "
import sys, json
s = json.load(sys.stdin)
for k in ('resourceVersion','uid','creationTimestamp'):
    s['metadata'].pop(k, None)
s['metadata'].pop('annotations', None)
s['metadata']['namespace'] = sys.argv[1]
s['metadata']['name'] = 'wildcard-tls'
print(json.dumps(s))
" "${_wc_ns}" | kubectl apply -f -
        success "wildcard-tls propagated to ${_wc_ns}."
    done

    # ACME staging: trust bundle for in-cluster OIDC (openDesk Synapse/Jitsi).
    if [[ "${ACME_ENV:-production}" == "staging" ]]; then
        local staging_ca_script="${SCRIPT_DIR}/scripts/create-staging-ca-secret.sh"
        if [[ -x "${staging_ca_script}" ]]; then
            info "Creating gentian-staging-ca-tls in ${app_ns} (ACME staging)..."
            "${staging_ca_script}" "${app_ns}" || warn "gentian-staging-ca-tls creation failed (tenant apps may not trust id.${KERNEL_DOMAIN})."
        fi
    fi
}

# =============================================================================
# 4. Install External Secrets Operator via Helm
# =============================================================================
install_eso() {
    banner "Step 4 — Installing External Secrets Operator"

    if helm status external-secrets -n external-secrets &>/dev/null; then
        success "ESO already installed. Skipping."
        return
    fi

    helm repo add external-secrets https://charts.external-secrets.io --force-update
    helm repo update
    helm install external-secrets external-secrets/external-secrets \
        -n external-secrets \
        --version "${ESO_CHART_VERSION}" \
        -f "${SCRIPT_DIR}/kernel/eso/values.yaml" \
        --wait --timeout 5m
    success "ESO installed."
}

# =============================================================================
# 5. Install ArgoCD + AppProject
# =============================================================================
resolve_argocd_url() {
    local ingress_host svc_type node_port lb_host lb_ip

    _is_testnet_ip() {
        local ip="$1"
        [[ "$ip" =~ ^192\.0\.2\.[0-9]+$ || "$ip" =~ ^198\.51\.100\.[0-9]+$ || "$ip" =~ ^203\.0\.113\.[0-9]+$ ]]
    }

    _pick_node_ip() {
        local detected
        if [[ -n "${NODE_IP:-}" ]]; then
            if _is_testnet_ip "${NODE_IP}"; then
                warn "NODE_IP=${NODE_IP} looks like documentation/testnet IP; auto-detecting real node IP instead." >&2
            else
                echo "${NODE_IP}"
                return 0
            fi
        fi

        detected=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null || true)
        if [[ -n "$detected" ]]; then
            echo "$detected"
            return 0
        fi
        detected=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="ExternalIP")].address}' 2>/dev/null || true)
        if [[ -n "$detected" ]]; then
            echo "$detected"
            return 0
        fi
        echo "<node-ip>"
        return 0
    }

    ingress_host=$(kubectl get ingress -n argocd \
        -o jsonpath='{.items[0].spec.rules[0].host}' 2>/dev/null || true)
    if [[ -n "$ingress_host" ]]; then
        echo "https://${ingress_host}"
        return 0
    fi

    svc_type=$(kubectl get svc argocd-server -n argocd \
        -o jsonpath='{.spec.type}' 2>/dev/null || true)
    node_port=$(kubectl get svc argocd-server -n argocd \
        -o jsonpath='{range .spec.ports[?(@.name=="https")]}{.nodePort}{end}' 2>/dev/null || true)
    if [[ -z "$node_port" ]]; then
        node_port=$(kubectl get svc argocd-server -n argocd \
            -o jsonpath='{range .spec.ports[0]}{.nodePort}{end}' 2>/dev/null || true)
    fi

    if [[ "$svc_type" == "LoadBalancer" ]]; then
        lb_host=$(kubectl get svc argocd-server -n argocd \
            -o jsonpath='{.status.loadBalancer.ingress[0].hostname}' 2>/dev/null || true)
        lb_ip=$(kubectl get svc argocd-server -n argocd \
            -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
        if [[ -n "$lb_host" ]]; then
            echo "https://${lb_host}"
            return 0
        fi
        if [[ -n "$lb_ip" ]]; then
            echo "https://${lb_ip}"
            return 0
        fi
    fi

    if [[ "$svc_type" == "NodePort" && -n "$node_port" ]]; then
        echo "https://$(_pick_node_ip):${node_port}"
        return 0
    fi

    # ClusterIP or unresolved external endpoint.
    echo "kubectl port-forward -n argocd svc/argocd-server 8080:443"
}

install_argocd() {
    banner "Step 5 — Installing ArgoCD"

    if kubectl get deployment argocd-server -n argocd &>/dev/null; then
        success "ArgoCD already installed."
    else
        bash "${SCRIPT_DIR}/scripts/install-argocd.sh"
        success "ArgoCD installed."
    fi

    kubectl apply -f "${SCRIPT_DIR}/kernel/argocd/projects/gentian.yaml"
    success "AppProject applied."

    info "Patching argocd-cm with annotation-based resource tracking..."
    # application.resourceTrackingMethod=annotation prevents ArgoCD from
    # treating Helm-managed resources as part of an ArgoCD app via the
    # default app.kubernetes.io/instance label. Helm charts (e.g. the
    # opendesk-postgresql/mariadb charts wrapped by Crossplane provider-helm
    # Release CRs) stamp every rendered resource with
    #   app.kubernetes.io/instance: <release-name>
    # which equals the ArgoCD Application name. With label-based tracking
    # ArgoCD then "adopts" those Helm-rendered StatefulSets/Services/etc.,
    # finds them missing from git, and PRUNES them seconds after Helm
    # creates them — leaving the Helm release in state=failed with errors
    # like 'services "opendesk-postgresql-dev" not found'. Annotation-based
    # tracking uses argocd.argoproj.io/tracking-id and only tracks resources
    # ArgoCD itself applied. See:
    # https://argo-cd.readthedocs.io/en/stable/user-guide/resource_tracking/
    kubectl patch configmap argocd-cm -n argocd --type merge -p '
{
  "data": {
    "application.resourceTrackingMethod": "annotation"
  }
}'
    success "ArgoCD annotation-based resource tracking configured."

    # Treat Pending PVCs as Healthy so ArgoCD sync waves are not blocked by
    # WaitForFirstConsumer PVCs. On a fresh install, a PVC with
    # volumeBindingMode=WaitForFirstConsumer (microk8s-hostpath) stays Pending
    # until a pod mounts it. Without this override ArgoCD considers the PVC
    # Progressing and never advances to the next wave where the consuming pod
    # (or hook job) would be created — causing a permanent deadlock.
    # Lost PVCs are still surfaced as Degraded.
    info "Patching argocd-cm with PVC WaitForFirstConsumer health override..."
    kubectl patch configmap argocd-cm -n argocd --type merge -p '
{
  "data": {
    "resource.customizations.health.PersistentVolumeClaim": "hs = {}\nif obj.status ~= nil then\n  if obj.status.phase == \"Bound\" then\n    hs.status = \"Healthy\"\n    hs.message = \"PVC bound\"\n    return hs\n  end\n  if obj.status.phase == \"Pending\" then\n    hs.status = \"Healthy\"\n    hs.message = \"PVC pending (WaitForFirstConsumer)\"\n    return hs\n  end\n  if obj.status.phase == \"Lost\" then\n    hs.status = \"Degraded\"\n    hs.message = \"PVC lost\"\n    return hs\n  end\nend\nhs.status = \"Progressing\"\nhs.message = \"Waiting for PVC\"\nreturn hs\n"
  }
}'
    success "ArgoCD PVC health override configured."

    # Prevent the ArgoCD application controller from entering a tight
    # reconciliation loop when Crossplane providers continuously update
    # .status on managed Keycloak resources.  Without this, the controller
    # re-enqueues keycloak-config-dev on every Crossplane status write (~20ms),
    # starving all other applications of reconciliation time.
    # resource.ignoreResourceUpdatesEnabled (ArgoCD ≥ 2.10) tells the
    # controller to skip re-queuing an app when only the listed JSON pointers
    # change on the affected resource.
    info "Patching argocd-cm with Crossplane Keycloak resource-update suppression..."
    kubectl patch configmap argocd-cm -n argocd --type merge -p '{
  "data": {
    "resource.ignoreResourceUpdatesEnabled": "true",
    "resource.customizations.ignoreResourceUpdates.client.keycloak.crossplane.io_ProtocolMapper": "jsonPointers:\n- /status\n- /metadata/resourceVersion\n- /metadata/generation\n",
    "resource.customizations.ignoreResourceUpdates.openidclient.keycloak.crossplane.io_Client": "jsonPointers:\n- /status\n- /metadata/resourceVersion\n- /metadata/generation\n",
    "resource.customizations.ignoreResourceUpdates.openidclient.keycloak.crossplane.io_ClientDefaultScopes": "jsonPointers:\n- /status\n- /metadata/resourceVersion\n- /metadata/generation\n",
    "resource.customizations.ignoreResourceUpdates.openidclient.keycloak.crossplane.io_ClientOptionalScopes": "jsonPointers:\n- /status\n- /metadata/resourceVersion\n- /metadata/generation\n",
    "resource.customizations.ignoreResourceUpdates.openidclient.keycloak.crossplane.io_ClientScope": "jsonPointers:\n- /status\n- /metadata/resourceVersion\n- /metadata/generation\n",
    "resource.customizations.ignoreResourceUpdates.keycloak.crossplane.io_ProviderConfig": "jsonPointers:\n- /status\n- /metadata/resourceVersion\n- /metadata/generation\n"
  }
}'
    success "ArgoCD Crossplane Keycloak resource-update suppression configured."

    # Configure ArgoCD server to serve plain HTTP so nginx can terminate TLS.
    # Without this flag ArgoCD redirects HTTP→HTTPS internally and nginx gets
    # into a redirect loop when doing TLS termination at the ingress.
    #
    # reposerver.repo.cache.expiration: how long the repo-server caches both
    # the branch→SHA resolution and the rendered manifest for a (repo, path,
    # revision) tuple.  The default is 24h, which means new commits to a
    # branch are not picked up for up to 24 hours without a webhook push
    # notification.  Setting this to 3m (same as timeout.reconciliation) means
    # every app-controller reconcile cycle triggers a fresh git fetch so new
    # commits are visible within one reconciliation window (~3 minutes).
    # GitHub webhooks further reduce this to near-zero for push events.
    info "Configuring ArgoCD server params (insecure + short repo cache)..."
    kubectl patch configmap argocd-cmd-params-cm -n argocd --type merge \
        -p '{"data":{"server.insecure":"true","reposerver.repo.cache.expiration":"3m"}}'
    kubectl rollout restart deployment argocd-server -n argocd
    kubectl rollout restart deployment argocd-repo-server -n argocd
    kubectl rollout status deployment argocd-server -n argocd --timeout=90s \
        2>/dev/null || true
    kubectl rollout status deployment argocd-repo-server -n argocd --timeout=90s \
        2>/dev/null || true
    success "ArgoCD server running in HTTP mode with 3-minute repo cache."

    # Configure GitHub webhook secret so ArgoCD accepts push notifications from
    # the gentian-org GitHub organisation.  The actual webhook must be registered
    # in GitHub (Settings → Webhooks, or via the GitHub CLI):
    #
    #   URL:     https://argocd.${KERNEL_DOMAIN}/api/webhook
    #   Content-Type: application/json
    #   Secret:  <value from OpenBao: identity/argocd/webhook-github-secret>
    #   Events:  push
    #
    # Without a GitHub webhook ArgoCD still detects new commits within
    # ~3 minutes (via the reduced cache expiry above), but with a webhook
    # syncs happen within seconds of a push.
    local github_webhook_secret="${ARGOCD_GITHUB_WEBHOOK_SECRET:-}"
    if [[ -z "$github_webhook_secret" ]]; then
        warn "ARGOCD_GITHUB_WEBHOOK_SECRET not set — generating a random secret."
        warn "Store it in OpenBao (identity/argocd/webhook-github-secret) and"
        warn "register it as a webhook on the gentian-org GitHub organisation."
        github_webhook_secret=$(openssl rand -hex 20)
    fi
    kubectl patch secret argocd-secret -n argocd --type merge \
        -p "{\"stringData\":{\"webhook.github.secret\":\"${github_webhook_secret}\"}}"
    success "ArgoCD GitHub webhook secret configured."
    info "Register webhook at: https://argocd.${KERNEL_DOMAIN:-<KERNEL_DOMAIN>}/api/webhook"

    # Create Ingress for argocd.${KERNEL_DOMAIN} if KERNEL_DOMAIN is set.
    # TLS uses wildcard-tls which is propagated by install_kernel_wildcard later;
    # the Ingress is safe to create before the Secret exists.
    if [[ -n "${KERNEL_DOMAIN:-}" ]]; then
        info "Creating ArgoCD Ingress for argocd.${KERNEL_DOMAIN}..."
        kubectl apply -f - <<EOF
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: argocd-server
  namespace: argocd
  annotations:
    nginx.ingress.kubernetes.io/backend-protocol: "HTTP"
    nginx.ingress.kubernetes.io/force-ssl-redirect: "true"
spec:
  ingressClassName: public
  rules:
  - host: argocd.${KERNEL_DOMAIN}
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: argocd-server
            port:
              number: 80
  tls:
  - hosts:
    - argocd.${KERNEL_DOMAIN}
    secretName: wildcard-tls
EOF
        success "ArgoCD Ingress created: https://argocd.${KERNEL_DOMAIN}"
    fi

    # Print ArgoCD admin credentials early so the user sees them even if
    # the install is interrupted before print_summary runs (verify step
    # can take up to 10 minutes).
    local argocd_pw argocd_url
    argocd_pw=$(kubectl get secret argocd-initial-admin-secret -n argocd \
                    -o jsonpath='{.data.password}' 2>/dev/null \
                    | base64 -d 2>/dev/null || echo "")
    argocd_url=$(resolve_argocd_url 2>/dev/null)
    if [[ -n "$argocd_pw" ]]; then
        info "ArgoCD URL   : ${argocd_url}"
        info "ArgoCD login : admin / ${argocd_pw}"
    else
        warn "ArgoCD initial-admin-secret not yet available; will be shown in final summary."
    fi
}

# =============================================================================
# 6. Create ArgoCD OCI registry secrets
# =============================================================================
setup_argocd_repos() {
    banner "Step 6 — ArgoCD OCI registry secrets"
    bash "${SCRIPT_DIR}/scripts/create-argocd-oci-secrets.sh" \
        "$OD_PRIVATE_REGISTRY_USERNAME" \
        "$OD_PRIVATE_REGISTRY_PASSWORD"
    success "ArgoCD OCI secrets configured."
}

# =============================================================================
# 7. Install ArgoCD Image Updater controller
# =============================================================================
install_argocd_image_updater() {
    banner "Step 7 — ArgoCD Image Updater"

    info "Adding Argo Helm repo..."
    helm repo add argo https://argoproj.github.io/argo-helm --force-update >/dev/null
    helm repo update >/dev/null

    info "Installing/upgrading argocd-image-updater chart..."
    helm upgrade --install argocd-image-updater argo/argocd-image-updater \
        --namespace argocd-image-updater \
        --create-namespace \
        --set "config.argocd\.namespace=argocd" \
        --set "config.watch\.namespaces=argocd" \
        --wait \
        --timeout 5m

    success "ArgoCD Image Updater controller is installed persistently in the cluster."
    info "Environment-specific ImageUpdater CRs should be managed in gentian-deployments (GitOps), not in this OS installer."
}

# =============================================================================
# 8. Deploy OpenBao transit seal instance
# =============================================================================
bootstrap_transit_app() {
    banner "Step 8 — OpenBao transit seal instance"

    # Note: CRI cleanup is intentionally NOT run here pre-flight. It is
    # invoked reactively by wait_for_running_pod's 2nd-tier escalation
    # only if the transit pod is demonstrably wedged (stuck 120s+ in
    # ContainerCreating with no IP), so a fresh / healthy cluster never
    # pays the sudo-prompt + sweep cost.

    if ! kubectl get secret openbao-transit-unseal -n openbao &>/dev/null; then
        kubectl create secret generic openbao-transit-unseal \
            -n openbao --from-literal=unseal-key=placeholder
        success "Placeholder openbao-transit-unseal secret created."
    fi

    kubectl apply -f "${SCRIPT_DIR}/kernel/bootstrap/openbao-transit-application.yaml"
    success "Applied openbao-transit-application.yaml"

    if ! wait_for_running_pod openbao "app.kubernetes.io/instance=openbao-transit" "openbao-transit" 480; then
        error "Step 7 failed: openbao-transit pod never became Ready. Aborting install."
        exit 1
    fi
}

# =============================================================================
# 9. Init the transit instance
# =============================================================================
init_openbao_transit() {
    banner "Step 9 — Transit instance init + autounseal Secret"
    if ! bash "${SCRIPT_DIR}/scripts/init-openbao-transit.sh"; then
        error "Step 8 failed: init-openbao-transit.sh exited non-zero."
        error "Without the openbao-transit-token Secret, the primary OpenBao"
        error "will be stuck in CreateContainerConfigError. Aborting install."
        exit 1
    fi
    # Sanity-check the side effects the script is supposed to produce. If
    # the script exited 0 but didn't actually create both Secrets (e.g. it
    # silently took an early-return path on a stale state), fail fast here
    # so subsequent steps don't proceed against a half-initialised transit.
    local missing=()
    kubectl get secret -n openbao openbao-transit-token  >/dev/null 2>&1 || missing+=(openbao-transit-token)
    kubectl get secret -n openbao openbao-transit-unseal >/dev/null 2>&1 || missing+=(openbao-transit-unseal)
    if (( ${#missing[@]} > 0 )); then
        error "Step 8 reported success but required Secrets are missing: ${missing[*]}"
        error "Re-run init-openbao-transit.sh manually and re-run install.sh."
        exit 1
    fi
}

# =============================================================================
# 10. Apply remaining ArgoCD bootstrap Applications
# =============================================================================
bootstrap_argocd_apps() {
    banner "Step 10 — ArgoCD bootstrap Applications"

    # Register public OCI chart repos used by bootstrap Applications.
    if [[ "$INSTALL_CLUSTER_INFRA" == "1" ]]; then
        kubectl apply -f "${SCRIPT_DIR}/kernel/argocd/repos/ghcr-stakater.yaml"
        kubectl apply -f "${SCRIPT_DIR}/kernel/argocd/repos/ghcr-cloudnative-pg.yaml"
    fi
    success "Applied public ArgoCD repository registrations."

    local apps=(openbao globals)
    if [[ "$INSTALL_CLUSTER_INFRA" == "1" ]]; then
        apps+=(reloader cnpg cnpg-cluster)
    fi

    for app in "${apps[@]}"; do
        kubectl apply -f "${SCRIPT_DIR}/kernel/bootstrap/${app}-application.yaml"
        success "Applied ${app}-application.yaml"
    done

    wait_for_running_pod openbao "app.kubernetes.io/name=openbao,app.kubernetes.io/instance=openbao" "openbao" 300 || {
        error "Step 9 failed: openbao pod never became Ready. Aborting install."
        exit 1
    }

    if [[ "$INSTALL_CLUSTER_INFRA" == "1" ]]; then
        # ArgoCD applies the Application and then syncs asynchronously, so the
        # Deployments do not exist yet when we return from the apply loop above.
        # Poll until the Deployment appears before calling kubectl wait.

        info "Waiting for reloader deployment to be created by ArgoCD (up to 5 min)..."
        _deadline=$((SECONDS + 300))
        until kubectl get deployment reloader-reloader -n stakater-system &>/dev/null; do
            (( SECONDS < _deadline )) || { error "Timed out waiting for reloader Deployment to appear."; exit 1; }
            sleep 5
        done
        kubectl wait --for=condition=available --timeout=300s \
            deployment/reloader-reloader -n stakater-system
        success "Reloader deployment is available."

        info "Waiting for CNPG operator deployment to be created by ArgoCD (up to 5 min)..."
        _deadline=$((SECONDS + 300))
        until kubectl get deployment cnpg-cloudnative-pg -n cnpg-system &>/dev/null; do
            (( SECONDS < _deadline )) || { error "Timed out waiting for CNPG Deployment to appear."; exit 1; }
            sleep 5
        done
        kubectl wait --for=condition=available --timeout=300s \
            deployment/cnpg-cloudnative-pg -n cnpg-system
        success "CNPG operator deployment is available."
    else
        warn "Cluster infra disabled: skipped reloader/CNPG bootstrap apps."
    fi
}

# =============================================================================
# 10. Initialize primary OpenBao (transit auto-unseal)
# =============================================================================
init_openbao() {
    banner "Step 10 — OpenBao init"

    info "Waiting for openbao service (up to 2 min)..."
    local i=0
    until kubectl get svc openbao -n openbao &>/dev/null; do
        echo -n "."; sleep 5; i=$((i + 5))
        [[ $i -lt 120 ]] || { error "Timed out."; exit 1; }
    done
    echo ""

    local BAO_SVC_IP
    BAO_SVC_IP=$(kubectl get svc openbao -n openbao -o jsonpath='{.spec.clusterIP}')
    local BAO_HTTP="http://${BAO_SVC_IP}:8200"

    local init_status
    init_status=$(curl -sf "${BAO_HTTP}/v1/sys/init" | jq -r '.initialized')

    if [[ "$init_status" == "true" ]]; then
        success "OpenBao already initialized."
        local sealed seal_type
        sealed=$(curl -sf "${BAO_HTTP}/v1/sys/seal-status" | jq -r '.sealed')
        seal_type=$(curl -sf "${BAO_HTTP}/v1/sys/seal-status" | jq -r '.type')
        if [[ "$sealed" == "true" && "$seal_type" == "transit" ]]; then
            warn "OpenBao sealed — waiting for transit auto-unseal..."
            sleep 15
            sealed=$(curl -sf "${BAO_HTTP}/v1/sys/seal-status" | jq -r '.sealed')
            [[ "$sealed" == "true" ]] && { error "Auto-unseal failed."; exit 1; }
            success "Transit auto-unseal completed."
        fi
        # Re-display stored credentials so the operator can verify them on re-runs.
        if [[ -f "${OPENBAO_INIT_FILE}" ]]; then
            local stored_key stored_token
            stored_key=$(jq -r '(.recovery_keys_base64 // .recovery_keys_b64 // .keys_base64 // [])[0] // empty' "${OPENBAO_INIT_FILE}" 2>/dev/null)
            stored_token=$(jq -r '.root_token // empty' "${OPENBAO_INIT_FILE}" 2>/dev/null)
            info "Stored init credentials (${OPENBAO_INIT_FILE}):"
            [[ -n "$stored_key"   ]] && info "  Recovery/Unseal Key : ${stored_key}"
            [[ -n "$stored_token" ]] && info "  Root Token          : ${stored_token}"
        fi
        return
    fi

    local seal_type_before
    seal_type_before=$(curl -sf "${BAO_HTTP}/v1/sys/seal-status" | jq -r '.type')

    if [[ "$seal_type_before" == "transit" ]]; then
        info "Initializing OpenBao with transit seal (recovery_shares=1)..."
        local init_resp
        init_resp=$(curl -sf -X PUT "${BAO_HTTP}/v1/sys/init" \
            -H "Content-Type: application/json" \
            -d '{"recovery_shares": 1, "recovery_threshold": 1}')

        echo "$init_resp" | jq '.' > "${OPENBAO_INIT_FILE}"
        chmod 600 "${OPENBAO_INIT_FILE}"

        local recovery_key root_token
        recovery_key=$(echo "$init_resp" | jq -r '(.recovery_keys_base64 // .recovery_keys_b64 // .recovery_keys // [])[0] // empty')
        root_token=$(echo "$init_resp"   | jq -r '.root_token // empty')

        if [[ -z "$recovery_key" || -z "$root_token" ]]; then
            error "Failed to parse OpenBao init response. Full payload saved at ${OPENBAO_INIT_FILE}."
            echo "$init_resp" | jq . >&2 || echo "$init_resp" >&2
            exit 1
        fi

        echo ""
        echo -e "${RED}╔═══════════════════════════════════════════════════════════════╗${NC}"
        echo -e "${RED}║  ⚠  SAVE THESE VALUES (password manager)                     ║${NC}"
        echo -e "${RED}╠═══════════════════════════════════════════════════════════════╣${NC}"
        echo -e "${RED}║  Recovery Key (= unseal key) : ${recovery_key}${NC}"
        echo -e "${RED}║  Root Token                  : ${root_token}${NC}"
        echo -e "${RED}╚═══════════════════════════════════════════════════════════════╝${NC}"
        echo ""
        read -rp "  Saved both values? [yes/no]: " confirmed
        [[ "$confirmed" == "yes" ]] || { error "Aborted."; exit 1; }

        export BAO_TOKEN="$root_token"

        info "Waiting for transit auto-unseal (up to 30 s)..."
        i=0
        until curl -sf "${BAO_HTTP}/v1/sys/seal-status" | jq -e '.sealed == false' >/dev/null 2>&1; do
            sleep 3; i=$((i + 3))
            [[ $i -lt 30 ]] || { error "Auto-unseal timed out."; exit 1; }
        done
        success "OpenBao initialized and auto-unsealed via transit."
    else
        info "Initializing OpenBao (1-of-1 Shamir — transit unavailable)..."
        local init_resp
        init_resp=$(curl -sf -X PUT "${BAO_HTTP}/v1/sys/init" \
            -H "Content-Type: application/json" \
            -d '{"secret_shares": 1, "secret_threshold": 1}') || {
            error "OpenBao init request failed against ${BAO_HTTP}."
            error "The openbao-0 pod likely has no Ready endpoints (check 'kubectl get pod -n openbao')."
            error "Common cause: the openbao-transit-token Secret is missing, leaving openbao-0 in CreateContainerConfigError."
            exit 1
        }

        echo "$init_resp" | jq '.' > "${OPENBAO_INIT_FILE}"
        chmod 600 "${OPENBAO_INIT_FILE}"

        local unseal_key root_token
        unseal_key=$(echo "$init_resp" | jq -r '.keys_base64[0] // empty')
        root_token=$(echo "$init_resp"  | jq -r '.root_token // empty')

        if [[ -z "$unseal_key" || -z "$root_token" ]]; then
            error "OpenBao init response missing keys_base64[0] or root_token."
            error "Raw response saved at ${OPENBAO_INIT_FILE}."
            echo "$init_resp" | jq . >&2 || echo "$init_resp" >&2
            exit 1
        fi

        echo ""
        echo -e "${RED}║  Unseal Key : ${unseal_key}${NC}"
        echo -e "${RED}║  Root Token : ${root_token}${NC}"
        read -rp "  Saved both values? [yes/no]: " confirmed
        [[ "$confirmed" == "yes" ]] || { error "Aborted."; exit 1; }

        curl -sf -X PUT "${BAO_HTTP}/v1/sys/unseal" \
            -H "Content-Type: application/json" \
            -d "{\"key\": \"${unseal_key}\"}" | jq .
        export BAO_TOKEN="$root_token"
        success "OpenBao initialized and unsealed (Shamir)."
    fi
}

# =============================================================================
# 11. Bootstrap OpenBao via bao CLI (KV engine, K8s auth, policies, roles)
#
# Creates the minimal permanent resources that the rest of the install needs:
#   • KV v2 mount at secret/
#   • Kubernetes auth backend + config
#   • eso-read policy + eso role  (ESO ClusterSecretStore authentication)
#
# The operator-write policy and gentian-os-operator role are NOT created here;
# they are managed as provider-vault Crossplane MRs in
# kernel/services/openbao-config/manifests/{env}/ (wave 15).
# =============================================================================
bao_bootstrap() {
    banner "Step 11 — OpenBao bootstrap (bao CLI)"

    local BAO_SVC_IP
    BAO_SVC_IP=$(kubectl get svc openbao -n openbao -o jsonpath='{.spec.clusterIP}')
    export VAULT_ADDR="http://${BAO_SVC_IP}:8200"

    if [[ -z "${BAO_TOKEN:-}" ]]; then
        if [[ -f "${OPENBAO_INIT_FILE}" ]]; then
            BAO_TOKEN=$(jq -r '.root_token' "${OPENBAO_INIT_FILE}")
        else
            read -rp "  Enter OpenBao root token: " BAO_TOKEN; echo ""
        fi
    fi
    export VAULT_TOKEN="$BAO_TOKEN"

    # ── 1. KV v2 mount at 'secret/' ──────────────────────────────────────────
    if bao secrets list -format=json 2>/dev/null | jq -e '."secret/"' >/dev/null 2>&1; then
        success "KV v2 mount at 'secret/' already present."
    else
        bao secrets enable -path=secret kv-v2
        success "KV v2 mount at 'secret/' enabled."
    fi

    # ── 2. Kubernetes auth backend ────────────────────────────────────────────
    if bao auth list -format=json 2>/dev/null | jq -e '."kubernetes/"' >/dev/null 2>&1; then
        success "Kubernetes auth backend already present."
    else
        bao auth enable -path=kubernetes kubernetes
        success "Kubernetes auth backend enabled."
    fi
    bao write auth/kubernetes/config \
        kubernetes_host="https://kubernetes.default.svc"
    success "Kubernetes auth backend configured."

    # ── 3. eso-read policy ────────────────────────────────────────────────────
    bao policy write eso-read - <<'POLICY'
path "secret/data/gentian-os/kernel/*"          { capabilities = ["read"] }
path "secret/metadata/gentian-os/kernel/*"      { capabilities = ["list"] }
path "secret/data/gentian-os/tenants/+/apps/*" { capabilities = ["read"] }
path "secret/metadata/gentian-os/tenants/*"     { capabilities = ["list"] }
POLICY
    success "eso-read policy written."

    # ── 4. eso Kubernetes auth role ───────────────────────────────────────────
    bao write auth/kubernetes/role/eso \
        bound_service_account_names=external-secrets \
        bound_service_account_namespaces=external-secrets \
        token_policies=eso-read \
        token_ttl=3600
    success "eso K8s auth role created."

    success "OpenBao bootstrap complete."
}

# =============================================================================
# 12. Seed kernel secrets
# =============================================================================
seed_secrets() {
    banner "Step 12 — Seeding kernel secrets"

    local BAO_SVC_IP
    BAO_SVC_IP=$(kubectl get svc openbao -n openbao -o jsonpath='{.spec.clusterIP}')
    export BAO_ADDR="http://${BAO_SVC_IP}:8200"

    if [[ -z "${BAO_TOKEN:-}" ]]; then
        if [[ -f "${OPENBAO_INIT_FILE}" ]]; then
            BAO_TOKEN=$(jq -r '.root_token' "${OPENBAO_INIT_FILE}")
        else
            read -rp "  Enter OpenBao root token: " BAO_TOKEN; echo ""
        fi
    fi
    export BAO_TOKEN

    # CF_API_TOKEN is forwarded via env var (not positional) so the
    # seed-openbao.sh contract stays backward-compatible. Seed-openbao
    # writes it to secret/gentian-os/kernel/dns/cloudflare when present.
    CF_API_TOKEN="${CF_API_TOKEN:-}" \
    MAIL_SERVICE_MODE="${MAIL_SERVICE_MODE:-external}" \
    EXTERNAL_SMTP_HOST="${EXTERNAL_SMTP_HOST:-}" \
    EXTERNAL_SMTP_PORT="${EXTERNAL_SMTP_PORT:-587}" \
    EXTERNAL_SMTP_SSL="${EXTERNAL_SMTP_SSL:-false}" \
    EXTERNAL_SMTP_STARTTLS="${EXTERNAL_SMTP_STARTTLS:-true}" \
    bash "${SCRIPT_DIR}/scripts/seed-openbao.sh" \
        "$MASTER_PASSWORD" \
        "$OD_PRIVATE_REGISTRY_USERNAME" \
        "$OD_PRIVATE_REGISTRY_PASSWORD" \
        "$OD_SMTP_RELAY_USERNAME" \
        "$OD_SMTP_RELAY_PASSWORD"
    success "All kernel secrets seeded."
}

# =============================================================================
# 13. Apply root ApplicationSet
# =============================================================================
bootstrap_root_appset() {
    banner "Step 13 — Applying root ApplicationSet"
    bash "${SCRIPT_DIR}/scripts/bootstrap.sh"
    success "Root ApplicationSet applied."
}

# =============================================================================
# 14. AppCatalogue CRD + kubectl-gentian plugin
# =============================================================================
install_app_catalogue() {
    banner "Step 14 — AppCatalogue CRD + kubectl-gentian plugin"

    kubectl apply -f "${SCRIPT_DIR}/config/crd/gentianos.io_appcatalogues.yaml"
    success "AppCatalogue CRD applied."

    local plugin_src="${SCRIPT_DIR}/scripts/kubectl-gentian"
    local plugin_dst="/usr/local/bin/kubectl-gentian"
    local alias_dst="/usr/local/bin/gtnctl"

    # Idempotency: skip if destination is identical to source (no sudo needed).
    if [[ -f "$plugin_dst" ]] && cmp -s "$plugin_src" "$plugin_dst"; then
        success "kubectl-gentian already up-to-date at ${plugin_dst}."
    elif [[ -w /usr/local/bin ]]; then
        install -m 755 "$plugin_src" "$plugin_dst"
        success "kubectl-gentian installed to ${plugin_dst}."
    else
        info "Installing kubectl-gentian to ${plugin_dst} (sudo required)..."
        if sudo install -m 755 "$plugin_src" "$plugin_dst"; then
            success "kubectl-gentian installed to ${plugin_dst}."
        else
            warn "Failed to install kubectl-gentian — install manually:"
            warn "  sudo install -m 755 ${plugin_src} ${plugin_dst}"
        fi
    fi

    if [[ ! -x "${plugin_dst}" ]]; then
        warn "Skipping gtnctl symlink — kubectl-gentian is not installed."
        return 0
    fi

    if [[ -L "${alias_dst}" ]] && [[ "$(readlink -f "${alias_dst}")" == "$(readlink -f "${plugin_dst}")" ]]; then
        success "gtnctl symlink already up-to-date at ${alias_dst}."
    elif [[ -w /usr/local/bin ]]; then
        ln -sf "${plugin_dst}" "${alias_dst}"
        success "gtnctl symlink installed at ${alias_dst}."
    else
        info "Installing gtnctl symlink at ${alias_dst} (sudo required)..."
        if sudo ln -sf "${plugin_dst}" "${alias_dst}"; then
            success "gtnctl symlink installed at ${alias_dst}."
        else
            warn "Failed to install gtnctl symlink — install manually:"
            warn "  sudo ln -sf ${plugin_dst} ${alias_dst}"
        fi
    fi
}

# =============================================================================
# 14b. Install ArgoCD Application that syncs AppProfiles from gentian-apps
# =============================================================================
# Renders kernel/bootstrap/appprofiles-application.yaml.tmpl with the user's
# chosen repo URL and branch, and applies it. Once Synced, every YAML in
# <gentian-apps>/profiles/ becomes a cluster-scoped AppProfile CR, which the
# AppStore controller projects into the AppCatalogue singleton (kubectl gentian
# apps list reads from there).
install_appprofiles_sync() {
    banner "Step 14b — ArgoCD Application syncing AppProfiles from gentian-apps"

    local tmpl="${SCRIPT_DIR}/kernel/bootstrap/appprofiles-application.yaml.tmpl"
    local rendered
    rendered="$(mktemp)"
    sed -e "s|%REPO_URL%|${GENTIAN_APPS_REPO}|g" \
        -e "s|%BRANCH%|${GENTIAN_APPS_BRANCH}|g" \
        "$tmpl" >"$rendered"

    info "Applying gentian-appprofiles Application:"
    info "  repo:   ${GENTIAN_APPS_REPO}"
    info "  branch: ${GENTIAN_APPS_BRANCH}"
    kubectl apply -f "$rendered"
    rm -f "$rendered"
    success "AppProfiles sync configured. ArgoCD will populate AppProfile CRs."
    info "After sync, list available app profiles with:"
    info "  kubectl gentian apps list"
}

# =============================================================================
# 15. Install gentian-os orchestrator (Helm chart + ArgoCD Application)
# =============================================================================
# The orchestrator chart at charts/gentian-os/ ships:
#   - CRDs: tenants, appprofiles, integrationbindings, appcatalogues
#   - Deployment + ServiceAccount + ClusterRole(Binding) for the operator
#   - ServiceMonitor + Grafana dashboard
#
# Two-phase install:
#   Phase 1 — Direct Helm bootstrap (fast):
#     CRDs and the operator Deployment are applied immediately so that
#     subsequent install steps can use them without waiting for ArgoCD.
#   Phase 2 — ArgoCD Application handoff:
#     The gentian-os ArgoCD Application (rendered from
#     kernel/bootstrap/gentian-os-application.yaml.tmpl) is applied.
#     ArgoCD takes ownership of the resources via ServerSideApply and from
#     this point drives all future chart upgrades.  Critically, Source 4 of
#     the Application deploys the ImageUpdater CR into the cluster, which
#     activates argocd-image-updater's automatic image rollout: whenever a
#     new image is pushed to GHCR for the tracked branch, argocd-image-updater
#     patches image.tag in the Application's Helm parameters and ArgoCD
#     triggers a Helm upgrade (rolling restart) automatically.
#
# Without Phase 2, argocd-image-updater reports "no ImageUpdater CRs to
# process" and image updates require manual kubectl rollout restart.
# =============================================================================
install_orchestrator() {
    banner "Step 15 — gentian-os orchestrator (CRDs + operator + ArgoCD handoff)"

    local chart_dir="${SCRIPT_DIR}/charts/gentian-os"
    local crd_dir="${chart_dir}/crds"
    local ns="gentian-system"
    local stage="${GENTIAN_DEPLOYMENTS_STAGE:-${ENV:-dev}}"
    local cluster="${GENTIAN_DEPLOYMENTS_CLUSTER:-default-cluster}"
    local required_crds=(
        tenants.gentianos.io
        appprofiles.gentianos.io
        integrationbindings.gentianos.io
        appcatalogues.gentianos.io
    )

    if ! kubectl get namespace "$ns" >/dev/null 2>&1; then
        kubectl create namespace "$ns"
    fi

    # ── Phase 1: Direct Helm bootstrap ────────────────────────────────────────
    info "Applying orchestrator CRDs (hard requirement for subsequent steps)..."
    if [[ ! -d "$crd_dir" ]]; then
        error "CRD directory not found: ${crd_dir}"
        exit 1
    fi
    kubectl apply -f "$crd_dir"

    # ── Pre-flight: adopt any webhook resources left from a previous run ──────
    # If ValidatingWebhookConfigurations exist without Helm ownership metadata,
    # helm upgrade --install will refuse to proceed.  Annotate and label them
    # so Helm can adopt them (idempotent if annotations are already present).
    local vwc
    for vwc in $(kubectl get validatingwebhookconfigurations \
                     -l "app.kubernetes.io/managed-by=Helm" \
                     --no-headers -o custom-columns=NAME:.metadata.name 2>/dev/null \
                 | grep -v "^$" || true); do
        :  # already owned
    done
    while IFS= read -r vwc; do
        [[ -z "$vwc" ]] && continue
        kubectl annotate validatingwebhookconfiguration "$vwc" \
            "meta.helm.sh/release-name=gentian-os" \
            "meta.helm.sh/release-namespace=${ns}" \
            --overwrite
        kubectl label validatingwebhookconfiguration "$vwc" \
            "app.kubernetes.io/managed-by=Helm" \
            --overwrite
        info "Adopted pre-existing ValidatingWebhookConfiguration '${vwc}' into Helm release."
    done < <(kubectl get validatingwebhookconfigurations \
                 --no-headers -o custom-columns=NAME:.metadata.name 2>/dev/null \
             | grep "^gentian-os-" || true)

    # ── Pre-flight: adopt any namespaces left from a previous run ────────────
    # The chart owns the shared-apps namespace. After an uninstall the namespace
    # may still exist (terminating finalizers, manual creation, etc.) without
    # Helm ownership annotations, causing helm upgrade --install to abort.
    local chart_ns="shared-apps"
    if kubectl get namespace "${chart_ns}" >/dev/null 2>&1; then
        kubectl annotate namespace "${chart_ns}" \
            "meta.helm.sh/release-name=gentian-os" \
            "meta.helm.sh/release-namespace=${ns}" \
            --overwrite
        kubectl label namespace "${chart_ns}" \
            "app.kubernetes.io/managed-by=Helm" \
            --overwrite
        info "Adopted pre-existing namespace '${chart_ns}' into Helm release."
    fi

    info "Bootstrapping gentian-os Helm release in namespace '${ns}'..."
    info "(ArgoCD will take ownership of this release in Phase 2 below.)"
    helm upgrade --install gentian-os "$chart_dir" \
        --namespace "$ns" \
        --set openbao.address="http://openbao.openbao.svc.cluster.local:8200" \
        --set argocd.namespace="argocd" \
        --set kernelDomain="${KERNEL_DOMAIN}" \
        --set tenancyMode="${TENANCY_MODE:-multi}" \
        --wait --timeout 5m

    info "Waiting for orchestrator CRDs to be Established..."
    for crd in "${required_crds[@]}"; do
        kubectl wait --for=condition=Established "crd/${crd}" --timeout=60s >/dev/null || {
            error "Required CRD ${crd} was not established."
            error "Orchestrator install is incomplete; aborting."
            exit 1
        }
    done
    success "Phase 1 complete: CRDs Established, operator running."

    # ── Phase 2: ArgoCD Application handoff ───────────────────────────────────
    # Render the Application template and apply it.  ArgoCD adopts the
    # already-running resources via ServerSideApply, adds the ImageUpdater CR
    # (Source 4), and drives all future upgrades from git.
    local gentian_os_branch
    gentian_os_branch=$(git -C "${SCRIPT_DIR}" rev-parse --abbrev-ref HEAD 2>/dev/null || echo "develop")

    local tmpl="${SCRIPT_DIR}/kernel/bootstrap/gentian-os-application.yaml.tmpl"
    local rendered
    rendered="$(mktemp)"
    sed -e "s|%GENTIAN_OS_BRANCH%|${gentian_os_branch}|g" \
        -e "s|%DEPLOYMENTS_REPO%|${GENTIAN_DEPLOYMENTS_REPO}|g" \
        -e "s|%DEPLOYMENTS_BRANCH%|${GENTIAN_DEPLOYMENTS_BRANCH}|g" \
        -e "s|%CLUSTER%|${cluster}|g" \
        -e "s|%STAGE%|${stage}|g" \
        "$tmpl" >"$rendered"

    info "Registering gentian-os Application + gentian-tenants ApplicationSet..."
    info "  operator branch:    ${gentian_os_branch}"
    info "  deployments repo:   ${GENTIAN_DEPLOYMENTS_REPO}"
    info "  deployments branch: ${GENTIAN_DEPLOYMENTS_BRANCH}"
    info "  deployments cluster:${cluster}"
    info "  deployments stage:  ${stage}"
    kubectl apply -f "$rendered"
    rm -f "$rendered"

    success "Phase 2 complete: gentian-os Application and gentian-tenants ApplicationSet registered with ArgoCD."
    success "  Image updates are now fully automatic via argocd-image-updater."
    info "Monitor operator:  kubectl get application gentian-os -n argocd"
    info "Monitor tenants:   kubectl get applicationset gentian-tenants -n argocd"
    info "Monitor updater:   kubectl get imageupdater gentian-os -n argocd"
    info "Provision tenants: kubectl gentian tenants list"
    info "                   kubectl gentian tenants deploy demo   # activate definition from definitions/"
}

# =============================================================================
# Step 15b — Deploy kernel mail services (Postfix + Dovecot)
#
# Called when MAIL_SERVICE_MODE=kernel. Applies the provider-helm Release CRs,
# ConfigMaps, and ExternalSecrets for postfix and dovecot from:
#   kernel/services/postfix/manifests/${ENV:-dev}/
#   kernel/services/dovecot/manifests/${ENV:-dev}/
#
# Both service directories follow the standard Pattern B layout:
#   configmap.yaml        — non-sensitive Helm values ConfigMaps
#   externalsecret.yaml   — ESO ExternalSecret (reads from OpenBao)
#   release.yaml          — Crossplane provider-helm Release CR
#
# Prerequisites:
#   - provider-helm must be Healthy (Step 13).
#   - OpenBao KV paths must be seeded (gentian-os-kernel-mail-postfix and
#     gentian-os-kernel-mail-dovecot Secrets must exist in crossplane-system).
#   - ESO ClusterSecretStore openbao must be ready.
#
# This function is idempotent (kubectl apply) and is also called from update.sh
# when --mail is used and MAIL_SERVICE_MODE=kernel, so it must not fail if
# resources already exist.
# =============================================================================
deploy_kernel_mail_services() {
    local mode="${MAIL_SERVICE_MODE:-external}"
    [[ "${mode}" != "kernel" ]] && return 0

    banner "Step 15b — Deploy kernel mail services (MAIL_SERVICE_MODE=kernel)"

    local env="${ENV:-dev}"
    local ns="gentian-${env}"

    # Ensure the target namespace exists (it is created by deploy_nubus but
    # calling this standalone from update.sh requires it to pre-exist).
    if ! kubectl get namespace "${ns}" >/dev/null 2>&1; then
        info "Creating namespace ${ns}..."
        kubectl create namespace "${ns}"
    fi

    # ── Postfix manifests ─────────────────────────────────────────────────────
    info "Applying postfix manifests (ConfigMaps, ExternalSecret, Release)..."
    kubectl apply -f "${SCRIPT_DIR}/kernel/services/postfix/manifests/${env}/"

    info "Waiting for postfix-sensitive-values ExternalSecret to sync (up to 60s)..."
    kubectl wait externalsecret/postfix-sensitive-values \
        -n "${ns}" --for=condition=Ready --timeout=60s \
    || warn "postfix-sensitive-values not yet Ready — it will sync when OpenBao is available."

    # ── Dovecot manifests ─────────────────────────────────────────────────────
    info "Applying dovecot manifests (ConfigMaps, ExternalSecret, Release)..."
    kubectl apply -f "${SCRIPT_DIR}/kernel/services/dovecot/manifests/${env}/"

    info "Waiting for dovecot-sensitive-values ExternalSecret to sync (up to 60s)..."
    kubectl wait externalsecret/dovecot-sensitive-values \
        -n "${ns}" --for=condition=Ready --timeout=60s \
    || warn "dovecot-sensitive-values not yet Ready — it will sync when OpenBao is available."


    # ── Reconcile mail.<domain> CoreDNS hairpin → Dovecot ClusterIP ──────────
    # OX App Suite connects to Dovecot via mail.<domain>:143 (STARTTLS). The
    # wildcard TLS cert (*.<domain>) validates against that hostname, so we
    # cannot point OX directly at the in-cluster service FQDN. Instead we keep
    # a CoreDNS hosts override so that mail.<domain> resolves to the current
    # Dovecot ClusterIP, bypassing the nginx ingress which does not proxy raw
    # IMAP/TCP on port 143.
    local mail_domain="mail.${KERNEL_DOMAIN:-}"
    if [[ -n "${KERNEL_DOMAIN:-}" ]]; then
        local dovecot_ip
        dovecot_ip=$(kubectl get svc "dovecot-${env}" -n "${ns}" \
            -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)
        if [[ -n "${dovecot_ip}" ]]; then
            info "Reconciling CoreDNS hairpin: ${mail_domain} → ${dovecot_ip}"
            local corefile patched
            corefile=$(kubectl get configmap coredns -n kube-system \
                -o jsonpath='{.data.Corefile}' 2>/dev/null || true)
            if echo "${corefile}" | grep -qF "${mail_domain}"; then
                # Replace the existing IP for mail.<domain> in the hairpin block.
                # shellcheck disable=SC2001  # regex IP substitution requires sed
                patched=$(echo "${corefile}" | sed \
                    "s|[0-9]\{1,3\}\.[0-9]\{1,3\}\.[0-9]\{1,3\}\.[0-9]\{1,3\}\([[:space:]]*${mail_domain}\)|${dovecot_ip}\1|g")
            elif echo "${corefile}" | grep -q "# BEGIN gentian-hairpin"; then
                # Hairpin block exists but lacks a mail entry — add it.
                # shellcheck disable=SC2001  # multiline insert requires sed
                patched=$(echo "${corefile}" | sed \
                    "s|# BEGIN gentian-hairpin|# BEGIN gentian-hairpin\n          ${dovecot_ip} ${mail_domain}|")
            else
                warn "CoreDNS Corefile has no gentian-hairpin block; skipping mail DNS update."
                patched="${corefile}"
            fi
            if [[ "${corefile}" != "${patched}" ]]; then
                local patch_json
                patch_json=$(printf '%s' "${patched}" | python3 -c \
                    'import sys,json; print(json.dumps({"data":{"Corefile":sys.stdin.read()}}))')
                kubectl patch configmap coredns -n kube-system \
                    --type=merge -p "${patch_json}" >/dev/null
                # Rolling restart so CoreDNS reloads the updated Corefile.
                kubectl rollout restart deployment coredns -n kube-system \
                    >/dev/null 2>&1 || true
                kubectl rollout status deployment coredns -n kube-system \
                    --timeout=60s >/dev/null 2>&1 || true
                success "CoreDNS hairpin updated: ${mail_domain} → ${dovecot_ip}"
            else
                info "CoreDNS hairpin for ${mail_domain} already correct (${dovecot_ip})."
            fi
        else
            warn "Dovecot service 'dovecot-${env}' not yet created; CoreDNS hairpin for" \
                 "${mail_domain} will be set on the next install/update run."
        fi
    fi

    success "Kernel mail services (postfix + dovecot) manifests applied."
    info "provider-helm will reconcile the Release CRs within 5 minutes."
    info "Monitor: kubectl get release.helm.crossplane.io | grep -E 'postfix|dovecot'"
    info "         argocd app sync gentian-infra-helm-${env}"
}

# =============================================================================
# _repair_nextcloud_object_home_mounts — fix LDAP user Files 500 errors
#
# With primary object storage, oc_mounts points at object::user:<uid> but first-
# login filecache rows are often created on home::<uid>. The Files app then
# returns "The root directory of the user's files is missing". Merge home::
# filecache into the object::user storage and point the mount at the home root.
# =============================================================================
_repair_nextcloud_object_home_mounts() {
    local env="${ENV:-dev}"
    local infra_ns="gentian-infra-${env}"
    local pg_pod="opendesk-postgresql-${env}-0"

    if ! kubectl get pod -n "${infra_ns}" "${pg_pod}" >/dev/null 2>&1; then
        info "PostgreSQL pod ${pg_pod} not found — skipping object/home mount repair"
        return 0
    fi

    local repaired
    repaired=$(kubectl exec -n "${infra_ns}" "${pg_pod}" -- psql -U nextcloud_user -d nextcloud -v ON_ERROR_STOP=1 -tA <<'EOSQL'
SELECT count(*) FROM (
  SELECT m.user_id,
         hs.numeric_id AS home_sid,
         os.numeric_id AS object_sid,
         hr.fileid AS home_root,
         (SELECT count(*) FROM oc_filecache WHERE storage = hs.numeric_id) AS home_files,
         (SELECT count(*) FROM oc_filecache WHERE storage = os.numeric_id) AS object_files
  FROM oc_mounts m
  JOIN oc_storages os ON os.id = 'object::user:' || m.user_id
  JOIN oc_storages hs ON hs.id = 'home::' || m.user_id
  JOIN oc_filecache hr ON hr.storage = hs.numeric_id AND hr.path = '' AND hr.parent = -1
  WHERE m.storage_id = os.numeric_id
    AND (SELECT count(*) FROM oc_filecache WHERE storage = hs.numeric_id)
        > (SELECT count(*) FROM oc_filecache WHERE storage = os.numeric_id)
) mismatched;
EOSQL
) || repaired=0

    if [[ "${repaired:-0}" == "0" ]]; then
        info "No object/home filecache mismatches detected"
        return 0
    fi

    info "Repairing ${repaired} Nextcloud user(s) with object/home filecache mismatch..."
    kubectl exec -n "${infra_ns}" "${pg_pod}" -- psql -U nextcloud_user -d nextcloud -v ON_ERROR_STOP=1 <<'EOSQL'
DO $$
DECLARE
  rec RECORD;
BEGIN
  FOR rec IN
    SELECT m.user_id,
           hs.numeric_id AS home_sid,
           os.numeric_id AS object_sid,
           hr.fileid AS home_root
    FROM oc_mounts m
    JOIN oc_storages os ON os.id = 'object::user:' || m.user_id
    JOIN oc_storages hs ON hs.id = 'home::' || m.user_id
    JOIN oc_filecache hr ON hr.storage = hs.numeric_id AND hr.path = '' AND hr.parent = -1
    WHERE m.storage_id = os.numeric_id
      AND (SELECT count(*) FROM oc_filecache WHERE storage = hs.numeric_id)
          > (SELECT count(*) FROM oc_filecache WHERE storage = os.numeric_id)
  LOOP
    DELETE FROM oc_filecache
      WHERE storage = rec.object_sid AND path = '' AND fileid <> rec.home_root;
    UPDATE oc_filecache SET storage = rec.object_sid WHERE storage = rec.home_sid;
    UPDATE oc_mounts
      SET storage_id = rec.object_sid, root_id = rec.home_root
      WHERE user_id = rec.user_id;
    RAISE NOTICE 'repaired user %', rec.user_id;
  END LOOP;
END $$;
EOSQL
}

# =============================================================================
# _ensure_nextcloud_portal_embedding_ingress — allow kernel portal to iframe Files
#
# Nextcloud is a kernel Helm release (not an AppProfile ingress). The operator
# does not manage its Ingress; CSP must be set in nextcloud-base-values and
# patched here so update.sh applies immediately without recreating the Release.
# =============================================================================
_ensure_nextcloud_portal_embedding_ingress() {
    local ns="${1:?}"
    local domain="${KERNEL_DOMAIN:?KERNEL_DOMAIN must be set}"
    local ingress_name="nextcloud-dev-aio"
    local snippet
    snippet=$(printf 'proxy_hide_header X-Frame-Options;\nproxy_hide_header Content-Security-Policy;\nadd_header Content-Security-Policy "frame-ancestors '\''self'\'' https://portal.%s" always;' "${domain}")

    if ! kubectl get ingress "${ingress_name}" -n "${ns}" >/dev/null 2>&1; then
        info "Ingress ${ingress_name} not found — portal embedding will apply on next Helm sync"
        return 0
    fi

    local current
    current=$(kubectl get ingress "${ingress_name}" -n "${ns}" \
        -o jsonpath='{.metadata.annotations.nginx\.ingress\.kubernetes\.io/configuration-snippet}' 2>/dev/null || true)
    if [[ "${current}" == "${snippet}" ]]; then
        info "Nextcloud ingress portal embedding CSP already correct"
        return 0
    fi

    info "Patching ${ingress_name} ingress for portal iframe embedding..."
    kubectl annotate ingress "${ingress_name}" -n "${ns}" \
        "nginx.ingress.kubernetes.io/configuration-snippet=${snippet}" \
        --overwrite >/dev/null
    success "Nextcloud ingress allows portal.${domain} in frame-ancestors"
}

# _apply_kernel_manifest_dir applies kernel service manifests from manifest_dir.
# nubus uses kustomize (configMapGenerator); kubectl apply -f dir/ fails on
# kustomization.yaml with "no matches for kind Kustomization".
# mode=all: ConfigMaps, ExternalSecrets, Ingresses, and Release CRs.
# mode=release: only release.yaml (after all other manifests are current).
_apply_kernel_manifest_dir() {
    local manifest_dir="$1"
    local mode="${2:-all}"

    if [[ -f "${manifest_dir}/kustomization.yaml" ]]; then
        kubectl apply -k "${manifest_dir}" >/dev/null
        return 0
    fi

    if [[ "${mode}" == "release" && -f "${manifest_dir}/release.yaml" ]]; then
        kubectl apply -f "${manifest_dir}/release.yaml" >/dev/null
        return 0
    fi

    while IFS= read -r -d '' f; do
        kubectl apply -f "${f}" >/dev/null
    done < <(find "${manifest_dir}" -maxdepth 1 -name '*.yaml' \
        ! -name 'kustomization.yaml' -print0 | sort -z)
}

# =============================================================================
# reconcile_nextcloud_office — ensure Collabora / richdocuments is configured
#
# nextcloud-management init (wave 9) configures richdocuments before Collabora
# (wave 12) is reachable, so doc_format and WOPI settings are often incomplete.
# The Nextcloud AIO postStart hook in nextcloud-base-values keeps them correct
# across restarts; this function applies the ConfigMap and re-runs the occ steps
# on the live pod so existing clusters converge without manual kubectl exec.
#
# Idempotent — safe to call from install.sh and update.sh --nextcloud-office.
# =============================================================================
reconcile_nextcloud_office() {
    local env="${ENV:-dev}"
    local ns="gentian-${env}"
    local domain="${KERNEL_DOMAIN:?KERNEL_DOMAIN must be set}"
    local manifest_dir="${SCRIPT_DIR}/kernel/services/nextcloud/manifests/${env}"
    local dry_run="${GENTIAN_DRY_RUN:-0}"

    banner "Nextcloud Office reconciliation (Collabora / richdocuments)"

    if [[ ! -d "${manifest_dir}" ]]; then
        warn "No nextcloud manifests for env=${env} — skipping"
        return 0
    fi

    if [[ "${dry_run}" == "1" ]]; then
        info "[dry-run] Would apply ${manifest_dir}/ ConfigMaps and restart nextcloud-dev-aio"
    else
        info "Applying nextcloud manifests (ConfigMaps)..."
        while IFS= read -r -d '' f; do
            kubectl apply -f "${f}" >/dev/null
        done < <(find "${manifest_dir}" -maxdepth 1 -name '*.yaml' \
            ! -name 'kustomization.yaml' -print0 | sort -z)

        # Restart the web pod so the postStart lifecycle hook re-runs. Do NOT
        # delete/recreate the Crossplane Release — that triggers a Helm upgrade
        # that can break LDAP user file mounts (HTTP 500 on /apps/files/).
        _ensure_nextcloud_portal_embedding_ingress "${ns}"

        if kubectl get deployment nextcloud-dev-aio -n "${ns}" >/dev/null 2>&1; then
            info "Restarting nextcloud-dev-aio to apply lifecycle hook changes..."
            kubectl rollout restart deployment/nextcloud-dev-aio -n "${ns}" >/dev/null
        fi
    fi

    if [[ "${dry_run}" == "1" ]]; then
        info "[dry-run] Would configure richdocuments on nextcloud-dev-aio pod"
        return 0
    fi

    if ! kubectl get deployment nextcloud-dev-aio -n "${ns}" >/dev/null 2>&1; then
        warn "nextcloud-dev-aio not deployed yet — office config will apply on first pod start"
        return 0
    fi

    info "Waiting for nextcloud-dev-aio rollout (up to 5m)..."
    kubectl rollout status deployment/nextcloud-dev-aio -n "${ns}" --timeout=5m \
        >/dev/null 2>&1 || warn "nextcloud-dev-aio rollout still in progress"

    _repair_nextcloud_object_home_mounts

    if kubectl get deployment collabora -n "${ns}" >/dev/null 2>&1; then
        info "Waiting for collabora rollout (up to 3m)..."
        kubectl rollout status deployment/collabora -n "${ns}" --timeout=3m \
            >/dev/null 2>&1 || warn "collabora rollout still in progress"
    fi

    if ! kubectl exec -n "${ns}" deploy/nextcloud-dev-aio -- \
        php /var/www/html/occ app:list --enabled 2>/dev/null | grep -q '  - richdocuments:'; then
        warn "richdocuments app not enabled — skipping office config"
        return 0
    fi

    local collabora_host="collabora.${ns}.svc.cluster.local:9980"
    local public_wopi="https://office.${domain}"

    info "Applying richdocuments office settings on live pod..."
    kubectl exec -n "${ns}" deploy/nextcloud-dev-aio -- \
        php /var/www/html/occ richdocuments:update-empty-templates >/dev/null 2>&1 || true
    kubectl exec -n "${ns}" deploy/nextcloud-dev-aio -- \
        php /var/www/html/occ config:app:set richdocuments doc_format --value=odf >/dev/null
    kubectl exec -n "${ns}" deploy/nextcloud-dev-aio -- \
        php /var/www/html/occ config:app:set richdocuments wopi_url \
        --value="http://${collabora_host}" >/dev/null
    kubectl exec -n "${ns}" deploy/nextcloud-dev-aio -- \
        php /var/www/html/occ config:app:set richdocuments public_wopi_url \
        --value="${public_wopi}" >/dev/null

    if kubectl exec -n "${ns}" deploy/nextcloud-dev-aio -- \
        php /var/www/html/occ richdocuments:activate-config 2>&1 | grep -q 'Detected WOPI server'; then
        success "Nextcloud Office configured (doc_format=odf, Collabora WOPI healthy)"
    else
        warn "richdocuments:activate-config did not confirm Collabora — check collabora pod and ingress"
    fi
}


# =============================================================================
# Polls every 15s for up to ${VERIFY_TIMEOUT:-600}s. Considers the platform

# =============================================================================
# wait_for_setup_iam_job — wait for nubus-dev-setup-iam-templates (ArgoCD hook)
#
# The job is deployed by nubus-manifests-dev (wave 21) as a PostSync hook.
# It must run after stack-data-ums has registered opendesk extended attributes.
# Returns 0 when the job completed successfully, 1 on timeout or failure.
# =============================================================================
wait_for_setup_iam_job() {
    local ns="gentian-${ENV:-dev}"
    local job="nubus-${ENV:-dev}-setup-iam-templates"
    local timeout="${SETUP_IAM_TIMEOUT:-300}"
    local elapsed=0
    local interval=10

    banner "Waiting for ${job} (up to ${timeout}s)"

    info "Waiting for job ${job} to appear in ${ns}..."
    while ! kubectl get "job/${job}" -n "${ns}" >/dev/null 2>&1; do
        if (( elapsed >= timeout )); then
            warn "Job ${job} did not appear within ${timeout}s."
            warn "  Check: kubectl get application nubus-manifests-${ENV:-dev} -n argocd"
            return 1
        fi
        sleep "$interval"
        elapsed=$((elapsed + interval))
    done

    info "Waiting for job ${job} to complete..."
    if kubectl wait "job/${job}" -n "${ns}" \
            --for=condition=complete --timeout=$((timeout - elapsed))s 2>/dev/null; then
        success "Job ${job} completed."
        return 0
    fi

    warn "Job ${job} did not complete successfully."
    warn "  kubectl logs -n ${ns} -l job-name=${job} --tail=40"
    warn "  Recover with: ./update.sh --setup-iam"
    return 1
}

# =============================================================================
# 16a. Verify Keycloak iframe policy (portal-embedded OIDC)
# =============================================================================
# Waits for the gentian-os KeycloakPlatformReconciler to patch id.<kernel> ingress
# and for browser-security Jobs to clear X-Frame-Options on Keycloak realms.
verify_keycloak_iframe_policy() {
    banner "Step 16a — Verifying Keycloak iframe policy"

    local kernel_domain="${KERNEL_DOMAIN:-}"
    if [[ -z "$kernel_domain" ]]; then
        warn "KERNEL_DOMAIN unset — skipping Keycloak iframe verification."
        return 0
    fi

    local services_ns="${SERVICES_NAMESPACE:-gentian-dev}"
    local kernel_ns="${KERNEL_NAMESPACE:-gentian-dev}"
    local ingress_name="${KEYCLOAK_PROXY_INGRESS_NAME:-nubus-dev-keycloak-extensions-proxy}"
    local timeout="${KEYCLOAK_FRAME_VERIFY_TIMEOUT:-300}"
    local interval=10
    local elapsed=0

    info "Waiting for Keycloak ingress ${ingress_name} and operator frame-ancestors patch..."

    while [[ $elapsed -lt $timeout ]]; do
        local snippet=""
        snippet=$(kubectl get ingress "$ingress_name" -n "$services_ns" \
            -o jsonpath='{.metadata.annotations.nginx\.ingress\.kubernetes\.io/configuration-snippet}' \
            2>/dev/null || true)

        if [[ -n "$snippet" ]] \
            && [[ "$snippet" == *"frame-ancestors"* ]] \
            && [[ "$snippet" == *"https://portal.${kernel_domain}"* ]]; then
            success "Keycloak ingress allows portal.${kernel_domain} in frame-ancestors."
            break
        fi

        printf "  …waiting for Keycloak ingress CSP (%ds/%ds)\n" "$elapsed" "$timeout"
        sleep "$interval"
        elapsed=$((elapsed + interval))
    done

    if [[ $elapsed -ge $timeout ]]; then
        warn "Keycloak ingress frame-ancestors not converged within ${timeout}s."
        warn "Portal-embedded OIDC (WinBox) may show Firefox iframe errors until the operator reconciles."
        return 1
    fi

    local bs_jobs
    bs_jobs=$(kubectl get jobs -n "$kernel_ns" \
        -l 'gentianos.io/keycloak-browser-security=1' \
        --no-headers 2>/dev/null | wc -l || echo 0)
    if [[ "$bs_jobs" -gt 0 ]]; then
        info "Waiting for Keycloak browser-security header jobs..."
        elapsed=0
        while [[ $elapsed -lt $timeout ]]; do
            local incomplete
            incomplete=$(kubectl get jobs -n "$kernel_ns" \
                -l 'gentianos.io/keycloak-browser-security=1' \
                --no-headers 2>/dev/null \
                | awk '$2 !~ /1\/1/ {print}' | wc -l || echo 0)
            if [[ "$incomplete" -eq 0 ]]; then
                success "Keycloak browser-security header jobs completed."
                return 0
            fi
            sleep "$interval"
            elapsed=$((elapsed + interval))
        done
        warn "Keycloak browser-security jobs did not all complete within ${timeout}s."
        return 1
    fi

    info "No browser-security jobs yet (no Tenant CRs?) — ingress CSP is ready."
    return 0
}

# =============================================================================
# 16. Verify ArgoCD Applications
# =============================================================================
# healthy when every Application is Synced+Healthy. Returns 0 on healthy,
# 1 if some apps are still degraded/out-of-sync after the timeout.
verify_argocd_apps() {
    banner "Step 16 — Verifying ArgoCD Applications"

    # Restart the application-controller once to clear any stale resource
    # health cached during the OpenBao seal-migration window (when ESO
    # transiently couldn't read secrets). Without this, Applications
    # whose underlying resources are now healthy can stay reported as
    # Degraded indefinitely because ArgoCD doesn't re-evaluate cached
    # resource health unless the resource generation changes.
    info "Restarting argocd-application-controller to clear stale health cache..."
    kubectl rollout restart statefulset -n argocd argocd-application-controller \
        >/dev/null 2>&1 || true
    kubectl rollout status  statefulset -n argocd argocd-application-controller \
        --timeout=120s >/dev/null 2>&1 || warn "application-controller rollout did not become ready in 120s; continuing."

    local timeout=${VERIFY_TIMEOUT:-600}
    local interval=15
    local elapsed=0
    local total synced healthy bad_lines
    info "Waiting up to ${timeout}s for all Applications to become Synced+Healthy..."

    while true; do
        # If no Applications exist yet, keep waiting (root ApplicationSet may
        # still be generating children).
        total=$(kubectl get applications -n argocd --no-headers 2>/dev/null | wc -l)
        if [[ "$total" -eq 0 ]]; then
            if [[ $elapsed -ge $timeout ]]; then
                warn "No ArgoCD Applications appeared within ${timeout}s."
                VERIFY_STATUS="empty"
                return 1
            fi
            printf "  …no Applications yet (%ds/%ds)\n" "$elapsed" "$timeout"
            sleep "$interval"; elapsed=$((elapsed + interval))
            continue
        fi

        synced=$(kubectl get applications -n argocd \
            -o jsonpath='{range .items[?(@.status.sync.status=="Synced")]}{.metadata.name}{"\n"}{end}' \
            2>/dev/null | wc -l)
        healthy=$(kubectl get applications -n argocd \
            -o jsonpath='{range .items[?(@.status.health.status=="Healthy")]}{.metadata.name}{"\n"}{end}' \
            2>/dev/null | wc -l)

        printf "  apps=%d synced=%d healthy=%d (%ds/%ds)\n" \
            "$total" "$synced" "$healthy" "$elapsed" "$timeout"

        if [[ "$synced" -eq "$total" && "$healthy" -eq "$total" ]]; then
            success "All ${total} ArgoCD Applications are Synced and Healthy."
            VERIFY_STATUS="ok"
            VERIFY_TOTAL="$total"
            return 0
        fi

        if [[ $elapsed -ge $timeout ]]; then
            bad_lines=$(kubectl get applications -n argocd \
                -o custom-columns='NAME:.metadata.name,SYNC:.status.sync.status,HEALTH:.status.health.status' \
                --no-headers 2>/dev/null | awk '$2!="Synced" || $3!="Healthy"')
            warn "Timed out after ${timeout}s with ${total} Applications, ${synced} Synced, ${healthy} Healthy."
            echo "  Degraded / out-of-sync Applications:"
            while IFS= read -r line; do
                [[ -n "$line" ]] && echo "    $line"
            done <<< "$bad_lines"
            VERIFY_STATUS="degraded"
            VERIFY_TOTAL="$total"
            VERIFY_BAD="$bad_lines"
            return 1
        fi

        sleep "$interval"; elapsed=$((elapsed + interval))
    done
}

# =============================================================================
# Summary — portal admin credentials for install output
# =============================================================================
# Portal login uses Keycloak kernel realm LDAP with mailPrimaryAddress (iam.md §1.2),
# not the LDAP uid "Administrator".
resolve_portal_admin_email() {
    local ns="gentian-${ENV:-dev}"
    local release="nubus-${ENV:-dev}"
    local ldap_pod email=""

    ldap_pod=$(kubectl get pod -n "${ns}" \
        -o jsonpath="{.items[?(@.metadata.name==\"${release}-ldap-server-primary-0\")].metadata.name}" \
        2>/dev/null || true)
    if [[ -n "${ldap_pod}" ]]; then
        email=$(kubectl exec -n "${ns}" "${ldap_pod}" -c main -- \
            ldapsearch -Y EXTERNAL -H ldapi:/// \
            -b 'uid=Administrator,cn=users,dc=swp-ldap,dc=internal' mailPrimaryAddress 2>/dev/null \
            | awk -F': ' '/^mailPrimaryAddress:/ {print $2; exit}' || true)
    fi
    if [[ -z "${email}" && -n "${KERNEL_DOMAIN:-}" ]]; then
        email="administrator@${KERNEL_DOMAIN}"
    fi
    echo "${email}"
}

resolve_portal_admin_password() {
    local ns="gentian-${ENV:-dev}"
    kubectl get secret nubus-credentials -n "${ns}" \
        -o jsonpath='{.data.default-admin-password}' 2>/dev/null | base64 -d 2>/dev/null || true
}

# =============================================================================
# Summary
# =============================================================================
print_summary() {
    local argocd_pw
    local argocd_url
    local cluster_admin_pw
    local cluster_admin_user
    local keycloak_admin_pw
    local nubus_secret_ns
    nubus_secret_ns="gentian-${ENV:-dev}"

    argocd_pw=$(kubectl get secret argocd-initial-admin-secret -n argocd \
                    -o jsonpath='{.data.password}' 2>/dev/null | base64 -d 2>/dev/null || echo "(not-ready)")
    cluster_admin_user=$(resolve_portal_admin_email)
    cluster_admin_pw=$(resolve_portal_admin_password)
    [[ -n "${cluster_admin_pw}" ]] || cluster_admin_pw="(not-ready)"
    keycloak_admin_pw=$(kubectl get secret nubus-credentials -n "${nubus_secret_ns}" \
                        -o jsonpath='{.data.keycloak-admin-password}' 2>/dev/null | base64 -d 2>/dev/null || echo "(not-ready)")
    argocd_url=$(resolve_argocd_url)
    portal_url="https://portal.${KERNEL_DOMAIN}/login/"
    keycloak_url="https://id.${KERNEL_DOMAIN}"

    echo ""
    if [[ "${VERIFY_STATUS:-unknown}" == "ok" ]]; then
        echo "  ✔ ArgoCD reachable"
        echo "  ✔ All Applications Synced + Healthy"
        echo "  ✔ AppCatalogue CRD installed"
        echo "  ✔ gentian-os orchestrator running (Tenant CRD Established)"
        echo "  ✔ Cluster admin credentials materialized (nubus-credentials)"
        echo ""

        echo -e "${GREEN}╔══════════════════════════════════════════════════════════╗${NC}"
        echo -e "${GREEN}║  ✅  Gentian OS bootstrap complete — all systems healthy! ║${NC}"
        echo -e "${GREEN}╠══════════════════════════════════════════════════════════╣${NC}"
        echo -e "${GREEN}║  Portal URL   : ${portal_url}${NC}"
        echo -e "${GREEN}║  Portal login : ${cluster_admin_user:-administrator@${KERNEL_DOMAIN}} / ${cluster_admin_pw}${NC}"
        echo -e "${GREEN}║  Keycloak URL : ${keycloak_url}${NC}"
        echo -e "${GREEN}║  Keycloak login (master realm) : admin / ${keycloak_admin_pw}${NC}"
        echo -e "${GREEN}║  ArgoCD URL   : ${argocd_url}${NC}"
        echo -e "${GREEN}║  ArgoCD login : admin / ${argocd_pw}${NC}"
        echo -e "${GREEN}║  Network mode : ${NETWORK_MODE:-tunnel}${NC}"
        echo -e "${GREEN}║  Routing mode : ${ROUTING_MODE:-ingress}${NC}"
        echo -e "${GREEN}║  Applications : ${VERIFY_TOTAL:-?} Synced + Healthy${NC}"
        echo -e "${GREEN}║  Tenants      : none (provision when ready)              ║${NC}"
        echo -e "${GREEN}╚══════════════════════════════════════════════════════════╝${NC}"
        echo ""
        echo "  Retrieve credentials later:"
        echo "    kubectl get secret nubus-credentials -n ${nubus_secret_ns} -o jsonpath='{.data.default-admin-password}' | base64 -d"
        echo ""
        echo "  Monitor sync:    kubectl get applications -n argocd"
        echo "  List tenants:    kubectl gentian tenants list"
        echo "  Provision tenant: kubectl gentian tenants deploy demo"
        echo "                    (activates clusters/<cluster>/definitions/<tenant>/<stage>/ on first run)"
        echo "  Undeploy tenant: kubectl gentian tenants undeploy demo"
        echo "  List apps:       kubectl gentian apps list"
        echo "  Install apps:    kubectl gentian apps install <profile> --tenant <tenant>"
        echo ""
        echo -e "${GREEN}🎉  Gentian OS successfully installed. Welcome aboard!${NC}"
    else
        echo -e "${YELLOW}╔══════════════════════════════════════════════════════════╗${NC}"
        echo -e "${YELLOW}║  ⚠  Gentian OS bootstrap finished with degraded Apps     ║${NC}"
        echo -e "${YELLOW}╠══════════════════════════════════════════════════════════╣${NC}"
        echo -e "${YELLOW}║  Portal URL   : ${portal_url}${NC}"
        echo -e "${YELLOW}║  Portal login : ${cluster_admin_user:-administrator@${KERNEL_DOMAIN}} / ${cluster_admin_pw}${NC}"
        echo -e "${YELLOW}║  Keycloak URL : ${keycloak_url}${NC}"
        echo -e "${YELLOW}║  Keycloak login (master realm) : admin / ${keycloak_admin_pw}${NC}"
        echo -e "${YELLOW}║  ArgoCD URL   : ${argocd_url}${NC}"
        echo -e "${YELLOW}║  ArgoCD login : admin / ${argocd_pw}${NC}"
        echo -e "${YELLOW}║  Network mode : ${NETWORK_MODE:-tunnel}${NC}"
        echo -e "${YELLOW}║  Status       : ${VERIFY_STATUS:-unknown} (${VERIFY_TOTAL:-0} apps)${NC}"
        echo -e "${YELLOW}╚══════════════════════════════════════════════════════════╝${NC}"
        echo ""
        echo "  Inspect failing Applications:"
        if [[ -n "${VERIFY_BAD:-}" ]]; then
            while IFS= read -r line; do
                [[ -n "$line" ]] && echo "    $line"
            done <<< "${VERIFY_BAD}"
        else
            echo "    kubectl get applications -n argocd"
            echo "    kubectl describe application -n argocd <name>"
        fi
        echo ""
        echo "  Retrieve credentials later:"
        echo "    kubectl get secret nubus-credentials -n ${nubus_secret_ns} -o jsonpath='{.data.default-admin-password}' | base64 -d"
        echo ""
        echo "  Re-run verification only:"
        echo "    VERIFY_TIMEOUT=600 ./install.sh --verify-only   # (or just wait + re-check)"
        echo "  List apps once synced:"
        echo "    kubectl gentian apps list"
    fi
}

# =============================================================================
# Main
# =============================================================================
main() {
    echo ""
    echo -e "${CYAN}╔══════════════════════════════════════════════════════════╗${NC}"
    echo -e "${CYAN}║     Gentian OS — Fresh Cluster Bootstrap                 ║${NC}"
    echo -e "${CYAN}╚══════════════════════════════════════════════════════════╝${NC}"
    echo ""

    parse_args "$@"
    if [[ "${INSTALL_VERIFY_ONLY}" == "1" ]]; then
        verify_argocd_apps || true
        print_summary
        return 0
    fi
    load_operator_config
    load_creds_cache
    [[ "${INSTALL_VALIDATE_ONLY}" == "1" ]] && { load_install_state; load_deployments_cluster_settings; try_load_creds_from_openbao; validate_config; }
    load_install_state
    try_load_creds_from_openbao
    prompt_credentials
    prompt_app_repos
    prompt_kernel_domain
    prompt_network_mode
    prompt_kernel_secrets
    check_prereqs
    install_tools
    create_namespaces
    prewarm_cluster
    install_cert_manager
    install_kernel_cert_resources
    install_envoy_gateway
    install_eso
    install_argocd
    setup_argocd_repos
    install_argocd_image_updater
    bootstrap_transit_app
    init_openbao_transit
    bootstrap_argocd_apps
    init_openbao
    bao_bootstrap
    seed_secrets
    install_kernel_wildcard
    bootstrap_root_appset
    install_app_catalogue
    install_appprofiles_sync
    install_orchestrator
    verify_argocd_apps || true
    print_summary
}

# Allow this file to be sourced as a function library without running main().
# Set GENTIAN_INSTALL_LIB_ONLY=1 before sourcing (done by install.sh).
[[ "${GENTIAN_INSTALL_LIB_ONLY:-0}" == "1" ]] || main "$@"
