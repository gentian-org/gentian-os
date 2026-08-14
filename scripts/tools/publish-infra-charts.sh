#!/usr/bin/env bash
# Regenerate charts/infra/packages/ (classic Helm repo) from vendored source charts.
# Crossplane provider-helm Release CRs and Argo CD ApplicationSets pull charts from this index.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PACKAGES="${ROOT}/charts/infra/packages"
BRANCH="${CHART_PACKAGES_BRANCH:-$(git -C "${ROOT}" rev-parse --abbrev-ref HEAD 2>/dev/null || echo develop)}"
REPO_URL="${CHART_PACKAGES_REPO_URL:-https://raw.githubusercontent.com/gentian-org/gentian-os/${BRANCH}/charts/infra/packages}"

mkdir -p "${PACKAGES}"

for chart in postgresql mariadb redis minio; do
  helm package "${ROOT}/charts/infra/${chart}" -d "${PACKAGES}"
done

helm repo index "${PACKAGES}" --url "${REPO_URL}"

echo "Packaged charts in ${PACKAGES} (index URL: ${REPO_URL}):"
ls -1 "${PACKAGES}"/*.tgz
