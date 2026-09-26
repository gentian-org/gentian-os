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
           "${ROOT}/scripts/steps-v5" "${ROOT}/kernel/bootstrap-v5" "${ROOT}/kernel/appsets-v5" "${ROOT}/kernel/data" 2>/dev/null \
         | grep -vE '^\S+:\s*#' | grep -vE 's/\^?[[:space:]]*namespace: ' || true)
         # Two exclusions, both "the old name here is the point": a comment
         # explaining what moved, and a sed pattern REPLACING the old name with
         # the layout's. The second used to require the pattern to start
         # immediately at `namespace:`, so an anchored one with leading spaces
         # -- `s/^  namespace: gentian-system$/` in C-05 -- was reported as a
         # v4 name surviving in a v5 file, which is the opposite of what it is.

# Every kernel namespace in the file is kernel-<function>, and the function
# matches the name.
while IFS= read -r line; do
  name="${line%% *}"; fn="${line##* }"
  [[ "${name}" == "kernel-${fn}" ]] || fail "kernel/namespaces.yaml: ${name} has function ${fn}; kernel namespaces are named kernel-<function>"
done < <(awk '/^kernel:/{s=1;next} /^[a-z]/{s=0} s && /- name:/{n=$3} s && /function:/{print n, $2}' "${ROOT}/kernel/namespaces.yaml")

# Every system namespace in the file is system-<function>, and the composition
# composes exactly those.
#
# Two lists that can disagree is how a namespace ends up composed under a name
# nothing looks for: the operator addresses them through layout.System(fn), the
# policies select on the labels, and the Cluster composition is what creates
# them — so the file and the composition have to name the same set.
composition="${ROOT}/crossplane/compositions/cluster-default.yaml"
# The unconditional ones, which the composition ranges over always, and the
# ones a `when` marks, which it adds from the claim. A function's `when` sits on
# the line after it, so the two have to be read together.
layout_functions() {
  awk -v want="$1" '
    function emit() { if ((want == "always" && cond == "") || (want == "conditional" && cond != "")) print fn }
    /^system:/ { s = 1; next }
    /^[a-z]/   { s = 0 }
    !s         { next }
    /^  - name:/     { if (fn != "") emit(); fn = ""; cond = "" }
    /^    function:/ { fn = $2 }
    /^    when:/     { cond = $2 }
    END { if (fn != "") emit() }
  ' "${ROOT}/kernel/namespaces.yaml" | sort
}
declared=$(layout_functions always)
conditional=$(layout_functions conditional)
while IFS= read -r line; do
  name="${line%% *}"; fn="${line##* }"
  [[ "${name}" == "system-${fn}" ]] || fail "kernel/namespaces.yaml: ${name} has function ${fn}; system namespaces are named system-<function>"
done < <(awk '/^system:/{s=1;next} /^[a-z]/{s=0} s && /- name:/{n=$3} s && /function:/{print n, $2}' "${ROOT}/kernel/namespaces.yaml")

# The composition's list, from the one range that creates them.
# The composition's unconditional list, matched on the assignment rather
# than on the range: the range iterates a variable, and the variable is
# what the claim may extend.
# shellcheck disable=SC2016 # $systemFns is Go template text, not a shell variable
composed=$(grep -oE '\$systemFns := list [^}]*' "${composition}" | grep -oE '"[a-z0-9-]+"' | tr -d '"' | sort)
if [[ -z "${composed}" ]]; then
  fail "crossplane/compositions/cluster-default.yaml composes no system namespaces; kernel/namespaces.yaml says it should"
elif [[ "${declared}" != "${composed}" ]]; then
  fail "the system tier disagrees — kernel/namespaces.yaml has [$(echo "${declared}" | tr '\n' ' ')], the composition composes [$(echo "${composed}" | tr '\n' ' ')]"
fi

# A `when` entry has to appear in the composition too, somewhere: it is added
# under a condition rather than ranged over, so only its presence is checked. A
# namespace declared conditional and composed nowhere is one the claim can ask
# for and never get.
while IFS= read -r fn; do
  [[ -z "${fn}" ]] && continue
  grep -q "\"${fn}\"" "${composition}" \
    || fail "kernel/namespaces.yaml declares system-${fn} under a condition; the composition never composes it"
done <<< "${conditional}"

# The v5 chart refuses to render without the layout.
if helm template x "${ROOT}/kernel/bootstrap-v5/chart" >/dev/null 2>&1; then
  fail "kernel/bootstrap-v5/chart renders without a namespace layout; every destination must come from it"
fi

[[ ${status} -eq 0 ]] && echo "OK — the v5 layout addresses namespaces by function only."
exit ${status}
