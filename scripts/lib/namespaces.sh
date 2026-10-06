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
# The awk program uses awk's own field variables; it is kept in a shell
# variable so that no positional-looking reference appears inside a function
# body, which the arity lint would otherwise read as a shell parameter.
# shellcheck disable=SC2016 # awk's $NF, deliberately not expanded by the shell
_NS_TABLE_AWK='
    /^kernel:/   { section = "kernel";   next }
    /^labelled:/ { section = "labelled"; next }
    /^[a-z]/     { section = "";         next }
    section == "" { next }
    /^  - name:/     { name = $NF; tier = (section == "kernel") ? "kernel" : ""; fn = ""; next }
    /^    tier:/     { tier = $NF; next }
    /^    function:/ { fn = $NF; print name, tier, fn; next }
'

_ns_table() {
    awk "${_NS_TABLE_AWK}" "${NAMESPACES_FILE}"
}

ns_kernel() {
    local fn="${1:?ns_kernel <function>}" name
    name="$(_ns_table | awk -v fn="${fn}" '$2 == "kernel" && $NF == fn { print $1 }')"
    [[ -n "${name}" ]] || { echo "no kernel namespace has function '${fn}' in ${NAMESPACES_FILE}" >&2; return 1; }
    echo "${name}"
}

# ns_system <function> — the system namespace with that function, from the
# same table. System namespaces are composed from the Cluster claim, so the
# name existing here does not mean the namespace exists on the cluster.
ns_system() {
    local fn="${1:?ns_system <function>}" name
    # Its own pass over the system section rather than a third section in
    # _ns_table, whose other readers (ns_ensure_kernel among them) would then
    # start labelling namespaces Crossplane owns.
    name="$(awk -v fn="${fn}" '
        /^system:/  { in_sys = 1; next }
        /^[a-z]/    { in_sys = 0; next }
        in_sys && /^  - name:/     { name = $NF; next }
        in_sys && /^    function:/ { if ($NF == fn) print name }
    ' "${NAMESPACES_FILE}")"
    [[ -n "${name}" ]] || { echo "no system namespace has function '${fn}' in ${NAMESPACES_FILE}" >&2; return 1; }
    echo "${name}"
}

ns_kernel_all() {
    _ns_table | awk '$2 == "kernel" && $1 ~ /^kernel-/ { print $1 }'
}

ns_labels() {
    local name="${1:?ns_labels <name>}"
    _ns_table | awk -v n="${name}" '$1 == n { printf "gentianos.io/tier=%s gentianos.io/function=%s\n", $2, $NF }'
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
    read -r tier fn < <(_ns_table | awk -v n="${name}" '$1 == n { print $2, $NF }')
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
        kubectl get namespace "${name}" >/dev/null 2>&1 || continue
        if ns_declined "${name}"; then
            # Declined after having been labelled: the labels come off, or the
            # claim would say one thing and the cluster go on doing the other.
            kubectl label namespace "${name}" gentianos.io/tier- gentianos.io/function- >/dev/null
            continue
        fi
        ns_ensure "${name}"
    done
    return 0
}

# ns_declined <name> -- whether the claim declines to label one of the
# platform's own namespaces.
#
# The kernel tier is what the baseline policies leave alone, so labelling a
# namespace into it is an exemption. The load balancer's is the one a cluster
# may refuse: MetalLB needs the host's network and capabilities the baseline
# forbids, and it is allowed them unless the claim sets platformParams.metallb
# to "false". The namespace is found by its function, never by its name.
ns_declined() {
    local name="${1:?ns_declined <name>}" fn
    fn="$(_ns_table | awk -v n="${name}" '$1 == n { print $NF }')"
    [[ "${fn}" == "load-balancer" && "${METALLB_ALLOWED:-true}" == "false" ]]
}
