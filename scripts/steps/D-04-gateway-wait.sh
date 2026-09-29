#!/usr/bin/env bash
# step: D-04-gateway-wait
# phase: applications
# requires: D-03-vault-oidc-config
# provides: kernel Gateway reporting Programmed
# check: none — a pure wait, non-fatal by design; a Gateway that is not yet Programmed does not invalidate the steps that follow
# mutates: nothing — waits on a condition

# One place that asserts the Gateway is programmed, named in the step graph so
# the dependency is something validate-steps and --status can see.
#
# v5 had none, and D-02 is the first step that reaches the cluster over a
# kernel hostname. A Gateway stuck on an invalid listener — a missing
# wildcard-tls, most often — surfaced inside the realm bootstrap as a
# connection failure rather than as a named step, which is a diagnosability
# loss rather than a functional one. It runs after the steps that need it
# rather than before, because those steps wait for what they need in their own
# right; what this adds is saying which thing is wrong.
#
# SERVICES_NAMESPACE, because wait_for_gateway_platform resolves its namespace
# through a lookup that reads the operator Deployment from gentian-system.
# C-03 sets it the same way for the same reason.

apply() {
    export SERVICES_NAMESPACE
    SERVICES_NAMESPACE="$(ns_kernel edge)"
    wait_for_gateway_platform || warn "Gateway platform not ready; continuing."
}
