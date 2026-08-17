#!/usr/bin/env bash
# step: A-09-argocd
# phase: control-plane
# requires: A-03-namespaces
# provides: ArgoCD server and controllers
# mutates: namespace argocd, ArgoCD CRDs
# pins: argocd

check() {
    # Includes the ApplicationSet CRD: the manifest creates Deployments before
    # CRDs, so a run that failed on the CRDs leaves a server that answers this
    # check and a cluster that cannot compose an ApplicationSet.
    argocd_installed
}

apply() {
    install_argocd
}

_argocd_strip_all_finalizers() {
    local kind
    # All three kinds, not just Applications: an ApplicationSet finalizer holds
    # the namespace open just as effectively, and AppProjects are deleted last
    # by the chart uninstall.
    for kind in applications applicationsets appprojects; do
        _argocd_strip_kubectl "${kind}.argoproj.io" || true
        _argocd_strip_raw "/apis/argoproj.io/v1alpha1/namespaces/argocd/${kind}" || true
    done
}

destroy() {
    # The chart goes first, and the finalizers after it.
    #
    # Stripping while Argo CD is running accomplishes nothing: the application
    # controller re-adds resources-finalizer.argocd.argoproj.io as fast as the
    # patch clears it, and any ApplicationSet still reconciling recreates the
    # Applications outright. The namespace then sits Terminating on dozens of
    # Applications that nothing reconciles any more, and the namespace delete
    # below blocks in its waiter with no indication of why.
    if helm status argocd -n argocd >/dev/null 2>&1; then
        gentian_run helm uninstall argocd -n argocd || true
    fi
    _argocd_strip_all_finalizers

    # --wait=false plus an explicit loop, rather than letting kubectl block.
    #
    # kubectl prints "namespace argocd deleted" on the delete response and only
    # then waits, so a hang here reads as a completed step. The loop also gets a
    # second pass at objects that were still being written when the first strip
    # ran, and says what is holding the namespace instead of waiting forever.
    kubectl delete namespace argocd --ignore-not-found=true --wait=false 2>/dev/null || true

    local deadline=$(( SECONDS + 180 ))
    while kubectl get namespace argocd >/dev/null 2>&1; do
        if (( SECONDS >= deadline )); then
            warn "namespace argocd is still Terminating after 180s:"
            kubectl get namespace argocd -o jsonpath='{range .status.conditions[?(@.status=="True")]}{.type}: {.message}{"\n"}{end}' \
                2>/dev/null | sed 's/^/       /' || true
            break
        fi
        _argocd_strip_all_finalizers
        sleep 5
    done
}
