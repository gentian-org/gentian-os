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
    kubectl get apiservice v1beta1.metrics.k8s.io -o jsonpath='{.status.conditions[?(@.type=="Available")].status}' 2>/dev/null | grep -q True
}

apply() {
    banner "metrics-server"
    if check; then
        success "metrics.k8s.io is already served; leaving the platform's metrics-server alone."
        return 0
    fi
    helm repo add metrics-server "$(gentian_pin metrics-server repo)" --force-update >/dev/null
    helm repo update metrics-server >/dev/null
    _helm_retry upgrade --install metrics-server metrics-server/metrics-server \
        --namespace kube-system --version "$(gentian_pin metrics-server chart)" \
        --wait --timeout 5m
}

destroy() {
    # Only what this installer put there: a platform's own metrics-server is
    # not ours to remove.
    if helm status metrics-server -n kube-system >/dev/null 2>&1; then
        helm uninstall metrics-server -n kube-system >/dev/null 2>&1 || true
    fi
}
