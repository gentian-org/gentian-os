#!/usr/bin/env bash
# =============================================================================
# scripts/lint/lint-password-schemes.sh — no password-equivalent in a passdb
# =============================================================================
# Dovecot verifies a login against a passdb entry. It can do that from a hash —
# ARGON2ID, SSHA512, BLF-CRYPT and SCRAM-SHA-256 are all available in the image
# — so the entry never needs to be the password itself.
#
# Writing {PLAIN} instead turns the passdb into a list of live credentials. That
# is worse here than the general case, for two reasons this cluster actually
# has: the file is delivered as a Kubernetes Secret, and Secrets are base64 in
# etcd rather than encrypted unless the API server runs with
# --encryption-provider-config; and a mail password is the reset vector for
# every other account its owner holds.
#
# This is not hypothetical. The first version of the Dovecot master user wrote
# {PLAIN} into a mounted Secret. It was removed for a different reason — a
# cluster-wide credential was the wrong shape — but nothing would have caught
# the scheme.
#
# Fatal, not advisory. There is no legitimate reason for a passdb entry to be a
# password, so unlike the claim-default lint this has no migration path to walk
# down: it is zero or it is a finding.
#
# Usage: scripts/lint/lint-password-schemes.sh
# =============================================================================

set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; DIM=$'\033[2m'; NC=$'\033[0m'

# Schemes that store the password, or something trivially reversible to it.
# CRYPT/DES-CRYPT truncate to 8 characters; SHA1/MD5 are unsalted and fall to a
# rainbow table. None belong in a new passdb.
FORBIDDEN='\{(PLAIN|CLEAR|CLEARTEXT|PLAIN-TRUNC|PLAIN-MD4|PLAIN-MD5|MD5|SHA|SHA1|SMD5|LDAP-MD5|DES-CRYPT|CRYPT)\}'

# Anything that could produce a Dovecot passdb entry: the mail charts, the
# operator's mail code, and the seeders. Scanned by content rather than by a
# fixed file list, so a new writer is covered without being added here.
#
# This file names every forbidden scheme, so scanning itself would report one
# finding per rule — the mistake lint-portability made until it excluded itself.
files() {
    git ls-files \
        'kernel/services/dovecot/*' \
        'kernel/services/postfix/*' \
        'internal/controller/*mail*' \
        'internal/kernel/secrets/*' \
        'scripts/lib/*.sh' \
        'scripts/bootstrap/*.sh' 2>/dev/null |
        grep -v '^scripts/lint/lint-password-schemes\.sh$'
}

echo ""
echo "Password scheme lint — is anything storing a password where a hash belongs?"
echo ""

_files=()
while IFS= read -r _f; do
    [[ -n "${_f}" ]] && _files+=("${_f}")
done < <(files)

if [[ ${#_files[@]} -eq 0 ]]; then
    echo "  ${RED}✗${NC} no files matched — the scan patterns are stale, which would pass silently."
    exit 1
fi

# Comments explaining the rule are not violations of it. A line is only a
# finding when the scheme appears in something being written, not described.
hits="$(grep -nE "${FORBIDDEN}" "${_files[@]}" 2>/dev/null |
        grep -vE ':[0-9]+:[[:space:]]*(#|//|--)' || true)"

n=0
[[ -n "${hits}" ]] && n="$(printf '%s\n' "${hits}" | wc -l | tr -d ' ')"

if [[ ${n} -eq 0 ]]; then
    printf '  %s✓%s %-46s %s\n' "${GREEN}" "${NC}" "passdb-capable files scanned" "${#_files[@]}"
    printf '  %s✓%s %-46s %s\n' "${GREEN}" "${NC}" "password-equivalent schemes" "0"
    echo ""
    echo "${GREEN}No passdb stores a password.${NC}"
    exit 0
fi

printf '  %s✗%s %s password-equivalent scheme(s):\n\n' "${RED}" "${NC}" "${n}"
printf '%s\n' "${hits}" | sed 's/^/      /'
echo ""
echo "${RED}A passdb entry must be a hash, not a credential.${NC}"
echo "Dovecot verifies ARGON2ID, SSHA512, BLF-CRYPT and SCRAM-SHA-256 — use one of those."
echo "${DIM}Secrets are base64 in etcd on a cluster without --encryption-provider-config,${NC}"
echo "${DIM}so a {PLAIN} passdb is a readable list of live mail credentials.${NC}"
exit 1
