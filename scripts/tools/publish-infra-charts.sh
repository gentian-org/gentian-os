#!/usr/bin/env bash
# Regenerate charts/infra/packages/ (classic Helm repo) from vendored source charts.
# The Cluster composition's provider-helm Releases pull their charts from this index.
# The index's chart URLs are relative, so it resolves against whatever ref or
# mirror serves it; CHART_PACKAGES_REPO_URL writes absolute ones instead.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PACKAGES="${ROOT}/charts/infra/packages"
REPO_URL="${CHART_PACKAGES_REPO_URL:-}"

mkdir -p "${PACKAGES}"

for chart in mariadb redis seaweedfs; do
  helm package "${ROOT}/charts/infra/${chart}" -d "${PACKAGES}"
done

# OpenFGA's chart is vendored beside its Argo CD manifests (see its
# UPSTREAM.md); cluster-default installs the same patched chart from this repo.
helm package "${ROOT}/kernel/services/openfga/manifests/charts/openfga" -d "${PACKAGES}"

if [[ -n "${REPO_URL}" ]]; then
  helm repo index "${PACKAGES}" --url "${REPO_URL}"
else
  helm repo index "${PACKAGES}"
fi

echo "Packaged charts in ${PACKAGES} (index URL: ${REPO_URL:-relative}):"
ls -1 "${PACKAGES}"/*.tgz
