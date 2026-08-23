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
    # A metrics-server this installer does not own is not this step's to report
    # on. apply() deliberately leaves it alone — Helm cannot adopt objects it
    # did not create — so answering "missing" here would name this step as
    # outstanding on every run of a cluster where nothing is wrong and nothing
    # can be done. Undefined is the honest verdict: not applicable here.
    if ! helm status metrics-server -n kube-system >/dev/null 2>&1 &&
       kubectl get deployment metrics-server -n kube-system >/dev/null 2>&1; then
        return "${CHECK_UNDEFINED}"
    fi

    kubectl get apiservice v1beta1.metrics.k8s.io >/dev/null 2>&1 &&
        kubectl get deployment metrics-server -n kube-system >/dev/null 2>&1
}

apply() {
    banner "Installing metrics-server"

    if helm status metrics-server -n kube-system >/dev/null 2>&1; then
        success "metrics-server already installed. Skipping."
        return
    fi

    # A metrics-server this installer did not put here.
    #
    # microk8s ships one as an addon, applied with plain kubectl and labelled
    # k8s-app=metrics-server, and other distros do the same. Helm refuses to
    # adopt those objects ("cannot be imported into the current release:
    # invalid ownership metadata"), so the install below cannot succeed and
    # must not be attempted — this step is explicitly optional, and a cluster
    # that already serves metrics.k8s.io needs nothing from it.
    if ! helm status metrics-server -n kube-system >/dev/null 2>&1 &&
       kubectl get deployment metrics-server -n kube-system >/dev/null 2>&1; then
        info "A metrics-server is already running in kube-system that this"
        info "  installer does not own (no Helm release). Leaving it alone —"
        info "  Helm cannot adopt objects it did not create, and metrics.k8s.io"
        info "  is metrics.k8s.io whoever installed it."
        return 0
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
    # Checked explicitly, not left to `set -e`. The driver calls apply() as
    # `apply || rc=$?`, and testing a function's status that way disables
    # errexit for its whole body — so an unchecked failure here does not stop
    # the step, it falls through to the success line below and the step
    # returns 0. That is exactly what happened: helm refused to adopt a
    # foreign metrics-server, printed its error, and the step reported
    # "[OK] metrics-server installed" over it.
    if ! gentian_run helm upgrade --install metrics-server metrics-server/metrics-server \
        -n kube-system \
        --version "$(gentian_pin metrics-server chart)" \
        --set 'args={--kubelet-insecure-tls}' \
        --wait --timeout 5m; then
        error "helm could not install metrics-server (see its error above)."
        return 1
    fi

    success "metrics-server installed."
    info "Enable the live usage series with usage.metricsServer.enabled=true in the operator's values."
}

destroy() {
    # Nothing of ours here: no Helm release means this installer never
    # installed metrics-server, and the APIService below belongs to whoever
    # did.
    #
    # This guard is the whole point. Deleting the APIService unconditionally
    # took out metrics.k8s.io for a microk8s addon that predated this
    # installer by 708 days — `kubectl top` and every HPA on the machine
    # stopped working, from a --purge of a product that never installed it.
    # "An APIService with no backing Service is noisy" only justifies removing
    # one whose Service we are the reason is gone.
    helm status metrics-server -n kube-system >/dev/null 2>&1 || return 0

    gentian_run helm uninstall metrics-server -n kube-system || true
    # Chart-owned, so it goes with the release. Left behind, an APIService
    # with no backing Service makes every `kubectl get --all` in the cluster
    # slow and noisy, which is a confusing thing to inherit from an add-on
    # someone removed on purpose.
    kubectl delete apiservice v1beta1.metrics.k8s.io --ignore-not-found=true >/dev/null 2>&1 || true
}
