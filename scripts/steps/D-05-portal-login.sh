#!/usr/bin/env bash
# step: D-05-portal-login
# phase: applications
# requires: D-03-mail
# provides: portal OIDC login, Keycloak realm SMTP configuration
# mutates: Keycloak realm settings, portal Application

check() {
    kubectl get application gentian-portal -n argocd >/dev/null 2>&1 || return "${CHECK_MISSING}"

    # The portal cannot authorize without the OpenFGA store id, and it only has
    # one if the authz bridge had already published openfga-runtime during the
    # 180s this step waits. A bridge that was still failing then leaves a portal
    # that answers /readyz with 503 for good — apply() would fix it, but a check
    # testing only that the Application exists never lets apply() run again.
    #
    # Reported satisfied when the bridge has not published either: there is
    # nothing for this step to copy yet, and re-running it would only wait out
    # the same timeout.
    kubectl get secret openfga-runtime -n platform-kernel >/dev/null 2>&1 || return 0

    kubectl get secret gentian-portal-secrets -n platform-kernel \
        -o jsonpath='{.data.OPENFGA_STORE_ID}' 2>/dev/null | grep -q .
}

apply() {
    # shellcheck source=scripts/lib/portal-login-bootstrap.sh
    source "${SCRIPT_DIR}/scripts/lib/portal-login-bootstrap.sh"
    configure_keycloak_realm_smtp || warn "Keycloak realm SMTP configuration skipped."
    install_portal_login
}

destroy() {
    kubectl delete application gentian-portal -n argocd \
        --ignore-not-found=true 2>/dev/null || true
}
