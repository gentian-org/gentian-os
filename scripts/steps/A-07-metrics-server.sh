#!/usr/bin/env bash
# step: A-07-metrics-server
# phase: control-plane
# requires: A-01-namespaces
# provides: metrics-server in kube-system (API aggregation convention), or the platform's own
# mutates: kube-system, the metrics.k8s.io APIService
# pins: metrics-server

# metrics-server stays in kube-system: it is an aggregated API and that is where
# the convention and most distributions put it. A cluster that already has one
# (k3s, managed clusters) is left alone.

check() {
    _a07_served || return 1
    # The platform's own release, where there is one, is at the pinned chart;
    # a cluster behind the pin is upgraded by a re-run. A metrics-server the
    # platform did not install (k3s, a managed cluster) has no release here
    # and is left alone.
    # Asked of Kubernetes, not of helm, which the driver does not configure
    # for check(): the chart labels the Deployment it renders with its own
    # name and version (helm.sh/chart), which is the release's version too.
    local chart
    chart="$(kubectl get deployment metrics-server -n kube-system -o jsonpath='{.metadata.labels.helm\.sh/chart}' 2>/dev/null)"
    if [[ -n "${chart}" ]]; then
        [[ "${chart}" == "metrics-server-$(gentian_pin metrics-server chart)" ]] || return 1
    fi
    return 0
}

_a07_served() {
    kubectl get apiservice v1beta1.metrics.k8s.io -o jsonpath='{.status.conditions[?(@.type=="Available")].status}' 2>/dev/null | grep -q True
}

apply() {
    banner "metrics-server"
    if _a07_served && [[ -z "$(kubectl get deployment metrics-server -n kube-system -o jsonpath='{.metadata.labels.helm\.sh/chart}' 2>/dev/null)" ]]; then
        success "metrics.k8s.io is already served; leaving the platform's metrics-server alone."
        return 0
    fi
    helm repo add metrics-server "$(gentian_pin metrics-server repo)" --force-update >/dev/null
    helm repo update metrics-server >/dev/null
    # The kubelets' certificates are verified unless the claim says otherwise
    # (metricsServer.kubeletInsecureTLS); the schema says what giving that up
    # costs. Left unset, verification is tried first and given up only where
    # it cannot work: kubelet certificates that name no IP address.
    local setting="${METRICS_SERVER_KUBELET_INSECURE_TLS:-}"
    if [[ "${setting}" == "true" ]]; then
        warn "metricsServer.kubeletInsecureTLS is true: metrics-server will not verify the kubelets' certificates."
        _a07_install --kubelet-insecure-tls || return 1
        _a07_wait
        return
    fi
    _a07_install || return 1
    local rc=0
    _a07_wait detect || rc=$?
    [[ "${rc}" -eq 2 ]] || return "${rc}"
    if [[ "${setting}" == "false" ]]; then
        error "metrics-server cannot verify the kubelets' certificates: they name no IP address."
        error "  metricsServer.kubeletInsecureTLS is false in the claim; set it to true, or remove it"
        error "  to let the installer decide, and run ./install.sh --only A-07 again."
        return 1
    fi
    warn "The kubelets' certificates name no IP address, so metrics-server cannot verify them."
    warn "  Reinstalling it with --kubelet-insecure-tls. Set metricsServer.kubeletInsecureTLS in the"
    warn "  claim to true to accept this, or to false to stop the install here instead."
    _a07_install --kubelet-insecure-tls || return 1
    _a07_wait
}

# _a07_install [flag] — the pinned chart, with the one metrics-server flag
# this step may add. Without --wait: _a07_wait reads the outcome, which helm's
# wait cannot tell apart from a slow start.
_a07_install() {
    local args=()
    [[ -n "${1:-}" ]] && args=(--set "args={$1}")
    _helm_retry upgrade --install metrics-server metrics-server/metrics-server \
        --namespace kube-system --version "$(gentian_pin metrics-server chart)" \
        ${args[@]+"${args[@]}"}
}

# _a07_wait [detect] — 0 once metrics-server is Ready, 1 after five minutes.
# With detect, 2 as soon as a metrics-server pod's log (the new one's, while an
# old one still runs) says the kubelets' certificates name no IP address.
# Without, that log line is waited out: it may still come from the
# pod being replaced.
_a07_wait() {
    local deadline=$((SECONDS + 300))
    while (( SECONDS < deadline )); do
        kubectl rollout status deployment/metrics-server -n kube-system --timeout=10s >/dev/null 2>&1 && return 0
        if [[ "${1:-}" == "detect" ]] && kubectl logs -l app.kubernetes.io/name=metrics-server -n kube-system --tail=100 2>/dev/null \
            | grep -q "doesn't contain any IP SANs"; then
            return 2
        fi
    done
    error "metrics-server did not become Ready within 5 minutes:"
    kubectl logs deployment/metrics-server -n kube-system --tail=10 2>&1 | sed 's/^/  /' >&2
    return 1
}

destroy() {
    # Only what this installer put there: a platform's own metrics-server is
    # not ours to remove.
    if helm status metrics-server -n kube-system >/dev/null 2>&1; then
        helm uninstall metrics-server -n kube-system >/dev/null 2>&1 || true
    fi
}
