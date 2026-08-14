#!/usr/bin/env bash
# step: 28-mail
# phase: applications
# requires: 26-operator
# provides: mail delivery per MAIL_SERVICE_MODE (external relay or kernel Postfix)
# mutates: mail Secrets and ConfigMaps in platform-kernel

check() {
    kubectl get secret keycloak-smtp-credentials -n platform-kernel >/dev/null 2>&1
}

apply() {
    install_kernel_mail
}

destroy() {
    kubectl delete secret keycloak-smtp-credentials -n platform-kernel \
        --ignore-not-found=true 2>/dev/null || true
}
