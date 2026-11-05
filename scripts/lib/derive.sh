#!/usr/bin/env bash
# =============================================================================
# scripts/lib/derive.sh — the kernel credential derivation
# =============================================================================
# Every kernel credential the installer derives is
#
#   hex(HMAC-SHA256(key = MASTER_PASSWORD + DERIVATION_SALT,
#                   message = "<context>:<purpose>"))
#
# 64 lowercase hex characters. This file is the one shell implementation;
# internal/vaultmigrate's derive is the Go one, and both are checked against
# scripts/tests/derivation-vectors.txt.
#
# No dependencies on the rest of the library: scripts/bootstrap/seed-openbao.sh
# sources it on its own.
# =============================================================================

# _derived <context> <purpose> — the derived value, whatever the secret mode.
#
# The key reaches python3 through the environment and the message through
# stdin; neither is an argument. DERIVATION_SALT must be set: a run that never
# settled its salt stops here instead of deriving values no cluster holds. It
# may be empty, which is what a cluster seeded before salts existed holds.
_derived() {
    : "${MASTER_PASSWORD?_derived: MASTER_PASSWORD is not set}"
    : "${DERIVATION_SALT?_derived: DERIVATION_SALT is not set}"
    printf '%s:%s' "${1}" "${2}" \
        | GENTIAN_DERIVE_KEY="${MASTER_PASSWORD}${DERIVATION_SALT}" python3 -I -c '
import hashlib, hmac, os, sys
key = os.environb[b"GENTIAN_DERIVE_KEY"]
sys.stdout.write(hmac.new(key, sys.stdin.buffer.read(), hashlib.sha256).hexdigest() + "\n")
'
}
