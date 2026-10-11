#!/usr/bin/env bash
# step: D-05-gateway-wait
# phase: applications
# requires: D-04-vault-oidc-config
# provides: kernel Gateway reporting Programmed, and platform.<kernel> resolving publicly
# check: none — a pure wait, non-fatal by design; a Gateway that is not yet Programmed does not invalidate the steps that follow
# mutates: nothing in the cluster — waits on a condition; on this host, the kept copy of the wildcard under ~/.gentian/certs

# One place that asserts the Gateway is programmed, named in the step graph so
# the dependency is something validate-steps and --status can see.
#
# v5 had none, and D-03 is the first step that reaches the cluster over a
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
    # C-03 keeps the wildcard only if it was issued within its own wait; by
    # now it has been, and a cluster wiped without --purge keeps it too.
    save_kernel_wildcard
    # The address the handover sends a person to. It exists from D-03, which
    # creates the platform tenant and with it the desktop that serves it, so
    # this is the first step that can wait for it to resolve.
    [[ -n "${KERNEL_DOMAIN:-}" ]] && gentian_dns_wait_for "platform.${KERNEL_DOMAIN}"
    return 0
}
