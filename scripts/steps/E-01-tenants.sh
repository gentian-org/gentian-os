#!/usr/bin/env bash
# step: E-01-tenants
# phase: handover
# requires: D-09-app-catalogue
# provides: nothing at install time — tenants are created after installation
# mutates: Tenant and App CRs, on teardown only

# The one destroy-only step. Tenants are created by operators through
# `kubectl gentian` or the admin console, never by the installer, so apply() has
# nothing to do. It exists because teardown must remove them *first*, and the
# driver derives teardown order by reversing the step list — so the thing that
# must be destroyed first has to be the last step.

check() {
    # Never satisfied and never missing: there is no install-time artefact to
    # look for, because tenants arrive after the install. UNDEFINED keeps it
    # silent on the forward pass without claiming a fresh cluster has tenants.
    #
    # The driver still runs destroy() on an UNDEFINED step, which is what this
    # one exists for.
    return "${CHECK_UNDEFINED}"
}

apply() {
    return 0
}

destroy() {
    # The operator registers a ValidatingWebhookConfiguration intercepting PATCH
    # on Tenant CRs. With the operator already gone its webhook service is
    # unavailable and every patch fails with "service not found", so the webhook
    # has to go before finalizers can be stripped.
    if kubectl get validatingwebhookconfiguration gentian-os-tenant-validator >/dev/null 2>&1; then
        info "Removing gentian-os-tenant-validator webhook before tenant teardown..."
        kubectl delete validatingwebhookconfiguration gentian-os-tenant-validator \
            --ignore-not-found=true 2>/dev/null || true
    fi

    local tenant ns app deadline
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
    if kubectl get crd apps.gentianos.io >/dev/null 2>&1; then
        # `while read` rather than mapfile: macOS ships bash 3.2 (docs/plans §7).
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
