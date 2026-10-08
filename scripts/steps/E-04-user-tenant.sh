#!/usr/bin/env bash
# step: E-04-user-tenant
# phase: handover
# requires: E-03-revoke-bootstrap-token
# provides: on a single-tenancy cluster, the user tenant Ready and its administrator's activation link issued; nothing on a multi-tenancy cluster
# mutates: an Argo CD sync request for Application tenant-user; in Keycloak, the user admin's required actions (through issue_admin_activation)

# The one tenant an install creates, and only under tenancyMode single.
#
# A single-tenancy cluster is the platform tenant plus exactly one user
# tenant, named "user", on the cluster's own addresses. Its manifest was
# scaffolded and committed, signed, in step 0 with the rest of the cluster's
# definition (scaffold_user_tenant). It has been in git ever since and the
# cluster has refused it ever since: the tenant webhook admits no tenant but
# the platform's until the platform admin has signed in once (the handover
# gate), and this manifest asks for no exception.
#
# So this step comes after the handover, and its whole job is what is left:
#
#   1. Ask Argo CD to try again. The tenants ApplicationSet retries a failed
#      sync ten times over about a quarter of an hour (kernel/appsets/raw/
#      12-tenants.yaml) and then stops, and automated sync does not start
#      again on a revision it already failed -- so by the time a person has
#      activated an account and signed in, the Application is usually parked
#      on the webhook's denial. request_argo_sync_if_stalled asks for a new
#      sync exactly then, and leaves a running or healthy one alone.
#   2. Wait for the operator's verdict, as D-03 waits for the platform
#      tenant's.
#   3. Hand the tenant's administrator account -- the user admin's -- over the
#      same way the platform admin's was: a single-use link, mailed when the
#      tenant's realm can send mail and shown here once when it cannot.
#
# It needs nothing the handover took away. E-03 revokes the installer's
# OpenBao token and deletes the init file; this step reads the cluster and
# asks Argo CD through the kubeconfig, and reaches Keycloak with the
# keycloak-admin Secret the same kubeconfig can read -- the credential
# ./install.sh --activate-admin uses on any cluster after its handover.
#
# No destroy(): the tenant is removed with every other by E-01's.

# _e04_handover_proven — has the platform admin signed in? The record E-03
# waits for, read from where the custodian writes it.
_e04_handover_proven() {
    [[ "$(kubectl get configmap gentian-handover -n "$(ns_kernel control)" \
        -o jsonpath='{.data.writePathProven}' 2>/dev/null || true)" == "true" ]]
}

_e04_tenant_phase() {
    kubectl get tenant "$1" -o jsonpath='{.status.phase}' 2>/dev/null || true
}

check() {
    # UNDEFINED on a multi-tenancy cluster, and on a single-tenancy one whose
    # definition holds no user tenant: there is then nothing the install
    # creates, and UNDEFINED keeps the step silent without claiming anything.
    local tenant
    tenant="$(gentian_user_tenant)"
    [[ -n "${tenant}" ]] || return "${CHECK_UNDEFINED}"
    # The operator says so in status.phase, as for the platform tenant.
    [[ "$(_e04_tenant_phase "${tenant}")" == "Ready" ]] || return "${CHECK_MISSING}"
    return 0
}

apply() {
    local tenant domain="${KERNEL_DOMAIN:-<kernel-domain>}"
    tenant="$(gentian_user_tenant)"
    [[ -n "${tenant}" ]] || return 0

    # Not before the handover. The cluster would refuse the tenant anyway;
    # saying so here is what keeps this from being fifteen minutes of waiting
    # for something that cannot happen yet. An unattended run ends here, and
    # the tenant is created by the first run after the sign-in.
    if ! _e04_handover_proven; then
        warn "The user tenant is not created yet: the platform admin has not signed in."
        warn "  This is a single-tenancy cluster, and its one tenant for users is admitted"
        warn "  only after that first sign-in at https://platform.${domain}/."
        warn "  Afterwards, finish with:  ./install.sh --only E-04"
        return 0
    fi

    # shellcheck source=scripts/lib/portal-login-bootstrap.sh
    source "${SCRIPT_DIR}/scripts/lib/portal-login-bootstrap.sh"

    local timeout="${GENTIAN_USER_TENANT_WAIT_SECS:-900}" gitops_ns deadline
    gitops_ns="$(ns_kernel gitops)"
    echo ""
    info "The handover is done. This is a single-tenancy cluster, so one thing is left:"
    info "  its user tenant, which the cluster held back until the platform admin signed in."
    info "Waiting for Tenant/${tenant} to be Ready (up to $(( timeout / 60 )) min)..."
    deadline=$((SECONDS + timeout))
    until [[ "$(_e04_tenant_phase "${tenant}")" == "Ready" ]]; do
        if (( SECONDS > deadline )); then
            # A warning, not a failure: the cluster is installed and handed
            # over, and what a tenant waits for can be something only a
            # signed-in administrator supplies -- a mail relay's credential,
            # say. check() goes on answering "not satisfied", so --status and
            # a later run keep naming this step until the tenant is Ready.
            echo ""
            warn "Tenant/${tenant} is not Ready after $(( timeout / 60 )) minutes. The install does not fail on it:"
            warn "  the cluster is installed and handed over. What the tenant is waiting for:"
            kubectl get tenant "${tenant}" -o jsonpath='{range .status.conditions[*]}    {.type}={.status} {.reason}: {.message}{"\n"}{end}' 2>/dev/null ||
                warn "    Tenant/${tenant} does not exist: is clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-<cluster>}/tenants/${tenant} pushed to gentian-deployments, and what does Argo CD say about Application tenant-${tenant}?"
            warn "  To check:   kubectl get tenant ${tenant}"
            warn "              kubectl get application tenant-${tenant} -n ${gitops_ns}"
            warn "  Once it is Ready, issue the user admin's activation link with either of:"
            warn "    ./install.sh --only E-04"
            warn "    kubectl gentian login && kubectl gentian tenants activate-admin ${tenant}"
            echo ""
            print_roles_summary
            return 0
        fi
        request_argo_sync_if_stalled "${gitops_ns}" "tenant-${tenant}" 2>/dev/null || true
        sleep 10
    done
    success "Tenant/${tenant} is Ready."

    echo ""
    warn "  THE USER ADMIN — in charge of the users and the user tenant"
    warn "  https://desktop.${domain}/"
    issue_tenant_admin_activation "${tenant}" ||
        warn "  No activation link could be issued for the user admin; later: kubectl gentian tenants activate-admin ${tenant}"
    info "  That account is the tenant's, in the tenant's own realm, and is another"
    info "  person's than the platform admin's unless you are both. Nothing here waits"
    info "  for it to be activated."
    echo ""
}
