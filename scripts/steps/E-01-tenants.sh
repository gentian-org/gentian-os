#!/usr/bin/env bash
# step: E-01-tenants
# phase: handover
# requires: D-07-app-catalogue
# provides: nothing at install time — tenants are created after installation
# mutates: Tenant and App CRs, on teardown only

# The one destroy-only step. Tenants are created by operators through
# `kubectl gentian` or the admin console, never by the installer, so apply() has
# nothing to do. It exists because teardown must remove them *first*, and the
# driver derives teardown order by reversing the step list — so the thing that
# must be destroyed first has to be the last step.

check() {
    # Always satisfied: there is no install-time artefact to create. Reporting
    # satisfied keeps it silent on the forward pass.
    return 0
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
    # `while read` rather than mapfile: macOS ships bash 3.2 (docs/plans §7).
    kubectl get tenant --no-headers -o custom-columns='NAME:.metadata.name' 2>/dev/null |
        grep -v '^$' | while IFS= read -r tenant; do
            info "Deleting App CRs for tenant ${tenant}..."
            kubectl delete app --all -n "${tenant}" --ignore-not-found=true 2>/dev/null || true
        done

    # Any App CR left in another namespace, e.g. from a partially-removed tenant.
    kubectl get app -A --no-headers 2>/dev/null |
        while read -r ns app _; do
            [[ -n "$ns" && -n "$app" ]] || continue
            kubectl delete app "$app" -n "$ns" --ignore-not-found=true 2>/dev/null || true
        done

    kubectl get tenant --no-headers -o custom-columns='NAME:.metadata.name' 2>/dev/null |
        grep -v '^$' | while IFS= read -r tenant; do
            gentian_run kubectl delete tenant "$tenant" --ignore-not-found=true || true
            # Wait for the operator to clear its finalizer, then force it. A
            # stuck finalizer here blocks every namespace deletion downstream.
            deadline=$((SECONDS + 60))
            while kubectl get tenant "$tenant" >/dev/null 2>&1; do
                if (( SECONDS >= deadline )); then
                    warn "Tenant ${tenant} finalizer did not clear; stripping it."
                    kubectl patch tenant "$tenant" --type=merge \
                        -p '{"metadata":{"finalizers":null}}' 2>/dev/null || true
                    break
                fi
                sleep 2
            done
        done
}
