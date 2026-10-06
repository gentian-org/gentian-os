#!/usr/bin/env bash
# step: E-01-tenants
# phase: handover
# requires: D-03-portal-login
# provides: the first tenant Ready, when the install was told one; nothing otherwise — every later tenant is created after installation
# mutates: on the forward pass nothing but an Argo CD sync request for the first tenant's Application; Tenant, Component and App CRs on teardown

# Two jobs that share a place in the order.
#
# Forward: tenants are created by operators through the console or
# `kubectl gentian`, never by the installer -- with one exception. A cluster's
# users live in a tenant of their own, and an install told the name of the
# first one (GENTIAN_FIRST_TENANT) scaffolded and committed its manifest in
# step 0, beside the platform tenant's. It arrives through the tenants
# ApplicationSet like any tenant; this step waits for the operator's verdict
# on it, as D-03 waits for the platform tenant's, so the handover that follows
# can hand its administrator account over. With no first tenant there is
# nothing to wait for and the step is silent.
#
# Reverse: teardown must remove tenants FIRST, and the driver derives teardown
# order by reversing the step list -- so the thing that must be destroyed
# first has to be the last step. That is the job carried over from v4.
#
# Without it a plain `--uninstall` leaves Tenant, Component and App CRs behind
# with finalizers, and the reverse pass removes the operator that was the only
# thing able to clear them. `--purge` covers most of this through its own
# sweeps; an uninstall has none.
#
# v5 adds Components to what v4 removed. A Component carries
# gentianos.io/component-cleanup, and a Component whose finalizer cannot clear
# blocks its tenant namespace exactly as an App does — which is not theoretical:
# two of them deadlocked this cluster on 2026-09-25, terminating and
# un-finalizable, when a required field was renamed under them.

check() {
    # UNDEFINED when the install wrote no first tenant: there is then no
    # install-time artefact to look for, because tenants arrive after the
    # install, and UNDEFINED keeps the step silent on the forward pass without
    # claiming a fresh cluster has tenants. The driver still runs destroy() on
    # an UNDEFINED step, which is what the reverse pass needs.
    local tenant
    tenant="$(gentian_first_tenant)"
    [[ -n "${tenant}" ]] || return "${CHECK_UNDEFINED}"
    # The operator says so in status.phase, as for the platform tenant.
    [[ "$(kubectl get tenant "${tenant}" -o jsonpath='{.status.phase}' 2>/dev/null)" == "Ready" ]] || return "${CHECK_MISSING}"
    return 0
}

apply() {
    local tenant
    tenant="$(gentian_first_tenant)"
    [[ -n "${tenant}" ]] || return 0

    # Nothing here applies the tenant: it is in git, and the tenants
    # ApplicationSet brings it. What is waited for is the operator's verdict.
    local timeout="${GENTIAN_FIRST_TENANT_WAIT_SECS:-900}" gitops_ns deadline
    gitops_ns="$(ns_kernel gitops)"
    info "Waiting for Tenant/${tenant}, this cluster's first tenant, to be Ready (up to $(( timeout / 60 )) min)..."
    deadline=$((SECONDS + timeout))
    until [[ "$(kubectl get tenant "${tenant}" -o jsonpath='{.status.phase}' 2>/dev/null)" == "Ready" ]]; do
        if (( SECONDS > deadline )); then
            # Not an error, and deliberately: what a tenant waits for can be
            # something only a signed-in administrator supplies -- a mail
            # relay's credential, say -- and the sign-in is the handover this
            # step stands in front of. Stopping here could then never clear.
            # check() goes on answering "not satisfied", so --status and the
            # closing summary keep naming this step until the tenant is Ready.
            warn "Tenant/${tenant} is not Ready after $(( timeout / 60 )) minutes. The install goes on to the handover."
            kubectl get tenant "${tenant}" -o jsonpath='{range .status.conditions[*]}    {.type}={.status} {.reason}: {.message}{"\n"}{end}' 2>/dev/null || \
                warn "  Tenant/${tenant} does not exist: is clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-<cluster>}/tenants/${tenant} pushed to gentian-deployments, and has Argo CD synced Application tenant-${tenant}?"
            warn "  Once it is Ready, hand its administrator account over with:"
            warn "    kubectl gentian tenants activate-admin ${tenant}"
            return 0
        fi
        request_argo_sync_if_stalled "${gitops_ns}" "tenant-${tenant}" 2>/dev/null || true
        sleep 10
    done
    success "Tenant/${tenant} is Ready: this cluster's users sign in at https://console.${tenant}.${KERNEL_DOMAIN:-<kernel-domain>}/"
}

destroy() {
    local tenant ns app comp deadline
    # The operator registers a ValidatingWebhookConfiguration intercepting PATCH
    # on Tenant CRs. With the operator already gone its webhook service is
    # unavailable and every patch fails with "service not found", so the webhook
    # has to go before finalizers can be stripped.
    if kubectl get validatingwebhookconfiguration gentian-os-tenant-validator >/dev/null 2>&1; then
        info "Removing gentian-os-tenant-validator webhook before tenant teardown..."
        kubectl delete validatingwebhookconfiguration gentian-os-tenant-validator \
            --ignore-not-found=true 2>/dev/null || true
    fi

    # apps.gentianos.io in full, never the `app` shortname.
    #
    # Argo CD's Application registers shortNames `app` AND `apps`. While both
    # CRDs are installed kubectl resolves the name to Gentian's App, but a second
    # uninstall runs with apps.gentianos.io already gone — D-01 removed it — and
    # then `kubectl delete app` means applications.argoproj.io. This step starts
    # deleting A-09's Argo Applications, and blocks on a finalizer no controller
    # is left to clear.
    #
    # Guarded on the CRD as well as fully qualified: with the CRD absent every
    # call below is an error, and the loops would run for nothing.
    # Components first: a Component owns the Helm release behind an app, so
    # removing it before the App CRs gives provider-helm a chance to uninstall
    # cleanly rather than having the release orphaned under it.
    if kubectl get crd components.gentianos.io >/dev/null 2>&1; then
        kubectl get components.gentianos.io -A --no-headers 2>/dev/null |
            while read -r ns comp _; do
                [[ -n "$ns" && -n "$comp" ]] || continue
                kubectl delete components.gentianos.io "$comp" -n "$ns" \
                    --ignore-not-found=true --wait=false 2>/dev/null || true
            done
        deadline=$(( SECONDS + 60 ))
        while kubectl get components.gentianos.io -A --no-headers 2>/dev/null | grep -q .; do
            if (( SECONDS >= deadline )); then
                warn "Component finalizers did not clear; stripping them."
                kubectl get components.gentianos.io -A --no-headers 2>/dev/null |
                    while read -r ns comp _; do
                        [[ -n "$ns" && -n "$comp" ]] || continue
                        kubectl patch components.gentianos.io "$comp" -n "$ns" --type=merge \
                            -p '{"metadata":{"finalizers":null}}' 2>/dev/null || true
                    done
                break
            fi
            sleep 2
        done
    fi

    if kubectl get crd apps.gentianos.io >/dev/null 2>&1; then
        # `while read` rather than mapfile: macOS ships bash 3.2, which has neither.
        kubectl get tenants.gentianos.io --no-headers -o custom-columns='NAME:.metadata.name' 2>/dev/null |
            grep -v '^$' | while IFS= read -r tenant; do
                info "Deleting App CRs for tenant ${tenant}..."
                kubectl delete apps.gentianos.io --all -n "${tenant}" \
                    --ignore-not-found=true --wait=false 2>/dev/null || true
            done

        # Any App CR left in another namespace, e.g. from a partially-removed tenant.
        kubectl get apps.gentianos.io -A --no-headers 2>/dev/null |
            while read -r ns app _; do
                [[ -n "$ns" && -n "$app" ]] || continue
                kubectl delete apps.gentianos.io "$app" -n "$ns" \
                    --ignore-not-found=true --wait=false 2>/dev/null || true
            done

        # --wait=false above, so clear whatever the operator did not finalize.
        # An App holding a finalizer blocks its tenant namespace, which blocks
        # every namespace delete downstream of this step.
        deadline=$(( SECONDS + 60 ))
        while kubectl get apps.gentianos.io -A --no-headers 2>/dev/null | grep -q .; do
            if (( SECONDS >= deadline )); then
                warn "App CR finalizers did not clear; stripping them."
                kubectl get apps.gentianos.io -A --no-headers 2>/dev/null |
                    while read -r ns app _; do
                        [[ -n "$ns" && -n "$app" ]] || continue
                        kubectl patch apps.gentianos.io "$app" -n "$ns" --type=merge \
                            -p '{"metadata":{"finalizers":null}}' 2>/dev/null || true
                    done
                break
            fi
            sleep 2
        done
    fi

    kubectl get tenants.gentianos.io --no-headers -o custom-columns='NAME:.metadata.name' 2>/dev/null |
        grep -v '^$' | while IFS= read -r tenant; do
            # --wait=false: the wait below is the one that matters. Letting
            # kubectl block here means blocking on the operator clearing a
            # finalizer, and if the operator is already gone the loop written to
            # force exactly that never gets to run.
            gentian_run kubectl delete tenants.gentianos.io "$tenant" \
                --ignore-not-found=true --wait=false || true
            # Wait for the operator to clear its finalizer, then force it. A
            # stuck finalizer here blocks every namespace deletion downstream.
            deadline=$((SECONDS + 60))
            while kubectl get tenants.gentianos.io "$tenant" >/dev/null 2>&1; do
                if (( SECONDS >= deadline )); then
                    warn "Tenant ${tenant} finalizer did not clear; stripping it."
                    kubectl patch tenants.gentianos.io "$tenant" --type=merge \
                        -p '{"metadata":{"finalizers":null}}' 2>/dev/null || true
                    break
                fi
                sleep 2
            done
        done
}
