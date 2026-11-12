#!/usr/bin/env bash
# =============================================================================
# scripts/lint/lint-chart-schemas.sh — every chart in the repository renders
# objects the Kubernetes API accepts
# =============================================================================
# Each chart is rendered with helm and the output validated by kubeconform in
# strict mode, against the schemas of the oldest Kubernetes minor the installer
# accepts (GENTIAN_MIN_KUBERNETES_MINOR in scripts/lib/common.sh). Strict mode
# refuses a field the schema does not declare and a key set twice in one
# document, which is what two documents rendered without a `---` between them
# look like: the API server would keep only the last of them.
#
# Kinds with no schema in kubeconform's catalog (CRDs: Argo CD, Crossplane,
# cert-manager, Gateway API, the platform's own) are skipped. The platform's
# CRDs are checked by internal/schemacheck and `make test-unit-schema`.
#
# Charts that refuse to render without the layout get it as the installer
# passes it (kernel/namespaces.yaml under `namespaces`, kernel/platforms.yaml),
# with placeholder values for the cluster's own settings. A subchart is
# rendered through its parent. Needs helm and kubeconform (make
# install-kubeconform) and fetches the schemas over the network.
# =============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${ROOT}"

KUBECONFORM="${KUBECONFORM:-kubeconform}"
command -v helm >/dev/null || { echo "lint-chart-schemas: helm is required" >&2; exit 1; }
command -v "${KUBECONFORM}" >/dev/null || { echo "lint-chart-schemas: ${KUBECONFORM} is required (make install-kubeconform)" >&2; exit 1; }

minor="$(sed -n 's/^GENTIAN_MIN_KUBERNETES_MINOR=\([0-9][0-9]*\)$/\1/p' scripts/lib/common.sh)"
[[ -n "${minor}" ]] || { echo "lint-chart-schemas: no GENTIAN_MIN_KUBERNETES_MINOR in scripts/lib/common.sh" >&2; exit 1; }

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT
{ echo "namespaces:"; sed 's/^/  /' kernel/namespaces.yaml; } > "${TMP}/namespaces.yaml"
{ echo "versions:"; sed 's/^/  /' versions.yaml; } > "${TMP}/versions.yaml"

# Values a chart requires and its defaults leave empty, by chart directory.
chart_args() {
    case "$1" in
        charts/infra/cache) echo "--set-string auth.existingSecret=placeholder" ;;
    esac
}

failed=0
count=0
# A Chart.yaml under another chart's charts/ directory is a subchart.
for chart in $(git ls-files '*Chart.yaml' | sed 's|/Chart.yaml$||' | grep -v '.\{1,\}/charts/' | sort); do
    count=$((count + 1))
    # shellcheck disable=SC2046  # chart_args yields separate words on purpose
    if ! helm template lint "${chart}" \
            -f "${TMP}/namespaces.yaml" -f "${TMP}/versions.yaml" -f kernel/platforms.yaml \
            --set-string kernelDomain=k.example --set-string cluster=c \
            $(chart_args "${chart}") > "${TMP}/rendered.yaml" 2> "${TMP}/helm.err"; then
        echo "FAIL  ${chart}: does not render"
        sed 's/^/      /' "${TMP}/helm.err"
        failed=$((failed + 1))
        continue
    fi
    if out="$("${KUBECONFORM}" -strict -ignore-missing-schemas -summary \
            -kubernetes-version "1.${minor}.0" "${TMP}/rendered.yaml" 2>&1)"; then
        echo "ok    ${chart}"
    else
        echo "FAIL  ${chart}"
        printf '%s\n' "${out}" | sed 's/^/      /'
        failed=$((failed + 1))
    fi
done

if [[ "${failed}" -gt 0 ]]; then
    echo "${failed} of ${count} charts render objects Kubernetes 1.${minor} would refuse"
    exit 1
fi
echo "all ${count} charts render valid objects (Kubernetes 1.${minor} schemas)"
