#!/usr/bin/env bash
# shellcheck disable=SC2015 # ok/bad report, never fail the line
# The director clones the deployments branch its cluster follows (os #1380):
# the bootstrap chart hands it deployments.revision, and the clone script the
# operator chart renders checks that branch out, so its pushes land there and
# not on the repository's default branch.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${ROOT}"
fail=0
ok()  { echo "  ok   $*"; }
bad() { echo "  FAIL $*"; fail=1; }
T="$(mktemp -d)"
trap 'rm -rf "${T}"' EXIT

# The bootstrap chart passes the branch Argo CD follows on to the operator.
python3 -c 'import yaml; print(yaml.safe_dump({"namespaces": yaml.safe_load(open("kernel/namespaces.yaml"))}))' > "${T}/ns.yaml"
bootstrap_branch() {
    helm template x kernel/bootstrap/chart -f kernel/platforms.yaml -f "${T}/ns.yaml" -f versions.yaml \
        --set operator.enabled=true --set-string operator.image=x --set-string operator.tag=develop-abc1234 \
        --set-string kernelDomain=k.example --set-string cluster=c "$@" \
        -s templates/gentian-os.yaml | python3 -c '
import sys, yaml
app = next(d for d in yaml.safe_load_all(sys.stdin) if d and d.get("kind") == "Application")
src = next(s for s in app["spec"]["sources"] if s.get("path") == "charts/gentian-os")
print(src["helm"]["valuesObject"]["appLifecycle"]["deployments"]["branch"])'
}
got="$(bootstrap_branch --set-string deployments.revision=exigo-dev)"
[[ "${got}" == exigo-dev ]] && ok "the operator is told the cluster's branch" || bad "branch handed on: ${got}"
got="$(bootstrap_branch)"
[[ "${got}" == main ]] && ok "the default revision hands on main" || bad "default branch handed on: ${got}"

# The clone script, as the operator chart renders it, run against a local
# repository whose default branch is main and which has a second branch.
clone_script() {
    helm template x charts/gentian-os --set director.enabled=true \
        --set-string appLifecycle.deployments.branch="$1" -s templates/director.yaml | python3 -c '
import sys, yaml
for d in yaml.safe_load_all(sys.stdin):
    if d and d.get("kind") == "Deployment":
        for c in d["spec"]["template"]["spec"].get("initContainers", []):
            if c["name"] == "clone-deployments":
                print(c["args"][0])'
}
git init -q -b main "${T}/seed"
git -C "${T}/seed" -c user.name=t -c user.email=t@t commit -q --allow-empty -m main
git -C "${T}/seed" checkout -q -b other
git -C "${T}/seed" -c user.name=t -c user.email=t@t commit -q --allow-empty -m other
git -C "${T}/seed" checkout -q main
git clone -q --bare "${T}/seed" "${T}/repo.git"

run_clone() {
    local script checkout="${T}/checkout-${1:-default}"
    script="$(clone_script "$1")"
    script="${script//\/data\/gentian-deployments/${checkout}}"
    mkdir -p "${checkout}"
    GENTIAN_DEPLOYMENTS_REPO="file://${T}/repo.git" GENTIAN_DEPLOYMENTS_BRANCH="$1" HOME="${T}" \
        sh -c "${script}" >/dev/null 2>&1
    git -C "${checkout}" rev-parse --abbrev-ref HEAD
}
got="$(run_clone other)"
[[ "${got}" == other ]] && ok "a branch is what the director checks out" || bad "checked out ${got} for branch other"
got="$(git -C "${T}/checkout-other" rev-parse --abbrev-ref '@{u}')"
[[ "${got}" == origin/other ]] && ok "and what it pushes to" || bad "upstream ${got} for branch other"
got="$(run_clone "")"
[[ "${got}" == main ]] && ok "no branch keeps the repository's default" || bad "checked out ${got} without a branch"

if (( fail )); then echo "FAILED"; exit 1; fi
echo "All checks passed."
