#!/usr/bin/env bash
# =============================================================================
# scripts/lib/common.sh — Shared install helpers: logging, config, cluster prep, verification.
# =============================================================================
# Sourced by scripts/lib/load.sh. Do not execute directly.
# =============================================================================

# ─── Colour helpers ──────────────────────────────────────────────────────────
RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'; CYAN='\033[0;36m'; NC='\033[0m'
info()    { echo -e "${CYAN}[INFO]${NC}  $*"; }
success() { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; }
banner()  { echo -e "\n${CYAN}══════════════════════════════════════════════════${NC}"; echo -e "${CYAN}  $*${NC}"; echo -e "${CYAN}══════════════════════════════════════════════════${NC}\n"; }

# Retry kubectl when the API server is temporarily unreachable (common on remote
# clusters or flaky client networks). Only connection-level failures are retried;
# resource errors (NotFound, wait timeouts, etc.) fail immediately.
# Override attempts/delay via KUBECTL_RETRY_ATTEMPTS / KUBECTL_RETRY_DELAY_SECS.
_kubectl_retry() {
    local attempts="${KUBECTL_RETRY_ATTEMPTS:-12}"
    local delay="${KUBECTL_RETRY_DELAY_SECS:-5}"
    local n=1 rc=0 err=""
    local err_file
    err_file="$(mktemp)"
    # shellcheck disable=SC2064
    trap "rm -f '${err_file}'" RETURN

    while (( n <= attempts )); do
        if kubectl "$@" 2>"${err_file}"; then
            return 0
        fi
        rc=$?
        err="$(<"${err_file}")"
        if [[ -n "$err" ]]; then
            printf '%s\n' "$err" >&2
        fi
        if ! [[ "$err" =~ (connection[[:space:]]refused|connection[[:space:]]reset|TLS[[:space:]]handshake[[:space:]]timeout|timeout[[:space:]]awaiting[[:space:]]response[[:space:]]headers|Unable[[:space:]]to[[:space:]]connect[[:space:]]to[[:space:]]the[[:space:]]server|no[[:space:]]route[[:space:]]to[[:space:]]host|i/o[[:space:]]timeout|dial[[:space:]]tcp) ]]; then
            return "$rc"
        fi
        if (( n >= attempts )); then
            return "$rc"
        fi
        warn "kubectl failed (attempt ${n}/${attempts}): transient API error"
        warn "  Retrying in ${delay}s..."
        sleep "$delay"
        n=$((n + 1))
    done
}

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
# SCRIPT_DIR is already set by the outer install.sh/update.sh/uninstall.sh to
# the repo root before this file is sourced. Do not overwrite it. The ":-"
# default only applies when SCRIPT_DIR is unset or empty.
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
    SMTP_RELAY_USERNAME
    SMTP_RELAY_PASSWORD
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
    ROUTING_MODE
    GENTIAN_APPS_REPO
    GENTIAN_APPS_BRANCH
    GENTIAN_DEPLOYMENTS_REPO
    GENTIAN_DEPLOYMENTS_BRANCH
    GENTIAN_DEPLOYMENTS_PATH
    GENTIAN_DEPLOYMENTS_CLUSTER
    GENTIAN_DEPLOYMENTS_STAGE
    GENTIAN_DEPLOYMENTS_GIT_TOKEN
    GENTIAN_DEPLOYMENTS_GIT_USERNAME
    GITHUB_ACTIONS_OS_REPO
    CI_BOT_PAT
    ARGOCD_SERVER
    ARGOCD_TOKEN
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
export ESO_CHART_VERSION="2.4.1"
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
    local deployments_root cluster
    local cluster_settings_file

    deployments_root="${GENTIAN_DEPLOYMENTS_PATH:-${HOME}/.gentian/gentian-deployments}"
    cluster="${GENTIAN_DEPLOYMENTS_CLUSTER:-default-cluster}"
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
        echo "  [OK]       MAIL_SERVICE_MODE=${MAIL_SERVICE_MODE}  (install-time; invitation mail uses in-cluster Postfix when kernel)"
    fi
    if [[ "${MAIL_SERVICE_MODE}" == "external" ]]; then
        _req_from EXTERNAL_SMTP_HOST "External SMTP host (e.g. smtp.gmail.com)" "${cluster_settings_file}"
        _opt_from EXTERNAL_SMTP_PORT "External SMTP port (default 587)" "${cluster_settings_file}"
        _req_from SMTP_RELAY_USERNAME "SMTP username (e.g. Gmail address)" "${INSTALL_SECRETS_FILE}"
        _req_from SMTP_RELAY_PASSWORD "SMTP password (e.g. Gmail App Password)" "${INSTALL_SECRETS_FILE}"
    else
        echo "  [OK]       SMTP_RELAY_USERNAME  (not required for MAIL_SERVICE_MODE=${MAIL_SERVICE_MODE})"
        echo "  [OK]       SMTP_RELAY_PASSWORD  (not required for MAIL_SERVICE_MODE=${MAIL_SERVICE_MODE})"
    fi

    NETWORK_MODE="${NETWORK_MODE:-tunnel}"
    if ! mail_network_mode_compatible "${MAIL_SERVICE_MODE}" "${NETWORK_MODE}"; then
        echo "  [INVALID]  MAIL_SERVICE_MODE=kernel with NETWORK_MODE=tunnel"
        echo "             Kernel mail (Postfix/Dovecot) needs a reachable SMTP ingress; use MAIL_SERVICE_MODE=external with an SMTP relay on tunnel clusters."
        (( errors++ )) || true
    elif [[ "${MAIL_SERVICE_MODE}" == "kernel" ]]; then
        echo "  [OK]       MAIL_SERVICE_MODE=kernel with NETWORK_MODE=${NETWORK_MODE}"
    fi

    # KERNEL_DOMAIN has exactly one authored copy — the cluster's Crossplane
    # Claim (claims/cluster.yaml) — not cluster-settings.env. Resolve it the
    # same way the real install/update flow does before judging it missing.
    resolve_kernel_domain_from_claim
    if [[ -z "${KERNEL_DOMAIN:-}" ]]; then
        if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" ]]; then
            echo "  [MISSING]  KERNEL_DOMAIN  — platform-wide DNS suffix; not resolvable from claims/cluster.yaml (cluster not bootstrapped yet) and GENTIAN_NONINTERACTIVE=1 (export KERNEL_DOMAIN or set it in ${INSTALL_CONFIG_FILE})"
            (( errors++ )) || true
        else
            echo "  [PENDING]  KERNEL_DOMAIN  — not yet bootstrapped; no claims/cluster.yaml for this cluster yet. Will be prompted for interactively during install (see gentian-os/docs/deployment.md §1)."
        fi
    elif [[ ! "${KERNEL_DOMAIN}" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ ]]; then
        echo "  [INVALID]  KERNEL_DOMAIN=${KERNEL_DOMAIN}  — must match ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?\$"
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
    _opt_from GENTIAN_APPS_REPO       "defaults to https://git.example.domain/gentian-apps" "${INSTALL_CONFIG_FILE}"
    _opt_from GENTIAN_APPS_BRANCH     "defaults to 'main'" "${INSTALL_CONFIG_FILE}"
    _opt_from GENTIAN_DEPLOYMENTS_REPO    "defaults to https://git.example.domain/gentian-deployments" "${INSTALL_CONFIG_FILE}"
    _opt_from GENTIAN_DEPLOYMENTS_BRANCH  "defaults to 'main'" "${INSTALL_CONFIG_FILE}"
    _opt_from GENTIAN_DEPLOYMENTS_GIT_TOKEN "GitHub PAT for operator in-cluster git push (install.secrets.env)" "${INSTALL_SECRETS_FILE}"
    _opt_from GENTIAN_DEPLOYMENTS_GIT_USERNAME "defaults to x-access-token for GitHub PATs" "${INSTALL_SECRETS_FILE}"
    _opt_from CI_BOT_PAT "GitHub PAT for gentian-os image-pin workflows (install.secrets.env)" "${INSTALL_SECRETS_FILE}"
    _opt_from ARGOCD_SERVER "ArgoCD URL for pin-workflow sync (optional; derived from KERNEL_DOMAIN)" "${INSTALL_SECRETS_FILE}"
    _opt_from ARGOCD_TOKEN "ArgoCD API token for pin-workflow sync (optional)" "${INSTALL_SECRETS_FILE}"
    _opt_from GITHUB_ACTIONS_OS_REPO "GitHub repo for Actions secrets upload (install.env)" "${INSTALL_CONFIG_FILE}"

    LLM_SUPPORT="${LLM_SUPPORT:-false}"
    if [[ "${LLM_SUPPORT}" != "true" && "${LLM_SUPPORT}" != "false" ]]; then
        echo "  [INVALID]  LLM_SUPPORT=${LLM_SUPPORT}  — must be 'true' or 'false' (set in ${INSTALL_CONFIG_FILE} or ${cluster_settings_file})"
        (( errors++ )) || true
    else
        echo "  [OK]       LLM_SUPPORT=${LLM_SUPPORT}"
    fi

    GPU_ACCELERATION="${GPU_ACCELERATION:-false}"
    if [[ "${GPU_ACCELERATION}" != "true" && "${GPU_ACCELERATION}" != "false" ]]; then
        echo "  [INVALID]  GPU_ACCELERATION=${GPU_ACCELERATION}  — must be 'true' or 'false' (set in ${INSTALL_CONFIG_FILE} or ${cluster_settings_file})"
        (( errors++ )) || true
    else
        echo "  [OK]       GPU_ACCELERATION=${GPU_ACCELERATION}"
    fi

    if [[ "${GPU_ACCELERATION}" == "true" ]]; then
        if [[ "${LLM_SUPPORT}" != "true" ]]; then
            echo "  [INVALID]  GPU_ACCELERATION=true requires LLM_SUPPORT=true (set in ${INSTALL_CONFIG_FILE} or ${cluster_settings_file})"
            (( errors++ )) || true
        fi

        # Verify that the cluster actually has GPU resources available
        if kubectl cluster-info --request-timeout=5s >/dev/null 2>&1; then
            local gpus
            gpus=$(kubectl get nodes -o jsonpath='{.items[*].status.allocatable.nvidia\.com/gpu}' 2>/dev/null || true)
            local total_gpus=0
            for gpu in ${gpus}; do
                if [[ "${gpu}" =~ ^[0-9]+$ ]]; then
                    total_gpus=$((total_gpus + gpu))
                fi
            done
            if (( total_gpus > 0 )); then
                echo "  [OK]       GPU_ACCELERATION checks: cluster has ${total_gpus} allocatable GPU(s)"
            else
                echo "  [INVALID]  GPU_ACCELERATION=true but no nodes in the cluster report allocatable 'nvidia.com/gpu' resources."
                (( errors++ )) || true
            fi
        else
            echo "  [WARN]     GPU_ACCELERATION=true: cluster is unreachable, skipping cluster GPU resource check"
            (( warnings++ )) || true
        fi
    fi

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
        for var in MASTER_PASSWORD SMTP_RELAY_USERNAME SMTP_RELAY_PASSWORD CF_API_TOKEN \
                   GENTIAN_DEPLOYMENTS_GIT_TOKEN CI_BOT_PAT ARGOCD_TOKEN; do
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
        if [[ -z "${SMTP_RELAY_USERNAME:-}" ]]; then
            read -rp  "  SMTP_RELAY_USERNAME (e.g. user@gmail.com): " SMTP_RELAY_USERNAME; echo ""
            export SMTP_RELAY_USERNAME
            prompted=1
        fi
        if [[ -z "${SMTP_RELAY_PASSWORD:-}" ]]; then
            read -rp "  SMTP_RELAY_PASSWORD (e.g. Gmail App Password): " SMTP_RELAY_PASSWORD; echo ""
            export SMTP_RELAY_PASSWORD
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
# Defaults use example.domain placeholders — override via GENTIAN_*_REPO env vars.
# Persist results to ~/.gentian/config (bash-sourceable) so the kubectl-gentian
# plugin can locate the deployments repo when running `kubectl gentian apps install/uninstall`.
# =============================================================================
prompt_app_repos() {
    local default_apps_repo="https://git.example.domain/gentian-apps"
    local default_apps_branch="main"
    local default_deploy_repo="https://git.example.domain/gentian-deployments"
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
    : "${GENTIAN_PRO_REPO:=https://git.example.domain/gentian-pro}"
    : "${GENTIAN_PRO_BRANCH:=main}"
    : "${GENTIAN_DEPLOYMENTS_REPO:=${default_deploy_repo}}"
    : "${GENTIAN_DEPLOYMENTS_BRANCH:=${default_deploy_branch}}"
        : "${GENTIAN_DEPLOYMENTS_CLUSTER:=${default_deploy_cluster}}"
        : "${GENTIAN_DEPLOYMENTS_STAGE:=${default_deploy_stage}}"
    : "${GENTIAN_DEPLOYMENTS_PATH:=${HOME}/.gentian/gentian-deployments}"
    export GENTIAN_APPS_REPO GENTIAN_APPS_BRANCH GENTIAN_PRO_REPO GENTIAN_PRO_BRANCH \
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

    if [[ -z "${GENTIAN_DEPLOYMENTS_GIT_TOKEN:-}" ]]; then
        warn "GENTIAN_DEPLOYMENTS_GIT_TOKEN not set — in-cluster App Store installs cannot push to gentian-deployments."
        warn "  Add to install.secrets.env when needed."
    fi
    : "${GENTIAN_DEPLOYMENTS_GIT_USERNAME:=x-access-token}"
    export GENTIAN_DEPLOYMENTS_GIT_USERNAME

    if [[ -z "${CI_BOT_PAT:-}" ]]; then
        warn "CI_BOT_PAT not set — gentian-ui image builds cannot auto-pin tags in gentian-os."
        warn "  Add to install.secrets.env when needed."
    fi
    : "${GITHUB_ACTIONS_OS_REPO:=example/gentian-os}"
    export GITHUB_ACTIONS_OS_REPO
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
# which all kernel UIs — Keycloak, Gentian portal, and Argo CD — are served, and
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

# RFC 5737 documentation addresses (TEST-NET-1/2/3).
_is_testnet_ip() {
    local ip="$1"
    [[ "$ip" =~ ^192\.0\.2\.[0-9]+$ || "$ip" =~ ^198\.51\.100\.[0-9]+$ || "$ip" =~ ^203\.0\.113\.[0-9]+$ ]]
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
            warn "will likely fail. Fix CF_API_TOKEN in install.secrets.env and re-run."
        fi
        return
    fi
    info "CF_API_TOKEN not set; skipping kernel wildcard Certificate."
    info "  Add CF_API_TOKEN to install.secrets.env to enable DNS-01 wildcard for *.${KERNEL_DOMAIN}."
}

# =============================================================================
# cleanup_orphaned_kyverno_webhooks — self-heal for a specific, cluster-breaking
# leftover state. Kyverno's MutatingWebhookConfiguration/ValidatingWebhookConfiguration
# objects are cluster-scoped and survive a `kubectl delete namespace kyverno`
# (or any teardown that doesn't go through Kyverno's own Helm uninstall hooks,
# e.g. a manual/partial teardown outside install.sh/uninstall.sh). Kyverno's
# webhooks fail-closed by default, so an orphaned one with no backing service
# blocks ALL matching resource creation cluster-wide — including Crossplane's
# own pods, before Kyverno is ever reinstalled later in the sequence.
#
# Safe to call unconditionally: only acts when the kyverno namespace is
# absent (i.e. Kyverno is not actually running) but its webhook
# registrations remain. A healthy, running Kyverno is left untouched.
# =============================================================================
cleanup_orphaned_kyverno_webhooks() {
    kubectl get namespace kyverno >/dev/null 2>&1 && return 0

    local hooks
    hooks=$(kubectl get mutatingwebhookconfiguration,validatingwebhookconfiguration \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null \
        | grep '^kyverno-' || true)
    [[ -z "${hooks}" ]] && return 0

    warn "Orphaned Kyverno webhook configuration(s) found with no kyverno namespace behind them:"
    warn "  $(printf '%s' "${hooks}" | tr '\n' ' ')"
    warn "  These fail closed by default and block ALL matching resource creation"
    warn "  cluster-wide (e.g. Crossplane's own pods). Removing them now —"
    warn "  Kyverno recreates them cleanly when reinstalled."
    while IFS= read -r hook; do
        [[ -z "${hook}" ]] && continue
        kubectl delete mutatingwebhookconfiguration "${hook}" --ignore-not-found=true 2>/dev/null || true
        kubectl delete validatingwebhookconfiguration "${hook}" --ignore-not-found=true 2>/dev/null || true
    done <<< "${hooks}"
    success "Orphaned Kyverno webhook configuration(s) removed."
}

# =============================================================================
# force_reconcile_failed_helm_releases — self-heal for Crossplane
# helm.crossplane.io Release CRs stuck in Helm's own "failed" state.
#
# provider-helm does not retry on its own: once a `helm upgrade` fails (e.g.
# two ConfigMaps colliding on the same values key — see 334fae7, which sat
# stuck for 2+ hours until manually annotated), the Release's Synced
# condition still reports ReconcileSuccess (Crossplane executed the
# reconcile; the reconcile just happened to conclude "failed") — so nothing
# about the object's own status signals "needs another look" or triggers
# another attempt on its own.
#
# update.sh's --reconcile-releases only covers Release CRs backed by a
# committed kernel/services/*/manifests/${env}/release.yaml — most Release
# CRs in this cluster are Crossplane-composition-generated (owned by
# XApp/XInfraData/XSuze, e.g. Keycloak, OpenFGA, infra-{mariadb,minio,
# postgresql,redis}), which that file-globbing approach can't see at all.
# This checks live Release objects directly instead, regardless of how
# they were created, and force-reconciles (annotate + let Crossplane retry)
# any genuinely in Helm's "failed" state. Safe to call unconditionally —
# a no-op when everything is deployed/healthy.
# =============================================================================
force_reconcile_failed_helm_releases() {
    local failed
    failed=$(kubectl get release.helm.crossplane.io \
        -o jsonpath='{range .items[?(@.status.atProvider.state=="failed")]}{.metadata.name}{"\n"}{end}' \
        2>/dev/null || true)
    [[ -z "${failed}" ]] && return 0

    warn "Crossplane Release CR(s) stuck in Helm 'failed' state (provider-helm does not retry on its own):"
    warn "  $(printf '%s' "${failed}" | tr '\n' ' ')"
    warn "  Forcing a re-reconcile on each..."
    while IFS= read -r name; do
        [[ -z "${name}" ]] && continue
        kubectl annotate release.helm.crossplane.io "${name}" \
            "gentian.io/force-reconcile=$(date +%s)" --overwrite >/dev/null 2>&1 || true
    done <<< "${failed}"
    success "Requested re-reconcile for: $(printf '%s' "${failed}" | tr '\n' ' ')"
    info "  Monitor with: kubectl get release.helm.crossplane.io"
}

# =============================================================================
# 0. Pre-flight checks
# =============================================================================
check_prereqs() {
    banner "Pre-flight checks"

    local missing=0

    # ── CLI tools ─────────────────────────────────────────────────────────────
    local base_tools=(kubectl helm jq yq openssl curl bao)
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
        cleanup_orphaned_kyverno_webhooks
        force_reconcile_failed_helm_releases
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
    if [[ -z "${MASTER_PASSWORD:-}" ]]; then
        error "MASTER_PASSWORD is not set"
        missing=$((missing + 1))
    else
        success "MASTER_PASSWORD set"
    fi

    MAIL_SERVICE_MODE="${MAIL_SERVICE_MODE:-external}"
    export MAIL_SERVICE_MODE
    if [[ "${MAIL_SERVICE_MODE}" == "external" ]]; then
        for var in SMTP_RELAY_USERNAME SMTP_RELAY_PASSWORD; do
            if [[ -z "${!var:-}" ]]; then
                error "$var is required when MAIL_SERVICE_MODE=external"
                missing=$((missing + 1))
            else
                success "$var set"
            fi
        done
        if [[ -z "${EXTERNAL_SMTP_HOST:-}" ]]; then
            error "EXTERNAL_SMTP_HOST is required when MAIL_SERVICE_MODE=external"
            missing=$((missing + 1))
        fi
    else
        info "MAIL_SERVICE_MODE=${MAIL_SERVICE_MODE}: SMTP relay credentials not required (Keycloak uses in-cluster Postfix)"
    fi

    if ! mail_network_mode_compatible "${MAIL_SERVICE_MODE}" "${NETWORK_MODE:-tunnel}"; then
        error "$(mail_network_mode_incompatibility_message)"
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
        if [[ "${ROUTING_MODE:-gateway}" == "gateway" ]]; then
            lb_label='app.kubernetes.io/name=gateway-helm'
            lb_ip=$(kubectl get svc -A -l "${lb_label}" \
                -o jsonpath='{.items[0].status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
        fi
        if [[ -z "$lb_ip" ]]; then
            warn "NETWORK_MODE=static-ip: Envoy Gateway LoadBalancer has no external IP yet."
            warn "  Make sure MetalLB (or a cloud LB) is configured with ${NODE_IP} before traffic can reach the cluster."
        else
            info "Gateway LoadBalancer external IP: ${lb_ip}"
        fi
    fi

    if [[ "${ROUTING_MODE:-gateway}" != "gateway" ]]; then
        error "ROUTING_MODE=${ROUTING_MODE} is no longer supported; use ROUTING_MODE=gateway."
        exit 1
    fi

    resolve_storage_class || exit 1

    success "All pre-flight checks passed."
}

# =============================================================================
# resolve_storage_class — settle on ONE StorageClass name for the whole install
#
# cluster-settings.env documents STORAGE_CLASS as "leave unset to use the
# cluster's own default StorageClass". That promise only holds if something
# actually resolves "unset" into a concrete name: kernel components are Helm
# charts fed from Git, and a chart cannot read the operator's shell. So resolve
# it here, once, and export it — bootstrap Applications pass the result down as
# a Helm parameter (see kernel/bootstrap/*-application.yaml.tmpl).
#
# Resolution order:
#   1. STORAGE_CLASS from cluster-settings.env / environment (explicit wins)
#   2. the cluster's default StorageClass (is-default-class annotation)
#   3. hard error — a silent fallback here surfaces much later as a PVC that
#      sits Pending forever, which is a far worse failure mode.
# =============================================================================
resolve_storage_class() {
    if [[ -n "${STORAGE_CLASS:-}" ]]; then
        if ! kubectl get storageclass "${STORAGE_CLASS}" &>/dev/null; then
            error "STORAGE_CLASS=${STORAGE_CLASS} does not exist on this cluster."
            error "  Available: $(kubectl get storageclass -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}' 2>/dev/null)"
            error "  Fix STORAGE_CLASS in cluster-settings.env, or leave it unset to use the cluster default."
            return 1
        fi
        info "StorageClass: ${STORAGE_CLASS} (explicit, from cluster-settings.env)"
        export STORAGE_CLASS
        return 0
    fi

    local default_sc
    default_sc=$(kubectl get storageclass \
        -o jsonpath='{range .items[?(@.metadata.annotations.storageclass\.kubernetes\.io/is-default-class=="true")]}{.metadata.name}{"\n"}{end}' \
        2>/dev/null | head -1)

    if [[ -z "${default_sc}" ]]; then
        error "STORAGE_CLASS is unset and this cluster has no default StorageClass."
        error "  Available: $(kubectl get storageclass -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}' 2>/dev/null)"
        error "  Set STORAGE_CLASS in cluster-settings.env, or annotate one class"
        error "  with storageclass.kubernetes.io/is-default-class=true, and re-run."
        return 1
    fi

    STORAGE_CLASS="${default_sc}"
    export STORAGE_CLASS
    info "StorageClass: ${STORAGE_CLASS} (cluster default; STORAGE_CLASS unset)"
    return 0
}

# =============================================================================
# apply_bootstrap_application — kubectl apply a bootstrap Application, rendering
# it first when it ships as a .yaml.tmpl.
#
# Bootstrap Applications are applied by install.sh from the local checkout
# rather than read from Git by ArgoCD, which is exactly why per-cluster values
# (STORAGE_CLASS) can be substituted into them at all.
#
# envsubst is called with an explicit variable allowlist. That is not a style
# choice: these manifests contain ArgoCD's multi-source "$values" reference, and
# an unrestricted envsubst would silently expand it to the empty string and
# break every valueFiles entry.
# =============================================================================
apply_bootstrap_application() {
    local name="$1"
    local base="${SCRIPT_DIR}/kernel/bootstrap/${name}-application"

    if [[ -f "${base}.yaml.tmpl" ]]; then
        if ! command -v envsubst &>/dev/null; then
            error "envsubst not found (install gettext-base). Aborting."
            exit 1
        fi
        if [[ -z "${STORAGE_CLASS:-}" ]]; then
            error "STORAGE_CLASS is empty while rendering ${name}-application.yaml.tmpl."
            error "  resolve_storage_class() should have set it during pre-flight."
            exit 1
        fi
        envsubst "\${STORAGE_CLASS}" < "${base}.yaml.tmpl" | kubectl apply -f -
    else
        kubectl apply -f "${base}.yaml"
    fi
}

# =============================================================================
# yq_get — read a YAML field, tolerant of either yq flavor (mikefarah/yq's
# "eval" subcommand, or kislyuk/yq's jq-style filter) since both are seen in
# the wild as `yq`. Echoes the value and returns 0, or returns 1 (nothing
# echoed) if the field is absent/null or the file doesn't exist.
# =============================================================================
yq_get() {
    local filter="$1" file="$2"
    [[ -f "${file}" ]] || return 1
    local out
    if out=$(yq eval "${filter}" "${file}" 2>/dev/null) && [[ -n "${out}" && "${out}" != "null" ]]; then
        echo "${out}"
        return 0
    fi
    if out=$(yq -r "${filter}" "${file}" 2>/dev/null) && [[ -n "${out}" && "${out}" != "null" ]]; then
        echo "${out}"
        return 0
    fi
    return 1
}

# =============================================================================
# resolve_kernel_domain_from_claim — an already-bootstrapped cluster's
# KERNEL_DOMAIN lives solely in its Crossplane Cluster claim
# (gentian-deployments/clusters/<cluster>/kernel/claims/cluster.yaml), not in
# cluster-settings.env. Read it from there. No-op (KERNEL_DOMAIN stays unset)
# for a brand-new cluster that has no claim file yet — install.sh's
# prompt_kernel_domain/scaffold_cluster_deployment handle that case.
# =============================================================================
resolve_kernel_domain_from_claim() {
    local claim_file="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER}/kernel/claims/cluster.yaml"
    [[ -n "${KERNEL_DOMAIN:-}" ]] && return 0

    local domain
    if domain=$(yq_get '.spec.kernelDomain' "${claim_file}"); then
        export KERNEL_DOMAIN="${domain}"
        info "KERNEL_DOMAIN=${KERNEL_DOMAIN} (read from ${claim_file})"
    fi
}

# =============================================================================
# upsert_gentian_cluster_config — cluster-wide ConfigMap for Crossplane / apps
# =============================================================================
# Idempotent. Used by install.sh (after Cluster XR Ready) and update.sh
# (--crossplane / --all) so day-2 runs pick up node.ip and service endpoints.
upsert_gentian_cluster_config() {
    if [[ -z "${NODE_IP:-}" ]]; then
        NODE_IP=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null || true)
        if [[ -n "${NODE_IP}" ]]; then
            info "Auto-detected NODE_IP: ${NODE_IP}"
        fi
    fi
    export NODE_IP

    local _cnpg_cluster="${CNPG_CLUSTER_NAME:-postgres}"
    local _minio_endpoint="${MINIO_ENDPOINT:-http://minio-${ENV:-dev}.gentian-infra-${ENV:-dev}.svc.cluster.local:9000}"
    local _cnpg_host="${CNPG_HOST:-${_cnpg_cluster}-rw.platform-kernel.svc.cluster.local}"
    local _storage_class="${STORAGE_CLASS:-}"
    local _mail_mode="${MAIL_SERVICE_MODE:-external}"
    local _routing_mode="${ROUTING_MODE:-gateway}"
    local _infra_ns="${INFRA_NAMESPACE:-gentian-infra-${ENV:-dev}}"
    local _services_ns="${SERVICES_NAMESPACE:-gentian-${ENV:-dev}}"
    local _openbao_ns="${OPENBAO_NAMESPACE:-openbao}"
    local _smtp_host="${MAIL_SMTP_HOST:-postfix-${ENV:-dev}.${_services_ns}.svc.cluster.local}"
    local _kube_api_cidr=""
    local _kube_api_endpoint_ip=""
    local _kube_api_endpoint_port=""
    if _kube_api_ip="$(kubectl get svc kubernetes -n default -o jsonpath='{.spec.clusterIP}' 2>/dev/null)"; then
        [[ -n "${_kube_api_ip}" ]] && _kube_api_cidr="${_kube_api_ip}/32"
    fi
    # Calico/Cilium evaluate egress against the post-DNAT apiserver endpoint, not
    # only the kubernetes Service ClusterIP. Tenant bootstrap Jobs (e.g. Matrix UVS)
    # need this endpoint reachable from isolated tenant namespaces.
    _kube_api_endpoint_ip="$(kubectl get endpoints kubernetes -n default \
        -o jsonpath='{.subsets[0].addresses[0].ip}' 2>/dev/null || true)"
    _kube_api_endpoint_port="$(kubectl get endpoints kubernetes -n default \
        -o jsonpath='{.subsets[0].ports[?(@.name=="https")].port}' 2>/dev/null || true)"
    if [[ -z "${_kube_api_endpoint_port}" ]]; then
        _kube_api_endpoint_port="$(kubectl get endpoints kubernetes -n default \
            -o jsonpath='{.subsets[0].ports[0].port}' 2>/dev/null || true)"
    fi

    info "Upserting gentian-cluster-config (node.ip=${NODE_IP:-<unset>}, kubeApi=${_kube_api_endpoint_ip}:${_kube_api_endpoint_port:-<unset>})..."
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
  mail.smtpHost: "${_smtp_host}"
  minio.endpoint: "${_minio_endpoint}"
  cnpg.host: "${_cnpg_host}"
  cnpg.clusterName: "${_cnpg_cluster}"
  storageClass: "${_storage_class}"
  mail.serviceMode: "${_mail_mode}"
  secretMode: "${SECRET_MODE:-derived}"
  node.ip: "${NODE_IP:-}"
  llm.enabled: "${LLM_SUPPORT:-false}"
  network.infraNamespace: "${_infra_ns}"
  network.servicesNamespace: "${_services_ns}"
  network.openbaoNamespace: "${_openbao_ns}"
  network.routingMode: "${_routing_mode}"
  network.kubeApiServerCidr: "${_kube_api_cidr}"
  network.kubeApiServerEndpointIp: "${_kube_api_endpoint_ip}"
  network.kubeApiServerEndpointPort: "${_kube_api_endpoint_port}"
  tenant.limitRange.default.cpu: "${TENANT_LIMITRANGE_DEFAULT_CPU:-500m}"
  tenant.limitRange.default.memory: "${TENANT_LIMITRANGE_DEFAULT_MEMORY:-512Mi}"
  tenant.limitRange.defaultRequest.cpu: "${TENANT_LIMITRANGE_DEFAULT_REQUEST_CPU:-100m}"
  tenant.limitRange.defaultRequest.memory: "${TENANT_LIMITRANGE_DEFAULT_REQUEST_MEMORY:-128Mi}"
  tenant.initJob.limits.cpu: "${TENANT_INITJOB_LIMIT_CPU:-500m}"
  tenant.initJob.limits.memory: "${TENANT_INITJOB_LIMIT_MEMORY:-512Mi}"
  tenant.initJob.requests.cpu: "${TENANT_INITJOB_REQUEST_CPU:-100m}"
  tenant.initJob.requests.memory: "${TENANT_INITJOB_REQUEST_MEMORY:-128Mi}"
EOF
    success "gentian-cluster-config ConfigMap upserted."
}

# =============================================================================
# Crossplane platform compositions (gentian-os only)
# =============================================================================
# Generic app-default and tenant/cluster compositions live in gentian-os.
# Profile-specific compositions are synced from gentian-apps via Argo CD
# ApplicationSet gentian-catalogue (see install_catalogue_sync).

apply_crossplane_app_compositions() {
    local comp_dir="${SCRIPT_DIR}/crossplane/compositions"
    info "Applying Composition app-default..."
    kubectl apply -f "${comp_dir}/app-default.yaml"
}

apply_crossplane_platform_compositions() {
    info "Applying Composition (cluster-default)..."
    kubectl apply -f "${SCRIPT_DIR}/crossplane/compositions/cluster-default.yaml"
    apply_crossplane_app_compositions
    info "Applying Composition (tenant-default)..."
    kubectl apply -f "${SCRIPT_DIR}/crossplane/compositions/tenant-default.yaml"
}

apply_crossplane_platform_compositions_update() {
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
# 1. Create namespaces (idempotent)
# =============================================================================
create_namespaces() {
    banner "Step 1 — Creating namespaces"

    local namespaces=(openbao external-secrets argocd gentian-system platform-kernel)
    if [[ "$INSTALL_CLUSTER_INFRA" == "1" ]]; then
        namespaces+=(stakater-system cnpg-system cert-manager)
        if [[ "${ROUTING_MODE:-gateway}" == "gateway" ]]; then
            namespaces+=("${ENVOY_GATEWAY_NAMESPACE}")
        fi
    fi

    for ns in "${namespaces[@]}"; do
        if kubectl get namespace "$ns" &>/dev/null; then
            success "Namespace $ns already exists."
        else
            _kubectl_retry create namespace "$ns"
            success "Namespace $ns created."
        fi
    done
}

# =============================================================================
# 1b. Pre-warm cluster (distro-agnostic PLEG/CRI-race mitigation)
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

    banner "Step 1b — Pre-warming cluster (PLEG/CRI race mitigation)"

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
# Deploy kernel mail services (Postfix + Dovecot)
#
# Called when MAIL_SERVICE_MODE=kernel — a conditional sub-step of Step 13b
# (install_kernel_mail), not a standalone pipeline step, since most installs
# use the default external-SMTP mode and never reach this. Applies the
# provider-helm Release CRs, ConfigMaps, and ExternalSecrets for postfix and
# dovecot from:
#   kernel/services/postfix/manifests/${ENV:-dev}/
#   kernel/services/dovecot/manifests/${ENV:-dev}/
#
# Both service directories follow the standard Pattern B layout:
#   configmap.yaml        — non-sensitive Helm values ConfigMaps
#   externalsecret.yaml   — ESO ExternalSecret (reads from OpenBao)
#   release.yaml          — Crossplane provider-helm Release CR
#
# Prerequisites:
#   - provider-helm must be Healthy (Step 11).
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

    banner "Deploy kernel mail services (MAIL_SERVICE_MODE=kernel)"

    local env="${ENV:-dev}"
    local ns="gentian-${env}"

    # Ensure the target namespace exists when invoked standalone from update.sh.
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
    _patch_postfix_allowed_sender_domains || true
    info "provider-helm will reconcile the Release CRs within 5 minutes."
    info "Monitor: kubectl get release.helm.crossplane.io | grep -E 'postfix|dovecot'"
    info "         argocd app sync gentian-infra-helm-${env}"
}

# _apply_kernel_manifest_dir applies kernel service manifests from manifest_dir.
# Services using kustomize (configMapGenerator) must be applied with -k;
# kubectl apply -f dir/ fails on kustomization.yaml with "no matches for kind Kustomization".
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
# Verify Keycloak iframe policy (portal-embedded OIDC)
# =============================================================================
# Waits for the gentian-os KeycloakPlatformReconciler to patch id.<kernel>
# HTTPRoute (ROUTING_MODE=gateway) and for browser-security Jobs to clear
# X-Frame-Options on Keycloak realms. Diagnostic/verification utility, not a
# pipeline step — currently only reachable via the commented-out block at
# the end of main_cp() (uncomment to enable).
verify_keycloak_iframe_policy() {
    banner "Verify — Keycloak iframe policy"

    local kernel_domain="${KERNEL_DOMAIN:-}"
    if [[ -z "$kernel_domain" ]]; then
        warn "KERNEL_DOMAIN unset — skipping Keycloak iframe verification."
        return 0
    fi

    local services_ns="${SERVICES_NAMESPACE:-gentian-${ENV:-dev}}"
    local kernel_ns="${KERNEL_NAMESPACE:-${services_ns}}"
    local route_name="${KEYCLOAK_IDP_HTTPROUTE_NAME:-kernel-idp}"
    local timeout="${KEYCLOAK_FRAME_VERIFY_TIMEOUT:-300}"
    local interval=10
    local elapsed=0

    info "Waiting for Keycloak HTTPRoute ${route_name} and operator frame-ancestors patch..."

    while [[ $elapsed -lt $timeout ]]; do
        local csp=""
        csp=$(kubectl get httproute "$route_name" -n "$services_ns" \
            -o jsonpath='{range .spec.rules[0].filters[*]}{.responseHeaderModifier.set[*].value}{"\n"}{end}' \
            2>/dev/null || true)

        if [[ -n "$csp" ]] \
            && [[ "$csp" == *"frame-ancestors"* ]] \
            && [[ "$csp" == *"https://portal.${kernel_domain}"* ]]; then
            success "Keycloak HTTPRoute allows portal.${kernel_domain} in frame-ancestors."
            break
        fi

        printf "  …waiting for Keycloak HTTPRoute CSP (%ds/%ds)\n" "$elapsed" "$timeout"
        sleep "$interval"
        elapsed=$((elapsed + interval))
    done

    if [[ $elapsed -ge $timeout ]]; then
        warn "Keycloak HTTPRoute frame-ancestors not converged within ${timeout}s."
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

    info "No browser-security jobs yet (no Tenant CRs?) — HTTPRoute CSP is ready."
    return 0
}
