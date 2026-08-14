#!/usr/bin/env bash
# step: E-02-tenant-reconcile
# phase: handover
# requires: E-01-tenants
# provides: per-tenant LiteLLM Teams and Keycloak realm SMTP, reconciled
# mutates: LiteLLM Team objects, per-tenant Keycloak realm SMTP settings

# Every other step converges *cluster* state: "does this cluster have X". This
# one converges *per-tenant* state — one LiteLLM Team per Tenant CR, SMTP on
# each tenant's Keycloak realm. Adding a tenant creates work here that no
# cluster-level check() would notice, which is why it is its own step rather
# than a tail on D-03-mail or D-04-llm-serving.
#
# On a fresh install there are no tenants yet and every call is a no-op, so its
# position after 33 costs nothing. It matters on re-runs, which is exactly when
# `install.sh --update` is the command an operator reaches for.

check() {
    # Deliberately always unsatisfied. Satisfaction would mean "every tenant
    # already has a Team and SMTP", which is precisely what the reconcilers
    # below determine — a check() would have to duplicate them to answer. They
    # are idempotent and cheap, so running them is the check.
    return 1
}

apply() {
    # portal-login-bootstrap.sh is not in scripts/lib/load.sh's list; step 30
    # sources it the same way.
    # shellcheck source=scripts/lib/portal-login-bootstrap.sh
    source "${SCRIPT_DIR}/scripts/lib/portal-login-bootstrap.sh"

    # Non-fatal throughout: a tenant whose realm is mid-provision must not abort
    # the run, and the next invocation picks it up.
    configure_tenant_realms_smtp || warn "Tenant realm SMTP reconciliation incomplete."

    if [[ "${LLM_SUPPORT:-false}" != "true" ]]; then
        info "LLM_SUPPORT is not true; skipping LiteLLM tenant reconciliation."
        return 0
    fi

    ensure_litellm_sso_secret >/dev/null || warn "LiteLLM SSO secret reconciliation failed."
    ensure_litellm_teams      || warn "LiteLLM team sync failed."
    ensure_litellm_vllm_model || warn "LiteLLM vLLM model sync failed."
}

# No destroy(): tenant teardown is E-01-tenants, which removes the Tenant CRs
# these objects hang off. There is nothing left for this step to reverse.
