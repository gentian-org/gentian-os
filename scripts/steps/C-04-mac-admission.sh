#!/usr/bin/env bash
# step: C-04-mac-admission
# phase: platform
# requires: C-02-root-appset
# provides: the Kyverno admission controller, Available (Stage 0 MAC)
# mutates: nothing on the forward pass — waits on a condition

# This step does not install anything. Kyverno and its baseline ClusterPolicies
# are Argo CD's, from kernel/appsets/raw/05-admission.yaml, with prune and
# selfHeal on. What apply() does is wait for the admission controller to report
# Available, so the steps after it do not create workloads that a
# not-yet-ready webhook silently admits.
#
# The header used to claim it provided the policies and mutated ClusterPolicy
# objects. It did once; C-02 taking over the appsets left the claim behind, and
# a step that overstates what it owns is a step whose destroy() gets written
# against the wrong thing — which is what happened below.

check() {
    # Deliberately Argo CD's objects, not this step's. The contract here is
    # readiness, so "are the policies live" IS the question — unlike a step that
    # owns something, where checking another owner's work reports satisfied on
    # the strength of what it does not do.
    kubectl get crd clusterpolicies.kyverno.io >/dev/null 2>&1 &&
        [[ -n "$(kubectl get clusterpolicy -o name 2>/dev/null)" ]]
}

apply() {
    install_mac_admission
}

destroy() {
    # Reverse-pass ordering makes most of this a fight Argo CD wins.
    #
    # drive_reverse walks the same list backwards, so C-04 is torn down BEFORE
    # C-02-root-appset and long before A-09-argocd: Argo is still running, still
    # syncing 05-admission.yaml, and selfHeal puts back every ClusterPolicy this
    # deletes. The helm uninstall branch is deader still — Argo renders the
    # chart and applies the manifests, so there is no Helm release to find.
    #
    # It is left in place because it is also the sweep that clears Kyverno's
    # cluster-scoped webhook configurations, which fail closed and would block
    # the next install if they outlived the cluster. That part still earns its
    # keep once Argo is gone. Making the rest honest means either ordering this
    # after the appset teardown or narrowing it to the webhook sweep.
    _delete_kyverno_scaffold || true
}
