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
# Local on-disk cache of the credentials prompted on the first run, so that
# re-running install.sh after a partial failure does not re-prompt. The file
# is gitignored and chmod 600. Set INSTALL_SECRETS_CACHE=/dev/null to disable.
INSTALL_SECRETS_CACHE="${INSTALL_SECRETS_CACHE:-${SCRIPT_DIR}/.install-secrets.env}"

# ─── Versions ────────────────────────────────────────────────────────────────
TOFU_VERSION="1.9.0"
BAO_VERSION="2.5.1"

usage() {
    cat <<'EOF'
Usage: ./install.sh [options]

Options:
  --no-cluster-infra   Skip cluster infra installation (cert-manager, reloader, CNPG)
  --cluster-infra      Force cluster infra installation (default)
  -h, --help           Show this help

Environment overrides:
  INSTALL_CLUSTER_INFRA=1|0
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
        # shellcheck disable=SC1090
        source "${INSTALL_SECRETS_CACHE}" || return 0
        info "Loaded cached credentials from ${INSTALL_SECRETS_CACHE}."
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
                   OD_SMTP_RELAY_USERNAME OD_SMTP_RELAY_PASSWORD; do
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
    : "${GENTIAN_APPS_REPO:=https://github.com/gentian-org/gentian-apps}"
    : "${GENTIAN_APPS_BRANCH:=main}"
    : "${GENTIAN_DEPLOYMENTS_REPO:=https://github.com/gentian-org/gentian-deployments}"
    : "${GENTIAN_DEPLOYMENTS_BRANCH:=main}"

    if [[ "${GENTIAN_NONINTERACTIVE:-0}" != "1" ]]; then
        echo ""
        info "App catalogue and deployment repositories (press <Enter> to accept defaults):"
        local v
        read -rp "  gentian-apps repo URL [${GENTIAN_APPS_REPO}]: " v
        [[ -n "$v" ]] && GENTIAN_APPS_REPO="$v"
        read -rp "  gentian-apps branch [${GENTIAN_APPS_BRANCH}]: " v
        [[ -n "$v" ]] && GENTIAN_APPS_BRANCH="$v"
        read -rp "  gentian-deployments repo URL [${GENTIAN_DEPLOYMENTS_REPO}]: " v
        [[ -n "$v" ]] && GENTIAN_DEPLOYMENTS_REPO="$v"
        read -rp "  gentian-deployments branch [${GENTIAN_DEPLOYMENTS_BRANCH}]: " v
        [[ -n "$v" ]] && GENTIAN_DEPLOYMENTS_BRANCH="$v"
    fi
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
# 3. Install cert-manager via Helm
# =============================================================================
install_cert_manager() {
    if [[ "$INSTALL_CLUSTER_INFRA" != "1" ]]; then
        warn "Cluster infra disabled: skipping cert-manager installation."
        return
    fi

    banner "Step 3 — Installing cert-manager"

    if helm status cert-manager -n cert-manager &>/dev/null; then
        success "cert-manager already installed (Helm release present). Skipping."
        return
    fi

    # Detect non-Helm installs (e.g. `microk8s enable cert-manager`) to avoid
    # ownership-metadata conflicts during `helm install`.
    if kubectl get deployment cert-manager -n cert-manager &>/dev/null \
        || kubectl get crd certificates.cert-manager.io &>/dev/null; then
        warn "cert-manager already present but not managed by Helm (e.g. microk8s addon)."
        warn "Skipping Helm install. Use that installation as-is, or remove it first to let install.sh manage it."
        return
    fi

    helm repo add jetstack https://charts.jetstack.io --force-update
    helm repo update
    helm install cert-manager jetstack/cert-manager \
        -n cert-manager \
        --create-namespace \
        --set crds.enabled=true \
        --wait --timeout 5m
    success "cert-manager installed."
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
    local argocd_pw
    argocd_pw=$(kubectl get secret argocd-initial-admin-secret -n argocd \
                    -o jsonpath='{.data.password}' 2>/dev/null \
                    | base64 -d 2>/dev/null || echo "")
    if [[ -n "$argocd_pw" ]]; then
        info "ArgoCD URL   : https://${NODE_IP}:30443"
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
    # Bucket) to register its informers — even though we never instantiate
    # them. Without these CRDs the controller crash-loops on cache-sync
    # timeout. See kernel/manifests/flux-crds/README.md.
    kubectl apply -f "${SCRIPT_DIR}/kernel/manifests/flux-crds/source-crds.yaml"
    success "Applied Flux source CRDs (required by tofu-controller)."

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
    local ns="gentian-system"

    if ! kubectl get namespace "$ns" >/dev/null 2>&1; then
        kubectl create namespace "$ns"
    fi

    info "Installing/upgrading gentian-os Helm release in namespace '${ns}'..."
    helm upgrade --install gentian-os "$chart_dir" \
        --namespace "$ns" \
        --set openbao.address="http://openbao.openbao.svc.cluster.local:8200" \
        --set argocd.namespace="argocd" \
        --wait --timeout 5m

    info "Waiting for Tenant CRD to be Established..."
    kubectl wait --for=condition=Established crd/tenants.gentianos.io --timeout=60s \
        || warn "Tenant CRD not yet Established — check 'kubectl get crds | grep gentianos'"

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
            echo "$bad_lines" | sed 's/^/    /'
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
    argocd_pw=$(kubectl get secret argocd-initial-admin-secret -n argocd \
                    -o jsonpath='{.data.password}' 2>/dev/null | base64 -d 2>/dev/null || echo "(not-ready)")

    echo ""
    if [[ "${VERIFY_STATUS:-unknown}" == "ok" ]]; then
        echo -e "${GREEN}╔══════════════════════════════════════════════════════════╗${NC}"
        echo -e "${GREEN}║  ✅  Gentian OS bootstrap complete — all systems healthy! ║${NC}"
        echo -e "${GREEN}╠══════════════════════════════════════════════════════════╣${NC}"
        echo -e "${GREEN}║  ArgoCD URL   : https://${NODE_IP}:30443${NC}"
        echo -e "${GREEN}║  ArgoCD login : admin / ${argocd_pw}${NC}"
        echo -e "${GREEN}║  Applications : ${VERIFY_TOTAL:-?} Synced + Healthy${NC}"
        echo -e "${GREEN}╚══════════════════════════════════════════════════════════╝${NC}"
        echo ""
        echo "  ✔ ArgoCD reachable"
        echo "  ✔ All Applications Synced + Healthy"
        echo "  ✔ AppCatalogue CRD installed"
        echo "  ✔ gentian-os orchestrator running (Tenant CRD Established)"
        echo ""
        echo "  Monitor sync:    kubectl get applications -n argocd"
        echo "  Provision tenant: kubectl apply -f config/samples/tenant_gtn-demo.yaml"
        echo "  Install apps:    kubectl gentian install <profile>"
    else
        echo -e "${YELLOW}╔══════════════════════════════════════════════════════════╗${NC}"
        echo -e "${YELLOW}║  ⚠  Gentian OS bootstrap finished with degraded Apps     ║${NC}"
        echo -e "${YELLOW}╠══════════════════════════════════════════════════════════╣${NC}"
        echo -e "${YELLOW}║  ArgoCD URL   : https://${NODE_IP}:30443${NC}"
        echo -e "${YELLOW}║  ArgoCD login : admin / ${argocd_pw}${NC}"
        echo -e "${YELLOW}║  Status       : ${VERIFY_STATUS:-unknown} (${VERIFY_TOTAL:-0} apps)${NC}"
        echo -e "${YELLOW}╚══════════════════════════════════════════════════════════╝${NC}"
        echo ""
        echo "  Inspect failing Applications:"
        echo "    kubectl get applications -n argocd"
        echo "    kubectl describe application -n argocd <name>"
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
    load_creds_cache
    try_load_creds_from_openbao
    prompt_credentials
    prompt_app_repos
    check_prereqs
    install_tools
    create_namespaces
    install_cert_manager
    install_eso
    install_argocd
    setup_argocd_repos
    bootstrap_transit_app
    init_openbao_transit
    bootstrap_argocd_apps
    init_openbao
    run_tofu_openbao_init
    seed_secrets
    bootstrap_root_appset
    install_app_catalogue
    install_appprofiles_sync
    install_orchestrator
    verify_argocd_apps || true
    print_summary
}

main "$@"
