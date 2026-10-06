#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-first-tenant-scaffold.sh
# =============================================================================
# An install told the name of a first tenant (GENTIAN_FIRST_TENANT) writes
# that tenant's manifest in step 0, beside the platform tenant's, and commits
# it with the rest of the cluster's definition. That is the one tenant the
# installer ever writes, so what it writes and, more to the point, when it
# writes nothing are both worth holding still:
#
#   - no name: nothing is written and nothing later waits for a tenant;
#   - a name: the manifest the director would write, plus the annotation that
#     admits it before the handover, every field of it on the Tenant CRD;
#   - a name that cannot be a tenant's, or a first tenant under tenancyMode
#     single: refused before anything is written;
#   - a tenant that exists, a cluster that already has another tenant, a
#     tenant that was removed: never rewritten, never brought back.
#
# Runs the library's own functions in a throwaway deployments checkout, under
# set -u, the way the installer runs them. No cluster.
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
REPO="$(pwd)"

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0

SANDBOX="$(mktemp -d)"
trap 'rm -rf "${SANDBOX}"' EXIT
g() { git -c user.name=t -c user.email=t@t -c init.defaultBranch=main -c commit.gpgsign=false "$@" >/dev/null 2>&1; }

CLUSTER=c1
# new_checkout <dir> — a deployments checkout holding the platform tenant, as
# step 0 leaves it before the first tenant is considered.
new_checkout() {
    local dir="$1"
    rm -rf "${dir}"
    mkdir -p "${dir}/clusters/${CLUSTER}/tenants/platform" "${dir}/clusters/${CLUSTER}/kernel/claims"
    g init -b main "${dir}"
    printf 'apiVersion: gentianos.io/v1alpha1\nkind: Tenant\nmetadata:\n  name: platform\nspec:\n  isolation:\n    keycloakRealm: kernel\n' \
        > "${dir}/clusters/${CLUSTER}/tenants/platform/tenant.yaml"
    g -C "${dir}" add -A
    g -C "${dir}" commit -m seed
}

# run <checkout> <env assignments...> -- <shell> : the library, then the shell
# given, in a fresh process with nothing inherited but HOME and PATH.
run() {
    local checkout="$1"; shift
    local -a envs=()
    while [[ "$1" != "--" ]]; do envs+=("$1"); shift; done
    shift
    env -i HOME="${SANDBOX}/home" PATH="${PATH}" SCRIPT_DIR="${REPO}" \
        GENTIAN_DEPLOYMENTS_PATH="${checkout}" GENTIAN_DEPLOYMENTS_CLUSTER_ID="${CLUSTER}" \
        KERNEL_DOMAIN=k.example ${envs[@]+"${envs[@]}"} \
        bash -c 'set -u; source scripts/lib/load.sh >/dev/null 2>&1; trap - ERR; set +e; '"$1" 2>&1
}

ok()  { printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1)); }
bad() { printf '  %sFAIL%s  %s\n%s\n' "${RED}" "${NC}" "$1" "${2:-}"; fail=$((fail + 1)); }
expect() { # <what> <output> <must contain> [must not contain]
    if [[ "$2" == *"$3"* && "$2" != *"unbound variable"* && ( -z "${4:-}" || "$2" != *"$4"* ) ]]; then ok "$1"; else bad "$1" "$2"; fi
}

echo ""
echo "Step 0: the first tenant's scaffold"
echo ""

CO="${SANDBOX}/co"
TENANT_FILE="${CO}/clusters/${CLUSTER}/tenants/acme/tenant.yaml"

# ── No first tenant named: the install behaves as it always has. ─────────────
new_checkout "${CO}"
# shellcheck disable=SC2016 # the scripts below are for the child shell to expand
out="$(run "${CO}" -- 'resolve_first_tenant; echo "resolve=$?"; scaffold_first_tenant c1; echo "wrote=$?"; echo "first=[$(gentian_first_tenant)]"; echo "paths=$(_cluster_scaffold_paths c1 | wc -l | tr -d " ")"')"
expect "no name: nothing is refused, written or waited for" "${out}" $'resolve=0\nwrote=1\nfirst=[]\npaths=3'
if [[ "$(find "${CO}/clusters/${CLUSTER}/tenants" -mindepth 1 -maxdepth 1 | wc -l | tr -d ' ')" == "1" ]]; then
    ok "no name: the platform tenant is still the only directory"
else
    bad "no name: something was written" "$(ls "${CO}/clusters/${CLUSTER}/tenants")"
fi

# ── A first tenant named. ────────────────────────────────────────────────────
# shellcheck disable=SC2016
out="$(run "${CO}" GENTIAN_FIRST_TENANT=acme 'GENTIAN_FIRST_TENANT_DISPLAY_NAME=ACME "AG": \ Zürich' -- \
    'resolve_first_tenant; echo "resolve=$?"; scaffold_first_tenant c1; echo "wrote=$?"; echo "first=[$(gentian_first_tenant)]"; _cluster_scaffold_paths c1 | tail -1')"
expect "a name: the manifest is written and is the install's first tenant" "${out}" $'resolve=0' "wrote=1"
expect "a name: its directory is among what step 0 commits" "${out}" $'first=[acme]\nclusters/c1/tenants/acme'
if [[ -f "${TENANT_FILE}" && -f "${CO}/clusters/${CLUSTER}/tenants/acme/kustomization.yaml" ]]; then
    ok "a name: tenant.yaml and kustomization.yaml exist"
else
    bad "a name: the files are missing" "$(ls -R "${CO}/clusters")"
fi

# What was written, read back as YAML and held to the Tenant CRD with the
# walk the scaffold lint uses: a field the CRD does not have is refused at
# admission, on somebody else's cluster.
verdict="$(python3 - "${TENANT_FILE}" "${REPO}" <<'PY'
import importlib.util, sys, yaml
path, repo = sys.argv[1], sys.argv[2]
spec = importlib.util.spec_from_file_location("lint", repo + "/scripts/lint/lint-scaffold-schemas.py")
lint = importlib.util.module_from_spec(spec); spec.loader.exec_module(lint)
doc = yaml.safe_load(open(path))
schema = lint.load_crd_schemas().get(("Tenant", "v1alpha1"))
problems = []
if schema is None:
    problems.append("no Tenant CRD found")
else:
    lint.walk(doc["spec"], schema, "spec", problems)
s, m = doc["spec"], doc["metadata"]
checks = {
    "kind": doc["kind"] == "Tenant" and doc["apiVersion"] == "gentianos.io/v1alpha1",
    "name": m["name"] == "acme",
    "its own realm": s["isolation"]["keycloakRealm"] == "acme",
    "prefixes": s["isolation"]["databasePrefix"] == "acme_" and s["isolation"]["s3Prefix"] == "acme-",
    "display name survives quoting": s["displayName"] == 'ACME "AG": \\ Zürich',
    "second factor on": s["admin"]["requireMFA"] is True,
    "retain": s["deletionPolicy"] == "Retain",
    "no apps": s["apps"] == [],
    "handover override carries a reason": len(m["annotations"].get("gentianos.io/handover-override", "")) > 20,
    "sync wave": m["annotations"].get("argocd.argoproj.io/sync-wave") == "2",
}
problems += [k for k, v in checks.items() if not v]
print("OK" if not problems else "PROBLEMS: " + ", ".join(problems))
PY
)"
if [[ "${verdict}" == "OK" ]]; then
    ok "a name: every field is on the Tenant CRD, and the values are the director's"
else
    bad "a name: the manifest is wrong" "${verdict}"
fi

# ── A second run finds the first one's work. ─────────────────────────────────
before="$(cat "${TENANT_FILE}")"
# shellcheck disable=SC2016
out="$(run "${CO}" GENTIAN_FIRST_TENANT=acme GENTIAN_FIRST_TENANT_DISPLAY_NAME=Other -- 'scaffold_first_tenant c1; echo "wrote=$?"')"
if [[ "${out}" == *"wrote=1"* && "$(cat "${TENANT_FILE}")" == "${before}" ]]; then
    ok "a re-run: the manifest is not rewritten"
else
    bad "a re-run rewrote the manifest" "${out}"
fi
# The answer is read back from the tree: a resumed run with no name set still
# knows which tenant the install wrote.
# shellcheck disable=SC2016
out="$(run "${CO}" -- 'echo "first=[$(gentian_first_tenant)]"')"
expect "a re-run with no name set: the tenant is found from its manifest" "${out}" "first=[acme]"

# ── Refused before anything is written. ──────────────────────────────────────
new_checkout "${CO}"
for name in platform default kernel master Acme -acme a.b "$(printf 'a%.0s' $(seq 1 64))"; do
    # shellcheck disable=SC2016
    out="$(run "${CO}" "GENTIAN_FIRST_TENANT=${name}" -- 'resolve_first_tenant; echo "resolve=$?"')"
    expect "refused as a name: ${name:0:20}" "${out}" "resolve=1"
done
# shellcheck disable=SC2016
out="$(run "${CO}" GENTIAN_FIRST_TENANT=acme TENANCY_MODE=single -- 'resolve_first_tenant; echo "resolve=$?"')"
expect "refused under tenancyMode single" "${out}" "resolve=1"
expect "refused under tenancyMode single: says which mode a single-tenant cluster is" "${out}" "tenancyMode multi"
# shellcheck disable=SC2016
out="$(run "${CO}" GENTIAN_FIRST_TENANT=acme KERNEL_REALM=acme -- 'resolve_first_tenant; echo "resolve=$?"')"
expect "refused when the name is the kernel realm's" "${out}" "resolve=1"

# ── Never the second tenant, never a tenant the director made. ───────────────
new_checkout "${CO}"
mkdir -p "${CO}/clusters/${CLUSTER}/tenants/demo"
printf 'apiVersion: gentianos.io/v1alpha1\nkind: Tenant\nmetadata:\n  name: demo\nspec:\n  isolation:\n    keycloakRealm: demo\n' \
    > "${CO}/clusters/${CLUSTER}/tenants/demo/tenant.yaml"
# shellcheck disable=SC2016
out="$(run "${CO}" GENTIAN_FIRST_TENANT=acme -- 'scaffold_first_tenant c1; echo "wrote=$?"; echo "first=[$(gentian_first_tenant)]"')"
expect "another tenant exists: not written, and the director is named" "${out}" "kubectl gentian tenants create acme"
expect "another tenant exists: nothing for the install to wait for" "${out}" $'wrote=1\nfirst=[]'
if [[ ! -e "${CO}/clusters/${CLUSTER}/tenants/acme" ]]; then
    ok "another tenant exists: no directory appeared"
else
    bad "another tenant exists: a directory appeared"
fi
# shellcheck disable=SC2016
out="$(run "${CO}" GENTIAN_FIRST_TENANT=demo -- 'scaffold_first_tenant c1; echo "wrote=$?"; echo "first=[$(gentian_first_tenant)]"')"
expect "the named tenant is the director's: left as it is, not the install's to hand over" "${out}" $'wrote=1\nfirst=[]'
if grep -q 'handover-override' "${CO}/clusters/${CLUSTER}/tenants/demo/tenant.yaml"; then
    bad "the director's tenant was given the handover override"
else
    ok "the director's tenant was not touched"
fi

# ── Never brought back. ──────────────────────────────────────────────────────
new_checkout "${CO}"
# shellcheck disable=SC2016
run "${CO}" GENTIAN_FIRST_TENANT=acme -- 'scaffold_first_tenant c1' >/dev/null
g -C "${CO}" add -A; g -C "${CO}" commit -m "first tenant"
g -C "${CO}" rm -r "clusters/${CLUSTER}/tenants/acme"; g -C "${CO}" commit -m "Retire tenant acme"
# shellcheck disable=SC2016
out="$(run "${CO}" GENTIAN_FIRST_TENANT=acme -- 'scaffold_first_tenant c1; echo "wrote=$?"')"
expect "a tenant that was removed: the install does not bring it back" "${out}" "was removed"
if [[ ! -e "${CO}/clusters/${CLUSTER}/tenants/acme" ]]; then
    ok "a tenant that was removed: no directory appeared"
else
    bad "a retired tenant was recreated"
fi

echo ""
if (( fail > 0 )); then
    printf '%s%d failed%s, %d passed\n' "${RED}" "${fail}" "${NC}" "${pass}"
    exit 1
fi
printf '%sAll %d cases correct.%s\n' "${GREEN}" "${pass}" "${NC}"
