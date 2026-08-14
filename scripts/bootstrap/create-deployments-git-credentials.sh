#!/usr/bin/env bash
# Create a git credentials Secret for the gentian-os operator lifecycle API.
#
# Usage:
#   ./create-deployments-git-credentials.sh <namespace> <token> [username] [host]
#
# The Secret is mounted by the operator so `git push` to gentian-deployments works
# from in-cluster App Store installs (GitOps app lifecycle).
set -euo pipefail

NAMESPACE="${1:-gentian-system}"
TOKEN="${2:-}"
USERNAME="${3:-x-access-token}"
HOST="${4:-github.com}"

if [[ -z "${TOKEN}" ]]; then
    echo "No deployments git token provided; skipping secret creation." >&2
    exit 0
fi

SECRET_NAME="gentian-deployments-git-credentials"
CRED_LINE="https://${USERNAME}:${TOKEN}@${HOST}"

echo "Creating deployments git credentials Secret in namespace: ${NAMESPACE}"

kubectl create secret generic "${SECRET_NAME}" \
    -n "${NAMESPACE}" \
    --from-literal=.git-credentials="${CRED_LINE}" \
    --dry-run=client -o yaml \
    | kubectl apply -f -

kubectl label secret "${SECRET_NAME}" -n "${NAMESPACE}" \
    app.kubernetes.io/managed-by=gentian-installer \
    --overwrite

echo "Deployments git credentials Secret ${SECRET_NAME} applied."
