#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-claims-kinds.sh
# =============================================================================
# Step 0 refuses to commit a claim whose kind this checkout does not define
# (_claims_this_checkout_cannot_apply). The director seeds the cluster's
# PlatformSecurityPolicy into claims/, a kind of the operator chart's CRDs and
# not an XRD; refusing it stopped every install run after the seed.
#
# Held here: an XRD's kind and an operator CRD's kind pass, a kind neither
# defines is named.
#
# No cluster.
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0
ok()  { printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1)); }
bad() { printf '  %sFAIL%s  %s\n' "${RED}" "${NC}" "$1"; [[ -n "${2:-}" ]] && printf '%s\n' "$2"; fail=$((fail + 1)); }

echo ""
echo "claims kinds: step 0 commits a claim only of a kind this checkout defines"
echo ""

SB="$(mktemp -d "${TMPDIR:-/tmp}/gto.XXXXXX")"
trap 'rm -rf "${SB}"' EXIT
mkdir -p "${SB}/kernel/claims"
claim() { printf 'apiVersion: %s\nkind: %s\nmetadata:\n  name: x\n' "$2" "$3" > "${SB}/kernel/claims/$1"; }
claim cluster.yaml gentianos.io/v1alpha1 Cluster
claim platform-security-policy.yaml gentianos.io/v1alpha1 PlatformSecurityPolicy
claim infra-data.yaml gentianos.io/v1alpha1 InfraData

out="$(SCRIPT_DIR="$(pwd)" bash -c 'source scripts/lib/load.sh >/dev/null 2>&1; set +e
_claims_this_checkout_cannot_apply "$1"' _ "${SB}/kernel" 2>&1)"

if [[ "${out}" != *"cluster.yaml"* ]]; then ok "a Cluster claim (an XRD's kind) passes"; else bad "a Cluster claim (an XRD's kind) passes" "${out}"; fi
if [[ "${out}" != *"platform-security-policy.yaml"* ]]; then ok "the seeded PlatformSecurityPolicy (an operator CRD's kind) passes"; else bad "the seeded PlatformSecurityPolicy (an operator CRD's kind) passes" "${out}"; fi
if [[ "${out}" == *$'infra-data.yaml\tInfraData'* ]]; then ok "a kind neither defines is named"; else bad "a kind neither defines is named" "${out}"; fi

echo ""
if [[ "${fail}" -gt 0 ]]; then
    echo "${RED}${fail} failed${NC}, ${pass} passed."
    exit 1
fi
echo "${GREEN}${pass} checks passed.${NC}"
