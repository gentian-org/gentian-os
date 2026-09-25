#!/usr/bin/env bash
# step: C-04-dns-wait
# phase: claims
# requires: C-03-wildcard-cert
# provides: the kernel hostnames resolving publicly
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
#   It runs at the END of the claims phase rather than inside applications,
#   because on v5 the first step to reach the cluster from outside is
#   D-02-portal-login, which bootstraps the kernel realm. A slow publish
#   surfaces there as Keycloak being unreachable rather than as DNS not yet
#   being live — which is the exact confusion this step was written about.
#
# Non-fatal by design. A cluster reached over a private DNS view, or one whose
# records an operator maintains by hand, is legitimate — this says so and moves
# on rather than refusing to continue.

_dns_wait_hosts() {
    # KERNEL_DOMAIN, which the installer resolves from the claim before any step
    # runs and every other step reads the same way.
    #
    # Two names, not every hostname the cluster serves: these are the ones the
    # steps after this actually reach over the public internet — id for the OIDC
    # discovery document, portal for the sign-in the handover waits on. Waiting
    # for more would make this fail for services nothing here depends on.
    local d="${KERNEL_DOMAIN:-}"
    [[ -n "${d}" ]] || return 0
    printf '%s\n' "id.${d}" "console.${d}"
}

apply() {
    local timeout="${GENTIAN_DNS_WAIT_SECS:-900}"
    local deadline=$(( SECONDS + timeout ))
    local host hosts pending reported=0

    hosts="$(_dns_wait_hosts)"
    if [[ -z "${hosts}" ]]; then
        info "No kernel domain resolved from the claim; nothing to wait for."
        return 0
    fi

    # external-dns first: until it runs, nothing is publishing anything, and
    # saying so is more useful than a name that will not resolve for reasons the
    # operator cannot see.
    if ! kubectl get deploy -n "$(ns_kernel edge)" -l app.kubernetes.io/name=external-dns \
        -o name 2>/dev/null | grep -q .; then
        info "Waiting for external-dns to be deployed (Argo CD delivers it)..."
    fi

    while (( SECONDS < deadline )); do
        pending=""
        while IFS= read -r host; do
            [[ -n "${host}" ]] || continue
            # The zone's own nameservers, not this machine's resolver: see
            # gentian_dns_resolves. A resolver that asked before the record
            # existed caches "no" for the zone's negative TTL, which is longer
            # than this wait and would time out on DNS that is already live.
            gentian_dns_resolves "${host}" "${KERNEL_DOMAIN:-}" \
                || pending="${pending}${host} "
        done <<< "${hosts}"

        if [[ -z "${pending}" ]]; then
            success "Kernel hostnames resolve: $(printf '%s' "${hosts}" | tr '\n' ' ')"
            return 0
        fi

        if (( reported == 0 )) || (( SECONDS % 60 < 10 )); then
            info "  waiting for DNS: ${pending%% }"
            reported=1
        fi
        sleep 10
    done

    # Not an error. The steps after this one wait for what they need in their own
    # right, and a cluster whose DNS an operator publishes by hand is a supported
    # arrangement rather than a fault.
    warn "Kernel hostnames did not resolve within $(( timeout / 60 ))m: ${pending%% }"
    warn "  external-dns publishes them from the Gateway's hostnames once it is"
    warn "  running, and its Cloudflare credential must cover this zone."
    warn "  Continuing — the steps that need a public name wait for it themselves."
    return 0
}
