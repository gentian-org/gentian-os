#!/usr/bin/env bash
# step: A-04-crossplane
# phase: control-plane
# requires: A-01-namespaces
# provides: Crossplane core and its CRDs in the provisioning namespace, without the default policy that activates every provider resource type
# mutates: the provisioning namespace, Crossplane CRDs, cluster-scoped RBAC
# pins: crossplane

# Which provider resource types exist.
#
# The chart's default (provider.defaultActivations: ["*"]) has Crossplane's
# init container create a ManagedResourceActivationPolicy named "default" that
# activates every type of every provider: several hundred CRDs, of which the
# platform uses a few dozen, each costing API-server memory. With an empty
# list the init container creates no such policy, and the types that exist are
# the ones crossplane/providers/activation.yaml names (applied by B-05).
#
# --set-json, not --set: `--set provider.defaultActivations={}` reaches the
# chart as a list holding one empty string, which renders `--activation ""`
# and creates the default policy after all, with an entry that matches nothing.
#
# CROSSPLANE_ACTIVATE_ALL=true in install.env is the way back: the chart's own
# default, every type activated. It is there for a fresh install that waits on
# a type missing from activation.yaml.
#
# One direction only. Crossplane never deactivates a type, and the init
# container leaves an existing "default" policy alone -- so a cluster that was
# installed with every type keeps every type, whatever is passed here.
_a04_activate_all() {
    [[ "${CROSSPLANE_ACTIVATE_ALL:-false}" == "true" ]]
}

# The value itself, one place for apply() and the tests to read.
_a04_default_activations() {
    if _a04_activate_all; then printf '%s' '["*"]'; else printf '%s' '[]'; fi
}

check() {
    helm_pinned_ok crossplane crossplane provisioning &&
        kubectl get crd compositeresourcedefinitions.apiextensions.crossplane.io >/dev/null 2>&1 || return 1
    # The way back has to reach a cluster that was installed without it: the
    # release is at the pinned version either way, so ask for the policy the
    # init container creates when every type is wanted.
    if _a04_activate_all; then
        kubectl get managedresourceactivationpolicies.apiextensions.crossplane.io default >/dev/null 2>&1 || return 1
    fi
    return 0
}

apply() {
    banner "Crossplane"
    if _a04_activate_all; then
        warn "CROSSPLANE_ACTIVATE_ALL=true: every resource type of every provider is installed (several hundred CRDs)."
    fi
    helm_pinned crossplane crossplane provisioning \
        --set-json "provider.defaultActivations=$(_a04_default_activations)"
}

destroy() {
    helm uninstall crossplane -n "$(ns_kernel provisioning)" >/dev/null 2>&1 || true
}
