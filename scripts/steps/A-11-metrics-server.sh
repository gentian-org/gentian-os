#!/usr/bin/env bash
# step: A-11-metrics-server
# phase: control-plane
# requires: A-03-namespaces
# provides: metrics.k8s.io — live CPU/memory for the Resources tab, kubectl top, HPA
# mutates: namespace kube-system (deployment metrics-server), APIService v1beta1.metrics.k8s.io
# pins: metrics-server

# Optional, and the platform is correct without it.
#
# What a tenant is billed for is what the ResourceQuota committed on their
# behalf, which the API server already reports; that series needs nothing
# installed. metrics-server answers a different question — whether the plan a
# tenant pays for is the plan they need — and a cluster that does not care to
# ask it can skip this step, leaving the Resources tab showing ceilings and
# committed usage with the live comparison marked unavailable.
#
# It is not a small Prometheus and does not grow into one: metrics.k8s.io serves
# the latest value only, in memory, with no history and no query language. The
# history behind the Resources tab is Gentian's own, sampled into each tenant's
# shell database, which is why adopting Prometheus later is a change of source
# rather than a restart of the record.

check() {
    kubectl get apiservice v1beta1.metrics.k8s.io >/dev/null 2>&1 &&
        kubectl get deployment metrics-server -n kube-system >/dev/null 2>&1
}

apply() {
    banner "Installing metrics-server"

    if helm status metrics-server -n kube-system >/dev/null 2>&1; then
        success "metrics-server already installed. Skipping."
        return
    fi

    helm repo add metrics-server "$(gentian_pin metrics-server repo)" --force-update
    helm repo update

    # --kubelet-insecure-tls is set deliberately and is the norm for a cluster
    # whose kubelets serve the self-signed certificates kubeadm issues. Without
    # it metrics-server rejects every kubelet it scrapes and reports no metrics
    # at all, while appearing healthy — the failure looks like an empty cluster
    # rather than a trust problem. The connection stays inside the node network
    # and carries no secrets; what it costs is authentication of the kubelet to
    # metrics-server, not confidentiality of anything a tenant owns.
    #
    # A cluster whose kubelets carry certificates from a CA the API server
    # trusts should drop this flag in its own values.
    gentian_run helm upgrade --install metrics-server metrics-server/metrics-server \
        -n kube-system \
        --version "$(gentian_pin metrics-server chart)" \
        --set 'args={--kubelet-insecure-tls}' \
        --wait --timeout 5m

    success "metrics-server installed."
    info "Enable the live usage series with usage.metricsServer.enabled=true in the operator's values."
}

destroy() {
    if helm status metrics-server -n kube-system >/dev/null 2>&1; then
        gentian_run helm uninstall metrics-server -n kube-system || true
    fi
    # The APIService is chart-owned and goes with the release. Left behind, an
    # APIService with no backing Service makes every `kubectl get --all` in the
    # cluster slow and noisy, which is a confusing thing to inherit from an
    # add-on someone removed on purpose.
    kubectl delete apiservice v1beta1.metrics.k8s.io --ignore-not-found=true >/dev/null 2>&1 || true
}
