#!/usr/bin/env bash
# step: D-03-mail
# phase: applications
# requires: D-01-operator
# provides: mail delivery per MAIL_SERVICE_MODE (external relay or kernel Postfix)
# mutates: Postfix ConfigMaps in the mail namespace

check() {
    # This step's own artefact, not another step's. keycloak-smtp-credentials is
    # written by configure_keycloak_realm_smtp, which runs in D-05 — and
    # install_kernel_mail says as much, deferring realm SMTP to portal
    # bootstrap. Testing it here meant apply() could never satisfy check(), so
    # the step reported missing on a cluster whose mail stack was running.
    case "${MAIL_SERVICE_MODE:-external}" in
        kernel)
            # Both keys, not just the ConfigMap. Postfix mounts them as two
            # separate texthash: files — virtual_mailbox_domains decides which
            # recipients it accepts, virtual_mailbox_maps decides where they go
            # — so a ConfigMap carrying only one of them leaves inbound mail
            # broken while looking present.
            local data
            data="$(kubectl get configmap postfix-kernel-virtual-mailbox-maps \
                -n "$(gentian_mail_namespace)" -o jsonpath='{.data}' 2>/dev/null)" || return "${CHECK_MISSING}"
            [[ "${data}" == *'"virtual_mailbox_domains"'* ]] || return "${CHECK_MISSING}"
            [[ "${data}" == *'"virtual_mailbox_maps"'* ]] || return "${CHECK_MISSING}"
            ;;
        *)
            # An external relay is configured on Postfix only if one is already
            # deployed; the step otherwise just reports where mail will go, so
            # there is no artefact whose absence would mean anything.
            return "${CHECK_UNDEFINED}"
            ;;
    esac
}

apply() {
    install_kernel_mail
}

destroy() {
    # keycloak-smtp-credentials belongs to D-05, which creates it; removing
    # another step's artefact here would tear it down at the wrong point in the
    # reverse order.
    kubectl delete configmap postfix-kernel-virtual-mailbox-maps \
        -n "$(gentian_mail_namespace)" --ignore-not-found=true 2>/dev/null || true
}
