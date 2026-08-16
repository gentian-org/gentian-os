#!/usr/bin/env bash
# step: B-04-openbao-init
# phase: secrets
# requires: B-03-argocd-bootstrap-apps
# provides: initialised primary OpenBao, BAO_TOKEN for the rest of the run
# mutates: Secret openbao/openbao-init, OpenBao storage
# pins: openbao

check() {
    # OpenBao itself is the record: sys/init reports whether it was ever
    # initialised. There is no local Secret for this, and inventing one would
    # put install state back on disk.
    #
    # Returns 1 when unreachable so apply() produces the real diagnostic, and
    # because BAO_TOKEN is per-run: a satisfied check would skip the step that
    # exports it, leaving every later step without a token.
    return "${CHECK_ALWAYS}"
}

apply() {
    init_openbao
}

destroy() {
    kubectl delete secret openbao-init -n openbao \
        --ignore-not-found=true 2>/dev/null || true
}
