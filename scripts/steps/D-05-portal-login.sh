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
        -o jsonpath='{.data.OPENFGA_STORE_ID}' 2>/dev/null | grep -q . || return "${CHECK_MISSING}"

    # Realm SMTP is half this step's `provides:`. In kernel mode its inputs are
    # derived, so the credential can always be produced and its absence means
    # the step did not finish. In external mode it depends on relay values that
    # may legitimately be unset, so it is not required here.
    if [[ "$(gentian_mail_service_mode)" == "kernel" ]]; then
        kubectl get secret keycloak-smtp-credentials -n platform-kernel >/dev/null 2>&1 ||
            return "${CHECK_MISSING}"
    fi
    return 0
}

apply() {
    # shellcheck source=scripts/lib/portal-login-bootstrap.sh
    source "${SCRIPT_DIR}/scripts/lib/portal-login-bootstrap.sh"
    # The portal bootstrap first, because it is what creates the kernel realm,
    # and configuring a realm's SMTP requires the realm.
    #
    # The other way round, the SMTP Job GET /admin/realms/kernel before anything
    # had created it, got a 404, and curl -sf exited 22 under set -e — printing
    # nothing at all, because -s suppresses the message. Three pods failed in
    # silence and the step reported only "Keycloak SMTP configure Job failed",
    # which named the Job rather than the reason. The realm appeared moments
    # later, created by the very call that used to come second.
    install_portal_login
    configure_keycloak_realm_smtp || warn "Keycloak realm SMTP configuration skipped."
}

destroy() {
    _delete_argocd_application gentian-portal
    # Created by configure_keycloak_realm_smtp in this step's apply().
    kubectl delete secret keycloak-smtp-credentials -n platform-kernel \
        --ignore-not-found=true 2>/dev/null || true
}
