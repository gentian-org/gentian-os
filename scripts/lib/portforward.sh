#!/usr/bin/env bash
# =============================================================================
# scripts/lib/portforward.sh — reach in-cluster Services from the install host
# =============================================================================
# Deliberately self-contained: sourced both through scripts/lib/load.sh and
# directly by standalone scripts (scripts/init-openbao-transit.sh). Depends only
# on kubectl, curl and python3 — all already required by pre-flight.
#
# Why this exists
# ---------------
# install.sh talks HTTP to OpenBao by resolving the Service's ClusterIP and
# curling it from the operator's machine. That assumes the host shares the
# cluster's Service network: true for single-node k3s or a local minikube,
# false for every remote/managed cluster. Against a managed cluster the
# ClusterIP just blackholes, and the installer reports
#
#   OpenBao HTTP listener at http://10.109.96.221:8200 never accepted
#   connections within 180s
#
# while the pod is perfectly healthy and serving — a failure message that
# points at the workload when the actual problem is host routing.
#
# gentian_service_addr keeps the direct ClusterIP as the fast path, so clusters
# where this already worked behave exactly as before, and falls back to a
# kubectl port-forward, which needs nothing beyond the kubeconfig in use.
# =============================================================================

[[ -n "${GENTIAN_PORTFORWARD_LOADED:-}" ]] && return 0
GENTIAN_PORTFORWARD_LOADED=1

# PIDs of port-forwards we started, one per line, tracked in a FILE rather than
# a shell variable on purpose.
#
# gentian_service_addr echoes its result, so every caller invokes it through
# command substitution — which runs in a subshell. A variable written there dies
# with the subshell, leaving the parent's cleanup with nothing to kill and a
# kubectl process orphaned for the rest of the session. A file is shared state
# that survives the subshell; the parent's EXIT trap can then see it.
#
# Bash resets non-ignored traps inside subshells, so the command-substitution
# subshell exiting does NOT fire the cleanup and tear down the forward we just
# established.
_GENTIAN_PF_PIDFILE="${_GENTIAN_PF_PIDFILE:-$(mktemp -t gentian-pf.XXXXXX)}"
export _GENTIAN_PF_PIDFILE

# Kill every port-forward we started. Registered on EXIT so an aborted install
# never leaves orphaned kubectl processes squatting on local ports.
gentian_cleanup_port_forwards() {
    [[ -f "${_GENTIAN_PF_PIDFILE}" ]] || return 0
    local pid
    while read -r pid; do
        [[ -n "${pid}" ]] && kill "${pid}" 2>/dev/null
    done < "${_GENTIAN_PF_PIDFILE}"
    rm -f "${_GENTIAN_PF_PIDFILE}"
    return 0
}
trap gentian_cleanup_port_forwards EXIT

_gentian_free_port() {
    python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

# gentian_service_addr <svc> <namespace> <port> [scheme]
#
# Echoes a base URL the install host can actually reach (no trailing slash).
# Returns 1 if the Service is absent or no route could be established, so
# callers can fail with their own domain-specific message.
gentian_service_addr() {
    local svc="$1" ns="$2" port="$3" scheme="${4:-http}"

    local ip
    ip=$(kubectl get svc "${svc}" -n "${ns}" -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)
    [[ -n "${ip}" && "${ip}" != "None" ]] || return 1

    # Fast path: the host routes to the Service network. Deliberately no -f —
    # any HTTP status (404, 403, a TLS error page) still proves the TCP
    # connection succeeded, which is all we are testing for here.
    if curl -sk -o /dev/null --max-time 3 "${scheme}://${ip}:${port}/" 2>/dev/null; then
        echo "${scheme}://${ip}:${port}"
        return 0
    fi

    local lport
    lport=$(_gentian_free_port 2>/dev/null) || return 1
    [[ -n "${lport}" ]] || return 1

    kubectl port-forward -n "${ns}" "svc/${svc}" "${lport}:${port}" >/dev/null 2>&1 &
    local pid=$!
    echo "${pid}" >> "${_GENTIAN_PF_PIDFILE}"

    local i
    for i in $(seq 1 30); do
        # port-forward died (RBAC, Service vanished) — no point waiting it out.
        kill -0 "${pid}" 2>/dev/null || return 1
        if curl -sk -o /dev/null --max-time 2 "${scheme}://127.0.0.1:${lport}/" 2>/dev/null; then
            echo "${scheme}://127.0.0.1:${lport}"
            return 0
        fi
        sleep 1
    done
    return 1
}
