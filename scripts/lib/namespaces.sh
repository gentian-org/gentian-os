#!/usr/bin/env bash
# The cluster's namespace layout, read from kernel/namespaces.yaml. Nothing
# else in the installer names a kernel namespace: a step asks for one by
# function, so the name lives in one file and the labels come with it.
#
#   ns_kernel <function>        → the namespace name
#   ns_kernel_all               → every kernel namespace name, in order
#   ns_system_all               → every system namespace name, in order
#   ns_labels <name>            → "gentianos.io/tier=… gentianos.io/function=…"
#   ns_ensure <name>            → create if absent, then apply the labels
#   ns_ensure_kernel            → all kernel namespaces, plus labels on the platform's own
#   ns_kernel_policies_sync     → the kernel namespaces' NetworkPolicies, or none (KERNEL_NETWORK_POLICIES)
#   ns_kernel_policies_ok       → whether the cluster carries exactly those

NAMESPACES_FILE="${NAMESPACES_FILE:-${SCRIPT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}/kernel/namespaces.yaml}"

# _ns_table prints "<name> <tier> <function>" per line for the kernel and the
# labeled sections. A flat awk pass: the file is regular by construction and
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
    # start labeling namespaces Crossplane owns.
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

# The system namespaces the layout names, whether or not this cluster's claim
# composes them.
ns_system_all() {
    awk '
        /^system:/  { in_sys = 1; next }
        /^[a-z]/    { in_sys = 0; next }
        in_sys && /^  - name:/ { print $NF }
    ' "${NAMESPACES_FILE}"
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
    # The platform's own namespaces are labeled where they exist and left alone
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
# The kernel tier is what the baseline policies leave alone, so labeling a
# namespace into it is an exemption. The load balancer's is the one a cluster
# may refuse: MetalLB needs the host's network and capabilities the baseline
# forbids, and it is allowed them unless the claim sets platformParams.metallb
# to "false". The namespace is found by its function, never by its name.
ns_declined() {
    local name="${1:?ns_declined <name>}" fn
    fn="$(_ns_table | awk -v n="${name}" '$1 == n { print $NF }')"
    [[ "${fn}" == "load-balancer" && "${METALLB_ALLOWED:-true}" == "false" ]]
}

# -----------------------------------------------------------------------------
# Who may reach a pod of a kernel namespace.
#
# One file of NetworkPolicies, generated from the inventory of who calls what
# (internal/kernel/kernelnet/inventory.yaml). It is applied with the
# namespaces, before a pod exists in any of them, so no rule ever arrives
# under a running workload on a fresh install and nothing has to be running
# to deliver it: not Argo CD, not the operator.
#
# KERNEL_NETWORK_POLICIES=true applies them. Anything else -- unset is off --
# removes every one of them and applies none. Off by default, because no
# cluster had run these rules when they were written: turn them on once an
# install has succeeded, with ./install.sh --only A-01,B-01
# (docs/install-reference.md), and off again the same way if a component
# then times out reaching another.
#
# Each policy carries the digest of the file it came from as a label, which
# is how a policy a later file no longer has is found and removed, and how
# check() tells that the cluster has this file's rules and not an earlier
# one's.
# -----------------------------------------------------------------------------
KERNEL_NETWORK_POLICIES_FILE="${KERNEL_NETWORK_POLICIES_FILE:-${SCRIPT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}/kernel/security/network-policies/kernel-network-policies.yaml}"
KERNEL_NETWORK_POLICY_LABEL="gentianos.io/kernel-network-policy"

ns_kernel_policies_wanted() {
    [[ "${KERNEL_NETWORK_POLICIES:-false}" == "true" ]]
}

_ns_kernel_policies_digest() {
    openssl dgst -sha256 < "${KERNEL_NETWORK_POLICIES_FILE}" | awk '{ print substr($NF, 1, 16) }'
}

# _ns_kernel_policies_stamped <digest> -- the file, with the digest as a label
# beside the one every policy carries. awk rather than sed: a newline in a
# replacement is not something every sed writes.
# shellcheck disable=SC2016 # awk's own variables, not the shell's
_NS_POLICY_STAMP_AWK='
    { print }
    $1 == label ":" && $2 == "\"true\"" {
        match($0, /^ */)
        printf "%*s%s-digest: \"%s\"\n", RLENGTH, "", label, digest
    }
'

_ns_kernel_policies_stamped() {
    awk -v label="${KERNEL_NETWORK_POLICY_LABEL}" -v digest="${1:?digest}" \
        "${_NS_POLICY_STAMP_AWK}" "${KERNEL_NETWORK_POLICIES_FILE}"
}

# _ns_kernel_policies_live [selector] -- "<namespace>/<name>" per policy these
# rules own, optionally narrowed by a further label selector.
_ns_kernel_policies_live() {
    local selector="${KERNEL_NETWORK_POLICY_LABEL}=true${1:+,${1}}"
    kubectl get networkpolicy --all-namespaces -l "${selector}" \
        -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' 2>/dev/null
}

ns_kernel_policies_ok() {
    if ! ns_kernel_policies_wanted; then
        [[ -z "$(_ns_kernel_policies_live)" ]]
        return
    fi
    local digest want have
    digest="$(_ns_kernel_policies_digest)" || return 1
    want="$(grep -c '^kind: NetworkPolicy$' "${KERNEL_NETWORK_POLICIES_FILE}")"
    have="$(_ns_kernel_policies_live "${KERNEL_NETWORK_POLICY_LABEL}-digest=${digest}" | grep -c .)"
    [[ "${have}" == "${want}" ]] &&
        [[ -z "$(_ns_kernel_policies_live "${KERNEL_NETWORK_POLICY_LABEL}-digest!=${digest}")" ]]
}

# _ns_kernel_policies_remove [selector] -- delete the policies these rules own.
_ns_kernel_policies_remove() {
    local entry
    while IFS= read -r entry; do
        [[ -n "${entry}" ]] || continue
        kubectl delete networkpolicy "${entry#*/}" -n "${entry%%/*}" --ignore-not-found >/dev/null
    done < <(_ns_kernel_policies_live "${1:-}")
}

ns_kernel_policies_sync() {
    if ! ns_kernel_policies_wanted; then
        _ns_kernel_policies_remove
        return 0
    fi
    local digest
    digest="$(_ns_kernel_policies_digest)" || return 1
    # The digest goes in beside the label every policy carries. One apply of
    # one file: a namespace's rules arrive together.
    _ns_kernel_policies_stamped "${digest}" | kubectl apply -f - >/dev/null || return 1
    # What an earlier file had and this one has not.
    _ns_kernel_policies_remove "${KERNEL_NETWORK_POLICY_LABEL}-digest!=${digest}"
}

# =============================================================================
# What a namespace holds — every namespaced kind, not a list kept by hand
# =============================================================================
#
# A namespace object and its contents are separate keys in the datastore.
# Removing the namespace (its finalizer forced through /finalize) while objects
# are still stored leaves those objects in place, invisible until a namespace of
# the same name is created again and they reappear in it: a purge on MicroK8s
# left forty of kernel-edge's objects — cert-manager's Deployments, its Helm
# release record, letsencrypt keys — inside the namespace A-01 recreated the
# next morning. So whatever removes a namespace, or vouches for one, asks this.

# Every kind that is namespaced and can be listed and deleted. Discovery that
# fails for one group (an aggregated API whose service is gone) still prints
# the rest and exits non-zero, so the output is taken whatever the exit code:
# an aggregated API stores nothing in the datastore, so nothing of it can be
# left behind. events.events.k8s.io is the same objects as core events.
ns_namespaced_kinds() {
    { kubectl api-resources --verbs=list,delete --namespaced -o name --request-timeout=30s 2>/dev/null || true; } \
        | { grep -vx 'events.events.k8s.io' || true; }
}

# One line per object: "<kind>[.<group>]/<name><TAB><creationTimestamp>" — the
# reference is one kubectl get/patch/delete takes as it is. Asked for all kinds
# in one call, and kind by kind when that call fails (a CRD removed between
# discovery and the list makes the whole call fail). Works for a namespace that
# no longer exists, which is exactly where objects left behind are found.
# Returns 2 when discovery returned no kinds at all: nothing can be vouched for.
# shellcheck disable=SC2016 # jq's own variables
_NS_OBJECTS_JQ='.items[]? |
    ((.kind | ascii_downcase)
     + (if (.apiVersion | contains("/")) then "." + (.apiVersion | split("/")[0]) else "" end)
     + "/" + .metadata.name) + "\t" + (.metadata.creationTimestamp // "")'

ns_objects() {
    local ns="${1:?ns_objects <namespace>}" kinds kind out
    kinds="$(ns_namespaced_kinds)"
    [[ -n "${kinds}" ]] || return 2
    if out="$(kubectl get "$(printf '%s\n' "${kinds}" | paste -sd, -)" -n "${ns}" \
            --ignore-not-found -o json --request-timeout=60s 2>/dev/null)"; then
        printf '%s' "${out}" | jq -r "${_NS_OBJECTS_JQ}" 2>/dev/null || true
        return 0
    fi
    for kind in ${kinds}; do
        kubectl get "${kind}" -n "${ns}" --ignore-not-found -o json --request-timeout=30s 2>/dev/null \
            | jq -r "${_NS_OBJECTS_JQ}" 2>/dev/null || true
    done
    return 0
}

# ns_warn_stale_objects <namespace> — objects older than the namespace that
# holds them. A namespace cannot contain anything created before it was, so
# each one is a leftover of an earlier namespace of the same name whose
# contents were never deleted. Reported with the command that removes them,
# never removed here: they may be the only copy of something (a Helm release
# record, a certificate's key) somebody wants to look at first.
ns_warn_stale_objects() {
    local ns="${1:?}" created stale count
    created="$(kubectl get namespace "${ns}" -o jsonpath='{.metadata.creationTimestamp}' 2>/dev/null || true)"
    [[ -n "${created}" ]] || return 0
    # RFC 3339 in UTC, so the strings order as the times do.
    stale="$(ns_objects "${ns}" | awk -F'\t' -v c="${created}" '$2 != "" && $2 < c { print $1 }' || true)"
    [[ -n "${stale}" ]] || return 0
    count="$(printf '%s\n' "${stale}" | wc -l | tr -d ' ')"
    warn "${ns} (created ${created}) holds ${count} object(s) older than itself:"
    printf '%s\n' "${stale}" | sed 's/^/      /'
    warn "  They are left over from an earlier ${ns} whose contents were never deleted,"
    warn "  and the steps that follow will find them as if they were their own."
    warn "  Remove them, then run ./install.sh again:"
    warn "    kubectl delete -n ${ns} --wait=false $(printf '%s\n' "${stale}" | paste -sd' ' -)"
    return 1
}
