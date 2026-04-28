#!/usr/bin/env bash
# =============================================================================
# install.sh — Fresh-cluster bootstrap for Gentian OS
# =============================================================================
# This script is fully self-contained. It depends only on files inside this
# repository (gentian-os) plus the user-supplied secrets listed below. No
# reference to any other Gentian repository is made.
#
# It installs and configures every kernel component in the correct order:
#    1. CLI tools (tofu, bao)
#    2. Kubernetes namespaces
#    3. cert-manager via Helm
#    4. External Secrets Operator (ESO) via Helm
#    5. ArgoCD + gentian AppProject
#    6. ArgoCD OCI registry secrets
#    7. OpenBao transit seal instance
#    8. Transit init + autounseal Secret
#    9. Remaining ArgoCD bootstrap Applications
#       (openbao, tofu-controller, reloader, cnpg, cnpg-cluster, globals)
#   10. Primary OpenBao init (transit auto-unseal)
#   11. OpenBao configuration via Tofu (KV engine, K8s auth, policies, operator role)
#   12. Seed kernel secrets (scripts/seed-openbao.sh)
#   13. Apply root ApplicationSet → ArgoCD syncs the full stack
#   14. Install AppCatalogue CRD + kubectl-gentian plugin
#   14b. Install ArgoCD Application that syncs AppProfiles from gentian-apps
#   15. Install gentian-os orchestrator (Helm chart → CRDs + operator)
#         leaves the cluster in a state where Tenant CRs can be applied
#   16. Verify all ArgoCD Applications are Synced + Healthy
#
# Required environment variables (prompted interactively if not pre-exported):
#   MASTER_PASSWORD                — master password for HKDF-derived secrets
#   OD_PRIVATE_REGISTRY_USERNAME   — registry.opencode.de username
#   OD_PRIVATE_REGISTRY_PASSWORD   — registry.opencode.de password or token
#   OD_SMTP_RELAY_USERNAME         — SMTP relay username (e.g. Gmail address)
#   OD_SMTP_RELAY_PASSWORD         — SMTP relay password (e.g. Gmail App Password)
#   KERNEL_DOMAIN                  — single platform-wide DNS suffix (e.g.
#                                    platform.example.com). Persisted to
#                                    .install-state.env after first prompt.
#
# Optional environment variables:
#   NODE_IP                       — cluster node IP (default: auto-detected)
#   SKIP_TOOLS                    — set to "1" to skip CLI tool installation
#   OPENBAO_INIT_FILE             — path to save OpenBao init keys (default: /tmp/openbao-init.json)
#   GENTIAN_APPS_REPO             — default https://github.com/gentian-org/gentian-apps
#   GENTIAN_APPS_BRANCH           — default main
#   GENTIAN_DEPLOYMENTS_REPO      — default https://github.com/gentian-org/gentian-deployments
#   GENTIAN_DEPLOYMENTS_BRANCH    — default main
#   GENTIAN_NONINTERACTIVE        — set to "1" to skip the repo-prompt and use defaults
#   INSTALL_CONFIG_FILE           — defaults to ./install.env when present
#   INSTALL_SECRETS_FILE          — defaults to ./install.secrets.env when present
#   INSTALL_AUTO_LOAD_CONFIG      — set to "0" to disable env-file loading
#
# Usage:
#   ./install.sh
#   ./install.sh --no-cluster-infra
# =============================================================================

set -euo pipefail

# ─── Colour helpers ──────────────────────────────────────────────────────────
RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'; CYAN='\033[0;36m'; NC='\033[0m'
info()    { echo -e "${CYAN}[INFO]${NC}  $*"; }
success() { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; }
banner()  { echo -e "\n${CYAN}══════════════════════════════════════════════════${NC}"; echo -e "${CYAN}  $*${NC}"; echo -e "${CYAN}══════════════════════════════════════════════════${NC}\n"; }

# =============================================================================
# ensure_sudo [REASON]
#
# Cache-aware sudo prompt. If passwordless sudo works (CI, NOPASSWD), returns
# silently. Otherwise prints REASON and prompts for the password ONCE, after
# which the credential is cached for ~15 min so subsequent sudo calls inside
# helpers don't re-prompt. Returns 1 if the user cancels or sudo isn't
# installed at all (so callers can fall back to a no-op + warn).
# =============================================================================
ensure_sudo() {
    local reason="${1:-Operation needs sudo}"
    if ! command -v sudo &>/dev/null; then
        return 1
    fi
    if sudo -n true 2>/dev/null; then
        return 0
    fi
    info "$reason — requesting sudo password..."
    if sudo -v; then
        return 0
    fi
    return 1
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

# =============================================================================
# cri_cleanup
#
# Sweep stale CRI state that accumulates from prior install/uninstall cycles
# and can wedge kubelet's pod-sync loop (symptom: pods sit in
# ContainerCreating with a calico-assigned IP but PodReadyToStartContainers
# never flips True).
#
# Removes:
#   - all CRI sandboxes in NotReady state
#   - all exited (dead) CRI containers
#   - orphan RUNNING sandboxes whose owning pod object no longer exists
#     (or whose UID no longer matches the labelled UID) — these hold
#     containerd name reservations and cause new pod attempts to fail
#     with "name ... is reserved for ...".
#
# Auto-detects crictl binary and containerd socket. On systems without
# crictl (e.g. stock microk8s) falls back to `microk8s.ctr` to delete
# STOPPED tasks in the k8s.io containerd namespace. Prompts for sudo
# password if needed; skips with a warning if neither tool is present.
# =============================================================================
cri_cleanup() {
    # Disable strict mode locally — this helper is best-effort and must
    # never fail the caller. Restore on return.
    local _prev_opts; _prev_opts=$(set +o); set +eo pipefail

    _cri_cleanup_impl() {
        # Acquire sudo (interactive prompt if needed).
        if ! ensure_sudo "CRI cleanup needs sudo to query containerd state"; then
            warn "CRI cleanup skipped: sudo not available."
            return 0
        fi

        # ── Path 1: crictl (preferred, CRI-spec compliant) ──────────────────
        # Resolve to an absolute path BEFORE invoking via sudo, since sudo
        # strips PATH (sudoers secure_path) and would otherwise fail with
        # "command not found" even when the binary is on the user's PATH.
        local crictl_bin="" sock=""
        if command -v crictl &>/dev/null; then
            crictl_bin=$(command -v crictl)
            for s in /var/snap/microk8s/common/run/containerd.sock \
                     /run/containerd/containerd.sock \
                     /var/run/crio/crio.sock; do
                if sudo test -S "$s" 2>/dev/null; then sock="$s"; break; fi
            done
        fi

        if [[ -n "$crictl_bin" ]]; then
            local crictl=("sudo" "$crictl_bin")
            [[ -n "$sock" ]] && crictl+=("--runtime-endpoint" "unix://$sock")
            info "CRI cleanup: removing stale sandboxes and exited containers (via crictl)..."
            local notready_pods exited_ctrs
            notready_pods=$("${crictl[@]}" pods --state notready -q 2>/dev/null | wc -l | tr -d ' ')
            exited_ctrs=$("${crictl[@]}" ps -a --state exited -q 2>/dev/null | wc -l | tr -d ' ')
            if (( notready_pods > 0 )); then
                "${crictl[@]}" pods --state notready -q 2>/dev/null \
                    | xargs -r "${crictl[@]}" rmp -f >/dev/null 2>&1 || true
            fi
            if (( exited_ctrs > 0 )); then
                "${crictl[@]}" ps -a --state exited -q 2>/dev/null \
                    | xargs -r "${crictl[@]}" rm >/dev/null 2>&1 || true
            fi
            # Orphan RUNNING sandboxes: list every Ready pod, look up the
            # corresponding Kubernetes pod object; if it's gone or its UID
            # differs, force-remove the sandbox so its containerd name
            # reservation is released.
            local orphans=0 sb_id sb_ns sb_name sb_uid live_uid
            while read -r sb_id; do
                [[ -z "$sb_id" ]] && continue
                local meta
                meta=$("${crictl[@]}" inspectp "$sb_id" 2>/dev/null) || continue
                sb_ns=$(printf '%s' "$meta"   | sed -n 's/.*"namespace": *"\([^"]*\)".*/\1/p' | head -1)
                sb_name=$(printf '%s' "$meta" | sed -n 's/.*"name": *"\([^"]*\)".*/\1/p'      | head -1)
                sb_uid=$(printf '%s' "$meta"  | sed -n 's/.*"uid": *"\([^"]*\)".*/\1/p'       | head -1)
                [[ -z "$sb_ns" || -z "$sb_name" || -z "$sb_uid" ]] && continue
                live_uid=$(kubectl get pod -n "$sb_ns" "$sb_name" \
                            -o jsonpath='{.metadata.uid}' 2>/dev/null || true)
                if [[ -z "$live_uid" || "$live_uid" != "$sb_uid" ]]; then
                    "${crictl[@]}" rmp -f "$sb_id" >/dev/null 2>&1 || true
                    orphans=$((orphans+1))
                fi
            done < <("${crictl[@]}" pods --state ready -q 2>/dev/null)
            success "CRI cleanup: removed ${notready_pods} stale sandbox(es), ${exited_ctrs} exited container(s), ${orphans} orphan sandbox(es)."
            return 0
        fi

        # ── Path 2: microk8s.ctr fallback ───────────────────────────────────
        # microk8s ships containerd's native `ctr` tool but not `crictl`. We
        # can still reap stale state by deleting STOPPED tasks in the k8s.io
        # containerd namespace; kubelet will then resync cleanly.
        local ctr_bin=""
        if command -v microk8s.ctr &>/dev/null; then
            ctr_bin=$(command -v microk8s.ctr)
        elif [[ -x /snap/bin/microk8s.ctr ]]; then
            ctr_bin=/snap/bin/microk8s.ctr
        fi

        if [[ -n "$ctr_bin" ]]; then
            info "CRI cleanup: removing stopped containerd tasks (via ctr)..."
            local stopped raw
            # Use long-form --namespace because some snap shims mis-parse
            # the short -n flag (it gets confused with sudo's -n or ctr's
            # own global options and prints help instead of the task list).
            raw=$(sudo "$ctr_bin" --namespace k8s.io tasks ls 2>/dev/null || true)
            stopped=$(printf '%s\n' "$raw" | awk 'NR>1 && $3=="STOPPED"{print $1}')
            local count=0
            if [[ -n "$stopped" ]]; then
                while IFS= read -r tid; do
                    [[ -z "$tid" ]] && continue
                    sudo "$ctr_bin" --namespace k8s.io tasks delete --force "$tid" >/dev/null 2>&1 || true
                    count=$((count+1))
                done <<< "$stopped"
            fi

            # Orphan RUNNING sandbox reaper: walk every container in the
            # k8s.io namespace, read the kubernetes labels (pod name, ns,
            # uid) from `containers info`, and if the live pod is gone or
            # has a different UID, force-delete task + container so its
            # containerd name reservation is released. This handles the
            # "name ... is reserved for ..." sandbox-collision wedge.
            local orphans=0 cid info_json c_ns c_name c_uid live_uid containers
            containers=$(sudo "$ctr_bin" --namespace k8s.io containers ls -q 2>/dev/null || true)
            if [[ -n "$containers" ]]; then
                while IFS= read -r cid; do
                    [[ -z "$cid" ]] && continue
                    info_json=$(sudo "$ctr_bin" --namespace k8s.io containers info "$cid" 2>/dev/null) || continue
                    c_ns=$(printf   '%s' "$info_json" | sed -n 's/.*"io.kubernetes.pod.namespace": *"\([^"]*\)".*/\1/p' | head -1)
                    c_name=$(printf '%s' "$info_json" | sed -n 's/.*"io.kubernetes.pod.name": *"\([^"]*\)".*/\1/p'      | head -1)
                    c_uid=$(printf  '%s' "$info_json" | sed -n 's/.*"io.kubernetes.pod.uid": *"\([^"]*\)".*/\1/p'       | head -1)
                    [[ -z "$c_ns" || -z "$c_name" || -z "$c_uid" ]] && continue
                    live_uid=$(kubectl get pod -n "$c_ns" "$c_name" \
                                -o jsonpath='{.metadata.uid}' 2>/dev/null || true)
                    if [[ -z "$live_uid" || "$live_uid" != "$c_uid" ]]; then
                        sudo "$ctr_bin" --namespace k8s.io tasks delete --force "$cid"   >/dev/null 2>&1 || true
                        sudo "$ctr_bin" --namespace k8s.io containers delete       "$cid"   >/dev/null 2>&1 || true
                        orphans=$((orphans+1))
                    fi
                done <<< "$containers"
            fi
            success "CRI cleanup: removed ${count} stopped task(s), ${orphans} orphan sandbox(es)."
            return 0
        fi

        warn "CRI cleanup skipped: neither crictl nor microk8s.ctr found."
        return 0
    }

    _cri_cleanup_impl || warn "CRI cleanup encountered an error (ignored)."
    eval "$_prev_opts"
    return 0
}

# =============================================================================
# kubelite_restart
#
# Last-resort recovery for the kubelet status-sync wedge: pod has a
# CNI-assigned IP (annotation set) but PodReadyToStartContainers stays
# False forever. CRI cleanup does not help because the wedge is in the
# kubelet's status manager itself, not the containerd state.
#
# Restarting the microk8s kubelite snap service kicks the status loop
# without taking the whole node down (`microk8s stop` would also tear
# down the API server). Best-effort: requires passwordless sudo, only
# acts when microk8s is detected.
# =============================================================================
kubelite_restart() {
    local _prev_opts; _prev_opts=$(set +o); set +eo pipefail

    if ! command -v snap &>/dev/null; then
        eval "$_prev_opts"; return 0
    fi
    if ! snap list 2>/dev/null | grep -q '^microk8s '; then
        eval "$_prev_opts"; return 0
    fi
    if ! ensure_sudo "Kubelite restart needs sudo to call systemctl"; then
        warn "Kubelite restart skipped: sudo not available."
        eval "$_prev_opts"; return 0
    fi

    warn "Restarting microk8s kubelite (status-sync wedge recovery)..."
    sudo systemctl restart snap.microk8s.daemon-kubelite >/dev/null 2>&1 || true
    # Give kubelite ~20s to come back and re-sync pod statuses.
    local i
    for i in {1..20}; do
        if /snap/bin/microk8s.kubectl get --raw=/healthz >/dev/null 2>&1; then
            success "Kubelite restarted (apiserver healthy after ${i}s)."
            eval "$_prev_opts"; return 0
        fi
        sleep 1
    done
    warn "Kubelite restart issued but apiserver did not respond healthy within 20s."
    eval "$_prev_opts"
    return 0
}

# ─── Paths ────────────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

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
#   4) OpenBao backfill for missing values
#   5) interactive prompts for missing required values
INPUT_HIERARCHY_VARS=(
    MASTER_PASSWORD
    OD_PRIVATE_REGISTRY_USERNAME
    OD_PRIVATE_REGISTRY_PASSWORD
    OD_SMTP_RELAY_USERNAME
    OD_SMTP_RELAY_PASSWORD
    KERNEL_DOMAIN
    NODE_IP
    SKIP_TOOLS
    OPENBAO_INIT_FILE
    LETSENCRYPT_EMAIL
    INGRESS_CLASS_NAME
    GENTIAN_APPS_REPO
    GENTIAN_APPS_BRANCH
    GENTIAN_DEPLOYMENTS_REPO
    GENTIAN_DEPLOYMENTS_BRANCH
    GENTIAN_DEPLOYMENTS_PATH
    GENTIAN_NONINTERACTIVE
    INSTALL_CLUSTER_INFRA
    GENTIAN_MANAGED_CERT_MANAGER
    CF_API_TOKEN
    CF_ZONE_NAME
)

# ─── Versions ────────────────────────────────────────────────────────────────
TOFU_VERSION="1.9.0"
BAO_VERSION="2.5.1"

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
    source "${file}" || true
    set +a

    for var in "${!before[@]}"; do
        declare -gx "$var=${before[$var]}"
    done

    info "Loaded ${label} from ${file}."
}

# validate_config checks that all required environment variables are set and
# that key values pass basic format validation. Exits 0 on success, 1 on
# failure. No cluster actions are taken.
validate_config() {
    local errors=0 warnings=0

    _req() {
        local var="$1" hint="$2"
        if [[ -z "${!var:-}" ]]; then
            echo "  [MISSING]  ${var}  — ${hint}"
            (( errors++ )) || true
        else
            echo "  [OK]       ${var}"
        fi
    }

    _opt() {
        local var="$1" hint="$2"
        if [[ -z "${!var:-}" ]]; then
            echo "  [WARN]     ${var}  — not set (${hint})"
            (( warnings++ )) || true
        else
            echo "  [OK]       ${var}"
        fi
    }

    echo ""
    echo "━━━ Required secrets ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    _req MASTER_PASSWORD          "HKDF master secret — used to derive all app secrets"
    _req OD_PRIVATE_REGISTRY_USERNAME "registry.opencode.de username"
    _req OD_PRIVATE_REGISTRY_PASSWORD "registry.opencode.de password or token"
    _req OD_SMTP_RELAY_USERNAME   "SMTP relay username (e.g. Gmail address)"
    _req OD_SMTP_RELAY_PASSWORD   "SMTP relay password (e.g. Gmail App Password)"

    echo ""
    echo "━━━ Required config ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    if [[ -z "${KERNEL_DOMAIN:-}" ]]; then
        echo "  [MISSING]  KERNEL_DOMAIN  — platform-wide DNS suffix (e.g. platform.example.com)"
        (( errors++ )) || true
    elif [[ ! "${KERNEL_DOMAIN}" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ ]]; then
        echo "  [INVALID]  KERNEL_DOMAIN=${KERNEL_DOMAIN}  — must match ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?\$"
        (( errors++ )) || true
    else
        echo "  [OK]       KERNEL_DOMAIN=${KERNEL_DOMAIN}"
    fi

    echo ""
    echo "━━━ Optional / recommended config ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    _opt LETSENCRYPT_EMAIL  "required for Let's Encrypt ACME; falls back to a dummy address"
    _opt INGRESS_CLASS_NAME "defaults to 'nginx' if not set"
    _opt NODE_IP            "auto-detected at install time if not set"
    _opt CF_API_TOKEN       "Cloudflare token — needed for DNS-01 wildcard certificates"
    _opt CF_ZONE_NAME       "Cloudflare zone — derived from KERNEL_DOMAIN if not set"
    _opt GENTIAN_APPS_REPO       "defaults to https://github.com/gentian-org/gentian-apps"
    _opt GENTIAN_APPS_BRANCH     "defaults to 'main'"
    _opt GENTIAN_DEPLOYMENTS_REPO    "defaults to https://github.com/gentian-org/gentian-deployments"
    _opt GENTIAN_DEPLOYMENTS_BRANCH  "defaults to 'main'"

    echo ""
    echo "━━━ Config sources ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    [[ -r "${INSTALL_CONFIG_FILE}" ]]  && echo "  [FILE]     ${INSTALL_CONFIG_FILE}" \
                                       || echo "  [ABSENT]   ${INSTALL_CONFIG_FILE}  (optional)"
    [[ -r "${INSTALL_SECRETS_FILE}" ]] && echo "  [FILE]     ${INSTALL_SECRETS_FILE}" \
                                       || echo "  [ABSENT]   ${INSTALL_SECRETS_FILE}  (optional, chmod 600)"
    [[ -r "${INSTALL_SECRETS_CACHE}" ]] && echo "  [CACHE]    ${INSTALL_SECRETS_CACHE}" \
                                        || echo "  [NO CACHE] ${INSTALL_SECRETS_CACHE}"

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

    if [[ "$prompted" -eq 1 ]]; then
        echo ""
        save_creds_cache
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

    : "${GENTIAN_APPS_REPO:=${default_apps_repo}}"
    : "${GENTIAN_APPS_BRANCH:=${default_apps_branch}}"
    : "${GENTIAN_DEPLOYMENTS_REPO:=${default_deploy_repo}}"
    : "${GENTIAN_DEPLOYMENTS_BRANCH:=${default_deploy_branch}}"
    : "${GENTIAN_DEPLOYMENTS_PATH:=${HOME}/.gentian/gentian-deployments}"
    export GENTIAN_APPS_REPO GENTIAN_APPS_BRANCH \
           GENTIAN_DEPLOYMENTS_REPO GENTIAN_DEPLOYMENTS_BRANCH GENTIAN_DEPLOYMENTS_PATH

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
GENTIAN_DEPLOYMENTS_PATH="${GENTIAN_DEPLOYMENTS_PATH}"
EOF
    chmod 0600 "$cfg_file"
    success "App repo configuration saved to ${cfg_file}"
}

# =============================================================================
# Persist non-secret installer state (kernel domain, etc.) across re-runs.
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
        echo "# Auto-generated by install.sh — non-secret installer state."
        echo "# Delete to be re-prompted on next run."
        val="${KERNEL_DOMAIN:-}"
        [[ -n "$val" ]] && printf 'export KERNEL_DOMAIN=%q\n' "$val"
        val="${GENTIAN_MANAGED_CERT_MANAGER:-}"
        [[ -n "$val" ]] && printf 'export GENTIAN_MANAGED_CERT_MANAGER=%q\n' "$val"
    } >"$tmp"
    install -m 0644 "$tmp" "${INSTALL_STATE_FILE}"
    rm -f "$tmp"
    info "Saved installer state to ${INSTALL_STATE_FILE}."
}

# =============================================================================
# Prompt for the cluster's kernel domain (the single platform-wide domain on
# which all kernel UIs — Keycloak, Nubus, Argo CD, Intercom — are served, and
# which provides the `<tenant>.<kernel_domain>` fallback for tenants without a
# vanity domain). See docs/architecture.md §2.5.
#
# Persisted to ${INSTALL_STATE_FILE} so subsequent re-runs do not re-prompt.
# =============================================================================
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
# Prompt for kernel-level secrets that depend on KERNEL_DOMAIN. Currently:
#
#   - CF_API_TOKEN: Cloudflare API token with Zone:Read + DNS:Edit on the
#     KERNEL_DOMAIN's zone. Used to solve DNS-01 ACME challenges for the
#     kernel wildcard `*.${KERNEL_DOMAIN}`. Optional — if left empty, only
#     the HTTP-01 ClusterIssuer is provisioned and tenants must rely on
#     per-host HTTP-01 (vanity domain mode). See docs/architecture.md §2.5.
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
    info "Leave empty to skip the wildcard and use HTTP-01 per-host certs only."
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
    for cmd in kubectl helm jq openssl curl; do
        if ! command -v "$cmd" &>/dev/null; then
            error "Required command not found: $cmd"
            missing=$((missing + 1))
        else
            success "$cmd found"
        fi
    done

    for var in MASTER_PASSWORD OD_PRIVATE_REGISTRY_USERNAME OD_PRIVATE_REGISTRY_PASSWORD \
               OD_SMTP_RELAY_USERNAME OD_SMTP_RELAY_PASSWORD; do
        if [[ -z "${!var:-}" ]]; then
            error "$var is not set"
            missing=$((missing + 1))
        else
            success "$var set"
        fi
    done

    if [[ "$missing" -gt 0 ]]; then
        error "$missing prerequisite(s) missing. Aborting."
        exit 1
    fi

    if [[ -z "${NODE_IP:-}" ]]; then
        NODE_IP=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
        info "Auto-detected NODE_IP: $NODE_IP"
    fi
    export NODE_IP

    success "All pre-flight checks passed."
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

    local namespaces=(openbao external-secrets argocd gentian-system platform-kernel tofu-system)
    if [[ "$INSTALL_CLUSTER_INFRA" == "1" ]]; then
        namespaces+=(stakater-system cnpg-system)
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

    local pod
    pod="prewarm-$(date +%s)"
    info "Applying throwaway pod kube-system/${pod}..."
    kubectl apply -f - <<EOF >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: ${pod}
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

    info "Waiting for pre-warm pod to reach a terminal phase (up to 180s)..."
    local deadline=$((SECONDS + 180))
    local phase=""
    while (( SECONDS < deadline )); do
        phase=$(kubectl get pod -n kube-system "${pod}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
        case "${phase}" in
            Succeeded) success "Pre-warm pod completed (kubelet<->containerd path warmed)."; break ;;
            Failed)    warn "Pre-warm pod failed (phase=Failed); continuing anyway."; break ;;
        esac
        sleep 3
    done

    if [[ "${phase}" != "Succeeded" && "${phase}" != "Failed" ]]; then
        warn "Pre-warm pod did not finish within 180s (last phase: ${phase:-unknown})."
        warn "Continuing — auto-recovery will handle any residual wedge."
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

# =============================================================================
# 3b. Install kernel cert-manager ClusterIssuers (always — both HTTP-01 and
# DNS-01-Cloudflare). The wildcard Certificate + cloudflare-api-token
# ExternalSecret are applied later by `install_kernel_wildcard` (after the
# OpenBao seeding step has populated the token). See docs/architecture.md §2.5.
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

    : "${LETSENCRYPT_EMAIL:=admin@${KERNEL_DOMAIN}}"
    : "${INGRESS_CLASS_NAME:=nginx}"
    export LETSENCRYPT_EMAIL INGRESS_CLASS_NAME KERNEL_DOMAIN

    if ! command -v envsubst &>/dev/null; then
        error "envsubst not found (install gettext-base). Aborting."
        exit 1
    fi

    # Resolve cert-manager namespace dynamically (Helm default is cert-manager,
    # but distro addons may place it elsewhere).
    if ! kubectl get deploy cert-manager-webhook -n "${CERT_MANAGER_NAMESPACE}" &>/dev/null; then
        local detected_ns=""
        detected_ns=$(kubectl get deploy -A -o json 2>/dev/null \
            | jq -r '.items[] | select(.metadata.name=="cert-manager-webhook") | .metadata.namespace' \
            | head -1 || true)
        if [[ -n "${detected_ns}" ]]; then
            CERT_MANAGER_NAMESPACE="${detected_ns}"
            export CERT_MANAGER_NAMESPACE
        fi
    fi

    if ! kubectl get deploy cert-manager-webhook -n "${CERT_MANAGER_NAMESPACE}" &>/dev/null; then
        error "cert-manager webhook deployment not found in namespace ${CERT_MANAGER_NAMESPACE}."
        error "cert-manager is not operational; cannot apply ClusterIssuers safely."
        error "Fix cert-manager first, then re-run install.sh."
        exit 1
    fi

    # Wait for cert-manager webhook to be ready (Certificate/ClusterIssuer
    # admission would otherwise be rejected by an uninitialized webhook).
    info "Waiting for cert-manager webhook to be ready in namespace ${CERT_MANAGER_NAMESPACE}..."
    kubectl rollout status -n "${CERT_MANAGER_NAMESPACE}" deploy/cert-manager-webhook --timeout=180s >/dev/null \
        || warn "cert-manager-webhook not Ready within 180s (continuing)."

    # Apply the two ClusterIssuers (always safe — no Cloudflare secret
    # needed yet). The wildcard Certificate is applied later by
    # install_kernel_wildcard, after seed_secrets populates OpenBao.
    envsubst "\${LETSENCRYPT_EMAIL} \${INGRESS_CLASS_NAME}" \
        < "${SCRIPT_DIR}/kernel/manifests/cert-manager/cluster-issuers.yaml" \
        | kubectl apply -f -
    success "ClusterIssuers letsencrypt-http01 and letsencrypt-dns01-cloudflare applied."
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
        info "  (HTTP-01 per-host certs will still work for tenants with vanity domains.)"
        return
    fi

    banner "Step 12b — Installing kernel wildcard Certificate"

    : "${LETSENCRYPT_EMAIL:=admin@${KERNEL_DOMAIN}}"
    : "${INGRESS_CLASS_NAME:=nginx}"
    export LETSENCRYPT_EMAIL INGRESS_CLASS_NAME KERNEL_DOMAIN

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

    # 3) Apply the wildcard Certificate.
    envsubst "\${KERNEL_DOMAIN}" \
        < "${SCRIPT_DIR}/kernel/manifests/cert-manager/wildcard-kernel-cert.yaml" \
        | kubectl apply -f -
    success "Kernel wildcard Certificate wildcard-kernel applied (cert-manager namespace)."
    info "Issuance status:  kubectl get certificate wildcard-kernel -n cert-manager"
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

    # Print ArgoCD admin credentials early so the user sees them even if
    # the install is interrupted before print_summary runs (verify step
    # can take up to 10 minutes).
    local argocd_pw argocd_url
    argocd_pw=$(kubectl get secret argocd-initial-admin-secret -n argocd \
                    -o jsonpath='{.data.password}' 2>/dev/null \
                    | base64 -d 2>/dev/null || echo "")
    argocd_url=$(resolve_argocd_url)
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
# 7. Deploy OpenBao transit seal instance
# =============================================================================
bootstrap_transit_app() {
    banner "Step 7 — OpenBao transit seal instance"

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

    if ! wait_for_running_pod openbao "app.kubernetes.io/instance=openbao-transit" "openbao-transit" 300; then
        error "Step 7 failed: openbao-transit pod never became Ready. Aborting install."
        exit 1
    fi
}

# =============================================================================
# 8. Init the transit instance
# =============================================================================
init_openbao_transit() {
    banner "Step 8 — Transit instance init + autounseal Secret"
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
# 9. Apply remaining ArgoCD bootstrap Applications
# =============================================================================
bootstrap_argocd_apps() {
    banner "Step 9 — ArgoCD bootstrap Applications"

    # Register public OCI chart repos used by bootstrap Applications.
    if [[ "$INSTALL_CLUSTER_INFRA" == "1" ]]; then
        kubectl apply -f "${SCRIPT_DIR}/kernel/argocd/repos/ghcr-stakater.yaml"
        kubectl apply -f "${SCRIPT_DIR}/kernel/argocd/repos/ghcr-cloudnative-pg.yaml"
    fi
    kubectl apply -f "${SCRIPT_DIR}/kernel/argocd/repos/ghcr-flux-iac.yaml"
    success "Applied public ArgoCD repository registrations."

    # Tofu Controller needs Flux source CRDs (GitRepository, OCIRepository,
    # Bucket) to register its informers. We also need the Flux source-
    # controller itself running because kernel/services/tofu/manifests/dev/
    # terraform.yaml creates a GitRepository CR ('gentian-server') that all
    # Terraform workspaces (infra-workspaces-dev, keycloak-config-dev, and
    # every per-tenant tf-*) reference as their sourceRef. Without source-
    # controller, that GitRepository never produces an artifact and every
    # Terraform CR stays stuck on "Source is not ready, artifact not found"
    # — Nubus never installs, tenants stall on IdentityReady forever.
    # See kernel/manifests/flux-crds/README.md for CRD versioning details.
    kubectl apply -f "${SCRIPT_DIR}/kernel/manifests/flux-crds/source-crds.yaml"
    success "Applied Flux source CRDs (required by tofu-controller)."

    if helm status flux2 -n flux-system &>/dev/null; then
        success "Flux source-controller already installed (Helm release present). Skipping."
    elif kubectl get deployment source-controller -n flux-system &>/dev/null; then
        warn "source-controller already present but not Helm-managed. Skipping install."
    else
        info "Installing Flux source-controller (chart 2.15.0, image v1.8.3)..."
        # Chart 2.15.0 is the newest version whose flux-check pre-install hook
        # accepts Kubernetes 1.31. We override the image tag to v1.8.3 to
        # match the bundled CRDs (chart default is v1.5.0 which still expects
        # the v1beta2 storage version that v1.8.x dropped).
        helm upgrade --install flux2 oci://ghcr.io/fluxcd-community/charts/flux2 \
            --namespace flux-system --create-namespace \
            --version 2.15.0 \
            --set installCRDs=false \
            --set sourceController.create=true \
            --set sourceController.tag=v1.8.3 \
            --set helmController.create=false \
            --set kustomizeController.create=false \
            --set notificationController.create=false \
            --set imageAutomationController.create=false \
            --set imageReflectionController.create=false \
            --set policies.create=false \
            --set rbac.createAggregation=false \
            --wait --timeout 5m
        success "Flux source-controller installed."
    fi

    # Wait for source-controller to be Available before deploying tofu-controller.
    # source-controller rewrites its own CRDs (including gitrepositories) after
    # startup; if tofu-controller starts in that window it crash-loops because the
    # CRD temporarily disappears. Waiting for Available ensures CRD churn is done.
    info "Waiting for Flux source-controller deployment to be Available (up to 3 min)..."
    kubectl wait --for=condition=available --timeout=180s \
        deployment/source-controller -n flux-system
    success "Flux source-controller is Available."

    local apps=(openbao tofu-controller globals)
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
        info "Waiting for reloader deployment (up to 5 min)..."
        kubectl wait --for=condition=available --timeout=300s \
            deployment/reloader-reloader -n stakater-system
        success "Reloader deployment is available."

        info "Waiting for CNPG operator deployment (up to 5 min)..."
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

        echo "$init_resp" > "${OPENBAO_INIT_FILE}"
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
        echo -e "${RED}║  Recovery Key : ${recovery_key}${NC}"
        echo -e "${RED}║  Root Token   : ${root_token}${NC}"
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

        echo "$init_resp" > "${OPENBAO_INIT_FILE}"
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
# 11. Configure OpenBao via Tofu
# =============================================================================
run_tofu_openbao_init() {
    banner "Step 11 — OpenBao configuration via Tofu"

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

    pushd "${SCRIPT_DIR}/kernel/tofu/platform/openbao-init" > /dev/null
        tofu init -backend=false
        tofu apply -auto-approve
    popd > /dev/null
    success "OpenBao configured via Tofu."
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
}

# =============================================================================
# 15. Install gentian-os orchestrator (Helm chart)
# =============================================================================
# The orchestrator chart at charts/gentian-os/ ships:
#   - CRDs: tenants, appprofiles, integrationbindings, appcatalogues
#   - Deployment + ServiceAccount + ClusterRole(Binding) for the operator
#   - ServiceMonitor + Grafana dashboard
# Once installed, applying a Tenant CR is enough to drive end-to-end provisioning.
install_orchestrator() {
    banner "Step 15 — gentian-os orchestrator (CRDs + operator)"

    local chart_dir="${SCRIPT_DIR}/charts/gentian-os"
    local crd_dir="${chart_dir}/crds"
    local ns="gentian-system"
    local required_crds=(
        tenants.gentianos.io
        appprofiles.gentianos.io
        integrationbindings.gentianos.io
        appcatalogues.gentianos.io
    )

    if ! kubectl get namespace "$ns" >/dev/null 2>&1; then
        kubectl create namespace "$ns"
    fi

    info "Applying orchestrator CRDs (hard requirement)..."
    if [[ ! -d "$crd_dir" ]]; then
        error "CRD directory not found: ${crd_dir}"
        exit 1
    fi
    kubectl apply -f "$crd_dir"

    info "Installing/upgrading gentian-os Helm release in namespace '${ns}'..."
    helm upgrade --install gentian-os "$chart_dir" \
        --namespace "$ns" \
        --set openbao.address="http://openbao.openbao.svc.cluster.local:8200" \
        --set argocd.namespace="argocd" \
        --set kernelDomain="${KERNEL_DOMAIN}" \
        --wait --timeout 5m

    info "Waiting for orchestrator CRDs to be Established..."
    for crd in "${required_crds[@]}"; do
        kubectl wait --for=condition=Established "crd/${crd}" --timeout=60s >/dev/null || {
            error "Required CRD ${crd} was not established."
            error "Orchestrator install is incomplete; aborting."
            exit 1
        }
    done
    success "All orchestrator CRDs are Established."

    success "Orchestrator installed; cluster is ready to provision tenants."
    info "Apply a Tenant CR to provision your first tenant, e.g.:"
    info "  kubectl apply -f ${SCRIPT_DIR}/config/samples/tenant_gtn-demo.yaml"
}

# =============================================================================
# Verify ArgoCD Applications
# =============================================================================
# Polls every 15s for up to ${VERIFY_TIMEOUT:-600}s. Considers the platform
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
# Summary
# =============================================================================
print_summary() {
    local argocd_pw
    local argocd_url
    local cluster_admin_pw
    local keycloak_admin_pw
    local nubus_secret_ns
    nubus_secret_ns="gentian-dev"

    argocd_pw=$(kubectl get secret argocd-initial-admin-secret -n argocd \
                    -o jsonpath='{.data.password}' 2>/dev/null | base64 -d 2>/dev/null || echo "(not-ready)")
    cluster_admin_pw=$(kubectl get secret nubus-credentials -n "${nubus_secret_ns}" \
                        -o jsonpath='{.data.admin-password}' 2>/dev/null | base64 -d 2>/dev/null || echo "(not-ready)")
    keycloak_admin_pw=$(kubectl get secret nubus-credentials -n "${nubus_secret_ns}" \
                        -o jsonpath='{.data.keycloak-admin-password}' 2>/dev/null | base64 -d 2>/dev/null || echo "(not-ready)")
    argocd_url=$(resolve_argocd_url)
    portal_url="https://portal.${KERNEL_DOMAIN}"
    keycloak_url="https://id.${KERNEL_DOMAIN}"

    echo ""
    if [[ "${VERIFY_STATUS:-unknown}" == "ok" ]]; then
        echo -e "${GREEN}╔══════════════════════════════════════════════════════════╗${NC}"
        echo -e "${GREEN}║  ✅  Gentian OS bootstrap complete — all systems healthy! ║${NC}"
        echo -e "${GREEN}╠══════════════════════════════════════════════════════════╣${NC}"
        echo -e "${GREEN}║  Portal URL   : ${portal_url}${NC}"
        echo -e "${GREEN}║  Portal login : Administrator / ${cluster_admin_pw}${NC}"
        echo -e "${GREEN}║  Keycloak URL : ${keycloak_url}${NC}"
        echo -e "${GREEN}║  Keycloak login (master realm) : admin / ${keycloak_admin_pw}${NC}"
        echo -e "${GREEN}║  ArgoCD URL   : ${argocd_url}${NC}"
        echo -e "${GREEN}║  ArgoCD login : admin / ${argocd_pw}${NC}"
        echo -e "${GREEN}║  Applications : ${VERIFY_TOTAL:-?} Synced + Healthy${NC}"
        echo -e "${GREEN}╚══════════════════════════════════════════════════════════╝${NC}"
        echo ""
        echo "  ✔ ArgoCD reachable"
        echo "  ✔ All Applications Synced + Healthy"
        echo "  ✔ AppCatalogue CRD installed"
        echo "  ✔ gentian-os orchestrator running (Tenant CRD Established)"
        echo "  ✔ Cluster admin credentials materialized (nubus-credentials)"
        echo ""
        echo "  Retrieve credentials later:"
        echo "    kubectl get secret nubus-credentials -n ${nubus_secret_ns} -o jsonpath='{.data.admin-password}' | base64 -d"
        echo "    kubectl get secret nubus-credentials -n ${nubus_secret_ns} -o jsonpath='{.data.keycloak-admin-password}' | base64 -d"
        echo ""
        echo "  Monitor sync:    kubectl get applications -n argocd"
        echo "  Provision tenant: kubectl apply -f config/samples/tenant_gtn-demo.yaml"
        echo "  Install apps:    kubectl gentian install <profile>"
        echo ""
        echo -e "${GREEN}🎉  Gentian OS successfully installed. Welcome aboard!${NC}"
    else
        echo -e "${YELLOW}╔══════════════════════════════════════════════════════════╗${NC}"
        echo -e "${YELLOW}║  ⚠  Gentian OS bootstrap finished with degraded Apps     ║${NC}"
        echo -e "${YELLOW}╠══════════════════════════════════════════════════════════╣${NC}"
        echo -e "${YELLOW}║  Portal URL   : ${portal_url}${NC}"
        echo -e "${YELLOW}║  Portal login : Administrator / ${cluster_admin_pw}${NC}"
        echo -e "${YELLOW}║  Keycloak URL : ${keycloak_url}${NC}"
        echo -e "${YELLOW}║  Keycloak login (master realm) : admin / ${keycloak_admin_pw}${NC}"
        echo -e "${YELLOW}║  ArgoCD URL   : ${argocd_url}${NC}"
        echo -e "${YELLOW}║  ArgoCD login : admin / ${argocd_pw}${NC}"
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
        echo "    kubectl get secret nubus-credentials -n ${nubus_secret_ns} -o jsonpath='{.data.admin-password}' | base64 -d"
        echo "    kubectl get secret nubus-credentials -n ${nubus_secret_ns} -o jsonpath='{.data.keycloak-admin-password}' | base64 -d"
        echo ""
        echo "  Re-run verification only:"
        echo "    VERIFY_TIMEOUT=600 ./install.sh --verify-only   # (or just wait + re-check)"
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
    [[ "${INSTALL_VALIDATE_ONLY}" == "1" ]] && { load_install_state; try_load_creds_from_openbao; validate_config; }
    load_install_state
    try_load_creds_from_openbao
    prompt_credentials
    prompt_app_repos
    prompt_kernel_domain
    prompt_kernel_secrets
    check_prereqs
    install_tools
    create_namespaces
    prewarm_cluster
    install_cert_manager
    install_kernel_cert_resources
    install_eso
    install_argocd
    setup_argocd_repos
    bootstrap_transit_app
    init_openbao_transit
    bootstrap_argocd_apps
    init_openbao
    run_tofu_openbao_init
    seed_secrets
    install_kernel_wildcard
    bootstrap_root_appset
    install_app_catalogue
    install_appprofiles_sync
    install_orchestrator
    verify_argocd_apps || true
    print_summary
}

main "$@"
