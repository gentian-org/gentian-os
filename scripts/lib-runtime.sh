#!/usr/bin/env bash
# =============================================================================
# lib-runtime.sh — Shared CRI / kubelet runtime helpers
# =============================================================================
# Sourced by install.sh and uninstall.sh. Keeps the sudo-prompt logic and the
# microk8s/containerd state-recovery helpers in one place so behaviour stays
# consistent between bootstrap and teardown.
#
# Required globals (must be defined by the caller before sourcing):
#   info(), success(), warn(), error()    — colourised log helpers
#
# Public functions:
#   ensure_sudo [REASON]
#       Cache-aware sudo prompt. Returns 0 if sudo is available (or already
#       cached / NOPASSWD), 1 otherwise.
#
#   cri_cleanup
#       Best-effort sweep of stale CRI state (orphan sandboxes, exited
#       containers, stopped tasks) that accumulates from prior install /
#       uninstall cycles and wedges kubelet's pod-sync loop. Auto-detects
#       crictl or microk8s.ctr. No-op (with warning) if neither is present
#       or sudo is unavailable.
#
#   kubelite_restart
#       Last-resort recovery for the kubelet status-sync wedge on microk8s.
#       Restarts the kubelite snap unit. No-op on non-microk8s clusters or
#       when sudo is unavailable.
# =============================================================================

# Guard against double-sourcing.
if [[ -n "${__GENTIAN_LIB_RUNTIME_SH:-}" ]]; then
    return 0 2>/dev/null
fi
__GENTIAN_LIB_RUNTIME_SH=1

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

cri_cleanup() {
    # Disable strict mode locally — this helper is best-effort and must
    # never fail the caller. Restore on return.
    local _prev_opts; _prev_opts=$(set +o); set +eo pipefail

    _cri_cleanup_impl() {
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
