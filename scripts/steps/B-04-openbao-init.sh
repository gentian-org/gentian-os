#!/usr/bin/env bash
# step: B-04-openbao-init
# phase: secrets
# requires: B-03-argocd-bootstrap-apps
# provides: initialised primary OpenBao, BAO_TOKEN for the rest of the run
# mutates: OpenBao storage; writes the local openbao-init file (E-03 removes it)
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

# No destroy(). It used to delete a Secret named openbao-init — there has
# never been one; grepped the repository to confirm nothing creates it, and
# B-04's own check() already says so above. The patch was always a silent
# no-op, discovered while making E-03 delete the local init file wholesale
# instead of stripping one field from it.
#
# Deliberately not replaced with something that deletes the local init file
# either. On a cluster that finished handover it is already gone — E-03
# removes it once a recovery kit exists. On one torn down before that, it may
# be the only surviving copy of a root token that is still live and a kit
# that was never exported; deleting it here would destroy the one way back in
# rather than protect anything. purge_local_state (scripts/lib/teardown.sh)
# already removes it unconditionally on a full --purge, which is the case
# where that trade is actually correct: storage is being destroyed too, so
# there is nothing left for the file to be a way back into.
