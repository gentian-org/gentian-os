#!/usr/bin/env bash
# Regenerate charts/infra/packages/ (classic Helm repo) from vendored source charts.
# Crossplane provider-helm Release CRs pull postgresql/mariadb from this index.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PACKAGES="${ROOT}/charts/infra/packages"
REPO_URL="https://raw.githubusercontent.com/gentian-org/gentian-os/develop/charts/infra/packages"

mkdir -p "${PACKAGES}"

for chart in postgresql mariadb; do
  helm package "${ROOT}/charts/infra/${chart}" -d "${PACKAGES}"
done

helm repo index "${PACKAGES}" --url "${REPO_URL}"

echo "Packaged charts in ${PACKAGES}:"
ls -1 "${PACKAGES}"/*.tgz
