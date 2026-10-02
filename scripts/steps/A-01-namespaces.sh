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
    # Last in reverse order, so everything inside has already gone -- except
    # what still carries a finalizer whose controller an earlier step removed.
    # _delete_namespace waits, then strips those, so a namespace left
    # Terminating cannot block the next install's first write into it.
    local ns
    for ns in $(ns_kernel_all); do
        kubectl get namespace "${ns}" >/dev/null 2>&1 || continue
        _delete_namespace "${ns}"
    done
    _a01_remove_orphaned_webhooks
}

# Admission webhooks whose service lived in a namespace this purge removed.
#
# The configurations are cluster-scoped, so deleting the namespace of the
# controller that answers them -- Kyverno in kernel-admission, cert-manager in
# kernel-edge, CNPG in kernel-data, Crossplane in kernel-provisioning -- left
# them pointing at nothing, most with failurePolicy Fail. Kyverno's then
# refused every pod outside the kernel tier: the purge left a cluster that
# could not start a pod in default. Only configurations whose service
# namespace is one of the platform's and is gone are removed; another
# product's webhook is not this installer's to touch.
_a01_remove_orphaned_webhooks() {
    local kind name svc_ns
    for kind in validatingwebhookconfiguration mutatingwebhookconfiguration; do
        while IFS=$'\t' read -r name svc_ns; do
            [[ -n "${name}" && -n "${svc_ns}" ]] || continue
            kubectl get namespace "${svc_ns}" >/dev/null 2>&1 && continue
            kubectl delete "${kind}" "${name}" --ignore-not-found >/dev/null 2>&1 &&
                info "  removed ${kind} ${name} (its service namespace ${svc_ns} is gone)"
        done < <(kubectl get "${kind}" -o json 2>/dev/null | jq -r '
            .items[] | .metadata.name as $n
            | [.webhooks[]? | .clientConfig.service.namespace // empty
               | select(test("^(kernel|system|tenant)-"))] | unique | .[]
            | [$n, .] | @tsv' 2>/dev/null || true)
    done
}
