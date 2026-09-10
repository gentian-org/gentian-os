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
    # Nothing to reverse: apply() waits, it does not create.
    #
    # This used to call _delete_kyverno_scaffold, which deleted objects Argo CD
    # owns while Argo CD was still running — drive_reverse walks the list
    # backwards, so C-04 is torn down two steps before C-02 removes the root
    # appset. Kyverno actually goes with that removal: the Application carries
    # resources-finalizer.argocd.argoproj.io, so deleting gentian-appsets
    # cascades through the children and takes the chart with it.
    #
    # The scaffold sweep still exists, for the case where Argo CD was already
    # broken and that cascade never happens. It runs at the tail of
    # A-09-argocd's destroy instead — the first point in the reverse pass where
    # Argo is gone and cannot put back what the sweep removes, and still before
    # A-03 deletes namespaces that a fail-closed webhook would block.
    #
    # No position in the step list can do better. The same order drives both
    # directions, and this step must apply AFTER C-02 because that is when
    # Kyverno appears — which forces it to destroy BEFORE C-02.
    return 0
}
