#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-user-tenant-scaffold.sh
# =============================================================================
# A cluster has one of two tenancy modes. Under single the install writes one
# tenant's manifest in step 0, beside the platform tenant's, and commits it
# with the rest of the cluster's definition: the user tenant, always named
# "user". Under multi it writes none. That is the one tenant the installer
# ever writes, so what it writes and when it writes nothing are both worth
# holding still:
#
#   - multi: nothing is written and nothing later waits for a tenant, whatever
#     the tree holds -- a tenant named user there is an ordinary one;
#   - single: tenants/user, the manifest the director would write with its
#     administrators allowed to approve public addresses, and nothing more --
#     no annotation admits it before the handover, and it may add no
#     catalogues -- every field of it on the Tenant CRD;
#   - a tenant that exists, a cluster that already has another user tenant, a
#     tenant that was removed: never rewritten, never brought back;
#   - the mode is the claim's when there is a claim, and what the run was told
#     only before one exists.
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
# step 0 leaves it before the user tenant is considered.
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
echo "Step 0: the user tenant's scaffold, per tenancy mode"
echo ""

CO="${SANDBOX}/co"
TENANT_FILE="${CO}/clusters/${CLUSTER}/tenants/user/tenant.yaml"
CLAIM="${CO}/clusters/${CLUSTER}/kernel/claims/cluster.yaml"

# write_claim <mode|""> — the Cluster claim, naming the mode or leaving it at
# its default as step 0 writes it.
write_claim() {
    printf 'apiVersion: gentianos.io/v1alpha1\nkind: Cluster\nmetadata:\n  name: c1\nspec:\n  kernelDomain: k.example\n' > "${CLAIM}"
    if [[ -n "$1" ]]; then
        printf '  tenancyMode: %s\n' "$1" >> "${CLAIM}"
    else
        printf '  # tenancyMode:  multi      the default\n' >> "${CLAIM}"
    fi
}

only_platform() {
    [[ "$(find "${CO}/clusters/${CLUSTER}/tenants" -mindepth 1 -maxdepth 1 | wc -l | tr -d ' ')" == "1" ]]
}

# ── multi: the install creates no tenant. ────────────────────────────────────
for told in "" "TENANCY_MODE=multi"; do
    new_checkout "${CO}"
    # shellcheck disable=SC2016 # the scripts below are for the child shell to expand
    out="$(run "${CO}" ${told:+"${told}"} -- 'echo "mode=$(gentian_tenancy_mode)"; scaffold_user_tenant c1; echo "wrote=$?"; echo "user=[$(gentian_user_tenant)]"; echo "paths=$(_cluster_scaffold_paths c1 | wc -l | tr -d " ")"')"
    expect "multi (${told:-the default}): nothing is written or waited for" "${out}" $'mode=multi\nwrote=1\nuser=[]\npaths=3'
    if only_platform; then
        ok "multi (${told:-the default}): the platform tenant is still the only directory"
    else
        bad "multi: something was written" "$(ls "${CO}/clusters/${CLUSTER}/tenants")"
    fi
done
# A tenant named user under multi is somebody's ordinary tenant: not the
# install's to commit, wait for or hand over.
mkdir -p "${CO}/clusters/${CLUSTER}/tenants/user"
printf 'apiVersion: gentianos.io/v1alpha1\nkind: Tenant\nmetadata:\n  name: user\nspec:\n  isolation:\n    keycloakRealm: user\n' > "${TENANT_FILE}"
# shellcheck disable=SC2016
out="$(run "${CO}" -- 'echo "user=[$(gentian_user_tenant)]"; echo "paths=$(_cluster_scaffold_paths c1 | wc -l | tr -d " ")"')"
expect "multi: a tenant named user is not the install's" "${out}" $'user=[]\npaths=3'

# ── single: tenants/user is written. ─────────────────────────────────────────
new_checkout "${CO}"
# shellcheck disable=SC2016
out="$(run "${CO}" TENANCY_MODE=single -- \
    'echo "mode=$(gentian_tenancy_mode)"; scaffold_user_tenant c1; echo "wrote=$?"; echo "user=[$(gentian_user_tenant)]"; _cluster_scaffold_paths c1 | tail -1')"
expect "single: the manifest is written" "${out}" $'mode=single' "wrote=1"
expect "single: its directory is among what step 0 commits" "${out}" $'user=[user]\nclusters/c1/tenants/user'
if [[ -f "${TENANT_FILE}" && -f "${CO}/clusters/${CLUSTER}/tenants/user/kustomization.yaml" ]]; then
    ok "single: tenant.yaml and kustomization.yaml exist"
else
    bad "single: the files are missing" "$(ls -R "${CO}/clusters")"
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
    "name": m["name"] == "user",
    "its own realm, not the kernel's": s["isolation"]["keycloakRealm"] == "user",
    "prefixes": s["isolation"]["databasePrefix"] == "user_" and s["isolation"]["s3Prefix"] == "user-",
    "display name": s["displayName"] == "User",
    "second factor on": s["admin"]["requireMFA"] is True,
    "retain": s["deletionPolicy"] == "Retain",
    "no apps": s["apps"] == [],
    "its administrators approve public addresses": s.get("perimeter") == {"adminsApprove": True},
    "its administrators add no catalogues": "catalogue" not in s,
    "no handover override": "gentianos.io/handover-override" not in m["annotations"],
    "only the sync wave": list(m["annotations"]) == ["argocd.argoproj.io/sync-wave"] and m["annotations"]["argocd.argoproj.io/sync-wave"] == "2",
}
problems += [k for k, v in checks.items() if not v]
print("OK" if not problems else "PROBLEMS: " + ", ".join(problems))
PY
)"
if [[ "${verdict}" == "OK" ]]; then
    ok "single: every field is on the Tenant CRD, the values are the director's, and nothing admits it early"
else
    bad "single: the manifest is wrong" "${verdict}"
fi
if grep -q 'handover-override' "${TENANT_FILE}"; then
    bad "single: the manifest mentions the handover override"
else
    ok "single: the manifest does not mention the handover override"
fi

# ── A second run finds the first one's work. ─────────────────────────────────
before="$(cat "${TENANT_FILE}")"
# shellcheck disable=SC2016
out="$(run "${CO}" TENANCY_MODE=single -- 'scaffold_user_tenant c1; echo "wrote=$?"')"
if [[ "${out}" == *"wrote=1"* && "$(cat "${TENANT_FILE}")" == "${before}" ]]; then
    ok "a re-run: the manifest is not rewritten"
else
    bad "a re-run rewrote the manifest" "${out}"
fi

# ── The mode is the claim's once there is a claim. ───────────────────────────
write_claim single
# shellcheck disable=SC2016
out="$(run "${CO}" -- 'echo "mode=$(gentian_tenancy_mode)"; echo "user=[$(gentian_user_tenant)]"')"
expect "a resumed run with nothing in the environment reads single from the claim" "${out}" $'mode=single\nuser=[user]'
# shellcheck disable=SC2016
out="$(run "${CO}" TENANCY_MODE=multi -- 'echo "mode=$(gentian_tenancy_mode)"')"
expect "the claim wins over what a later run is told" "${out}" "mode=single"
write_claim multi
# shellcheck disable=SC2016
out="$(run "${CO}" TENANCY_MODE=single -- 'echo "mode=$(gentian_tenancy_mode)"; echo "user=[$(gentian_user_tenant)]"; scaffold_user_tenant c1; echo "wrote=$?"')"
expect "a claim that says multi: no user tenant, whatever the environment says" "${out}" $'mode=multi\nuser=[]\nwrote=1'
write_claim ""
# shellcheck disable=SC2016
out="$(run "${CO}" -- 'echo "mode=$(gentian_tenancy_mode)"')"
expect "a claim that leaves the mode at its default is multi" "${out}" "mode=multi"
# shellcheck disable=SC2016
out="$(run "${CO}" TENANCY_MODE=single -- 'echo "mode=$(gentian_tenancy_mode)"; scaffold_user_tenant c1; echo "wrote=$?"')"
expect "a claim at its default is multi even when the environment says single" "${out}" $'mode=multi\nwrote=1'
rm -f "${CLAIM}"
# shellcheck disable=SC2016
out="$(run "${CO}" TENANCY_MODE=nonsense -- 'echo "mode=$(gentian_tenancy_mode)"')"
expect "anything but single is multi" "${out}" "mode=multi"

# ── Never beside another user tenant. ────────────────────────────────────────
new_checkout "${CO}"
mkdir -p "${CO}/clusters/${CLUSTER}/tenants/demo"
printf 'apiVersion: gentianos.io/v1alpha1\nkind: Tenant\nmetadata:\n  name: demo\nspec:\n  isolation:\n    keycloakRealm: demo\n' \
    > "${CO}/clusters/${CLUSTER}/tenants/demo/tenant.yaml"
# shellcheck disable=SC2016
out="$(run "${CO}" TENANCY_MODE=single -- 'scaffold_user_tenant c1; echo "wrote=$?"; echo "user=[$(gentian_user_tenant)]"')"
expect "another user tenant exists: not written, and the mode's rule is said" "${out}" "exactly one user tenant, named user"
expect "another user tenant exists: it is named" "${out}" "Retire demo first"
expect "another user tenant exists: nothing for the install to wait for" "${out}" $'wrote=1\nuser=[]'
if [[ ! -e "${CO}/clusters/${CLUSTER}/tenants/user" ]]; then
    ok "another user tenant exists: no directory appeared"
else
    bad "another user tenant exists: a directory appeared"
fi

# ── Never brought back. ──────────────────────────────────────────────────────
new_checkout "${CO}"
# shellcheck disable=SC2016
run "${CO}" TENANCY_MODE=single -- 'scaffold_user_tenant c1' >/dev/null
g -C "${CO}" add -A; g -C "${CO}" commit -m "user tenant"
g -C "${CO}" rm -r "clusters/${CLUSTER}/tenants/user"; g -C "${CO}" commit -m "Retire tenant user"
# shellcheck disable=SC2016
out="$(run "${CO}" TENANCY_MODE=single -- 'scaffold_user_tenant c1; echo "wrote=$?"')"
expect "a user tenant that was removed: the install does not bring it back" "${out}" "was removed"
expect "a user tenant that was removed: the way to create it is named" "${out}" "kubectl gentian tenants create user"
if [[ ! -e "${CO}/clusters/${CLUSTER}/tenants/user" ]]; then
    ok "a user tenant that was removed: no directory appeared"
else
    bad "a retired tenant was recreated"
fi

# ── What the install says at the end, per mode. ──────────────────────────────
new_checkout "${CO}"
# shellcheck disable=SC2016
run "${CO}" TENANCY_MODE=single -- 'scaffold_user_tenant c1' >/dev/null
# shellcheck disable=SC2016
out="$(run "${CO}" TENANCY_MODE=single -- 'print_roles_summary')"
expect "single: the platform admin and where" "${out}" "platform admin — in charge of the platform"
expect "single: the platform admin's address" "${out}" "https://platform.k.example/"
expect "single: the user admin and what of" "${out}" "user admin — in charge of the users and the user tenant"
expect "single: the user admin's address and login" "${out}" "https://desktop.k.example/      user-admin@k.example"
expect "single: no tenant admin, and nothing about creating tenants" "${out}" "single-tenancy cluster" "tenants create <name>"
new_checkout "${CO}"
# shellcheck disable=SC2016
out="$(run "${CO}" -- 'print_roles_summary')"
expect "multi: the platform admin and where" "${out}" "https://platform.k.example/"
expect "multi: tenants are the platform admin's to create" "${out}" "kubectl gentian tenants create <name>" "user admin"
expect "multi: the tenant admin is named" "${out}" "tenant admin — in charge of one tenant and its users"

# ── The step that brings the user tenant up comes after the handover. ────────
order="$(find scripts/steps -maxdepth 1 -name '[A-Z]-[0-9][0-9]-*.sh' | LC_ALL=C sort | tr '\n' ' ')"
case "${order}" in
    *E-03-revoke-bootstrap-token.sh\ scripts/steps/E-04-user-tenant.sh\ ) ok "E-04-user-tenant is the last step, after the handover" ;;
    *) bad "E-04-user-tenant is not the step after E-03" "${order}" ;;
esac
if grep -rq 'GENTIAN_FIRST_TENANT\|handover-override' scripts/lib scripts/steps install.sh install.env.template; then
    bad "the installer still names a first tenant or the handover override" \
        "$(grep -rn 'GENTIAN_FIRST_TENANT\|handover-override' scripts/lib scripts/steps install.sh install.env.template)"
else
    ok "nothing in the installer creates a tenant before the handover"
fi

echo ""
if (( fail > 0 )); then
    printf '%s%d failed%s, %d passed\n' "${RED}" "${fail}" "${NC}" "${pass}"
    exit 1
fi
printf '%sAll %d cases correct.%s\n' "${GREEN}" "${pass}" "${NC}"
