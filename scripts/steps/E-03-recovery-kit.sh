#!/usr/bin/env bash
# step: E-03-recovery-kit
# phase: handover
# requires: E-02-litellm-reconcile
# provides: an exported recovery kit, recorded in gentian-handover
# mutates: writes the kit file; records recoveryKitExported

# The install writes the kit itself rather than asking the operator to.
#
# Everything the kit carries exists by now and nowhere durable: the derivation
# salt, the master password, and the OpenBao recovery key that opens this
# cluster when its login path is what broke. E-04 deletes the file the recovery
# key lives in. Between those two facts there is exactly one moment when a kit
# can be made, and leaving it to a separate command the operator had to know
# about meant that moment was routinely missed — an install would finish, the
# summary would say a kit was still needed, and the recovery key was one
# reboot from gone the whole time.
#
# It lands beside the checkout, which is not where it belongs. That is
# deliberate and the export says so in as many words: the installer cannot know
# where this operator's break-glass material lives, and the one thing worse
# than a kit in an awkward place is no kit at all. Moving it is the first
# instruction of handover.

_kit_path() {
    # Named for the cluster, so two clusters' kits do not overwrite one another
    # in an operator's downloads directory six months from now.
    local id="${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-cluster}"
    local ext="age"
    command -v age >/dev/null 2>&1 || ext="enc"
    echo "${SCRIPT_DIR}/gentian-recovery-kit-${id}.${ext}"
}

check() {
    local ns="${GENTIAN_SYSTEM_NAMESPACE:-gentian-system}"
    [[ "$(kubectl get configmap gentian-handover -n "${ns}" \
        -o jsonpath='{.data.recoveryKitExported}' 2>/dev/null || true)" == "true" ]]
}

apply() {
    # The kit needs the master password and the salt, which export_recovery_kit
    # reads from OpenBao when they are not already in this shell. Both are there
    # by now — B-10 seeded them — so a scoped run of this step alone works too.
    local out
    out="$(_kit_path)"

    if [[ "${GENTIAN_DRY_RUN:-0}" == "1" ]]; then
        info "Would export the recovery kit to ${out} (dry run)."
        return 0
    fi

    # age -p and openssl both read a passphrase from the terminal, so an
    # unattended run has no way to answer and would hang. GENTIAN_KIT_RECIPIENT
    # is the documented way through that, and saying so here is more use than
    # letting the prompt block a pipeline.
    if [[ "${GENTIAN_NONINTERACTIVE:-0}" == "1" && -z "${GENTIAN_KIT_RECIPIENT:-}" ]]; then
        error "Cannot export the recovery kit unattended without GENTIAN_KIT_RECIPIENT."
        error "  Encryption would prompt for a passphrase and there is no terminal"
        error "  to answer it. Set GENTIAN_KIT_RECIPIENT to an age public key."
        return 1
    fi

    echo ""
    info "Exporting this cluster's recovery kit. You will be asked for a"
    info "  passphrase — it is what protects the file, so choose one you can"
    info "  find again, and keep it somewhere other than the kit."
    echo ""
    export_recovery_kit "${out}"
}

# No destroy(): a kit is the operator's, not the cluster's, and the whole point
# of it is to outlive the cluster it came from. purge_local_state does not
# delete it either.
