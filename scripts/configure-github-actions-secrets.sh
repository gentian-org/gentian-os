#!/usr/bin/env bash
# Upload GitHub Actions secrets for gentian-os automation (optional ArgoCD sync).
#
# Usage (normally called from install.sh):
#   CI_BOT_PAT=... KERNEL_DOMAIN=platform.example.com \
#     ./scripts/configure-github-actions-secrets.sh
#
# Requires: gh CLI authenticated with permission to manage secrets on both repos.
set -euo pipefail

OS_REPO="${GITHUB_ACTIONS_OS_REPO:-gentian-org/gentian-os}"
UI_REPO="${GITHUB_ACTIONS_UI_REPO:-gentian-org/gentian-ui}"

if ! command -v gh >/dev/null 2>&1; then
    echo "gh CLI not found — skipping GitHub Actions secret upload." >&2
    echo "  Install gh and run: ./scripts/configure-github-actions-secrets.sh" >&2
    exit 0
fi

if ! gh auth status >/dev/null 2>&1; then
    echo "gh is not authenticated — skipping GitHub Actions secret upload." >&2
    echo "  Run: gh auth login" >&2
    exit 0
fi

if [[ -z "${CI_BOT_PAT:-}" ]]; then
    echo "CI_BOT_PAT not set — skipping GitHub Actions secret upload." >&2
    echo "  Add CI_BOT_PAT to install.secrets.env (Contents read/write on ${OS_REPO})." >&2
    exit 0
fi

echo "Configuring GitHub Actions secrets..."

gh secret set CI_BOT_PAT --repo "${OS_REPO}" --body "${CI_BOT_PAT}"
echo "  CI_BOT_PAT set on ${OS_REPO}."

gh secret set CI_BOT_PAT --repo "${UI_REPO}" --body "${CI_BOT_PAT}"
echo "  CI_BOT_PAT set on ${UI_REPO} (pass-through for workflow_call)."

argocd_server="${ARGOCD_SERVER:-}"
if [[ -z "${argocd_server}" && -n "${KERNEL_DOMAIN:-}" ]]; then
    argocd_server="https://argocd.${KERNEL_DOMAIN}"
fi
if [[ -n "${argocd_server}" ]]; then
    gh secret set ARGOCD_SERVER --repo "${OS_REPO}" --body "${argocd_server}"
    echo "  ARGOCD_SERVER set on ${OS_REPO} (${argocd_server})."
fi

if [[ -n "${ARGOCD_TOKEN:-}" ]]; then
    gh secret set ARGOCD_TOKEN --repo "${OS_REPO}" --body "${ARGOCD_TOKEN}"
    echo "  ARGOCD_TOKEN set on ${OS_REPO}."
else
    echo "  ARGOCD_TOKEN not set — pin workflows skip immediate ArgoCD sync (polling still works)."
fi

echo "GitHub Actions secrets configured."
