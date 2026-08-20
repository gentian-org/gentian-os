#!/usr/bin/env bash
# =============================================================================
# scripts/tools/verify-claim-applied.sh — the cluster carries what the claim says
# =============================================================================
# The Cluster claim is the source for a cluster's settings, but not every
# consumer can read it. Compositions and the operator go through
# gentian-cluster-config; the ApplicationSets cannot, because Argo CD renders
# them before that ConfigMap exists, so their settings arrive as Helm parameters
# the installer writes onto the Application once.
#
# "Once" is the problem this checks. Those Applications are applied by the
# installer and never re-applied, so git and the claim can both say one thing
# while the live object says another, indefinitely, with nothing to show for it.
#
# That is not hypothetical. mail.serviceMode said kernel in the claim and
# external on the cluster: the operator skipped Dovecot provisioning, the
# ApplicationSet that manages Dovecot was never rendered, and the running
# Dovecot lost its owner while continuing to serve mailboxes. The symptom that
# eventually surfaced was an unrelated DNS record not updating.
#
# Reads only. It reports disagreement; it does not reconcile, because the fix
# differs per setting — some want the installer re-run, some a patch, and one
# wants the composition to carry the value so the copy can go.
#
# Usage:
#   scripts/tools/verify-claim-applied.sh
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; YELLOW=$'\033[1;33m'; DIM=$'\033[2m'; NC=$'\033[0m'

for cmd in kubectl python3; do
    if ! command -v "${cmd}" >/dev/null 2>&1; then
        echo "${YELLOW}SKIP${NC} — ${cmd} not found."
        exit 0
    fi
done
if ! kubectl cluster-info >/dev/null 2>&1; then
    echo "${YELLOW}SKIP${NC} — no reachable cluster."
    exit 0
fi

echo ""
echo "Claim settings, as the cluster actually carries them"
echo ""

pass=0; fail=0

# The composite, not the claim: the API server materialises the XRD's defaults
# onto it, so a setting the author left out still has the value everything
# downstream will see. Comparing against the claim alone would call a defaulted
# setting missing.
SPEC="$(kubectl get xclusters.gentianos.io -o jsonpath='{.items[0].spec}' 2>/dev/null)"
if [[ -z "${SPEC}" ]]; then
    echo "${YELLOW}SKIP${NC} — no XCluster on this cluster."
    exit 0
fi

claim_value() {
    printf '%s' "${SPEC}" | python3 -c "
import json,sys
d=json.load(sys.stdin)
for part in '$1'.split('.'):
    d = d.get(part) if isinstance(d, dict) else None
    if d is None: break
print('' if d is None else d)
" 2>/dev/null
}

report() {
    local label="$1" want="$2" got="$3" where="$4"
    if [[ "${want}" == "${got}" ]]; then
        printf '    %s✓%s %-46s %s\n' "${GREEN}" "${NC}" "${label}" "${DIM}${got}${NC}"
        pass=$((pass + 1))
    else
        printf '    %s✗%s %-46s claim says %s, %s says %s\n' \
            "${RED}" "${NC}" "${label}" "${want:-<empty>}" "${where}" "${got:-<absent>}"
        fail=$((fail + 1))
    fi
}

# ── gentian-cluster-config: what Compositions and the operator read ──────────
CC="$(kubectl get cm gentian-cluster-config -n crossplane-system -o jsonpath='{.data}' 2>/dev/null)"
cc_value() {
    printf '%s' "${CC}" | python3 -c "
import json,sys
try: print(json.load(sys.stdin).get('$1',''))
except Exception: print('')
" 2>/dev/null
}

echo "  gentian-cluster-config ${DIM}(Compositions and the operator read this)${NC}"
for pair in "mail.serviceMode:mail.serviceMode" "mail.egressHost:mail.egressHost"; do
    field="${pair%%:*}"; key="${pair##*:}"
    report "${field}" "$(claim_value "${field}")" "$(cc_value "${key}")" "the ConfigMap"
done

# ── gentian-appsets: Helm parameters the installer wrote once ────────────────
APPSET_PARAMS="$(kubectl get application gentian-appsets -n argocd \
    -o jsonpath='{.spec.source.helm.parameters}' 2>/dev/null)"
param_value() {
    printf '%s' "${APPSET_PARAMS}" | python3 -c "
import json,sys
try:
    for p in json.load(sys.stdin):
        if p.get('name') == '$1': print(p.get('value','')); break
    else: print('')
except Exception: print('')
" 2>/dev/null
}

echo ""
echo "  gentian-appsets ${DIM}(applied by the installer, never re-applied)${NC}"
for pair in "mail.serviceMode:mailServiceMode" "mail.egressHost:mailEgressHost" "kernelDomain:kernelDomain"; do
    field="${pair%%:*}"; param="${pair##*:}"
    report "${field}" "$(claim_value "${field}")" "$(param_value "${param}")" "the Application"
done

echo ""
if [[ ${fail} -gt 0 ]]; then
    echo "${RED}${fail} setting(s) the cluster does not carry as the claim states.${NC}"
    echo "A disagreement here is not cosmetic: the claim is what git shows and reviews,"
    echo "while the cluster runs on the other value, silently, until something downstream"
    echo "fails for an unrelated-looking reason."
    exit 1
fi
echo "${GREEN}${pass} setting(s) agree.${NC} The claim and the cluster say the same thing."
