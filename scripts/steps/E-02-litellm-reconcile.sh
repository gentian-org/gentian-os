#!/usr/bin/env bash
# step: E-02-litellm-reconcile
# phase: handover
# requires: E-01-tenants
# provides: LiteLLM's SSO client secret and model registrations, reconciled
# mutates: LiteLLM SSO secret, LiteLLM model registrations

# What is left here is the LiteLLM state that lives in LiteLLM's own database
# rather than in a manifest: the SSO client secret it authenticates the console
# with, and the model registrations that must match spec.llm.instances.
#
# Both are cluster-level, so this reads like a tail on D-04-llm-serving. It is a
# separate step because of ordering: D-04 runs in the applications phase and
# owns the vLLM release, while these two calls need LiteLLM itself to be serving
# — and LiteLLM arrives through Argo CD, which converges on its own schedule.
# The handover phase is the first point where waiting for it is reasonable.
#
# Per-tenant state used to be reconciled here and no longer is. Tenant realm
# SMTP moved to the TenantReconciler in e29db18e; LiteLLM Teams followed, for
# the same reason — a script converges per-tenant state only when an operator
# re-runs it, so a tenant created afterwards went without.

check() {
    # Deliberately always unsatisfied. Satisfaction would mean "the SSO secret
    # matches Keycloak and the registrations match the claim", which is
    # precisely what the reconcilers below determine — a check() would have to
    # duplicate them to answer. They are idempotent and cheap, so running them
    # is the check.
    return "${CHECK_ALWAYS}"
}

apply() {
    # portal-login-bootstrap.sh is not in scripts/lib/load.sh's list; step 30
    # sources it the same way.
    # shellcheck source=scripts/lib/portal-login-bootstrap.sh
    source "${SCRIPT_DIR}/scripts/lib/portal-login-bootstrap.sh"

    # Non-fatal throughout: a tenant whose realm is mid-provision must not abort
    # the run, and the next invocation picks it up.
    if [[ "${LLM_SUPPORT:-false}" != "true" ]]; then
        info "LLM_SUPPORT is not true; skipping LiteLLM tenant reconciliation."
        return 0
    fi

    ensure_litellm_sso_secret >/dev/null || warn "LiteLLM SSO secret reconciliation failed."
    ensure_litellm_vllm_model || warn "LiteLLM vLLM model sync failed."
}

# No destroy(): LiteLLM's database goes when the LiteLLM release goes, and that
# release is Argo CD's. There is nothing left for this step to reverse.
