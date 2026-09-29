#!/bin/bash
# LiteLLM Teams reconciliation — one Team per Gentian Tenant CR.
# Sourced from scripts/lib/load.sh; called from E-02-litellm-reconcile.

# Removes the Deployment+Service for any vLLM instance that was previously
# applied but is no longer in the given desired-instances list — pass ""
# to remove every real vLLM instance (e.g. GPU_ACCELERATION flipped back
# to false; the mock backend's fixed-name Deployment doesn't collide with
# any of these, so nothing prunes them automatically otherwise). PVCs are
# deliberately left behind (orphaned, not deleted) — cached model weights
# can be tens of GB and take many minutes to redownload (see the HF_TOKEN
# rate-limit note in agentic-ai.md §10.2); re-adding the same instance ID
# later picks the cache back up instead of paying that cost again. Remove
# stale PVCs manually if you want the disk space back:


# Keycloak Admin API calls run in-cluster (Job) because litellm-proxy is a
# ClusterIP Service and is not reachable from the install host.
# ensure_litellm_teams was here. Per-tenant LiteLLM Teams are the
# TenantReconciler's now (internal/controller/litellm_team.go), for the same
# reason tenant realm SMTP moved there in e29db18e: per-tenant state converged
# by a script only converges when somebody re-runs the installer, so a tenant
# created afterwards had no Team until then. A controller reconciles it when the
# Tenant appears, and retries when LiteLLM is not up yet.

