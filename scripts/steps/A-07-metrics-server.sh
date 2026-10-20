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
    # Verification of the kubelets' certificates is given up only where the
    # claim says so (metricsServer.kubeletInsecureTLS); the schema says what
    # that costs and on which clusters nothing else works.
    local args=()
    if [[ "${METRICS_SERVER_KUBELET_INSECURE_TLS:-$(xrd_default metricsServer.kubeletInsecureTLS)}" == "true" ]]; then
        warn "metricsServer.kubeletInsecureTLS is true: metrics-server will not verify the kubelets' certificates."
        args=(--set 'args={--kubelet-insecure-tls}')
    fi
    _helm_retry upgrade --install metrics-server metrics-server/metrics-server \
        --namespace kube-system --version "$(gentian_pin metrics-server chart)" \
        ${args[@]+"${args[@]}"} \
        --wait --timeout 5m
}

destroy() {
    # Only what this installer put there: a platform's own metrics-server is
    # not ours to remove.
    if helm status metrics-server -n kube-system >/dev/null 2>&1; then
        helm uninstall metrics-server -n kube-system >/dev/null 2>&1 || true
    fi
}
