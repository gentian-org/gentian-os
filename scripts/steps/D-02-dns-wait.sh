#!/usr/bin/env bash
# step: D-02-dns-wait
# phase: applications
# requires: D-01-operator
# provides: id.<kernel> resolving publicly, for the OIDC discovery D-03 is the first to need
# check: none — a pure wait; DNS either resolves or it does not, and there is no artefact to test for
# mutates: nothing — waits on a condition

# The public names, before anything asks a question that needs one.
#
# external-dns publishes them from the Gateway and HTTPRoute hostnames, and it
# arrives through Argo CD rather than through a step — so on a fresh cluster it
# is deployed some minutes after the claims phase begins, and every step that
# reaches the cluster from outside is racing it.
#
# Carried over from v4's D-03 with three changes, each because v5 differs:
#
#   external-dns runs in the edge namespace rather than one of its own, and is
#   found by label because the release name is not fixed.
#
#   The second hostname is console.<kernel>, not portal.<kernel>: the portal in
#   the edge namespace was retired in S7 and the desktop serves console.
#
#   It runs right after the operator and right before D-03-portal-login, the
#   first step to reach the cluster from outside. Not earlier: on a tunnel
#   cluster the kernel hostnames are published from a DNSEndpoint the
#   operator writes, and on a static-ip one external-dns reads them off the
#   kernel Gateway the operator reconciles -- so before D-01 nothing can have
#   published them. It sat at the end of the claims phase and, on the first
#   fresh install, waited out its fifteen minutes for a record that only the
#   next phase could create. Not later: a slow publish would surface in
#   D-03 as Keycloak being unreachable rather than as DNS not yet live --
#   the exact confusion this step was written about.
#
# Non-fatal by design. A cluster reached over a private DNS view, or one whose
# records an operator maintains by hand, is legitimate — this says so and moves
# on rather than refusing to continue.

_dns_wait_hosts() {
    # KERNEL_DOMAIN, which the installer resolves from the claim before any step
    # runs and every other step reads the same way.
    #
    # id only: the OIDC discovery document D-03 fetches. console.<kernel> is
    # the platform tenant's desktop, which D-03 creates -- nothing routes it
    # before then, so nothing can have published it, and waiting for it here
    # waited out the full timeout on the first fresh install. D-05 waits for
    # it, after D-03, with the same function.
    local d="${KERNEL_DOMAIN:-}"
    [[ -n "${d}" ]] || return 0
    printf '%s\n' "id.${d}"
}

apply() {
    gentian_dns_wait_for "$(_dns_wait_hosts)"
}
