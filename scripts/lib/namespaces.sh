#!/usr/bin/env bash
# The cluster's namespace layout, read from kernel/namespaces.yaml. Nothing
# else in the installer names a kernel namespace: a step asks for one by
# function, so the name lives in one file and the labels come with it.
#
#   ns_kernel <function>        → the namespace name
#   ns_kernel_all               → every kernel namespace name, in order
#   ns_labels <name>            → "gentianos.io/tier=… gentianos.io/function=…"
#   ns_ensure <name>            → create if absent, then apply the labels
#   ns_ensure_kernel            → all kernel namespaces, plus labels on the platform's own

NAMESPACES_FILE="${NAMESPACES_FILE:-${SCRIPT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}/kernel/namespaces.yaml}"

# _ns_table prints "<name> <tier> <function>" per line for the kernel and the
# labelled sections. A flat awk pass: the file is regular by construction and
# the installer must not depend on a YAML parser being present.
_ns_table() {
    awk '
        /^kernel:/   { section = "kernel";   next }
        /^labelled:/ { section = "labelled"; next }
        /^[a-z]/     { section = "";         next }
        section == "" { next }
        /^  - name:/     { name = $3; tier = (section == "kernel") ? "kernel" : ""; fn = ""; next }
        /^    tier:/     { tier = $2; next }
        /^    function:/ { fn = $2; print name, tier, fn; next }
    ' "${NAMESPACES_FILE}"
}

ns_kernel() {
    local fn="${1:?ns_kernel <function>}" name
    name="$(_ns_table | awk -v fn="${fn}" '$2 == "kernel" && $3 == fn { print $1 }')"
    [[ -n "${name}" ]] || { echo "no kernel namespace has function '${fn}' in ${NAMESPACES_FILE}" >&2; return 1; }
    echo "${name}"
}

ns_kernel_all() {
    _ns_table | awk '$2 == "kernel" && $1 ~ /^kernel-/ { print $1 }'
}

ns_labels() {
    local name="${1:?ns_labels <name>}"
    _ns_table | awk -v n="${name}" '$1 == n { printf "gentianos.io/tier=%s gentianos.io/function=%s\n", $2, $3 }'
}

ns_ensure() {
    local name="${1:?ns_ensure <name>}" labels
    labels="$(ns_labels "${name}")"
    [[ -n "${labels}" ]] || { echo "namespace '${name}' is not in ${NAMESPACES_FILE}" >&2; return 1; }
    if ! kubectl get namespace "${name}" >/dev/null 2>&1; then
        kubectl create namespace "${name}" >/dev/null
    fi
    # shellcheck disable=SC2086 # two key=value words, by construction
    kubectl label --overwrite namespace "${name}" ${labels} >/dev/null
}

ns_labelled_ok() {
    local name="${1:?}" tier fn
    read -r tier fn < <(_ns_table | awk -v n="${name}" '$1 == n { print $2, $3 }')
    [[ -n "${tier}" ]] || return 1
    [[ "$(kubectl get namespace "${name}" -o jsonpath='{.metadata.labels.gentianos\.io/tier}' 2>/dev/null)" == "${tier}" ]] &&
    [[ "$(kubectl get namespace "${name}" -o jsonpath='{.metadata.labels.gentianos\.io/function}' 2>/dev/null)" == "${fn}" ]]
}

ns_ensure_kernel() {
    local name
    for name in $(ns_kernel_all); do
        ns_ensure "${name}"
    done
    # The platform's own namespaces are labelled where they exist and left alone
    # where they do not: gentian-os does not create kube-system.
    for name in $(_ns_table | awk '$1 !~ /^kernel-/ { print $1 }'); do
        kubectl get namespace "${name}" >/dev/null 2>&1 && ns_ensure "${name}"
    done
    return 0
}
