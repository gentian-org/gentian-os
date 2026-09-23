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
# gentian:platform:admin group, and the administrator account itself --
# whose password is derived from the master password, like every other kernel
# credential.
#
# The work is portal-login-bootstrap.sh, which v4 runs too. What this step adds
# is where each piece lives: that library used to name one namespace for the
# portal, Keycloak, OpenFGA, the wildcard certificate and Argo CD alike, and
# under this layout those are five different answers.

_d02_export_layout() {
    export PORTAL_NAMESPACE IDENTITY_NAMESPACE AUTHZ_NAMESPACE GITOPS_NAMESPACE \
        EDGE_NAMESPACE GENTIAN_SYSTEM_NAMESPACE CROSSPLANE_NAMESPACE OBSERVABILITY_NAMESPACE
    # The portal answers on the edge, beside the Gateway that serves it -- the
    # same namespace the operator's routes send portal traffic to.
    PORTAL_NAMESPACE="$(ns_kernel edge)"
    IDENTITY_NAMESPACE="$(ns_kernel authentication)"
    AUTHZ_NAMESPACE="$(ns_kernel authorization)"
    GITOPS_NAMESPACE="$(ns_kernel gitops)"
    EDGE_NAMESPACE="$(ns_kernel edge)"
    GENTIAN_SYSTEM_NAMESPACE="$(ns_kernel control)"
    CROSSPLANE_NAMESPACE="$(ns_kernel provisioning)"
    OBSERVABILITY_NAMESPACE="$(ns_kernel observability)"
}

check() {
    _d02_export_layout
    kubectl get application gentian-portal -n "${GITOPS_NAMESPACE}" >/dev/null 2>&1 || return "${CHECK_MISSING}"
    # The realm is the thing this step exists to create, and the Secret the
    # portal reads is only meaningful once the clients in that realm exist.
    kubectl get secret gentian-portal-secrets -n "${PORTAL_NAMESPACE}" >/dev/null 2>&1 || return "${CHECK_MISSING}"
    # The platform is a tenant whose realm is this one (AD-10). It is
    # scaffolded with the cluster and synced by the tenants ApplicationSet,
    # and it is Ready only once the operator has adopted the realm and its
    # groups exist in it -- which is what this step is for.
    [[ "$(kubectl get tenant platform -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" == "True" ]] || return "${CHECK_MISSING}"
    # The kernel zone at the edge: the zone client's secret, which this step
    # writes, and the session policy the operator puts on the kernel UIs once
    # it exists. Without the first there is no session; without the second the
    # kernel UIs have no route (never an open one).
    kubectl get secret edge-kernel-oidc -n "${EDGE_NAMESPACE}" >/dev/null 2>&1 || return "${CHECK_MISSING}"
    [[ "$(kubectl get securitypolicy sp-kernel-argocd -n "${EDGE_NAMESPACE}" -o jsonpath='{.status.ancestors[0].conditions[?(@.type=="Accepted")].status}' 2>/dev/null)" == "True" ]] || return "${CHECK_MISSING}"
    # The platform's desktop: a component of the platform tenant, the console
    # at console.<kernel> behind the kernel session. Ready means its chart is
    # deployed and its routes exist.
    [[ "$(kubectl get component desktop -n tenant-platform -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" == "True" ]] || return "${CHECK_MISSING}"
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
    #
    # The same render turns Headlamp's OIDC on: the realm, the headlamp client
    # and its Secret exist by the time this runs, and the proxy that verifies
    # the person's token comes with it. Before this step there is no realm to
    # sign in against, which is why a fresh cluster starts on token login.
    apply_gentian_portal_argocd_application() {
        V5_APPSETS=true V5_OPERATOR=true V5_PORTAL=true V5_HEADLAMP_OIDC=true \
            _v5_render | kubectl apply -f - >/dev/null
    }

    # The Application first would be the wrong order: it syncs a chart whose
    # pods read gentian-portal-secrets, and that Secret carries the client id
    # and secret this bootstrap creates. The realm also has to exist before
    # anything configures its SMTP.
    install_portal_login
    configure_keycloak_realm_smtp || warn "Keycloak realm SMTP configuration skipped."

    # Tenant/platform arrives from git through the tenants ApplicationSet and
    # is provisioned by the operator against the realm just created. Nothing
    # here applies it; what is waited for is the operator's verdict.
    info "Waiting for Tenant/platform to be Ready..."
    local deadline=$((SECONDS + 900))
    until [[ "$(kubectl get tenant platform -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" == "True" ]]; do
        if (( SECONDS > deadline )); then
            error "Tenant/platform is not Ready after 15 minutes."
            kubectl get tenant platform -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}' 2>/dev/null || \
                error "  Tenant/platform does not exist: is clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID}/tenants/platform pushed to gentian-deployments?"
            return 1
        fi
        request_argo_sync_if_stalled "${GITOPS_NAMESPACE}" "tenant-platform" 2>/dev/null || true
        sleep 10
    done
    success "Tenant/platform is Ready: the platform tenant adopts realm ${KERNEL_REALM:-kernel}."

    # The zone's session on the kernel UIs. The operator writes the policies
    # once the zone secret exists; Envoy Gateway accepts them once the shim's
    # Service and the secret resolve.
    info "Waiting for the kernel zone's session policy on argocd.${KERNEL_DOMAIN}..."
    deadline=$((SECONDS + 600))
    until [[ "$(kubectl get securitypolicy sp-kernel-argocd -n "${EDGE_NAMESPACE}" -o jsonpath='{.status.ancestors[0].conditions[?(@.type=="Accepted")].status}' 2>/dev/null)" == "True" ]]; do
        if (( SECONDS > deadline )); then
            error "SecurityPolicy sp-kernel-argocd is not Accepted after 10 minutes."
            kubectl get securitypolicy sp-kernel-argocd -n "${EDGE_NAMESPACE}" -o jsonpath='{range .status.ancestors[0].conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}' 2>/dev/null || \
                error "  The policy does not exist: is the operator running, and does ${EDGE_NAMESPACE}/edge-kernel-oidc exist?"
            return 1
        fi
        sleep 10
    done
    success "The kernel UIs sit behind the kernel zone's session and the ext-auth shim."

    # The platform desktop: the component reconciler installs it from the
    # desktop profile once the zone exists and the database credential is
    # delivered; its chart comes from the registry the profile names.
    info "Waiting for the platform desktop (Component tenant-platform/desktop)..."
    deadline=$((SECONDS + 900))
    until [[ "$(kubectl get component desktop -n tenant-platform -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" == "True" ]]; do
        if (( SECONDS > deadline )); then
            error "The platform desktop is not Ready after 15 minutes."
            kubectl get component desktop -n tenant-platform -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}' 2>/dev/null || \
                error "  Component tenant-platform/desktop does not exist: is the operator running and the desktop profile installed?"
            return 1
        fi
        sleep 10
    done
    success "The platform desktop serves console.${KERNEL_DOMAIN} behind the kernel zone's session."
}

destroy() {
    _d02_export_layout
    kubectl delete application gentian-portal -n "${GITOPS_NAMESPACE}" \
        --ignore-not-found --wait=false >/dev/null 2>&1 || true
    kubectl delete secret keycloak-smtp-credentials -n "${IDENTITY_NAMESPACE}" \
        --ignore-not-found=true >/dev/null 2>&1 || true
}
