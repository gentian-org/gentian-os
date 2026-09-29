#!/usr/bin/env bash
# step: A-01-namespaces
# phase: control-plane
# requires:
# provides: every kernel namespace of kernel/namespaces.yaml, labelled with gentianos.io/tier and gentianos.io/function; the platform's own namespaces labelled
# mutates: namespaces only

# The list is kernel/namespaces.yaml and nothing else: the operator reads the
# same file's Go twin, and a lint keeps the two equal. Every later step asks for
# a namespace by function (ns_kernel edge), never by name.

check() {
    local ns
    for ns in $(ns_kernel_all); do
        ns_labelled_ok "${ns}" || return 1
    done
    return 0
}

apply() {
    banner "Kernel namespaces"
    ns_ensure_kernel
    local ns
    for ns in $(ns_kernel_all); do
        success "${ns}  $(ns_labels "${ns}" | tr ' ' '  ')"
    done
}

destroy() {
    # Last in reverse order, so everything inside has already gone.
    local ns
    for ns in $(ns_kernel_all); do
        kubectl delete namespace "${ns}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    done
}
