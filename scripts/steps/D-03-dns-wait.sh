#!/usr/bin/env bash
# step: D-03-dns-wait
# phase: applications
# requires: D-02-gateway-wait
# provides: the kernel hostnames resolving publicly
# check: none — a pure wait; DNS either resolves or it does not, and there is no artefact to test for
# mutates: nothing — waits on a condition

# The public names, before anything asks a question that needs one.
#
# external-dns publishes them from the Gateway and HTTPRoute hostnames, and it
# arrives through Argo CD rather than through a step — so on a fresh cluster it
# is deployed some minutes after this phase begins, and every step that reaches
# the cluster from outside is racing it.
#
# That race was invisible for a long time. Every install until now used a domain
# whose records had been configured by hand, so the names resolved before the
# installer ever ran. Changing the kernel domain exposed it: D-07's discovery
# check sat on a name that could not resolve, reported the document unreadable,
# and advised fixing spec.oidc.discoveryUrl — which was correct all along.
#
# One place that waits, named in the step graph, so the dependency is something
# validate-steps and --status can see rather than a polling loop buried in
# whichever consumer happened to be written first.
#
# Non-fatal by design, like D-02. A cluster reached over a private DNS view, or
# one whose records an operator maintains by hand, is legitimate — this says so
# and moves on rather than refusing to continue.

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
    printf '%s\n' "id.${d}" "portal.${d}"
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
    if ! kubectl get deploy -n external-dns -o name >/dev/null 2>&1; then
        info "Waiting for external-dns to be deployed (Argo CD delivers it)..."
    fi

    while (( SECONDS < deadline )); do
        pending=""
        while IFS= read -r host; do
            [[ -n "${host}" ]] || continue
            getent hosts "${host}" >/dev/null 2>&1 || pending="${pending}${host} "
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
