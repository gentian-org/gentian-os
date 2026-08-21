#!/usr/bin/env bash
# =============================================================================
# scripts/lint/lint-bootstrap-apps.sh — every bootstrap Application name must
# name a template that exists
# =============================================================================
# apply_bootstrap_application takes a NAME and renders
# kernel/bootstrap/chart/templates/<name>.yaml. The coupling is a string
# literal, so renaming a template silently orphans every caller that passes the
# old name — and no existing check sees it: validate-steps reads step headers,
# lint-resolvable resolves function calls, and neither knows that "cnpg-cluster"
# is supposed to be a filename.
#
# That is not hypothetical. Renaming cnpg-cluster.yaml to kernel-admin.yaml left
# two callers passing the old name, and the installer went on to report
# "Applied cnpg-cluster-application.yaml" while creating nothing: no shared CNPG
# Cluster and none of the seven admin ExternalSecrets. The apply function now
# fails loudly on a missing template, which catches it at run time; this catches
# it before the commit lands.
#
# Names are read from the two lists that drive install and teardown rather than
# from every string in the tree, because those are the two places the coupling
# actually exists.
# =============================================================================
set -euo pipefail

# scripts/lint/ -> repo root
cd "$(dirname "${BASH_SOURCE[0]}")/../.."

RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; DIM=$'\033[2m'; NC=$'\033[0m'

TEMPLATES="kernel/bootstrap/chart/templates"
rc=0
checked=0

# The install list: apps=(...) and apps+=(...) in bootstrap_argocd_apps.
# The teardown list: the for-loop in B-03's destroy().
names="$(
    {
        grep -hoE 'apps\+?=\([a-z0-9 -]+\)' scripts/lib/argocd.sh 2>/dev/null |
            sed -E 's/apps\+?=\(//; s/\)//'
        grep -hoE 'for app in [a-z0-9 -]+; do' scripts/steps/B-03-argocd-bootstrap-apps.sh 2>/dev/null |
            sed -E 's/for app in //; s/; do//'
    } | tr ' ' '\n' | sort -u | grep -v '^$'
)"

if [[ -z "${names}" ]]; then
    echo "${RED}FAIL${NC} — found no bootstrap Application names to check."
    echo "       The lists moved; this lint is reading the wrong place and would"
    echo "       pass forever. Point it at them again."
    exit 1
fi

echo ""
echo "Bootstrap Applications — does every name have a template?"
echo ""

while IFS= read -r name; do
    checked=$((checked + 1))
    if [[ -f "${TEMPLATES}/${name}.yaml" ]]; then
        printf '  %s✓%s %-24s %s%s/%s.yaml%s\n' "${GREEN}" "${NC}" "${name}" "${DIM}" "${TEMPLATES}" "${name}" "${NC}"
    else
        printf '  %s✗%s %-24s no %s/%s.yaml\n' "${RED}" "${NC}" "${name}" "${TEMPLATES}" "${name}"
        rc=1
    fi
done <<<"${names}"

echo ""
if (( rc == 0 )); then
    echo "${GREEN}Every bootstrap Application name resolves${NC} (${checked} names)."
else
    echo "${RED}A bootstrap Application names a template that does not exist.${NC}"
    echo "  Either the template was renamed and its callers were not, or the name"
    echo "  is a typo. The installer would create nothing for it."
fi
exit "${rc}"
