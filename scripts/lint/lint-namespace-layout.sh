#!/usr/bin/env bash
# The v5 layout names no namespace by hand: v5 steps, the v5 bootstrap chart
# and the Go layout package address kernel namespaces by function, and the
# old names must not survive in them. kernel/namespaces.yaml is the one list;
# internal/layout's test keeps its Go twin equal to it.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

status=0
fail() { echo "FAIL — $*" >&2; status=1; }

# Names of the v4 layout that v5 files may not contain.
old='\b(platform-kernel|gentian-system|crossplane-system|cnpg-system|stakater-system|envoy-gateway-system|external-secrets|argocd-image-updater|kyverno|cert-manager|external-dns|openbao)\b'
# … as a namespace. The words also name charts and releases, so only the
# namespace positions are checked: `namespace: <name>`, `-n <name>`, `--namespace <name>`.
while IFS= read -r hit; do
  fail "an old namespace name in a v5 file: ${hit}"
done < <(grep -rnE "(namespace:|[[:space:]]-n|--namespace)[[:space:]]+\"?(${old#\\b(}" \
           "${ROOT}/scripts/steps-v5" "${ROOT}/kernel/bootstrap-v5" "${ROOT}/kernel/data" 2>/dev/null \
         | grep -vE '^\S+:\s*#' | grep -vE 's/namespace: ' || true)  # a sed pattern replacing the old name is the point

# Every kernel namespace in the file is kernel-<function>, and the function
# matches the name.
while IFS= read -r line; do
  name="${line%% *}"; fn="${line##* }"
  [[ "${name}" == "kernel-${fn}" ]] || fail "kernel/namespaces.yaml: ${name} has function ${fn}; kernel namespaces are named kernel-<function>"
done < <(awk '/^kernel:/{s=1;next} /^[a-z]/{s=0} s && /- name:/{n=$3} s && /function:/{print n, $2}' "${ROOT}/kernel/namespaces.yaml")

# The v5 chart refuses to render without the layout.
if helm template x "${ROOT}/kernel/bootstrap-v5/chart" >/dev/null 2>&1; then
  fail "kernel/bootstrap-v5/chart renders without a namespace layout; every destination must come from it"
fi

[[ ${status} -eq 0 ]] && echo "OK — the v5 layout addresses namespaces by function only."
exit ${status}
