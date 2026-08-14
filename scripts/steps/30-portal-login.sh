#!/usr/bin/env bash
# step: 30-portal-login
# requires: 28-mail
# provides: portal OIDC login, Keycloak realm SMTP configuration
# mutates: Keycloak realm settings, portal Application

check() {
    kubectl get application gentian-portal -n argocd >/dev/null 2>&1
}

apply() {
    # shellcheck source=scripts/portal-login-bootstrap.sh
    source "${SCRIPT_DIR}/scripts/portal-login-bootstrap.sh"
    configure_keycloak_realm_smtp || warn "Keycloak realm SMTP configuration skipped."
    install_portal_login
}

destroy() {
    kubectl delete application gentian-portal -n argocd \
        --ignore-not-found=true 2>/dev/null || true
}
