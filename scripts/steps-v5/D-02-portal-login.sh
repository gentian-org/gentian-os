#!/usr/bin/env bash
# step: D-02-portal-login
# phase: applications
# requires: D-01-operator
# provides: the kernel realm with its clients and the administrator user, Argo CD signing in against it, and the portal Application serving the desktop
# mutates: Keycloak realm, clients, groups and users; Secrets in the edge and gitops namespaces; the argocd-cm, argocd-rbac-cm and argocd-tls-certs-cm ConfigMaps; the gentian-portal Application

# _v5_render is B-01's; a step file is a library of verbs and sourcing another
# one is how they are shared.
# shellcheck source=scripts/steps-v5/B-01-bootstrap-apps.sh
source "${SCRIPT_DIR}/scripts/steps-v5/B-01-bootstrap-apps.sh"

# The bootstrap that turns a running Keycloak into one this cluster can be
# signed in to: the kernel realm, the portal's public client, the confidential
# clients the portal backend and Argo CD authenticate with, the
# gentian:platform:superadmin group, and the administrator account itself --
# whose password is derived from the master password, like every other kernel
# credential.
#
# The work is portal-login-bootstrap.sh, which v4 runs too. What this step adds
# is where each piece lives: that library used to name one namespace for the
# portal, Keycloak, OpenFGA, the wildcard certificate and Argo CD alike, and
# under this layout those are five different answers.

_d02_export_layout() {
    export PORTAL_NAMESPACE IDENTITY_NAMESPACE AUTHZ_NAMESPACE GITOPS_NAMESPACE \
        EDGE_NAMESPACE GENTIAN_SYSTEM_NAMESPACE CROSSPLANE_NAMESPACE
    # The portal answers on the edge, beside the Gateway that serves it -- the
    # same namespace the operator's routes send portal traffic to.
    PORTAL_NAMESPACE="$(ns_kernel edge)"
    IDENTITY_NAMESPACE="$(ns_kernel authentication)"
    AUTHZ_NAMESPACE="$(ns_kernel authorization)"
    GITOPS_NAMESPACE="$(ns_kernel gitops)"
    EDGE_NAMESPACE="$(ns_kernel edge)"
    GENTIAN_SYSTEM_NAMESPACE="$(ns_kernel control)"
    CROSSPLANE_NAMESPACE="$(ns_kernel provisioning)"
}

check() {
    _d02_export_layout
    kubectl get application gentian-portal -n "${GITOPS_NAMESPACE}" >/dev/null 2>&1 || return "${CHECK_MISSING}"
    # The realm is the thing this step exists to create, and the Secret the
    # portal reads is only meaningful once the clients in that realm exist.
    kubectl get secret gentian-portal-secrets -n "${PORTAL_NAMESPACE}" >/dev/null 2>&1 || return "${CHECK_MISSING}"
    return 0
}

apply() {
    banner "Portal login"
    _d02_export_layout

    # shellcheck source=scripts/lib/portal-login-bootstrap.sh
    source "${SCRIPT_DIR}/scripts/lib/portal-login-bootstrap.sh"

    # That library renders the v4 bootstrap chart's Application, which names
    # one namespace for everything. This layout's Application comes from the
    # same chart every other kernel Application here comes from, so the
    # renderer is B-01's and the destination follows the layout.
    apply_gentian_portal_argocd_application() {
        V5_APPSETS=true V5_OPERATOR=true V5_PORTAL=true _v5_render | kubectl apply -f - >/dev/null
    }

    # The Application first would be the wrong order: it syncs a chart whose
    # pods read gentian-portal-secrets, and that Secret carries the client id
    # and secret this bootstrap creates. The realm also has to exist before
    # anything configures its SMTP.
    install_portal_login
    configure_keycloak_realm_smtp || warn "Keycloak realm SMTP configuration skipped."
}

destroy() {
    _d02_export_layout
    kubectl delete application gentian-portal -n "${GITOPS_NAMESPACE}" \
        --ignore-not-found --wait=false >/dev/null 2>&1 || true
    kubectl delete secret keycloak-smtp-credentials -n "${IDENTITY_NAMESPACE}" \
        --ignore-not-found=true >/dev/null 2>&1 || true
}
